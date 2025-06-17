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
// Implementations are expected to perform complex updates (e.g., RegisterOrUpdateFlow, UpdateShardCount) atomically to
// preserve system invariants.
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
//
// # Shard Garbage Collection
//
// When a shard is decommissioned via UpdateShardCount, the FlowRegistry must ensure a graceful shutdown. It must mark
// the shard as inactive to prevent new enqueues, allow the FlowController to continue draining its queues, and only
// delete the shard's state after the associated worker has fully terminated and all queues are empty.
type FlowRegistry interface {
	FlowRegistryAdmin
	ShardProvider
}

// FlowRegistryAdmin defines the administrative interface for the global control plane. This interface is intended for
// external systems (like a Kubernetes operator) to configure flows, manage system parallelism, and query aggregated
// statistics for observability.
type FlowRegistryAdmin interface {
	// RegisterOrUpdateFlow handles the registration of a new flow or the update of an existing flow's specification.
	// This method orchestrates complex state transitions atomically across all managed shards.
	//
	// # Dynamic Update Behaviors
	//
	//   - Priority Changes: If a flow's priority level changes, its current active ManagedQueue instances are marked
	//     as inactive to drain existing requests. A new instance is activated at the new priority level. If a flow is
	//     updated to a priority level where an instance is already draining (e.g., during a rapid rollback), that
	//     draining instance is re-activated.
	//
	//   - Intra-Flow Policy Changes: If a flow's intra-flow policies change, a compatibility check is performed.
	//       - If the existing SafeQueue instances support the new policies' capabilities, the policies are swapped
	//         in-place. If the queue supports CapabilityDynamicPriority, it is signaled to re-sort its items.
	//       - If not compatible, a full "drain and re-enqueue" migration is performed: new compatible SafeQueue
	//         instances are created, and all items are atomically moved from the old queues to the new ones,
	//         preserving the original enqueue timestamp if the new queue is FIFO-based.
	//
	//   - Flow Capacity Changes: New per-flow capacity limits are applied immediately for subsequent enqueue checks.
	//     Items already in a queue that cause it to exceed the new limit are not preemptively removed.
	//
	// # Returns
	//
	//   - nil on success.
	//   - An error wrapping ErrFlowIDEmpty if spec.ID() is empty.
	//   - An error wrapping ErrPriorityBandNotFound if spec.Priority() refers to an unconfigured priority level.
	//   - Other errors if internal creation/activation of policy or queue instances fail.
	RegisterOrUpdateFlow(spec types.FlowSpecification) error

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
	//
	// # Dynamic Update Behaviors
	//
	//   - On Increase: New, empty state shards are initialized with all registered flows. The FlowController's request
	//     distribution logic will naturally balance load to these new shards over time.
	//   - On Decrease: A specified number of existing shards are marked as inactive. They stop accepting new requests
	//     but continue to drain existing items. They are fully removed only after their queues are empty.
	//
	// The implementation MUST atomically re-partition capacity allocations across all active shards when the count
	// changes.
	UpdateShardCount(n uint) error

	// Stats returns globally aggregated statistics for the entire FlowRegistry.
	Stats() AggregateStats

	// ShardStats returns a slice of statistics, one for each internal shard. This provides visibility for debugging and
	// monitoring per-shard behavior (e.g., identifying hot or stuck shards).
	ShardStats() []ShardStats
}

// ShardProvider defines a minimal interface for consumers that need to discover and iterate over available shards.
type ShardProvider interface {
	// Shards returns a slice of accessors, one for each internal state shard.
	//
	// A "shard" is an internal, parallel execution unit that allows the FlowController's core dispatch logic to be
	// parallelized, preventing a CPU bottleneck at high request rates. The FlowRegistry's state is sharded to support
	// this parallelism by reducing lock contention.
	//
	// The returned slice includes accessors for both active and draining shards. Consumers MUST use IsActive() to
	// determine if new work should be routed to a shard.
	Shards() []RegistryShard
}

