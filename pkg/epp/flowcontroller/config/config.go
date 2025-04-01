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

// Package config defines configuration structures with defaulting and validation logic for the Flow Controller and the
// Flow Registry.
package config

import (
	"fmt"
	"time"

	"github.com/go-logr/logr"
	interd "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/dispatch/interflow"
	intrad "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/dispatch/intraflow"
	interp "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/preemption/interflow"
	intrap "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/preemption/intraflow"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/queue"
)

// Default values for FlowControllerConfig
const (
	// DefaultFCQueueTTL is the default Time-To-Live for requests in the FlowController.
	// Used if a request's InitialEffectiveTTL() is zero or if overridden by controller policy.
	DefaultFCQueueTTL = 30 * time.Second
	// DefaultFCExpiryCleanupInterval is the default frequency for the FlowController's background routine to check for
	// and remove expired items.
	DefaultFCExpiryCleanupInterval = 1 * time.Second
)

// Default values for PriorityBandConfig
const (
	// DefaultPriorityBandMaxBytes is the default maximum byte capacity for a priority band if not explicitly configured.
	// Set to a generous value suitable for LLM serving.
	DefaultPriorityBandMaxBytes uint64 = 1 * 1024 * 1024 * 1024 // 1 GB
)

// FlowControllerConfig groups configuration parameters for the FlowController engine.
// Note: Default values are applied by the FlowController during its initialization if specific fields are not set or
// are invalid.
type FlowControllerConfig struct {
	// DefaultQueueTTL is the default Time-To-Live applied to requests within queues if not otherwise specified by the
	// incoming request's InitialEffectiveTTL() or overridden by more specific configurations.
	// Optional: If not set or set to a non-positive value, a system default (e.g., 30 seconds) will be used.
	// Example: "30s".
	DefaultQueueTTL time.Duration
	// ExpiryCleanupInterval is the frequency at which the FlowController's background routine checks for and removes
	// expired items from all managed queues.
	// Optional: If not set or set to a non-positive value, a system default (e.g., 1 second) will be used.
	// Example: "1s".
	ExpiryCleanupInterval time.Duration
	// MaxGlobalBytes defines an optional overall limit on the total byte size of requests across all queues in all
	// priority bands. If set to a positive value, this is enforced by the FlowController's capacity checking logic in
	// addition to per-priority limits (which are sourced from the FlowRegistry).
	// Optional: A value of 0 means no global byte limit is enforced by the FlowController.
	// Defaults to 0.
	MaxGlobalBytes uint64
	// TODO: Consider adding MaxFlowBytes (per-flow capacity limit within a priority band) as a future enhancement for
	// finer-grained fairness and resource isolation. This would likely involve changes to FlowSpecification or a new
	// per-flow policy, and the FlowController's capacity checks.
}

// ValidateAndApplyDefaults validates the FlowControllerConfig and applies default values if necessary.
func (fcc *FlowControllerConfig) ValidateAndApplyDefaults(logger logr.Logger) error {
	if fcc.DefaultQueueTTL <= 0 {
		logger.V(1).Info("FlowControllerConfig.DefaultQueueTTL is not set or invalid, using default.",
			"default", DefaultFCQueueTTL)
		fcc.DefaultQueueTTL = DefaultFCQueueTTL
	}
	if fcc.ExpiryCleanupInterval <= 0 {
		logger.V(1).Info("FlowControllerConfig.ExpiryCleanupInterval is not set or invalid, using default.",
			"default", DefaultFCExpiryCleanupInterval)
		fcc.ExpiryCleanupInterval = DefaultFCExpiryCleanupInterval
	}
	// MaxGlobalBytes can be 0 (meaning no global limit), so no default needed if 0.
	return nil
}

// FlowRegistryConfig holds the configuration for the FlowRegistry, primarily defining the priority bands.
// Note: Default values for sub-configurations (like PriorityBandConfig) are applied by the FlowRegistry during its
// initialization if specific fields are not set or are invalid.
type FlowRegistryConfig struct {
	// PriorityBands defines the set of priority bands managed by the FlowRegistry.
	// Required: At least one PriorityBandConfig should typically be provided for a functional registry.
	PriorityBands []PriorityBandConfig
}

// validateAndApplyDefaults validates the FlowRegistryConfig by validating each of its PriorityBandConfigs.
func (frc *FlowRegistryConfig) ValidateAndApplyDefaults(logger logr.Logger) error {
	for i := range frc.PriorityBands {
		if err := frc.PriorityBands[i].validateAndApplyDefaults(logger); err != nil {
			return fmt.Errorf("invalid config for priority band (priority %d, name %s): %w",
				frc.PriorityBands[i].Priority, frc.PriorityBands[i].PriorityName, err)
		}
	}
	return nil
}

