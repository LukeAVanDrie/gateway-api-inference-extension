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

// Package ports defines the service interfaces used by the core `controller.FlowController` engine to interact with its
// primary dependencies. In alignment with a "Ports and Adapters" architectural style, these interfaces represent the
// "ports". They decouple the engine's operational logic from the concrete implementations of its two main services: the
// `FlowRegistry` system (for state management) and the `SaturationDetector` (for system load awareness).
package ports

import (
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
)

// FlowRegistry is the complete interface for the global control plane, composed of administrative functions and the
// ability to provide shard accessors. A concrete implementation of this interface is the single source of truth for all
// flow control state and configuration.
//
// # Conformance
//
// All methods defined in this interface (including those embedded) MUST be goroutine-safe.
// Implementations are expected to perform complex updates atomically to preserve system invariants.
//
// # Invariants
//
// Concrete implementations of FlowRegistry MUST uphold the following invariants across all operations:
//  1. Shard Consistency: All configured priority bands and logical flows must be represented on every active internal
//     shard. Plugin instance types (e.g., the specific SafeQueue implementation or policy plugins) must be consistent
//     for a given flow or band across all shards.
//  2. Flow Instance Uniqueness per Band: For any given logical flow, there can be a maximum of one ManagedQueue
//     instance per priority band. An instance can be either 'active' or 'draining'.
//  3. Single Active Instance per Flow: For any given logical flow, there can be a maximum of one *active* ManagedQueue
//     instance across all priority bands. All other instances for that flow must be in a 'draining' state.
//  4. Capacity Partitioning Consistency: Global and per-band capacity limits are uniformly partitioned across all
//     active shards. The sum of the capacity limits allocated to each shard must not exceed the globally configured
//     limits.
//
// # Flow Lifecycle States
//
//   - Registered: A logical flow is 'registered' when it is known to the FlowRegistry. It has exactly one 'active'
//     instance across all priority bands and zero or more 'draining' instances.
//   - Active: A specific instance of a flow within a priority band is 'active' if it is the designated target for all
//     new enqueues for that logical flow.
//   - Draining: A flow instance is 'draining' if it no longer accepts new enqueues but still contains items that are
//     eligible for dispatch. This occurs after a priority change or when a flow is unregistered.
//   - Unregistered: A logical flow is 'unregistered' after a call to UnregisterFlow. It has no 'active' instances,
//     though 'draining' instances may still exist until their queues are empty.
type FlowRegistry interface {
	FlowRegistryAdmin
	ShardProvider
}

// FlowRegistryAdmin defines the administrative interface for the global control plane. This interface is intended for
// external systems (like a Kubernetes operator) to configure flows, manage system parallelism, and query aggregated
// statistics for observability.
type FlowRegistryAdmin interface {
	// RegisterFlow handles the registration of a new flow.
	//
	// # Returns
	//
	//   - nil on success.
	//   - An error wrapping types.ErrFlowIDEmpty if spec.ID() is empty.
	//   - An error wrapping ErrPriorityBandNotFound if spec.Priority() refers to an unconfigured priority level.
	//   - Other errors if internal creation/activation of policy or queue instances fail.
	RegisterFlow(spec types.FlowSpecification) error

	// UnregisterFlow marks a flow as inactive across all shards. This action marks all active ManagedQueue instances for
	// the given flowID as inactive, allowing them to drain gracefully. New requests for this flow will be rejected.
	// The FlowRegistry is responsible for the eventual garbage collection of the flow's resources once all its per-shard
	// queue instances are empty.
	//
	// # Returns
	//
	//   - nil on success.
	//   - An error wrapping ErrFlowNotRegistered if the flowID is not found.
	UnregisterFlow(flowID string) error

	// UpdateShardCount dynamically adjusts the number of internal state shards, triggering a state rebalance.
	// UpdateShardCount(n uint) error // MVP: Not supported, assume fixed shard count (1).

	// Stats returns globally aggregated statistics for the entire FlowRegistry.
	Stats() AggregateStats
}

// ShardProvider defines a minimal interface for consumers that need to discover and iterate over available shards.
type ShardProvider interface {
	// Shards returns a slice of accessors, one for each internal state shard.
	//
	// A "shard" is an internal, parallel execution unit that allows the FlowController's core dispatch logic to be
	// parallelized, preventing a CPU bottleneck at high request rates. The FlowRegistry's state is sharded to support
	// this parallelism by reducing lock contention.
	//
	// The returned slice includes accessors for both active and draining shards. Consumers MUST determine if new work
	// should be routed to a shard.
	Shards() []RegistryShard
}

// AggregateStats holds globally aggregated statistics for the entire FlowRegistry.
type AggregateStats struct {
	// TotalCapacityBytes is the optional, maximum total byte size limit aggregated across all priority bands and shards.
	TotalCapacityBytes uint64
	// TotalByteSize is the total byte size of all items currently queued across all priority bands and shards.
	TotalByteSize uint64
	// TotalLen is the total number of items currently queued across all priority bands and shards.
	TotalLen uint64
	// PerPriorityBandStats maps each configured priority level to its aggregated statistics across all shards.
	// The key is the numerical priority level.
	PerPriorityBandStats map[uint]PriorityBandStats
}

