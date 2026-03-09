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

package contracts

import (
	"iter"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
)

// FlowRegistry is the complete interface for the global flow control plane.
// It composes all role-based interfaces. A concrete implementation of this interface is the single source of truth for
// all flow control state.
//
// # Conformance: Implementations MUST be goroutine-safe.
//
// # Flow Lifecycle
//
// A flow instance, identified by its immutable FlowKey, has a lease-based lifecycle managed by this interface.
// Any implementation MUST adhere to this lifecycle:
//
//  1. Lease Acquisition: A client calls Connect to acquire a lease. This signals that the flow is in use and protects
//     it from garbage collection. If the flow does not exist, it is created Just-In-Time (JIT).
//  2. Active State: A flow is "Active" as long as its lease count is greater than zero.
//  3. Lease Release: The client MUST call `Close()` on the returned `FlowConnection` to release the lease.
//     When the lease count drops to zero, the flow becomes "Idle".
//  4. Garbage Collection: The implementation MUST automatically garbage collect a flow after it has remained
//     continuously Idle for a configurable duration.
//
// # System Invariants
//
// Concrete implementations MUST uphold the following invariants:
//
//  1. Shard Consistency: All configured priority bands and registered flow instances must exist on every Active shard.
//  2. Capacity Partitioning: Global and per-band capacity limits must be uniformly partitioned across all Active
//     shards.
type FlowRegistry interface {
	FlowRegistryObserver
	FlowRegistryDataPlane
}

// FlowRegistryObserver defines the read-only, observation interface for the registry.
type FlowRegistryObserver interface {
	// ActiveShardIDs returns an iterator of identifiers for all shards currently considered active.
	// This is used by background processes (like the orchestrator) to discover which shards require processing loops.
	ActiveShardIDs() iter.Seq[string]

	// ActiveShards returns a current snapshot of accessors for all Active internal state shards.
	ActiveShards() iter.Seq[RegistryShard]
}

// FlowRegistryDataPlane defines the high-throughput, request-path interface for the registry.
type FlowRegistryDataPlane interface {
	// WithConnection manages a scoped, leased session for a given flow.
	// It is the primary and sole entry point for interacting with the data path.
	//
	// This method handles the entire lifecycle of a flow connection:
	// 1. Just-In-Time (JIT) Registration: If the flow for the given FlowKey does not exist, it is created and registered
	//    automatically.
	// 2. Lease Acquisition: It acquires a lifecycle lease, protecting the flow from garbage collection.
	// 3. Callback Execution: It invokes the provided function `fn`.
	// 4. Guaranteed Lease Release: It ensures the lease is safely released when the callback function returns.
	//
	// This functional, callback-based approach makes resource leaks impossible, as the caller is not responsible for
	// manually closing the connection.
	//
	// Errors returned by the callback `fn` are propagated up.
	// Returns `ErrFlowIDEmpty` if the provided key has an empty ID.
	WithConnection(key flowcontrol.FlowKey, fn func() error) error
}

// RegistryShard defines the interface for a single slice (shard) of the `FlowRegistry`'s state.
// A shard acts as an independent, parallel execution unit, allowing the system's dispatch logic to scale horizontally.
//
// # Conformance: Implementations MUST be goroutine-safe.
type RegistryShard interface {
	// ID returns a unique identifier for this shard, which must remain stable for the shard's lifetime.
	ID() string

	// IsActive returns true if the shard should accept new requests for enqueueing. A false value indicates the shard is
	// being gracefully drained and should not be given new work.
	IsActive() bool

	// ManagedQueue retrieves the managed queue for the given, unique FlowKey. This is the primary method for accessing
	// a specific flow's queue for either enqueueing or dispatching requests.
	//
	// Returns an error wrapping ErrPriorityBandNotFound if the priority specified in the key is not configured, or
	// ErrFlowInstanceNotFound if no instance exists for the given key.
	ManagedQueue(key flowcontrol.FlowKey) (ManagedQueue, error)

	// FairnessPolicy retrieves the FairnessPolicy singleton configured for the specified priority band on this shard.
	// This method provides access to the immutable logic component that governs inter-flow contention.
	// The registry guarantees that a non-nil policy is returned for any active priority band.
	//
	// Returns:
	//   - FairnessPolicy: The active policy instance.
	//   - error: A wrapped ErrPriorityBandNotFound if the priority level is not configured on this shard.
	FairnessPolicy(priority int) (flowcontrol.FairnessPolicy, error)

	// PriorityBandState retrieves the state and iterator required by a FairnessPolicy.
	//
	// Returns an error wrapping ErrPriorityBandNotFound if the priority level is not configured.
	PriorityBandState(priority int) (state any, queues iter.Seq[flowcontrol.FlowQueueAccessor], err error)

	// AllOrderedPriorityLevels returns all configured priority levels that this shard is aware of, sorted in descending
	// numerical order. This order corresponds to highest priority (highest numeric value) to lowest priority (lowest
	// numeric value).
	// The returned iterator provides a definitive, ordered list of priority levels for iteration, for example, by a
	// `controller.FlowController` worker's dispatch loop.
	AllOrderedPriorityLevels() iter.Seq[int]

	// HasCapacity checks if the shard has enough capacity to admit a new item of the specified size at the given
	// priority level.
	// This validates both the global shard limit and the per-band limit in a lock-free manner.
	HasCapacity(priority int, itemByteSize uint64) bool
}

// ManagedQueue defines the interface for a flow's queue on a specific shard.
// It acts as a stateful decorator that *use an underlying SafeQueue, augmenting it with statistics tracking, and
// lifecycle awareness (e.g., rejecting adds when a shard is draining).
//
// Conformance: Implementations MUST be goroutine-safe.
type ManagedQueue interface {
	// Add attempts to enqueue an item, performing an atomic check on the parent shard's lifecycle state before adding
	// the item to the underlying queue.
	// Returns ErrShardDraining if the parent shard is no longer Active.
	Add(item flowcontrol.QueueItemAccessor) error

	// Remove atomically finds and removes an item from the underlying queue using its handle.
	Remove(handle flowcontrol.QueueItemHandle) (flowcontrol.QueueItemAccessor, error)

	// Cleanup removes all items from the underlying queue that satisfy the predicate.
	Cleanup(predicate PredicateFunc) []flowcontrol.QueueItemAccessor

	// Drain removes all items from the underlying queue.
	Drain() []flowcontrol.QueueItemAccessor

	// FlowQueueAccessor returns a read-only, flow-aware accessor for this queue, used by policy plugins.
	// Conformance: This method MUST NOT return nil.
	FlowQueueAccessor() flowcontrol.FlowQueueAccessor
}
