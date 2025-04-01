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

package flowcontroller

import (
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

// flowItem is the internal representation of a request managed by the FlowController.
//
// It wraps the original FlowControlRequest and adds metadata for queuing, lifecycle management, and policy
// interaction.
//
// flowItem implements the types.QueueItemAccessor interface.
type flowItem struct {
	// originalRequest is the underlying request submitted to the FlowController.
	originalRequest types.FlowControlRequest
	// flowID is the unique identifier of the flow this item belongs to.
	// This is cached from originalRequest.FlowID().
	flowID string
	// enqueueTime is the timestamp when the item was logically accepted by the FlowController for processing (i.e., when
	// EnqueueAndWait was called).
	enqueueTime time.Time
	// effectiveTTL is the actual Time-To-Live assigned to this item by the FlowController, considering the request's
	// preference and controller defaults.
	effectiveTTL time.Duration
	// queueHandle is the opaque handle returned by the FlowQueue when this item is successfully added to a queue.
	// It's used by the FlowController to instruct the FlowQueue to remove this specific item.
	// This is nil until the item is successfully enqueued into a FlowQueue.
	queueHandle types.QueueItemHandle
	// done is closed exactly once when the item is finalized (dispatched or evicted/rejected). Callers of
	// EnqueueAndWait() block on this channel.
	done chan struct{}
	// err stores the final error state if the item was not successfully dispatched.
	// Set atomically via finalize().
	err atomic.Value // Stores error
	// outcome stores the final QueueOutcome of the item's lifecycle.
	// Set atomically via finalize().
	outcome atomic.Value // Stores types.QueueOutcome
	// finalizedOnce ensures the finalize() logic runs only once.
	finalizedOnce sync.Once
}

// newFlowItem creates a new flowItem.
// The flowSpec and effectiveTTL are determined and provided by the FlowController.
func newFlowItem(req types.FlowControlRequest, effectiveTTL time.Duration, enqueueTime time.Time) *flowItem {
	fi := &flowItem{
		originalRequest: req,
		flowID:          req.FlowID(),
		enqueueTime:     enqueueTime,
		effectiveTTL:    effectiveTTL,
		done:            make(chan struct{}),
	}
	// Initialize outcome to a sensible default before any processing.
	// If rejected pre-queue, this might be updated by the EnqueueAndWait logic.
	fi.outcome.Store(types.QueueOutcomeRejectedOther) // A pessimistic default until explicitly set otherwise
	return fi
}

// --- Implementation of types.QueueItemAccessor ---

var _ types.QueueItemAccessor = &flowItem{} // Compile-time validation

func (fi *flowItem) EnqueueTime() time.Time {
	return fi.enqueueTime
}

func (fi *flowItem) ByteSize() uint64 {
	return fi.originalRequest.ByteSize()
}

func (fi *flowItem) FlowID() string {
	return fi.flowID
}

func (fi *flowItem) EffectiveTTL() time.Duration {
	return fi.effectiveTTL
}

func (fi *flowItem) RequestID() string {
	return fi.originalRequest.ID()
}

func (fi *flowItem) OriginalRequest() types.FlowControlRequest {
	return fi.originalRequest
}

func (fi *flowItem) Handle() types.QueueItemHandle {
	return fi.queueHandle
}

func (fi *flowItem) SetHandle(handle types.QueueItemHandle) {
	fi.queueHandle = handle
}

// --- Lifecycle Management Methods (called by FlowController) ---

// finalize sets the item's terminal state (outcome, error) and closes its 'done' channel idempotently using sync.Once.
// This is the single point where an item's lifecycle within the FlowController concludes.
// The FlowController is responsible for determining the correct outcome and error.
func (fi *flowItem) finalize(outcome types.QueueOutcome, err error) {
	fi.finalizedOnce.Do(func() {
		if err != nil {
			fi.err.Store(err)
		}
		fi.outcome.Store(outcome)
		close(fi.done)
	})
}

// getFinalState extracts the final outcome and error stored atomically.
// Should be called after item.done is closed or known to be closed.
func (fi *flowItem) getFinalState() (types.QueueOutcome, error) {
	outcomeVal := fi.outcome.Load()
	errVal := fi.err.Load()

	var finalOutcome types.QueueOutcome
	if oc, ok := outcomeVal.(types.QueueOutcome); ok {
		finalOutcome = oc
	} else {
		// This case should ideally not happen if finalize is always called correctly.
		// Default to an error state if outcome is not what's expected.
		finalOutcome = types.QueueOutcomeRejectedOther // Or some other error default
	}

	var finalErr error
	if e, ok := errVal.(error); ok {
		finalErr = e
	}
	return finalOutcome, finalErr
}

// isFinalized checks if the item has been finalized without blocking.
func (fi *flowItem) isFinalized() bool {
	select {
	case <-fi.done:
		return true
	default:
		return false
	}
}
