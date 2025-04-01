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

// Package types defines the core interfaces, data structures, behavioral contracts, and standard error/outcome types
// for the FlowController system.
package types

// FlowSpecification defines the properties of a logical flow relevant for flow control, primarily its identity and
// registered priority.
// Instances are typically managed by the FlowRegistry.

type FlowSpecification interface {
	// ID returns the unique name or identifier for this flow (e.g., model name, tenant ID).
	// This corresponds to the value from FlowControlRequest.FlowID().
	ID() string
	// Priority returns the numerical priority level currently associated with this flow within the FlowRegistry.
	// Convention: Lower numerical values indicate higher priority.
	Priority() uint
}

// FlowRegistry defines the interface for managing the lifecycle of flows, their associated ManagedQueues, policies, and
// aggregated statistics.
// It acts as the central control plane for flow definitions and provides the FlowController with access to the
// necessary components for request processing.
//
// Conformance:
//   - All methods defined in this interface MUST be goroutine-safe, as the FlowRegistry may be accessed concurrently by
//     the FlowController's dispatch/enqueue loops and by external configuration mechanisms.
type FlowRegistry interface {
	// RegisterOrUpdateFlow handles the registration of a new flow or the update of an existing flow's specification
	// (e.g., a change in its priority).
	//
	// If a flow's priority changes, its current active ManagedQueue instance is marked as inactive to drain existing
	// requests, and a new ManagedQueue instance is activated (or created) at the new priority level. Subsequent requests
	// for the flow are directed to this new active instance.
	// The old, inactive instance is cleaned up by the registry once its queue is empty (typically signaled internally by
	// its ManagedQueue wrapper when it empties).
	//
	// Returns:
	//   - nil on successful registration or update.
	//   - An error wrapping types.ErrFlowIDEmpty if spec.ID() is empty.
	//   - An error wrapping types.ErrInvalidFlowPriority if spec.Priority() refers to a priority level not configured in
	//     the registry.
	//   - Other errors if internal creation/activation of policies or queues fails (e.g., due to plugin factory errors or
	//     capability mismatches), in which case the registration or update will not complete.
	RegisterOrUpdateFlow(spec FlowSpecification) error
	// UnregisterFlow marks a flow as inactive across all its instances.
	// Its associated resources (ManagedQueues and underlying SafeQueues) will be cleaned up by the registry once they are
	// empty. New requests for this flow will be rejected by the FlowController (as ActiveManagedQueue will fail).
	//
	// Returns:
	//   - nil on successful marking for unregistration.
	//   - An error wrapping types.ErrFlowIDEmpty if flowID is empty.
	//   - An error wrapping types.ErrFlowNotRegistered if the flowID is not found or was already fully
	//     unregistered/cleaned up.
	UnregisterFlow(flowID string) error
	// ActiveManagedQueue returns the currently active ManagedQueue for the given flowID.
	// This is the queue to which new requests for this flow should be enqueued by the FlowController. The returned
	// ManagedQueue provides concurrency-safe operations that are validated against the flow's lifecycle in the registry
	// and integrate with statistics updates.
	//
	// Returns:
	//   - (ManagedQueue, nil) on success.
	//   - (nil, error wrapping types.ErrFlowNotRegistered) if no active instance for flowID is found (e.g., flow not
	//     registered or currently inactive/draining).
	ActiveManagedQueue(flowID string) (ManagedQueue, error)
	// ManagedQueue retrieves a specific flow instance's ManagedQueue, by flowID and priority. This is critical for
	// accessing queues that are inactive or draining (e.g., after a flow's priority changes or it's unregistered) to
	// allow the FlowController to continue dispatching their remaining items.
	//
	// Returns:
	//   - (ManagedQueue, nil) on success.
	//   - (nil, error wrapping types.ErrFlowInstanceNotFound) if no instance for the given flowID and priority exists.
	ManagedQueue(flowID string, priority uint) (ManagedQueue, error)
	// IntraFlowDispatchPolicy retrieves a specific flow instance's configured IntraFlowDispatchPolicy. The registry
	// guarantees a policy is always returned (defaulting if necessary) if the flow instance itself exists.
	//
	// Returns:
	//   - (IntraFlowDispatchPolicy, nil) on success.
	//   - (nil, error wrapping types.ErrFlowInstanceNotFound) if no instance for the given flowID and priority exists.
	IntraFlowDispatchPolicy(flowID string, priority uint) (IntraFlowDispatchPolicy, error)
	// IntraFlowPreemptionPolicy retrieves a specific flow instance's configured IntraFlowPreemptionPolicy. The registry
	// guarantees a policy is always returned (defaulting if necessary) if the flow instance itself exists.
	//
	// Returns:
	//   - (IntraFlowPreemptionPolicy, nil) on success.
	//   - (nil, error wrapping types.ErrFlowInstanceNotFound) if no instance for the given flowID and priority exists.
	IntraFlowPreemptionPolicy(flowID string, priority uint) (IntraFlowPreemptionPolicy, error)
	// InterFlowDispatchPolicy retrieves a priority band's configured InterFlowDispatchPolicy. The registry guarantees a
	// policy is always returned (defaulting if necessary) if the priority band itself exists.
	//
	// Returns:
	//   - (InterFlowDispatchPolicy, nil) on success.
	//   - (nil, error wrapping types.ErrPriorityBandNotFound) if the specified priority level is not a configured band.
	InterFlowDispatchPolicy(priority uint) (InterFlowDispatchPolicy, error)
	// InterFlowPreemptionPolicy retrieves a priority band's configured InterFlowPreemptionPolicy. The registry guarantees
	// a policy is always returned (defaulting if necessary) if the priority band itself exists.
	//
	// Returns:
	//   - (InterFlowPreemptionPolicy, nil) on success.
	//   - (nil, error wrapping types.ErrPriorityBandNotFound) if the specified priority level is not a configured band.
	InterFlowPreemptionPolicy(priority uint) (InterFlowPreemptionPolicy, error)
	// PriorityBandAccessor retrieves a PriorityBandAccessor for a given priority level, allowing inter-flow policies to
	// inspect the state of that band.
	//
	// Returns:
	//   - (PriorityBandAccessor, nil) on success.
	//   - (nil, error wrapping types.ErrPriorityBandNotFound) if the specified priority level is not a configured band.
	PriorityBandAccessor(priority uint) (PriorityBandAccessor, error)
	// AllOrderedPriorityLevels returns all configured priority levels, sorted from highest to lowest priority.
	// Convention: Lower numerical value means higher priority (e.g., 0 is highest).
	// The returned slice is sorted in ascending numerical order.
	AllOrderedPriorityLevels() []uint
	// GetStats returns aggregated statistics for the FlowRegistry, including global and per-priority-band metrics for
	// queue lengths and byte sizes.
	// These statistics are updated atomically by ManagedQueue operations.
	GetStats() FlowRegistryStats
}

