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

// Package turns provides a FairnessMetric implementation that counts the number of times a flow has been selected for
// dispatch ("taken a turn") within a configured sliding time window.
package turns

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/fairnessmetrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/requestcontrol"
	schedulingtypes "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

const (
	// MetricName is the unique identifier for this fairness metric plugin.
	// This is the name used in configuration to refer to this specific metric implementation.
	MetricName = "turns"
	// defaultWindowSize defines the default duration for the sliding window.
	defaultWindowSize = 60 * time.Second
	// defaultBucketDur defines the default time resolution for each bucket in the window.
	defaultBucketDur = 1 * time.Second
)

// Config holds the configuration for the TurnsFairnessMetric.
type Config struct {
	// WindowSize is the total duration of the sliding window, specified as a
	// string (e.g., "5m", "10s").
	WindowSize string `json:"windowSize"`
	// BucketDuration is the time resolution of each bucket within the window,
	// specified as a string (e.g., "1s").
	BucketDuration string `json:"bucketDuration"`
}

// TurnsFairnessMetric counts the number of times a flow has been dispatched within a sliding window.
//
// This is a stateful plugin and is safe for concurrent use.
//
// It implements the following plugin interfaces:
//   - plugins.Plugin (base)
//   - framework.FairnessMetric
//   - requestcontrol.PreRequest (to update its counts)
type TurnsFairnessMetric struct {
	typedName      plugins.TypedName
	clock          clock.Clock
	mu             sync.Mutex // protects the dispatchCounts map
	dispatchCounts map[types.FlowKey]*fairnessmetrics.CircularBuffer[fairnessmetrics.NumericInt64]
	windowSize     time.Duration
	bucketDuration time.Duration
	numBuckets     int
	logger         logr.Logger
}

// New is the factory function for the TurnsFairnessMetric.
func New(name string, params json.RawMessage, handle plugins.Handle) (plugins.Plugin, error) {
	logger := logr.FromContextOrDiscard(handle.Context()).WithName("turns-factory")
	logger.Info("Creating TurnsFairnessMetric", "name", name, "params", string(params))
	cfg := Config{
		WindowSize:     defaultWindowSize.String(),
		BucketDuration: defaultBucketDur.String(),
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config for %s metric: %w", name, err)
		}
	}

	windowSize, err := time.ParseDuration(cfg.WindowSize)
	if err != nil {
		return nil, fmt.Errorf("invalid windowSize duration for %s metric: %w", name, err)
	}
	bucketDuration, err := time.ParseDuration(cfg.BucketDuration)
	if err != nil {
		return nil, fmt.Errorf("invalid bucketDuration for %s metric: %w", name, err)
	}
	if bucketDuration <= 0 {
		return nil, fmt.Errorf("bucketDuration must be positive for %s metric", name)
	}
	if windowSize < bucketDuration {
		return nil, fmt.Errorf("windowSize must be >= bucketDuration for %s metric", name)
	}

	m := &TurnsFairnessMetric{
		typedName:      plugins.TypedName{Type: framework.FairnessMetricType, Name: name},
		clock:          clock.RealClock{},
		dispatchCounts: make(map[types.FlowKey]*fairnessmetrics.CircularBuffer[fairnessmetrics.NumericInt64]),
		windowSize:     windowSize,
		bucketDuration: bucketDuration,
		numBuckets:     int(windowSize / bucketDuration),
		logger:         logr.FromContextOrDiscard(handle.Context()).WithName(MetricName),
	}
	if clk, ok := handle.(interface{ Clock() clock.Clock }); ok {
		m.clock = clk.Clock()
	}
	logger.Info("Successfully created TurnsFairnessMetric", "name", name)
	return m, nil
}

func init() {
	plugins.Register(MetricName, New)
}

// TypedName returns the type and name of the plugin instance.
func (m *TurnsFairnessMetric) TypedName() plugins.TypedName {
	return m.typedName
}

// GetValue returns the total number of dispatches for the given flow key within the current sliding window.
// It returns 0 for untracked keys.
func (m *TurnsFairnessMetric) GetValue(key types.FlowKey) float64 {
	m.mu.Lock()
	cb, exists := m.dispatchCounts[key]
	m.mu.Unlock()
	if !exists {
		return 0.0
	}
	return float64(cb.Get())
}

// GetValues returns the current turn counts for all specified flow keys.
// Untracked keys are omitted from the returned map.
func (m *TurnsFairnessMetric) GetValues(flowKeys []types.FlowKey) map[types.FlowKey]float64 {
	vals := make(map[types.FlowKey]float64, len(flowKeys))
	for _, key := range flowKeys {
		if val := m.GetValue(key); val > 0 {
			vals[key] = val
		}
	}
	return vals
}

// GetValues returns the current turn counts for all tracked flow keys.
func (m *TurnsFairnessMetric) GetAllValues() map[types.FlowKey]float64 {
	m.mu.Lock()
	countsSnapshot := make(map[types.FlowKey]*fairnessmetrics.CircularBuffer[fairnessmetrics.NumericInt64], len(m.dispatchCounts))
	maps.Copy(countsSnapshot, m.dispatchCounts)
	m.mu.Unlock()

	allValues := make(map[types.FlowKey]float64, len(countsSnapshot))
	for key, cb := range countsSnapshot {
		allValues[key] = float64(cb.Get())
	}
	return allValues
}

// PreRequest is called before a request is dispatched. It increments the turn count for the request's flow key.
func (m *TurnsFairnessMetric) PreRequest(_ context.Context, request *schedulingtypes.LLMRequest, _ *schedulingtypes.SchedulingResult) {
	key := request.FlowKey
	m.mu.Lock()
	cb, exists := m.dispatchCounts[key]
	if !exists {
		cb = fairnessmetrics.NewCircularBuffer[fairnessmetrics.NumericInt64](m.numBuckets, m.bucketDuration, m.clock)
		m.dispatchCounts[key] = cb
	}
	m.mu.Unlock()
	cb.Add(1)
}

var _ plugins.Plugin = &TurnsFairnessMetric{}
var _ framework.FairnessMetric = &TurnsFairnessMetric{}
var _ requestcontrol.PreRequest = &TurnsFairnessMetric{}
