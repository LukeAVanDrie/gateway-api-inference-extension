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

// FlowQueueAccessor provides a policy-facing, read-only view of a single flow's queue.
// It combines general queue inspection methods with flow-specific metadata, giving policies the necessary context to
// make decisions without allowing them to mutate queue state directly.
//
// Conformance: Implementations MUST ensure all methods are goroutine-safe.
type FlowQueueAccessor interface {
	QueueInspectionMethods

	// Comparator returns the ItemComparator that defines the ordering logic of the items within this queue.
	// This is determined by the IntraFlowDispatchPolicy associated with this queue's flow.
	Comparator() ItemComparator

	// FlowKey returns the unique, immutable key of the flow associated with this queue.
	// This provides essential context (e.g., Fairness ID, Priority) to policies.
	FlowKey() types.FlowKey
}

// PriorityBandAccessor provides a read-only view into a specific priority band, which contains all flow queues at that
// priority level.
// It is the primary interface used by an InterFlowDispatchPolicy to inspect the state of its domain.
//
// Conformance: Implementations MUST ensure all methods are goroutine-safe.
type PriorityBandAccessor interface {
	// Priority returns the numerical priority level of this band.
	Priority() int

	// PriorityName returns the human-readable name of this priority band.
	PriorityName() string

	// FlowKeys returns a slice of the keys for every flow within this priority band.
	// The order of keys is not guaranteed.
	FlowKeys() []types.FlowKey

	// Queue returns a FlowQueueAccessor for the specified flow ID within this band.
	// Note: This uses the flow's ID (types.FlowKey.ID), as the priority is already scoped by this accessor.
	//
	// Conformance: Implementations MUST return nil if the ID is not found.
	Queue(id string) FlowQueueAccessor

	// IterateQueues executes the given callback for each FlowQueueAccessor in the band.
	// Iteration stops if the callback returns false.
	// The order of iteration is not guaranteed.
	IterateQueues(callback func(queue FlowQueueAccessor) (keepIterating bool))
}
