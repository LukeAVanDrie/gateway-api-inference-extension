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
	"fmt"
	"reflect"

	dto "github.com/prometheus/client_model/go"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	fwkdl "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/datalayer"
	fwkplugin "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
	sourcemetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/plugins/datalayer/source/metrics"
)

const GenericPrometheusMetricPluginType = "generic-prometheus-metric"

// Spec defines the query structure for reaching into Prometheus maps.
type Spec struct {
	Name   string
	Labels map[string]string
}

// Extractor performs decoupling and aggregation of dynamically searched Prometheus metrics.
type Extractor struct {
	typedName fwkplugin.TypedName
	specs     map[string]*Spec // Internal Alias -> Metric Spec
}

// FloatValue is a simple wrapper for Attribute since values must be Cloneable.
type FloatValue struct {
	Value float64
}

func (f *FloatValue) Clone() fwkdl.Cloneable {
	return &FloatValue{Value: f.Value}
}

// HistogramValue allows snapshotting complex datasets to endpoint contexts.
type HistogramValue struct {
	Snapshot datalayer.HistogramSnapshot
}

func (h *HistogramValue) Clone() fwkdl.Cloneable {
	clone := &HistogramValue{
		Snapshot: datalayer.HistogramSnapshot{
			Count:   h.Snapshot.Count,
			Sum:     h.Snapshot.Sum,
			Buckets: make([]datalayer.Bucket, len(h.Snapshot.Buckets)),
		},
	}
	copy(clone.Snapshot.Buckets, h.Snapshot.Buckets)
	return clone
}

var (
	_ fwkplugin.ProducerPlugin = &Extractor{}
	_ fwkdl.Extractor          = &Extractor{}
)

// NewGenericPrometheusExtractor returns an Extractor bound to decoupling named structures.
func NewGenericPrometheusExtractor(specs map[string]*Spec) *Extractor {
	return &Extractor{
		typedName: fwkplugin.TypedName{
			Type: GenericPrometheusMetricPluginType,
			Name: GenericPrometheusMetricPluginType,
		},
		specs: specs,
	}
}

func (e *Extractor) TypedName() fwkplugin.TypedName {
	return e.typedName
}

func (e *Extractor) ExpectedInputType() reflect.Type {
	return sourcemetrics.PrometheusMetricType
}

func (e *Extractor) Produces() map[string]any {
	results := make(map[string]any)
	for alias := range e.specs {
		results[alias] = (*fwkdl.Cloneable)(nil)
	}
	return results
}

func (e *Extractor) Extract(ctx context.Context, data any, ep fwkdl.Endpoint) error {
	families, ok := data.(sourcemetrics.PrometheusMetricMap)
	if !ok {
		return fmt.Errorf("unexpected input in Extract: %T", data)
	}

	for alias, spec := range e.specs {
		family, exists := families[spec.Name]
		if !exists {
			continue
		}

		cloneable := aggregateMetricFamily(family, spec)
		if cloneable != nil {
			ep.GetAttributes().Put("prometheus_"+alias, cloneable)
		}
	}

	return nil
}

// aggregateMetricFamily iterates over all series in a metric family. If multiple series match the
// label spec, they are mathematically summed to provide an accurate endpoint-level aggregate,
// preventing silent data drops.
func aggregateMetricFamily(family *dto.MetricFamily, spec *Spec) fwkdl.Cloneable {
	var (
		isFloat     bool
		floatSum    float64
		isHist      bool
		histCount   uint64
		histSum     float64
		histBuckets []datalayer.Bucket
	)

	for _, metric := range family.GetMetric() {
		if !matchesLabels(metric, spec.Labels) {
			continue
		}

		// Accumulate float types (Gauges, Counters, Untyped).
		if metric.Gauge != nil {
			floatSum += metric.Gauge.GetValue()
			isFloat = true
		} else if metric.Counter != nil {
			floatSum += metric.Counter.GetValue()
			isFloat = true
		} else if metric.Untyped != nil {
			floatSum += metric.Untyped.GetValue()
			isFloat = true

			// Accumulate histogram types (fast-path slice merging).
		} else if metric.Histogram != nil {
			isHist = true
			h := metric.Histogram

			histCount += h.GetSampleCount()
			histSum += h.GetSampleSum()

			// Initialize the bucket slice on the first match.
			if histBuckets == nil {
				histBuckets = make([]datalayer.Bucket, len(h.Bucket))
				for i, b := range h.Bucket {
					histBuckets[i] = datalayer.Bucket{
						UpperBound: b.GetUpperBound(),
						Count:      b.GetCumulativeCount(),
					}
				}
			} else {
				// Fast-path merge: Assumes standard identical bucket topologies across dimensions.
				for i, b := range h.Bucket {
					if i < len(histBuckets) && histBuckets[i].UpperBound == b.GetUpperBound() {
						histBuckets[i].Count += b.GetCumulativeCount()
					}
				}
			}
		}
	}

	if isHist {
		return &HistogramValue{
			Snapshot: datalayer.HistogramSnapshot{
				Count:   histCount,
				Sum:     histSum,
				Buckets: histBuckets,
			},
		}
	} else if isFloat {
		return &FloatValue{Value: floatSum}
	}

	return nil
}

// matchesLabels executes a zero-allocation check to verify a metric series contains all required labels.
func matchesLabels(metric *dto.Metric, requiredLabels map[string]string) bool {
	if len(requiredLabels) == 0 {
		return true // No constraints, aggregate everything in the family.
	}

	for reqKey, reqVal := range requiredLabels {
		found := false
		for _, label := range metric.GetLabel() {
			if label.GetName() == reqKey && label.GetValue() == reqVal {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
