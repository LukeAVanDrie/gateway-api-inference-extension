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

package hypervisor

import (
	"context"
	"math"
	"reflect"
	"sync"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	fwkdl "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/datalayer"
	fwkplugin "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/plugins/datalayer/extractor/metrics/generic"
	sourcemetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/plugins/datalayer/source/metrics"
)

// EpochEvaluator abstracts the Auto-Tuner to break the cyclic import dependency.
type EpochEvaluator interface {
	EvaluateEpoch(delta *datalayer.EpochDelta, currentUsed ResourceVector)
	SetKVBlocks(totalKVBlocks int64)
	GetKVBlocks() int64
}

type endpointState struct {
	deltaEngine *datalayer.PodDeltaEngine
	autoTuner   EpochEvaluator
	endpointID  string
}

const (
	// Standard generic extraction scaling attributes applied against prometheus telemetry feeds.
	AttrTimePerOutputToken = "time_per_output_token_seconds"
	AttrTimeToFirstToken   = "time_to_first_token_seconds"
	AttrPrefillSeconds     = "prefill_seconds"
	AttrGenerationTokens   = "generation_tokens_total"
	AttrRequestSuccess     = "request_success_total"
	AttrNumSwapped         = "num_requests_swapped"
	AttrMaxNumSeqs         = "max_num_seqs"
)

// TelemetryBridge maintains a periodic reconciler state for extracting Prometheus metrics,
// translating them into vectors, and feeding the ledger and autonomic engines.
type TelemetryBridge struct {
	ledger TokenLedger

	mu        sync.RWMutex
	endpoints map[string]*endpointState
}

func (t *TelemetryBridge) TypedName() fwkplugin.TypedName {
	return fwkplugin.TypedName{
		Type: "hypervisor-metrics-bridge",
		Name: "hypervisor-metrics-bridge",
	}
}

func (t *TelemetryBridge) ExpectedInputType() reflect.Type {
	return sourcemetrics.PrometheusMetricType
}

var _ fwkdl.Extractor = (*TelemetryBridge)(nil)

// NewTelemetryBridge returns a fresh bridge attached to the hypervisor ledger.
func NewTelemetryBridge(ledger TokenLedger) *TelemetryBridge {
	return &TelemetryBridge{
		ledger:    ledger,
		endpoints: make(map[string]*endpointState),
	}
}

// RegisterEndpoint sets up a newly discovered endpoint in the state machine.
func (t *TelemetryBridge) RegisterEndpoint(endpointID string, deltaEngine *datalayer.PodDeltaEngine, tuner EpochEvaluator) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.endpoints[endpointID] = &endpointState{
		deltaEngine: deltaEngine,
		autoTuner:   tuner,
		endpointID:  endpointID,
	}
}

// DeregisterEndpoint cleans up endpoint tracking on removal.
func (t *TelemetryBridge) DeregisterEndpoint(endpointID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.endpoints, endpointID)
	t.ledger.RemoveEndpoint(endpointID)
}

func getFloatValue(attributes fwkdl.AttributeMap, key string) float64 {
	val, ok := attributes.Get(key)
	if !ok {
		return 0.0
	}
	if f, ok := val.(*generic.FloatValue); ok {
		return f.Value
	}
	return 0.0
}

func getHistogramValue(attributes fwkdl.AttributeMap, key string) datalayer.HistogramSnapshot {
	val, ok := attributes.Get(key)
	if !ok {
		return datalayer.HistogramSnapshot{}
	}
	if h, ok := val.(*generic.HistogramValue); ok {
		return h.Snapshot
	}
	return datalayer.HistogramSnapshot{}
}

// Extract acts as an Extractor that processes a snapshot update for a single endpoint.
func (t *TelemetryBridge) Extract(ctx context.Context, data any, ep fwkdl.Endpoint) error {
	meta := ep.GetMetadata()
	if meta == nil {
		return nil
	}

	endpointID := meta.NamespacedName.String()
	t.mu.RLock()
	state, exists := t.endpoints[endpointID]
	t.mu.RUnlock()

	if !exists {
		return nil
	}

	attr := ep.GetAttributes()

	// Required attribute keys from generic extractor
	tpot := getHistogramValue(attr, AttrTimePerOutputToken)
	ttft := getHistogramValue(attr, AttrTimeToFirstToken)
	prefill := getHistogramValue(attr, AttrPrefillSeconds)
	genTokens := getFloatValue(attr, AttrGenerationTokens)
	reqSuccess := getFloatValue(attr, AttrRequestSuccess)

	// Create the parse EpochSnapshot snapshot.
	snapshot := datalayer.EpochSnapshot{
		Timestamp:             time.Now(),
		TPOTHistogram:         tpot,
		TTFTHistogram:         ttft,
		PrefillHistogram:      prefill,
		GenerationTokensTotal: uint64(genTokens),
		RequestSuccessTotal:   uint64(reqSuccess),
	}

	// Vector Translation
	// Pull statically typed metrics scraped via standard metrics watcher where available to avoid
	// generic parsing overhead. Generic attributes are still used where explicit schemas aren't
	// mapped (for any non-standard metrics missing from the Model Server Protocol).
	var cacheUsagePer float64
	var running float64
	swapped := getFloatValue(attr, AttrNumSwapped)

	// Fetch dynamic parameters from extracted metadata.
	totalKVBlocks := state.autoTuner.GetKVBlocks()
	maxActiveRequests := getFloatValue(attr, AttrMaxNumSeqs)

	if metrics := ep.GetMetrics(); metrics != nil {
		cacheUsagePer = metrics.KVCacheUsagePercent
		running = float64(metrics.RunningRequestsSize)

		// Propagate rigid physical boundaries directly into the Ledger.
		// The Autotuner handles dynamic scaling of compute limits, but Physical Storage capacity
		// is determined explicitly via local instantiation.
		if metrics.CacheNumGPUBlocks > 0 {
			totalKVBlocks = int64(metrics.CacheNumGPUBlocks)
			t.ledger.UpdateEndpointKVBlocks(state.endpointID, totalKVBlocks)
			state.autoTuner.SetKVBlocks(totalKVBlocks)
		}
	}

	if maxActiveRequests > 0 {
		t.ledger.UpdateEndpointActiveRequests(state.endpointID, int64(maxActiveRequests))
	}

	// Calculate the instantaneous ResourceVector.
	currentUsage := ResourceVector{
		KVBlocks:       int64(math.Ceil(float64(totalKVBlocks) * cacheUsagePer / 100.0)),
		ActiveRequests: int64(running + swapped),
		PrefillTokens:  0, // Passive reliance on Ledger Transit Debt math
		DecodeTokens:   0, // Passive reliance on Ledger Transit Debt math
	}

	// Ledger Baseline Override
	t.ledger.ReconcileEndpointCapacity(state.endpointID, currentUsage)

	// Data Extractor Engine Scrape
	delta := state.deltaEngine.UpdateScrape(snapshot)

	// Auto-Tuner State Machine Update
	if delta != nil {
		state.autoTuner.EvaluateEpoch(delta, currentUsage)
	}

	return nil
}
