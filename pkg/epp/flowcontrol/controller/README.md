# FlowController Engine: Architecture and Design Rationale

This document provides an in-depth explanation of the `FlowController` engine, its core operational mechanisms, and the
architectural principles that guide its design for high-throughput, scalable request management.

## Overview

The `FlowController` is the central processing engine of the flow control system. It is a sharded, high-throughput
component responsible for managing the lifecycle of all incoming requests—from initial submission via the synchronous
`EnqueueAndWait` method to a terminal outcome (dispatch, rejection, or eviction). It achieves this by orchestrating the
`FlowRegistry`, pluggable `Policy` framework, and `SaturationDetector` to make continuous, state-aware decisions.

## Key Operational Mechanisms

* **Request Ingress (`EnqueueAndWait`)**: The primary entry point for all requests. This method is synchronous and
  blocks the calling goroutine until the Flow Controller determines a final outcome for the request. The rationale for
  this model is detailed below.

* **Request Distribution**: A top-level distributor receives requests and uses a
  **Join-the-Shortest-Queue-by-Bytes (JSQ-Bytes)** algorithm to assign each request to a specific worker shard,
  balancing the load across the system.

* **The Worker Processing Loop (`run`)**: Each `shardProcessor` instance executes the main processing loop, which is the
  hot path for request orchestration on its shard. The loop constantly interleaves accepting new requests with
  attempting to dispatch already-queued requests. This design is a deliberate strategy for **contention management**.
  With potentially many (`M`) concurrent goroutines calling `EnqueueAndWait` and `N` worker goroutines dispatching, this
  interleaving ensures that the channel from the distributor to the worker does not become a deep, unmanaged buffer or
  starve dispatch attempts providing backpressure and responsiveness.

* **Dispatch and Saturation Gating**: The dispatch process iterates through configured priority bands, from highest to
  lowest. Before attempting to dispatch from any queue, the worker consults the `SaturationDetector.IsSaturated()`
  method. If the system is saturated, dispatch is throttled to prevent backend overload. If unsaturated, the worker
  invokes the band's configured `InterFlowDispatchPolicy` and `IntraFlowDispatchPolicy` to select and dispatch an item.

* **Error Handling Philosophy**: The engine employs a robust, two-tiered error handling strategy to isolate failures and
  maximize system availability:
    * **"Fail Open" for Priority Bands (Inter-Flow):** If an inter-flow policy fails, the worker logs the error,
    **skips that priority band** for the current cycle, and continues to the next, promoting work conservation.
    * **"Fail Close" for a Band (Intra-Flow):** If an intra-flow operation fails after a queue has been selected, the
    worker **ceases further processing within that entire priority band** for the cycle to prevent a stateless
    inter-flow policy from repeatedly selecting a known-problematic queue.

---

## Architectural Deep Dive 1: The `EnqueueAndWait` Model

A fundamental design choice is the synchronous, blocking `EnqueueAndWait` method. In the context of the Gateway API
Inference Extension's Endpoint Picker (EPP), which operates as an Envoy External Processing (`ext_proc`) server, this
model is deliberately chosen for its simplicity and robustness.

* **Alignment with the `ext_proc` Request Lifecycle**: The `ext_proc` protocol is stream-based. A single goroutine
  within the EPP manages the stream for a given HTTP request and is ultimately responsible for telling Envoy how to
  proceed. `EnqueueAndWait` fits this perfectly: the request-handling goroutine calls it, blocks, and upon return, has
  the definitive outcome. It can then immediately act on that outcome (e.g., proceed to the scheduler or return an error
  to Envoy), maintaining clear request-goroutine affinity.

* **Simplified State Management**: The state of a "waiting" request is implicitly managed by the blocked goroutine's
  stack and its `context.Context`. The Flow Controller only needs to signal this specific goroutine to unblock it. An
  alternative, non-blocking handoff model would require complex intermediate data structures, explicit state machines,
  and correlation logic to route a decision back to the original request context.

* **Direct Backpressure Propagation**: If queues are full and displacement fails, `EnqueueAndWait` returns an
  `ErrQueueAtCapacity`. This provides immediate, direct backpressure to the earliest point of contact, preventing the
  system from accepting work it cannot handle.

* **Clearer Error Handling**: When `EnqueueAndWait` returns an error, the original goroutine in charge of the `ext_proc`
  stream can immediately formulate the correct HTTP response. A staged, asynchronous model would require a more complex
  mechanism to communicate a failure from a later stage back to the goroutine managing the Envoy stream.

---

## Architectural Deep Dive 2: The Sharded Model

