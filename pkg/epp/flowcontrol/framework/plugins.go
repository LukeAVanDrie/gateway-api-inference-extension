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

import "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"

const (
	InterFlowDispatchPolicyType = "InterflowDispatchPolicy"
)

// InterFlowDispatchPolicy selects which flow's queue to service next from a given priority band.
// Implementations define the fairness or dispatch ordering logic between different flows sharing the same priority
// level.
type InterFlowDispatchPolicy interface {
	plugins.Plugin

	// SelectQueue inspects the flow queues within the provided PriorityBandAccessor and returns the queue chosen for the
	// next dispatch attempt.
	// A return of (nil, nil) indicates that no queue was selected (e.g., all queues in the band are empty), which is not
	// considered an error.
	// Conformance: Implementations MUST be goroutine-safe.
	SelectQueue(band PriorityBandAccessor) (selectedQueue FlowQueueAccessor, err error)
}
