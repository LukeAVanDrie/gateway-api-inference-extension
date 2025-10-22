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

package maxminfairness

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

// --- Test Doubles (Fakes) ---

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
	// A real handle would key by type then name, but this is sufficient for testing.
	return h.pluginRegistry[name]
}

// A simple plugin that does not implement the FairnessMetric interface, used for testing type assertion failures.
type nonMetricPlugin struct{ plugins.Plugin }

func (p *nonMetricPlugin) TypedName() plugins.TypedName {
	return plugins.TypedName{Name: "not-a-metric"}
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

// --- Tests ---

func TestMaxMinFairness_New(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		config          json.RawMessage
		pluginsInHandle map[string]plugins.Plugin
		expectErr       bool
		errContains     string
	}{
		{
			name:   "success: valid config with correct metric dependency",
			config: []byte(`{"metricName": "test-turns"}`),
			pluginsInHandle: map[string]plugins.Plugin{
				"test-turns": &fakeMetric{},
			},
			expectErr: false,
		},
		{
			name:        "error: metricName is missing",
			config:      []byte(`{}`),
			expectErr:   true,
			errContains: "metricName is a required configuration field",
		},
		{
			name:            "error: metric plugin not found in handle",
			config:          []byte(`{"metricName": "not-found-metric"}`),
			pluginsInHandle: map[string]plugins.Plugin{"test-turns": &fakeMetric{}},
			expectErr:       true,
			errContains:     "there is no plugin with the name",
		},
		{
			name:   "error: referenced plugin is not a FairnessMetric",
			config: []byte(`{"metricName": "wrong-type"}`),
			pluginsInHandle: map[string]plugins.Plugin{
				"wrong-type": &nonMetricPlugin{},
			},
			expectErr:   true,
			errContains: "is not an instance of",
		},
		{
			name:        "error: invalid json config",
			config:      []byte(`{"metricName":}`),
			expectErr:   true,
			errContains: "unmarshal",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handle := &fakeHandle{pluginRegistry: tc.pluginsInHandle}
			policy, err := NewMaxMinFairness(PolicyNameMaxMinFairness, tc.config, handle)

			if tc.expectErr {
				require.Error(t, err, "expected an error during policy creation")
				require.ErrorContains(t, err, tc.errContains, "error message should contain expected text")
			} else {
				require.NoError(t, err, "expected no error during policy creation")
				typedName := policy.TypedName()
				require.Equal(t, PolicyNameMaxMinFairness, typedName.Name, "plugin name should match the policy's constant")
				require.Equal(t, framework.InterFlowDispatchPolicyType, typedName.Type,
					"plugin type should be InterFlowDispatchPolicy")
			}
		})
	}
}

