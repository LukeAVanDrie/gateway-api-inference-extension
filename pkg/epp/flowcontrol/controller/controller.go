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

// Package controller contains a concrete implementation of the FlowController engine responsible for orchestrating
// the flow control framework with its pluggabel policies and queues.
package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/controller/internal/item"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/controller/internal/processor"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/ports"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const (
	// Default system values if not provided in config
	defaultQueueTTL              = 30 * time.Second
	defaultExpiryCleanupInterval = 1 * time.Second
)

type shardProcessor interface {
	Enqueue(item *item.FlowItem)
	Run(ctx context.Context)
}

type shardProcessorFactory func(
	shard ports.RegistryShard,
	saturationDetector ports.SaturationDetector,
	clock processor.Clock,
	expiryCleanupInterval time.Duration,
	logger logr.Logger) shardProcessor

// realClock implements the processor.Clock interface using the actual system
// time.
type realClock struct{}

var _ processor.Clock = realClock{}

func (c realClock) Now() time.Time { return time.Now() }

type FlowController struct {
	shard              ports.RegistryShard // TODO: replace with future ports.ShardProvider once we support more than one shard.
	saturationDetector ports.SaturationDetector
	logger             logr.Logger
	cfg                Config

	// For dependency injection in tests.
	clock                 processor.Clock
	shardProcessorFactory shardProcessorFactory

	enqueueChan chan *item.FlowItem
	stopCh      chan struct{}
	onceStop    sync.Once
	wg          sync.WaitGroup // Used to wait for the shardProcessor goroutine to exit.
}

func NewFlowController(
	shard ports.RegistryShard,
	saturationDetector ports.SaturationDetector,
	logger logr.Logger,
	cfg Config,
) (*FlowController, error) {
	if shard == nil {
		return nil, fmt.Errorf("RegistryShard cannot be nil")
	}
	if saturationDetector == nil {
		return nil, fmt.Errorf("SaturationDetector cannot be nil")
	}

	if cfg.DefaultQueueTTL <= 0 {
		cfg.DefaultQueueTTL = defaultQueueTTL
	}
	if cfg.ExpiryCleanupInterval <= 0 {
		cfg.ExpiryCleanupInterval = defaultExpiryCleanupInterval
	}

	return &FlowController{
		shard:              shard,
		saturationDetector: saturationDetector,
		logger:             logger,
		cfg:                cfg,

		clock: realClock{},
		shardProcessorFactory: func(
			shard ports.RegistryShard,
			saturationDetector ports.SaturationDetector,
			clock processor.Clock,
			expiryCleanupInterval time.Duration,
			logger logr.Logger) shardProcessor {
			return processor.NewShardProcessor(shard, saturationDetector, clock, expiryCleanupInterval, logger)
		},

		stopCh:      make(chan struct{}),
		enqueueChan: make(chan *item.FlowItem),
	}, nil
}

