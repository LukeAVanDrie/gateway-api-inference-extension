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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	testutils "sigs.k8s.io/gateway-api-inference-extension/test/utils"
)

// newTestBand creates a new MockPriorityBandAccessor based with the provided queues.
func newTestBand(t *testing.T, queues ...framework.FlowQueueAccessor) *mocks.MockPriorityBandAccessor {
	t.Helper()
	flowKeys := make([]types.FlowKey, 0, len(queues))
	queuesByID := make(map[string]framework.FlowQueueAccessor, len(queues))
	for _, q := range queues {
		key := q.FlowKey()
		flowKeys = append(flowKeys, key)
		queuesByID[key.ID] = q
	}
	return &mocks.MockPriorityBandAccessor{
		FlowKeysFunc: func() []types.FlowKey { return flowKeys },
		QueueFunc: func(id string) framework.FlowQueueAccessor {
			return queuesByID[id]
		},
		IterateQueuesFunc: func(iterator func(queue framework.FlowQueueAccessor) bool) {
			for _, key := range flowKeys {
				if !iterator(queuesByID[key.ID]) {
					break
				}
			}
		},
	}
}

// --- Conformance Test Suite ---

// TestInterFlowDispatchPolicyConformance is the main conformance test suite for all .InterFlowDispatchPolicy
// implementations.
// It discovers policies from the plugins.Registry and runs a series of sub-tests to ensure they adhere to the
// fundamental contracts of the interface.
func TestInterFlowDispatchPolicyConformance(t *testing.T) {
	t.Parallel()

	handle := testutils.NewTestHandle(context.Background())
	for pluginType, reg := range plugins.Registry {
		const pluginName = "my-inter-flow-plugin"
		plugin, err := reg.Factory(pluginName, nil, handle)
		if err != nil {
			t.Logf("Warning: Failed to instantiate plugin %s: %v", pluginName, err)
			continue
		}
		policy, ok := plugin.(framework.InterFlowDispatchPolicy)
		if !ok {
			continue
		}

		t.Run(pluginType, func(t *testing.T) {
			t.Parallel()

			t.Run("Initialization", func(t *testing.T) {
				t.Parallel()
				assert.Equal(t, pluginName, plugin.TypedName().Name,
					"TypedName().Name should match the plugin's name")
				assert.Equal(t, pluginType, plugin.TypedName().Type,
					"TypedName().Type should match the plugin's registered type")
			})

			t.Run("SelectQueue Contract", func(t *testing.T) {
				t.Parallel()
				runSelectQueueConformanceTests(t, policy)
			})
		})
	}
}

// runSelectQueueConformanceTests validates that a policy correctly handles edge cases like nil bands, empty bands, and
// bands with only empty queues.
// Every policy must handle these cases gracefully by returning a nil queue and no error.
func runSelectQueueConformanceTests(t *testing.T, policy framework.InterFlowDispatchPolicy) {
	t.Helper()

<<<<<<< HEAD
	flowIDEmpty := "flow-empty"
	mockQueueEmpty := &frameworkmocks.MockFlowQueueAccessor{
		LenV:      0,
		PeekHeadV: nil,
		FlowKeyV:  types.FlowKey{ID: flowIDEmpty},
=======
	emptyKey1 := types.FlowKey{ID: "empty1"}
	emptyQueue1 := &mocks.MockFlowQueueAccessor{
		FlowKeyV: emptyKey1,
		LenV:     0,
	}
	emptyKey2 := types.FlowKey{ID: "empty2"}
	emptyQueue2 := &mocks.MockFlowQueueAccessor{
		FlowKeyV: emptyKey2,
		LenV:     0,
>>>>>>> c7f7795 (feat: Adapt InterFlowDispatchPolicy to be a plugin)
	}

	testCases := []struct {
		name string
		band framework.PriorityBandAccessor
	}{
		{
			name: "handles a nil priority band accessor",
			band: nil,
		},
		{
			name: "handles a priority band with no queues",
			band: newTestBand(t),
		},
		{
			name: "handles a band containing one empty queue",
			band: newTestBand(t, emptyQueue1),
		},
		{
			name: "handles a band containing multiple empty queues",
			band: newTestBand(t, emptyQueue1, emptyQueue2),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			selectedQueue, err := policy.SelectQueue(tc.band)

			// The fundamental contract is that none of these edge cases should ever cause an error.
			// And in all these cases, no queue can possibly be selected.
			require.NoError(t, err, "SelectQueue for policy %s returned an unexpected error", policy.TypedName())
			assert.Nil(t, selectedQueue, "SelectQueue for policy %s should return a nil queue", policy.TypedName())
		})
	}
}