// RegistryShard defines the read-oriented interface that a FlowController worker uses to access its specific slice of
// the FlowRegistry's state. It provides the necessary methods for a worker to perform its dispatch and displacement
// operations without exposing registry-wide configuration methods.
type RegistryShard interface {
	// Conformance: All methods MUST be goroutine-safe.

	// ID returns a unique identifier for this shard, which must remain stable for the shard's lifetime.
	ID() string

	// IsActive returns true if the shard should accept new requests for enqueueing. A false value indicates the shard is
	// being gracefully drained.
	IsActive() bool

	// ActiveManagedQueue returns the currently active ManagedQueue for a given flow on this shard. This is the queue to
	// which new requests for the flow should be enqueued.
	// Returns an error wrapping ErrFlowNotRegistered if no active instance exists.
	ActiveManagedQueue(flowID string) (ManagedQueue, error)

	// ManagedQueue retrieves a specific (potentially draining) ManagedQueue instance from this shard. This allows a
	// worker to continue dispatching items from queues that are draining.
	// Returns an error wrapping ErrFlowInstanceNotFound if no instance for the given flowID and priority exists.
	ManagedQueue(flowID string, priority uint) (ManagedQueue, error)

	// IntraFlowDispatchPolicy retrieves a flow's configured framework.IntraFlowDispatchPolicy for this shard.
	// The registry guarantees that a non-nil default policy (as configured at the priority-band level) is returned if
	// none is specified on the flow itself.
	// Returns an error wrapping ErrFlowInstanceNotFound if the flow instance does not exist.
	IntraFlowDispatchPolicy(flowID string, priority uint) (framework.IntraFlowDispatchPolicy, error)

	// IntraFlowDisplacementPolicy retrieves a flow's configured framework.IntraFlowDisplacementPolicy for this shard.
	// The registry guarantees that a non-nil default policy (as configured at the priority-band level) is returned if
	// none is specified on the flow itself.
	// Returns an error wrapping ErrFlowInstanceNotFound if the flow instance does not exist.
	IntraFlowDisplacementPolicy(flowID string, priority uint) (framework.IntraFlowDisplacementPolicy, error)

	// InterFlowDispatchPolicy retrieves a priority band's configured framework.InterFlowDispatchPolicy for this shard.
	// The registry guarantees that a non-nil default policy is returned if none is configured for the band.
	// Returns an error wrapping ErrPriorityBandNotFound if the priority level is not configured.
	InterFlowDispatchPolicy(priority uint) (framework.InterFlowDispatchPolicy, error)

	// InterFlowDisplacementPolicy retrieves a priority band's configured framework.InterFlowDisplacementPolicy for this
	// shard.
	// The registry guarantees that a non-nil default policy is returned if none is configured for the band.
	// Returns an error wrapping ErrPriorityBandNotFound if the priority level is not configured.
	InterFlowDisplacementPolicy(priority uint) (framework.InterFlowDisplacementPolicy, error)

	// PriorityBandAccessor retrieves a read-only accessor for a given priority level, providing a view of the band's
	// state as seen by this specific shard.
	PriorityBandAccessor(priority uint) (framework.PriorityBandAccessor, error)

	// Stats returns statistics for this specific shard.
	Stats() ShardStats
}

// ManagedQueue is the interface for a flow's queue on a specific shard. It wraps an underlying framework.SafeQueue,
// adding lifecycle validation against the FlowRegistry and integrating atomic statistics updates.
type ManagedQueue interface {
	// Conformance:
	//   - All methods (including those from the embedded framework.SafeQueue) MUST be goroutine-safe.
	//   - Mutating methods (Add, Remove, CleanupExpired, Drain) MUST ensure the flow instance still exists within the
	//     registry before proceeding and MUST atomically update statistics.
	//   - Returned newLen and newByteSize values from mutating methods MUST reflect the state *after* both the
	//     underlying queue operation and statistics reconciliation are complete.
	// 	 - Non-mutating methods may operate on a "zombie" instance (a handle to a queue whose corresponding flow instance
	//     has been fully garbage-collected from the registry). Consumers should be aware that data from such an instance
	//     might be stale.
	framework.SafeQueue

	// FlowQueueAccessor returns a read-only, flow-aware accessor for this queue, used by policies for inspection.
	// This method MUST NOT return nil.
	FlowQueueAccessor() framework.FlowQueueAccessor

	// FlowSpec returns the specification of the flow this queue is associated with.
	//
	// Warning: Capacity limits defined within the FlowSpecification reflect the globally configured limits for the
	// logical flow. They DO NOT reflect the per-shard partitioned capacity limits that this ManagedQueue must adhere to.
	// The partitioned capacity for a queue should be determined from its corresponding framework.FlowQueueAccessor and
	// framework.PriorityBandAccessor.
	//
	// This method MUST NOT return nil.
	FlowSpec() types.FlowSpecification
}

// AggregateStats holds summary statistics for the global scope (across all shards).
type AggregateStats struct {
	// Total byte size of all items across all queues in all shards.
	ByteSize uint64
	// Total number of items across all queues in all shards.
	Len uint64
	// Per-priority-band statistics, aggregated across all shards. Keyed by priority level.
	PerPriorityBandStats map[uint]PriorityBandStats
}

// ShardStats holds statistics for a single internal shard.
type ShardStats struct {
	ShardID string
	// Total byte size of all items across all queues in this shard.
	TotalByteSize uint64
	// Total number of items across all queues in this shard.
	TotalLen uint64
	// Per-priority-band statistics for this shard. Keyed by priority level.
	PerPriorityBandStats map[uint]PriorityBandStats
}

// PriorityBandStats holds aggregated statistics for a single priority band within a given scope.
type PriorityBandStats struct {
	Priority     uint
	PriorityName string
	// Total byte size of items in this band within the scope.
	ByteSize uint64
	// Total number of items in this band within the scope.
	Len uint64
}
