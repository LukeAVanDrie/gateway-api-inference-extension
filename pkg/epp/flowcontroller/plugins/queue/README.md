# FlowController Queue Plugins (`plugins/queue/`)

This directory contains concrete implementations of the `types.SafeQueue` interface, which defines the contract for
core, self-contained queue data structures used by the FlowController.

## Overview

The FlowController manages requests by organizing them into queues. Each logical "flow" (e.g., representing a specific
model or workload) within a given priority band will have its own `types.ManagedQueue` instance, which wraps a
`types.SafeQueue`. This allows the FlowController to apply policies at both the inter-flow (across different flows) and
intra-flow (within a single flow's queue) levels.

The `types.SafeQueue` interface abstracts the underlying data structure and its specific ordering or access mechanisms.
This pluggable design, in conjunction with the `types.ManagedQueue` wrapper and `types.FlowQueueAccessor` for policy
inspection, allows for:

- **Different Queuing Disciplines**: While a basic FIFO (First-In, First-Out) queue (`ListQueue`) is provided as a
  default, future needs or specific policies might require other disciplines (e.g., priority queues, LIFO queues).
- **Specialized Capabilities**: Policies can declare `RequiredQueueCapabilities()` (e.g., `types.CapabilityFIFO`,
  `types.CapabilityPriorityConfigurable`, `types.CapabilityDoubleEnded`). The FlowController ensures that a policy is
  paired with a queue implementation that provides the necessary capabilities.
- **Performance Optimization**: Different queue implementations might offer varying performance characteristics suitable
  for different scales or workload patterns.
- **Future-Proofing**: This abstraction is key to potentially supporting more advanced in-memory queue structures (e.g.,
  min-max heaps for efficient double-ended priority queue operations) or even external, persistent, or distributed
  queues (e.g., Redis-backed) for scenarios requiring higher availability or different operational models (like offline
  batch processing or durable HA).

## Contributing a New `SafeQueue` Implementation

To contribute a new queue implementation:

1.  **Define Your Implementation**:
    - Create a new Go file in this directory (e.g., `mycustomqueue.go`).
    - Define a struct that will hold the state for your queue.  This struct should include any necessary fields for
      managing the queue's data and state (e.g., a slice or linked list for storing items, mutexes for synchronization).
    - Implement all methods of the `types.SafeQueue` interface on your struct. This includes:
      - `Add(item types.QueueItemAccessor) (uint64, uint64, error)`
      - `Remove(handle types.QueueItemHandle) (types.QueueItemAccessor, uint64, uint64, error)`
      - `CleanupExpired(currentTime time.Time, isItemExpired types.IsItemExpiredFunc) ([]types.ExpiredItemInfo, error)`
      - And all methods from the embedded `types.QueueInspectionMethods`:
        - `Len() int`
        - `ByteSize() uint64`
        - `Name() string` (return a unique name for your queue type)
        - `Capabilities() []types.QueueCapability` (declare what your queue can do)
        - `PeekHead() (types.QueueItemAccessor, error)`
        - `PeekTail() (types.QueueItemAccessor, error)`
      - You will also need to implement `types.QueueItemHandle` for the handles your queue issues (see `listqueue.go`
        for an example with `listItemHandle`).
    - Remember that all methods of `types.SafeQueue` (including those from the embedded `types.QueueInspectionMethods`
      and the write/mutating methods) MUST be goroutine-safe for concurrent access with respect to the queue's own
      internal data structures.
    - If your queue declares `types.CapabilityPriorityConfigurable`, it MUST use the `types.ItemComparator` passed to
      its constructor for ordering items.
    - The `ManagedQueue` wrapper provided by the FlowRegistry will handle higher-level serialization for write
      operations concerning FlowRegistry state and statistics.

2. **Register Your Queue**:
    - To make your queue discoverable by the system and automatically included in conformance tests, register it with
      the central factory. This is typically done in an `init()` function within your queue's Go file (e.g.,
      `mycustomqueue.go`).
    - Call `queue.RegisterQueue()` from [`plugins/queue/factory.go`](factory.go), passing your queue's unique name and a
      constructor function.
    - Define a `RegisteredQueueName` constant for your queue's name within your queue's Go file. This makes it easily
      referenceable.
    - Conformance tests in [`plugins/queue/conformance_test.go`](conformance_test.go) automatically iterate over all
      queues registered with the factory, so your queue will be included in these checks once registered.

3.  **Testing**:
    - **Conformance Tests**: The tests in [`plugins/queue/conformance_test.go`](conformance_test.go) are designed to
      verify that any `SafeQueue` implementation correctly adheres to the contractual obligations of the
      `types.SafeQueue` interface (e.g., behavior with empty queues, handle invalidation, state updates on
      Add/Remove/CleanupExpired, and ordering based on the provided `ItemComparator`). If you register your queue with
      the factory, it will automatically be covered by these tests.
    - **Implementation-Specific Tests (If Necessary)**: If your queue has unique internal logic or behaviors not
      directly covered by the `SafeQueue` interface (e.g., specific constructor validations, internal data structure
      invariants not exposed via the interface), you can add a test file (e.g., `mycustomqueue_test.go`) for these.
      However, for simple queues like `ListQueue`, the conformance tests are often sufficient.

4.  **Documentation**:
    - Add GoDoc comments to your new queue type and its methods, explaining its behavior, any specific capabilities, and
      its intended use cases.

## Example Implementation

Refer to:

- [`listqueue.go`](listqueue.go): For an example of a FIFO queue based on `container/list`.
- [`conformance_test.go`](conformance_test.go): To understand the baseline behaviors tested for all queues.

By following these steps, you can integrate new queueing mechanisms into the FlowController, enabling more sophisticated
and adaptable request management.
