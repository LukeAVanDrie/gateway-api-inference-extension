/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package framework defines the core plugin interfaces for extending the Flow Controller. This includes contracts for
// policies, queues, and comparators, as well as framework-specific errors.
package framework

import "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"

// ItemComparatorFunc defines the function signature for comparing two `types.QueueItemAccessor` instances to determine
// their relative dispatch priority.
//
// An implementation of this function determines if item 'a' should be dispatched before item 'b'. It returns true if
// 'a' is of higher priority, and false otherwise. The specific criteria for "higher priority" (e.g., earlier deadline,
// lower enqueue time) are defined by the `IntraFlowDispatchPolicy` that vends this function via an `ItemComparator`.
type ItemComparatorFunc func(a, b types.QueueItemAccessor) bool

// ItemComparator encapsulates the logic for comparing two `types.QueueItemAccessor` instances to determine their
// relative dispatch priority. It is the definitive source of ordering truth for a flow's queue.
//
// It is vended by an `IntraFlowDispatchPolicy` and used by `SafeQueue` implementations that support the
// `CapabilityPriorityConfigurable` capability. It can also be used by inter-flow policies to compare items from
// different queues, provided their `ScoreType` is compatible.
//
// Design Justification: This design treats item priority as a relational concept defined by a policy, rather than a
// static attribute on the item itself. This allows for sophisticated, dynamic priority evaluation (e.g., based on
// real-time SLO attainment), as the comparison logic can be stateful.
type ItemComparator interface {
	// Func returns the core comparison logic as an `ItemComparatorFunc`.
	//
	// This function is the single source of truth for determining the relative priority between two items. A `SafeQueue`
	// that declares `CapabilityPriorityConfigurable` MUST use this function for its internal ordering. Inter-flow
	// policies MAY use this function to compare items from different queues after ensuring `ScoreType` compatibility.
	//
	// Conformance: MUST NOT return nil.
	Func() ItemComparatorFunc

	// ScoreType returns a string descriptor that defines the semantic meaning and domain of the comparison logic.
	//
	// A non-empty, descriptive string is required for two primary reasons:
	//  1. Comparability Check: Inter-flow policies that compare items across different queues (e.g., a "BestHead" policy)
	//     MUST check for identical `ScoreType` strings before using the comparator functions. A comparison is only
	//     meaningful if the underlying scoring logic is the same.
	//  2. Introspectability: The string makes the priority scheme human-readable for debugging and observability.
	//
	// Examples: "enqueue_time_ns_asc", "slo_urgency_score_desc".
	//
	// Future Considerations: While currently a simple string for initial simplicity, a future enhancement could introduce
	// a more structured `ScoreType`. Such a structure might explicitly encode ordering (ascending/descending) and value
	// semantics (e.g., time, custom_metric), potentially enabling advanced features like cross-`ScoreType` normalization
	// plugins.
	//
	// Conformance:
	//   - MUST return a non-empty, meaningful string that describes the domain or unit of comparison.
	//   - For the present, policies MUST NOT assume any implicit cross-`ScoreType` normalization capabilities.
	ScoreType() string
}

// InterFlowDispatchPolicy selects which flow's queue to service next from a given priority band. Implementations define
// the fairness or dispatch ordering logic between different flows that share the same priority level.
type InterFlowDispatchPolicy interface {
	// SelectQueue inspects the flow queues within the provided priority band and returns the `FlowQueueAccessor` of the
	// queue chosen for the next dispatch attempt.
	//
	// Returns:
	//   - The selected `FlowQueueAccessor` and a nil error if a queue is successfully chosen.
	//   - A nil accessor and a nil error if no queue is selected (e.g., all queues in the band are empty or the policy
	//     logic determines a pause is appropriate).
	//   - A nil accessor and a non-nil error if an unrecoverable error occurs. Policies should be resilient to transient
	//     issues (like a queue becoming empty during inspection) and select from other available queues if possible,
	//     rather than returning an error.
	//
	// Conformance: Implementations MUST be goroutine-safe if they maintain internal state.
	SelectQueue(band PriorityBandAccessor) (selectedQueue FlowQueueAccessor, err error)

	// RequiredQueueCapabilities returns a slice of capabilities that `SafeQueue` implementations associated with this
	// policy MUST support. Inter-flow policies should primarily require *structural* capabilities (e.g.,
	// `CapabilityDoubleEnded` for `PeekTail`) for inspection, as behavioral ordering is handled by the
	// `IntraFlowDispatchPolicy`.
	RequiredQueueCapabilities() []QueueCapability

	// Name returns the unique registered string identifier for this policy implementation.
	// Useful for debugging and introspection.
	// Examples (see `Policy Naming Conventions` in `README.md` for more details):
	//   - "RoundRobin"
	//   - "ShortestQueueFirst-Bytes"
	//   - "BestHead"
	Name() string
}

