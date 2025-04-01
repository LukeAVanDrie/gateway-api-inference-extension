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
	"testing"
	"time"

	"slices"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/testing/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

func TestQueue_Conformance(t *testing.T) {
	t.Parallel()
	for queueName, factory := range registeredQueues {
		queueName := queueName
		factory := factory

		t.Run(string(queueName), func(t *testing.T) {
			t.Parallel()
			flowSpec := mocks.NewMockFlowSpecification("test-flow-1", 0)

			// Define a specific ItemComparatorFunc based on enqueue time (First-Come, First-Served).
			// This comparator is passed to each queue factory.
			// For queues declaring CapabilityPriorityConfigurable, they MUST use this comparator for ordering.
			// For queues with inherent FIFO behavior, this comparator aligns with their natural ordering.
			// This allows the conformance tests to deterministically verify PeekHead/PeekTail's behavior against a known
			// ordering principle for any arbitrary queue implementation.
			comparator := mocks.NewMockItemComparator(func(a, b types.QueueItemAccessor) bool {
				return a.EnqueueTime().Before(b.EnqueueTime())
			}, "enqueue_time_ns_asc")

			t.Run("NewQueueInitialization", func(t *testing.T) {
				t.Parallel()
				q, err := factory(comparator)
				require.NoError(t, err, "Queue factory failed")

				require.NotNil(t, q, "Queue instance should not be nil")
				assert.Equal(t, 0, q.Len(), "Queue length should be 0")
				assert.Equal(t, uint64(0), q.ByteSize(), "Queue byte size should be 0")
				assert.Equal(t, string(queueName), q.Name(), "Queue name should match expected")
				assert.NotNil(t, q.Capabilities(), "Queue capabilities should not be nil")
			})

			t.Run("Lifecycle", func(t *testing.T) {
				t.Parallel()
				q, err := factory(comparator)
				require.NoError(t, err, "Queue factory failed")

				hasCapabilityDoubleEnded := hasCapability(q, types.CapabilityDoubleEnded)

				now := time.Now()
				item1Time := now.Add(-2 * time.Second) // Earliest
				item2Time := now.Add(-1 * time.Second) // Middle
				item3Time := now                       // Latest

				item1 := mocks.NewMockQueueItemAccessor("item1", flowSpec.ID(), 100, item1Time)
				item2 := mocks.NewMockQueueItemAccessor("item2", flowSpec.ID(), 50, item2Time)
				item3 := mocks.NewMockQueueItemAccessor("item3", flowSpec.ID(), 20, item3Time)
				itemsInOrder := []types.QueueItemAccessor{item1, item2, item3}

				// PeekHead on empty queue
				peeked, err := q.PeekHead()
				assert.ErrorIs(t, err, types.ErrQueueEmpty, "PeekHead on empty queue should return ErrQueueEmpty")
				assert.Nil(t, peeked, "PeekHead on empty queue should return nil item")

				// PeekTail on empty queue
				if hasCapabilityDoubleEnded {
					peeked, err := q.PeekTail()
					assert.ErrorIs(t, err, types.ErrQueueEmpty, "PeekTail on empty queue should return ErrQueueEmpty")
					assert.Nil(t, peeked, "PeekTail on empty queue should return nil item")
				}

				// Add
				currentExpectedLen := 0
				var currentExpectedByteSize uint64
				for i, item := range itemsInOrder {
					newLen, newByteSize, err := q.Add(item)
					require.NoError(t, err, "Failed to add item %s", item.RequestID())
					require.NotNil(t, item.Handle(), "Handle for item %s should not be nil after Add", item.RequestID())
					require.False(t, item.Handle().IsInvalidated(),
						"Handle for item %s should not be invalidated after Add", item.RequestID())

					currentExpectedLen++
					currentExpectedByteSize += item.ByteSize()
					assert.Equal(t, uint64(currentExpectedLen), newLen,
						"newLen after adding item %s (index %d)", item.RequestID(), i)
					assert.Equal(t, currentExpectedByteSize, newByteSize,
						"newByteSize after adding item %s (index %d)", item.RequestID(), i)
				}
				initialLen := len(itemsInOrder)
				initialByteSize := item1.ByteSize() + item2.ByteSize() + item3.ByteSize()
				assert.Equal(t, initialLen, q.Len(), "Queue length after adding all items")
				assert.Equal(t, initialByteSize, q.ByteSize(), "Queue byte size after adding all items")

				// Peek and Remove cycle (verifying FCFS order due to provided comparator)
				expectedLen := initialLen
				expectedByteSize := initialByteSize
				for i, expectedItem := range itemsInOrder {
					t.Logf("Peek/Remove cycle for item: %s", expectedItem.RequestID())

					// PeekHead
					peeked, err := q.PeekHead()
					require.NoError(t, err, "PeekHead should not error on non-empty queue (iteration %d)", i)
					require.NotNil(t, peeked, "PeekHead should return a non-nil item (iteration %d)", i)
					assert.Equal(t, expectedItem.RequestID(), peeked.RequestID(),
						"PeekHead should return item %s (earliest enqueued)", expectedItem.RequestID())
					peekedHandle := peeked.Handle()
					require.NotNil(t, peekedHandle, "Handle from peeked head item %s should not be nil", peeked.RequestID())
					require.False(t, peekedHandle.IsInvalidated(),
						"Handle from peeked head item %s should not be invalidated", peeked.RequestID())
					assert.Equal(t, expectedLen, q.Len(),
						"Queue length should be unchanged after PeekHead (item: %s, iteration %d)", expectedItem.RequestID(), i)
					assert.Equal(t, expectedByteSize, q.ByteSize(),
						"Queue byte size should be unchanged after PeekHead (item: %s, iteration %d)", expectedItem.RequestID(), i)

					// PeekTail
					if hasCapabilityDoubleEnded {
						peeked, err := q.PeekTail()
						require.NoError(t, err, "PeekTail should not error on non-empty queue (iteration %d)", i)
						require.NotNil(t, peeked, "PeekTail should return a non-nil item (iteration %d)", i)
						assert.Equal(t, item3.RequestID(), peeked.RequestID(),
							"PeekTail should return item %s (latest enqueued)", item3.RequestID())
						peekedHandle := peeked.Handle()
						require.NotNil(t, peekedHandle, "Handle from peeked tail item %s should not be nil", peeked.RequestID())
						require.False(t, peekedHandle.IsInvalidated(),
							"Handle from peeked tail item %s should not be invalidated", peeked.RequestID())
						assert.Equal(t, expectedLen, q.Len(),
							"Queue length should be unchanged after PeekTail (item: %s, iteration %d)", expectedItem.RequestID(), i)
						assert.Equal(t, expectedByteSize, q.ByteSize(),
							"Queue byte size should be unchanged after PeekTail (item: %s, iteration %d)",
							expectedItem.RequestID(), i)
					}

					// Remove the peeked (head) item by its handle
					removed, newLen, newByteSize, err := q.Remove(peekedHandle)
					require.NoError(t, err, "Remove(peekedHandle) for item %s failed", peeked.RequestID())
					require.NotNil(t, removed, "Remove(peekedHandle) for item %s returned nil", peeked.RequestID())
					assert.Equal(t, expectedItem.RequestID(), removed.RequestID(),
						"Removed item should be %s", expectedItem.RequestID())
					assert.True(t, peekedHandle.IsInvalidated(),
						"Handle of removed item %s should be invalidated", removed.RequestID())

					expectedLen--
					expectedByteSize -= removed.ByteSize()
					assert.Equal(t, uint64(expectedLen), newLen, "newLen after removing %s", removed.RequestID())
					assert.Equal(t, expectedByteSize, newByteSize, "newByteSize after removing %s", removed.RequestID())
					assert.Equal(t, expectedLen, q.Len(), "Queue length after removing %s", removed.RequestID())
					assert.Equal(t, expectedByteSize, q.ByteSize(), "Queue byte size after removing %s", removed.RequestID())
				}

				assert.Equal(t, 0, q.Len(), "Queue should be empty after all items are removed")
				assert.Equal(t, uint64(0), q.ByteSize(), "Queue byte size should be 0 after all items are removed")

				// PeekHead on empty queue again
				peeked, err = q.PeekHead()
				assert.ErrorIs(t, err, types.ErrQueueEmpty, "PeekHead on empty queue should return ErrQueueEmpty")
				assert.Nil(t, peeked, "PeekHead on empty queue should return nil item")

				// PeekTail on empty queue again
				if hasCapabilityDoubleEnded {
					peeked, err := q.PeekTail()
					assert.ErrorIs(t, err, types.ErrQueueEmpty, "PeekTail on empty queue should return ErrQueueEmpty")
					assert.Nil(t, peeked, "PeekTail on empty queue should return nil item")
				}
			})

			t.Run("Add_Nil", func(t *testing.T) {
				t.Parallel()
				q, err := factory(comparator)
				require.NoError(t, err, "Queue factory failed")

				currentLen := q.Len()
				currentByteSize := q.ByteSize()
				newLen, newByteSize, err := q.Add(nil)
				assert.ErrorIs(t, err, types.ErrNilQueueItem, "Add(nil) should return ErrNilQueueItem")
				assert.Equal(t, uint64(currentLen), newLen, "Add(nil) should return current length")
				assert.Equal(t, currentByteSize, newByteSize, "Add(nil) should return current byte size")
				assert.Equal(t, currentLen, q.Len(), "Queue length should be unchanged after Add(nil)")
				assert.Equal(t, currentByteSize, q.ByteSize(), "Queue byte size should be unchanged after Add(nil)")
			})

			t.Run("Remove_InvalidHandle", func(t *testing.T) {
				t.Parallel()
				q, err := factory(comparator)
				require.NoError(t, err, "Queue factory failed")

				item := mocks.NewMockQueueItemAccessor("item", flowSpec.ID(), 100, time.Now())
				_, _, err = q.Add(item)
				require.NoError(t, err, "Add item failed in Remove_InvalidHandle setup")

				for _, test := range []struct {
					name        string
					setupHandle func() types.QueueItemHandle
				}{
					{
						name:        "nil handle",
						setupHandle: func() types.QueueItemHandle { return nil },
					},
					{
						name: "invalidated handle",
						setupHandle: func() types.QueueItemHandle {
							mockHandle := mocks.NewMockQueueItemHandle(nil)
							mockHandle.Invalidate()
							return mockHandle
						},
					},
					{
						name: "alien handle",
						setupHandle: func() types.QueueItemHandle {
							otherFlowSpec := mocks.NewMockFlowSpecification("other-flow", 1)
							otherQ, errFactoryOther := factory(comparator)
							require.NoError(t, errFactoryOther, "Queue factory failed for otherQ")
							otherItem := mocks.NewMockQueueItemAccessor("otherItem", otherFlowSpec.ID(), 10, time.Now())
							_, _, errAddOther := otherQ.Add(otherItem)
							require.NoError(t, errAddOther)
							otherHandle := otherItem.Handle()
							require.NotNil(t, otherHandle)
							return otherHandle
						},
					},
				} {
					t.Run(test.name, func(t *testing.T) {
						t.Parallel()
						h := test.setupHandle()
						currentLen := q.Len()
						currentByteSize := q.ByteSize()

						_, newLen, newByteSize, err := q.Remove(h)
						assert.ErrorIs(t, err, types.ErrInvalidQueueItemHandle, "Remove should return ErrInvalidQueueItemHandle")
						assert.Equal(t, uint64(currentLen), newLen, "newLen after Remove with %s", test.name)
						assert.Equal(t, currentByteSize, newByteSize, "newByteSize after Remove with %s", test.name)
						assert.Equal(t, currentLen, q.Len(), "Queue length should be unchanged after Remove with %s", test.name)
						assert.Equal(t, currentByteSize, q.ByteSize(),
							"Queue byte size should be unchanged after Remove with %s", test.name)
					})
				}
			})

			t.Run("Remove_NonHead", func(t *testing.T) {
				t.Parallel()
				q, err := factory(comparator)
				require.NoError(t, err, "Queue factory failed")

				now := time.Now()
				item1Time := now.Add(-2 * time.Second) // Earliest
				item2Time := now.Add(-1 * time.Second) // Middle
				item3Time := now                       // Latest

				item1 := mocks.NewMockQueueItemAccessor("item1", flowSpec.ID(), 10, item1Time)
				item2 := mocks.NewMockQueueItemAccessor("item2", flowSpec.ID(), 20, item2Time)
				item3 := mocks.NewMockQueueItemAccessor("item3", flowSpec.ID(), 30, item3Time)
				_, _, _ = q.Add(item1)
				_, _, _ = q.Add(item2)
				_, _, _ = q.Add(item3)
				handleNonHead := item2.Handle()

				removed, newLen, newByteSize, err := q.Remove(handleNonHead)
				require.NoError(t, err, "Error removing non-head item item2")
				require.NotNil(t, removed, "Removed item should not be nil when removing non-head item2")
				assert.Equal(t, item2.RequestID(), removed.RequestID(), "Removed item ID should be item2's ID")
				assert.True(t, handleNonHead.IsInvalidated(), "Handle for item2 should be invalidated after removal")
				assert.Equal(t, uint64(2), newLen, "newLen should be 2 after removing item2")
				assert.Equal(t, item1.ByteSize()+item3.ByteSize(), newByteSize,
					"newByteSize should be sum of item1 and item3 after removing item2")
				assert.Equal(t, item1.ByteSize()+item3.ByteSize(), q.ByteSize(),
					"Queue ByteSize should be sum of item1 and item3 after removing item2")
				assert.Equal(t, 2, q.Len(), "Queue Len should be 2 after removing item2")

				peeked, _ := q.PeekHead()
				require.NotNil(t, peeked, "PeekHead should not return nil after removing a non-head item")
				assert.Equal(t, item1.RequestID(), peeked.RequestID(), "PeekHead should still be item1 after removing item2")

				_, _, _, errStaleNonHead := q.Remove(handleNonHead)
				assert.ErrorIs(t, errStaleNonHead, types.ErrInvalidQueueItemHandle)
			})

			t.Run("CleanupExpired", func(t *testing.T) {
				t.Parallel()
				q, err := factory(comparator)
				require.NoError(t, err, "Queue factory failed")
				now := time.Now()

				expireAfter := 7 * time.Second
				item1 := mocks.NewMockQueueItemAccessor("item1", flowSpec.ID(), 10, now.Add(-10*time.Second)) // Expired
				item2 := mocks.NewMockQueueItemAccessor("item2", flowSpec.ID(), 20, now.Add(-5*time.Second))  // Not expired
				item3 := mocks.NewMockQueueItemAccessor("item3", flowSpec.ID(), 30, now)                      // Not expired
				item4 := mocks.NewMockQueueItemAccessor("item4", flowSpec.ID(), 40, now.Add(-15*time.Second)) // Expired

				_, _, err = q.Add(item1)
				require.NoError(t, err, "Failed to add item1 for CleanupExpired test")
				handle1 := item1.Handle()
				_, _, err = q.Add(item2)
				require.NoError(t, err, "Failed to add item2 for CleanupExpired test")
				handle2 := item2.Handle()
				_, _, err = q.Add(item3)
				require.NoError(t, err, "Failed to add item3 for CleanupExpired test")
				handle3 := item3.Handle()
				_, _, err = q.Add(item4)
				require.NoError(t, err, "Failed to add item4 for CleanupExpired test")
				handle4 := item4.Handle()

				isExpiredFunc := func(item types.QueueItemAccessor, currentTime time.Time) (bool, types.QueueOutcome, error) {
					if currentTime.Sub(item.EnqueueTime()) > expireAfter {
						return true, types.QueueOutcomeEvictedTTL, types.ErrTTLExpired
					}
					return false, types.QueueOutcomeDispatched, nil // Outcome and error are ignored if not expired
				}

				removedInfos, errClean := q.CleanupExpired(now, isExpiredFunc)
				require.NoError(t, errClean, "CleanupExpired should not return an error")
				require.Len(t, removedInfos, 2, "Should remove 2 expired items (item1, item4)")

				assert.Equal(t, 2, q.Len()) // item2 and item3 remain
				assert.Equal(t, item2.ByteSize()+item3.ByteSize(), q.ByteSize(),
					"ByteSize should be sum of remaining items after CleanupExpired")
				assert.True(t, handle1.IsInvalidated(), "Handle for item1 should be invalidated after CleanupExpired")
				assert.False(t, handle2.IsInvalidated(), "Handle for item2 should NOT be invalidated")
				assert.False(t, handle3.IsInvalidated(), "Handle for item3 should NOT be invalidated")
				assert.True(t, handle4.IsInvalidated(), "Handle for item4 should be invalidated after CleanupExpired")

				removedIDsFound := make(map[string]bool)
				for _, info := range removedInfos {
					assert.Equal(t, types.QueueOutcomeEvictedTTL, info.Outcome,
						"Outcome for expired item %s should be EvictedTTL", info.Item.RequestID())
					assert.ErrorIs(t, info.Error, types.ErrTTLExpired,
						"Error for expired item %s should be ErrTTLExpired", info.Item.RequestID())
					removedIDsFound[info.Item.RequestID()] = true
					itemHandle := info.Item.Handle()
					require.NotNil(t, itemHandle, "Handle from ExpiredItemInfo.Item should not be nil")
					assert.True(t, itemHandle.IsInvalidated(),
						"Handle from ExpiredItemInfo.Item should be invalidated by CleanupExpired")
				}
				assert.True(t, removedIDsFound[item1.RequestID()], "item1 should be in removedItemsInfo")
				assert.True(t, removedIDsFound[item4.RequestID()], "item4 should be in removedItemsInfo")
			})
		})
	}
}

func hasCapability(q types.SafeQueue, cap types.QueueCapability) bool {
	return slices.Contains(q.Capabilities(), cap)
}
