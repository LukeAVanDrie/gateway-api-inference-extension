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
	"errors"
)

// --- SafeQueue Errors ---
// Errors relating to operations directly on a SafeQueue implementation.
//
// These are returned by `SafeQueue` methods and might be handled or wrapped by the Flow Registry's `ports.ManagedQueue`
// or the Flow Controller.
var (
	// ErrQueueEmpty indicates an attempt to operate on an empty `SafeQueue` in a way that requires items (e.g.,
	// `SafeQueue.PeekHead()`).
	ErrQueueEmpty = errors.New("queue is empty")

	// ErrQueueItemNotFound indicates that a `SafeQueue.Remove(handle)` operation did not find an item matching the
	// provided valid `QueueItemHandle`.
	ErrQueueItemNotFound = errors.New("queue item not found for the given handle")

	// ErrNilQueueItem indicates that a nil `types.QueueItemAccessor` was passed to `SafeQueue.Add()`.
	ErrNilQueueItem = errors.New("queue item cannot be nil")

	// ErrInvalidQueueItemHandle indicates that a `types.QueueItemHandle` provided to a `SafeQueue` operation (like
	// `SafeQueue.Remove()`) is not valid for that queue or operation.
	ErrInvalidQueueItemHandle = errors.New("invalid queue item handle")

	// ErrOperationNotSupported indicates that an operation (e.g., `SafeQueue.PeekTail()`) was called on a `SafeQueue`
	// implementation that does not support it.
	ErrOperationNotSupported = errors.New("operation not supported by this queue type")
)

// --- Policy Errors ---
// Errors returned by Policy implementations or by the Flow Registry during policy validation.
var (
	// ErrIncompatiblePriorityType is returned by a policy (typically an `InterFlow...Policy`) if it cannot meaningfully
	// compare items from different queues because their `ItemComparator` instances have incompatible
	// `ScoreType`s.
	ErrIncompatiblePriorityType = errors.New("incompatible item comparator ScoreTypes for comparison by policy")

	// ErrPolicyQueueMismatch is returned by the Flow Registry during configuration if a policy's
	// `RequiredQueueCapabilities()` are not met by the `SafeQueue` it is being associated with.
	ErrPolicyQueueMismatch = errors.New("policy requirements incompatible with configured queue capabilities")
)
