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

package config

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	interd "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/dispatch/interflow"
	intrad "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/dispatch/intraflow"
	interp "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/preemption/interflow"
	intrap "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/preemption/intraflow"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/queue"
)

func TestFlowControllerConfig_ValidateAndApplyDefaults(t *testing.T) {
	logger := logr.Discard()
	tests := []struct {
		name     string
		input    FlowControllerConfig
		expected FlowControllerConfig
		wantErr  bool
	}{
		{
			name: "all values provided and valid",
			input: FlowControllerConfig{
				DefaultQueueTTL:       10 * time.Second,
				ExpiryCleanupInterval: 500 * time.Millisecond,
				MaxGlobalBytes:        1024,
			},
			expected: FlowControllerConfig{
				DefaultQueueTTL:       10 * time.Second,
				ExpiryCleanupInterval: 500 * time.Millisecond,
				MaxGlobalBytes:        1024,
			},
			wantErr: false,
		},
		{
			name: "DefaultQueueTTL zero, should default",
			input: FlowControllerConfig{
				DefaultQueueTTL:       0,
				ExpiryCleanupInterval: 500 * time.Millisecond,
			},
			expected: FlowControllerConfig{
				DefaultQueueTTL:       DefaultFCQueueTTL,
				ExpiryCleanupInterval: 500 * time.Millisecond,
			},
			wantErr: false,
		},
		{
			name: "ExpiryCleanupInterval negative, should default",
			input: FlowControllerConfig{
				DefaultQueueTTL:       10 * time.Second,
				ExpiryCleanupInterval: -1 * time.Second,
			},
			expected: FlowControllerConfig{
				DefaultQueueTTL:       10 * time.Second,
				ExpiryCleanupInterval: DefaultFCExpiryCleanupInterval,
			},
			wantErr: false,
		},
		{
			name:  "empty config, all should default",
			input: FlowControllerConfig{},
			expected: FlowControllerConfig{
				DefaultQueueTTL:       DefaultFCQueueTTL,
				ExpiryCleanupInterval: DefaultFCExpiryCleanupInterval,
				MaxGlobalBytes:        0, // Default is 0
			},
			wantErr: false,
		},
		{
			name: "MaxGlobalBytes zero, remains zero",
			input: FlowControllerConfig{
				MaxGlobalBytes: 0,
			},
			expected: FlowControllerConfig{
				DefaultQueueTTL:       DefaultFCQueueTTL,
				ExpiryCleanupInterval: DefaultFCExpiryCleanupInterval,
				MaxGlobalBytes:        0,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.input // Make a copy
			err := cfg.ValidateAndApplyDefaults(logger)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, cfg)
			}
		})
	}
}

func TestFlowRegistryConfig_ValidateAndApplyDefaults(t *testing.T) {
	logger := logr.Discard()
	tests := []struct {
		name     string
		input    FlowRegistryConfig
		expected FlowRegistryConfig // For checking defaults on sub-configs
		wantErr  bool
		errText  string // Substring to check in error message
	}{
		{
			name:     "empty PriorityBands, valid",
			input:    FlowRegistryConfig{PriorityBands: []PriorityBandConfig{}},
			expected: FlowRegistryConfig{PriorityBands: []PriorityBandConfig{}},
			wantErr:  false,
		},
		{
			name: "one valid PriorityBandConfig",
			input: FlowRegistryConfig{
				PriorityBands: []PriorityBandConfig{
					{Priority: 0, PriorityName: "Critical", MaxBytes: 500},
				},
			},
			expected: FlowRegistryConfig{ // Expected after sub-config defaults
				PriorityBands: []PriorityBandConfig{
					{
						Priority:                  0,
						PriorityName:              "Critical",
						InterFlowDispatchPolicy:   interd.BestHeadPriorityScoreDispatchPolicyName,
						InterFlowPreemptionPolicy: interp.RoundRobinPreemptionPolicyName,
						IntraFlowDispatchPolicy:   intrad.FCFSDispatchPolicyName,
						IntraFlowPreemptionPolicy: intrap.TailPreemptionPolicyName,
						QueueType:                 queue.ListQueueName,
						MaxBytes:                  500,
					},
				},
			},
			wantErr: false,
		},
		{
			name: "multiple valid PriorityBandConfigs",
			input: FlowRegistryConfig{
				PriorityBands: []PriorityBandConfig{
					{Priority: 0, PriorityName: "Critical", MaxBytes: 500},
					{Priority: 1, PriorityName: "Standard", MaxBytes: 0}, // MaxBytes will default
				},
			},
			expected: FlowRegistryConfig{
				PriorityBands: []PriorityBandConfig{
					{
						Priority:                  0,
						PriorityName:              "Critical",
						InterFlowDispatchPolicy:   interd.BestHeadPriorityScoreDispatchPolicyName,
						InterFlowPreemptionPolicy: interp.RoundRobinPreemptionPolicyName,
						IntraFlowDispatchPolicy:   intrad.FCFSDispatchPolicyName,
						IntraFlowPreemptionPolicy: intrap.TailPreemptionPolicyName,
						QueueType:                 queue.ListQueueName,
						MaxBytes:                  500,
					},
					{
						Priority:                  1,
						PriorityName:              "Standard",
						InterFlowDispatchPolicy:   interd.BestHeadPriorityScoreDispatchPolicyName,
						InterFlowPreemptionPolicy: interp.RoundRobinPreemptionPolicyName,
						IntraFlowDispatchPolicy:   intrad.FCFSDispatchPolicyName,
						IntraFlowPreemptionPolicy: intrap.TailPreemptionPolicyName,
						QueueType:                 queue.ListQueueName,
						MaxBytes:                  DefaultPriorityBandMaxBytes, // Defaulted
					},
				},
			},
			wantErr: false,
		},
		{
			name: "one invalid PriorityBandConfig (missing PriorityName)",
			input: FlowRegistryConfig{
				PriorityBands: []PriorityBandConfig{
					{Priority: 0, PriorityName: ""}, // Invalid
				},
			},
			wantErr: true,
			errText: "PriorityName cannot be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.input // Make a copy
			err := cfg.ValidateAndApplyDefaults(logger)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errText != "" {
					assert.Contains(t, err.Error(), tt.errText)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, cfg)
			}
		})
	}
}

