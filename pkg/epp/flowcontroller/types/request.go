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
	"context"
	"time"
)

// FlowControlRequest defines the essential data the FlowController needs from an incoming request when it is first
// submitted for processing via FlowController.EnqueueAndWait().
// The FlowController will wrap instances of this interface with its own internal structure (e.g.,
// flowcontroller.flowItem, which implements QueueItemAccessor) to manage the request's lifecycle within the flow
// control system.
type FlowControlRequest interface {
	// Context returns the request's context. The FlowController uses this for monitoring cancellation (e.g., if the
	// client disconnects or a request-scoped timeout occurs), which can lead to the request being evicted from a queue.
	Context() context.Context
	// FlowID returns the unique identifier for the flow this request belongs to (e.g., model name, tenant ID). The
	// FlowController uses this ID, in conjunction with the flow's registered priority, to look up the active ManagedQueue
	// from the FlowRegistry.
	FlowID() string
	// ByteSize returns the request's size in bytes (e.g., prompt size). This is used by the FlowController and queue
	// implementations for managing byte-based capacity limits and for statistics.
	ByteSize() uint64
	// InitialEffectiveTTL returns the suggested initial Time-To-Live for this request within the FlowController's queues.
	// The FlowController may use this value as a hint or override it based on its own configuration or per-flow policies.
	// A value of 0 typically indicates no specific TTL preference from the request's perspective, in which case a
	// FlowController-defined default TTL may apply.
	InitialEffectiveTTL() time.Duration
	// ID returns an optional, user-facing unique identifier for this specific request.
	// This ID is primarily intended for logging, tracing, and observability across systems. It is distinct from the
	// internal QueueItemHandle used by SafeQueue implementations.
	ID() string
}

// QueueItemAccessor provides a view of a request item as it is managed within the FlowController's queues. It is the
// primary interface through which SafeQueue implementations and policy plugins interact with the request data and its
// associated flow control metadata.
//
// The FlowController internally creates an object that implements this interface (e.g., flowcontroller.flowItem) by
// wrapping an incoming FlowControlRequest.
type QueueItemAccessor interface {
	// EnqueueTime is the timestamp when the item was logically accepted by the FlowController for queuing (i.e., when
	// FlowController.EnqueueAndWait was called and the item was passed to the internal enqueue channel).
	EnqueueTime() time.Time
	// ByteSize returns the byte size of the original request, cached from FlowControlRequest.ByteSize(). Used for
	// capacity management and statistics.
	ByteSize() uint64
	// FlowID returns the unique identifier of the flow this item belongs to, cached from FlowControlRequest.FlowID().
	FlowID() string
	// EffectiveTTL is the actual Time-To-Live assigned to this item by the FlowController, taking into account the
	// request's preference (FlowControlRequest.InitialEffectiveTTL()) and any FlowController or per-flow
	// defaults/policies.
	EffectiveTTL() time.Duration
	// RequestID is the user-facing ID from the original request (FlowControlRequest.ID()), primarily for logging and
	// tracing.
	RequestID() string
	// OriginalRequest returns the underlying FlowControlRequest that this accessor provides a view of. This allows
	// policies or components that are aware of more specific FlowControlRequest implementations to perform type
	// assertions and access richer, application-specific data if necessary. In short, this is useful escape hatch for
	// rich request metadata passthrough.
	OriginalRequest() FlowControlRequest
	// Handle returns the QueueItemHandle associated with this item once it has been  successfully added to a SafeQueue.
	// Returns nil if no handle has been set yet (e.g., before the item is successfully processed by SafeQueue.Add()).
	Handle() QueueItemHandle
	// SetHandle associates a QueueItemHandle with this item.
	// Conformance:
	//   - This method MUST be called by a SafeQueue implementation within its Add method, immediately after a new
	//     QueueItemHandle is created for the item being added.
	//     This ensures that the QueueItemAccessor (which is typically stored in the queue and passed to policies) always
	//     has a reference to its queue-specific handle.
	//   - This method is not intended for general use outside of SafeQueue implementations.
	SetHandle(handle QueueItemHandle)
}

// QueueItemHandle represents an opaque, queue-specific handle to an item that has been successfully added to a
// SafeQueue.
// It allows the FlowController or other authorized components to refer to a specific item for operations like targeted
// removal (SafeQueue.Remove()). The handle also provides a mechanism for the SafeQueue to invalidate it after the item
// is removed or the handle is otherwise deemed stale.
type QueueItemHandle interface {
	// Handle returns the underlying, queue-specific raw handle (e.g., *list.Element). This is primarily for internal use
	// by the SafeQueue implementation that created it.
	// External users should treat this as opaque.
	Handle() any
	// Invalidate marks this handle instance as no longer valid for future operations on the SafeQueue from which it
	// originated.
	// This method is typically called by the SafeQueue implementation itself after the item associated with this handle
	// has been successfully removed, or if the queue otherwise determines the handle is stale (e.g., during
	// CleanupExpired for items it internally removes).
	// Conformance:
	//   - Must be idempotent; subsequent calls after the first should have no effect.
	Invalidate()
	// IsInvalidated returns true if this handle instance has been marked as invalid (e.g., by a call to Invalidate()).
	// If true, this handle should not be used for further operations on the SafeQueue. An attempt to use an invalidated
	// handle with SafeQueue.Remove() MUST result in ErrInvalidQueueItemHandle.
	IsInvalidated() bool
}
