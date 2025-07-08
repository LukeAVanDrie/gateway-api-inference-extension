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

package registry

import (
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/ports"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

// managedQueue implements `ports.ManagedQueue`.
// It wraps a `framework.SafeQueue` and handles atomic statistics updates with the registry.
type managedQueue struct {
	mu sync.RWMutex

	flowSpec types.FlowSpecification
	queue framework.SafeQueue
	dispatchPolicy framework.IntraFlowDispatchPolicy

	byteSize atomic.Uint64
	len      atomic.Uint64
	reconcileShardStats func(lenDelta, byteSizeDelta int64)

	logger logr.Logger
}

// newManagedQueue creates a new instance of a `managedQueue`.
func newManagedQueue(
	queue framework.SafeQueue,
	dispatchPolicy framework.IntraFlowDispatchPolicy,
	flowSpec types.FlowSpecification,
	logger logr.Logger,
	reconcileShardStats func(lenDelta, byteSizeDelta int64),
) *managedQueue {
	mqLogger := logger.WithName("managed-queue").WithValues(
		"flowID", flowSpec.ID,
		"priority", flowSpec.Priority,
		"queueType", queue.Name(),
	)
	return &managedQueue{
		queue: queue,
		dispatchPolicy: dispatchPolicy,
		flowSpec:  flowSpec,
		reconcileShardStats: reconcileShardStats,
		logger:    mqLogger,
	}
}

// FlowQueueAccessor returns a new `flowQueueAccessor` instance.
func (mq *managedQueue) FlowQueueAccessor() framework.FlowQueueAccessor {
	// TODO
	return nil
}

func (mq *managedQueue) Add(item types.QueueItemAccessor) (newLen uint64, newByteSize uint64, err error) {
	mq.len.Load()
	mq.byteSize.Load()

	len, byteSize := mq.len.Load(), mq.byteSize.Load()
	newLen, newByteSize, err = mq.queue.Add(item)

	lenDelta, byteSizeDelta := int64(newLen)-int64(len), int64(newByteSize)-int64(byteSize)
	var expectedLenDelta, expectedByteSizeDelta int64
	if item != nil {
		expectedLenDelta, expectedByteSizeDelta = 1, int64(item.OriginalRequest().ByteSize())
	}

	if lenDelta != expectedLenDelta || byteSizeDelta != expectedByteSizeDelta {
		mq.logger.V(logutil.DEBUG).Info("Inconsistent queue stats after Add",
			"expectedLenDelta", expectedLenDelta, "expectedByteSizeDelta", expectedByteSizeDelta,
			"actualLenDelta", lenDelta, "actualByteSizeDelta", byteSizeDelta)
	}

	mq.len.Store(newLen)
	mq.byteSize.Store(newByteSize)
	mq.reconcileShardStats(lenDelta, byteSizeDelta)
	return newLen, newByteSize, err
}

func (mq *managedQueue) Remove(
	handle types.QueueItemHandle,
) (removedItem types.QueueItemAccessor, newLen uint64, newByteSize uint64, err error) {
	len, byteSize := mq.len.Load(), mq.byteSize.Load()
	removedItem, newLen, newByteSize, err = mq.queue.Remove(handle)

	lenDelta, byteSizeDelta := int64(newLen)-int64(len), int64(newByteSize)-int64(byteSize)
	var expectedLenDelta, expectedByteSizeDelta int64
	if removedItem != nil {
		expectedLenDelta, expectedByteSizeDelta = -1, -int64(removedItem.OriginalRequest().ByteSize())
	}

	if lenDelta != expectedLenDelta || byteSizeDelta != expectedByteSizeDelta {
		mq.logger.V(logutil.DEBUG).Info("Inconsistent queue stats after Remove",
			"expectedLenDelta", expectedLenDelta, "expectedByteSizeDelta", expectedByteSizeDelta,
			"actualLenDelta", lenDelta, "actualByteSizeDelta", byteSizeDelta)
	}

	mq.len.Store(newLen)
	mq.byteSize.Store(newByteSize)
	mq.reconcileShardStats(lenDelta, byteSizeDelta)

	// TODO: If empty, signal shard for optimistic instance cleanup.
	return removedItem, newLen, newByteSize, err
}

func (mq *managedQueue) Cleanup(predicate framework.PredicateFunc) (cleanedItems []types.QueueItemAccessor, err error) {
	// And so on...
	return nil, nil
}

func (mq *managedQueue) Drain() ([]types.QueueItemAccessor, error) {
	// And so on...
	return nil, nil
}

func (mq *managedQueue) Name() string { return mq.queue.Name() }
func (mq *managedQueue) Capabilities() []framework.QueueCapability { return mq.queue.Capabilities() }
func (mq *managedQueue) Len() int { return mq.queue.Len() }
func (mq *managedQueue) ByteSize() uint64 { return mq.queue.ByteSize() }
func (mq *managedQueue) PeekHead() (types.QueueItemAccessor, error) { return mq.queue.PeekHead() }
func (mq *managedQueue) FlowSpec() types.FlowSpecification { return mq.flowSpec }

var _ ports.ManagedQueue = &managedQueue{}

// --- flowQueueAccessorImpl ---

// TODO vend the flow queue accessor via delegation
