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

import "strconv"

// QueueOutcome clarifies the high-level final outcome of a request's lifecycle within the FlowController. This enum is
// designed for concise reporting in return values from FlowController.EnqueueAndWait() and for use as a low-cardinality
// label in metrics. For fine-grained details on failures, the accompanying error should be inspected.
type QueueOutcome int

const (
	// QueueOutcomeDispatched indicates the request was successfully processed by the FlowController and unblocked for the
	// caller to proceed.
	// The associated error from EnqueueAndWait will be nil.
	QueueOutcomeDispatched QueueOutcome = iota

	// --- Pre-Enqueue Rejection Outcomes (request never entered a SafeQueue) ---
	// For these outcomes, the error from EnqueueAndWait will wrap ErrRejected.

	// QueueOutcomeRejectedCapacity indicates rejection because queue capacity limits were met and preemption (if
	// applicable) failed to make space.
	// The associated error will wrap types.ErrQueueAtCapacity (and types.ErrRejected).
	QueueOutcomeRejectedCapacity

	// QueueOutcomeRejectedOther indicates rejection for reasons other than capacity before the request was formally
	// enqueued.
	// Examples: invalid input (nil request, empty flowID), flow not registered, or FlowController shutdown before
	// internal queuing.
	// The specific underlying cause can be determined from the associated error (e.g., types.ErrNilRequest,
	// types.ErrFlowNotRegistered, types.ErrFlowControllerShutdown, all wrapped by types.ErrRejected).
	QueueOutcomeRejectedOther

	// --- Post-Enqueue Eviction Outcomes (request was in a SafeQueue but not dispatched) ---
	// For these outcomes, the error from EnqueueAndWait will wrap ErrEvicted.

	// QueueOutcomeEvictedTTL indicates eviction from a queue because the request's effective Time-To-Live expired.
	// The associated error will wrap types.ErrTTLExpired (and types.ErrEvicted).
	QueueOutcomeEvictedTTL

	// QueueOutcomeEvictedContextCancelled indicates eviction from a queue because the request's own context (from
	// FlowControlRequest.Context()) was cancelled.
	// The associated error will wrap types.ErrContextCancelled (which may further wrap context.Canceled or
	// context.DeadlineExceeded) and types.ErrEvicted.
	QueueOutcomeEvictedContextCancelled

	// QueueOutcomeEvictedPreempted indicates eviction from a queue to make space for another request due to a preemption
	// policy.
	// The associated error will wrap types.ErrPreempted (and types.ErrEvicted).
	QueueOutcomeEvictedPreempted

	// QueueOutcomeEvictedOther indicates eviction from a queue for reasons not covered by more specific eviction outcomes
	// (e.g., FlowController shutdown while the item was queued, or an unexpected internal error during dispatch).
	// The specific underlying cause can be determined from the associated error (e.g., types.ErrFlowControllerShutdown,
	// wrapped by types.ErrEvicted).
	QueueOutcomeEvictedOther
)

// String returns a human-readable string representation of the QueueOutcome.
// It includes the underlying integer value for unknown outcomes to aid debugging.
func (o QueueOutcome) String() string {
	switch o {
	case QueueOutcomeDispatched:
		return "Dispatched"
	case QueueOutcomeRejectedCapacity:
		return "RejectedCapacity" // Associated error wraps types.ErrQueueAtCapacity
	case QueueOutcomeRejectedOther:
		return "RejectedOther"
	case QueueOutcomeEvictedTTL:
		return "EvictedTTL"
	case QueueOutcomeEvictedContextCancelled:
		return "EvictedContextCancelled"
	case QueueOutcomeEvictedPreempted:
		return "EvictedPreempted"
	case QueueOutcomeEvictedOther:
		return "EvictedOther"
	default:
		// Return the integer value for unknown outcomes to aid in debugging.
		return "UnknownOutcome(" + strconv.Itoa(int(o)) + ")"
	}
}
