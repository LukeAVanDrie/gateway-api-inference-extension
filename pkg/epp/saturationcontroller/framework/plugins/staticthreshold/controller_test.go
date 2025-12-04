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

package staticthreshold

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
)

// newMockPodMetrics is a helper to create fake metrics for testing.
func newMockPodMetrics(name string, metrics *backendmetrics.MetricsState) *backendmetrics.FakePodMetrics {
	return &backendmetrics.FakePodMetrics{
		Pod: &backend.Pod{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "ns1"},
		},
		Metrics: metrics,
	}
}

func TestController_ShouldDispatch(t *testing.T) {
	t.Parallel()

	baseTime := time.Now()
	defaultConfig := &Config{
		queueDepthThreshold:       5,
		kvCacheUtilThreshold:      0.90,
		metricsStalenessThreshold: 100 * time.Millisecond,
	}

	tests := []struct {
		name           string
		config         *Config
		pods           []backendmetrics.PodMetrics
		shouldDispatch bool
	}{
		{
			name:           "No candidate pods",
			config:         defaultConfig,
			pods:           []backendmetrics.PodMetrics{},
			shouldDispatch: false, // 0 Capacity = Do not dispatch
		},
		{
			name:   "Single pod with good capacity",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    2,
					KVCacheUsagePercent: 0.5,
				}),
			},
			shouldDispatch: true,
		},
		{
			name:   "Single pod with stale metrics",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime.Add(-200 * time.Millisecond), // Stale
					WaitingQueueSize:    1,
					KVCacheUsagePercent: 0.1,
				}),
			},
			shouldDispatch: false,
		},
		{
			name:   "Single pod with high queue depth",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    10, // Exceeds threshold 5
					KVCacheUsagePercent: 0.1,
				}),
			},
			shouldDispatch: false,
		},
		{
			name:   "Single pod with high KV cache utilization",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    1,
					KVCacheUsagePercent: 0.95, // Exceeds threshold 0.90
				}),
			},
			shouldDispatch: false,
		},
		{
			name:   "Single pod with nil metrics",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", nil),
			},
			shouldDispatch: false,
		},
		{
			name:   "Multiple pods, all good capacity",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    1,
					KVCacheUsagePercent: 0.1,
				}),
				newMockPodMetrics("pod2", &backendmetrics.MetricsState{
					UpdateTime:          baseTime.Add(-10 * time.Millisecond),
					WaitingQueueSize:    0,
					KVCacheUsagePercent: 0.2,
				}),
			},
			shouldDispatch: true,
		},
		{
			name:   "Multiple pods, one good, one bad (stale)",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime, // Good
					WaitingQueueSize:    1,
					KVCacheUsagePercent: 0.1,
				}),
				newMockPodMetrics("pod2", &backendmetrics.MetricsState{
					UpdateTime:          baseTime.Add(-300 * time.Millisecond), // Stale
					WaitingQueueSize:    0,
					KVCacheUsagePercent: 0.2,
				}),
			},
			shouldDispatch: true, // One good pod is enough
		},
		{
			name:   "Multiple pods, one good, one bad (high queue)",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    1,
					KVCacheUsagePercent: 0.1,
				}),
				newMockPodMetrics("pod2", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    15, // Bad queue
					KVCacheUsagePercent: 0.2,
				}),
			},
			shouldDispatch: true,
		},
		{
			name:   "Multiple pods, all bad capacity",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime.Add(-200 * time.Millisecond), // Stale
					WaitingQueueSize:    1,
					KVCacheUsagePercent: 0.1,
				}),
				newMockPodMetrics("pod2", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    20, // High queue
					KVCacheUsagePercent: 0.2,
				}),
				newMockPodMetrics("pod3", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    1,
					KVCacheUsagePercent: 0.99, // High KV
				}),
			},
			shouldDispatch: false,
		},
		{
			name:   "Queue depth exactly at threshold",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    defaultConfig.queueDepthThreshold, // Exactly at threshold (good)
					KVCacheUsagePercent: 0.1,
				}),
			},
			shouldDispatch: true,
		},
		{
			name:   "KV cache exactly at threshold",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime,
					WaitingQueueSize:    1,
					KVCacheUsagePercent: defaultConfig.kvCacheUtilThreshold, // Exactly at threshold (good)
				}),
			},
			shouldDispatch: true,
		},
		{
			name:   "Metrics age just over staleness threshold",
			config: defaultConfig,
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("pod1", &backendmetrics.MetricsState{
					UpdateTime:          baseTime.Add(-defaultConfig.metricsStalenessThreshold - time.Nanosecond), // Just over (stale)
					WaitingQueueSize:    1,
					KVCacheUsagePercent: 0.1,
				}),
			},
			shouldDispatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			controller := NewController("test-controller", tc.config)
			got := controller.ShouldDispatch(context.Background(), tc.pods)
			assert.Equal(t, tc.shouldDispatch, got)
		})
	}
}
