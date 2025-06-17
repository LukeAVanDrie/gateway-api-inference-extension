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

package ports

import (
	"errors"
)

// --- FlowRegistry Errors ---

// The following errors relate to operations on the FlowRegistry, such as flow registration, updates, or lookups.
// They are typically returned by FlowRegistry methods and may be wrapped by types.ErrRejected if they cause a
// FlowController.EnqueueAndWait operation to fail.

var (
	// ErrFlowIDEmpty indicates that a required flow ID was not provided.
	ErrFlowIDEmpty = errors.New("flow ID cannot be empty")

	// ErrFlowNotRegistered indicates that an operation requiring an active flow was attempted on an unregistered flow.
	ErrFlowNotRegistered = errors.New("flow not registered")

	// ErrFlowInstanceNotFound indicates that an operation was attempted on a specific instance of a flow (i.e., a flow at
	// a particular priority) that does not currently exist in the registry.
	ErrFlowInstanceNotFound = errors.New("flow instance not found in registry")

	// ErrPriorityBandNotFound indicates that an operation was attempted with a priority value that does not correspond to
	// any configured priority band.
	ErrPriorityBandNotFound = errors.New("priority band not configured")
)