The design of the Flow Controller is built on a sharded architecture to enable parallel processing and prevent the
central dispatch loop from becoming a bottleneck at high request rates. This choice has profound implications for state
management, fairness, and request distribution.

### The Sharded Architecture and its Implications

The `FlowController` consists of a top-level manager and a pool of independent `shardProcessor` workers. The
`FlowRegistry` guarantees that every logical flow is represented by a distinct queue instance on every active shard.
This creates `N` parallel instances for each flow, managed by `N` independent workers.

This architecture trades deterministic global state for high throughput and scalability. The key challenge, and the
system's most critical assumption, revolves around ensuring this distributed model can still achieve global fairness
objectives.

#### The Critical Assumption: Workload Homogeneity Within Flows

The effectiveness of the sharded model hinges on a critical assumption: **while the system as a whole manages a**
**heterogeneous set of flows, the traffic *within a single logical flow* is assumed to be roughly homogeneous in its**
**characteristics over a sliding time window.** A logical flow is intended to represent a single workload or tenant;
therefore, the most unpredictable variables (like decode behavior based on non-structural request characteristics) are
expected to be statistically similar *within* that flow.

### Request Distribution: Join the Shortest Queue by Bytes (JSQ-Bytes)

To make the critical assumption as robust as possible, the `FlowController` uses a
**Join the Shortest Queue by Bytes (JSQ-Bytes)** algorithm to distribute incoming requests.

* **What JSQ-Bytes Measures**: `ByteSize` is an excellent proxy for the resources the Flow Controller explicitly
  manages: host memory pressure and queuing capacity. It is also a reasonable proxy for prefill compute time and
  Head-of-Line (HOL) blocking within the controller itself.
* **Why It's a Better Fit**: The goal of the distributor is not to perfectly predict backend compute time, but to
  intelligently balance the load at the controller level. JSQ-Bytes achieves this by:
    1.  **Reflecting True Load**: It distributes work based on each shard's current queue size in bytes—a direct measure
        of its memory and capacity congestion.
    2.  **Adapting to Real-Time Congestion**: The byte-size of a queue is a real-time signal of a shard's overall
        congestion. JSQ-Bytes adaptively steers new work away from momentarily struggling workers.
    3.  **Hedging Against Assumption Violations**: This adaptive, self-correcting nature makes it a powerful hedge. It
        doesn't just distribute; it actively *load balances* based on the most relevant feedback available.

### Stateful Policies in a Sharded Registry

Sharding fundamentally changes how stateful policies achieve global objectives.

* **Shard-Local State**: Policies like a simple Round Robin can operate with purely shard-local state. When the critical
  assumption holds, the independent actions of these `N` policies result in **emergent, approximate global fairness**.
* **Global State Dependencies**: Achieving true, deterministic global fairness is still possible. However, this requires
  policies to be designed with a dependency on an external, globally-consistent state store (e.g., a central metrics
  service for policies that track SLO attainment).
* **The Low-QPS Challenge**: The critical assumption is most stressed by low-QPS flows. Managing the `shard count` is a
  key operational lever to mitigate this.

### The Displacement Strategy: Iterative and Shard-Local

Displacement is a corrective, non-hot-path mechanism activated only when a request cannot be enqueued due to capacity
limits. The strategy is designed for robustness and simplicity over theoretical perfection.

#### Why Displacement is Shard-Local

A key principle is that the entire displacement process is **confined to the shard where the request landed**. A global
mechanism would require cross-shard locking, re-introducing massive contention and **destroying the scalability**
**benefits of the sharded architecture**. We consciously trade optimal packing efficiency for superior performance.

#### Why Displacement is Iterative, Not Batched

The engine displaces victims one by one, re-evaluating after each removal.

* **The Challenge: State Mutation**: Policies are dynamic, and displacement mutates state. Removing a single victim
  changes the `ByteSize` and `Len` of a queue. Furthermore, both policies and the underlying `SafeQueue` implementation
  have internal state that changes upon removal (e.g., a min-heap must re-heapify, changing the next head item).
* **Why Simulation is Intractable**: Because every removal changes the state, pre-calculating a batch of victims would
  require a slow, complex, serial simulation, which is antithetical to the system's performance goals.
* **The Pragmatic Solution**: We use a fast, initial feasibility check:
  `(total_bytes_in_lower_bands) * (tolerance_factor) > bytes_needed`. The **tolerance factor** provides a crucial
  buffer, acknowledging that policies may legitimately choose not to select a victim. This heuristic aims to prevent a
  costly and futile displacement cascade where a large request churns through many smaller items only to be rejected
  anyway.
