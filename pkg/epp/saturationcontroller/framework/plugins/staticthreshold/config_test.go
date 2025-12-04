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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewConfig_Validation(t *testing.T) {
	t.Parallel()

	intPtr := func(i int) *int { return &i }
	floatPtr := func(f float64) *float64 { return &f }
	durPtr := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

	tests := []struct {
		name        string
		input       *pluginConfig
		wantConfig  *Config
		wantErr     bool
		errContains string
	}{
		{
			name:  "Nil input returns defaults",
			input: nil,
			wantConfig: &Config{
				queueDepthThreshold:       defaultQueueDepthThreshold,
				kvCacheUtilThreshold:      defaultKVCacheUtilThreshold,
				metricsStalenessThreshold: defaultMetricsStalenessThreshold,
			},
			wantErr: false,
		},
		{
			name: "Valid custom configuration",
			input: &pluginConfig{
				QueueDepthThreshold:       intPtr(10),
				KVCacheUtilThreshold:      floatPtr(0.5),
				MetricsStalenessThreshold: durPtr(500 * time.Millisecond),
			},
			wantConfig: &Config{
				queueDepthThreshold:       10,
				kvCacheUtilThreshold:      0.5,
				metricsStalenessThreshold: 500 * time.Millisecond,
			},
			wantErr: false,
		},
		{
			name: "QueueDepth 0 is valid (Latency Mode)",
			input: &pluginConfig{
				QueueDepthThreshold: intPtr(0),
			},
			wantConfig: &Config{
				queueDepthThreshold:       0,
				kvCacheUtilThreshold:      defaultKVCacheUtilThreshold,
				metricsStalenessThreshold: defaultMetricsStalenessThreshold,
			},
			wantErr: false,
		},
		{
			name: "Negative QueueDepth invalid",
			input: &pluginConfig{
				QueueDepthThreshold: intPtr(-1),
			},
			wantErr:     true,
			errContains: "queueDepthThreshold must be non-negative",
		},
		{
			name: "KV Cache 0.0 invalid (No Capacity)",
			input: &pluginConfig{
				KVCacheUtilThreshold: floatPtr(0.0),
			},
			wantErr:     true,
			errContains: "kvCacheUtilThreshold must be strictly between 0 and 1",
		},
		{
			name: "KV Cache 1.0 valid",
			input: &pluginConfig{
				KVCacheUtilThreshold: floatPtr(1.0),
			},
			wantConfig: &Config{
				queueDepthThreshold:       defaultQueueDepthThreshold,
				kvCacheUtilThreshold:      1.0,
				metricsStalenessThreshold: defaultMetricsStalenessThreshold,
			},
			wantErr: false,
		},
		{
			name: "KV Cache > 1.0 invalid",
			input: &pluginConfig{
				KVCacheUtilThreshold: floatPtr(1.1),
			},
			wantErr:     true,
			errContains: "kvCacheUtilThreshold must be strictly between 0 and 1",
		},
		{
			name: "Staleness negative invalid",
			input: &pluginConfig{
				MetricsStalenessThreshold: durPtr(-1 * time.Second),
			},
			wantErr:     true,
			errContains: "metricsStalenessThreshold must be positive",
		},
		{
			name: "Staleness 0 invalid",
			input: &pluginConfig{
				MetricsStalenessThreshold: durPtr(0),
			},
			wantErr:     true,
			errContains: "metricsStalenessThreshold must be positive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewConfig(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantConfig, got)
			}
		})
	}
}

func TestSaturationControllerFactory_JSONParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		jsonParams  string
		wantErr     bool
		errContains string
		checkConfig func(*Config) // Optional callback to inspect the parsed config
	}{
		{
			name:       "Empty JSON returns defaults",
			jsonParams: `{}`,
			wantErr:    false,
			checkConfig: func(c *Config) {
				assert.Equal(t, defaultQueueDepthThreshold, c.queueDepthThreshold)
			},
		},
		{
			name:       "Partial JSON applies defaults",
			jsonParams: `{"queueDepthThreshold": 20}`,
			wantErr:    false,
			checkConfig: func(c *Config) {
				assert.Equal(t, 20, c.queueDepthThreshold)
				assert.Equal(t, defaultKVCacheUtilThreshold, c.kvCacheUtilThreshold)
			},
		},
		{
			name:       "Valid Duration String",
			jsonParams: `{"metricsStalenessThreshold": "1h"}`,
			wantErr:    false,
			checkConfig: func(c *Config) {
				assert.Equal(t, time.Hour, c.metricsStalenessThreshold)
			},
		},
		{
			name:        "Invalid JSON syntax",
			jsonParams:  `{"queueDepthThreshold": "NotAnInt"}`,
			wantErr:     true,
			errContains: "failed to unmarshal parameters",
		},
		{
			name:        "Logic Error (Negative Queue)",
			jsonParams:  `{"queueDepthThreshold": -5}`,
			wantErr:     true,
			errContains: "queueDepthThreshold must be non-negative",
		},
		{
			name:        "Logic Error (Bad Duration format)",
			jsonParams:  `{"metricsStalenessThreshold": "not-a-duration"}`,
			wantErr:     true,
			errContains: "failed to unmarshal", // metav1.Duration fails unmarshal on bad string
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plugin, err := StaticThresholdSaturationControllerFactory("test-instance", json.RawMessage(tc.jsonParams), nil)

			if tc.wantErr {
				require.Error(t, err)
				if tc.errContains != "" {
					assert.Contains(t, err.Error(), tc.errContains)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, plugin)

			// Cast back to concrete type to inspect internal config state.
			controller, ok := plugin.(*controller)
			require.True(t, ok, "Factory returned wrong type")

			if tc.checkConfig != nil {
				tc.checkConfig(controller.config)
			}
		})
	}
}
