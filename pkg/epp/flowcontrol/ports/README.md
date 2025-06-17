# Flow Controller Service Ports

This document describes the service interfaces in the `flowcontrol/ports` package. These interfaces define the clear,
decoupled boundaries between the core `FlowController` engine and its primary dependencies. Following a "Ports and
Adapters" architectural style, these interfaces act as the "ports" that separate the engine's operational logic from the
concrete "adapters" that implement its required services.

## Architectural Overview

The `FlowController` engine relies on two primary service ports to function:

1.  **The `FlowRegistry` System:** Exposed via the `FlowRegistry` interface, this system is the definitive control plane
    for all flow configuration, state, and lifecycle management.
2.  **The `SaturationDetector`:** Exposed via the `SaturationDetector` interface, this component provides a real-time
    signal indicating the load state of backends, which the `FlowController` uses to gate its dispatch decisions.

This decoupled design enables robust unit testing through dependency injection and allows the implementations of these
services to evolve independently of the core engine's logic.

---

## `FlowRegistry` System Interfaces

The `FlowRegistry` is the single source of truth for the entire flow control system. It is exposed through a composite
`FlowRegistry` interface, which is composed of interfaces tailored to its different consumers. A key feature of its
design is an internally sharded state, which allows the `FlowController`'s workers to operate in parallel with minimal
lock contention.

### `FlowRegistryAdmin` Interface

The `FlowRegistryAdmin` interface defines the contract for the global control plane. It is the single point of entry for
external systems (e.g., a Kubernetes operator) to configure flows, manage the system's parallelism (sharding), and query
aggregated statistics.

#### Primary Responsibilities

* **Flow Lifecycle Management:** Orchestrates the registration (`RegisterOrUpdateFlow`), unregistration
  (`UnregisterFlow`), and dynamic updates of all logical flows.
* **Shard Management:** Exposes the `UpdateShardCount` method to manage the lifecycle and parallelism of internal state
  shards.
* **Observability:** Provides a unified view of the system's state through globally aggregated (`Stats`) and per-shard
  (`ShardStats`) metrics.

#### Design Rationale for Dynamic Update Strategies

The `FlowRegistryAdmin` contract specifies precise behaviors for handling dynamic updates. These strategies were chosen
to prioritize system stability, correctness, and minimal disruption:

* **Graceful Draining (for Priority/Shard Lifecycle Changes)**: For operations that change a flow's priority or
  decommission a shard, the affected queue instances are marked as inactive but are not immediately deleted. They enter
  a "draining" state where they no longer accept new requests but are still processed for dispatch. This ensures that
  requests already accepted by the system are processed to completion. Crucially, requests in a draining queue continue
  to be dispatched according to the priority level and policies they were enqueued with, ensuring consistency.

* **Atomic Queue Migration (for Incompatible Intra-Flow Policy Changes):** In contrast, when an intra-flow policy is
  updated to one that is incompatible with the existing queue data structure, a full "drain and re-enqueue" migration is
  performed. This more disruptive operation is necessary to guarantee correctness. A simpler "graceful drain"—by
  creating a second instance of the same flow in the same priority band—is not used because it would violate the
  system's "one flow instance per band" invariant. This invariant is critical because it ensures that inter-flow
  policies operate on a clean set of distinct flows, stateful intra-flow policies have a single authoritative view of
  their flow's state, and lookups are unambiguous.

* **Self-Balancing on Shard Scale-Up:** When new shards are added via `UpdateShardCount`, the framework relies on the
  `FlowController`'s request distribution logic (e.g., a "Join the Shortest Queue by Bytes (JSQ-Bytes)" strategy) to
  naturally funnel *new* requests to the less-loaded shards. This design choice strategically avoids the complexity of
  actively migrating or rebalancing existing items that are already queued on other shards, promoting system stability
  during scaling events.

### `ShardProvider` Interface

The `ShardProvider` interface defines the minimal contract needed by the `FlowController` engine to discover its
operational units (`RegistryShard` instances).

### Supporting Interfaces

#### `RegistryShard` Interface

The `RegistryShard` interface defines a tailored, operational view into a single internal shard of the `FlowRegistry`'s
state. It serves as the primary port for a single `FlowController` worker, enforcing the principle of least privilege by
exposing only the state and operations relevant to that shard.

#### `ManagedQueue` Interface

The `ManagedQueue` interface is the object that the `FlowController` engine uses to directly interact with a flow's
queue on a specific shard. It acts as a safe, state-aware wrapper around a `framework.SafeQueue` implementation, adding
lifecycle validation and atomic statistics updates.

---

## `SaturationDetector` Interface

The `SaturationDetector` interface defines the contract for the component that provides real-time backend load signals
to the `FlowController`.

Its core responsibility is to abstract away the complexity of determining system saturation. It consumes backend metrics
(e.g., queue depths, KV cache utilization, observed latencies) and translates them into a simple boolean signal. The
`FlowController` engine consumes this signal via the `IsSaturated` method to make crucial decisions, such as pausing
dispatch operations to prevent overwhelming backend resources.
