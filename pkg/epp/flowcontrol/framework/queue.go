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
)

// QueueCapability defines a functional capability that a `SafeQueue` implementation can provide. These capabilities
// allow policies to declare their operational requirements, which the Flow Registry then validates.
//
// Capabilities are broadly categorized as:
//  1. Structural: Describe methods a queue exposes (e.g., `CapabilityDoubleEnded` for `PeekTail`). These are the
//     primary concern of inter-flow and displacement policies that need to inspect queue structure.
//  2. Behavioral: Describe a queue's internal ordering logic (e.g., `CapabilityFIFO`,
//     `CapabilityPriorityConfigurable`). These are the primary concern of an `IntraFlowDispatchPolicy`, which dictates
//     the dispatch order for a flow. The `ItemComparator` provided by an `IntraFlowDispatchPolicy` then serves as the
//     standardized way for *any* policy (intra or inter-flow) to understand a queue's dispatch order, abstracting the
//     underlying behavioral implementation.
type QueueCapability string

const (
	// CapabilityFIFO indicates that the queue operates in a First-In, First-Out manner. `PeekHead()` will return the
	// oldest item. To remove it, its handle would be obtained and used with `Remove(handle)`.
	// Policies requiring strict FIFO ordering would specify this capability.
	// This is a *behavioral* capability.
	CapabilityFIFO QueueCapability = "FIFO"

	// CapabilityPriorityConfigurable indicates that the queue can be configured with an `ItemComparator` (typically
	// provided by an `IntraFlowDispatchPolicy`). Its `PeekHead()` will return the highest priority item according to this
	// comparator. To remove it, its handle would be obtained and used with `Remove(handle)`. This capability is essential
	// for implementing priority-based dispatch within a flow.
	// This is a *behavioral* capability.
	CapabilityPriorityConfigurable QueueCapability = "PriorityConfigurable"

	// CapabilityDoubleEnded indicates that the queue supports operations at both ends, specifically `PeekTail()`.
	// This is useful for policies that might need to inspect or target items at the "end" or "lowest priority" part of
	// the queue, such as certain displacement strategies.
	// This is a *structural* capability.
	CapabilityDoubleEnded QueueCapability = "DoubleEnded"

	// CapabilityDynamicPriority indicates that the queue's behavior correctly reflects changes in item priority over im
	// time, as defined by its configured `ItemComparator`. This is a behavioral promise: `PeekHead()` will endeavor to
	// return the highest-priority item, even if priorities change while items are queued.
	//
	// How a queue achieves this is an implementation detail. For queues that can support efficient, synchronous, targeted
	// updates, the optional `PriorityUpdater` interface provides an explicit trigger for re-ordering.
	//
	// This is a *behavioral* capability with optional, *structural* implications.
	CapabilityDynamicPriority QueueCapability = "DynamicPriority"

	// CapabilityScannable indicates that the queue supports the `Scan()` method, allowing for read-only inspection of its
	// contents based on a predicate. This capability should be used with care, as scanning can be an expensive operation.
	// This is a *structural* capability.
	CapabilityScannable QueueCapability = "Scannable"
)

// QueueInspectionMethods defines common read-only and content inspection methods shared by `SafeQueue` and
// `FlowQueueAccessor`.
//
// It is used as an embedded interface to ensure DRY (Don't Repeat Yourself).
type QueueInspectionMethods interface {
	// Len returns the current number of items in the queue.
	Len() int

	// ByteSize returns the current total byte size of all items in the queue (from `types.QueueItemAccessor.ByteSize()`).
	ByteSize() uint64

	// Name returns a string identifier for the concrete queue implementation type (e.g., "ListQueue", MinMaxHeap",
	// "RedisSortedSet").
	// Useful for debugging and introspection.
	Name() string

	// Capabilities returns the set of functional capabilities this queue instance provides.
	Capabilities() []QueueCapability

	// PeekHead returns the QueueItemAccessor for the item at the "head" of the queue (according to the queue's ordering)
	// without removing it.
	//
	// Returns:
	//   - (peekedItem, nil) if the queue is non-empty.
	//   - (nil, `ErrQueueEmpty`) if the queue is empty.
	PeekHead() (peekedItem types.QueueItemAccessor, err error)

	// PeekTail returns the QueueItemAccessor for the item at the "tail" of the queue without removing it, if the queue
	// supports this capability (`CapabilityDoubleEnded`).
	//
	// Returns:
	//   - (peekedItem, nil) if the queue is non-empty.
	//   - (nil, `ErrQueueEmpty`) if the queue is empty.
	//   - (nil, `ErrOperationNotSupported`) if not supported by the implementation.
	PeekTail() (peekedItem types.QueueItemAccessor, err error)
}