// IntraFlowDispatchPolicy selects a specific request to dispatch next from a single flow's queue. Implementations
// define the ordering of requests *within* a single flow.
type IntraFlowDispatchPolicy interface {
	// SelectItem inspects a flow's queue and returns the `types.QueueItemAccessor` of the item chosen for dispatch.
	//
	// For queues that inherently order items by dispatch preference (e.g., a priority queue configured with this
	// policy's `ItemComparator`), this method will typically just call `queue.PeekHead()`.
	//
	// The Flow Controller uses the handle from the returned item to instruct the `ports.ManagedQueue` to remove it.
	//
	// Returns:
	//   - The selected `types.QueueItemAccessor` and a nil error if an item is successfully chosen.
	//   - A nil accessor and a nil error if no item is selected (e.g., the queue is empty or the policy logic determines
	//     a pause is appropriate).
	//   - A nil accessor and a non-nil error if an unrecoverable error occurs.
	//
	// Conformance: Implementations MUST be goroutine-safe if they maintain internal state.
	SelectItem(queue FlowQueueAccessor) (selectedItem types.QueueItemAccessor, err error)

	// Comparator returns the `ItemComparator` that defines this policy's item ordering logic. This is the definitive
	// source for how items within a flow governed by this policy should be prioritized.
	//
	// A policy MUST provide a meaningful comparator even if it relies on a queue's inherent ordering (e.g., an FCFS
	// policy using a `CapabilityFIFO` queue should return a comparator based on enqueue time). This makes the ordering
	// principle explicit and available to other components, like inter-flow policies.
	//
	// Conformance: MUST NOT return nil.
	Comparator() ItemComparator

	// RequiredQueueCapabilities returns a slice of capabilities that the `SafeQueue` used with this policy MUST support.
	// This policy is responsible for defining the ordering of items within a flow and so it must require the relevant
	// *behavioral* capability (e.g., `CapabilityPriorityConfigurable` or `CapabilityFIFO`). The `ItemComparator` vended
	// by this policy then defines that behavior.
	RequiredQueueCapabilities() []QueueCapability

	// Name returns the unique registered string identifier for this policy implementation.
	// Useful for debugging and introspection.
	// Examples (see `Policy Naming Conventions` in `README.md` for more details):
	//   - "FCFS" (First-Come, First-Served)
	//   - "EDF-TTFT" (Earliest-Deadline-First for Time-To-First-Token)
	//   - "EDF-E2E" (Earliest-Deadline-First for E2E)
	//   - "ShortestRequestFirst-Bytes" ("SJF"--shortest job first--with a simple heuristic for LLMs)
	//   - "ShortestRequestFirst-Predictive" ("SJF" adapted for autoregressive nature of LLM serving)
	//   - "SLOViolationProbability-TTFT
	//   - "SLOViolationProbability-E2E"
	Name() string
}

// InterFlowDisplacementPolicy selects a victim flow's queue from a lower-priority band from which an item should be
// displaced. Implementations define the logic for choosing which flow to target when making space for a new request.
type InterFlowDisplacementPolicy interface {
	// SelectQueue inspects the flow queues within the provided priority band and returns the `FlowQueueAccessor` of the
	// queue chosen as the next displacement target.
	//
	// Returns:
	//   - The selected `FlowQueueAccessor` and a nil error if a victim queue is successfully chosen.
	//   - A nil accessor and a nil error if no queue is selected (e.g., all queues in the band are empty or the policy
	//     logic determines a pause is appropriate).
	//   - A nil accessor and a non-nil error if an unrecoverable error occurs. Policies should be resilient to transient
	//     issues (like a queue becoming empty during inspection) and select from other available queues if possible,
	//     rather than returning an error.
	//
	// Conformance: Implementations MUST be goroutine-safe if they maintain internal state.
	SelectQueue(victimBand PriorityBandAccessor) (victimQueue FlowQueueAccessor, err error)

	// RequiredQueueCapabilities returns a slice of capabilities that `SafeQueue` implementations associated with this
	// policy MUST support. This will typically be a *structural* capability (e.g., `CapabilityDoubleEnded`) needed
	// to inspect potential victims.
	RequiredQueueCapabilities() []QueueCapability

	// Name returns the unique registered string identifier for this policy implementation.
	// Useful for debugging and introspection.
	// Examples (see `Policy Naming Conventions` in `README.md` for more details):
	//   - "RoundRobin"
	//   - "LargestQueueFirst-Bytes"
	//   - "WorstTail"
	//   - "None"
	Name() string
}

// IntraFlowDisplacementPolicy selects a single victim item from within a specific flow's queue to be displaced.
// Implementations define which item is the best victim if that flow is targeted for displacement.
type IntraFlowDisplacementPolicy interface {
	// SelectItem inspects a flow's queue and returns the `types.QueueItemAccessor` of the item chosen for displacement.
	//
	// The Flow Controller uses the handle from the returned item to instruct the `ports.ManagedQueue` to remove it.
	//
	// Returns:
	//   - The selected `types.QueueItemAccessor` and a nil error if a victim is successfully chosen.
	//   - A nil accessor and a nil error if no victim is selected (e.g., the queue is empty or the policy logic
	//     determines a pause is appropriate).
	//   - A nil accessor and a non-nil error if an unrecoverable error occurs.
	//
	// Conformance: Implementations MUST be goroutine-safe if they maintain internal state.
	SelectItem(queue FlowQueueAccessor) (victimItem types.QueueItemAccessor, err error)

	// RequiredQueueCapabilities returns a slice of capabilities that the `SafeQueue` used with this policy MUST support.
	// This will typically be a *structural* capability (e.g., `CapabilityDoubleEnded` to inspect the tail, or
	// `CapabilityScannable` for more complex logic).
	RequiredQueueCapabilities() []QueueCapability
	// Name returns the unique registered string identifier for this policy implementation.
	// Useful for debugging and introspection.
	// Examples (see `Policy Naming Conventions` in `README.md` for more details):
	//   - "Tail"
	//   - "Largest-Bytes"
	//   - "None"
	Name() string
}