// FlowRegistryStats holds aggregated statistics for the entire FlowRegistry.
type FlowRegistryStats struct {
	GlobalByteSize       uint64
	GlobalLen            uint64
	PerPriorityBandStats map[uint]PriorityBandStats // Keyed by priority level
}

// PriorityBandStats holds aggregated statistics for a single priority band.
type PriorityBandStats struct {
	PriorityLevel uint
	PriorityName  string
	ByteSize      uint64 // Total byte size of items in this band.
	Len           uint64 // Total number of items in this band.
}

// PriorityBandAccessor provides a read-only view into a specific priority band within the FlowRegistry. It allows
// inter-flow policies to inspect the state of all flow queues within that band.
//
// Conformance:
//   - All methods MUST be goroutine-safe for concurrent access.
type PriorityBandAccessor interface {
	// Priority returns the numerical priority level of this band.
	Priority() uint
	// PriorityName returns the human-readable name of this priority band.
	PriorityName() string
	// CapacityBytes returns the configured maximum total byte size for this priority band. The FlowController uses this
	// limit in its capacity checking logic. A value of 0 might indicate no specific byte limit for this band (beyond
	// global limits or other constraints).
	CapacityBytes() uint64
	// FlowIDs returns a slice of all flow IDs currently active or draining within this priority band. The order is not
	// guaranteed unless specified by the implementation (e.g., for deterministic testing).
	FlowIDs() []string
	// Queue returns a FlowQueueAccessor for the specified flowID within this band.
	// Conformance:
	//   - Returns nil if the flowID is not found in this band.
	Queue(flowID string) FlowQueueAccessor
	// IterateQueues executes the given callback for each FlowQueueAccessor in his priority band. Iteration stops if the
	// callback returns false.
	// The order of iteration is not guaranteed unless specified by the implementation.
	IterateQueues(callback func(queue FlowQueueAccessor) (keepIterating bool))
}
