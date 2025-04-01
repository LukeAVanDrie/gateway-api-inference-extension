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

import (
	"time"
)

// QueueInspectionMethods defines common read-only and content inspection methods shared by SafeQueue and
// FlowQueueAccessor.
// It is used as an embedded interface to ensure DRY (Don't Repeat Yourself).
type QueueInspectionMethods interface {
	// Len returns the current number of items in the queue.
	Len() int
	// ByteSize returns the current total byte size of all items in the queue (from QueueItemAccessor.ByteSize()).
	ByteSize() uint64
	// Name returns a string identifier for the concrete queue implementation type (e.g., "ListQueue", "MinMaxHeap",
	// "RedisSortedSet").
	// Useful for debugging and introspection.
	Name() string
	// Capabilities returns the set of functional capabilities this queue instance provides.
	Capabilities() []QueueCapability
	// PeekHead returns the QueueItemAccessor for the item at the "head" of the queue (according to the queue's ordering)
	// without removing it.
	// Conformance:
	//   - Returns (nil, ErrQueueEmpty) if the queue is empty.
	PeekHead() (QueueItemAccessor, error)
	// PeekTail returns the QueueItemAccessor for the item at the "tail" of the queue without removing it, if the queue
	// supports this capability.
	// Conformance:
	//   - Returns (nil, ErrQueueEmpty) if the queue is empty.
	//   - Returns (nil, ErrOperationNotSupported) if not supported by the implementation.
	PeekTail() (QueueItemAccessor, error)
}

// SafeQueue defines the contract for a core, self-contained queue implementation.
// Plugin implementers provide concrete types that satisfy this interface.
// These raw queues are then wrapped by a ManagedQueue by the FlowRegistry.
//
// Conformance:
//   - All methods defined in this interface (including those embedded from QueueInspectionMethods and the
//     write/mutating methods) MUST be goroutine-safe for concurrent access with respect to the queue's own internal
//     data structures.
//   - Methods that mutate the queue (Add, Remove) MUST return the queue's new length and total byte size after the
//     operation.
//   - If this queue was configured with an ItemComparator (because it reported CapabilityPriorityConfigurable), it
//     should use the comparator's Func for ordering.
type SafeQueue interface {
	QueueInspectionMethods // Embeds Len, ByteSize, Name, Capabilities, PeekHead, PeekTail.
	// Add attempts to enqueue an item.
	// Upon successful addition, it returns the queue's new length and total byte size.
	// Conformance:
	//   - Must call item.SetHandle(createdHandle) with a new, unique QueueItemHandle.
	//   - Must store the item and accurately update internal state for Len and ByteSize.
	//   - If item is nil, must return (currentLen, currentByteSize, ErrNilQueueItem) where currentLen and currentByteSize
	//     are the queue's state before attempting to add.
	Add(item QueueItemAccessor) (newLen uint64, newByteSize uint64, err error)
	// Remove removes and returns the item identified by the given handle.
	// Upon successful removal, it returns the removed item, and the queue's new length and total byte size.
	// Conformance:
	//   - If handle is invalid (nil, wrong type, already invalidated), must return (nil, currentLen, currentByteSize,
	//     ErrInvalidQueueItemHandle) (currentLen and currentByteSize are the queue's state before attempting removal).
	//   - If handle is valid but item not found, must return (nil, currentLen, currentByteSize, types.ErrQueueItemNotFound)
	//   - Must update internal state (Len and ByteSize) and invalidate the provided handle upon successful removal.
	Remove(handle QueueItemHandle) (removedItem QueueItemAccessor, newLen uint64, newByteSize uint64, err error)
	// CleanupExpired iterates items, using isItemExpired to check each one.
	// If an item is deemed expired, it's removed internally. Information about all removed items is returned. The
	// underlying queue's length and byte size are updated internally by this operation. The ManagedQueue wrapper will
	// subsequently call Len() and ByteSize() to get the final state for reconciliation.
	// Conformance:
	//   - For each item removed, its associated QueueItemHandle MUST be invalidated.
	//   - Must accurately update internal state (Len, ByteSize).
	CleanupExpired(currentTime time.Time, isItemExpired IsItemExpiredFunc) (removedItemsInfo []ExpiredItemInfo, err error)
}

