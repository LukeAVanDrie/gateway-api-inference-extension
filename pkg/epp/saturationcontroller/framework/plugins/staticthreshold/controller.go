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
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationcontroller/framework"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const StaticThresholdSaturationControllerType = "static-threshold-saturation-controller"

func init() {
	plugins.Register(StaticThresholdSaturationControllerType, StaticThresholdSaturationControllerFactory)
}

// StaticThresholdSaturationControllerFactory creates a new instance of the StaticThreshold Saturation Controller.
func StaticThresholdSaturationControllerFactory(name string, params json.RawMessage, handle plugins.Handle) (plugins.Plugin, error) {
	conf := &pluginConfig{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, conf); err != nil {
			return nil, fmt.Errorf("failed to unmarshal parameters for %s: %w", name, err)
		}
	}

	cfg, err := NewConfig(conf)
	if err != nil {
		return nil, err
	}
	return NewController(name, cfg), nil
}

// controller acts as a gatekeeper using a set of static saturation signals.
type controller struct {
	typedName plugins.TypedName
	config    *Config
	logger    logr.Logger
}

// Ensure Controller satisfies the interface.
var _ framework.SaturationController = &controller{}

// NewController creates a new instance of the Controller.
func NewController(name string, cfg *Config) *controller {
	resolvedName := name
	if name == "" {
		resolvedName = StaticThresholdSaturationControllerType
	}

	typedName := plugins.TypedName{
		Type: StaticThresholdSaturationControllerType,
		Name: resolvedName,
	}

	return &controller{
		typedName: typedName,
		config:    cfg,
		logger:    log.Log.WithName(StaticThresholdSaturationControllerType).WithValues("instance", typedName),
	}
}

// TypedName returns the name of the plugin instance.
func (c *controller) TypedName() plugins.TypedName {
	return c.typedName
}

// ShouldDispatch determines if the current request should be allowed to proceed to the scheduling layer.
//
// It returns true if at least one pod in the pool has "good capacity".
// "Good capacity" is defined as:
//  1. Metrics are fresh (not stale).
//  2. WaitingQueueSize <= QueueDepthThreshold.
//  3. KVCacheUsagePercent <= KVCacheUtilThreshold.
func (c *controller) ShouldDispatch(ctx context.Context, candidates []backendmetrics.PodMetrics) bool {
	logger := c.logger.V(logutil.TRACE)

	// Scale-from-Zero / Misconfiguration check.
	// If no candidates exist, we are effectively saturated (0 capacity).
	// Returning false triggers HoL blocking in the Flow Controller.
	if len(candidates) == 0 {
		return false
	}

	for _, podMetric := range candidates {
		metrics := podMetric.GetMetrics()
		podNn := "unknown-pod"
		if pod := podMetric.GetPod(); pod != nil {
			podNn = pod.NamespacedName.String()
		}

		if metrics == nil {
			if logger.Enabled() {
				logger.Info("Pod has nil metrics, skipping", "pod", podNn)
			}
			continue
		}

		// Check for metric staleness.
		if time.Since(metrics.UpdateTime) > c.config.metricsStalenessThreshold {
			logger.Info("Pod metrics are stale", "pod", podNn,
				"age", time.Since(metrics.UpdateTime),
				"threshold", c.config.metricsStalenessThreshold)
			continue
		}

		// Check queue depth (the set point).
		if metrics.WaitingQueueSize > c.config.queueDepthThreshold {
			logger.Info("Pod queue depth exceeded", "pod", podNn,
				"current", metrics.WaitingQueueSize,
				"threshold", c.config.queueDepthThreshold)
			continue
		}

		// Check KV cache utilization (the safety ceiling).
		if metrics.KVCacheUsagePercent > c.config.kvCacheUtilThreshold {
			logger.Info("Pod KV cache utilization exceeded", "pod", podNn,
				"current", metrics.KVCacheUsagePercent,
				"threshold", c.config.kvCacheUtilThreshold)
			continue
		}

		logger.Info("Found pod with good capacity", "pod", podNn)
		return true
	}

	logger.Info("System saturated: no pods with good capacity found")
	return false
}
