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

package processor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/controller/internal/item"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/ports"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

// Clock defines an interface for getting the current time.
type Clock interface {
	Now() time.Time
}

type ShardProcessor struct {
	shard  ports.RegistryShard
	saturationDetector ports.SaturationDetector
	clock  Clock
	expiryCleanupInterval time.Duration
	logger logr.Logger

	enqueueChan chan *item.FlowItem
	wg     sync.WaitGroup  // Used to wait for background tasks like expiry cleanup.
}

func NewShardProcessor(
	shard ports.RegistryShard,
	saturationDetector ports.SaturationDetector,
	clock Clock,
	expiryCleanupInterval time.Duration,
	logger logr.Logger,
) *ShardProcessor {
	return &ShardProcessor{
		shard:  shard,
		saturationDetector: saturationDetector,
		clock: clock,
		expiryCleanupInterval: expiryCleanupInterval,
		logger: logger,
		enqueueChan: make(chan *item.FlowItem, 100), // We don't want ShardProcessor.Enqueue() calls to be blocking.
	}
}

func (sp *ShardProcessor) Run(ctx context.Context) {
	sp.logger.V(logutil.VERBOSE).Info("Shard processor run loop starting.")
	defer sp.logger.V(logutil.VERBOSE).Info("Shard processor run loop stopped.")

	sp.wg.Add(1)
	go sp.runExpiryCleanup(ctx)

	for {
		select {
		case <-ctx.Done():
			sp.logger.V(logutil.VERBOSE).Info("Context cancelled, shard processor shutting down.")
			sp.evictAll()
			sp.wg.Wait()
			return
		case item, ok := <- sp.enqueueChan:
			if !ok { // Should not happen in practice.
				sp.logger.V(logutil.VERBOSE).Info("Enqueue channel closed, shard processor shutting down.")
				sp.evictAll()
				sp.wg.Wait()
				return
			}
			if item == nil {
				sp.logger.Error(nil, "Nil item received on shard processor enqueue channel, ignoring.")
				continue
			}
			sp.enqueue(item)
			sp.dispatchCycle()
		default:
			if !sp.dispatchCycle() {
				// Short pause to prevent busy looping.
				// TODO: Should this be configurable? Should we use a backoff mechanism?
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
}

func (sp *ShardProcessor) Enqueue(item *item.FlowItem) {
	sp.enqueueChan <- item
}

func (sp *ShardProcessor) enqueue(item *item.FlowItem) {
	logger := log.FromContext(item.OriginalRequest().Context()).WithName("enqueue").WithValues(
		"flowID", item.OriginalRequest().FlowID(),
		"reqID", item.OriginalRequest().ID(),
		"reqByteSize", item.OriginalRequest().ByteSize(),
	)

	managedQ, err := sp.shard.ManagedQueue(item.OriginalRequest().FlowID())
	if err != nil {
		logger.Error(err, "Failed to get ManagedQueue for flow; rejecting item.")
		item.Finalize(types.QueueOutcomeRejectedOther, fmt.Errorf("%w: %w", types.ErrRejected, err))
		return
	}
	priority := managedQ.FlowQueueAccessor().FlowSpec().Priority
	logger = logger.WithValues("priority", priority)

	band, err := sp.shard.PriorityBandAccessor(priority)
	if err != nil {
		logger.Error(err, "Failed to get PriorityBandAccessor for priority; rejecting item.")
		item.Finalize(types.QueueOutcomeRejectedOther, fmt.Errorf("%w: %w", types.ErrRejected, err))
	}
	logger = logger.WithValues("priorityName", band.PriorityName())

	if !sp.hasCapacity(priority, item.OriginalRequest().ByteSize()) {
		logger.Error(nil, "At capacity; rejecting item.")
		item.Finalize(types.QueueOutcomeRejectedCapacity, fmt.Errorf("%w: %w", types.ErrRejected, types.ErrQueueAtCapacity))
		return
	}

	// Optimistic defensive measure against race conditions before queue mutation.
	// Not stricly necessary since our expiry cleanup loop would catch this later.
	if item.IsFinalized() {
		logger.V(logutil.VERBOSE).Info("Item finalized before adding to ManagedQueue.")
		return
	}

	_, _, err = managedQ.Add(item)
	if err != nil {
		logger.Error(err, "Failed to add item to ManagedQueue.")
		item.Finalize(types.QueueOutcomeRejectedOther, fmt.Errorf("%w: %w", types.ErrRejected, err))
		return
	}
	logger.V(logutil.VERBOSE).Info("Item enqueued to ManagedQueue.")
}

func (sp *ShardProcessor) hasCapacity (priority uint, itemByteSize uint64) bool {
	if itemByteSize == 0 {
		return true
	}
	stats := sp.shard.Stats()
	if stats.TotalCapacityBytes > 0 && stats.TotalByteSize + itemByteSize > stats.TotalCapacityBytes {
		return false
	}
	bandStats := stats.PerPriorityBandStats[priority] // relying on documented representation guarantees
	return bandStats.ByteSize + itemByteSize <= bandStats.CapacityBytes
}

func (sp *ShardProcessor) dispatchCycle() bool {
	baseLogger := sp.logger.WithName("dispatchCycle")

	for _, priority := range sp.shard.AllOrderedPriorityLevels() {
		band, err := sp.shard.PriorityBandAccessor(priority)
		if err != nil {
			baseLogger.Error(err, "Failed to get PriorityBandAccessor for priority; skipping.")
		}
		logger := baseLogger.WithValues("priority", priority, "priorityName", band.PriorityName())

		if sp.saturationDetector.IsSaturated() {
			logger.V(logutil.VERBOSE).Info("System saturated, pausing dispatch for this shard.")
			return false
		}

		item, err := sp.applyDispatchPolicies(band, logger)
		if err != nil {
			logger.Error(err, "Failed to apply dispatch policies; skipping band.")
			continue
		}
		if item == nil {
			logger.V(logutil.VERBOSE).Info("No item selected by dispatch policies; skipping band.")
			continue
		}
		logger = logger.WithValues("flowID", item.OriginalRequest().FlowID(), "reqID", item.OriginalRequest().ID())

		if err := sp.dispatchItem(item, logger); err != nil {
			logger.Error(err, "Failed to dispatch item.")
			return false
		}
		return true
	}
	return false
}

func (sp *ShardProcessor) applyDispatchPolicies(band framework.PriorityBandAccessor, logger logr.Logger) (types.QueueItemAccessor, error) {
	interP, err := sp.shard.InterFlowDispatchPolicy(band.Priority())
	if err != nil {
		return nil, errors.New("failed to get InterFlowDispatchPolicy for priority")
	}
	queue, err := interP.SelectQueue(band)
	if err != nil {
		return nil, fmt.Errorf("failed to apply InterFlowDispatchPolicy for priority: %w", err)
	}
	if queue == nil {
		logger.V(logutil.VERBOSE).Info("No queue selected by InterFlowDispatchPolicy for priority.")
		return nil, nil
	}
	logger = logger.WithValues("selectedFlowID", queue.FlowSpec().ID)

	intraP, err := sp.shard.IntraFlowDispatchPolicy(queue.FlowSpec().ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get IntraFlowDispatchPolicy for flow: %w", err)
	}
	item, err := intraP.SelectItem(queue)
	if err != nil {
		return nil, fmt.Errorf("failed to apply IntraFlowDispatchPolicy for flow: %w", err)
	}
	if item == nil {
		logger.V(logutil.VERBOSE).Info("No item selected by IntraFlowDispatchPolicy for flow.")
		return nil, nil
	}
	return item, nil
}

func (sp *ShardProcessor) dispatchItem(itemAcc types.QueueItemAccessor, logger logr.Logger) error {
	logger = logger.WithName("dispatchItem")

	managedQ, err := sp.shard.ManagedQueue(itemAcc.OriginalRequest().FlowID())
	if err != nil {
		return fmt.Errorf("failed to get ManagedQueue for flow: %w", err)
	}

	removedItemAcc, _, _, err := managedQ.Remove(itemAcc.Handle())
	if err != nil {
		return fmt.Errorf("failed to remove item from ManagedQueue: %w", err)
	}

	removedItem, ok := removedItemAcc.(*item.FlowItem) // This should be the same as itemAcc.
	if !ok {
		panic(fmt.Sprintf("item %s of type %T is not an *item.FlowItem", removedItemAcc.OriginalRequest().ID(),
			removedItemAcc))
	}

	isExpired, outcome, expiryErr := checkItemExpiry(removedItem, sp.clock.Now())
	if isExpired {
		logger.V(logutil.VERBOSE).Info("Item found to be expired/cancelled at time of dispatch.", "outcome", outcome,
			"expiryErr", expiryErr)
		removedItem.Finalize(outcome, fmt.Errorf("%w: %w", types.ErrEvicted, expiryErr))
		return fmt.Errorf("item expired before dispatch: %w", expiryErr)
	}

	removedItem.Finalize(types.QueueOutcomeDispatched, nil)
	logger.V(logutil.VERBOSE).Info("Item dispatched.")
	return nil
}

func checkItemExpiry(itemAcc types.QueueItemAccessor, now time.Time) (bool, types.QueueOutcome, error) {
	item, ok := itemAcc.(*item.FlowItem)
	if !ok {
		panic(fmt.Sprintf("item %s of type %T is not an *item.FlowItem", itemAcc.OriginalRequest().ID(), itemAcc))
	}

	// This shouldn't happen in practice if finalization is behaving properly, but it is an important defensive measure.
	if item.IsFinalized() {
		outcome, err := item.FinalState()
		return true, outcome, err
	}

	if ctxErr := item.OriginalRequest().Context().Err(); ctxErr != nil {
		return true, types.QueueOutcomeEvictedContextCancelled, fmt.Errorf("%w: %w", types.ErrContextCancelled, ctxErr)
	}

	if item.EffectiveTTL() > 0 && now.Sub(item.EnqueueTime()) > item.EffectiveTTL() {
		return true, types.QueueOutcomeEvictedTTL, types.ErrTTLExpired
	}
	return false, types.QueueOutcomeNotYetFinalized, nil
}

func (sp *ShardProcessor) runExpiryCleanup(ctx context.Context) {
	defer sp.wg.Done()
	logger := sp.logger.WithName("runExpiryCleanup")
	logger.Info("Shard expiry cleanup goroutine starting.")
	defer logger.Info("Shard expiry cleanup goroutine stopped.")

	ticker := time.NewTicker(sp.expiryCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			sp.cleanupExpired(now, logger)
		}
	}
}

func (sp *ShardProcessor) cleanupExpired(now time.Time, logger logr.Logger) {
	logger = logger.WithName("cleanupExpired")
	var bandWg sync.WaitGroup

	for _, priority := range sp.shard.AllOrderedPriorityLevels() {
		bandWg.Add(1)
		go func(p uint) {
			defer bandWg.Done()
			bandLogger := logger.WithValues("priority", priority)
			band, err := sp.shard.PriorityBandAccessor(priority)
			if err != nil {
				logger.Error(err, "Failed to get PriorityBandAccessor for expiry cleanup.")
				return
			}
			bandLogger = logger.WithValues("priorityName", band.PriorityName())

			var queueWg sync.WaitGroup
			band.IterateQueues(func(queue framework.FlowQueueAccessor) bool {
				queueWg.Add(1)
				go func (q framework.FlowQueueAccessor) {
					defer queueWg.Done()
					queueLogger := bandLogger.WithValues("flowID", q.FlowSpec().ID)
					managedQ, err := sp.shard.ManagedQueue(q.FlowSpec().ID)
					if err != nil {
						queueLogger.Error(err, "Failed to get ManagedQueue for expiry cleanup.")
						return
					}

					predicate := func(item types.QueueItemAccessor) bool {
						isExpired, _, _ := checkItemExpiry(item, now)
						return isExpired
					}

					removedItems, err := managedQ.Cleanup(predicate)
					if err != nil {
						queueLogger.Error(err, "Error during ManagedQueue Cleanup.")
					}

					for _, i := range removedItems {
						item, ok := i.(*item.FlowItem)
						if !ok {
							panic(fmt.Sprintf("item %s of type %T is not an *item.FlowItem", i.OriginalRequest().ID(), i))
						}
						_, outcome, expiryErr := checkItemExpiry(i, now)
						item.Finalize(outcome, fmt.Errorf("%w: %w", types.ErrEvicted, expiryErr))
						queueLogger.V(logutil.VERBOSE).Info("Item evicted during expiry cleanup.", "reqID", item.OriginalRequest().ID(),
							"outcome", outcome, "expiryErr", expiryErr)
					}
				}(queue)
				return true // We swallow errors, so always keep iterating.
			})
			queueWg.Wait()
		}(priority)
		bandWg.Wait()
	}
}

func (sp *ShardProcessor) evictAll() {
	logger := sp.logger.WithName("evictAll")
	var bandWg sync.WaitGroup

	for _, priority := range sp.shard.AllOrderedPriorityLevels() {
		bandWg.Add(1)
		go func(p uint) {
			defer bandWg.Done()
			bandLogger := logger.WithValues("priority", priority)
			band, err := sp.shard.PriorityBandAccessor(priority)
			if err != nil {
				logger.Error(err, "Failed to get PriorityBandAccessor for evicting all.")
				return
			}
			bandLogger = logger.WithValues("priorityName", band.PriorityName())

			var queueWg sync.WaitGroup
			band.IterateQueues(func(queue framework.FlowQueueAccessor) bool {
				queueWg.Add(1)
				go func (q framework.FlowQueueAccessor) {
					defer queueWg.Done()
					queueLogger := bandLogger.WithValues("flowID", q.FlowSpec().ID)
					managedQ, err := sp.shard.ManagedQueue(q.FlowSpec().ID)
					if err != nil {
						queueLogger.Error(err, "Failed to get ManagedQueue for evicting all.")
						return
					}

					removedItems, err := managedQ.Drain()
					if err != nil {
						queueLogger.Error(err, "Error during ManagedQueue Drain.")
					}

					for _, i := range removedItems {
						item, ok := i.(*item.FlowItem)
						if !ok {
							panic(fmt.Sprintf("item %s of type %T is not an *item.FlowItem", i.OriginalRequest().ID(), i))
						}
						item.Finalize(types.QueueOutcomeEvictedOther, fmt.Errorf("%w: %w", types.ErrEvicted,
							types.ErrFlowControllerShutdown))
						queueLogger.V(logutil.VERBOSE).Info("Item evicted.", "reqID", item.OriginalRequest().ID())
					}
				}(queue)
				return true // We swallow errors, so always keep iterating.
			})
			queueWg.Wait()
		}(priority)
		bandWg.Wait()
	}
}
