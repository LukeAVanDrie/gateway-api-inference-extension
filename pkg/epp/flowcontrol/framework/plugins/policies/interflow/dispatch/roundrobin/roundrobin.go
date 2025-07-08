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
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/interflow/dispatch"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/interflow/internal/roundrobin"
)

const RoundRobinPolicyName = "RoundRobin"

func init() {
	dispatch.MustRegisterPolicy(dispatch.RegisteredPolicyName(RoundRobinPolicyName),
		func() (framework.InterFlowDispatchPolicy, error) {
			return newRoundRobin(), nil
		})
}

// roundRobin implements the `framework.InterFlowDispatchPolicy` interface using a round-robin strategy.
type roundRobin struct {
	iterator *roundrobin.Iterator
}

func newRoundRobin() framework.InterFlowDispatchPolicy {
	return &roundRobin{
		iterator: roundrobin.NewIterator(),
	}
}

// SelectQueue selects the next flow queue in a round-robin fashion from the given priority band.
// It returns nil if all queues in the band are empty or if an error occurs.
func (p *roundRobin) SelectQueue(band framework.PriorityBandAccessor) (framework.FlowQueueAccessor, error) {
	if band == nil {
		return nil, nil
	}
	selectedQueue := p.iterator.SelectNextQueue(band)
	return selectedQueue, nil
}
