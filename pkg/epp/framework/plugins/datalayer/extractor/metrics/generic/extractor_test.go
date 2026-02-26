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

package generic

import (
	"context"
	"sync"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fwkdl "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/datalayer"
	sourcemetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/plugins/datalayer/source/metrics"
)

func makeFloat64(v float64) *float64 { return &v }
func makeUint64(v uint64) *uint64    { return &v }
func makeString(v string) *string    { return &v }

func TestExtractorTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		specs         map[string]*Spec
		mockFamilies  any
		validateFn    func(t *testing.T, ep fwkdl.Endpoint)
		expectedError string
	}{
		{
			name: "single matching standard float gauge value works accurately",
			specs: map[string]*Spec{
				"GpuTotal": {Name: "vllm:num_total_gpu_blocks"},
			},
			mockFamilies: sourcemetrics.PrometheusMetricMap{
				"vllm:num_total_gpu_blocks": &dto.MetricFamily{
					Metric: []*dto.Metric{
						{Gauge: &dto.Gauge{Value: makeFloat64(100.0)}},
					},
				},
			},
			validateFn: func(t *testing.T, ep fwkdl.Endpoint) {
				attr, ok := ep.GetAttributes().Get("prometheus_GpuTotal")
				require.True(t, ok, "prometheus_GpuTotal Attribute was not set during the snapshot extraction")
				assert.Equal(t, 100.0, attr.(*FloatValue).Value, "prometheus_GpuTotal scalar correctly recorded")
			},
		},
		{
			name: "aggregates matching series by summing multiple Gauge components into single scalar",
			specs: map[string]*Spec{
				"ActiveReq": {Name: "vllm:num_requests_running", Labels: map[string]string{"type": "inference"}},
			},
			mockFamilies: sourcemetrics.PrometheusMetricMap{
				"vllm:num_requests_running": &dto.MetricFamily{
					Metric: []*dto.Metric{
						{
							Label: []*dto.LabelPair{{Name: makeString("type"), Value: makeString("inference")}},
							Gauge: &dto.Gauge{Value: makeFloat64(2.0)},
						},
						{
							Label: []*dto.LabelPair{{Name: makeString("type"), Value: makeString("inference")}},
							Gauge: &dto.Gauge{Value: makeFloat64(1.0)},
						},
						{
							Label: []*dto.LabelPair{{Name: makeString("type"), Value: makeString("admin")}},
							Gauge: &dto.Gauge{Value: makeFloat64(500.0)}, // should be ignored
						},
					},
				},
			},
			validateFn: func(t *testing.T, ep fwkdl.Endpoint) {
				attr, ok := ep.GetAttributes().Get("prometheus_ActiveReq")
				require.True(t, ok, "prometheus_ActiveReq aggregated attribute correctly created")
				assert.Equal(t, 3.0, attr.(*FloatValue).Value, "Inference types dynamically accumulated")
			},
		},
		{
			name: "accurately aggregates dynamic histograms merging matching metrics",
			specs: map[string]*Spec{
				"TpotHist": {Name: "vllm:tpot"},
			},
			mockFamilies: sourcemetrics.PrometheusMetricMap{
				"vllm:tpot": &dto.MetricFamily{
					Metric: []*dto.Metric{
						{
							Histogram: &dto.Histogram{
								SampleCount: makeUint64(2),
								SampleSum:   makeFloat64(2.5),
								Bucket: []*dto.Bucket{
									{UpperBound: makeFloat64(1.0), CumulativeCount: makeUint64(1)},
									{UpperBound: makeFloat64(2.0), CumulativeCount: makeUint64(2)},
								},
							},
						},
						{
							Histogram: &dto.Histogram{
								SampleCount: makeUint64(1),
								SampleSum:   makeFloat64(1.0),
								Bucket: []*dto.Bucket{
									{UpperBound: makeFloat64(1.0), CumulativeCount: makeUint64(1)},
									{UpperBound: makeFloat64(2.0), CumulativeCount: makeUint64(1)},
								},
							},
						},
					},
				},
			},
			validateFn: func(t *testing.T, ep fwkdl.Endpoint) {
				attr, ok := ep.GetAttributes().Get("prometheus_TpotHist")
				require.True(t, ok, "TpotHist correctly extracted and persisted")
				hist := attr.(*HistogramValue).Snapshot
				assert.Equal(t, uint64(3), hist.Count)
				assert.Equal(t, 3.5, hist.Sum)
				require.Equal(t, 2, len(hist.Buckets))
				assert.Equal(t, 1.0, hist.Buckets[0].UpperBound)
				assert.Equal(t, uint64(2), hist.Buckets[0].Count) // 1 + 1
				assert.Equal(t, 2.0, hist.Buckets[1].UpperBound)
				assert.Equal(t, uint64(3), hist.Buckets[1].Count) // 2 + 1
			},
		},
		{
			name: "valid metric with extra labels (dynamic subset should match)",
			specs: map[string]*Spec{
				"GpuTotal": {
					Name:   "vllm:num_total_gpu_blocks",
					Labels: map[string]string{"label1": "value1"},
				},
			},
			mockFamilies: sourcemetrics.PrometheusMetricMap{
				"vllm:num_total_gpu_blocks": &dto.MetricFamily{
					Metric: []*dto.Metric{
						{
							Label: []*dto.LabelPair{
								{Name: makeString("label1"), Value: makeString("value1")},
								{Name: makeString("extra"), Value: makeString("foo")},
							},
							Gauge: &dto.Gauge{Value: makeFloat64(100.0)},
						},
					},
				},
			},
			validateFn: func(t *testing.T, ep fwkdl.Endpoint) {
				attr, ok := ep.GetAttributes().Get("prometheus_GpuTotal")
				require.True(t, ok, "prometheus_GpuTotal Attribute was not set during the extraction")
				assert.Equal(t, 100.0, attr.(*FloatValue).Value, "promethues_GpuTotal dynamically loaded properly with subset matching")
			},
		},
		{
			name:          "invalid data type",
			specs:         map[string]*Spec{"test": {Name: "test"}},
			mockFamilies:  "string which is not a PrometheusMetricMap",
			expectedError: "unexpected input in Extract: string",
			validateFn:    func(t *testing.T, ep fwkdl.Endpoint) {},
		},
		{
			name: "skips unhandled/malformed data",
			specs: map[string]*Spec{
				"MissingMetric": {Name: "vllm:does_not_exist"},
			},
			mockFamilies: sourcemetrics.PrometheusMetricMap{},
			validateFn: func(t *testing.T, ep fwkdl.Endpoint) {
				_, ok := ep.GetAttributes().Get("prometheus_MissingMetric")
				assert.False(t, ok, "should ignore metrics not matching specifications")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			extractor := NewGenericPrometheusExtractor(tt.specs)
			ep := fwkdl.NewEndpoint(nil, nil)

			err := extractor.Extract(context.Background(), tt.mockFamilies, ep)
			if tt.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedError)
			} else {
				require.NoError(t, err)
				if tt.validateFn != nil {
					tt.validateFn(t, ep)
				}
			}
		})
	}
}

func TestExtractorConcurrencySafe(t *testing.T) {
	t.Parallel()

	extractor := NewGenericPrometheusExtractor(map[string]*Spec{
		"CachePercent": {Name: "vllm:gpu_cache_usage_perc"},
	})

	mockFamily := &dto.MetricFamily{
		Metric: []*dto.Metric{
			{Gauge: &dto.Gauge{Value: makeFloat64(0.85)}},
		},
	}

	mockFamilies := sourcemetrics.PrometheusMetricMap{
		"vllm:gpu_cache_usage_perc": mockFamily,
	}

	ep := fwkdl.NewEndpoint(nil, nil)

	var wg sync.WaitGroup
	workers := 10 // Concurrent workers
	iterations := 100

	for range workers {
		wg.Go(func() {
			for range iterations {
				_ = extractor.Extract(context.Background(), mockFamilies, ep)
			}
		})
	}

	wg.Wait()

	// Ensure accurate state persisted correctly multiple times without panic.
	attr, ok := ep.GetAttributes().Get("prometheus_CachePercent")
	require.True(t, ok, "Concurrency successfully writes safely without triggering thread lockups across maps.")
	assert.Equal(t, 0.85, attr.(*FloatValue).Value, "State remains accurate concurrently.")
}