// PriorityUpdater is an optional interface that a `SafeQueue` can implement to support efficient, synchronous priority
// updates for specific items.
//
// The Flow Controller or Flow Registry can use a type assertion to check if a `SafeQueue` implements this interface.
// If it does, the system can call `UpdatePriority` to explicitly trigger a re-ordering operation when an item's
// priority is known to have changed, which is often more efficient than a full queue re-sort.
type PriorityUpdater interface {
	// UpdatePriority signals to the queue that the priority of the item associated with the given handle has changed.
	// The queue should then perform an efficient re-ordering operation (such as a heap "fix") to reflect the item's
	// new priority.
	//
	// This method may be called, for example, after a flow's `IntraFlowDispatchPolicy` is updated, changing the
	// `ItemComparator` logic.
	//
	// Returns:
	//   - error wrapping `ErrInvalidQueueItemHandle` if the handle is invalid (nil, wrong type, already invalidated).
	//   - error wrapping `ErrQueueItemNotFound`) if the handle is valid but the item is not found.
	//   - any other non-recoverable errors.
	UpdatePriority(handle types.QueueItemHandle) error
}

// PredicateFunc defines a function that returns true if a given queue item matches a certain condition.
// It is used by `SafeQueue` methods like `Cleanup()` and `Scan()`.
type PredicateFunc func(item types.QueueItemAccessor) bool

// SafeQueue defines the contract for a single, concurrent-safe queue implementation. A `SafeQueue` instance is created
// by the Flow Registry for each flow on each shard and is wrapped by a `ports.ManagedQueue` to integrate lifecycle
// management and statistics.
//
// Conformance:
//   - All methods defined in this interface (including those embedded from `QueueInspectionMethods` and the
//     write/mutating methods) MUST be goroutine-safe for concurrent access with respect to the queue's own internal
//     data structures.
//   - Methods that mutate the queue (`Add()`, `Remove()`) MUST return the queue's new length and total byte size after
//     the operation.
//   - If this queue was configured with an `ItemComparator` (because it reported `CapabilityPriorityConfigurable`), it
//     MUST use the comparator's `Func` for ordering.
type SafeQueue interface {
	QueueInspectionMethods

	// Add attempts to enqueue an item. On success, it must associate a new, unique `QueueItemHandle` with the item.
	//
	// Returns:
	//   - (newLen, newByteSize, nil) if the item is successfully emqueued.
	//   - (currentLen, currentByteSize, `ErrNilQueueItem`) if the item is nil.
	//   - (currentLen, currentByteSize, error) for any other non-recoverable errors.
	//
	// Conformance:
	//   - MUST call `item.SetHandle(createdHandle)` before returning.
	//   - MUST accurately update its internal state to reflect the new item for subsequent calls to `Len()` and
	// 		 `ByteSize()`.
	//   - MUST return the queue's new total length and byte size.
	Add(item types.QueueItemAccessor) (newLen, newByteSize uint64, err error)

	// Remove atomically finds and removes the item identified by the given handle.
	//
	// Returns:
	//   - (removedItem, newLen, newByteSize, nil) if the item is successfully removed.
	//   - (nil, currentLen, currentByteSize, error wrapping `ErrInvalidQueueItemHandle`) if the handle is invalid (nil,
	//     wrong type, already invalidated).
	//   - (nil, currentLen, currentByteSize, error wrapping `ErrQueueItemNotFound`) if the handle is valid but the item
	//     is not found.
	//   - (nil, currentLen, currentByteSize, error) for any other non-recoverable errors.
	//
	// Conformance:
	//   - MUST invalidate the provided handle by calling `handle.Invalidate()` upon successful removal.
	//   - MUST accurately update its internal state for `Len()` and `ByteSize()`.
	Remove(handle types.QueueItemHandle) (removedItem types.QueueItemAccessor, newLen, newByteSize uint64, err error)

	// Cleanup iterates through the queue and atomically removes all items for which the predicate returns true.
	// This method is designed to be the highly-performant, primary mechanism for frequent, partial eviction tasks like
	// TTL/cancellation expiry.
	//
	// Returns:
	//   - A slice of the items that were removed.
	//   - An error if an unrecoverable issue occurs during the operation.
	//
	// Conformance:
	//   - Implementations MUST perform the find-and-remove logic as an atomic operation to prevent race conditions.
	//   - The handle for each removed item MUST be invalidated.
	Cleanup(predicate PredicateFunc) (cleanedItems []types.QueueItemAccessor, err error)

	// Drain atomically removes all items from the queue and returns them in a slice.
	// This method provides an unambiguous and performant way to empty a queue, primarily for use cases like flow
	// migrations within the FlowRegistry.
	//
	// Conformance: The handles for all items MUST be invalidated.
	Drain() (drainedItems []types.QueueItemAccessor, err error)

	// Scan provides a read-only mechanism to find all items in the queue that match a given predicate.
	// This method is intended for advanced policies that need to perform complex inspections without mutation.
	//
	// Conformance:
	//   - This method MUST only be implemented by queues that also advertise the 'CapabilityScannable'.
	//   - If a queue does not support this capability, it MUST return `ErrOperationNotSupported`.
	Scan(predicate PredicateFunc) (foundItems []types.QueueItemAccessor, err error)
}
