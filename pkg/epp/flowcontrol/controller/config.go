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

package controller

import "time"

// FlowControllerConfig holds the top-level configuration for a FlowController instance.

// FlowControllerConfig holds the top-level configuration for a FlowController instance.
type FlowControllerConfig struct {
	// DefaultQueueTTL is the default Time-To-Live applied to requests within queues if the incoming request does not
	// specify its own via InitialEffectiveTTL().
	//
	// Optional: If not set, a reasonable system default (e.g., 30 seconds) will be used.
	DefaultQueueTTL time.Duration

	// ExpiryCleanupInterval is the frequency at which each FlowController worker's background routine checks for and
	// removes expired items from all managed queues in its shard.
	//
	// Optional: If not set or set to zero, a reasonable system default (e.g., 1 second) will be used.
	ExpiryCleanupInterval time.Duration
}
