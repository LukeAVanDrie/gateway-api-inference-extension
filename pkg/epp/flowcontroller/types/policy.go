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

package types

// ItemComparatorFunc defines the function signature for comparing two QueueItemAccessors to determine their relative
// dispatch priority.
//
// The function encapsulates the logic for "higher priority". It should return true if item 'a' is considered to have
// higher dispatch priority than item 'b', and false otherwise. The specific criteria for "higher priority" (e.g.,
// earlier deadline, higher SLO urgency, lower enqueue time) are determined by the specific IntraFlowDispatchPolicy that
// provides this function via an ItemComparator.
//
// This function operates on arbitrary QueueItemAccessors, enabling comparisons not just within a single queue, but
// potentially across items from different flows if their associated ItemComparators have compatible ScoreTypes.
type ItemComparatorFunc func(a, b QueueItemAccessor) bool

// ItemComparator defines the contract for an object that encapsulates both the logic (Func) and the semantic type
// (ScoreType) of item comparison for dispatch priority.
// It is provided by an IntraFlowDispatchPolicy (via its Comparator() method) and serves as the primary mechanism for
// defining item ordering within and potentially across flow queues.
//
// This approach makes item priority a policy-driven, relational concept rather than a static attribute of an item. It
// allows for dynamic priority evaluation if the providing policy is stateful, enabling sophisticated dispatch
// strategies (e.g., based on real-time SLO attainment or predicted completion times).
//
// Conformance:
//   - Implementations returned by IntraFlowDispatchPolicy.Comparator() MUST NOT be nil.
//   - The Func() method MUST return a non-nil ItemComparatorFunc.
//   - The ScoreType() method MUST return a non-empty, meaningful string that describes the domain or unit of comparison
//     (e.g., "nanoseconds_deadline_asc", "urgency_score_0_1_desc").
type ItemComparator interface {
	// Func returns the core comparison logic function.
	// This function is the single source of truth for determining the relative priority between two items according to
	// the policy that vends this comparator.
	// A SafeQueue that declares CapabilityPriorityConfigurable will use this function for its internal ordering.
	// Inter-flow policies might use this function (after checking ScoreType compatibility) to compare items from
	// different queues.
	Func() ItemComparatorFunc
	// ScoreType describes the semantic meaning and domain of the comparison defined by Func(). For example,
	// "enqueue_time_ns_asc" implies the Func compares enqueue timestamps, and lower values are higher priority.
	// "slo_urgency_desc" might imply a calculated urgency score where higher values are higher priority.
	//
	// This ScoreType is crucial for:
	//   1. Understanding: It makes the priority scheme human-understandable.
	//   2. Comparability: Inter-flow policies MUST check for ScoreType compatibility before attempting to compare items
	//      from different queues using their respective ItemComparators. Direct comparison is only meaningful for
	//      identical ScoreTypes. Policies should not assume any cross-ScoreType normalization exists unless explicitly
	//      documented by such a future extension.
	ScoreType() string
}

// InterFlowDispatchPolicy selects which flow's queue (identified by its FlowQueueAccessor) to service next from a given
// priority band.
// Implementations of this interface define the fairness or dispatch ordering logic between different flows sharing the
// same priority level.
type InterFlowDispatchPolicy interface {
	// SelectQueue inspects the queues within the provided priority band (using the PriorityBandAccessor) and returns the
	// FlowQueueAccessor of the flow queue chosen for the next dispatch attempt.
	//
	// Returns:
	//   - (selectedQueue, nil): If a queue is successfully selected.
	//   - (nil, nil): If no queue is selected at this moment (e.g., all eligible queues in the band are empty, or the
	//     policy determines a pause is needed).
	//   - (nil, error): If an irrecoverable error occurs that prevents selection.
	//     Policies should generally be resilient to transient issues (like a queue becoming empty during inspection) and
	//     attempt to select from other available queues if possible, rather than returning an error for such cases.
	//     An error might be returned for issues like ErrIncompatiblePriorityType if the policy cannot compare scores from
	//     different queues in the band.
	//
	// Conformance:
	//   - Implementations MUST be goroutine-safe if they maintain internal state.
	SelectQueue(band PriorityBandAccessor) (selectedQueue FlowQueueAccessor, err error)
	// RequiredQueueCapabilities returns a slice of capabilities that SafeQueue implementations within the band this
	// policy operates on MUST support for the policy to function correctly. The FlowRegistry will validate that new flows
	// added to a band meet these requirements. Inter-flow policies should primarily require *structural* capabilities
	// (e.g., CapabilityDoubleEnded for PeekTail) as behavioral ordering is typically understood via each queue's
	// ItemComparator (sourced from its IntraFlowDispatchPolicy).
	RequiredQueueCapabilities() []QueueCapability
	// Name returns the unique string identifier for this policy implementation (e.g., "RoundRobin",
	// "ShortestQueueFirst-Bytes").
	// Useful for debugging and introspection.
	Name() string
}