// PriorityBandConfig defines the configuration for a single priority band within a FlowRegistry.
// Note: Default values are applied by the FlowRegistry during its initialization if specific fields are not set or are
// invalid.
type PriorityBandConfig struct {
	// Priority is the numerical priority level for this band.
	// Convention: Lower numerical values indicate higher priority (e.g., 0 is highest).
	// Required.
	Priority uint
	// PriorityName is a human-readable name for this priority band (e.g., "Critical", "Standard". "Sheddable").
	// Required.
	PriorityName string
	// InterFlowDispatchPolicy specifies the name of the registered policy used to select which flow's queue to service
	// next from this band.
	// Optional: If empty, a system default (e.g., "BestHeadPriorityScore") will be used.
	InterFlowDispatchPolicy interd.RegisteredInterFlowDispatchPolicyName
	// InterFlowPreemptionPolicy specifies the name of the registered policy used to select a victim flow's queue from
	// this band if preemption is triggered from a higher priority band targeting this one.
	// Optional: If empty, a system default (e.g., "RoundRobin") will be used.
	InterFlowPreemptionPolicy interp.RegisteredInterFlowPreemptionPolicyName
	// IntraFlowDispatchPolicy specifies the name of the registered policy used to select a specific request to dispatch
	// next from within a single flow's queue in this band.
	// Optional: If empty, a system default (e.g., "FCFS") will be used.
	IntraFlowDispatchPolicy intrad.RegisteredIntraFlowDispatchPolicyName
	// IntraFlowPreemptionPolicy specifies the name of the registered policy used to select a victim item from within a
	// specific flow's queue in this band if preemption is triggered.
	// Optional: If empty, a system default (e.g., "Tail") will be used.
	IntraFlowPreemptionPolicy intrap.RegisteredIntraFlowPreemptionPolicyName
	// QueueType specifies the name of the registered SafeQueue implementation to be used for flow queues within this
	// band.
	// Optional: If empty, a system default (e.g., "ListQueue") will be used.
	QueueType queue.RegisteredQueueName
	// MaxBytes defines the maximum total byte size for this specific priority band. The FlowController uses this limit
	// in its capacity checking logic.
	// Optional: If not set or set to a non-positive value, a system default (e.g., 1 GB) will be used.
	MaxBytes uint64
}

// validateAndApplyDefaults validates and applies defaults for a single PriorityBandConfig.
func (pbc *PriorityBandConfig) validateAndApplyDefaults(logger logr.Logger) error {
	if pbc.PriorityName == "" {
		return fmt.Errorf("PriorityName cannot be empty for priority level %d", pbc.Priority)
	}
	bandLogger := logger.WithValues("priority", pbc.Priority, "priorityName", pbc.PriorityName)

	if pbc.InterFlowDispatchPolicy == "" {
		bandLogger.V(1).Info("InterFlowDispatchPolicy is empty, using default", "defaultPolicy",
			interd.BestHeadPriorityScoreDispatchPolicyName)
		pbc.InterFlowDispatchPolicy = interd.BestHeadPriorityScoreDispatchPolicyName
	}
	if pbc.InterFlowPreemptionPolicy == "" {
		bandLogger.V(1).Info("InterFlowPreemptionPolicy is empty, using default", "defaultPolicy",
			interp.RoundRobinPreemptionPolicyName)
		pbc.InterFlowPreemptionPolicy = interp.RoundRobinPreemptionPolicyName
	}
	if pbc.IntraFlowDispatchPolicy == "" {
		bandLogger.V(1).Info("IntraFlowDispatchPolicy is empty, using default", "defaultPolicy",
			intrad.FCFSDispatchPolicyName)
		pbc.IntraFlowDispatchPolicy = intrad.FCFSDispatchPolicyName
	}
	if pbc.IntraFlowPreemptionPolicy == "" {
		bandLogger.V(1).Info("IntraFlowPreemptionPolicy is empty, using default", "defaultPolicy",
			intrap.TailPreemptionPolicyName)
		pbc.IntraFlowPreemptionPolicy = intrap.TailPreemptionPolicyName
	}
	if pbc.QueueType == "" {
		bandLogger.V(1).Info("QueueType is empty, using default", "defaultQueue", queue.ListQueueName)
		pbc.QueueType = queue.ListQueueName
	}
	if pbc.MaxBytes <= 0 {
		bandLogger.V(1).Info("PriorityBandConfig.MaxBytes is not set or invalid, using default",
			"default", DefaultPriorityBandMaxBytes)
		pbc.MaxBytes = DefaultPriorityBandMaxBytes
	}
	return nil
}
