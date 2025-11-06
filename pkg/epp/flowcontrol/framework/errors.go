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
// These sentinel errors are returned by SafeQueue plugin implementations.
var (
	// ErrNilQueueItem is returned by SafeQueue.Add when the provided item is nil.
	ErrNilQueueItem = errors.New("queue item cannot be nil")

	// ErrQueueEmpty is returned by queue operations that require at least one item (e.g., PeekHead) when the queue is
	// empty.
	ErrQueueEmpty = errors.New("queue is empty")

	// ErrInvalidQueueItemHandle is returned by SafeQueue.Remove when the provided handle is not valid for the queue
	// This can occur if the handle is nil, was created by a different queue instance, or has already been invalidated by
	// a prior removal operation.
	ErrInvalidQueueItemHandle = errors.New("invalid queue item handle")

	// ErrQueueItemNotFound is returned by SafeQueue.Remove when the provided handle is valid, but the corresponding item
	// is not found in the queue.
	// This typically indicates that the item was removed by a concurrent operation after its handle was acquired.
	ErrQueueItemNotFound = errors.New("queue item not found for the given handle")
)

// --- Policy Errors ---
// These sentinel errors are returned by Policy plugin implementations.
var (
	// ErrIncompatiblePriorityType is returned by an InterFlowDispatchPolicy when it attempts to compare items from two
	// queues whose ItemComparator plugins
	// have mismatching ScoreType() values. A meaningful comparison is only possible if the scoring domains are identical.
	ErrIncompatiblePriorityType = errors.New("incompatible priority score type for comparison")
)