// ManagedQueue is the interface returned by the FlowRegistry for a flow's active queue.
// It wraps an underlying SafeQueue, adding lifecycle validation against the FlowRegistry and integrating atomic
// statistics updates.
//
// Conformance:
//   - Implementations of this interface (provided by the FlowRegistry's wrapper) ensure that operations on the
//     underlying SafeQueue are only performed if the flow instance is still valid within the registry for mutating
//     operations (Add, Remove, CleanupExpired). If the instance is found to be invalid (e.g., cleaned up from the
//     registry), these mutating operations will return an error (typically wrapping types.ErrFlowInstanceNotFound).
//   - Read-only inspection methods (Len, ByteSize, Name, Capabilities, PeekHead, PeekTail) may return data from a
//     "zombie" queue instance if its corresponding flow instance has been removed from the registry. Similarly,
//     FlowSpec() and FlowQueueAccessor() will return values based on the state of the ManagedQueue when it was
//     valid, even if the underlying FlowRegistry instance has since been cleaned up; users should be aware that this
//     data might be stale.
//   - Write operations (Add, Remove, CleanupExpired) are effectively serialized by the wrapper and made atomic with
//     respect to FlowRegistry state and statistics.
//   - All methods (including those from embedded SafeQueue) are goroutine-safe.
//   - Mutating methods (Add, Remove) that return the queue's new length and byte size reflect the state *after* the
//     operation and internal statistics reconciliation.
type ManagedQueue interface {
	// Embeds all methods from SafeQueue
	// The wrapper's implementation of these embedded methods will:
	// 1. Validate flow instance with FlowRegistry (likely under registry RLock).
	// 2. Call the corresponding method on the underlying SafeQueue instance, receiving newLen and newByteSize for
	//    mutating operations.
	// 3. Atomically update its associated flowInstance's statistics and then the FlowRegistry's global/band statistics
	//    based on these values or calculated deltas.
	// 4. Return the results (including newLen, newByteSize) from the SafeQueue call.
	SafeQueue
	// FlowQueueAccessor returns a read-only, flow-aware accessor for this managed queue.
	// This is how policies inspect queue state.
	// Conformance:
	//   - Must return a non-nil FlowQueueAccessor.
	FlowQueueAccessor() FlowQueueAccessor
	// FlowSpec returns the specification of the flow this managed queue is associated with.
	// This is a convenience method for accessing the flow specification without needing to call
	// FlowQueueAccessor().FlowSpec().
	// Conformance:
	//   - Must return a non-nil FlowSpecification.
	FlowSpec() FlowSpecification
}

// FlowQueueAccessor provides a read-only, flow-aware view of a queue's state and its items.
// It is intended for use by policy plugins to inspect queues.
// Instances are vended by a ManagedQueue.
//
// Conformance:
//   - All methods defined in this interface (including those embedded from QueueInspectionMethods) MUST be
//     goroutine-safe for concurrent access.
type FlowQueueAccessor interface {
	QueueInspectionMethods // Embeds Len, ByteSize, Name, Capabilities, PeekHead, PeekTail.
	// FlowSpec returns the specification of the flow this queue accessor is associated with, providing essential context
	// (like FlowID) to policies.
	// Conformance:
	//   - Must return a non-nil FlowSpecification.
	FlowSpec() FlowSpecification
	// Comparator returns the ItemComparator that defines the dispatch ordering for items within this queue, sourced from
	// from the IntraFlowDispatchPolicy configured for this flow.
	// Conformance:
	//   - Must return a non-nil ItemComparator.
	Comparator() ItemComparator
}

