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

package queue

import (
	"container/list"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

const ListQueueName = "ListQueue"

func init() {
	RegisterQueue(ListQueueName, func(_ types.ItemComparator) (types.SafeQueue, error) {
		return NewListQueue(), nil
	})
}

// ListQueue implements the types.SafeQueue interface using a standard container/list.List for FIFO (First-In,
// First-Out) behavior.
type ListQueue struct {
	requests *list.List
	byteSize atomic.Uint64
	mu       sync.RWMutex
}

// listItemHandle is the concrete type for types.QueueItemHandle used by ListQueue.
// It wraps the list.Element and includes a pointer to the owning ListQueue for validation, ensuring that a handle from
// one queue instance cannot be used to operate on another.
// It also tracks its invalidation state.
type listItemHandle struct {
	element       *list.Element // The actual element in the container/list
	owner         *ListQueue    // Pointer to the ListQueue instance that owns this handle
	isInvalidated bool
	mu            sync.Mutex
}

// Handle returns the underlying queue-specific raw handle, which is the *list.Element.
func (lh *listItemHandle) Handle() any {
	return lh.element
}

// Invalidate marks this handle instance as no longer valid for future operations.
// It is idempotent.
func (lh *listItemHandle) Invalidate() {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	lh.isInvalidated = true
}

// IsInvalidated returns true if this handle instance has been marked as invalid.
func (lh *listItemHandle) IsInvalidated() bool {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	return lh.isInvalidated
}

var _ types.QueueItemHandle = &listItemHandle{} // Compile-time validation

// NewListQueue creates a new ListQueue.
func NewListQueue() *ListQueue {
	return &ListQueue{
		requests: list.New(),
	}
}

// --- SafeQueue Interface Implementation ---

// Add attempts to enqueue an item to the back of the list.
func (lq *ListQueue) Add(item types.QueueItemAccessor) (newLen uint64, newByteSize uint64, err error) {
	lq.mu.Lock()
	defer lq.mu.Unlock()

	if item == nil {
		return uint64(lq.requests.Len()), lq.byteSize.Load(), types.ErrNilQueueItem
	}

	element := lq.requests.PushBack(item)
	lq.byteSize.Add(item.ByteSize())
	item.SetHandle(&listItemHandle{element: element, owner: lq})
	return uint64(lq.requests.Len()), lq.byteSize.Load(), nil
}

// Remove removes and returns the QueueItemAccessor for the item identified by the given handle.
func (lq *ListQueue) Remove(
	handle types.QueueItemHandle,
) (removedItem types.QueueItemAccessor, newLen uint64, newByteSize uint64, err error) {
	lq.mu.Lock()
	defer lq.mu.Unlock()

	if handle == nil {
		return nil, uint64(lq.requests.Len()), lq.byteSize.Load(), types.ErrInvalidQueueItemHandle
	}

	if handle.IsInvalidated() {
		return nil, uint64(lq.requests.Len()), lq.byteSize.Load(),
			fmt.Errorf("%w: provided handle is already marked as invalidated", types.ErrInvalidQueueItemHandle)
	}

	lh, ok := handle.(*listItemHandle)
	if !ok {
		return nil, uint64(lq.requests.Len()), lq.byteSize.Load(),
			fmt.Errorf("%w: expected *listItemHandle, got %T", types.ErrInvalidQueueItemHandle, handle)
	}

	if lh.owner != lq {
		return nil, uint64(lq.requests.Len()), lq.byteSize.Load(),
			fmt.Errorf("%w: handle owner mismatch, invalid for this ListQueue instance", types.ErrInvalidQueueItemHandle)
	}

	if lh.element == nil {
		// This case implies the handle itself is malformed.
		// Since IsInvalidated() was false, this indicates an inconsistent or improperly managed handle state.
		handle.Invalidate() // Mark it as invalid now
		return nil, uint64(lq.requests.Len()), lq.byteSize.Load(),
			fmt.Errorf("%w: handle's internal element is nil", types.ErrInvalidQueueItemHandle)
	}

	item := lh.element.Value.(types.QueueItemAccessor)
	lq.requests.Remove(lh.element)
	lq.byteSize.Add(^(item.ByteSize() - 1))
	handle.Invalidate()
	return item, uint64(lq.requests.Len()), lq.byteSize.Load(), nil
}

// CleanupExpired iterates through items, using isItemExpired to check each one.
// If an item is expired, it is removed. The ExpiredItemInfo will contain the QueueItemAccessor of the removed item.
// Any QueueItemHandle associated with a removed item should be considered invalidated.
func (lq *ListQueue) CleanupExpired(
	currentTime time.Time,
	isItemExpired types.IsItemExpiredFunc,
) ([]types.ExpiredItemInfo, error) {
	lq.mu.Lock()
	defer lq.mu.Unlock()

	var removedItemsInfo []types.ExpiredItemInfo
	var next *list.Element

	for e := lq.requests.Front(); e != nil; e = next {
		next = e.Next() // Get next element before potentially removing current 'e'

		item := e.Value.(types.QueueItemAccessor)
		expired, outcome, errForExpiry := isItemExpired(item, currentTime)
		if expired {
			lq.requests.Remove(e)
			lq.byteSize.Add(^(item.ByteSize() - 1))
			if itemHandle := item.Handle(); itemHandle != nil {
				itemHandle.Invalidate()
			}

			removedItemsInfo = append(removedItemsInfo, types.ExpiredItemInfo{
				Item:    item,
				Outcome: outcome,
				Error:   errForExpiry,
			})
		}
	}
	return removedItemsInfo, nil
}

// --- SafeQueue Interface: QueueInspectionMethods Implementation ---

// Len returns the current number of items in the queue.
func (lq *ListQueue) Len() int {
	lq.mu.RLock()
	defer lq.mu.RUnlock()
	return lq.requests.Len()
}

// ByteSize returns the current total byte size of all items in the queue.
func (lq *ListQueue) ByteSize() uint64 {
	return lq.byteSize.Load()
}

// Name returns a string identifier for this type of concrete queue implementation.
func (lq *ListQueue) Name() string {
	return ListQueueName
}

// Capabilities returns the set of capabilities this queue instance provides.
func (lq *ListQueue) Capabilities() []types.QueueCapability {
	// container/list supports efficient Front and Back operations, so it can be considered DoubleEnded for peeking.
	return []types.QueueCapability{types.CapabilityFIFO, types.CapabilityDoubleEnded}
}

// PeekHead returns a QueueItemAccessor for the item at the front of the list, without removing it.
func (lq *ListQueue) PeekHead() (types.QueueItemAccessor, error) {
	lq.mu.RLock()
	defer lq.mu.RUnlock()

	if lq.requests.Len() == 0 {
		return nil, types.ErrQueueEmpty
	}
	element := lq.requests.Front()
	return element.Value.(types.QueueItemAccessor), nil
}

// PeekTail returns a QueueItemAccessor for the item at the back of the list, without removing it.
func (lq *ListQueue) PeekTail() (types.QueueItemAccessor, error) {
	lq.mu.RLock()
	defer lq.mu.RUnlock()

	if lq.requests.Len() == 0 {
		return nil, types.ErrQueueEmpty
	}
	element := lq.requests.Back()
	return element.Value.(types.QueueItemAccessor), nil
}

var _ types.SafeQueue = &ListQueue{} // Compile-time validation
