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
	"errors"
)

// --- Standard Runtime Sentinel Errors ---

// ErrRejected is a generic error indicating a request was rejected by the FlowController *before* being formally
// enqueued into a SafeQueue.
// It is typically paired with a specific QueueOutcome (e.g., QueueOutcomeRejectedCapacity, QueueOutcomeRejectedOther).
// Errors returned by FlowController.EnqueueAndWait() that signify pre-queue rejection will wrap this error.
var ErrRejected = errors.New("request rejected pre-queue")

// ErrEvicted is a generic error indicating a request was removed from a FlowController-managed queue after being
// successfully enqueued, but for reasons other than successful dispatch (e.g., TTL expiry, preemption, context
// cancellation, shutdown).
// It is typically paired with a specific QueueOutcome. Errors returned by FlowController.EnqueueAndWait() that signify
// post-queue eviction will wrap this error.
var ErrEvicted = errors.New("request evicted from queue")

// PreEnqueueRejectionErrors are errors that can occur before a request is formally added to a SafeQueue.
// When returned by FlowController.EnqueueAndWait(), these specific errors will typically be wrapped by ErrRejected.
var (
	// ErrNilRequest indicates that a nil types.FlowControlRequest was provided.
	ErrNilRequest = errors.New("FlowControlRequest cannot be nil")

	// ErrFlowIDEmpty indicates that a flow ID was empty when one was required.
	ErrFlowIDEmpty = errors.New("flow ID cannot be empty")

	// ErrQueueAtCapacity indicates that a request could not be enqueued because queue capacity limits were met and
	// preemption (if applicable) failed to make space.
	ErrQueueAtCapacity = errors.New("queue at capacity and preemption failed to make space")

	// ErrFlowNotRegistered indicates that the flow ID provided in a request is not registered or has no active instance
	// in the FlowRegistry.
	// (This error is also used by FlowRegistry methods directly).
	ErrFlowNotRegistered = errors.New("flow not registered or no active instance")
)

// PostEnqueueEvictionErrors are errors that occur when a request, already in a SafeQueue, is removed for reasons other
// than dispatch.
// When returned by FlowController.EnqueueAndWait(), these specific errors will typically be wrapped by ErrEvicted.
var (
	// ErrTTLExpired indicates a request was evicted from a queue because its effective Time-To-Live expired.
	ErrTTLExpired = errors.New("request TTL expired")

	// ErrContextCancelled indicates a request was evicted from a queue because its associated context (from
	// FlowControlRequest.Context()) was cancelled.
	// This error will often wrap the specific context error (context.Canceled or context.DeadlineExceeded).
	ErrContextCancelled = errors.New("request context cancelled")

	// ErrPreempted indicates a request was evicted from a queue because it was chosen as a victim by a preemption policy
	// to make space for another request.
	ErrPreempted = errors.New("request preempted")
)

// FlowRegistryErrors relate to operations on the FlowRegistry, such as flow registration, updates, or lookups. These
// are typically returned directly by FlowRegistry methods. Some (like ErrFlowNotRegistered or ErrInvalidFlowPriority)
// might also be wrapped by ErrRejected if they cause FlowController.EnqueueAndWait() to fail.
var (
	// ErrInvalidFlowPriority indicates that a flow priority value provided during flow registration or lookup is not
	// recognized or supported by the FlowRegistry's configuration.
	ErrInvalidFlowPriority = errors.New("invalid or unconfigured priority level for flow")

	// ErrFlowInstanceNotFound indicates that a specific instance of a flow (e.g., a flow at a particular priority) was
	// not found by a FlowRegistry lookup.
	ErrFlowInstanceNotFound = errors.New("specific flow instance not found in registry")

	// ErrPriorityBandNotFound indicates that an operation targeted a priority band that is not configured in the
	// FlowRegistry.
	ErrPriorityBandNotFound = errors.New("priority band not configured")
)

// SafeQueueErrors relate to operations directly on a SafeQueue implementation.
// These are typically returned by SafeQueue methods and might be handled or wrapped by the ManagedQueue or
// FlowController.
var (
	// ErrQueueEmpty indicates an attempt to operate on an empty SafeQueue in a way that requires items (e.g.,
	// SafeQueue.PeekHead()).
	ErrQueueEmpty = errors.New("queue is empty")

	// ErrQueueItemNotFound indicates that a SafeQueue.Remove(handle) operation did not find an item matching the provided
	// valid QueueItemHandle.
	ErrQueueItemNotFound = errors.New("queue item not found for the given handle")

	// ErrNilQueueItem indicates that a nil types.QueueItemAccessor was passed to SafeQueue.Add().
	ErrNilQueueItem = errors.New("queue item cannot be nil")

	// ErrInvalidQueueItemHandle indicates that a QueueItemHandle provided to a SafeQueue operation
	// (like SafeQueue.Remove()) is not valid for that queue or operation.
	ErrInvalidQueueItemHandle = errors.New("invalid queue item handle")

	// ErrOperationNotSupported indicates that an operation (e.g., SafeQueue.PeekTail()) was called on a SafeQueue
	// implementation that does not support it.
	ErrOperationNotSupported = errors.New("operation not supported by this queue type")
)

// PolicyErrors relate to issues encountered by or with policy plugins.
// These are typically returned by policy methods and handled or wrapped by the FlowController.
var (
	// ErrIncompatiblePriorityType may be returned by policy implementations (e.g., InterFlowDispatchPolicy) if they
	// attempt to compare items from different queues whose configured ItemComparators have different or incompatible
	// ScoreTypes, and the policy cannot reconcile them.
	ErrIncompatiblePriorityType = errors.New("incompatible item comparator ScoreTypes for comparison by policy")

	// ErrPolicyQueueMismatch is primarily a setup or validation error if the FlowRegistry attempts to associate a policy
	// with a SafeQueue whose capabilities do not meet the policy's RequiredQueueCapabilities().
	ErrPolicyQueueMismatch = errors.New("policy requirements incompatible with configured queue capabilities")
)

// GeneralFlowControllerErrors are general runtime errors for the FlowController.
var (
	// ErrFlowControllerShutdown indicates that an operation could not complete or an item was evicted because the
	// FlowController is shutting down or has stopped.
	// When returned by FlowController.EnqueueAndWait(), this will be wrapped by ErrRejected (if rejection happens before
	// internal queuing) or ErrEvicted (if eviction happens after internal queuing).
	ErrFlowControllerShutdown = errors.New("FlowController is shutting down")
)
