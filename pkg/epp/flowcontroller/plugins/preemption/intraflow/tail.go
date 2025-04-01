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

package intraflowpreemption

import (
	"errors"
	"fmt"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

const TailPreemptionPolicyName RegisteredIntraFlowPreemptionPolicyName = "Tail"

func init() {
	RegisterPolicy(TailPreemptionPolicyName, func() (types.IntraFlowPreemptionPolicy, error) {
		return NewTail(), nil
	})
}

// Tail implements the types.IntraFlowPreemptionPolicy interface.
// It selects the item at the tail of the queue as the victim for preemption.
// For a typical FIFO queue (like ListQueue where items are added to the back/tail), this means the newest item in the
// queue would be selected. For a priority queue, this would be the lowest priority request.
type Tail struct{}

// NewTail creates a new Tail IntraFlowPreemptionPolicy.
func NewTail() *Tail {
	return &Tail{}
}

// SelectVictim returns the item at the tail of the queue.
// It relies on the queue's PeekTail() method.
func (p *Tail) SelectVictim(queue types.FlowQueueAccessor) (types.QueueItemAccessor, error) {
	if queue == nil {
		return nil, nil // No error for nil queue, just no victim
	}

	victim, err := queue.PeekTail()
	if err != nil {
		if errors.Is(err, types.ErrOperationNotSupported) {
			// If the queue doesn't support PeekTail, this policy cannot function.
			return nil, fmt.Errorf("%w: Tail policy requires PeekTail capability: %w", types.ErrPolicyQueueMismatch, err)
		}
		return nil, nil // For other errors like ErrQueueEmpty, still return no victim, no unrecoverable policy error.
	}
	return victim, nil
}

// RequiredQueueCapabilities specifies that this policy needs a queue that supports peeking at its tail end.
func (p *Tail) RequiredQueueCapabilities() []types.QueueCapability {
	return []types.QueueCapability{types.CapabilityDoubleEnded}
}

// Name returns the unique string identifier for this policy implementation.
func (p *Tail) Name() string {
	return string(TailPreemptionPolicyName)
}

var _ types.IntraFlowPreemptionPolicy = &Tail{} // Compile-time validation
