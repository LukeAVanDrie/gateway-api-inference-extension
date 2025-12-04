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

import (
	"context"

	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

const SaturationControllerExtensionPoint = "SaturationController"

// SaturationController defines the contract for a plugin that governs the admission of requests from the Flow Control
// layer to the Scheduling layer.
//
// It actively participates in the control loop, enforcing backpressure based on the health and capacity of the backend
// pool. Implementations may range from simple static threshold checks (e.g., Queue Depth) to advanced dynamic control
// theoretic approaches (e.g., PID controllers, Concurrency Limiters).
type SaturationController interface {
	plugins.Plugin

	// ShouldDispatch determines whether the backend pool has sufficient capacity to accept a new request.
	//
	// This method is invoked by the Flow Controller on the "hot path" of the dispatch loop; therefore, implementations
	// MUST return quickly and MUST be non-blocking. Heavy computation or synchronous I/O should be avoided or offloaded
	// to a background process (e.g., updating internal state via a ticker).
	//
	// Arguments:
	//   ctx: The context for the operation.
	//   candidates: A list of candidate pods eligible to serve the pending request.
	//               If the list is empty (Scale-from-Zero), implementations should generally return false.
	//
	// Returns:
	//   true:  Dispatch is permitted. The system has capacity.
	//   false: Dispatch is denied. The system is saturated.
	//          This triggers Head-of-Line (HoL) blocking in the Flow Controller, pausing dispatch until capacity
	//          recovers.
	ShouldDispatch(ctx context.Context, candidates []backendmetrics.PodMetrics) bool
}
