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

// Package maxminfairness provides an InterFlowDispatchPolicy that maximizes the minimum service received by any flow,
// based on a configurable FairnessMetric.
package maxminfairness

import (
	"encoding/json"
	"fmt"
	"math"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

const PolicyNameMaxMinFairness = "MaxMinFairness"

// MaxMinFairness is an InterFlowDispatchPolicy that implements a max-min fairness algorithm, inspired by the Virtual
// Token Counter (VTC) approach for LLM serving (https://arxiv.org/abs/2401.00588)
//
// It operates by always selecting the queue from the flow that has received the least amount of service, as measured by
// a configured `FairnessMetric` plugin.
//
// This policy faithfully implements the "counter lift" mechanism described in the VTC paper. When a new or previously
// idle flow becomes active, its effective service value is "lifted" to the minimum service level of all other currently
// active flows. This prevents it from gaining an unfair, history-less advantage while still ensuring it gets
// prioritized.
//
// As this policy holds no internal mutable state, a single instance can be safely shared by multiple consumers (i.e.,
// it can be used as a singleton).
type MaxMinFairness struct {
	typedName plugins.TypedName
	metric    framework.FairnessMetric
}

// Config holds the configuration for the MaxMinFairness policy.
type Config struct {
	// MetricName is the name of the registered FairnessMetric plugin that this policy should use to measure service.
	// This field is required.
	MetricName string `json:"metricName"`
}

// NewMaxMinFairness is the factory function for the MaxMinFairness policy.
func NewMaxMinFairness(name string, params json.RawMessage, handle plugins.Handle) (plugins.Plugin, error) {
	if name != PolicyNameMaxMinFairness {
		return nil, fmt.Errorf("plugin name mismatch: expected %s, got %s", PolicyNameMaxMinFairness, name)
	}
	var cfg Config
	if err := json.Unmarshal(params, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config for %s policy: %w", name, err)
	}
	if cfg.MetricName == "" {
		return nil, fmt.Errorf("metricName is a required configuration field for %s policy", name)
	}

	metric, err := plugins.PluginByType[framework.FairnessMetric](handle, cfg.MetricName)
	if err != nil {
		return nil, fmt.Errorf("invalid reference to FairnessMetric plugin %q: %w", cfg.MetricName, err)
	}

	return &MaxMinFairness{
		typedName: plugins.TypedName{Type: framework.InterFlowDispatchPolicyType, Name: name},
		metric:    metric,
	}, nil
}

func init() {
	plugins.Register(PolicyNameMaxMinFairness, NewMaxMinFairness)
}

// TypedName returns the type and name of the plugin instance.
func (p *MaxMinFairness) TypedName() plugins.TypedName {
	return p.typedName
}

// SelectQueue selects the queue from the flow that has received the least
// service, as determined by the configured FairnessMetric.
func (p *MaxMinFairness) SelectQueue(band framework.PriorityBandAccessor) (framework.FlowQueueAccessor, error) {
	if band == nil {
		return nil, nil
	}
	var activeKeys []types.FlowKey
	band.IterateQueues(func(queue framework.FlowQueueAccessor) bool {
		if queue.Len() > 0 {
			activeKeys = append(activeKeys, queue.FlowKey())
		}
		return true
	})

	if len(activeKeys) == 0 {
		return nil, nil // No active queues to choose from.
	}
	if len(activeKeys) == 1 {
		// Fast path for the common case of only one active flow.
		return band.Queue(activeKeys[0].ID), nil
	}

	// Find the minimum value among all initialized (non-zero) flows.
	// This value will be used as the "lift" for any new/uninitialized flows.
	values := p.metric.GetValues(activeKeys)
	minInitializedVal := math.MaxFloat64
	hasInitializedFlows := false
	for _, val := range values {
		// A value is considered initialized if it's greater than zero.
		if val > 0 && val < minInitializedVal {
			minInitializedVal = val
			hasInitializedFlows = true
		}
	}
	if !hasInitializedFlows {
		// If no flows have received service yet, there is no minimum to lift to.
		// All flows are considered equal; select the first active one for a deterministic choice.
		return band.Queue(activeKeys[0].ID), nil
	}

	// Find the flow with the overall minimum effective value.
	var minKey types.FlowKey
	minEffectiveVal := math.MaxFloat64
	found := false
	for _, key := range activeKeys {
		val, tracked := values[key]

		// This is the "counter lift" logic from the VTC paper.
		// If a flow is untracked or has a zero value, its effective value for this scheduling decision is the minimum of
		// all other active flows.
		effectiveVal := val
		if !tracked || val == 0 {
			effectiveVal = minInitializedVal
		}

		if !found || effectiveVal < minEffectiveVal {
			minEffectiveVal = effectiveVal
			minKey = key
			found = true
		}
	}
	return band.Queue(minKey.ID), nil
}

var _ framework.InterFlowDispatchPolicy = &MaxMinFairness{}
var _ plugins.Plugin = &MaxMinFairness{}
