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

// Package flowcontroller implements the core logic for managing and controlling the flow of requests.
package flowcontroller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/config"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

// clock defines an interface for getting the current time, allowing for time manipulation in tests.
type clock interface {
	// Now returns the current time.
	Now() time.Time
}

// realClock implements the clock interface using the actual system time.
type realClock struct{}

// Now returns the current system time.
func (c realClock) Now() time.Time { return time.Now() }

// capacityFailureReason indicates the reason why a capacity check failed.
type capacityFailureReason int

const (
	// capacityFailureReasonNone indicates no capacity failure (i.e., has capacity).
	capacityFailureReasonNone capacityFailureReason = iota
	// capacityFailureReasonGlobalLimitExceeded indicates the global byte limit was exceeded.
	capacityFailureReasonGlobalLimitExceeded
	// capacityFailureReasonBandLimitExceeded indicates a priority band's byte limit was exceeded.
	capacityFailureReasonBandLimitExceeded
	// capacityFailureReasonBandConfigError indicates an error retrieving band configuration (e.g., its accessor or
	// capacity limit from the FlowRegistry).
	capacityFailureReasonBandConfigError
)

// String returns a human-readable string representation of the capacityFailureReason.
func (cfr capacityFailureReason) String() string {
	switch cfr {
	case capacityFailureReasonNone:
		return "None"
	case capacityFailureReasonGlobalLimitExceeded:
		return "GlobalLimitExceeded"
	case capacityFailureReasonBandLimitExceeded:
		return "BandLimitExceeded"
	case capacityFailureReasonBandConfigError:
		return "BandConfigError"
	default:
		return "Unknown"
	}
}

// SaturationDetector provides a signal indicating whether the backends are considered saturated.
// Conformance:
//   - Implementations MUST be goroutine-safe.
type SaturationDetector interface {
	// IsSaturated returns true if the system (e.g., backend model servers) is considered saturated according to its
	// configured thresholds and observed metrics. The FlowController uses this to gate dispatch decisions.
	IsSaturated() bool
}

// FlowController manages the queuing, prioritization, fairness, and dispatch of requests based on configured policies
// and system saturation state.
// It is designed to be a portable library for flow control logic.
//
// The primary interaction point for submitting requests is EnqueueAndWait.
// The controller's processing loops are started via the Run method.
//
// == Error Handling Strategy for Priority Band and Flow Queue Iteration ==
//
// The FlowController employs a two-tiered error handling strategy during operations like request dispatch and
// preemption, which involve iterating through priority bands and the flow queues within them:
//
// 1. Priority Band Domain (Inter-Flow Operations - "Fail Open for System"):
//   - Behavior: If an issue prevents processing at the priority band level itself—such as failing to retrieve a band's
//     InterFlowDispatchPolicy from the FlowRegistry, or if an InterFlow...Policy method returns an unrecoverable error
//     for that band—the FlowController will typically log the error, skip processing for that specific priority band in
//     the current operational cycle, and continue to the next available priority band.
//   - Rationale: This "fail open" approach for band-level setup or inter-flow policy execution errors allows the system
//     to attempt to provide service via other healthy bands. In this sense, the system is work-conserving.
//
// 2. Queue Domain (Intra-Flow Operations within a Band - "Fail Close for Band"):
//   - Behavior: Once an InterFlow...Policy successfully selects a specific flow's queue for processing within a band,
//     if an unrecoverable error occurs during this queue-specific (intra-flow) stage—such as failing to retrieve the
//     IntraFlow...Policy for that queue from the FlowRegistry, or if an IntraFlow...Policy method itself returns an
//     unrecoverable error—the FlowController will "fail close" for the *current priority band*. This means it will log
//     the error and cease further attempts to select or process additional queues *from that same band* during the
//     current operational cycle. The overall operation then moves to the next priority band, if any.
//   - Rationale: Inter-flow policies may be stateless and lack a feedback mechanism to know if a previously selected
//     queue encountered an intra-flow processing issue.
//     This strategy prevents potential loops where an inter-flow policy might repeatedly select problematic queues
//     within a band where intra-flow processing consistently fails.
//
// 3. Invariant Violations: Critical internal inconsistencies or invariant violations (e.g., an item retrieved from a queue
// not being the expected internal `*flowItem` type) will result in a `panic` to immediately signal a severe bug.
//
// This tiered strategy aims to maximize system robustness and work conservation by isolating failures, while preventing
// cascading issues or processing loops due to problematic configurations or plugin behaviors at different levels.
type FlowController struct {
	registry           types.FlowRegistry
	saturationDetector SaturationDetector
	clock              clock
	logger             logr.Logger

	// config holds the operational configuration for the FlowController.
	config config.FlowControllerConfig

	// enqueueChan is an unbuffered channel for submitting new flowItems to the Run loop.
	enqueueChan chan *flowItem

	// stopCh is closed to signal the Run loop and other goroutines to terminate.
	stopCh   chan struct{}
	onceStop sync.Once // Ensures stopCh is closed only once.

	// wg is used to wait for background goroutines (like expiry cleanup) to finish.
	wg sync.WaitGroup
}

// NewFlowController creates and initializes a new FlowController instance.
// The FlowRegistry provided should already be configured with its priority bands, policies, and queue types.
// Per-priority band capacity limits are sourced from the FlowRegistry via types.PriorityBandAccessor.
func NewFlowController(
	detector SaturationDetector,
	registry types.FlowRegistry,
	cfg config.FlowControllerConfig,
	logger logr.Logger,
) (*FlowController, error) {
	if detector == nil {
		return nil, fmt.Errorf("SaturationDetector cannot be nil")
	}
	if registry == nil {
		return nil, fmt.Errorf("FlowRegistry cannot be nil")
	}

	configCopy := cfg // Operate on a copy
	// Validate and apply defaults to the FlowControllerConfig
	if err := (&configCopy).ValidateAndApplyDefaults(logger.WithName("fc-config")); err != nil {
		return nil, fmt.Errorf("invalid FlowControllerConfig: %w", err)
	}

	fc := &FlowController{
		registry:           registry,
		saturationDetector: detector,
		clock:              realClock{},
		logger:             logger.WithName("flow-controller"),
		config:             configCopy,
		enqueueChan:        make(chan *flowItem),
		stopCh:             make(chan struct{}),
	}

	fc.logger.V(logutil.DEFAULT).Info("FlowController initialized",
		"defaultQueueTTL", fc.config.DefaultQueueTTL,
		"expiryCleanupInterval", fc.config.ExpiryCleanupInterval,
		"maxGlobalBytes", fc.config.MaxGlobalBytes,
	)
	return fc, nil
}

