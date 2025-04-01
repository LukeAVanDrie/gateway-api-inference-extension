# FlowController Intra-Flow Preemption Policy Plugins (`plugins/preemption/intraflow/`)

This directory contains concrete implementations of the `types.IntraFlowPreemptionPolicy` interface. These policies are
responsible for selecting a specific request (victim) to preempt *from within a single flow's queue*. This typically
occurs when a higher-priority flow needs capacity, and the FlowController has decided to target a lower-priority flow
for preemption.

## Overview

When the FlowController determines that preemption is necessary to make space for an incoming request, it first uses an
`InterFlowPreemptionPolicy` to select a victim *flow's queue* from a lower flow priority band. Once a victim flow's
queue is chosen, the `IntraFlowPreemptionPolicy` associated with that flow (or a default for that priority band) is
invoked to pick the actual item to remove from that queue.

Key responsibilities and characteristics of an `IntraFlowPreemptionPolicy`:

1.  **Victim Selection (`SelectVictim`)**: The primary method,
    `SelectVictim(queue types.FlowQueueAccessor) (types.QueueItemAccessor, error)`, inspects the given flow's queue (via
    a read-only accessor) and returns the `QueueItemAccessor` of the item chosen for preemption. If no item is selected
    (e.g., the queue is empty, or the policy decides not to preempt from this queue despite being asked), it returns
    `(nil, nil)`. An error should only be returned for unexpected policy execution issues, not for failing to find a
    victim.

2.  **Preemption Logic**: Policies can implement various victim selection strategies:
    - **Structural (Current Scope)**: Given the current `types.FlowQueueAccessor` interface (which provides `PeekHead()`
    and `PeekTail()`), policies are primarily structural. For example, the `Tail` policy selects the item at the tail of
    the queue (often the newest in a FIFO queue). Similarly, a "Head" preemption policy could be implemented. These are
    generic to how item priority is defined for dispatch.
    - **Attribute-based (Future Work)**: To implement policies that select victims based on iterating through all items
    in the queue to find one based on attributes not necessarily used for dispatch ordering (e.g., selecting the largest
    or smallest request by `ByteSize()`, or an item with a specific characteristic from `OriginalRequest()`), the
    `types.FlowQueueAccessor` would need to be enhanced or wrapped (see Future Work). It is important to note that full
    queue iteration for preemption is a less efficient operation than typical dispatch (which often only inspects the
    head) and should be designed with this trade-off in mind; the efficiency of the primary dispatch path (intra-flow
    dispatch) remains paramount.

3.  **Queue Compatibility (`RequiredQueueCapabilities`)**: The policy specifies the capabilities its associated
    `SafeQueue` must support. For example, a policy that preempts from the tail (`Tail`) requires
    `types.CapabilityDoubleEnded`.

The `IntraFlowPreemptionPolicy` allows for fine-grained control over which specific request within a flow is sacrificed
during contention.

## Contributing a New Intra-Flow Preemption Policy

To contribute a new intra-flow preemption policy:

1.  **Define Your Policy Implementation**:
    - Create a new Go file in this directory (e.g., `mypreemptionpolicy.go`).
    - Define a struct for your policy.
    - Implement all methods of the `types.IntraFlowPreemptionPolicy` interface on your struct:
    - `SelectVictim(queue types.FlowQueueAccessor) (types.QueueItemAccessor, error)`
    - `RequiredQueueCapabilities() []types.QueueCapability`
    - `Name() string` (return a unique name for your policy type)

2.  **Register Your Policy**:
    - To make your policy discoverable by the system and automatically included in conformance tests, register it with
      the central factory. This is typically done in an `init()` function within your policy's Go file (e.g.,
      `mypreemptionpolicy.go`).
    - Call `intraflowpreemption.RegisterPolicy()` from [`plugins/preemption/intraflow/factory.go`](factory.go), passing
      your policy's unique name and a constructor function.
    - If your policy is intended to be a generally available type (e.g., one of the default options for the system),
      define its `RegisteredIntraFlowPreemptionPolicyName` constant within your policy's Go file. This makes it easily
      referenceable from configurations or other parts of the system.
    - Conformance tests in [`plugins/preemption/intraflow/conformance_test.go`](conformance_test.go) automatically
      iterate over all policies registered with the factory, so your policy will be included in these checks once
      registered.

3.  **Testing**:
    - **Conformance Tests**: The tests in [`plugins/preemption/intraflow/conformance_test.go`](conformance_test.go)
      verify basic contractual obligations. Registering your policy includes it in these checks.
    - **Implementation-Specific Tests**: Create a new test file (e.g., `mypreemptionpolicy_test.go`). Add unit tests
      covering your `SelectVictim` logic, especially how it interacts with different queue states and required
      capabilities.

4.  **Documentation**:
    - Add GoDoc comments explaining your policy's victim selection strategy and any specific queue capabilities it
      relies on.

## Example Implementation

Refer to:

-  [`tail.go`](tail.go): For an example of a policy that selects the tail item for preemption.
-  [`tail_test.go`](tail_test.go): For examples of implementation-specific tests.
-  [`conformance_test.go`](conformance_test.go): For baseline behaviors tested for all intra-flow preemption policies.

## Future Work

- **Iterable Queue Accessor**: For more advanced `IntraFlowPreemptionPolicy` implementations that need to inspect all
  items in a queue (e.g., to find an item based on attributes like its `ByteSize()` if the queue isn't already ordered
  by this, or to apply more complex selection logic across the entire queue), the `types.FlowQueueAccessor` would need
  to be enhanced, or a new `IterableFlowQueueAccessor` wrapper/interface could be introduced. This would allow policies
  to iterate through items beyond just peeking at the head or tail.

This allows for flexible and targeted preemption strategies within the FlowController.
