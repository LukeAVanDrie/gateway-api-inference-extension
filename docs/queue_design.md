# Inference Gateway Queuing System Design

## Objectives

This document details the design and implementation of a centralized queuing system for the Inference Extension. The system aims to improve the performance, fairness, and resource utilization of LLM inference serving by queueing requests at the extension before scheduling when individual backend model servers are loaded. This centralized queuing approach enables more sophisticated queuing algorithms and control mechanisms.
The Inference Extension uses a separate goroutine for each request, lacking a central processing thread; therefore, queuing a request means blocking its goroutine. Initial discussions and a prototype explored implementing queuing within this constraint. This document formalizes the design and addresses its nuances.

The key objectives of this queuing system are:

* **Fairness:**  Ensure fair access to backend model servers for all flows (models), preventing starvation during periods of high demand. This is achieved using Weighted Fair Queuing (WFQ) in the `FairRequestQueue`.
* **Prioritization:** Enable prioritization of requests based on model criticality. Initially, this will be partly achieved by rejecting non-critical requests when the system is under load. Later, we will support queuing all requests with a criticality-based eviction strategy for when the queue reaches capacity and a criticality-aware dequeueing algorithm.
* **Improved Performance:**  Reduce overall latency and improve throughput, especially under high load, by queueing requests at the extension and regulating the flow to backend servers. This is achieved through dynamic stock management and the hysteresis band mechanism in the `RequestQueueController`.
* **Efficient Resource Utilization:** Optimize the utilization of backend model servers by dynamically adjusting the request outflow based on backend load ("stock"). The dynamic stock management and bypass mechanisms in the `RequestQueueController` address this objective.

These objectives will be validated through benchmarking and performance testing, as described in the Performance and Scaling section.

## Analogy: An Intelligent National Mail Service

Imagine a nationwide postal system that services corporate clients (models) which may define service level agreements (SLAs). These clients may also have different divisions (model variants/target models).

The postal service is comprised of a national routing center (e.g., an L7 load balancer like Envoy), an independent sorting hub in each state (e.g., an Inference Extension deployment in a cloud zone), and many regional distribution centers (backends in a backend pool) within each state. Each distribution center is general purpose but may experience temporary efficiency gains (analogous to a "warm-up" effect) when handling mail from the same corporate client or division consecutively (variant affinity).

Corporate clients and their divisions send diverse mail (requests) continuously and independently. Each piece of mail has visible characteristics (like size and weight) and hidden characteristics (like package contents), impacting processing time at the regional distribution centers. This is analogous to varying request sizes and complexity in LLM inference.

1. **National Routing (L7 Load Balancer):** Mail arrives at the national routing center, which directs it to the appropriate state sorting hub based on factors like sender location, client service agreements, and overall system load. This initial routing is outside the scope of individual state hubs.

2. **State Sorting Hub (RequestQueueController):** Each state sorting hub independently manages its local mail flow. The hub monitors the current *total* mail volume ("stock") in its state--including mail in transit to distribution centers and mail already queued for processing at the centers--as well as the number of *open and operational* regional distribution centers (ready backends). It aims to maintain the *average stock per regional distribution center* close to a target level. The sorting hub uses upper and lower thresholds around the target average stock (a hysteresis band) to smooth its queueing decisions, preventing rapid oscillations between queueing and bypassing. If "stock" is too high, mail enters the state's holding area (queue). If "stock" is too low, mail bypasses the holding area entirely and is directly handled by the regional routing system. The hysteresis band prevents rapid oscillations between queueing and bypassing.

3. **State Holding Area (FairRequestQueue):** Each state's holding area employs Weighted Fair Queuing (WFQ) to sequentially move mail to its regional routing system (scheduler). This ensures each corporate client receives fair access to the regional routing system proportional to its SLA, regardless of its total mail volume across its divisions, the visible mail characteristics, or speculations about the hidden mail characteristics. It also prevents starvation for any client. Critically, the mail for each client is sent to the regional routing system in the order it was received (FIFO); however, this property is not preserved across clients.

4. **Regional Routing System (Scheduler)**: Each state has a routing system that selects the best regional distribution center *within* the state for each piece of mail. The routing system considers: recent processing history of each distribution center for clients' divisions (variant affinity), current queue length (backend queue length), and overall busyness of each center (kv-cache utilization).

