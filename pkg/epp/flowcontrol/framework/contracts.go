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

package framework

import "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"

// PriorityBandAccessor provides a read-only view into a specific priority band within the Flow Registry. It allows the
// Flow Controller and inter-flow policies to inspect the state of all flow queues within that band.
//
// Conformance: All methods MUST be goroutine-safe for concurrent access.
type PriorityBandAccessor interface {
	// Priority returns the numerical priority level of this band.
	Priority() uint

	// PriorityName returns the human-readable name of this priority band.
	PriorityName() string

	// CapacityBytes returns the configured maximum total byte size for this priority band.
	// The Flow Controller uses this limit in its capacity checking logic. A value of 0 might indicate no specific byte
	// limit for this band (beyond global limits or other constraints).
	CapacityBytes() uint64

	// FlowIDs returns a slice of all flow IDs currently active or draining within this priority band.
	// The order is not guaranteed unless specified by the implementation (e.g., for deterministic testing).
	FlowIDs() []string

	// Queue returns a `FlowQueueAccessor` for the specified flowID within this band.
	// Conformance: Returns nil if the flowID is not found in this band.
	Queue(flowID string) FlowQueueAccessor

	// IterateQueues executes the given callback for each `FlowQueueAccessor` in this priority band.
	// Iteration stops if the callback returns false.
	// The order of iteration is not guaranteed unless specified by the implementation (e.g., for deterministic testing).
	IterateQueues(callback func(queue FlowQueueAccessor) (keepIterating bool))
}

// FlowQueueAccessor provides a policy-facing, read-only view of a single flow's queue. It combines general queue
// inspection methods with flow-specific metadata.
//
// Instances are vended by a `ports.ManagedQueue` and are the primary means by which policies inspect queue state.
//
// Conformance: All methods defined in this interface (including those embedded from `QueueInspectionMethods`) MUST be
// goroutine-safe for concurrent access.
type FlowQueueAccessor interface {
	QueueInspectionMethods

	// FlowSpec returns the specification of the flow this queue accessor is associated with, providing essential context
	// (like FlowID) to policies.
	//
	// Conformance: MUST return a non-nil `types.FlowSpecification`.
	FlowSpec() types.FlowSpecification

	// Comparator returns the `ItemComparator` that defines the dispatch ordering for items within this queue, sourced
	// from the `IntraFlowDispatchPolicy` configured for this flow.
	//
	// Conformance: MUST return a non-nil `ItemComparator`.
	Comparator() ItemComparator
}
