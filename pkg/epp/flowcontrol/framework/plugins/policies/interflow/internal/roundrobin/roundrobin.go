// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package roundrobin

import (
	"slices"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
)

// Iterator implements a thread-safe round-robin selection logic for flow queues.
// It maintains an internal index to cycle through available flow queues.
type Iterator struct {
	mu            sync.Mutex
	lastFlowIndex int
}

// NewIterator creates a new round-robin Iterator.
func NewIterator() *Iterator {
	return &Iterator{
		lastFlowIndex: -1, // Start before the first flow
	}
}

// SelectNextQueue iterates through the flow queues in a round-robin fashion.
// It starts from the flow after the one selected in the previous call.
// If no non-empty queue is found, it returns nil.
func (r *Iterator) SelectNextQueue(band framework.PriorityBandAccessor) framework.FlowQueueAccessor {
	r.mu.Lock()
	defer r.mu.Unlock()

	flowIDs := band.FlowIDs()
	if len(flowIDs) == 0 {
		return nil
	}

	slices.Sort(flowIDs)
	numFlows := len(flowIDs)
	for range numFlows {
		r.lastFlowIndex = (r.lastFlowIndex + 1) % numFlows
		currentFlowID := flowIDs[r.lastFlowIndex]
		queue := band.Queue(currentFlowID)
		if queue != nil && queue.Len() > 0 {
			return queue
		}
	}

	return nil
}
