# FlowController Inter-Flow Preemption Policy Plugins (`plugins/preemption/interflow/`)

This directory contains concrete implementations of the `types.InterFlowPreemptionPolicy` interface. These policies are
responsible for selecting *which flow's queue* to target for preemption when the FlowController needs to make space for
a higher-priority request and has decided to look for victims in a specific lower flow priority band.

## Overview

When an incoming request cannot be enqueued due to capacity limits (either per-flow-priority-band or global), the
FlowController's preemption logic is triggered. It iterates through flow priority bands *lower* than that of the
incoming request, starting from the very lowest. For each such "victim band," it invokes the configured
`InterFlowPreemptionPolicy`.

The role of the `InterFlowPreemptionPolicy` is to inspect the victim band and decide which specific flow (i.e., which
flow's queue) within that band should be considered for preemption. If this policy selects a flow's queue, the
FlowController then uses an `IntraFlowPreemptionPolicy` (e.g., "Tail") to pick an actual item from that chosen queue.

Key responsibilities and characteristics of an `InterFlowPreemptionPolicy`:

1.  **Victim Flow Queue Selection (`SelectVictimQueue`)**: The primary method,
    `SelectVictimQueue(victimBand types.PriorityBandAccessor) (types.FlowQueueAccessor, error)`, inspects all flow
    queues within the given `victimBand` (which is of strictly lower priority than the preemptor) and returns the
    `FlowQueueAccessor` of the flow queue chosen as the target.
    - If no suitable flow queue is found (e.g., all queues in the band are empty, or the policy decides not to select\
      any), it returns `(nil, nil)`.
    - An error should generally not be returned unless there's an unexpected issue with the policy's execution.

2.  **Selection Strategy**: Policies can implement various strategies for choosing a victim flow:
    - **Structural/Iterative**: Like `RoundRobin`, which cycles through the available non-empty flow queues in the band
      to distribute preemption attempts.
    - **Attribute-based**: Policies could target flows based on attributes like current queue length
      (`FlowQueueAccessor.Len()`), total byte size (`FlowQueueAccessor.ByteSize()`), or historical metrics like least
      recently serviced.
    - **Score-based**: Policies could potentially peek at items (e.g., head/tail) in different queues and use their
      respective `ItemComparator`s (obtained via `FlowQueueAccessor.Comparator()`) to assess preemptability, assuming
      compatible `ScoreType`s and a clear convention for what constitutes "worst" for preemption. This often overlaps
      with dispatch logic.
    - **Consumption-based**: More advanced policies might track historical resource consumption or dispatch counts per
      flow to make fairness-oriented preemption decisions.

3.  **Stateless or Stateful**: Policies like Round Robin are stateful (they need to remember the last selected flow per
    band). Others might be stateless.

This policy allows the FlowController to strategically decide which broader workload/flow should bear the cost of
preemption.

## Contributing a New Inter-Flow Preemption Policy

To contribute a new inter-flow preemption policy:

1.  **Define Your Policy Implementation**:
    - Create a new Go file in this directory (e.g., `mylargeflowpreemption.go`).
    - Define a struct for your policy, including any state it needs to maintain (e.g., for Round Robin). Remember to
      handle concurrency if state is shared.
    - Implement the `types.InterFlowPreemptionPolicy` interface:
      - `SelectVictimQueue(victimBand types.PriorityBandAccessor) (types.FlowQueueAccessor, error)`
      - `Name() string`

2.  **Register Your Policy**:
    - To make your policy discoverable by the system and automatically included in conformance tests, register it with
      the central factory. This is typically done in an `init()` function within your policy's Go file (e.g.,
      `mylargeflowpreemption.go`).
    - Call `interflowpreemption.RegisterPolicy()` from [`plugins/preemption/interflow/factory.go`](factory.go), passing
      your policy's unique name and a constructor function.
    - If your policy is intended to be a generally available type (e.g., one of the default options for the system),
      define its `RegisteredInterFlowPreemptionPolicyName` constant within your policy's Go file. This makes it easily
      referenceable from configurations or other parts of the system.
    - Conformance tests in [`plugins/preemption/interflow/conformance_test.go`](conformance_test.go) automatically
      iterate over all policies registered with the factory, so your policy will be included in these checks once
      registered.

3.  **Testing**:
    - **Conformance Tests**: Ensure basic contractual obligations are met.
    - **Implementation-Specific Tests**: Test the unique logic of your `SelectVictimQueue` method, especially its state
      management and selection criteria across various band states.

4.  **Documentation**:
    - Add GoDoc comments explaining your policy's selection strategy.

## Example Implementation

Refer to:

- [`roundrobin.go`](roundrobin.go): For an example of a stateful round-robin policy.
- [`roundrobin_test.go`](roundrobin_test.go): For its specific tests.
- [`conformance_test.go`](conformance_test.go): For baseline behaviors.
