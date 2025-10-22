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
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

// FairnessMetric defines the interface for plugins that measure the service received by different flows.
// This interface represents the minimum contract required by a metric-driven fairness policy.
//
// Concrete implementations can optionally implement other plugin interfaces (e.g., requestcontrol.PreRequest,
// requestcontrol.ResponseComplete) to hook into the request lifecycle for instrumentation. The system will use type
// assertions to discover and register these additional capabilities.
//
// # Concurrency
//
// Implementations of this interface MUST be goroutine-safe.
type FairnessMetric interface {
	plugins.Plugin

	// GetValue returns the current fairness value for a single flow key.
	// If the flow key is unknown (i.e., untracked), the implementation must return 0 to signal this state to the
	// consumer.
	GetValue(key types.FlowKey) float64

	// GetValues returns the current fairness values for a slice of flow keys.
	// For any key that is untracked, its entry must be omitted from the returned map.
	GetValues(flowKeys []types.FlowKey) map[types.FlowKey]float64

	// GetAllValues returns all current fairness values for every flow key.
	GetAllValues() map[types.FlowKey]float64
}
