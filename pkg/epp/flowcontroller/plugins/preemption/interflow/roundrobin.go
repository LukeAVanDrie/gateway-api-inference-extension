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

package interflowpreemption

import (
	"sort"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

const RoundRobinPreemptionPolicyName RegisteredInterFlowPreemptionPolicyName = "RoundRobin"

func init() {
	RegisterPolicy(RoundRobinPreemptionPolicyName, func() (types.InterFlowPreemptionPolicy, error) {
		return NewRoundRobin(), nil
	})
}

// RoundRobin implements the types.InterFlowPreemptionPolicy interface.
// It selects a victim flow's queue from a lower-priority band in a round-robin fashion.
// It aims to distribute preemption attempts fairly across flows within the target band.
// This policy instance is assumed to be scoped to a single priority band.
type RoundRobin struct {
	mu sync.Mutex
	// lastSelectedFlowIndex stores the index of the last flow ID selected from the sorted list of flow IDs
	// within the band this policy instance is responsible for.
	lastSelectedFlowIndex int
}

// NewRoundRobin creates a new RoundRobin policy.
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{
		lastSelectedFlowIndex: -1, // Initialize to -1 to ensure the first selection starts at index 0
	}
}

// SelectVictimQueue selects the next non-empty flow's queue from the victimBand in a round-robin order.
// It returns (nil, nil) if no non-empty queue is found in the band.
func (p *RoundRobin) SelectVictimQueue(victimBand types.PriorityBandAccessor) (types.FlowQueueAccessor, error) {
	if victimBand == nil {
		return nil, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	flowIDs := victimBand.FlowIDs()
	if len(flowIDs) == 0 {
		return nil, nil
	}

	// Sort flowIDs to ensure consistent iteration order for round-robin.
	sort.Strings(flowIDs)

	// Iterate up to twice the number of flows to ensure we check every flow at least once starting from the one after
	// the last selected, and wrap around if needed.
	startIndex := p.lastSelectedFlowIndex
	for i := 0; i < len(flowIDs)*2; i++ {
		currentIndex := (startIndex + 1 + i) % len(flowIDs)
		currentFlowID := flowIDs[currentIndex]
		queue := victimBand.Queue(currentFlowID)

		if queue != nil && queue.Len() > 0 {
			p.lastSelectedFlowIndex = currentIndex
			return queue, nil // Found a non-empty queue
		}
	}

	// No non-empty queue found in this band.
	// p.lastSelectedFlowIndex remains unchanged, so if flows repopulate, it will continue from where it left off in the
	// cycle.
	return nil, nil
}

// Name returns the unique string identifier for this policy implementation.
func (p *RoundRobin) Name() string {
	return string(RoundRobinPreemptionPolicyName)
}

var _ types.InterFlowPreemptionPolicy = &RoundRobin{} // Compile-time validation
