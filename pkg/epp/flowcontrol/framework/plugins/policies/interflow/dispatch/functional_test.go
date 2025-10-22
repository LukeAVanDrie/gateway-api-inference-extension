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

package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"

	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/interflow/dispatch/besthead"
	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/interflow/dispatch/maxminfairness"
)

// --- Test Doubles (Fakes) for Conformance Testing ---

type fakeMetric struct {
	framework.FairnessMetric
	values map[types.FlowKey]float64
}

func (m *fakeMetric) TypedName() plugins.TypedName {
	return plugins.TypedName{Type: framework.FairnessMetricType, Name: "fake-metric"}
}

func (m *fakeMetric) GetValue(key types.FlowKey) float64 {
	return m.values[key] // Returns zero value (0.0) if key is not present
}

func (m *fakeMetric) GetValues(flowKeys []types.FlowKey) map[types.FlowKey]float64 {
	res := make(map[types.FlowKey]float64)
	for _, k := range flowKeys {
		if v, ok := m.values[k]; ok {
			res[k] = v
		}
	}
	return res
}

func (m *fakeMetric) GetAllValues() map[types.FlowKey]float64 {
	return m.values
}

type fakeHandle struct {
	plugins.Handle
	pluginRegistry map[string]plugins.Plugin
}

func (h *fakeHandle) Plugin(name string) plugins.Plugin {
	return h.pluginRegistry[name]
}

func (h *fakeHandle) AddPlugin(name string, plugin plugins.Plugin) {
	h.pluginRegistry[name] = plugin
}

func (h *fakeHandle) Context() context.Context {
	return context.Background()
}

// newConformanceTestHandle creates a mock plugins.Handle that is pre-populated
// with the dependencies required by the policies under test.
func newConformanceTestHandle(t *testing.T) plugins.Handle {
	t.Helper()
	handle := &fakeHandle{pluginRegistry: make(map[string]plugins.Plugin)}

	// Add a fake metric to satisfy the MaxMinFairness policy's dependency.
	metric := &fakeMetric{values: make(map[types.FlowKey]float64)}
	handle.AddPlugin("conformance-metric", metric)

	return handle
}

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

// TestInterFlowDispatchPolicyConformance is the main conformance test suite for
// all `framework.InterFlowDispatchPolicy` implementations.
//
// It discovers policies from the global `plugins.Registry` and runs a series of
// sub-tests to ensure they adhere to the fundamental contracts of the interface,
// especially around handling empty or nil inputs.
func TestInterFlowDispatchPolicyConformance(t *testing.T) {
	t.Parallel()

	// Define a map to hold any specific configurations required for each policy.
	policyConfigs := map[string]json.RawMessage{
		"BestHead":       []byte(`{}`),
		"MaxMinFairness": []byte(`{"metricName": "conformance-metric"}`),
	}

	handle := newConformanceTestHandle(t)

	for pluginName, factory := range plugins.Registry {
		// Get the specific config for this plugin, or nil if not defined.
		config, knownPolicy := policyConfigs[pluginName]
		if !knownPolicy {
			// Try to instantiate with nil config if not explicitly defined.
			config = nil
		}

		plugin, err := factory(pluginName, config, handle)
		if err != nil {
			t.Logf("Warning: Failed to instantiate plugin %s: %v", pluginName, err)
			continue
		}

		policy, ok := plugin.(framework.InterFlowDispatchPolicy)
		if !ok {
			// This plugin is not an InterFlowDispatchPolicy, skip it.
			continue
		}

		// Run conformance tests for this InterFlowDispatchPolicy
		t.Run(pluginName, func(t *testing.T) {
			t.Parallel()

			// --- Act & Assert ---
			t.Run("Initialization", func(t *testing.T) {
				t.Parallel()
				assert.Equal(t, pluginName, plugin.TypedName().Name,
					"TypedName().Name should match the plugin's registered name")
				assert.Equal(t, framework.InterFlowDispatchPolicyType, plugin.TypedName().Type,
					"TypedName().Type should be InterFlowDispatchPolicy")
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

	emptyKey1 := types.FlowKey{ID: "empty1"}
	emptyQueue1 := &mocks.MockFlowQueueAccessor{
		FlowKeyV: emptyKey1,
		LenV:     0,
	}
	emptyKey2 := types.FlowKey{ID: "empty2"}
	emptyQueue2 := &mocks.MockFlowQueueAccessor{
		FlowKeyV: emptyKey2,
		LenV:     0,
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