func TestMaxMinFairness_SelectQueue(t *testing.T) {
	t.Parallel()

	keyA := types.FlowKey{ID: "A", Priority: 0}
	keyB := types.FlowKey{ID: "B", Priority: 0}
	keyC := types.FlowKey{ID: "C", Priority: 0}
	keyD := types.FlowKey{ID: "D", Priority: 0}

	testCases := []struct {
		name                 string
		metricState          map[types.FlowKey]float64
		queues               []*mocks.MockFlowQueueAccessor
		expectedSelectionKey *types.FlowKey
	}{
		{
			name:                 "empty band: returns nil when no queues are active",
			metricState:          map[types.FlowKey]float64{keyA: 100},
			queues:               []*mocks.MockFlowQueueAccessor{{FlowKeyV: keyA, LenV: 0}},
			expectedSelectionKey: nil,
		},
		{
			name:                 "fast path: returns the only active queue",
			metricState:          map[types.FlowKey]float64{keyA: 100},
			queues:               []*mocks.MockFlowQueueAccessor{{FlowKeyV: keyA, LenV: 1}},
			expectedSelectionKey: &keyA,
		},
		{
			name: "simple case: selects queue with the lowest metric value",
			metricState: map[types.FlowKey]float64{
				keyA: 100,
				keyB: 50, // B is the clear minimum.
				keyC: 200,
			},
			queues: []*mocks.MockFlowQueueAccessor{
				{FlowKeyV: keyA, LenV: 1},
				{FlowKeyV: keyB, LenV: 1},
				{FlowKeyV: keyC, LenV: 1},
			},
			expectedSelectionKey: &keyB,
		},
		{
			name: "VTC counter lift: new (untracked) flow joins initialized set",
			metricState: map[types.FlowKey]float64{
				keyA: 100,
				keyB: 50, // B is the minimum initialized value.
			},
			// keyC is active but untracked. Its effective value is lifted to 50.
			// The policy should select the original minimum, keyB.
			queues: []*mocks.MockFlowQueueAccessor{
				{FlowKeyV: keyA, LenV: 1},
				{FlowKeyV: keyB, LenV: 1},
				{FlowKeyV: keyC, LenV: 1},
			},
			expectedSelectionKey: &keyB,
		},
		{
			name: "VTC counter lift: flow with zero value joins initialized set",
			metricState: map[types.FlowKey]float64{
				keyA: 100,
				keyB: 50, // B is the minimum initialized value.
				keyC: 0,  // C has an explicit zero value, treated as uninitialized.
			},
			// keyC's effective value is lifted to 50. The policy should select the original minimum, keyB.
			queues: []*mocks.MockFlowQueueAccessor{
				{FlowKeyV: keyA, LenV: 1},
				{FlowKeyV: keyB, LenV: 1},
				{FlowKeyV: keyC, LenV: 1},
			},
			expectedSelectionKey: &keyB,
		},
		{
			name: "VTC counter lift: complex case with multiple initialized and one untracked",
			metricState: map[types.FlowKey]float64{
				keyA: 100,
				keyB: 200,
				keyC: 50, // C is the true minimum of the initialized set.
			},
			// keyD is untracked. The min initialized value is 50 (from C).
			// keyD's effective value becomes 50. The policy should select C.
			queues: []*mocks.MockFlowQueueAccessor{
				{FlowKeyV: keyA, LenV: 1},
				{FlowKeyV: keyB, LenV: 1},
				{FlowKeyV: keyC, LenV: 1},
				{FlowKeyV: keyD, LenV: 1},
			},
			expectedSelectionKey: &keyC,
		},
		{
			name:        "all flows are new/untracked, selects first deterministically",
			metricState: map[types.FlowKey]float64{},
			queues: []*mocks.MockFlowQueueAccessor{
				{FlowKeyV: keyA, LenV: 1},
				{FlowKeyV: keyB, LenV: 1},
				{FlowKeyV: keyC, LenV: 1},
			},
			expectedSelectionKey: &keyA,
		},
		{
			name:        "all flows have zero value, selects first deterministically",
			metricState: map[types.FlowKey]float64{keyA: 0, keyB: 0, keyC: 0},
			queues: []*mocks.MockFlowQueueAccessor{
				{FlowKeyV: keyA, LenV: 1},
				{FlowKeyV: keyB, LenV: 1},
				{FlowKeyV: keyC, LenV: 1},
			},
			expectedSelectionKey: &keyA,
		},
		{
			name: "ignores empty queues when determining the minimum",
			metricState: map[types.FlowKey]float64{
				keyA: 20, // A is the minimum among *active* queues.
				keyB: 10, // B has the lowest value overall, but its queue is empty.
				keyC: 30,
			},
			queues: []*mocks.MockFlowQueueAccessor{
				{FlowKeyV: keyA, LenV: 1},
				{FlowKeyV: keyB, LenV: 0},
				{FlowKeyV: keyC, LenV: 1},
			},
			expectedSelectionKey: &keyA,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			metric := &fakeMetric{values: tc.metricState}
			handle := &fakeHandle{pluginRegistry: map[string]plugins.Plugin{"test-metric": metric}}
			config := json.RawMessage(`{"metricName": "test-metric"}`)
			plugin, err := NewMaxMinFairness(PolicyNameMaxMinFairness, config, handle)
			require.NoError(t, err, "test setup failed: could not create policy instance")
			policy, ok := plugin.(framework.InterFlowDispatchPolicy)
			require.True(t, ok, "instantiated plugin does not implement the required policy interface")

			var queues = make([]framework.FlowQueueAccessor, 0, len(tc.queues))
			for _, q := range tc.queues {
				queues = append(queues, q)
			}
			selected, err := policy.SelectQueue(newTestBand(t, queues...))

			require.NoError(t, err, "SelectQueue should not error in these scenarios")
			if tc.expectedSelectionKey == nil {
				require.Nil(t, selected, "expected no queue to be selected, but one was")
			} else {
				require.NotNil(t, selected, "expected a queue to be selected, but got nil")
				require.Equal(t, *tc.expectedSelectionKey, selected.FlowKey(), "the selected queue has the wrong flow key")
			}
		})
	}
}
