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

// PriorityBandAccessor provides a read-only view into a specific priority band within the `ports.FlowRegistry`.
// It allows the `controller.FlowController` and inter-flow policies to inspect the state of all flow queues within that
// band.
//
// Conformance: Implementations MUST ensure all methods are goroutine-safe for concurrent access.
type PriorityBandAccessor interface {
	// Priority returns the numerical priority level of this band.
	Priority() uint

	// PriorityName returns the human-readable name of this priority band.
	PriorityName() string

	// FlowIDs returns a slice of all flow IDs within this priority band.
	// The order of items in the slice is not guaranteed. Test implementations should return a sorted slice for
  // deterministic behavior.
	FlowIDs() []string

	// Queue returns a `FlowQueueAccessor` for the specified `flowID` within this priority band.
	//
	// Conformance: Implementations MUST return nil if the `flowID` is not found in this band.
	Queue(flowID string) FlowQueueAccessor

	// IterateQueues executes the given `callback` for each `FlowQueueAccessor` in this priority band.
	// Iteration stops if the `callback` returns false. The order of iteration is not guaranteed unless specified by the
	// implementation (e.g., for deterministic testing scenarios).
	IterateQueues(callback func(queue FlowQueueAccessor) (keepIterating bool))
}

// FlowQueueAccessor provides a policy-facing, read-only view of a single flow's queue.
// It combines general queue inspection methods (embedded via `QueueInspectionMethods`) with flow-specific metadata.
//
// Instances of `FlowQueueAccessor` are vended by a `ports.ManagedQueue` and are the primary means by which policies
// inspect individual queue state.
//
// Conformance: Implementations MUST ensure all methods (including those embedded from `QueueInspectionMethods`) are
// goroutine-safe for concurrent access.
type FlowQueueAccessor interface {
	QueueInspectionMethods

	Comparator() ItemComparator

	// FlowSpec returns the `types.FlowSpecification` of the flow this queue accessor is associated with.
	// This provides essential context (like `FlowID`) to policies.
	//
	// Conformance: Implementations MUST return a non-nil `types.FlowSpecification`.
	FlowSpec() types.FlowSpecification
}
