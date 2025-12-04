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

package runner

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	configapi "sigs.k8s.io/gateway-api-inference-extension/apix/config/v1alpha1"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationcontroller/framework/plugins/staticthreshold"
)

func TestApplyDeprecatedOverrides_FeatureGates(t *testing.T) {
	// t.Parallel() cannot be used with t.Setenv.

	// Enable deprecated feature gate via ENV.
	t.Setenv("ENABLE_EXPERIMENTAL_DATALAYER_V2", "true")
	raw := &configapi.EndpointPickerConfig{
		FeatureGates: []string{}, // Initially empty
	}

	applyDeprecatedOverrides(logr.Discard(), raw)

	assert.Contains(t, raw.FeatureGates, datalayer.FeatureGate, "deprecated env var should enable datalayer feature gate")
}

func TestApplyDeprecatedOverrides_SaturationConfig(t *testing.T) {
	// t.Parallel() cannot be used with t.Setenv.

	// Mixed environment (Env vars + Existing JSON)
	t.Setenv("SD_QUEUE_DEPTH_THRESHOLD", "99")
	t.Setenv("SD_METRICS_STALENESS_THRESHOLD", "15s")

	// Create a raw config that ALREADY has some settings in JSON.
	initialJSON := `{"kvCacheUtilThreshold": 0.5, "queueDepthThreshold": 10}`
	raw := &configapi.EndpointPickerConfig{
		Plugins: []configapi.PluginSpec{
			{
				Name:       staticthreshold.StaticThresholdSaturationControllerType,
				Type:       staticthreshold.StaticThresholdSaturationControllerType,
				Parameters: []byte(initialJSON),
			},
		},
	}

	applyDeprecatedOverrides(logr.Discard(), raw)

	// Decode result to verify.
	type saturationParams struct {
		QueueDepth       *int             `json:"queueDepthThreshold"`
		KVCache          *float64         `json:"kvCacheUtilThreshold"`
		MetricsStaleness *metav1.Duration `json:"metricsStalenessThreshold"`
	}
	res := &saturationParams{}
	spec := raw.Plugins[0]
	err := json.Unmarshal(spec.Parameters, res)
	require.NoError(t, err, "patched parameters should be valid JSON")

	// Queue Depth: Env Var (99) should OVERRIDE existing JSON (10).
	assert.NotNil(t, res.QueueDepth)
	assert.Equal(t, 99, *res.QueueDepth, "env var should take precedence over existing JSON")

	// KV Cache: No Env Var, should keep existing JSON (0.5).
	assert.NotNil(t, res.KVCache)
	assert.Equal(t, 0.5, *res.KVCache, "existing JSON should be preserved if env var is unset")

	// Staleness: No existing JSON, should be added from Env Var (15s).
	assert.NotNil(t, res.MetricsStaleness)
	assert.Equal(t, 15*time.Second, res.MetricsStaleness.Duration, "new parameter should be added from env var")
}
