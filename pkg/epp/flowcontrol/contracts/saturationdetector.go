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

package contracts

import (
	"context"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
)

// PodLocator defines the contract for a component that resolves the set of candidate pods for a request based on its
// metadata.
//
// This interface allows the Flow Controller to fetch a fresh list of pods dynamically during the dispatch cycle,
// enabling support for "Scale-from-Zero" scenarios where pods may not exist when the request is first enqueued.
// It also decouples the Flow Controller from the underlying datastore and filtering logic (e.g., subsetting).
//
// # Conformance
//
// Implementations MUST be goroutine-safe.
type PodLocator interface {
	// Locate returns a list of pod metrics that match the criteria defined in the request metadata.
	Locate(ctx context.Context, requestMetadata map[string]any) []metrics.PodMetrics
}
