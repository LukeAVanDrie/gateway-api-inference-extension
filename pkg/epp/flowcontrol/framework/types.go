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

// PriorityScoreType is a descriptor for the domain of a policy's item comparator.
type PriorityScoreType string

const (
	// EnqueueTimePriorityScoreType indicates that priority is based on the item's enqueue time, with earlier times (lower
	// values) being higher priority.
	EnqueueTimePriorityScoreType PriorityScoreType = "enqueue_time_ns_asc"
)

// QueueCapability defines a functional capability that a SafeQueue can provide.
// Intra-flow policies use these capabilities to declare their requirements.
type QueueCapability string

const (
	// CapabilityFIFO indicates that the queue guarantees First-In, First-Out ordering.
	CapabilityFIFO QueueCapability = "FIFO"

	// CapabilityPriorityConfigurable indicates that the queue's ordering is determined by an externally provided
	// ItemComparator.
	CapabilityPriorityConfigurable QueueCapability = "PriorityConfigurable"
)

// ItemComparatorFunc defines the signature for comparing two items.
// It returns true if item 'a' has higher priority than item 'b'.
type ItemComparatorFunc func(a, b types.QueueItemAccessor) bool

// PredicateFunc defines a function that returns true if a given item matches a certain condition.
// It is used by SafeQueue.Cleanup to identify items for removal.
type PredicateFunc func(item types.QueueItemAccessor) bool