func (fc *FlowController) EnqueueAndWait(req types.FlowControlRequest) (types.QueueOutcome, error) {
	if req == nil {
		return types.QueueOutcomeRejectedOther, fmt.Errorf("%w: %w", types.ErrRejected, types.ErrNilRequest)
	}
	if req.FlowID() == "" {
		return types.QueueOutcomeRejectedOther, fmt.Errorf("%w: %w", types.ErrRejected, types.ErrFlowIDEmpty)
	}

	effectiveTTL := req.InitialEffectiveTTL()
	if effectiveTTL <= 0 {
		effectiveTTL = fc.cfg.DefaultQueueTTL
	}

	item := item.NewFlowItem(req, effectiveTTL, fc.clock.Now())

	logger := log.FromContext(item.OriginalRequest().Context()).WithName("EnqueueAndWait").WithValues(
		"flowID", item.OriginalRequest().FlowID(), "reqID", item.OriginalRequest().ID(),
		"reqByteSize", item.OriginalRequest().ByteSize(), "effectiveTTL", item.EffectiveTTL(),
		"enqueueTime", item.EnqueueTime())

	// TODO: TTL is effective from the moment item.NewFlowItem is called (even if not yet picked up by the shard
	// processor and added to a queue. Can we catch TLL expiry *before* enqueuing an item? Similar for external context
	// expiry. We check it once directly below, but not periodically; i.e., we don't monitor it.

	select {
	case <-req.Context().Done():
		err := fmt.Errorf("%w: %w: %w", types.ErrRejected, types.ErrContextCancelled, req.Context().Err())
		logger.V(logutil.VERBOSE).Info("Request context cancelled before submission to shard processor; rejecting request.",
			"error", err)
		item.Finalize(types.QueueOutcomeEvictedContextCancelled, err)
		return item.FinalState()
	case <-fc.stopCh:
		err := fmt.Errorf("%w: %w", types.ErrRejected, types.ErrFlowControllerShutdown)
		logger.V(1).Info("FlowController shutting down before submission to shard processor; rejecting request.",
			"error", err)
		item.Finalize(types.QueueOutcomeRejectedOther, err)
		return item.FinalState()
	case fc.enqueueChan <- item:
		logger.V(logutil.VERBOSE).Info("Item submitted FlowController's enqueue channel.")
	}

	select {
	case <-req.Context().Done():
		<-item.Done
		outcome, err := item.FinalState()
		logger.V(logutil.VERBOSE).Info("Request context cancelled while item managed by shard processor.",
			"outcome", outcome, "error", err, "originalContextError", req.Context().Err())
		return outcome, err
	case <-fc.stopCh:
		<-item.Done
		outcome, err := item.FinalState()
		logger.V(logutil.VERBOSE).Info("FlowController shutting down while item managed by shard processor.",
			"outcome", outcome, "error", err)
		return outcome, err
	case <-item.Done:
		// TODO: I don't like item.Done being a public field. Can we encapsulate this in a method and still have the
		// blocking receive work? Perhaps this select needs to be in a loop (we could also hook in expiry checks here
		// with this methodology -- though shard processor has its own cleanup loop that scans and cleans up expired
		// items).
		outcome, err := item.FinalState()
		logger.V(logutil.VERBOSE).Info("Item processing finalized by shard processor.", "outcome", outcome, "error", err)
		return outcome, err
	}
}

func (fc *FlowController) Run(ctx context.Context) {
	fc.logger.V(logutil.VERBOSE).Info("FlowController Run loop starting.")
	defer func() {
		fc.logger.V(logutil.VERBOSE).Info("FlowController Run loop stopped.")
		fc.signalStop()
		fc.wg.Wait()
	}()

	// TODO: once we support multiple shards, this will be responsible for dynamically updating the worker pool based on
	// the shards exposed from the registry.
	shardProcessor := fc.shardProcessorFactory(fc.shard, fc.saturationDetector, fc.clock, fc.cfg.ExpiryCleanupInterval,
		fc.logger)

	fc.wg.Add(1)
	go func() {
		defer fc.wg.Done()
		shardProcessor.Run(ctx)
	}()

	for {
		select {
		case <-ctx.Done():
			fc.logger.V(logutil.VERBOSE).Info("FlowController Run context cancelled, initiating shutdown of processor.")
			return
		case item := <-fc.enqueueChan:
			// TODO: once we support multiple shards, this will be responsible for distributing items across shards following
			// a "Join Shortest Queue by Bytes" algorithm. Currently, we only support a single shard, so we can enqueue
			// directly.

			// TODO: Think carefully about whether this should be blocking if the internal buffer enqueue channel is full.
			// Right now, we use a goroutine here to not blcok the main flow controller Run loop in addition to buffering the
			// shard processor's internal enqueue channel.
			go func() {
				shardProcessor.Enqueue(item)
				logger := log.FromContext(item.OriginalRequest().Context()).WithValues(
					"flowID", item.OriginalRequest().FlowID(), "reqID", item.OriginalRequest().ID(),
					"byteSize", item.OriginalRequest().ByteSize(), "effectiveTTL", item.EffectiveTTL(),
					"enqueueTime", item.EnqueueTime())
				logger.V(logutil.VERBOSE).Info("Item submitted to shard processor's enqueue channel.")
			}()
		}
	}
}

func (fc *FlowController) signalStop() {
	fc.onceStop.Do(func() {
		close(fc.stopCh)
	})
}
