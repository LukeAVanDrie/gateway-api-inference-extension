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

// Package plugins defines the catalog of registered names for all built-in `Policy` and `SafeQueue` implementations.
// These names are used in the `registry.FlowRegistryConfig` to select and configure the desired plugins.
package plugins

// RegisteredQueueName is a type alias for the string names of registered SafeQueue implementations.
type RegisteredQueueName string

const (
	// ListQueue is a simple, double-ended queue implementation.
	ListQueue RegisteredQueueName = "ListQueue"
)

// RegisteredInterFlowDispatchPolicyName is a type alias for the string names of registered InterFlowDispatchPolicy
// implementations.
type RegisteredInterFlowDispatchPolicyName string

const (
	// BestHead selects the flow queue whose head item has the highest priority.
	BestHead RegisteredInterFlowDispatchPolicyName = "BestHead"
	// RoundRobinDispatch selects flow queues in a simple round-robin order.
	RoundRobinDispatch RegisteredInterFlowDispatchPolicyName = "RoundRobinDispatch"
)

// RegisteredInterFlowDisplacementPolicyName is a type alias for the string names of registered
// InterFlowDisplacementPolicy implementations.
type RegisteredInterFlowDisplacementPolicyName string

const (
	// WorstTail selects the flow queue whose tail item has the lowest priority.
	WorstTail RegisteredInterFlowDisplacementPolicyName = "WorstTail"
	// RoundRobinDisplacement selects a victim flow queue in a simple round-robin order.
	RoundRobinDisplacement RegisteredInterFlowDisplacementPolicyName = "RoundRobinDisplacement"
)

// RegisteredIntraFlowDispatchPolicyName is a type alias for the string names of registered IntraFlowDispatchPolicy
// implementations.
type RegisteredIntraFlowDispatchPolicyName string

const (
	// FCFS implements "First-Come, First-Served" ordering.
	FCFS RegisteredIntraFlowDispatchPolicyName = "FCFS"
)

// RegisteredIntraFlowDisplacementPolicyName is a type alias for the string names of registered
// IntraFlowDisplacementPolicy implementations.
type RegisteredIntraFlowDisplacementPolicyName string

const (
	// Tail selects the item at the tail of the queue as the victim.
	Tail RegisteredIntraFlowDisplacementPolicyName = "Tail"
)
