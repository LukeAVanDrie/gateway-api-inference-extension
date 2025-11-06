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

// Package framework defines the plugin extension points and contracts for the Flow Control layer.
// It provides a set of interfaces that allow developers to customize the core logic of request queuing, prioritization,
// and dispatch.
//
// # Architecture Overview
//
// The framework is built on three core concepts:
//
// 1. Policies: These plugins define the "decision-making" logic of the system.
//   - ItemComparator: The most fundamental building block, defining the relative priority between two requests.
//   - IntraFlowDispatchPolicy: Decides which request to dispatch next from *within* a single request flow (e.g., FCFS).
//   - InterFlowDispatchPolicy: Decides which flow to service next from a set of competing flows at the same priority
//     level, thus defining fairness.
//
// 2. State Management (SafeQueue): This plugin interface defines the contract for a concurrent-safe queue that stores
// requests for a single flow. Implementations can range from simple FIFO queues to complex priority heaps.
//
// 3. Accessors: These are read-only interfaces that provide policies with a safe, controlled view into the system's
// state (e.g., FlowQueueAccessor, PriorityBandAccessor). Policies use these accessors to inspect queues without being
// able to mutate them directly, enforcing a clean separation of concerns.
//
// A developer seeking to customize the Flow Control layer will typically implement one or more of the plugin interfaces
// defined in this package and register them in the EPP configuration.
package framework
