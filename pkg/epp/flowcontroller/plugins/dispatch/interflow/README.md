# FlowController Inter-Flow Dispatch Policy Plugins (`plugins/dispatch/interflow/`)

This directory contains concrete implementations of the `types.InterFlowDispatchPolicy` interface. These policies are responsible for selecting *which flow's queue* to service next from all eligible flows within a single flow priority band.

## Overview

The FlowController manages requests organized into **flow priority bands** based on each flow's `FlowSpecification.Priority()`. Within each priority band, multiple "flows" (representing different models, tenants, or workloads) can exist, each with its own queue of requests. The `InterFlowDispatchPolicy` determines the fairness or priority mechanism for choosing among these competing flows within that specific priority band.

Key responsibilities and characteristics of an `InterFlowDispatchPolicy`:

1.  **Flow Queue Selection (`SelectQueue`)**: The primary method, `SelectQueue(band types.PriorityBandAccessor) (types.FlowQueueAccessor, error)`, inspects all the flow queues within the given priority band (via a read-only accessor) and returns the `FlowQueueAccessor` of the chosen flow.
    - If no flow is selected (e.g., all queues are empty, or the policy decides to pause), it returns `(nil, nil)`.
    - If an irrecoverable issue occurs that prevents selection (e.g., a `PriorityScoreType` mismatch when comparing queues), it returns `(nil, relevantError)`.
    - Implementations should also aim to be resilient to transient issues with individual queues (e.g., a temporary failure to `PeekHead`), attempting to select from other available queues before returning an error.

2.  **Fairness Criteria**: Policies can implement various fairness criteria:
    - **Score-based**: Like `BestHeadPriorityScore`, which looks at the `PriorityScore()` of the head item of each queue (as defined by each queue's `IntraFlowDispatchPolicy`) and picks the flow with the "best" score (e.g., numerically lowest, per convention). This type of policy must be careful about comparing scores from queues with different `PriorityScoreTypes`.
    - **Structural/Round-Robin**: Policies like Round Robin cycle through available flow queues without necessarily inspecting item scores.
    - **Consumption-based**: More advanced policies could track historical dispatch counts, token counts, or other resource consumption metrics per flow to make fairness decisions (e.g., [VTC-like algorithms](https://arxiv.org/abs/2401.00588) (Sheng et al.)).

3.  **Stateless or Stateful**: Policies can be stateless (making decisions based only on the current snapshot of the band) or stateful (e.g., remembering the last flow selected for Round Robin, or tracking consumption metrics).

The `InterFlowDispatchPolicy` is crucial for ensuring that different workloads sharing an inference pool receive equitable opportunities for their requests to be processed, according to the configured fairness objectives for that priority level. It works in conjunction with `IntraFlowDispatchPolicy` (which defines order within a flow's queue) to determine the overall dispatch order.

## Contributing a New Inter-Flow Dispatch Policy

To contribute a new inter-flow dispatch policy:

1.  **Define Your Policy Implementation**:
    - Create a new Go file in this directory (e.g., `myfairnesspolicy.go`).
    - Define a struct for your policy. If it's stateful, include fields to hold that state.
    - Implement all methods of the `types.InterFlowDispatchPolicy` interface on your struct:
      - `SelectQueue(band types.PriorityBandAccessor) (types.FlowQueueAccessor, error)`
      - `Name() string` (return a unique name for your policy type)

2.  **Register Your Policy**:
    - To make your policy discoverable by the system and automatically included in conformance tests, register it with the central factory. This is typically done in an `init()` function within your policy's Go file (e.g., `myfairnesspolicy.go`).
    - Call `interflowdispatch.RegisterPolicy()` from [`plugins/dispatch/interflow/factory.go`](factory.go), passing your policy's unique name and a constructor function.
    - If your policy is intended to be a generally available type (e.g., one of the default options for the system), define its `RegisteredInterFlowDispatchPolicyName` constant within your policy's Go file. This makes it easily referenceable from configurations or other parts of the system.
    - Conformance tests in [`plugins/dispatch/interflow/conformance_test.go`](conformance_test.go) automatically iterate over all policies registered with the factory, so your policy will be included in these checks once registered.

3.  **Testing**:
    - **Conformance Tests**: The tests in [`plugins/dispatch/interflow/conformance_test.go`](conformance_test.go) verify that any `InterFlowDispatchPolicy` implementation adheres to the basic contractual obligations of the interface (e.g., `Name()` is not empty, `SelectQueue` handles nil/empty bands gracefully). Registering your policy with the factory (as described in step 2) will automatically include it in these checks.
    - **Implementation-Specific Tests**: Create a new test file (e.g., `myfairnesspolicy_test.go`) in this directory. Add unit tests that cover the unique logic of your `SelectQueue` method, including how it handles different queue states, score comparisons (if applicable), error conditions (like `PriorityScoreType` mismatches), and state updates (for stateful policies).

4.  **Documentation**:
    - Add GoDoc comments to your new policy struct and its methods, explaining its selection logic, fairness criteria, any state it maintains, and its intended use cases.

## Example Implementation

Refer to:

- [`bestheadpriorityscore.go`](bestheadpriorityscore.go): For an example of a policy that selects based on the priority scores of head items.
- [`bestheadpriorityscore_test.go`](bestheadpriorityscore_test.go): For examples of implementation-specific tests.
- [`conformance_test.go`](conformance_test.go): To understand the baseline behaviors tested for all inter-flow dispatch policies.

By following these steps, you can introduce new strategies for arbitrating access between different flows, enhancing the fairness and flexibility of the FlowController.
