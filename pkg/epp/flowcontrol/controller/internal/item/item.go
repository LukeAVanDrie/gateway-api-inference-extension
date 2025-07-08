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

package item

import (
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
)

// FlowItem is the internal representation of a request managed by the FlowController.
//
// It wraps the original FlowControlRequest and adds metadata for queuing, lifecycle management, and policy interaction.
//
// FlowItem implements the types.QueueItemAccessor interface.
type FlowItem struct {
	enqueueTime     time.Time
	effectiveTTL    time.Duration
	originalRequest types.FlowControlRequest
	handle          types.QueueItemHandle

	// Done is closed exactly once when the item is finalized (dispatched or evicted/rejected).
	Done chan struct{}
	// err stores the final error state if the item was not successfully dispatched.
	// Set atomically via `finalize()`.
	err atomic.Value // Stores error
	// outcome stores the final QueueOutcome of the item's lifecycle.
	// Set atomically via `finalize()`.
	outcome atomic.Value // Stores types.QueueOutcome
	// onceFinalize ensures the `finalize()` logic is idempotent.
	onceFinalize sync.Once
}

var _ types.QueueItemAccessor = &FlowItem{} // Compile-time validation

func NewFlowItem(req types.FlowControlRequest, effectiveTTL time.Duration, enqueueTime time.Time) *FlowItem {
	fi := &FlowItem{
		enqueueTime:     enqueueTime,
		effectiveTTL:    effectiveTTL,
		originalRequest: req,
		Done:            make(chan struct{}),
	}
	fi.outcome.Store(types.QueueOutcomeNotYetFinalized)
	return fi
}

func (fi *FlowItem) EnqueueTime() time.Time                    { return fi.enqueueTime }
func (fi *FlowItem) EffectiveTTL() time.Duration               { return fi.effectiveTTL }
func (fi *FlowItem) OriginalRequest() types.FlowControlRequest { return fi.originalRequest }
func (fi *FlowItem) Handle() types.QueueItemHandle             { return fi.handle }

// TODO: should we enforce idempotency?
func (fi *FlowItem) SetHandle(handle types.QueueItemHandle) { fi.handle = handle }

// finalize sets the item's terminal state (outcome, error) and closes its done channel idempotently using sync.Once.
// This is the single point where an item's lifecycle within the FlowController concludes.
func (fi *FlowItem) Finalize(outcome types.QueueOutcome, err error) {
	fi.onceFinalize.Do(func() {
		if err != nil {
			fi.err.Store(err)
		}
		fi.outcome.Store(outcome)
		close(fi.Done)
	})
}

// FinalState extracts the final outcome and error stored atomically.
// Should be called after item.done is closed or known to be closed.
func (fi *FlowItem) FinalState() (types.QueueOutcome, error) {
	outcomeVal := fi.outcome.Load()
	errVal := fi.err.Load()

	var finalOutcome types.QueueOutcome
	if oc, ok := outcomeVal.(types.QueueOutcome); ok {
		finalOutcome = oc
	} else { // This case should ideally not happen if finalize is always called correctly.
		finalOutcome = types.QueueOutcomeNotYetFinalized
	}

	var finalErr error
	if e, ok := errVal.(error); ok {
		finalErr = e
	}
	return finalOutcome, finalErr
}

// IsFinalized checks if the item has been finalized without blocking.
func (fi *FlowItem) IsFinalized() bool {
	select {
	case <-fi.Done:
		return true
	default:
		return false
	}
}
