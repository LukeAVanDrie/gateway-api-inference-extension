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
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	fwkdl "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/datalayer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/plugins/datalayer/extractor/metrics/generic"
)

type mockEvaluator struct{}

func (m *mockEvaluator) EvaluateEpoch(delta *datalayer.EpochDelta, currentUsed ResourceVector) {}
func (m *mockEvaluator) SetKVBlocks(totalKVBlocks int64)                                       {}
func (m *mockEvaluator) GetKVBlocks() int64                                                    { return 1000 }

type mockMeta struct {
	attr fwkdl.AttributeMap
}

func (m *mockMeta) GetMetadata() *fwkdl.EndpointMetadata {
	return &fwkdl.EndpointMetadata{
		NamespacedName: types.NamespacedName{Name: "test-ep"},
	}
}

func (m *mockMeta) UpdateMetadata(*fwkdl.EndpointMetadata) {}
func (m *mockMeta) GetAttributes() fwkdl.AttributeMap      { return m.attr }

type mockEndpoint struct {
	mockMeta
}

func (m *mockEndpoint) String() string                 { return "mock" }
func (m *mockEndpoint) GetMetrics() *fwkdl.Metrics     { return &fwkdl.Metrics{} }
func (m *mockEndpoint) UpdateMetrics(_ *fwkdl.Metrics) {}

func TestTelemetryBridge(t *testing.T) {
	t.Parallel()

	ledger := &TwoTierLedger{}
	bridge := NewTelemetryBridge(ledger)

	deltaEngine := &datalayer.PodDeltaEngine{}
	evaluator := &mockEvaluator{}

	// "/test-ep" is what types.NamespacedName{Name: "test-ep"}.String() returns
	bridge.RegisterEndpoint("/test-ep", deltaEngine, evaluator)

	if len(bridge.endpoints) != 1 {
		t.Errorf("Expected 1 endpoint, got %d", len(bridge.endpoints))
	}

	bridge.DeregisterEndpoint("/test-ep")

	if len(bridge.endpoints) != 0 {
		t.Errorf("Expected 0 endpoints, got %d", len(bridge.endpoints))
	}
}

func TestReconcile(t *testing.T) {
	t.Parallel()

	ledger := &TwoTierLedger{}
	bridge := NewTelemetryBridge(ledger)

	deltaEngine := &datalayer.PodDeltaEngine{}
	evaluator := &mockEvaluator{}

	bridge.RegisterEndpoint("/test-ep", deltaEngine, evaluator)

	attr := fwkdl.NewAttributes()
	attr.Put("prometheus_vllm_generation_tokens_total", &generic.FloatValue{Value: 200.0})
	attr.Put("prometheus_vllm_request_success_total", &generic.FloatValue{Value: 10.0})
	attr.Put("prometheus_vllm_cache_usage", &generic.FloatValue{Value: 0.50})
	attr.Put("prometheus_vllm_num_requests_running", &generic.FloatValue{Value: 2.0})
	attr.Put("prometheus_vllm_num_requests_swapped", &generic.FloatValue{Value: 1.0})

	hist := &generic.HistogramValue{
		Snapshot: datalayer.HistogramSnapshot{
			Sum:   1.0,
			Count: 1,
		},
	}
	attr.Put("prometheus_vllm_time_per_output_token_seconds", hist)
	attr.Put("prometheus_vllm_time_to_first_token_seconds", hist)
	attr.Put("prometheus_vllm_prefill_seconds", hist)

	ep := &mockEndpoint{mockMeta: mockMeta{attr: attr}}

	bridge.Reconcile([]fwkdl.Endpoint{ep})
}
