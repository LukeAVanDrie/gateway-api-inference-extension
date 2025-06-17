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

// Package ports defines the service interfaces that the core FlowController engine uses to interact with its primary
// dependencies. In alignment with a "Ports and Adapters" architectural style, these interfaces represent the "ports"
// that decouple the engine's operational logic from the concrete implementations of its two main services: the
// FlowRegistry system (for state management) and the SaturationDetector (for system load awareness).
package ports