// EnqueueAndWait submits a request for flow control management and blocks until the request's processing is finalized
// within the FlowController.
// Finalization means the request has reached a terminal state, which can be:
//   - Unblocking of the calling goroutine, signifying the request has passed flow control checks and can proceed to the
//     next processing stage (managed by the caller).
//   - Rejection before or during enqueueing (e.g., due to capacity limits, invalid flow, or FlowController shutdown).
//   - Eviction from a queue after being enqueued (e.g., due to TTL expiry, preemption, or cancellation of the request's
//     context).
//   - FlowController shutdown while the request is being managed.
//
// This method is goroutine-safe and is the primary entry point for requests
// into the FlowController.
//
// Parameters:
//   - req: The FlowControlRequest to be processed. Must not be nil.
//     The request's Context() is monitored for cancellation throughout its lifecycle within the FlowController.
//
// Returns:
//   - types.QueueOutcome: Indicates the final status of the request's lifecycle.
//   - error: Non-nil if the request was not successfully unblocked for further processing by the caller.
//     The error will always wrap either types.ErrRejected or types.ErrEvicted. Callers can use errors.Is() to check for
//     these general categories and then unwrap further for specific sentinel errors.
//     If the outcome allows the caller to proceed (e.g. types.QueueOutcomeDispatched), the error will be nil.
//
// Conformance:
//   - If req is nil, returns (types.QueueOutcomeRejectedOther, an error wrapping types.ErrRejected and
//     types.ErrNilRequest).
//   - If req.FlowID() is empty, returns (types.QueueOutcomeRejectedOther, an error wrapping types.ErrRejected and
//     types.ErrFlowIDEmpty).
func (fc *FlowController) EnqueueAndWait(req types.FlowControlRequest) (types.QueueOutcome, error) {
	if req == nil {
		return types.QueueOutcomeRejectedOther, fmt.Errorf("%w: %w", types.ErrRejected, types.ErrNilRequest)
	}
	if req.FlowID() == "" {
		return types.QueueOutcomeRejectedOther, fmt.Errorf("%w: %w", types.ErrRejected, types.ErrFlowIDEmpty)
	}

	effectiveTTL := req.InitialEffectiveTTL()
	if effectiveTTL <= 0 {
		effectiveTTL = fc.config.DefaultQueueTTL
	}

	item := newFlowItem(req, effectiveTTL, fc.clock.Now())

	logger := log.FromContext(req.Context()).WithName("EnqueueAndWait").WithValues(
		"flowID", item.FlowID(),
		"reqID", item.RequestID(),
		"reqByteSize", item.ByteSize(),
		"effectiveTTL", item.EffectiveTTL(),
	)

	// Submit the item to the Run loop's enqueueChan.
	logger.V(logutil.DEBUG).Info("Attempting to submit item to FlowController's internal channel.")
	select {
	case <-req.Context().Done():
		// Request context cancelled before it could be submitted to the internal channel.
		err := fmt.Errorf("%w: %w: %w", types.ErrRejected, types.ErrContextCancelled, req.Context().Err())
		logger.V(logutil.VERBOSE).Info("Request context cancelled before submission.", "error", err)
		item.finalize(types.QueueOutcomeRejectedOther, err)
		return item.getFinalState()
	case <-fc.stopCh:
		// FlowController is shutting down before the item could be submitted.
		err := fmt.Errorf("%w: %w", types.ErrRejected, types.ErrFlowControllerShutdown)
		logger.V(logutil.VERBOSE).Info("FlowController shutting down before submission.", "error", err)
		item.finalize(types.QueueOutcomeRejectedOther, err)
		return item.getFinalState()
	case fc.enqueueChan <- item:
		logger.V(logutil.DEBUG).Info("Item submitted to internal enqueue channel.")
	}

	// Wait for the item to be finalized by the Run loop (dispatched or evicted).
	logger.V(logutil.DEBUG).Info("Item submitted, waiting for finalization by Run loop.")
	select {
	case <-req.Context().Done():
		// Request context cancelled while the item was (or was about to be) managed by the FlowController.
		// The Run loop's expiry cleanup or dispatch logic should eventually detect this context cancellation and finalize
		// the item. We must wait for item.done.
		err := fmt.Errorf("%w: %w: %w", types.ErrEvicted, types.ErrContextCancelled, req.Context().Err())
		logger.V(logutil.VERBOSE).Info("Request context cancelled while item was managed.", "error", err)
		<-item.done // Wait for FC to finalize
		// It's possible item.finalize was called with a different reason if multiple conditions met.
		// getFinalState will return whatever was set first.
		return item.getFinalState()
	case <-fc.stopCh:
		// FlowController shut down while the item was being managed.
		// The Run loop's shutdown logic (evictAllOnShutdown) should finalize the item.
		logger.V(logutil.VERBOSE).Info("FlowController shutting down while item was managed.")
		<-item.done // Wait for FC to finalize
		return item.getFinalState()
	case <-item.done:
		// Item processing finished (dispatched or evicted by Run loop).
		outcome, err := item.getFinalState()
		if err == nil && outcome == types.QueueOutcomeDispatched {
			logger.V(logutil.VERBOSE).Info("Request processing completed: Dispatched.")
		} else {
			logger.V(logutil.VERBOSE).Info("Request processing completed.", "outcome", outcome.String(), "error", err)
		}
		return outcome, err
	}
}