// IntraFlowDispatchPolicy selects a specific request (identified by its QueueItemAccessor) to dispatch next from a
// single given flow's queue.
// Implementations define the ordering of requests within a single flow.
type IntraFlowDispatchPolicy interface {
	// SelectItem inspects the given flow's queue (using the FlowQueueAccessor).
	// If it determines an item should be dispatched from this queue according to its policy, it returns the
	// QueueItemAccessor for that item.
	// Otherwise (e.g., queue is empty, or policy decides not to select an item now), it returns nil.
	//
	// For queues that inherently order items by dispatch preference (e.g., a priority queue where SafeQueue implements
	// CapabilityPriorityConfigurable and is configured with this policy's ItemComparator), this method might simply call
	// queue.PeekHead().
	//
	// The FlowController will use the handle from the returned QueueItemAccessor (via item.Handle()) to instruct the
	// ManagedQueue (and underlying SafeQueue) to remove the item for dispatch.
	//
	// Conformance:
	//   - Implementations MUST be goroutine-safe if they maintain internal state.
	SelectItem(queue FlowQueueAccessor) (selectedItem QueueItemAccessor)
	// Comparator returns the ItemComparator defining this policy's item ordering logic.
	// This comparator encapsulates both the comparison function and its semantic type.
	// It is the definitive source for how items within a flow governed by this policy should be prioritized for dispatch.
	//
	// Even policies for simple orderings like FCFS should provide a meaningful ItemComparator (e.g., based on enqueue
	// time) to make their ordering principle explicit and potentially usable by other components like inter-flow
	// policies.
	//
	// Conformance:
	//   - MUST NOT return nil.
	//   - The returned ItemComparator's Func() MUST NOT return nil.
	//   - The returned ItemComparator's ScoreType() MUST be non-empty and meaningful.
	Comparator() ItemComparator
	// RequiredQueueCapabilities returns a slice of capabilities that the SafeQueue used with this policy MUST support for
	// the policy to function correctly.
	// Example: A FIFO policy would require [CapabilityFIFO]. A policy providing an ItemComparator for a priority queue
	// would require [CapabilityPriorityConfigurable].
	RequiredQueueCapabilities() []QueueCapability
	// Name returns the unique string identifier for this policy implementation (e.g., "FIFO",
	// "ShortestJobFirst-PredictedCost").
	// Useful for debugging and introspection.
	Name() string
}

// InterFlowPreemptionPolicy selects a victim flow's queue (identified by its FlowQueueAccessor) from a target priority
// band (which is of strictly lower priority than the request needing space) to be considered for preemption.
type InterFlowPreemptionPolicy interface {
	// SelectVictimQueue inspects the queues within the victimBand. If a suitable queue to target for preemption is found
	// according to the policy's criteria, its FlowQueueAccessor is returned. Otherwise (e.g., all queues are empty, or no
	// queue meets preemption criteria), it returns nil.
	//
	// Returns:
	//   - (victimQueue, nil): If a victim queue is successfully selected.
	//   - (nil, nil): If no victim queue is selected from this band.
	//   - (nil, error): If an irrecoverable error occurs.
	//
	// Conformance:
	//   - Implementations MUST be goroutine-safe if they maintain internal state.
	SelectVictimQueue(victimBand PriorityBandAccessor) (victimQueue FlowQueueAccessor, err error)
	// RequiredQueueCapabilities returns a slice of capabilities that SafeQueue implementations within the band this
	// policy operates on MUST support for the policy to function correctly. The FlowRegistry will validate that new flows
	// added to a band meet these requirements. Inter-flow policies should primarily require *structural* capabilities
	// (e.g., CapabilityDoubleEnded for PeekTail) as behavioral ordering is typically understood via each queue's
	// ItemComparator (sourced from its IntraFlowDispatchPolicy).
	RequiredQueueCapabilities() []QueueCapability
	// Name returns the unique string identifier for this policy implementation (e.g., "LeastRecentlyDispatched",
	// "LargestQueueFirst-Bytes").
	// Useful for debugging and introspection.
	Name() string
}

// IntraFlowPreemptionPolicy selects a single victim item (identified by its QueueItemAccessor) from within a specific
// flow's queue to be preempted.
type IntraFlowPreemptionPolicy interface {
	// SelectVictim inspects the given queue (using the FlowQueueAccessor).
	// If a suitable victim item is found within this queue according to the policy's criteria (e.g., oldest item, largest
	// item, lowest internal priority item), its QueueItemAccessor is returned. Otherwise, it returns nil.
	//
	// The FlowController will use the handle from the returned QueueItemAccessor to instruct the ManagedQueue to remove
	// the item.
	//
	// Returns:
	//   - (victimItem, nil): If a victim item is successfully selected.
	//   - (nil, nil): If no victim item is selected from this queue.
	//   - (nil, error): If an irrecoverable error occurs.
	//
	// Conformance:
	//   - Implementations MUST be goroutine-safe if they maintain internal state.
	SelectVictim(queue FlowQueueAccessor) (victimItem QueueItemAccessor, err error)
	// RequiredQueueCapabilities returns a slice of capabilities that the SafeQueue used with this policy MUST support.
	// Example: An "EvictTail" policy might require [CapabilityDoubleEnded]. An "EvictLowestPriority" policy for a
	// priority queue would need [CapabilityPriorityConfigurable, CapabilityDoubleEnded].
	RequiredQueueCapabilities() []QueueCapability
	// Name returns the unique string identifier for this policy implementation (e.g., "EvictOldest", "EvictLargest").
	// Useful for debugging and introspection.
	Name() string
}