// QueueCapability defines a functional capability that a SafeQueue implementation can provide.
// These capabilities can be broadly categorized into:
//  1. Structural Capabilities: Describe the methods or interface contract a queue exposes (e.g., CapabilityDoubleEnded
//     implies a working PeekTail method). Inter-flow policies primarily rely on these to ensure they can perform
//     necessary inspection operations.
//  2. Behavioral Capabilities: Describe the internal operational logic or ordering principle of a queue (e.g.,
//     CapabilityFIFO, CapabilityPriorityConfigurable). These are primarily the concern of IntraFlowDispatchPolicy,
//     which selects a desired behavior and requires the corresponding capability. The ItemComparator provided by an
//     IntraFlowDispatchPolicy then serves as the standardized way for *any* policy (intra or inter-flow) to understand
//     a queue's dispatch order, abstracting the underlying behavioral implementation.
//
// Policies use these capabilities to declare their operational requirements, allowing the FlowRegistry to ensure that a
// compatible SafeQueue is used with a given policy.
type QueueCapability string

const (
	// CapabilityFIFO indicates that the queue operates in a First-In, First-Out manner. PeekHead() will return the oldest
	// item. To remove it, its handle would be obtained and used with Remove(handle).
	// Policies requiring strict FIFO ordering would specify this capability.
	// This is primarily a *behavioral* capability.
	CapabilityFIFO QueueCapability = "FIFO"

	// CapabilityPriorityConfigurable indicates that the queue can be configured with an ItemComparator (typically
	// provided by an IntraFlowDispatchPolicy). Its PeekHead() will return the highest priority item according to this
	// comparator. To remove it, its handle would be obtained and used with Remove(handle). This capability is essential
	// for implementing priority-based dispatch within a flow.
	// This is primarily a *behavioral* capability.
	CapabilityPriorityConfigurable QueueCapability = "PriorityConfigurable"

	// CapabilityDoubleEnded indicates that the queue supports operations at both ends, specifically PeekTail().
	// This is useful for policies that might need to inspect or target items at the "end" or "lowest priority" part of
	// the queue, such as certain preemption strategies.
	// This is a *structural* capability.
	CapabilityDoubleEnded QueueCapability = "DoubleEnded"

	// CapabilityDynamicPriority indicates that the queue can efficiently handle items whose dispatch priorities (as
	// determined by the configured ItemComparator) may change while they are in the queue. Such queues (e.g., a priority
	// heap) should be able to re-order or re-evaluate item positions as needed, or allow for efficient re-prioritization
	// if signaled. This is crucial for policies that implement dynamic scoring based on evolving system state.
	// This is primarily a *behavioral* capability, often coupled with CapabilityPriorityConfigurable.
	CapabilityDynamicPriority QueueCapability = "DynamicPriority"
)

// ExpiredItemInfo holds structured information about a single QueueItemAccessor that was removed from a SafeQueue
// during its CleanupExpired process because it was deemed expired by the IsItemExpiredFunc callback.
type ExpiredItemInfo struct {
	// Item is the QueueItemAccessor for the item that was removed.
	Item QueueItemAccessor
	// Outcome indicates the specific reason (as a QueueOutcome) why the item was considered expired and subsequently
	// removed by the queue during cleanup.
	// Examples: QueueOutcomeEvictedTTL, QueueOutcomeEvictedContextCancelled.
	Outcome QueueOutcome
	// Error is the specific error associated with the expiry condition that led to the item's removal (e.g.,
	// types.ErrTTLExpired, context.Canceled).
	Error error
}

// IsItemExpiredFunc is a function type defining the callback signature used by SafeQueue.CleanupExpired(). The
// FlowController implements this function, encapsulating its logic for determining if a queued item should be
// considered expired (e.g., due to TTL violation or context cancellation).
//
// The SafeQueue implementation calls this function for items during its CleanupExpired routine.
//
// Parameters:
//   - item: The QueueItemAccessor of the item being checked.
//   - currentTime: The current time, provided by the caller of CleanupExpired (typically the FlowController's expiry
//     cleanup loop) to ensure consistency across multiple checks in a single cleanup cycle.
//
// Returns:
//   - isExpired (bool): True if the item is considered expired and should be removed from the queue.
//   - outcomeForExpiry (QueueOutcome): The QueueOutcome to be associated with this specific expiry reason if isExpired
//     is true.
//   - errForExpiry (error): The specific error associated with this expiry reason if isExpired is true (e.g.,
//     types.ErrTTLExpired).
type IsItemExpiredFunc func(item QueueItemAccessor, currentTime time.Time) (isExpired bool, outcomeForExpiry QueueOutcome, errForExpiry error)
