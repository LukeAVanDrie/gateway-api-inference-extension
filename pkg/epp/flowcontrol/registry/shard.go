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

package registry

import (
	"fmt"
	"sync"
	"sync/atomic"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/ports"
)

// priorityBandState holds the state for a priority band within a shard.
type priorityBandState struct {
	mu sync.RWMutex // Protects managedQueues map within this band

	id       string
	name     string
	priority uint

	// Resolved policies for this band
	interFlowDispatchPolicy framework.InterFlowDispatchPolicy
	defaultIntraFlowDispatchPolicy framework.IntraFlowDispatchPolicy

	byteSize atomic.Uint64
	len      atomic.Uint64
}

// registryShard implements the `ports.RegistryShard` interface.
type registryShard struct {
	mu sync.RWMutex

	id                  string
	isActive            bool                              // For MVP, always true
	registry            *flowRegistry                     // Reference back to the main registry
	priorityBands       map[uint]*priorityBandState       // priority -> priorityBandState
	activeManagedQueues map[string]uint                   // flowID -> priority
	managedQueues       map[string]map[uint]*managedQueue // flowID -> priority -> managedQueue

	byteSize atomic.Uint64
	len      atomic.Uint64
}

var _ ports.RegistryShard = &registryShard{}

func (s *registryShard) ID() string { return s.id }

func (s *registryShard) IsActive() bool {
	return s.isActive // Always true for MVP's single shard
}

func (s *registryShard) ActiveManagedQueue(flowID string) (ports.ManagedQueue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	mQ, ok := s.activeManagedQueues[flowID]
	if !ok {
		return nil, fmt.Errorf("%w: flow %s not registered on shard %s", ports.ErrFlowNotRegistered, flowID, s.id)
	}
	return mQ, nil
}

func (s *registryShard) ManagedQueue(flowID string, priority uint) (ports.ManagedQueue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	priorityMap, registered := s.managedQueues[flowID]
	if !registered {
		return nil, fmt.Errorf("%w: flow %s not registered on shard %s", ports.ErrFlowNotRegistered, flowID, s.id)
	}

	mQ, ok := priorityMap[priority]
	if !ok {
		return nil, fmt.Errorf("%w: flow %s at priority %d not found on shard %s",
			ports.ErrFlowInstanceNotFound, flowID, priority, s.id)
	}
	return mQ, nil
}

func (s *registryShard) IntraFlowDispatchPolicy(flowID string, priority uint) (framework.IntraFlowDispatchPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	priorityMap, registered := s.managedQueues[flowID]
	if !registered {
		return nil, fmt.Errorf("%w: flow %s not registered on shard %s", ports.ErrFlowNotRegistered, flowID, s.id)
	}

	mQ, ok := priorityMap[priority]
	if !ok {
		return nil, fmt.Errorf("%w: flow %s at priority %d not found on shard %s",
			ports.ErrFlowInstanceNotFound, flowID, priority, s.id)
	}
	return mQ.dispatchPolicy, nil
}

func (s *registryShard) InterFlowDispatchPolicy(priority uint) (framework.InterFlowDispatchPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bandState, ok := s.priorityBands[priority]
	if !ok {
		return nil, fmt.Errorf("%w: priority %d not found on shard %s", ports.ErrPriorityBandNotFound, priority, s.id)
	}
	return bandState.interFlowDispatchPolicy, nil
}

func (s *registryShard) Stats() ports.ShardStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pbs := make(map[uint]ports.PriorityBandStats)
	for p, bandState := range s.priorityBands {
		bandState.mu.RLock()
		pbs[p] = ports.PriorityBandStats{
			Priority:      p,
			PriorityName:  bandState.name,
			CapacityBytes: bandState.config.CapacityBytes, // This is wrong
			ByteSize:      bandState.byteSize.Load(),
			Len:           bandState.len.Load(),
		}
		bandState.mu.RUnlock()
	}

	return ports.ShardStats{
		TotalCapacityBytes:   0, // TODO
		TotalByteSize:        s.byteSize.Load(),
		TotalLen:             s.len.Load(),
		PerPriorityBandStats: pbs,
	}
}
