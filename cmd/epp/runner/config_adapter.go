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
	"os"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configapi "sigs.k8s.io/gateway-api-inference-extension/apix/config/v1alpha1"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol"
	satctrl "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationcontroller/framework/plugins/staticthreshold"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/env"
)

const (
	// --- Deprecated Feature Gates ---
	envEnableDatalayerV2 = "ENABLE_EXPERIMENTAL_DATALAYER_V2"
	envEnableFlowControl = "ENABLE_EXPERIMENTAL_FLOW_CONTROL_LAYER"

	// --- Deprecated Saturation Detector Config ---
	envSdQueueDepth       = "SD_QUEUE_DEPTH_THRESHOLD"
	envSdKVCacheUtil      = "SD_KV_CACHE_UTIL_THRESHOLD"
	envSdMetricsStaleness = "SD_METRICS_STALENESS_THRESHOLD"
)

// applyDeprecatedOverrides patches the raw configuration based on legacy environment variables.
// This function serves as an Anti-Corruption Layer, isolating deprecated logic from the main runner.
//
// TODO: Remove this entire file in the next major release.
func applyDeprecatedOverrides(log logr.Logger, raw *configapi.EndpointPickerConfig) {
	applyDeprecatedFeatureGates(log, raw)
	applyDeprecatedSaturationConfig(log, raw)
}

func applyDeprecatedFeatureGates(log logr.Logger, raw *configapi.EndpointPickerConfig) {
	// Helper to check env and append feature gate if enabled.
	checkAndEnable := func(envVar, featureGate string) {
		if _, exists := os.LookupEnv(envVar); exists {
			log.Info("DEPRECATION WARNING: Enabling feature via environment variable is deprecated.",
				"envVar", envVar,
				"featureGate", featureGate,
				"action", "Use 'featureGates' field in the configuration file instead.")

			if env.GetEnvBool(envVar, false, log) {
				raw.FeatureGates = append(raw.FeatureGates, featureGate)
			}
		}
	}

	checkAndEnable(envEnableDatalayerV2, datalayer.FeatureGate)
	checkAndEnable(envEnableFlowControl, flowcontrol.FeatureGate)
}

func applyDeprecatedSaturationConfig(log logr.Logger, raw *configapi.EndpointPickerConfig) {
	// Check if any legacy env vars are present. If not, exit early.
	if os.Getenv(envSdQueueDepth) == "" &&
		os.Getenv(envSdKVCacheUtil) == "" &&
		os.Getenv(envSdMetricsStaleness) == "" {
		return
	}

	log.Info("DEPRECATION WARNING: Configuring Saturation Detector via environment variables is deprecated.",
		"action", "Configure the 'SaturationController' plugin parameters in the configuration file.")

	// 1. Locate or Create the Plugin Spec
	var spec *configapi.PluginSpec
	for i := range raw.Plugins {
		if raw.Plugins[i].Type == satctrl.StaticThresholdSaturationControllerType {
			spec = &raw.Plugins[i]
			break
		}
	}

	// If the plugin isn't explicitly configured, add it so we can patch the defaults.
	if spec == nil {
		raw.Plugins = append(raw.Plugins, configapi.PluginSpec{
			Name: satctrl.StaticThresholdSaturationControllerType,
			Type: satctrl.StaticThresholdSaturationControllerType,
		})
		spec = &raw.Plugins[len(raw.Plugins)-1]
	}

	// 2. Define Patch Structure
	// This struct matches the JSON contract of the Saturation Controller's config.
	// We use pointers to distinguish between "unset" and "zero value".
	type saturationParams struct {
		QueueDepth       *int             `json:"queueDepthThreshold,omitempty"`
		KVCache          *float64         `json:"kvCacheUtilThreshold,omitempty"`
		MetricsStaleness *metav1.Duration `json:"metricsStalenessThreshold,omitempty"`
	}

	// 3. Unmarshal existing params (if any) to preserve non-conflicting settings.
	params := &saturationParams{}
	if len(spec.Parameters) > 0 {
		if err := json.Unmarshal(spec.Parameters, params); err != nil {
			log.Error(err, "Failed to unmarshal existing saturation parameters while applying legacy overrides. Overwriting with env vars.")
			// Proceeding with empty params to ensure env vars are applied.
			params = &saturationParams{}
		}
	}

	// 4. Apply Overrides
	if val := env.GetEnvInt(envSdQueueDepth, 0, log); val > 0 {
		params.QueueDepth = &val
	}
	if val := env.GetEnvFloat(envSdKVCacheUtil, 0, log); val > 0 {
		params.KVCache = &val
	}
	if val := env.GetEnvDuration(envSdMetricsStaleness, 0, log); val > 0 {
		d := metav1.Duration{Duration: val}
		params.MetricsStaleness = &d
	}

	// 5. Commit Changes
	newBytes, err := json.Marshal(params)
	if err != nil {
		log.Error(err, "Failed to marshal patched saturation parameters. Legacy overrides ignored.")
		return
	}
	spec.Parameters = newBytes
}