// Run starts the FlowController's main processing loops. This method blocks until the provided context is cancelled.
//
// The Run loop is responsible for orchestrating request processing. It interleaves the acceptance of new requests (from
// an unbuffered internal channel fed by EnqueueAndWait) with attempts to dispatch eligible requests from the managed
// queues. This interleaving is designed for contention management and responsiveness under load.
// The loop also manages periodic tasks like queue item expiry cleanup.
//
// It is intended to be called once, typically in its own goroutine.
//
// Parameters:
//   - ctx: A context used to signal shutdown. Upon context cancellation, Run will initiate a graceful shutdown,
//     finalizing pending requests.
func (fc *FlowController) Run(ctx context.Context) {
	fc.logger.V(logutil.DEFAULT).Info("FlowController Run loop starting.")
	defer func() {
		fc.logger.Info("FlowController Run loop stopped.")
		fc.signalStop() // Ensure stopCh is closed to signal any pending EnqueueAndWait calls.
		fc.wg.Wait()    // Wait for background goroutines to complete.
	}()

	fc.wg.Add(1) // For the expiry cleanup goroutine
	go fc.runExpiryCleanup(ctx)

	// Main processing loop:
	for {
		select {
		case <-ctx.Done():
			fc.logger.V(logutil.DEFAULT).Info("Context cancelled, initiating FlowController shutdown.")
			fc.evictAllOnShutdown(fmt.Errorf("%w: context cancelled", types.ErrEvicted), types.QueueOutcomeEvictedOther)
			return
		case <-fc.stopCh: // Typically handled by ctx.Done() in defer, but good practice
			fc.logger.V(logutil.DEFAULT).Info("Internal stop signal received, initiating FlowController shutdown from stopCh.")
			fc.evictAllOnShutdown(
				fmt.Errorf("%w: %w", types.ErrEvicted, types.ErrFlowControllerShutdown),
				types.QueueOutcomeEvictedOther)
			return
		case item, ok := <-fc.enqueueChan:
			if !ok { // Should not happen if fc.stopCh is handled, but good practice
				fc.logger.V(logutil.DEFAULT).Info("Enqueue channel closed, initiating shutdown.")
				fc.evictAllOnShutdown(
					fmt.Errorf("%w: enqueue channel closed unexpectedly", types.ErrEvicted),
					types.QueueOutcomeEvictedOther)
				return
			}
			if item == nil { // Should not happen, fail open
				fc.logger.Error(errors.New("nil item received from enqueueChan"), "Nil item received, ignoring.")
				continue
			}
			fc.handleEnqueue(item)    // Process the newly submitted item
			fc.attemptDispatchCycle() // After handling an enqueue, immediately try to dispatch
		default:
			dispatched := fc.attemptDispatchCycle()
			if !dispatched { // Short pause to prevent busy looping; TODO(lukevandrie): should this be configurable?
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
}

// signalStop idempotently closes the stopCh.
func (fc *FlowController) signalStop() {
	fc.onceStop.Do(func() {
		close(fc.stopCh)
	})
}

// handleEnqueue processes a single flowItem received from the enqueueChan.
// This method orchestrates the admission control logic for a new request:
//  1. Retrieves the active ManagedQueue for the item's flow.
//  2. Checks if the system and the item's target priority band have capacity.
//  3. If capacity limits are hit (specifically global, not the item's own band), it attempts preemption of items from
//     lower priority bands.
//  4. If the item can be accommodated (either initially or after preemption), it's added to its ManagedQueue.
//  5. If any step fails (e.g., no active queue, capacity check error, capacity full and preemption fails/inapplicable),
//     the item is finalized with an appropriate rejection outcome and error.
func (fc *FlowController) handleEnqueue(item *flowItem) {
	reqCtx := item.OriginalRequest().Context()
	logger := log.FromContext(reqCtx).WithName("handleEnqueue").WithValues(
		"flowID", item.FlowID(),
		"reqID", item.RequestID(),
		"reqByteSize", item.ByteSize(),
	)

	managedQ, err := fc.registry.ActiveManagedQueue(item.FlowID())
	if err != nil {
		logger.Error(err, "Failed to get active ManagedQueue for flow; rejecting item.")
		item.finalize(
			types.QueueOutcomeRejectedOther,
			fmt.Errorf("%w: failed to get active ManagedQueue for flow: %w", types.ErrRejected, err))
		return
	}
	logger = logger.WithValues("priority", managedQ.FlowSpec().Priority(), "queueType", managedQ.Name())

	bandAccessor, err := fc.registry.PriorityBandAccessor(managedQ.FlowSpec().Priority())
	if err != nil {
		logger.Error(err, "Failed to get PriorityBandAccessor for item's priority band; rejecting item.")
		item.finalize(
			types.QueueOutcomeRejectedOther,
			fmt.Errorf("%w: failed to get PriorityBandAccessor for item's priority band: %w", types.ErrRejected, err))
		return
	}
	logger = logger.WithValues("priorityName", bandAccessor.PriorityName())

	canFit, reason, err := fc.hasCapacity(bandAccessor, item.ByteSize(), logger)
	if err != nil {
		logger.Error(err, "Failed to check capacity; rejecting item.")
		item.finalize(
			types.QueueOutcomeRejectedOther,
			fmt.Errorf("%w: failed to check capacity: %w", types.ErrRejected, err))
		return
	}

	if !canFit {
		logger.V(logutil.VERBOSE).Info("Capacity limit reached.", "reason", reason.String())
		if reason == capacityFailureReasonBandLimitExceeded {
			logger.V(logutil.VERBOSE).Info("Item's own priority band is at capacity; preemption not possible for this band.")
			item.finalize(types.QueueOutcomeRejectedCapacity, fmt.Errorf("%w: priority band %d ('%s') at capacity: %w",
				types.ErrRejected, managedQ.FlowSpec().Priority(), bandAccessor.PriorityName(), types.ErrQueueAtCapacity))
			return
		}

		madeSpace, preemptionErr := fc.tryPreemptForRequest(item, bandAccessor, logger)
		if !madeSpace {
			finalErr := types.ErrQueueAtCapacity
			if preemptionErr != nil {
				finalErr = fmt.Errorf("%w: preemption failed: %w", types.ErrQueueAtCapacity, preemptionErr)
			}
			logger.V(logutil.VERBOSE).Info("Failed to make space via preemption.", "error", finalErr)
			item.finalize(types.QueueOutcomeRejectedCapacity, fmt.Errorf("%w: %w", types.ErrRejected, finalErr))
			return
		}
		logger.V(logutil.VERBOSE).Info("Space successfully made via preemption.")
	}

	if item.isFinalized() { // Check before adding to queue to avoid enqueuing an already terminal item
		logger.V(logutil.DEBUG).Info("Item finalized concurrently before enqueuing into ManagedQueue.")
		return
	}

	_, _, err = managedQ.Add(item)
	if err != nil {
		logger.Error(err, "Failed to add item to ManagedQueue.")
		item.finalize(
			types.QueueOutcomeRejectedOther,
			fmt.Errorf("%w: failed to add item to ManagedQueue: %w", types.ErrRejected, err))
		return
	}
	logger.V(logutil.DEBUG).Info("Item successfully enqueued into ManagedQueue.")
	// The item is now in the queue; its 'done' channel remains open until it is dispatched or evicted by other mechanisms
	// (expiry, subsequent preemption).
}

// hasCapacity checks if there's sufficient capacity to accommodate an item of a given byte size.
// This check considers both the FlowController's global byte limit (fc.config.MaxGlobalBytes) and the specific
// capacity limit of the target priority band (obtained from bandAccessor.CapacityBytes()).
//
// Parameters:
//   - bandAccessor: The PriorityBandAccessor for the priority band into which the item would be enqueued.
//   - itemByteSize: The byte size of the item for which capacity is being checked.
//   - logger: A contextual logger for detailed logging of capacity decisions.
//
// Returns:
//   - canFit (bool): True if the item can fit according to configured limits.
//   - reason (capacityFailureReason): The reason for failure if canFit is false.
//   - err (error): Any unexpected error encountered during the check (e.g., registry issues).
func (fc *FlowController) hasCapacity(
	bandAccessor types.PriorityBandAccessor,
	itemByteSize uint64,
	logger logr.Logger,
) (canFit bool, reason capacityFailureReason, err error) {
	logger = logger.WithName("hasCapacity")
	if itemByteSize == 0 {
		return true, capacityFailureReasonNone, nil // Zero-size items always "fit" concerning byte limits.
	}

	registryStats := fc.registry.GetStats()

	// 1. Check global capacity limit.
	if fc.config.MaxGlobalBytes > 0 && (registryStats.GlobalByteSize+itemByteSize) > fc.config.MaxGlobalBytes {
		logger.V(logutil.DEBUG).Info("Global capacity limit would be exceeded.",
			"currentGlobalByteSize", registryStats.GlobalByteSize,
			"globalByteLimit", fc.config.MaxGlobalBytes)
		return false, capacityFailureReasonGlobalLimitExceeded, nil
	}

	// 2. Check per-priority band capacity limit.
	bandCapacityLimit := bandAccessor.CapacityBytes()
	currentBandStats, ok := registryStats.PerPriorityBandStats[bandAccessor.Priority()]
	if !ok {
		err := fmt.Errorf("stats not found for priority band %s (%d) in FlowRegistryStats",
			bandAccessor.PriorityName(), bandAccessor.Priority())
		logger.Error(err, "Failed to retrieve stats for priority band.")
		return false, capacityFailureReasonBandConfigError, err
	}

	if bandCapacityLimit > 0 && (currentBandStats.ByteSize+itemByteSize) > bandCapacityLimit {
		logger.V(logutil.DEBUG).Info("Priority band capacity limit would be exceeded.",
			"currentBandByteSize", currentBandStats.ByteSize,
			"bandByteLimit", bandCapacityLimit)
		return false, capacityFailureReasonBandLimitExceeded, nil
	}

	return true, capacityFailureReasonNone, nil
}

// tryPreemptForRequest attempts to make space for 'itemToFit' by preempting items from strictly lower priority bands
// than itemToFit's own band.
// The method iterates through lower priority bands, from the lowest upwards. Within each victim band, it repeatedly
// applies inter-flow and intra-flow preemption policies to select and evict victim items. After each successful
// preemption, it re-evaluates if 'itemToFit' can now be accommodated.
//
// Parameters:
//   - itemToFit: The flowItem for which space needs to be made.
//   - itemToFitBandAccessor: The PriorityBandAccessor for itemToFit's own priority band.
//     This is used to re-check capacity for itemToFit after potential preemptions.
//   - logger: A contextual logger for tracing preemption attempts.
//
// Error Handling Strategy (see FlowController GoDoc for details):
//   - Band-Level/Inter-Policy Issues for a victim band (e.g., error getting PriorityBandAccessor, error
//     getting/executing InterFlowPreemptionPolicy): Skips that victim band and tries the next lower priority band.
//   - Queue-Level/Intra-Policy Issues for a selected victim queue (e.g., error getting/executing
//     IntraFlowPreemptionPolicy): Stops attempting preemption from the current victim band and moves to the next lower
//     priority band.
//
// Returns:
//   - madeEnoughSpace (bool): True if sufficient space was made for itemToFit.
//   - err (error): Any significant, unrecoverable error encountered during the preemption process that halted it.
//     This excludes the Band-Level/Inter-Policy and Queue-Level/Intra-Policy issues which are logged and skipped as
//     specified in the Error Handling Strategy.
func (fc *FlowController) tryPreemptForRequest(
	itemToFit *flowItem,
	itemToFitBandAccessor types.PriorityBandAccessor,
	logger logr.Logger,
) (madeEnoughSpace bool, err error) {
	logger = logger.WithName("tryPreemptForRequest")
	if itemToFit.ByteSize() == 0 {
		return true, nil
	}

	logger.V(logutil.DEBUG).Info("Attempting preemption to free up space.")

	// Iterate victim priority bands from lowest actual priority (highest numeric value) up to, but not including,
	// itemToFit's priority.
	priorityLevels := fc.registry.AllOrderedPriorityLevels()
	for i := len(priorityLevels) - 1; i >= 0; i-- {
		priority := priorityLevels[i]

		// Preempt only from strictly lower (higher numeric value) priority bands.
		if priority <= itemToFitBandAccessor.Priority() {
			continue
		}

		bandAccessor, err := fc.registry.PriorityBandAccessor(priority)
		if err != nil {
			logger.Error(err, "Failed to get PriorityBandAccessor for victim band, skipping band.",
				"victimPriority", priority)
			continue
		}
		bandLogger := logger.WithValues("victimPriority", priority, "victimPriorityName", bandAccessor.PriorityName())

		// Loop to potentially preempt multiple items from queues within this band until enough space is made or the band
		// offers no more victims.
		for {
			if itemToFit.isFinalized() { // Stop preemption efforts if the item needing space is already finalized
				bandLogger.V(logutil.VERBOSE).Info("Preemptor item (itemToFit) finalized concurrently; stopping preemption for it.")
				return false, nil
			}

			if err := fc.preemptItem(bandAccessor, bandLogger); err != nil {
				bandLogger.Error(err, "Failed to preempt item. Stopping preemption for this band.")
				break
			}

			// Re-check capacity for itemToFit *after* preempting a new victim.
			// This accounts for space freed by previous preemptions in this or other bands, or concurrent
			// dispatches/expiries.
			canFitNow, reason, err := fc.hasCapacity(itemToFitBandAccessor, itemToFit.ByteSize(), logger)
			if err != nil {
				return false, fmt.Errorf("error checking capacity for itemToFit during preemption loop; aborted preemption attempt: %w", err)
			}
			if canFitNow {
				bandLogger.V(logutil.VERBOSE).Info("Sufficient space now available for itemToFit.")
				return true, nil
			}
			if reason == capacityFailureReasonBandLimitExceeded {
				return false, fmt.Errorf("cannot fit item: its own priority band %d ('%s') is at capacity. Preemption from lower bands cannot resolve this band-specific limit. Aborted preemption attempt",
					itemToFitBandAccessor.Priority(), itemToFitBandAccessor.PriorityName())
			}
		}
	}
	return false, nil
}

// preemptItem attempts to select and preempt a single item from the given victim priority band.
// It uses `applyPreemptionPolicies` to select a victim. If a victim is found, it's removed from its ManagedQueue and
// finalized with a types.QueueOutcomeEvictedPreempted outcome.
//
// Parameters:
//   - bandAccessor: The PriorityBandAccessor for the victim priority band from which an item should be preempted.
//   - logger: A contextual logger, typically scoped to the victim band being processed.
//
// Returns an error if victim selection or removal fails critically, or if no victim is selected by policies.
func (fc *FlowController) preemptItem(bandAccessor types.PriorityBandAccessor, logger logr.Logger) error {
	logger = logger.WithName("preemptItem")
	itemAccessor, err := fc.applyPreemptionPolicies(bandAccessor, logger)
	if err != nil {
		return fmt.Errorf("failed to select a victim item due to policy or registry error: %w", err)
	}
	if itemAccessor == nil {
		return errors.New("no further victim item selected by by policies in this band")
	}
	logger = logger.WithValues(
		"victimReqID", itemAccessor.RequestID(),
		"victimFlowID", itemAccessor.FlowID(),
		"victimByteSize", itemAccessor.ByteSize(),
	)

	managedQ, err := fc.registry.ManagedQueue(itemAccessor.FlowID(), bandAccessor.Priority())
	if err != nil {
		return fmt.Errorf("failed to get flow '%s' ManagedQueue for victim item '%s' removal: %w",
			itemAccessor.FlowID(), itemAccessor.RequestID(), err)
	}

	// removedItemAccessor should always be equal to itemAccessor for properly behaving queues; however, we finalize the
	// removedItemAccessor.(*flowItem) to be extra cautious.
	logger.V(logutil.DEFAULT).Info("Attempting to preempt victim request.")
	removedItemAccessor, _, _, err := managedQ.Remove(itemAccessor.Handle())
	if err != nil {
		return fmt.Errorf("failed to remove victim item '%s' from flow '%s' queue '%s': %w",
			itemAccessor.RequestID(), itemAccessor.FlowID(), managedQ.Name(), err)
	}
	removedItem, ok := removedItemAccessor.(*flowItem)
	if !ok {
		panic(fmt.Errorf("CRITICAL: Removed victim item '%s' from flow '%s' queue '%s' is not of type *flowItem, but %T",
			itemAccessor.RequestID(), itemAccessor.FlowID(), managedQ.Name(), itemAccessor))
	}

	// This is idempotent. It is possible the victim item was already finalized by other means, such as concurrent expiry
	// or dispatch.
	// Whatever outcome was reported first "wins".
	removedItem.finalize(types.QueueOutcomeEvictedPreempted,
		fmt.Errorf("%w: %w: preempted to make space for request in higher priority band",
			types.ErrEvicted, types.ErrPreempted))
	return nil
}

// applyPreemptionPolicies orchestrates the selection of a single victim item for preemption from a given priority band.
// It performs the following steps:
// 1. Retrieves the InterFlowPreemptionPolicy for the band.
// 2. Calls the inter-flow policy to select a victim FlowQueueAccessor from the band.
// 3. If a victim queue is selected, retrieves the IntraFlowPreemptionPolicy for that specific flow.
// 4. Calls the intra-flow policy to select a victim QueueItemAccessor from the chosen queue.
//
// Parameters:
//   - bandAccessor: The PriorityBandAccessor for the victim priority band.
//   - logger: A contextual logger, typically scoped to the victim band being processed.
//
// If policies simply do not select a victim (returning nil item/queue without error), this function returns (nil, nil).
func (fc *FlowController) applyPreemptionPolicies(
	bandAccessor types.PriorityBandAccessor,
	logger logr.Logger,
) (victimItem types.QueueItemAccessor, err error) {
	logger = logger.WithName("applyPreemptionPolicies")
	interPolicy, err := fc.registry.InterFlowPreemptionPolicy(bandAccessor.Priority())
	if err != nil {
		return nil, fmt.Errorf("failed to get InterFlowPreemptionPolicy for band %d ('%s'): %w",
			bandAccessor.Priority(), bandAccessor.PriorityName(), err)
	}
	queueAccessor, err := interPolicy.SelectVictimQueue(bandAccessor)
	if err != nil {
		return nil, fmt.Errorf("InterFlowPreemptionPolicy SelectVictimQueue failed for band %d ('%s'): %w",
			bandAccessor.Priority(), bandAccessor.PriorityName(), err)
	}
	if queueAccessor == nil {
		logger.V(logutil.DEBUG).Info("No victim queue selected by inter-flow preemption policy in this band.")
		return nil, nil
	}

	flowSpec := queueAccessor.FlowSpec()
	logger = logger.WithValues("victimFlowID", flowSpec.ID(), "victimQueueType", queueAccessor.Name())

	intraPolicy, err := fc.registry.IntraFlowPreemptionPolicy(flowSpec.ID(), flowSpec.Priority())
	if err != nil {
		return nil, fmt.Errorf("failed to get IntraFlowPreemptionPolicy for flow '%s' in band %d ('%s'): %w",
			flowSpec.ID(), bandAccessor.Priority(), bandAccessor.PriorityName(), err)
	}
	itemAccessor, err := intraPolicy.SelectVictim(queueAccessor)
	if err != nil {
		return nil, fmt.Errorf("IntraFlowPreemptionPolicy SelectVictim failed for flow '%s' in band %d ('%s'): %w",
			flowSpec.ID(), bandAccessor.Priority(), bandAccessor.PriorityName(), err)
	}
	if itemAccessor == nil {
		logger.V(logutil.DEBUG).Info("No victim item selected by intra-flow preemption policy from this queue.")
		return nil, nil
	}
	logger.V(logutil.DEBUG).Info("Victim item selected for preemption.", "victimReqID", itemAccessor.RequestID())
	return itemAccessor, nil
}

// attemptDispatchCycle tries to dispatch one eligible request.
// It iterates through priority bands from highest to lowest priority. For each band, it attempts to select and dispatch
// a single candidate item via the `dispatchItem` method.
// Returns true if an item was successfully dispatched from any band.
// Returns false if no item was dispatched. This can occur if all queues are empty, the system is saturated, policies do
// not select any item for dispatch, a selected candidate is found to be invalid (e.g., expired) during processing, or
// if errors occur during policy application or registry access for all considered bands.
func (fc *FlowController) attemptDispatchCycle() bool {
	logger := fc.logger.WithName("attemptDispatchCycle")
	for _, priority := range fc.registry.AllOrderedPriorityLevels() {
		bandAccessor, err := fc.registry.PriorityBandAccessor(priority)
		if err != nil {
			logger.Error(err, "Failed to get PriorityBandAccessor for dispatch, skipping band.", "priority", priority)
			continue
		}
		bandLogger := logger.WithValues("priority", priority, "priorityName", bandAccessor.PriorityName())

		if fc.saturationDetector.IsSaturated() { // Short circuit if system becomes saturated mid dispatch cycle
			logger.V(logutil.DEBUG).Info("System saturated, pausing dispatch attempts for this cycle.")
			return false
		}

		err = fc.dispatchItem(bandAccessor, bandLogger)
		if err == nil {
			return true
		}
		// If dispatchItem returns an error, it means either no item was selected from this band, or an error occurred
		// during policy application or item processing for this band.
		// Log the error and continue to the next (lower) priority band.
		bandLogger.Error(err, "Failed to dispatch item from band, attempting next priority band.")
	}
	return false
}

// dispatchItem attempts to select and dispatch a single item from the given priority band.
// It uses `applyDispatchPolicies` to select a candidate. The selected candidate is then removed from its ManagedQueue.
// After removal, the item's validity (e.g., not expired or context cancelled) is checked.
// If still valid, it's finalized with types.QueueOutcomeDispatched. If it became invalid (e.g., expired just before or
// during this process), it's finalized with the appropriate eviction outcome.
func (fc *FlowController) dispatchItem(bandAccessor types.PriorityBandAccessor, logger logr.Logger) error {
	logger = logger.WithName("dispatchItem")
	itemAccessor, err := fc.applyDispatchPolicies(bandAccessor, logger)
	if err != nil {
		return fmt.Errorf("failed to select a dispatch candidate due to policy or registry error: %w", err)
	}
	if itemAccessor == nil {
		return errors.New("no dispatch candidate selected by by policies in this band")
	}
	logger = logger.WithValues(
		"candidateID", itemAccessor.RequestID(),
		"flowID", itemAccessor.FlowID(),
		"candidateByteSize", itemAccessor.ByteSize(),
	)

	managedQ, err := fc.registry.ManagedQueue(itemAccessor.FlowID(), bandAccessor.Priority())
	if err != nil {
		return fmt.Errorf("failed to get flow '%s' ManagedQueue for dispatch candidate '%s' removal: %w",
			itemAccessor.FlowID(), itemAccessor.RequestID(), err)
	}

	// removedItemAccessor should always be equal to itemAccessor for properly behaving queues; however, we finalize the
	// removedItemAccessor.(*flowItem) to be extra cautious.
	logger.V(logutil.DEFAULT).Info("Attempting to dispatch candidate request.")
	removedItemAccessor, _, _, err := managedQ.Remove(itemAccessor.Handle())
	if err != nil {
		return fmt.Errorf("failed to remove dipatch candidate '%s' from flow '%s' queue '%s': %w",
			itemAccessor.RequestID(), itemAccessor.FlowID(), managedQ.Name(), err)
	}
	removedItem, ok := removedItemAccessor.(*flowItem)
	if !ok {
		panic(fmt.Errorf("CRITICAL: Removed dispatch candidate '%s' from flow '%s' queue '%s' is not of type *flowItem, but %T",
			itemAccessor.RequestID(), itemAccessor.FlowID(), managedQ.Name(), itemAccessor))
	}

	// Check if the item became invalid (expired, context cancelled).
	// This prevents dispatching an item that should have been evicted.
	isExpired, outcome, err := isItemExpiredFunc(logger)(itemAccessor, fc.clock.Now())
	if isExpired {
		logger.V(logutil.DEBUG).Info("Dispatch candidate found to be expired/cancelled at time of dispatch processing (after removal), finalizing accordingly.",
			"outcome", outcome.String(), "error", err)
		removedItem.finalize(outcome, err)
		return fmt.Errorf("dispatch candidate %s for flow %s became invalid before dispatch: %w", itemAccessor.RequestID(), itemAccessor.FlowID(), err)
	}

	// This is idempotent. It is possible the dispatch candidate was already finalized by other means, such as concurrent
	// expiry or preemption.
	// Whatever outcome was reported first "wins".
	removedItem.finalize(types.QueueOutcomeDispatched, nil)
	logger.V(logutil.DEFAULT).Info("Request dispatched.")
	return nil
}

// applyDispatchPolicies orchestrates the selection of a single item for dispatch from a given priority band.
// It performs the following steps:
// 1. Retrieves the InterFlowDispatchPolicy for the band.
// 2. Calls the inter-flow policy to select a dispatch candidate FlowQueueAccessor from the band.
// 3. If a candidate queue is selected, retrieves the IntraFlowDispatchPolicy for that specific flow.
// 4. Calls the intra-flow policy to select a dispatch candidate QueueItemAccessor from the chosen queue.
func (fc *FlowController) applyDispatchPolicies(
	bandAccessor types.PriorityBandAccessor,
	logger logr.Logger,
) (selectedItem types.QueueItemAccessor, err error) {
	logger = logger.WithName("applyDispatchPolicies")
	interPolicy, err := fc.registry.InterFlowDispatchPolicy(bandAccessor.Priority())
	if err != nil {
		return nil, fmt.Errorf("failed to get InterFlowDispatchPolicy for band %d ('%s'): %w",
			bandAccessor.Priority(), bandAccessor.PriorityName(), err)
	}
	queueAccessor, err := interPolicy.SelectQueue(bandAccessor)
	if err != nil {
		return nil, fmt.Errorf("InterFlowDispatchPolicy SelectQueue failed for band %d ('%s'): %w",
			bandAccessor.Priority(), bandAccessor.PriorityName(), err)
	}
	if queueAccessor == nil {
		logger.V(logutil.DEBUG).Info("No queue selected by inter-flow dispatch policy in this band.")
		return nil, nil
	}

	flowSpec := queueAccessor.FlowSpec()
	logger = logger.WithValues("flowID", flowSpec.ID(), "queueType", queueAccessor.Name())

	intraPolicy, err := fc.registry.IntraFlowDispatchPolicy(flowSpec.ID(), flowSpec.Priority())
	if err != nil {
		return nil, fmt.Errorf("failed to get IntraFlowDispatchPolicy for flow '%s' in band %d ('%s'): %w",
			flowSpec.ID(), bandAccessor.Priority(), bandAccessor.PriorityName(), err)
	}
	itemAccessor := intraPolicy.SelectItem(queueAccessor)
	if itemAccessor == nil {
		logger.V(logutil.DEBUG).Info("No item selected by intra-flow dispatch policy from this queue.")
		return nil, nil
	}
	logger.V(logutil.DEBUG).Info("Item selected for dispatch.", "reqID", itemAccessor.RequestID())
	return itemAccessor, nil
}

// runExpiryCleanup periodically checks for and removes expired items from all queues.
func (fc *FlowController) runExpiryCleanup(ctx context.Context) {
	defer fc.wg.Done()
	logger := fc.logger.WithName("runExpiryCleanup")
	logger.V(logutil.VERBOSE).Info("Expiry cleanup goroutine starting.")
	defer logger.V(logutil.VERBOSE).Info("Expiry cleanup goroutine stopped.")

	ticker := time.NewTicker(fc.config.ExpiryCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-fc.stopCh:
			return
		case now := <-ticker.C:
			logger.V(logutil.DEBUG).Info("Running periodic expiry cleanup cycle.")
			fc.cleanupAllExpiredItems(now, logger)
		}
	}
}

// isItemExpiredFunc is a factory that returns a types.IsItemExpiredFunc.
// The returned function checks if a given QueueItemAccessor is considered expired due to TTL violation or context
// cancellation. It also handles the edge case where an item might already be finalized.
func isItemExpiredFunc(logger logr.Logger) types.IsItemExpiredFunc {
	return func(itemAccessor types.QueueItemAccessor, currentTime time.Time) (bool, types.QueueOutcome, error) {
		fi, ok := itemAccessor.(*flowItem)
		if !ok {
			panic(fmt.Errorf("CRITICAL: item '%s' from flow '%s' queue is not of type *flowItem, but %T",
				itemAccessor.RequestID(), itemAccessor.FlowID(), itemAccessor))
		}

		itemLogger := logger.WithValues(
			"reqID", itemAccessor.RequestID(),
			"flowID", itemAccessor.FlowID(),
			"reqByteSize", itemAccessor.ByteSize(),
		)

		if fi.isFinalized() {
			// This should ideally not happen if items are correctly removed from queues upon finalization.
			// However, if it does, treat it as "expired" to ensure it is cleaned up from the queue.
			itemLogger.V(logutil.DEBUG).Info("Item already finalized, treating as expired to trigger removal since it should no longer be in queue.")
			outcome, err := fi.getFinalState()
			return true, outcome, err
		}

		if ctxErr := fi.OriginalRequest().Context().Err(); ctxErr != nil {
			itemLogger.V(logutil.DEBUG).Info("Request context cancelled.", "contextErr", ctxErr)
			return true, types.QueueOutcomeEvictedContextCancelled, fmt.Errorf("%w: %w: %w", types.ErrEvicted, types.ErrContextCancelled, ctxErr)
		}

		if fi.EffectiveTTL() > 0 && currentTime.Sub(fi.EnqueueTime()) > fi.EffectiveTTL() {
			itemLogger.V(logutil.DEBUG).Info("TTL expired, treating as expired.",
				"overTTL", currentTime.Sub(fi.EnqueueTime())-fi.EffectiveTTL())
			return true, types.QueueOutcomeEvictedTTL, fmt.Errorf("%w: %w", types.ErrEvicted, types.ErrTTLExpired)
		}
		return false, types.QueueOutcomeDispatched /* any value, not used */, nil
	}
}

// cleanupAllExpiredItems iterates through all managed queues and removes expired items using the standard expiry logic.
func (fc *FlowController) cleanupAllExpiredItems(now time.Time, logger logr.Logger) {
	logger = logger.WithName("cleanupAllExpiredItems")
	logger.V(logutil.DEBUG).Info("Cleaning up all expired items.")
	fc.applyIsItemExpiredFunc(now, logger, isItemExpiredFunc)
	logger.V(logutil.DEBUG).Info("Completed cleaning up all expired items.")
}

// evictAllOnShutdown is called when the FlowController is stopping.
// It iterates all queues and finalizes any remaining items with a shutdown-related outcome.
func (fc *FlowController) evictAllOnShutdown(shutdownError error, shutdownOutcome types.QueueOutcome) {
	logger := fc.logger.WithName("evictAllOnShutdown")
	logger.Info("Evicting all remaining items due to shutdown.",
		"outcome", shutdownOutcome.String(), "error", shutdownError)
	fc.applyIsItemExpiredFunc(fc.clock.Now(), logger, func(logger logr.Logger) types.IsItemExpiredFunc {
		return func(itemAccessor types.QueueItemAccessor, currentTime time.Time) (bool, types.QueueOutcome, error) {
			return true, shutdownOutcome, shutdownError
		}
	})
	logger.Info("Finished evicting all items on shutdown.")
}

// applyIsItemExpiredFunc is a generic helper that orchestrates the cleanup of items across all priority bands and their
// respective queues. It operates with the following concurrency model:
//  1. Iterates through all configured priority bands, launching a separate goroutine for each band to process it
//     concurrently.
//  2. Within each band-specific goroutine, it iterates through all flow queues in that band, launching a separate
//     goroutine for each queue to process it concurrently.
//  3. For each queue, it calls `ManagedQueue.CleanupExpired()` with a provided `IsItemExpiredFunc` (produced by the
//     factory `f`). This call is synchronous within the queue's goroutine, meaning the goroutine waits for
//     `CleanupExpired` (which mutates the queue and its statistics) to complete.
//  4. After `CleanupExpired` returns the list of removed items, this function launches a new "fire-and-forget"
//     goroutine for each removed item to finalize it (i.e., call `flowItem.finalize()`). The queue-processing
//     goroutine (and thus the band-processing goroutine) does NOT wait for these individual item finalizations to
//     complete.
//  5. The `applyIsItemExpiredFunc` method itself waits for all band-level goroutines to complete (which in turn wait
//     for their queue-level `CleanupExpired` calls) before returning.
func (fc *FlowController) applyIsItemExpiredFunc(
	now time.Time,
	logger logr.Logger,
	f func(logger logr.Logger) types.IsItemExpiredFunc,
) {
	var bandWg sync.WaitGroup
	for _, priority := range fc.registry.AllOrderedPriorityLevels() {
		bandWg.Add(1)
		go func(prio uint) {
			defer bandWg.Done()
			bandAccessor, err := fc.registry.PriorityBandAccessor(prio)
			if err != nil {
				logger.Error(err, "Failed to get PriorityBandAccessor.", "priority", prio)
				return
			}
			bandLogger := logger.WithValues("priority", prio, "priorityName", bandAccessor.PriorityName())

			var queueWg sync.WaitGroup
			bandAccessor.IterateQueues(func(qAccessor types.FlowQueueAccessor) bool {
				queueWg.Add(1)
				go func(qAcc types.FlowQueueAccessor) {
					defer queueWg.Done()
					queueLogger := bandLogger.WithValues("flowID", qAcc.FlowSpec().ID(), "queueType", qAcc.Name())
					managedQ, err := fc.registry.ManagedQueue(qAcc.FlowSpec().ID(), qAcc.FlowSpec().Priority())
					if err != nil {
						queueLogger.Error(err, "Failed to get ManagedQueue")
						return
					}

					// The factory `f` produces the specific IsItemExpiredFunc (e.g., standard expiry or shutdown eviction).
					removedInfos, cleanupErr := managedQ.CleanupExpired(now, f(queueLogger))
					if cleanupErr != nil {
						queueLogger.Error(cleanupErr, "Error during ManagedQueue CleanupExpired")
					}

					// Finalize each removed item concurrently in a fire-and-forget manner.
					// The queueWg.Done() call (and thus bandWg.Done()) will not wait for these.
					for _, info := range removedInfos {
						go func(i types.ExpiredItemInfo, qType string) {
							fi, ok := i.Item.(*flowItem)
							if !ok {
								panic(fmt.Errorf("CRITICAL: expired item '%s' from flow '%s' queue '%s' is not of type *flowItem, but %T",
									i.Item.RequestID(), i.Item.FlowID(), qType, i.Item))
							}
							// Ensure the flowItem itself is finalized. This is idempotent.
							fi.finalize(i.Outcome, i.Error)
							queueLogger.V(logutil.VERBOSE).Info("Item removed by ManagedQueue CleanupExpired.",
								"reqID", fi.RequestID(), "reqByteSize", fi.ByteSize(),
								"outcome", i.Outcome.String(), "error", i.Error)
						}(info, managedQ.Name())
					}
					// Note: No itemWg.Wait() here, item finalization is fire-and-forget.
				}(qAccessor)
				return true // We swallow errors, so always keep iterating
			})
			queueWg.Wait() // Wait for all queue CleanupExpired operations in this band to complete.
		}(priority)
	}
	bandWg.Wait() // Wait for all band processing to complete.
}
