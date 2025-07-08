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

package besthead

import (
	"fmt"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/interflow/dispatch"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
)

const BestHeadPolicyName = "BestHead"

func init() {
	dispatch.MustRegisterPolicy(dispatch.RegisteredPolicyName(BestHeadPolicyName),
		func() (framework.InterFlowDispatchPolicy, error) {
			return newBestHead(), nil
		})
}

type bestHead struct{}

func newBestHead() *bestHead {
	return &bestHead{}
}

// SelectQueue inspects the head item of each non-empty queue in the priority band.
// It uses the `framework.ItemComparator` associated with each flow to determine the "best" head item.
// It requires that comparators have compatible `framework.ScoreTypes` to make a meaningful decision.
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

		item, err := queue.PeekHead()
		if err != nil || item == nil {
			return true
		}

		if bestQueue == nil {
			bestQueue = queue
			bestItem = item
			return true
		}

		if queue.Comparator().ScoreType() != bestQueue.Comparator().ScoreType() {
			iterationErr = fmt.Errorf("%w: expected %q, got %q", framework.ErrIncompatiblePriorityType,
				bestQueue.Comparator().ScoreType(), queue.Comparator().ScoreType())
			return false
		}

		if bestQueue.Comparator().Func()(item, bestItem) {
			bestQueue = queue
			bestItem = item
		}
		return true
	})

	if iterationErr != nil {
		return nil, iterationErr
	}
	return bestQueue, nil
}
