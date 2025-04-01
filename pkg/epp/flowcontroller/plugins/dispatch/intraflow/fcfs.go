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

package intraflowdispatch

import (
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

const FCFSDispatchPolicyName RegisteredIntraFlowDispatchPolicyName = "FCFS"

func init() {
	RegisterPolicy(FCFSDispatchPolicyName, func() (types.IntraFlowDispatchPolicy, error) {
		return NewFCFS(), nil
	})
}

// FCFS (First-Come, First-Served) implements the types.IntraFlowDispatchPolicy interface.
// It selects the item at the head of the queue, assuming the queue itself maintains FIFO order.
type FCFS struct{}

var _ types.IntraFlowDispatchPolicy = &FCFS{} // Compile-time validation

// NewFCFS creates a new FCFS IntraFlowDispatchPolicy.
func NewFCFS() *FCFS {
	return &FCFS{}
}

// SelectItem selects the next item from the queue.
// If the queue is the preferred "listqueue" type, it uses PeekHead().
// Otherwise, it iterates through all items to find the one with the oldest EnqueueTime.
func (p *FCFS) SelectItem(queue types.FlowQueueAccessor) types.QueueItemAccessor {
	if queue == nil {
		return nil
	}
	// For FCFS, we simply peek the head. The error is ignored here as per typical policy plugin behavior; if PeekHead
	// fails (e.g., empty queue), it returns nil, which is the correct signal for "no item selected".
	item, _ := queue.PeekHead()
	return item
}

// ItemComparator returns nil because FCFS relies on the queue's inherent FIFO ordering (e.g., a ListQueue) and does
// not require a custom comparator for a generic priority queue.
func (p *FCFS) Comparator() types.ItemComparator {
	return nil
}

// RequiredQueueCapabilities specifies that this policy needs a queue that supports basic FIFO operations (implicitly,
// PeekHead).
func (p *FCFS) RequiredQueueCapabilities() []types.QueueCapability {
	return []types.QueueCapability{types.CapabilityFIFO}
}

// Name returns the unique string identifier for this policy implementation.
func (p *FCFS) Name() string {
	return string(FCFSDispatchPolicyName)
}
