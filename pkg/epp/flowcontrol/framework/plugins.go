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

package framework

import (
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

// Plugin Extension Point Names
const (
	SafeQueueExtensionPoint               = "SafeQueue"
	ItemComparatorExtensionPoint          = "ItemComparator"
	IntraFlowDispatchPolicyExtensionPoint = "IntraFlowDispatchPolicy"
	InterFlowDispatchPolicyExtensionPoint = "InterFlowDispatchPolicy"
)

// ItemComparator encapsulates the logic for comparing two items to determine their relative dispatch priority.
// It is the definitive source of ordering truth.
//
// This interface is provided by an IntraFlowDispatchPolicy to make its internal ordering logic explicit and available
// to other components. For example, an InterFlowDispatchPolicy might use the comparators from two different queues to
// decide which one has the higher-priority item at its head.
//
// Design Justification: This design treats item priority as a relational concept defined by a policy, rather than a
// static attribute on the item itself. This allows for sophisticated, dynamic priority evaluation (e.g., based on
// real-time SLO attainment), as the comparison logic can be stateful.
type ItemComparator interface {
	plugins.Plugin

	// Func returns the core comparison logic as an ItemComparatorFunc.
	// This function is the single source of truth for determining if item 'a' has a higher priority than item 'b'.
	// A SafeQueue that declares the CapabilityPriorityConfigurable capability MUST use this function for its internal
	// ordering.
	//
	// Conformance: The returned function MUST NOT be nil.
	Func() ItemComparatorFunc

	// ScoreType returns a string descriptor that defines the semantic meaning and domain of the comparison logic.
	// A non-empty, descriptive string (e.g., "enqueue_time_ns_asc") is required for two reasons:
	//
	// 1. Comparability Check: An InterFlowDispatchPolicy that compares items across different queues (e.g., a "BestHead"
	//    policy) MUST verify that their ScoreType strings are identical before comparing them. A comparison is only
	//    meaningful if the underlying scoring logic is the same.
	// 2. Introspectability: The string makes the priority scheme human-readable for debugging and observability.
	//
	// Conformance: MUST return a non-empty, meaningful string. Policies MUST NOT assume any implicit cross-ScoreType
	// normalization capabilities.
	ScoreType() string
}

// IntraFlowDispatchPolicy selects which request to dispatch next from *within* a single flow's queue.
// Implementations define the ordering of requests for one stream (e.g., First-Come, First-Served).
type IntraFlowDispatchPolicy interface {
	plugins.Plugin

	// SelectItem inspects a flow's queue and returns the item chosen for dispatch.
	// For queues that inherently order items by dispatch preference (e.g., a priority heap), this method will typically
	// just call queue.PeekHead().
	//
	// The caller is responsible for using the handle from the returned item to instruct the underlying queue to remove
	// it.
	//
	// A return of (nil, nil) indicates that no item was selected (e.g., the queue is empty), which is not considered an
	// error.
	//
	// Conformance: Implementations MUST be goroutine-safe.
	SelectItem(queue FlowQueueAccessor) (selectedItem types.QueueItemAccessor, err error)

	// Comparator returns the ItemComparator that defines this policy's item ordering logic.
	// A policy MUST provide a meaningful comparator, as this makes the ordering principle explicit and available to other
	// components, like inter-flow policies.
	//
	// Conformance: MUST NOT return nil.
	Comparator() ItemComparator

	// RequiredQueueCapabilities returns the set of capabilities that a SafeQueue MUST support to be compatible with this
	// policy.
	// This allows for static validation of the system configuration, preventing runtime errors from mismatched policy and
	// queue types.
	RequiredQueueCapabilities() []QueueCapability
}

// InterFlowDispatchPolicy selects which flow's queue to service next from a given priority band.
// Implementations define the fairness or dispatch ordering logic *between* different flows sharing the same priority
// level.
type InterFlowDispatchPolicy interface {
	plugins.Plugin

	// SelectQueue inspects the flow queues within the provided PriorityBandAccessor and returns the queue chosen for the
	// next dispatch attempt.
	//
	// A return of (nil, nil) indicates that no queue was selected (e.g., all queues in the band are empty), which is not
	// considered an error.
	//
	// Conformance: Implementations MUST be goroutine-safe.
	SelectQueue(band PriorityBandAccessor) (selectedQueue FlowQueueAccessor, err error)
}

// --- SafeQueue Plugin and Supporting Types ---

// SafeQueue defines the contract for a single, concurrent-safe queue that holds items for a flow.
// All implementations MUST be goroutine-safe.
type SafeQueue interface {
	plugins.Plugin
	QueueInspectionMethods

	// Add attempts to enqueue an item.
	//
	// Conformance: On success, the implementation MUST create a new, unique types.QueueItemHandle, associate it with the
	// enqueued item, and attach it to the item by calling item.SetHandle(). This handle serves as an opaque token that
	// uniquely identifies the item's residency in this queue instance.
	//
	// It returns ErrNilQueueItem if the provided item is nil.
	Add(item types.QueueItemAccessor) error

	// Remove atomically finds and removes the item identified by the given handle.
	//
	// Conformance: On success, implementations MUST invalidate the provided handle by calling handle.Invalidate().
	//
	// It returns ErrInvalidQueueItemHandle if the handle is invalid (e.g., nil, wrong type, or created by a different
	// queue).
	// It returns ErrQueueItemNotFound if the handle is valid but the item is not in the queue.
	Remove(handle types.QueueItemHandle) (removedItem types.QueueItemAccessor, err error)

	// Cleanup iterates through the queue and atomically removes all items for which the predicate returns true.
	//
	// Conformance: The handle for each removed item MUST be invalidated.
	Cleanup(predicate PredicateFunc) (cleanedItems []types.QueueItemAccessor, err error)

	// Drain atomically removes all items from the queue.
	//
	// Conformance: The handle for all removed items MUST be invalidated. The queue MUST be empty after this operation
	// completes.
	Drain() (drainedItems []types.QueueItemAccessor, err error)
}

// QueueInspectionMethods defines the read-only methods of a SafeQueue.
// This interface is embedded in both SafeQueue and FlowQueueAccessor to provide a consistent, non-mutating view of a
// queue's state.
type QueueInspectionMethods interface {
	// Capabilities returns the set of functional capabilities this queue provides.
	Capabilities() []QueueCapability

	// Len returns the current number of items in the queue.
	Len() int

	// ByteSize returns the current total byte size of all items in the queue.
	ByteSize() uint64

	// PeekHead returns the item at the head of the queue (the one with the highest priority) without removing it.
	// It returns ErrQueueEmpty if the queue is empty.
	PeekHead() (peekedItem types.QueueItemAccessor, err error)

	// PeekTail returns the item at the tail of the queue (the one with the lowest priority) without removing it.
	// It returns ErrQueueEmpty if the queue is empty.
	PeekTail() (peekedItem types.QueueItemAccessor, err error)
}