func TestPriorityBandConfig_ValidateAndApplyDefaults(t *testing.T) {
	logger := logr.Discard()

	tests := []struct {
		name     string
		input    PriorityBandConfig
		expected PriorityBandConfig
		wantErr  bool
		errText  string
	}{
		{
			name: "all values provided and valid",
			input: PriorityBandConfig{
				Priority:                  0,
				PriorityName:              "Critical",
				InterFlowDispatchPolicy:   "CustomInterDispatch",
				InterFlowPreemptionPolicy: "CustomInterPreempt",
				IntraFlowDispatchPolicy:   "CustomIntraDispatch",
				IntraFlowPreemptionPolicy: "CustomIntraPreempt",
				QueueType:                 "CustomQueue",
				MaxBytes:                  1024,
			},
			expected: PriorityBandConfig{
				Priority:                  0,
				PriorityName:              "Critical",
				InterFlowDispatchPolicy:   "CustomInterDispatch",
				InterFlowPreemptionPolicy: "CustomInterPreempt",
				IntraFlowDispatchPolicy:   "CustomIntraDispatch",
				IntraFlowPreemptionPolicy: "CustomIntraPreempt",
				QueueType:                 "CustomQueue",
				MaxBytes:                  1024,
			},
			wantErr: false,
		},
		{
			name: "empty policy/queue names, should default",
			input: PriorityBandConfig{
				Priority:     1,
				PriorityName: "Standard",
				MaxBytes:     512, // Valid MaxBytes
			},
			expected: PriorityBandConfig{
				Priority:                  1,
				PriorityName:              "Standard",
				InterFlowDispatchPolicy:   interd.BestHeadPriorityScoreDispatchPolicyName,
				InterFlowPreemptionPolicy: interp.RoundRobinPreemptionPolicyName,
				IntraFlowDispatchPolicy:   intrad.FCFSDispatchPolicyName,
				IntraFlowPreemptionPolicy: intrap.TailPreemptionPolicyName,
				QueueType:                 queue.ListQueueName,
				MaxBytes:                  512,
			},
			wantErr: false,
		},
		{
			name: "PriorityName empty, should return error",
			input: PriorityBandConfig{
				Priority:     0,
				PriorityName: "",
			},
			wantErr: true,
			errText: "PriorityName cannot be empty",
		},
		{
			name: "MaxBytes zero, should default",
			input: PriorityBandConfig{
				Priority:     2,
				PriorityName: "Sheddable",
				MaxBytes:     0,
			},
			expected: PriorityBandConfig{
				Priority:                  2,
				PriorityName:              "Sheddable",
				InterFlowDispatchPolicy:   interd.BestHeadPriorityScoreDispatchPolicyName,
				InterFlowPreemptionPolicy: interp.RoundRobinPreemptionPolicyName,
				IntraFlowDispatchPolicy:   intrad.FCFSDispatchPolicyName,
				IntraFlowPreemptionPolicy: intrap.TailPreemptionPolicyName,
				QueueType:                 queue.ListQueueName,
				MaxBytes:                  DefaultPriorityBandMaxBytes,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.input // Make a copy
			err := cfg.validateAndApplyDefaults(logger)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errText != "" {
					assert.Contains(t, err.Error(), tt.errText)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, cfg)
			}
		})
	}
}