Critically, while visible mail characteristics are known (e.g., are we routing a letter, an oversized package, etc.), the *exact* processing time at a distribution center is uncertain. This is because the processing itself has an unpredictable component dependent on the hidden mail characteristics (e.g., manually sorting through a package's contents). This uncertainty, where the center doesn’t know the exact processing time until a mail piece is fully handled, mirrors the auto-regressive nature of LLM inference. Additionally, the routing system only selects the destination distribution center; it *does not* manage the queues *within* each center.

**Distribution Center (Backends):** Each distribution center has its own internal processing queue of limited capacity. Mail accumulates at the center until processed until proceessed. The state sorting hub, by controlling the overall flow of mail in the state, indirectly influences how quickly these queues fill up. If the sorting hub is holding back mail due to high state-level volume, the distribution centers will receive mail at a slower rate, giving them more time to process their existing backlog. This helps prevent individual centers from becoming overwhelmed, even though the sorting hub doesn't directly manage the distribution of mail across centers or their queues. If the internal queue is full, the center rejects new mail until it can process some of its backlog. Processing time depends on mail characteristics, the center's load (pending mail + busyness), context switching overhead (due to client or division affinity), and the inherent uncertainty of the processing itself. Congestion can occur independently at each center. Occasionally, mail might be lost or damaged in transit to or from a distribution center, or even within the center itself, representing expirations or errors that occur outside the sorting hub's control.


## Assumptions

The design and implementation of the queuing system rely on the following key assumptions:

1. **Model Identification:** Each model is uniquely identified by its name. This is crucial for flow management, allowing the system to track requests for different models independently and apply per-model queue configurations (e.g., `flowCapacity`).  Without unique model identifiers, the system would not be able to distinguish between different flows and enforce fairness or prioritization policies.

2. **Request Characteristics:** Requests can be characterized by a variable, but finite and quantifiable, size (e.g., token counts). This is crucial for future fairness enhancements in WFQ (allowing for weighted fairness based on request size) and more accurate stock management (allowing stock to reflect the actual load imposed by requests). Initially, the system will use a fixed request size, simplifying implementation but potentially impacting fairness.

3. **Routing and Backend Handling:**
  * **Backend Agnosticism:** Flows (models) are backend-agnostic. The system assumes any ready backend can handle any request.
  * **Request Routing through Inference Extension:** All requests to backends are routed through the Inference Extension. This is essential for accurate stock measurement, as the Inference Extension needs to track all requests entering and exiting the system.  If requests bypass the Inference Extension, the stock metric will be inaccurate, potentially leading to incorrect queueing decisions.

## Overview

The Inference Extension's queuing system optimizes resource utilization and ensures fair access to backend models by dynamically managing request flow and applying Weighted Fair Queuing (WFQ).  The system uses a dynamic stock-and-flow mechanism, where "stock" represents the number of requests currently being processed by backends (including those waiting in backend queues, in transit, or actively being processed). This centralized queuing system is distinct from any queues that might exist on individual backend model servers.  The `FairRequestQueue` prioritizes queued requests using WFQ, ensuring fairness across different models. The `RequestQueueController` monitors the stock level and dynamically adapts to varying backend availability, applying backpressure to prevent overload and bypassing the queue when the system is underutilized.  The system also handles request expirations and timeouts gracefully. *(See the Detailed Design section for a comprehensive description.)*

![System Diagram](diagram.png)  *(Placeholder for diagram illustrating request flow and key components)*

## Request Lifecycle

This section describes the journey of a request through the Inference Extension, focusing on its interactions with the queuing system.

**1. Request Entry:**

- Envoy forwards the request to the Inference Extension, where it is processed in its own goroutine.
- The extension resolves the target model.

**2. Queueing Decision (RequestQueueController):**

The `RequestQueueController` acts as the gatekeeper, deciding whether a request should be queued or bypass the queue based on system load ("stock"), queue length, and request criticality.

- **Bypass:**
    - *Conditions:* Stock below overall `upperBound` (calculated from `upperBoundPerBackend`) AND queue (`FairRequestQueue`) empty.
    - *Rationale:* System is underutilized; bypassing minimizes latency (skips queueing latency).
    - *Action:*  `OnRequestScheduled` is called (incrementing stock), and the request proceeds directly to scheduling.
- **Enqueue (Critical Requests Only):**
    - *Conditions:* Stock above overall `upperBound` (calculated from `upperBoundPerBackend`) OR queue not empty, AND the request is marked as critical.
    - *Rationale:* System is near or over capacity; queueing provides backpressure and ensures fairness. Critical requests are prioritized.
    - *Action:* Request added to the `FairRequestQueue`. The request goroutine is blocked until the request is dequeued, its TTL expires, or its context is cancelled.
- **Reject (Non-Critical Requests or Queue Capacity Exceeded):**
    - *Conditions:*  Queue capacity reached OR queue not empty and the request is not marked as critical.
    - *Rationale:* System is already at capacity or busy processing critical requests; non-critical requests are rejected to prioritize critical requests and prevent excessive queueing.
    - *Action:* Request is immediately rejected with a `503 Service Unavailable` error.

**3. In Queue (FairRequestQueue):**

- *State:*  Request waits in the `FairRequestQueue`.
- *Mechanism:* The `FairRequestQueue` uses the Weighted Fair Queuing (WFQ) algorithm to determine the order of dequeueing.  Requests are not necessarily processed in strict FIFO order across different models due to fairness enforcement.
- *Purpose:* WFQ ensures fair access to backend resources across different models based on configured weights and priorities.

**4. Dequeueing (Popper Goroutines & RequestQueueController):**

- *Conditions:* Stock below overall `lowerBound` (calculated from `lowerBoundPerBackend`) AND request selected by WFQ.
- *Rationale:* System has available capacity to process more requests.
- *Action:* A popper goroutine dequeues the request from the `FairRequestQueue`. The request goroutine is unblocked. `OnRequestScheduled` is called (incrementing stock), and the request proceeds to scheduling.

**5. Scheduling (Scheduler):**

- *Action:* The scheduler selects a suitable backend pod for the request (whether dequeued or bypassed).

**6. Backend Processing and Response:**

- *Action:* Envoy forwards the request to the selected backend (outside the Inference Extension).  The backend processes the request and returns a response to Envoy.

**7. Response Processing (Inference Extension):**

- *Action:* Envoy forwards the response to the Inference Extension. The extension processes the response and calls `OnRequestComplete`, decrementing the stock regardless of whether the backend processing was successful or not.  This reflects that the backend resources allocated to the request are now free.

**8. Request Completion:**

- *Action:*  Envoy forwards the response to the client.

## Detailed Design

The queuing system comprises two main components: the `FairRequestQueue` and the `RequestQueueController`. These components work together to manage the flow of requests, ensuring fairness, preventing overload, and handling expirations. Multiple "popper" goroutines facilitate concurrent dequeueing and scheduling.

### FairRequestQueue

The `FairRequestQueue` employs the Weighted Fair Queuing (WFQ) algorithm to prioritize queued requests. WFQ assigns each request a virtual finish time (`virFinishTime`) based on the queue's current virtual time and the request's size (`requestSize`). Currently, `requestSize` is fixed at 1, effectively treating all requests equally. Future enhancements will incorporate request size estimation (e.g., using token counts) for more granular fairness and more accurate stock management, aligning the stock value with the request size used in the WFQ algorithm.

The queue is implemented as a thread safe wrapper around a set of min-heap priority queues, one per flow.

**Interface**

```go
type QueuedRequestContext interface {
	Model() string
	IsCritical() bool
	EvictAndCancel(reason EvictionReason)
	IsCancelled() bool
}

type FairRequestQueue interface {
    Push(x QueuedRequestContext) error
    Pop() (QueuedRequestContext, error)
    Len() int
    CleanupExpired()
}
```

**WFQ Algorithm:**

The `virFinishTime` for a request *r* in flow *f* within the queue *frq* is calculated as follows:

```go
virFinishTime(r) = max(frq.virtualTime, f.lastVirFinishTime) + requestSize
```

Where:

* `frq.virtualTime`: The queue's current virtual time, representing hypothetical time in a perfectly fair system.
* `f.lastVirFinishTime`: The virtual finish time of the ast request enqueued to flow *f*. This prevents starvation of flows with infrequent requests.
* `requestSize`: Currently fixed at 1, but future implementations will dynamically determine this based on factors like token count.

A min-heap data structure per flow efficiently manages requests, prioritizing those with the earliest `virFinishTime`.

**Other Properties:**

The queue supports TTL-based expiration (`queueTTL`) and external context cancellation. The `CleanupExpired` method, periodically invoked by the `RequestQueueController`, removes expired requests. Future optimizations may address potential inefficiencies of this cleanup process for large queues. Concurrency is managed using a `sync.RWMutex`, allowing concurrent reads while ensuring exclusive access during modifications.

### RequestQueueController

The `RequestQueueController` is the central control mechanism, governing the flow of requests to and from the `FairRequestQueue`. It monitors the "stock," representing the total number of requests currently being processed by backends (including those waiting in backend queues, in transit, or actively being processed). Stock serves as a proxy for backend load.  Future work will investigate tying the stock value to the WFQ `requestSize`, using a more representative unit of backend load than a simple request count.

**Interface**

```go
type QueueableRequestContext interface {
	Model() string
	IsCritical() bool
	Context() context.Context
	CancelFunc() context.CancelFunc
}

type QueueController interface {
	TryEnqueue(req QueueableRequestContext) error
	OnRequestScheduled()
	OnRequestComplete()
}
```

**Dynamic Stock Management and Hysteresis Band:**

The controller employs a dynamic stock-and-flow model with a hysteresis band (`lowerBound`, `upperBound`) derived from a target `watermarkPerBackend` and `percentageDeviationForBounds`, configurable per backend. Alternatively, `lowerBoundPerBackend` and `upperBoundPerBackend` can be configured directly. The controller scales these per-backend values based on the number of ready backends (`numReadyBackends`). The hysteresis band smooths queue behavior, preventing oscillations. When the stock exceeds the `upperBound`, requests are enqueued to apply backpressure.  When the stock falls below the `lowerBound`, poppers dequeue requests. The width of the hysteresis band (determined by `percentageDeviationForBounds` unless `upperBoundPerBackend` and `lowerBoundPerBackend` overrides are explicitly provided) influences responsiveness and stability; a narrow band increases responsiveness at the cost of potential oscillations.


**Key Responsibilities:**

* **Enqueue/Bypass Decisions:** If stock > `upperBound` or the queue isn't empty, new requests are enqueued.  If stock < `upperBound` *and* the queue is empty, requests bypass the queue.  `OnRequestScheduled`, which increments the stock, is always called.
* **Dequeueing (Popper Goroutines):** Multiple poppers monitor the stock. When stock < `lowerBound`, a popper attempts to dequeue from `FairRequestQueue`.  A small randomized wait (`popperWaitBase` + jitter) reduces lock contention. A `sync.Mutex` protects stock.
* **Stock Management:** `OnRequestScheduled` increments stock when a request is dispatched (directly or after dequeueing), while `OnRequestComplete` decrements stock on request completion (regardless of success or failure).
* **Expiry Cleanup:** A goroutine periodically cleans expired requests using `CleanupExpired`.

**Popper Goroutine Management:**

The `RequestQueueController` uses a fixed number (`numPoppers`) of "popper" goroutines, created during controller initialization.  These goroutines continuously attempt to dequeue requests when the stock falls below `lowerBound`. The poppers run until the `RequestQueueController` shuts down.  Future work may explore dynamically adjusting the number of poppers based on queue length and load.

### Queue Controller and Scheduler Interaction

The queue controller and scheduler are decoupled. The controller manages overall system load (stock) without backend-specific knowledge, while the scheduler independently selects backends.

**Advantages:** Simplicity, modularity, flexibility. You can disable queueing entirely and the Inference Extension will still work.

**Limitations:**
- **Indirect Backpressure:** Controller's backpressure is based on overall stock, not individual backend congestion.
- **Flow-Specific Congestion:** Controller cannot react to flow-specific congestion on backends (post-scheduling)

**Mitigating Decoupling Limitations:**

The scheduler could provide aggregate metrics to influence queue controller parameters or stock values. This indirect feedback allows the queue controller to react to backend load without needing backend-specific logic.

### Flow Isolation and Management

`FairRequestQueue` provides a degree of flow isolation via `flowCapacity` (maximum queued requests per flow) and `totalCapacity` (maximum queued requests across all flows). Exceeding `flowCapacity` yields 503 errors for that flow. Exceeding `totalCapacity` blocks new flows, but existing flows can still queue (up to their `flowCapacity`). However, this isolation only applies to the queue itself, not concurrent backend execution. A congested flow can still impact other flows by consuming shared backend resources. Future enhancements may include stricter isolation (limiting concurrent requests per flow per backend) and more sophisticated backpressure mechanisms.

Currently, WFQ does not ensure isolation of flows in terms of the number of concurrent requests post-scheduling. For example, if a particular flow's requests require disproportionately more resources from the backends, then this flow might inadvertently impact other flows by consuming a disproportionately large amount of resources at the backends, despite the (backend access) fairness enforced by the WFQ algorithm.

### Future Enhancements: Request Size Integration

Currently, WFQ uses a fixed `requestSize`. Future work will integrate dynamic request size estimation (e.g., token counts) into both WFQ and stock calculations. This will improve fairness and provide a more accurate representation of backend load. This will also impact the RequestQueueController’s stock management approach where dynamic request size contributions using `requestSize` will likely be used rather than a simple count of requests, allowing for fairer management of resources under varying request size conditions. Specifically, it will improve the responsiveness of the queue's backpressure to requests with varying sizes by adjusting the stock value proportionally to `requestSize`. This change will allow the controller to manage the queue more efficiently and ensure that requests are dispatched to backends in a fairer manner.

Specifically, these changes to the WFQ and Stock algorithms will be incorporated into the key responsibilities of the `RequestQueueController`: enqueue/bypass decisions, dequeueing decisions, stock management, and expiry cleanup. For example, incorporating dynamic request sizes into the WFQ algorithm will mean that poppers will consider the request size along with the virtual finish times when making dequeue decisions, ensuring more responsive allocation of backend server resources to larger, more complex requests.  Similarly, this change will also enhance how the system handles request expirations. Since request sizes will contribute more proportionally to the stock value, the controller will recognize expirations of larger requests faster and thus be able to free up more resources, allowing the queue to operate more efficiently.

## Invariants

This section outlines the key invariant properties that users of the queuing system can rely on. These properties hold true under all operating conditions, barring bugs or system failures outside the queuing system's control (e.g., backend failures, network outages).

**1. Request Ordering within a Flow:**

* **FIFO within a Flow:** Requests belonging to the same flow (model/targetModel combination) are processed in First-In, First-Out (FIFO) order *within that flow*.  This guarantees that requests for a specific model variant maintain their relative order, even if they are queued. Note that the WFQ algorithm influences queueing priority *across* flows.  Therefore, FIFO ordering is guaranteed only *within* a flow but *not* across all requests. This FIFO property assumes all requests have the same fixed size.  Once we introduce dynamic request sizes into the system, this property will no longer be strictly held.

**2. Stock Management:**

* **Stock Represents Backend Load:** The "stock" signal always reflects the total number of requests currently being processed by backends, including those queued at the backends, in transit, or actively being processed.
* **Stock Monotonicity:** The stock value can only change due to the following events:
    - Increment: `OnRequestScheduled` is called (when a request is dispatched to a backend, either directly or after being dequeued).
    - Decrement: `OnRequestComplete` is called (when a backend finishes processing a request, regardless of success or failure).
    - Decrement: `CleanupExpired` is called, which triggers when a request times out in the queue based on its TTL or its external context being cancelled.

**3. Queue Capacity Limits:**

* **Bounded Queue Length:** The `FairRequestQueue` enforces capacity limits. `totalCapacity` limits the total number of queued requests across all flows. `flowCapacity` limits the number of queued requests for each individual flow.  Attempts to enqueue requests beyond these limits will result in errors (`ErrTooManyFlows`, `ErrInsufficientFlowCapacity`).

**4. Timeout Enforcement:**

* **TTL Adherence:** Requests in the queue are evicted if they exceed their TTL (`queueTTL`).  Expired requests are not dispatched to backends.

**5. Backend Agnosticism:**

* **Flow-Backend Independence:**  The queuing system makes no assumptions about backend affinity or specialization.  Any ready backend can process any request from any flow.  Queueing decisions are made independently of backend selection, which is the scheduler's responsibility.

## Configuration Parameters

The following parameters control the behavior of the queuing system. Some of these parameters can be configured via flags which are passed onto the queue controller or fair request queue. Others are configured in code for those same components. More per-flow configurations may be introduced in the future.

**FairRequestQueue Parameters:**

| Parameter        | Description                                                     | Default | Constraints        | Tuning Guidance                                                                                      | Example Configuration                       |
|-----------------|-----------------------------------------------------------------|---------|--------------------|------------------------------------------------------------------------------------------------------|-------------------------------------------|
| `totalCapacity` | Maximum number of queued requests across all flows.              | 1000    | Positive integer   | Set based on available (host) memory and desired maximum queue depth. Consider the combined impact of this setting with per-flow capacity limits. | `fair_request_queue: { total_capacity: 2000 }` |
| `flowCapacity`  | Maximum number of queued requests for a single flow.            | 100     | Positive integer   | Set based on expected per-flow load. Monitor `queue_length` to identify congested flows.             | `fair_request_queue: { flow_capacity: 200 }`  |
| `queueTTL`     | Maximum time (seconds) a request can remain in the queue.        | 30s     | Positive duration  | Tune based on acceptable latency and client timeout settings.                                      | `fair_request_queue: { queue_ttl: "90s" }`    |


**RequestQueueController Parameters:**

| Parameter | Description | Default | Constraints | Tuning Guidance | Example Configuration |
|---|---|---|---|---|---|
| `watermarkPerBackend` | Desired stock level per backend. | 2 | Positive integer | Start with a low value and gradually increase, monitoring backend load. | `request_queue_controller: { watermark_per_backend: 3 }` |
| `percentageDeviationForBounds` | Percentage deviation from `watermarkPerBackend` used to derive `lowerBoundPerBackend` and `upperBoundPerBackend`.  Must be between 0 and 1 (exclusive). | 0.1 | (0, 1) | Smaller values increase responsiveness, larger values increase stability. Tune based on load variability and monitoring. | `request_queue_controller: { percentage_deviation_for_bounds: 0.15 }` |
| `lowerBoundPerBackend` | Lower bound of hysteresis band per backend. Dequeueing allowed below this level. If not configured, it is derived from `percentageDeviationForBounds`. | - | Non-negative integer, <= `watermarkPerBackend` | Manually setting this overrides the percentage deviation.  Use with caution. | `request_queue_controller: { lower_bound_per_backend: 1 }` |
| `upperBoundPerBackend` | Upper bound of hysteresis band per backend. Enqueueing allowed above this level. If not configured, it is derived from `percentageDeviationForBounds`. | - | Integer >= `watermarkPerBackend` | Manually setting this overrides the percentage deviation. Use with caution. | `request_queue_controller: { upper_bound_per_backend: 4 }` |
| `numPoppers` | Number of goroutines that dequeue requests.  | 10 | Positive integer | Tune based on queue length and load.  More poppers can increase throughput but also resource consumption.  | (Internal only) |
| `popperWaitBase` | Base wait time (milliseconds) for poppers before retrying dequeue.  | 10ms | Positive duration |  A small value reduces latency but increases lock contention. | (Internal only) |
| `popperWaitJitterMax` | Maximum random jitter (milliseconds) added to `popperWaitBase`. | 1ms | Non-negative duration |  Adds randomness to popper wait times to reduce lock contention. | (Internal only) |
| `expiryCleanupInterval` | Interval (seconds) at which expired requests are cleaned up.  | 1s | Positive duration |  Frequent cleanup reduces wasted resources but increases overhead. | `request_queue_controller: { expiry_cleanup_interval: "5s" }` |

## Performance and Scaling

Detailed performance analysis and benchmark data are forthcoming. The following are preliminary considerations regarding performance and scaling, focusing on the unique aspects of the queuing system.

**Key Factors and Potential Bottlenecks:**

Performance will depend on:

* **Queueing System Overhead:** Minimized under low load due to the bypass mechanism. The efficiency of the `FairRequestQueue`, `RequestQueueController`, and popper goroutines becomes more significant under higher loads.  Areas of potential optimization include the queue's data structure and the popper goroutine management strategy.
* **WFQ Algorithm:** While WFQ ensures fairness, it introduces a small overhead. Tuning the system to balance fairness and efficiency is crucial, especially under high load.  The planned incorporation of dynamic request sizing will influence WFQ performance and allow for more fine-grained fairness based on request complexity.

**Scaling and Optimization Strategies:**

Several strategies can improve scalability and efficiency:

* **Dynamic Popper Adjustment:**  Dynamically adjusting the number of popper goroutines based on queue length, stock levels, or other relevant metrics can optimize throughput and responsiveness. This adaptation will allow the system to scale more effectively to varying load conditions.
* **Queue Optimization:**  Efficient data structures and algorithms for the `FairRequestQueue` can minimize overhead, particularly under high load. This includes optimizing the cleanup process for expired requests.  Investigating alternative queue implementations (e.g., lock-free queues) could yield further performance improvements.

**Future Work (Performance Analysis and Benchmarking):**

Comprehensive benchmarking and load testing will provide quantitative data to inform optimization efforts and **validate the queuing system's effectiveness in achieving its stated objectives**.  This data will be used to analyze throughput, latency, resource utilization, and the impact of various scaling and optimization strategies.

Specific areas of focus include:

* **Quantifying Impact on Objectives:** Measuring the queuing system's impact on tail latency, overall resource utilization, throughput, and average latency, comparing performance with and without the queuing system enabled.  This will directly address the objectives of improved fairness, efficient resource management, and enhanced performance.
* **Characterizing Overhead:** Analyzing the overhead introduced by the queuing system components (`FairRequestQueue`, `RequestQueueController`, popper goroutines) under varying load conditions.
* **Evaluating WFQ Effectiveness and Fairness/Efficiency Trade-off:**  Assessing the impact of WFQ on fairness across different models/flows, considering both average and tail latency. This will also involve analyzing the trade-off between fairness and overall system efficiency.
* **Measuring Impact of Dynamic Request Sizing and Popper Adjustment:**  Quantifying the performance benefits of incorporating dynamic request sizing into WFQ and dynamically adjusting the number of popper goroutines.

## Error Handling and Resilience

The queuing system is designed to be resilient to various error conditions, prioritizing availability and graceful degradation. It employs a **fail-open** strategy for *dequeue* errors (issues encountered while retrieving requests from the queue) to ensure the system continues functioning even if the queuing mechanism experiences transient problems.  However, it uses a **fail-closed** strategy for *enqueue* errors (issues encountered while adding requests to the queue) and request timeouts, prioritizing system stability and preventing overload by rejecting requests that cannot be handled safely.

**1. Queue Capacity Errors (Fail-Closed):**

These errors occur when attempting to add requests beyond the defined capacity limits, leading to immediate rejection to prevent overload.

* **`ErrTooManyFlows`:**  Attempting to enqueue a request for a model that is not currently tracked by the queue, and adding this new model/flow would exceed the total configured queue capacity (`totalQueueCapacity`).  The request for the new flow is *rejected* with a `503 Service Unavailable` error.
* **`ErrInsufficientFlowCapacity`:** Attempting to enqueue a request for a model/flow that has already reached its maximum capacity (`modelQueueCapacity`). The request is *rejected* with a `503 Service Unavailable` error.

**2. Request Timeouts (Fail-Closed):**

These errors occur when a request spends too long in the queue, resulting in cancellation and rejection.

* **Queue TTL Expiry (`ErrExpiredTTL`):** A request resides in the queue longer than the configured `queueTTL`. The request is cancelled and a `503 Service Unavailable` error is returned.
* **External Context Cancellation/Timeout:**  A client closes the connection or the client-side context times out while the request is waiting in the queue.  The request is cancelled and a `499 Client Closed Request` or `504 Gateway Timeout` error, respectively, is returned.

**3. Queue Operation Errors (Internal):**

These are unexpected errors encountered during queue operations. *Dequeue* operations (retrieving requests) follow a **fail-open** strategy, while *enqueue* operations (adding requests) generally follow a **fail-closed** strategy.

* **`ErrQueueEmpty` (Dequeue - Fail-Open):** A popper goroutine attempts to dequeue a request from an empty queue. This is a normal condition, and the popper waits and retries.
* **Other `Pop()` Errors (Dequeue - Fail-Open):**  Unexpected errors during dequeue operations (other than `ErrQueueEmpty`). These errors are logged, and the affected popper goroutine waits briefly before retrying.
* **Other `Push()` Errors (Enqueue - Fail-Closed):**  Unexpected errors during enqueue operations typically lead to the request being *rejected* with a `500 Internal Server Error`.  This fail-closed behavior ensures that the queue does not accept requests it cannot handle reliably.

**Summary of Error Responses:**

| Error Condition                        | HTTP Status Code          | Action                              |
|----------------------------------------|---------------------------|-------------------------------------|
| `ErrTooManyFlows`                      | 503 Service Unavailable   | Reject new flow/model request       |
| `ErrInsufficientFlowCapacity`          | 503 Service Unavailable   | Reject request for overloaded flow  |
| `ErrExpiredTTL`                        | 503 Service Unavailable   | Cancel and reject expired request   |
| Client Context Cancelled               | 499 Client Closed Request | Cancel and reject request           |
| Client Context Deadline Exceeded       | 504 Gateway Timeout       | Cancel and reject request           |
| Other `Push()` errors (Enqueue)        | 500 Internal Server Error | Reject request                      |
| Other `Pop()` errors (Dequeue - rare)  | N/A                       | Log error, popper waits and retries |
| `ErrQueueEmpty` (Dequeue - normal)     | N/A                       | Popper waits and retries            |

## Request Timeout Mechanisms and Cleanup

The queuing system manages request timeouts to ensure efficient resource use and prevent stale requests. Two mechanisms are employed: TTL-based expiry and external context cancellation.

**1. TTL-Based Expiry:**

Requests in the `FairRequestQueue` have a Time-To-Live (TTL) defined by `queueTTL`. Expired requests are periodically cleaned up, triggering a `503 Service Unavailable` error to the client.

**2. External Context Cancellation:**

Each request has an associated `context.Context`.  If this context is cancelled while waiting, `TryEnqueue` returns an error, and a  `499 Client Closed Request` or `504 Gateway Timeout` is returned to the client. The request is removed during the next cleanup cycle.

**3. Cleanup Strategy:**

A background goroutine periodically cleans up expired and cancelled requests using the queue's `CleanupExpired` method.  Context cancellation immediately frees the client caller, but the request's removal from the queue is delayed for thread safety and performance.

**4. Interaction with Error Handling:**

Timeouts are integrated with error handling. Expired or cancelled requests trigger specific error codes as described in the Error Handling section.


## Metrics

The following metrics are crucial for monitoring the queuing system's health, performance, and effectiveness. These metrics leverage the internal state and events within the `FairRequestQueue` and `RequestQueueController`. Wherever possible, metrics are labeled with both `model` (the original model requested) and `targetModel` (the resolved target model variant) to provide granular insights.

**Queue Length and Latency:**

* **`queue_length` (Type: Gauge, Labels: `model`, `targetModel`):** The number of requests currently waiting in the `FairRequestQueue`.
* **`queue_latency_seconds` (Type: Distribution, Labels: `model`, `targetModel`, `success`):** The time a request spends in the queue. Includes average and percentile values (p99, p999).  The `success` label indicates whether the request was ultimately processed successfully.

**Stock Level and Bypass Rate:**

* **`stock_level` (Type: Gauge, Labels: `model`, `targetModel`):** The portion of the current overall "stock" level attributable to each model and targetModel. This metric is aggregated based on the labels from the global stock counter maintained by the `RequestQueueController`.
* **`bypass_rate` (Type: Counter, Labels: `model`, `targetModel`):** The rate at which requests bypass the queue.

**Errors and Timeouts:**

* **`requests_expired` (Type: Counter, Labels: `model`, `targetModel`):** The number of requests expired due to TTL.
* **`requests_cancelled` (Type: Counter, Labels: `model`, `targetModel`):** The number of requests cancelled due to external context cancellation.
* **`queue_push_errors` (Type: Counter, Labels: `model`, `targetModel`, `error_type`):** The number of failed attempts to enqueue a request into the queue. Includes the `error_type` label indicating `ErrTooManyFlows` or `ErrInsufficientFlowCapacity`.  This helps distinguish between new flows being rejected and requests for existing flows being dropped due to capacity limits.

**Queue Controller Internal State:**

* **`upper_bound` (Type: Gauge):** The current upper bound of the hysteresis band. This is a global value calculated from `upperBoundPerBackend`.
* **`lower_bound` (Type: Gauge):** The current lower bound of the hysteresis band. This is a global value calculated from `lowerBoundPerBackend`.
* **`watermark` (Type: Gauge):**  The dynamically scaled watermark level. This is a global value calculated from `watermarkPerBackend`.

**Popper Goroutine Metrics:**

* **`popper_dequeue_attempts` (Type: Counter):** The total number of dequeue attempts.
* **`popper_dequeue_successes` (Type: Counter):** The number of successful dequeues.

**Flow Metrics:**

* **`active_flows` (Type: Gauge):** The number of currently active flows (models) being tracked by the queuing system. This metric gives insight into how many distinct models are currently utilizing the system.

## Future Work

This section outlines potential enhancements and extensions to the queuing system, categorized by area of improvement.  These enhancements are presented in a general order of priority, though actual implementation priorities may vary based on evolving needs and resource constraints.

**1. Fairness and Request Sizing:**

* **Dynamic Request Sizing:**  Transition from a fixed `requestSize` in the WFQ algorithm to a dynamic measure based on estimated request complexity (e.g., token count, input/output byte size). This will significantly improve fairness by ensuring that larger, more resource-intensive requests are appropriately prioritized. This will require modifications to both the `FairRequestQueue` (specifically, the `virFinishTime` calculation) and the `RequestQueueController`'s stock management logic, aligning stock units with request size.
* **Per-Flow WFQ Weights:** Implement per-flow weights in the WFQ algorithm to allow for prioritizing certain models based on business needs or SLAs.  This could leverage the `InferenceModel`'s existing `Criticality` field or a new configuration parameter.  This enhancement builds upon dynamic request sizing and will provide even finer-grained control over fairness.

**2. Priority-Based Preemption:**

The current prioritization mechanism, based on rejecting non-critical requests, provides a basic level of prioritization but can lead to starvation of non-critical requests.  In the next iteration of the queuing system, we will implement a more sophisticated prioritization mechanism based on preemption and cascading WFQ, addressing starvation while maintaining strict prioritization for critical requests.  This approach will introduce more nuanced control over queue management compared to the initial implementation.

**Key Features:**

* **Enqueue All Requests:**  All incoming requests, regardless of criticality level (Critical, Default, Sheddable), will be enqueued. This ensures that all requests have a chance to be processed, preventing outright rejection based on criticality.
* **Preemption by Criticality (Except Critical Requests):** When the `FairRequestQueue` reaches its `totalCapacity`, requests will be preempted based on a strict, well-ordered criticality hierarchy (Sheddable < Default < Critical).  Crucially, *Critical requests cannot be preempted*, guaranteeing their processing even under extreme load.  Sheddable requests are preempted first, followed by Default requests if necessary. A double-ended min-max heap will efficiently manage the queue and identify preemption candidates. The preemption algorithm will prioritize the least recently preempted flow of a given criticality level, distributing the impact of preemption more evenly.
* **Cascading WFQ Dequeueing:**  The WFQ algorithm will be applied *within each criticality level* to maintain fairness among requests of the same priority. The dequeueing process cascades as follows:
    1. **Critical:** The WFQ algorithm selects the next request from the Critical queue.
    2. **Default (if no Critical requests available):** If the Critical queue is empty, WFQ selects the next request from the Default queue.
    3. **Sheddable (if no Critical or Default requests available):** If both Critical and Default queues are empty, WFQ selects the next request from the Sheddable queue.

    This approach ensures fairness within each criticality level but does *not* guarantee fairness across levels.  Under sustained high load of a higher priority level, lower-priority requests may experience starvation.  Future enhancements could address this by incorporating weights for each criticality level during dequeue selection (e.g., 70% Critical, 20% Default, 10% Sheddable).
* **Guaranteed Capacity for Critical Flows:** Critical flows are guaranteed a minimum reserved capacity within the `FairRequestQueue`, controlled by the `flowCapacity` parameter.  This reservation ensures that critical requests can always be enqueued, up to their allocated capacity, regardless of the queue's overall fill level.  The `ErrTooManyFlows` error will *only* be triggered if the sum of `flowCapacity` for all critical flows exceeds the `totalCapacity` of the queue.  Non-critical flows do not have a capacity guarantee and may be preempted if the queue reaches its `totalCapacity`.

**Benefits:**

* **Eliminates Starvation (Mostly):** This preemption-based strategy significantly reduces the risk of starvation for non-critical requests compared to the initial rejection-based approach. However, starvation is still possible under sustained high load of a higher priority level, as WFQ operates within, but not across, criticality levels.
* **Strict Critical Request Prioritization:**  Guarantees processing of critical requests, even under extreme overload conditions, by preventing their preemption.
* **More Nuanced Prioritization and Fairness:**  Provides more fine-grained control over prioritization and fairness by combining preemption with WFQ within each criticality level.
* **Improved Resource Utilization:** Enqueues all requests and dynamically manages their priorities, leading to better overall resource utilization compared to rejecting non-critical requests outright.

**Implementation Details:**

* **`FairRequestQueue`:** The `FairRequestQueue` implementation will be modified to:
    - Utilize a double-ended min-max heap to efficiently manage requests and facilitate preemption based on criticality and LRU within each level.
    - Implement the cascading WFQ selection logic.
* **`RequestQueueController`:** The controller will be updated to integrate with the enhanced `FairRequestQueue` and manage the preemption mechanism.
* **Metrics:** New metrics will track preemption rates for each criticality level, providing insights into system behavior and potential starvation issues.

This preemption-based approach presents a significant advancement in the queuing system's ability to manage diverse workloads with varying priorities. It balances fairness within criticality levels with strict prioritization of critical requests, while also laying the groundwork for future enhancements like weighted cross-criticality selection to further mitigate starvation.

**2. Enhanced Backpressure and Flow Isolation:**

* **Flow-Specific Concurrency Limits:** Implement limits on the number of concurrent requests *per flow* at each backend to address the current limitation of flow isolation. This will prevent a single flow from monopolizing backend resources, even if WFQ provides fair access to the queue. This will require coordination between the queueing system and the scheduler, potentially through shared metrics or a feedback mechanism.  Challenges include efficiently tracking concurrent requests per flow across all backends and potential performance overhead.
* **Explicit Backpressure Signaling:** Implement a mechanism to signal backpressure to upstream components (e.g., Envoy) when the queue becomes congested or backends are overloaded. This could involve exposing metrics or status indicators that Envoy can monitor and use to adjust its request forwarding behavior.  This would enhance the system's responsiveness to overload conditions.  Challenges include defining appropriate backpressure signals and ensuring their timely and reliable communication to Envoy.

**3. Queue Management and Optimization:**

* **Dynamic Popper Adjustment:**  Implement dynamic adjustment of the number of popper goroutines based on queue length, stock level, and other relevant system metrics.  This will optimize resource utilization and improve responsiveness to changing load conditions. Challenges include defining the adjustment algorithm and ensuring it doesn't introduce instability.
* **Queue Data Structure Optimization:** Investigate alternative queue implementations (e.g., lock-free queues) to further improve performance and reduce contention under high load.  This requires careful benchmarking and analysis to ensure the chosen data structure meets the system's performance and concurrency requirements.  Challenges include ensuring thread safety and correctness of the alternative implementation.

**4. Scheduler Integration and Advanced Features:**

* **Enhanced Scheduler Integration:**  Explore tighter integration with the scheduler to share information about backend load and queue status. This information sharing could enable more informed scheduling decisions and more efficient resource utilization across the system.  Challenges include defining the interface for information exchange between the scheduler and the queuing system and ensuring that this integration doesn't introduce excessive complexity or overhead.

## Glossary

* **`FairRequestQueue`:** Implements Weighted Fair Queuing (WFQ) to manage queued requests.
* **Flow:** A stream of requests for a specific model.
* **Hysteresis Band:** The range between `lowerBound` and `upperBound` around the `watermark`, used to smooth queue behavior and prevent oscillations in stock.
* **Popper Goroutine:** Dequeues requests from the `FairRequestQueue` when the stock falls below the `lowerBound`.
* **`RequestQueueController`:** Manages the queue, makes enqueue/bypass decisions, and tracks stock.
* **Stock:** Number of requests currently being processed by backends (waiting, in-transit, or active).  A proxy for backend load.
* **`watermark`:** Target average stock level per backend. Used by the `RequestQueueController` for queueing decisions.
* **`lowerBound`:** Lower threshold of the hysteresis band.
* **`upperBound`:** Upper threshold of the hysteresis band.
* **WFQ (Weighted Fair Queuing):** Algorithm used by `FairRequestQueue` to prioritize requests and ensure fairness across flows.
