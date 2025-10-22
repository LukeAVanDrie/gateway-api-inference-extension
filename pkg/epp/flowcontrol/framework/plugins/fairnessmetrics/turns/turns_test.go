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

package turns

import (
	"context"
	"encoding/json"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	schedulingtypes "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

type fakeHandle struct {
	plugins.Handle
	clock *testingclock.FakeClock
}

func (h fakeHandle) Clock() clock.Clock {
	return h.clock
}

func (h fakeHandle) Context() context.Context {
	return context.Background()
}

func TestTurnsFairnessMetric_New(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name      string
		config    json.RawMessage
		expectErr bool
	}{
		{
			name:      "valid config",
			config:    []byte(`{"windowSize": "10m", "bucketDuration": "5s"}`),
			expectErr: false,
		},
		{
			name:      "default config",
			config:    []byte(`{}`),
			expectErr: false,
		},
		{
			name:      "invalid windowSize duration",
			config:    []byte(`{"windowSize": "10minutes"}`),
			expectErr: true,
		},
		{
			name:      "invalid bucketDuration duration",
			config:    []byte(`{"bucketDuration": "5seconds"}`),
			expectErr: true,
		},
		{
			name:      "bucketDuration > windowSize",
			config:    []byte(`{"windowSize": "1s", "bucketDuration": "2s"}`),
			expectErr: true,
		},
		{
			name:      "zero bucketDuration",
			config:    []byte(`{"bucketDuration": "0s"}`),
			expectErr: true,
		},
		{
			name:      "invalid json",
			config:    []byte(`{"windowSize": "1m"`),
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metric, err := New(MetricName, tc.config, fakeHandle{})
			if tc.expectErr {
				require.Error(t, err, "expected an error for invalid config")
			} else {
				require.NoError(t, err, "expected no error for valid config")
				typedName := metric.TypedName()
				assert.Equal(t, MetricName, typedName.Name, "plugin name should match the metrics's constant")
				assert.Equal(t, framework.FairnessMetricType, typedName.Type, "plugin type should be FairnessMetric")
			}
		})
	}
}

func TestTurnsFairnessMetric_Lifecycle(t *testing.T) {
	t.Parallel()
	startTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeClock := testingclock.NewFakeClock(startTime)
	handle := fakeHandle{clock: fakeClock}

	config := json.RawMessage(`{"windowSize": "10s", "bucketDuration": "1s"}`)
	plugin, err := New(MetricName, config, handle)
	require.NoError(t, err, "metric should initialize without error")
	metric := plugin.(*TurnsFairnessMetric)

	keyA := types.FlowKey{ID: "A", Priority: 0}
	keyB := types.FlowKey{ID: "B", Priority: 0}
	keyC := types.FlowKey{ID: "C", Priority: 0} // Untracked key
	reqA := &schedulingtypes.LLMRequest{FlowKey: keyA}
	reqB := &schedulingtypes.LLMRequest{FlowKey: keyB}

	// Initial state: GetValue on non-existent key returns 0.
	valA := metric.GetValue(keyA)
	require.Equal(t, 0.0, valA, "value for a non-existent key should be 0")

	// Initial state: GetValues and GetAllValues
	valsAB := metric.GetValues([]types.FlowKey{keyA, keyB})
	assert.Empty(t, valsAB, "GetValues should be empty for untracked keys")
	allVals := metric.GetAllValues()
	assert.Empty(t, allVals, "GetAllValues should be empty initially")

	// PreRequest for keyA
	metric.PreRequest(context.Background(), reqA, nil)
	valA = metric.GetValue(keyA)
	require.Equal(t, 1.0, valA, "value should be 1 after first request")

	// PreRequest for keyA again
	metric.PreRequest(context.Background(), reqA, nil)
	valA = metric.GetValue(keyA)
	require.Equal(t, 2.0, valA, "value should be 2 after second request for the same key")

	// PreRequest for keyB
	metric.PreRequest(context.Background(), reqB, nil)
	valB := metric.GetValue(keyB)
	require.Equal(t, 1.0, valB, "value for key B should be 1")
	valsABC := metric.GetValues([]types.FlowKey{keyA, keyB, keyC})
	expectedValsABC := map[types.FlowKey]float64{keyA: 2.0, keyB: 1.0}
	assert.True(t, maps.Equal(expectedValsABC, valsABC), "GetValues should return values for tracked keys A and B")
	allVals = metric.GetAllValues()
	expectedAllVals := map[types.FlowKey]float64{keyA: 2.0, keyB: 1.0}
	assert.True(t, maps.Equal(expectedAllVals, allVals), "GetAllValues should return all tracked keys and values")

	// Advance time beyond window, value for key A should expire.
	fakeClock.Step(11 * time.Second)
	metric.PreRequest(context.Background(), reqB, nil) // A request to trigger the advance for keyB
	valA = metric.GetValue(keyA)
	require.Equal(t, 0.0, valA, "value for key A should be 0 after window expires")
	valB = metric.GetValue(keyB)
	require.Equal(t, 1.0, valB, "value for key B should be 1 (1 expired, 1 new)")
	valsAB = metric.GetValues([]types.FlowKey{keyA, keyB})
	expectedValsAB := map[types.FlowKey]float64{keyB: 1.0} // keyA is now 0, so omitted
	assert.True(t, maps.Equal(expectedValsAB, valsAB), "GetValues should omit keyA after its value becomes 0")
	allVals = metric.GetAllValues()
	expectedAllVals = map[types.FlowKey]float64{keyA: 0.0, keyB: 1.0}
	assert.True(t, maps.Equal(expectedAllVals, allVals), "GetAllValues should show keyA as 0 and keyB as 1")
}

func TestTurnsFairnessMetric_Concurrency(t *testing.T) {
	const numGoroutines = 100
	const requestsPerGoroutine = 50

	// --- Arrange ---
	config := json.RawMessage(`{"windowSize": "1m", "bucketDuration": "1s"}`)
	plugin, err := New(MetricName, config, fakeHandle{clock: testingclock.NewFakeClock(time.Now())})
	require.NoError(t, err, "metric should initialize without error")
	metric := plugin.(*TurnsFairnessMetric)

	keys := []types.FlowKey{
		{ID: "A", Priority: 0},
		{ID: "B", Priority: 0},
		{ID: "C", Priority: 1},
	}
	requests := []*schedulingtypes.LLMRequest{
		{FlowKey: keys[0]},
		{FlowKey: keys[1]},
		{FlowKey: keys[2]},
	}

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// --- Act ---
	// Hammer the PreRequest method from many goroutines with a shared set of keys.
	// This creates contention on both the map lock and the underlying buffer locks.
	for i := range numGoroutines {
		go func(routineID int) {
			defer wg.Done()
			for j := range requestsPerGoroutine {
				// Distribute requests across the keys.
				req := requests[(routineID+j)%len(requests)]
				metric.PreRequest(context.Background(), req, nil)
			}
		}(i)
	}
	wg.Wait()

	var totalCount float64
	for _, key := range keys {
		totalCount += metric.GetValue(key)
	}
	expectedTotal := float64(numGoroutines * requestsPerGoroutine)
	require.Equal(t, expectedTotal, totalCount, "sum of all values must equal total number of requests after concurrency")
}
