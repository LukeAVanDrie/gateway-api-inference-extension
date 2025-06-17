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

// Package registry contains the concrete implementation of the FlowRegistry system, including its configuration structs
// and constructor. It implements the interfaces defined in the ports package.
package registry

import (
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins"
)

// FlowRegistryConfig holds the top-level configuration for a FlowRegistry instance.
type FlowRegistryConfig struct {
	// InitialShardCount specifies the number of parallel workers (shards) the FlowController should be initialized with.
	// This value must be at least 1 and within the bounds of MinShards and MaxShards.
	//
	// Optional: Defaults to 1 (a single-worker, non-sharded configuration).
	InitialShardCount uint

	// MinShards defines the minimum number of shards for dynamic scaling. The active shard count cannot be updated
	// below this value.
	//
	// Optional: Defaults to 1.
	MinShards uint

	// MaxShards defines the maximum number of shards for dynamic scaling. The active shard count cannot be updated
	// above this value. Setting MaxShards to 1 effectively disables scaling up.
	//
	// Optional: Defaults to 1 for safety, assuming a single-core host.
	MaxShards uint

	// MaxBytes defines an optional, global maximum total byte size limit aggregated across all priority bands and shards.
	// The FlowController enforces this limit in addition to per-band capacity limits.
	//
	// Optional: Defaults to 0, which signifies that the global limit is ignored.
	MaxBytes uint64

	// PriorityBands defines the set of priority bands managed by the FlowRegistry. The configuration for each band,
	// including its default policies and queue types, is specified here.
	//
	// Required: At least one PriorityBandConfig must be provided for a functional registry.
	PriorityBands []PriorityBandConfig
}

// PriorityBandConfig defines the configuration for a single priority band within the FlowRegistry.
type PriorityBandConfig struct {
	// Priority is the numerical priority level for this band.
	// Convention: Lower numerical values indicate higher priority (e.g., 0 is highest).
	//
	// Required.
	Priority uint

	// PriorityName is a human-readable name for this priority band (e.g., "Critical", "Standard", "Sheddable").
	//
	// Required.
	PriorityName string

	// InterFlowDispatchPolicy specifies the name of the registered policy used to select which flow's queue to service
	// next from this band.
	//
	// Optional: If empty, a system default (e.g., "BestHead") is used.
	InterFlowDispatchPolicy plugins.RegisteredInterFlowDispatchPolicyName

	// InterFlowDisplacementPolicy specifies the name of the registered policy used to select a victim flow's queue from
	// this band when displacement is triggered from a higher-priority band.
	//
	// Optional: If empty, a system default (e.g., "RoundRobinDispatch") is used.
	InterFlowDisplacementPolicy plugins.RegisteredInterFlowDisplacementPolicyName

	// IntraFlowDispatchPolicy specifies the default name of the registered policy used to select a specific request to
	// dispatch next from within a single flow's queue in this band. This default can be overridden on a per-flow basis.
	//
	// Optional: If empty, a system default (e.g., "FCFS") is used.
	IntraFlowDispatchPolicy plugins.RegisteredIntraFlowDispatchPolicyName

	// IntraFlowDisplacementPolicy specifies the default name of the registered policy used to select a victim item from
	// within a single flow's queue in this band when displacement is triggered. This default can be overridden on a
	// per-flow basis.
	//
	// Optional: If empty, a system default (e.g., "Tail") is used.
	IntraFlowDisplacementPolicy plugins.RegisteredIntraFlowDisplacementPolicyName

	// QueueType specifies the default name of the registered SafeQueue implementation to be used for flow queues within
	// this band.
	//
	// Optional: If empty, a system default (e.g., "ListQueue") is used.
	QueueType plugins.RegisteredQueueName

	// MaxBytes defines the maximum total byte size for this specific priority band, aggregated across all shards.
	//
	// Optional: If not set, a system default (e.g., 1 GB) is applied.
	MaxBytes uint64
}
