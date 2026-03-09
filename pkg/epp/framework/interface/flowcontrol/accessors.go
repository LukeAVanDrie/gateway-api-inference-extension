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

package flowcontrol

// FlowQueueAccessor represents the runtime state of a single active Flow.
//
// Role in Fairness:
// To a Fairness Policy, this interface represents a "Candidate": a distinct stream of requests that is competing for
// dispatch. The policy may inspect the state of this object (e.g., how many requests are waiting, how long they have
// been waiting, etc.) to decide if it should be the "Winner". Alternatively, it may operate on orthogonal state tracked
// for each FlowKey.
//
// Conformance: Implementations MUST ensure all methods are goroutine-safe.
type FlowQueueAccessor interface {
	QueueInspectionMethods

	// OrderingPolicy returns the policy implementation that rules this queue's internal ordering.
	// This allows fairness policies (like "global-strict-fairness-policy") to inspect the ordering logic when comparing
	// items across queues.
	OrderingPolicy() OrderingPolicy

	// FlowKey returns the unique, immutable identity of the flow instance this queue belongs to.
	// This provides essential context (like the logical grouping ID and Priority) to policies.
	FlowKey() FlowKey
}