// RegistryShard defines the read-oriented interface that a `controller.FlowController` worker uses to access its
// specific slice (shard) of the `FlowRegistry`'s state.
// It provides the necessary methods for a worker to perform its dispatch operations.
//
// Conformance: All methods MUST be goroutine-safe.
type RegistryShard interface {
	// ManagedQueue returns the `ManagedQueue` instance for a given `flowID` on this shard.
	//
	// Returns:
	//   - `ManagedQueue`: The queue instance.
	//   - error: An error wrapping `ErrFlowInstanceNotFound` if the specified flow instance does not exist on this shard.
	ManagedQueue(flowID string) (ManagedQueue, error)

	// IntraFlowDispatchPolicy retrieves a flow's configured `framework.IntraFlowDispatchPolicy` for a given `flowID` on
	// this shard.
	// The registry guarantees that a non-nil default policy is returned(as configured at the priority-band level) is
	// returned if none is specified on the flow itself.
	//
	// Returns:
	//   - `framework.IntraFlowDispatchPolicy`: The applicable dispatch policy.
	//   - error: An error wrapping `ErrFlowInstanceNotFound` if the specified flow instance does not exist on this shard.
	IntraFlowDispatchPolicy(flowID string) (framework.IntraFlowDispatchPolicy, error)

	// InterFlowDispatchPolicy retrieves a priority band's configured `framework.InterFlowDispatchPolicy` for this shard.
	// The registry guarantees that a non-nil default policy is returned if none is explicitly configured for the band.
	//
	// Returns:
	//   - `framework.InterFlowDispatchPolicy`: The applicable dispatch policy for the priority band.
	//   - error: An error wrapping `ErrPriorityBandNotFound` if the specified priority level is not configured.
	InterFlowDispatchPolicy(priority uint) (framework.InterFlowDispatchPolicy, error)

	// PriorityBandAccessor retrieves a read-only accessor for a given priority level, providing a view of that band's
	// state and configuration as seen by this specific shard. This is used by inter-flow policies.
	//
	// Returns:
	//   - `framework.PriorityBandAccessor`: An accessor for the priority band.
	//   - error: An error wrapping `ErrPriorityBandNotFound` if the specified priority level is not configured.
	PriorityBandAccessor(priority uint) (framework.PriorityBandAccessor, error)

	// AllOrderedPriorityLevels returns all configured priority levels that this shard is aware of, sorted in ascending
	// numerical order. This order corresponds to highest priority (lowest numeric value) to lowest priority (highest
	// numeric value).
	// The returned slice provides a definitive, ordered list of priority levels for iteration, for example, by a
	// `controller.FlowController` worker's dispatch loop.
	AllOrderedPriorityLevels() []uint

	// Stats returns statistics specific to this shard's activity and queued items.
	Stats() ShardStats
}

// ManagedQueue defines the interface for a flow's queue instance on a specific shard.
// It wraps an underlying `framework.SafeQueue`, augmenting it with lifecycle validation against the `FlowRegistry` and
// integrating atomic statistics updates.
//
// Conformance:
//   - All methods (including those embedded from `framework.SafeQueue`) MUST be goroutine-safe.
//   - Mutating methods (`Add()`, `Remove()`, `CleanupExpired()`, `Drain()`) MUST ensure the flow instance still exists
//     and is valid within the `FlowRegistry` before proceeding. They MUST also atomically update relevant statistics
//     (e.g., queue length, byte size) at both the queue and priority-band levels.
//   - Returned `newLen` and `newByteSize` values from mutating methods MUST reflect the state after both the underlying
//     queue operation and statistics reconciliation are complete.
type ManagedQueue interface {
	framework.SafeQueue

	// FlowQueueAccessor returns a read-only, flow-aware accessor for this queue.
	// This accessor is primarily used by policy plugins to inspect the queue's state in a structured way.
	//
	// Conformance: This method MUST NOT return nil.
	FlowQueueAccessor() framework.FlowQueueAccessor
}

// ShardStats holds statistics for a single internal shard within the `FlowRegistry`.
type ShardStats struct {
	// TotalCapacityBytes is the optional, maximum total byte size limit aggregated across all priority bands within this
	// shard. Its value represents the globally configured limit for the `FlowRegistry` partitioned for this shard.
	// The `controller.FlowController` enforces this limit in addition to any per-band capacity limits.
	// A value of 0 signifies that this global limit is ignored, and only per-band limits apply.
	TotalCapacityBytes uint64
	// TotalByteSize is the total byte size of all items currently queued across all priority bands within this shard.
	TotalByteSize uint64
	// TotalLen is the total number of items currently queued across all priority bands within this shard.
	TotalLen uint64
	// PerPriorityBandStats maps each configured priority level to its statistics within this shard.
	// The key is the numerical priority level.
	// All configured priority levels are guaranteed to be represented.
	PerPriorityBandStats map[uint]PriorityBandStats
}

// PriorityBandStats holds aggregated statistics for a single priority band.
type PriorityBandStats struct {
	// Priority is the numerical priority level this struct describes.
	Priority uint
	// PriorityName is an optional, human-readable name for the priority level (e.g., "Critical", "Sheddable").
	PriorityName string
	// CapacityBytes is the configured maximum total byte size for this priority band, aggregated across all items in
	// all flow queues within this band. If scoped to a shard, its value represents the configured band limit for the
	// `FlowRegistry` partitioned for this shard.
	// The `controller.FlowController` enforces this limit.
	// A default non-zero value is guaranteed if not configured.
	CapacityBytes uint64
	// ByteSize is the total byte size of items currently queued in this priority band.
	ByteSize uint64
	// Len is the total number of items currently queued in this priority band.
	Len uint64
}
