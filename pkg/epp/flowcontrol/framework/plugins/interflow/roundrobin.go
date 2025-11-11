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

package interflow

import (
	"encoding/json"
	"slices"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

const RoundRobinType = "round-robin"

var _ framework.InterFlowDispatchPolicy = &roundRobin{}

func init() {
	plugins.RegisterWithMetadata(RoundRobinType, plugins.PluginRegistration{
		Factory:   RoundRobinFactory,
		Lifecycle: plugins.LifecycleTransient,
	})
}

// RoundRobinFactory defines the factory function for RoundRobin.
func RoundRobinFactory(name string, _ json.RawMessage, _ plugins.Handle) (plugins.Plugin, error) {
	return newRoundRobin().withName(name), nil
}

// roundRobin is an InterFlowDispatchPolicy that implements a simple, round-robin strategy.
type roundRobin struct {
	typedName plugins.TypedName
	iterator  *iterator
}

func newRoundRobin() *roundRobin {
	return &roundRobin{
		iterator:  newIterator(),
		typedName: plugins.TypedName{Type: RoundRobinType, Name: RoundRobinType},
	}
}

func (p *roundRobin) withName(name string) *roundRobin {
	p.typedName.Name = name
	return p
}

// TypedName returns the type and name of the plugin instance.
func (p *roundRobin) TypedName() plugins.TypedName {
	return p.typedName
}

// SelectQueue selects the next flow in a round-robin fashion.
func (p *roundRobin) SelectQueue(band framework.PriorityBandAccessor) (framework.FlowQueueAccessor, error) {
	if band == nil {
		return nil, nil
	}
	selectedQueue := p.iterator.selectNextQueue(band)
	return selectedQueue, nil
}

// iterator implements a thread-safe round-robin selection logic.
// It maintains the ID of the last selected flow to ensure the selection order is correct even when the set of available
// flows changes dynamically.
type iterator struct {
	mu           sync.Mutex
	lastSelected *types.FlowKey
}

func newIterator() *iterator {
	return &iterator{}
}

// selectNextQueue iterates through the flows in a round-robin fashion, starting from the flow after the one selected in
// the previous call.
// It sorts the flow IDs to ensure a deterministic ordering.
// If no non-empty queue is found after a full cycle, it returns nil.
func (r *iterator) selectNextQueue(band framework.PriorityBandAccessor) framework.FlowQueueAccessor {
	r.mu.Lock()
	defer r.mu.Unlock()

	keys := band.FlowKeys()
	if len(keys) == 0 {
		r.lastSelected = nil // Reset state if no flows are present.
		return nil
	}
	slices.SortFunc(keys, func(a, b types.FlowKey) int { return a.Compare(b) }) // Sort for deterministic ordering.

	startIndex := 0
	if r.lastSelected != nil {
		// Find the index of the last selected flow.
		// If it's not found (e.g., the flow was removed), we start from the beginning.
		if idx := slices.Index(keys, *r.lastSelected); idx != -1 {
			startIndex = (idx + 1) % len(keys)
		}
	}

	numFlows := len(keys)
	for i := range numFlows {
		currentIdx := (startIndex + i) % numFlows
		currentKey := keys[currentIdx]
		queue := band.Queue(currentKey.ID)
		if queue != nil && queue.Len() > 0 {
			r.lastSelected = &currentKey
			return queue
		}
	}

	// No non-empty queue was found.
	r.lastSelected = nil
	return nil
}
