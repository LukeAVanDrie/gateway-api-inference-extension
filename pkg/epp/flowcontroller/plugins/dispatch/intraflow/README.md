# FlowController Intra-Flow Dispatch Policy Plugins (`plugins/dispatch/intraflow/`)

This directory contains concrete implementations of the `types.IntraFlowDispatchPolicy` interface. These policies are responsible for determining the order in which requests are selected for dispatch *from within a single flow's queue*.

## Overview

The FlowController processes requests belonging to different logical "flows" (e.g., different models or workloads). These flows are first organized by the FlowController into **flow priority bands** based on their `FlowSpecification.Priority()`. Within each such band, each individual flow has its own queue. The `IntraFlowDispatchPolicy` assigned to a flow's queue defines the "local" scheduling or ordering discipline for requests that share the same `FlowID` (and thus the same high-level flow priority).

Key responsibilities and characteristics of an `IntraFlowDispatchPolicy`:

1.  **Request Selection (`SelectItem`)**: The primary method, `SelectItem(queue types.FlowQueueAccessor) types.QueueItemAccessor`, inspects the given flow's queue (via a read-only accessor) and decides which item, if any, should be dispatched nex from *that specific queue*.

2.  **Priority Definition (`PriorityScoreType`, `ItemComparator`)**:
    - This policy type is unique because it defines the nature of priority for items *within its specific managed queue*. This "item priority" or "score" is distinct from the overall "flow priority" itself and operates within this broader priority determined by `FlowSpecification.Priority()` which is used by the FlowController to assign flows to priority bands.
    - It declares a `PriorityScoreType()` string (e.g., `"enqueue_time_ns"`, `"slo_deadline_urgency"`) that signifies how `QueueItemAccessor.PriorityScore()` should be interpreted for items in queues governed by this policy.
    - If the policy requires a queue that can order items based on a custom comparison logic (rather than relying on simple FIFO/LIFO), it must provide an `ItemComparator()` function and declare `types.CapabilityPriorityConfigurable` in its `RequiredQueueCapabilities()`. The FlowController would then use this comparator to configure a suitable priority queue. For simpler policies like FCFS that rely on inherent queue ordering (e.g., `ListQueue`), `ItemComparator()` can return `nil`.

3.  **Queue Compatibility (`RequiredQueueCapabilities`)**: The policy specifies the capabilities its associated `FlowQueue` must support for it to function correctly (e.g., `types.CapabilityFIFO` for an FCFS policy, `types.CapabilityPriorityConfigurable` for a policy that uses a custom comparator).

The `IntraFlowDispatchPolicy` allows for fine-grained control over how individual requests within a single flow are serviced, enabling strategies like basic FCFS, or more advanced schemes based on SLOs, deadlines, or predicted request costs for items belonging to that flow. This policy operates *after* the `InterFlowDispatchPolicy` has selected which flow's queue (from a given flow priority band) gets the next dispatch opportunity.

## Contributing a New Intra-Flow Dispatch Policy

To contribute a new intra-flow dispatch policy:

1.  **Define Your Policy Implementation**:
    - Create a new Go file in this directory (e.g., `mycustompolicy.go`).
    - Define a struct for your policy.
    - Implement all methods of the `types.IntraFlowDispatchPolicy` interface on your struct:
      - `SelectItem(queue types.FlowQueueAccessor) types.QueueItemAccessor`
      - `ItemComparator() types.ItemComparator` (return `nil` if not using a custom priority queue)
      - `PriorityScoreType() string` (define a clear string for your score type)
      - `RequiredQueueCapabilities() []types.QueueCapability`
      - `Name() string` (return a unique name for your policy type)

2.  **Register Your Policy (Optional but Recommended for Defaults)**:
    - To make your policy discoverable by the system and automatically included in conformance tests, register it with the central factory. This is typically done in an `init()` function within your policy's Go file (e.g., `mycustompolicy.go`).
    - Call `intraflowdispatch.RegisterPolicy()` from [`plugins/dispatch/intraflow/factory.go`](factory.go), passing your policy's unique name and a constructor function.
    - If your policy is intended to be a generally available type (e.g., one of the default options for the system), define its `RegisteredIntraFlowDispatchPolicyName` constant within your policy's Go file. This makes it easily referenceable from configurations or other parts of the system.
    - Conformance tests in [`plugins/dispatch/intraflow/conformance_test.go`](conformance_test.go) automatically iterate over all policies registered with the factory, so your policy will be included in these checks once registered.

3.  **Testing**:
    - **Conformance Tests**: The tests in [`plugins/dispatch/intraflow/conformance_test.go`](conformance_test.go) verify that any `IntraFlowDispatchPolicy` implementation adheres to the basic contractual obligations of the interface (e.g., `Name()` is not empty, `SelectItem` handles nil/empty queues gracefully). Registering your policy will include it in these checks.
    - **Implementation-Specific Tests**: Create a new test file (e.g., `mycustompolicy_test.go`) in this directory. Add unit tests that cover the unique logic of your `SelectItem` method, the behavior of your `ItemComparator` (if any), and verify that `PriorityScoreType()` and `RequiredQueueCapabilities()` return the correct values.

4.  **Documentation**:
    - Add GoDoc comments to your new policy struct and its methods, explaining its selection logic, how it defines priority, any specific queue capabilities it relies on, and its intended use cases.

## Example Implementation

Refer to:

- [`fcfs.go`](fcfs.go): For an example of a simple FCFS policy.
- [`fcfs_test.go`](fcfs_test.go): For examples of implementation-specific tests for the FCFS policy.
- [`conformance_test.go`](conformance_test.go): To understand the baseline behaviors tested for all intra-flow dispatch policies.

By following these steps, you can introduce new per-flow request ordering strategies into the FlowController, enabling more tailored and sophisticated scheduling behaviors.
