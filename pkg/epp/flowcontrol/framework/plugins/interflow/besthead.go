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
	"fmt"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

const BestHeadType = "best-head"

var _ framework.InterFlowDispatchPolicy = &bestHead{}

func init() {
	plugins.RegisterWithMetadata(BestHeadType, plugins.PluginRegistration{
		Factory:   BestHeadFactory,
		Lifecycle: plugins.LifecycleTransient,
	})
}

// BestHeadFactory defines the factory function for BestHead.
func BestHeadFactory(name string, _ json.RawMessage, _ plugins.Handle) (plugins.Plugin, error) {
	return newBestHead().withName(name), nil
}

// bestHead is an InterFlowDispatchPolicy that implements a greedy, non-fair strategy.
// It effectively "disables" inter-flow fairness by always selecting the queue that contains the single "best" item from
// across all queues in the priority band.
//
// This policy is useful for maximizing utilization when fairness is not a concern.
type bestHead struct {
	typedName plugins.TypedName
}

func newBestHead() *bestHead {
	return &bestHead{
		typedName: plugins.TypedName{Type: BestHeadType, Name: BestHeadType},
	}
}

func (p *bestHead) withName(name string) *bestHead {
	p.typedName.Name = name
	return p
}

// TypedName returns the type and name of the plugin instance.
func (p *bestHead) TypedName() plugins.TypedName {
	return p.typedName
}

// SelectQueue iterates through all non-empty queues in the band, peeks at their head items, and uses each queue's
// ItemComparator to find the single highest-priority item overall.
//
// It requires that all queues being compared have a compatible ScoreType to ensure the comparison is meaningful.
// If an incompatible comparator is found, the selection fails with an error.
func (p *bestHead) SelectQueue(band framework.PriorityBandAccessor) (framework.FlowQueueAccessor, error) {
	if band == nil {
		return nil, nil
	}

	var bestQueue framework.FlowQueueAccessor
	var bestItem types.QueueItemAccessor
	var iterationErr error
	band.IterateQueues(func(queue framework.FlowQueueAccessor) (keepIterating bool) {
		if queue == nil || queue.Len() == 0 {
			return true
		}
<<<<<<< HEAD

		item := queue.PeekHead()
		if item == nil {
=======
		item, err := queue.PeekHead()
		if err != nil || item == nil {
>>>>>>> c7f7795 (feat: Adapt InterFlowDispatchPolicy to be a plugin)
			return true
		}

		if bestQueue == nil {
			bestQueue = queue
			bestItem = item
			return true
		}

		comp := queue.Comparator()
		if comp.ScoreType() != bestQueue.Comparator().ScoreType() {
			iterationErr = fmt.Errorf("%w: cannot compare queues with different score types, expected %q, got %q",
				framework.ErrIncompatiblePriorityType,
				bestQueue.Comparator().ScoreType(),
				comp.ScoreType())
			return false
		}

		if comp.Func()(item, bestItem) {
			bestItem = item
			bestQueue = queue
		}
		return true
	})

	if iterationErr != nil {
		return nil, iterationErr
	}
	return bestQueue, nil
}
