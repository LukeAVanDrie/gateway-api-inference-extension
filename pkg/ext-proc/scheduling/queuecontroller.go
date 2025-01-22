package scheduling

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	klog "k8s.io/klog/v2"
)

// QueueControllerOptions allows configuring the QueueController. Fields with
// zero values will be defaulted.
//
// Since the scheduler, not the queue controller, controls the distribution of
// scheduled requests across backends, the "per backend" options should be
// treated as averages across all backends.
type QueueControllerOptions struct {
	WatermarkPerBackend          int           // Required: Desired stock level per backend.
	NumPoppers                   int           // Number of concurrent popper goroutines. Defaults to 1.
	PopperWaitBase               time.Duration // Base wait time for poppers (with jitter). Defaults to 10ms.
	PopperWaitJitterMax          time.Duration // Max jitter added to popperWaitBase. Defaults to 1ms.
	PercentageDeviationForBounds float64       // Percentage deviation from watermark for bounds. Defaults to 0.1 (10%). Must be between 0 and 1.
	LowerBoundPerBackend         int           // Optional: Override for derived lower bound. Not used if PercentageDeviationForBounds is specified.
	UpperBoundPerBackend         int           // Optional: Override for derived upper bound. Not used if PercentageDeviationForBounds is specified.
}

var (
	// ErrNilQueue occurs when constructing queue controller with a nil queue.
	ErrNilQueue = errors.New("queue cannot be nil")
	// ErrNilPodMetricsProvider occurs when constructing queue controller with a
	// nil pod metrics provider.
	ErrNilPodMetricsProvider = errors.New("pmp cannot be nil")
	// ErrInvalidWatermarkPerBackend occurs when constructing queue controller
	// with invalid watermark.
	ErrInvalidWatermarkPerBackend = errors.New("watermarkPerBackend must be positive")
	// ErrInvalidNumPoppers occurs when constructing queue controller with
	// invalid number of poppers.
	ErrInvalidNumPoppers = errors.New("numPoppers must be at least one")
	// ErrInvalidPopperWaitBase occurs when constructing the queue controller
	// with an invalid popper wait base.
	ErrInvalidPopperWaitBase = errors.New("popperWaitBase must be positive")
	// ErrInvalidPopperWaitJitterMax occurs when constructing the queue
	// controller with an invalid popper wait jitter max.
	ErrInvalidPopperWaitJitterMax = errors.New("popperWaitJitterMax must be non-negative")
	// ErrInvalidLowerBoundPerBackend occurs when constructing queue controller
	// with invalid lower bound value.
	ErrInvalidLowerBoundPerBackend = errors.New("lowerBoundPerBackend must be non-negative and at most watermarkPerBackend")
	// ErrInvalidUpperBoundPerBackend occurs when constructing queue controller
	// with invalid upper bound value.
	ErrInvalidUpperBoundPerBackend = errors.New("upperBoundPerBackend must be at least watermarkPerBackend")
	// ErrInvalidPercentageDeviationForBounds occurs when percentage deviation is
	// not between 0 and 1.
	ErrInvalidPercentageDeviationForBounds = errors.New("percentageDeviationForBounds must be between 0 and 1")
)

const (
	defaultNumPoppers                   = 1
	defaultPopperWaitBase               = 10 * time.Millisecond
	defaultPopperWaitJitterMax          = time.Millisecond
	defaultPercentageDeviationForBounds = 0.1
)

// QueueController manages request queueing based on the current system load
// ("stock").  It implements a closed-loop control system with negative
// feedback, specifically a stock-and-flow model, using a hysteresis band
// (lowerBoundPerBackend, upperBoundPerBackend) around a watermark (target
// stock level per backend) to control queueing and dampen oscillations.
// Multiple popper goroutines concurrently dequeue requests, allowing parallel
// request processing.
//
// Stock represents the number of requests currently being processed by
// backends.  The queue acts as a buffer to control the rate at which requests
// are scheduled to the backends, thereby controlling the stock.  It's
// important to distinguish between queue length (number of buffered requests)
// and stock (number of in-flight, scheduled requests). Enqueueing *delays* the
// increase of stock, while dequeueing *increases* stock.
//
// Actions and their effect on stock:
//   - Increase Stock:
//   - Dequeuing a request (and scheduling it to a backend for processing)
//   - Bypassing the queue entirely (when the queue is empty and stock is
//     below the upper bound).
//   - Decrease Stock: A scheduled request completing (successfully or
//     unsuccessfully) on a backend.
//   - Enqueuing: Delays the increase in stock by preventing a request from
//     being immediately scheduled to a backend.  It acts as backpressure when
//     the system is overloaded (stock is above the upperBound).  Note requests
//     are not enqueued when the queue is empty and stock is below the upper
//     bound to opportunistically decrease our scheduling latency.
//
// The QueueController uses the following logic with the hysteresis band to
// balance the stock around the watermark.
//   - When the stock exceeds the upper bound or there are already requests in
//     the queue, new requests are enqueued (and their processing is delayed)
//     until the stock falls below the upper bound.
//   - When the stock falls below the lower bound, queued requests are dequeued
//     to increase stock.
type QueueController struct {
	queue               *FairRequestQueue
	watermark           int                // Desired stock level per backend.
	lowerBound          int                // Lower bound of the hysteresis band per backend. Dequeueing is allowed below this level.
	upperBound          int                // Upper bound of the hysteresis band per backend. Enqueuing is allowed above this level.
	stock               atomic.Int64       // Current stock of in-flight, scheduled requests.
	numPoppers          int                // Number of concurrent popper goroutines.
	popperDone          chan struct{}      // Signals popper goroutines to exit.
	poppersWG           sync.WaitGroup     // Waits for popper goroutines to exit.
	popperWaitBase      time.Duration      // Base wait time for poppers (with jitter).
	popperWaitJitterMax time.Duration      // Maximum jitter added to popperWaitBase.
	rand                *rand.Rand         // Random source for jitter.
	podMetricsProvider  PodMetricsProvider // For tracking backend pods metrics.
}

// validateAndDefaultOptions validates the provided QueueControllerOptions and
// sets default values for optional fields. It returns an error if any of the
// options are invalid.
func (opts *QueueControllerOptions) validateAndDefaultOptions() error {
	if opts.WatermarkPerBackend <= 0 {
		return ErrInvalidWatermarkPerBackend
	}
	if opts.NumPoppers < 1 {
		opts.NumPoppers = defaultNumPoppers
	}
	if opts.PopperWaitBase <= 0 {
		return ErrInvalidPopperWaitBase
	}
	if opts.PopperWaitJitterMax < 0 {
		return ErrInvalidPopperWaitJitterMax
	}
	if opts.PercentageDeviationForBounds == 0 {
		opts.PercentageDeviationForBounds = defaultPercentageDeviationForBounds
	} else if opts.PercentageDeviationForBounds < 0 || opts.PercentageDeviationForBounds > 1 {
		return ErrInvalidPercentageDeviationForBounds
	}
	if opts.LowerBoundPerBackend < 0 || opts.LowerBoundPerBackend > opts.WatermarkPerBackend {
		return ErrInvalidLowerBoundPerBackend
	}
	if opts.UpperBoundPerBackend < opts.WatermarkPerBackend {
		return ErrInvalidUpperBoundPerBackend
	}
	return nil
}

// NewQueueController creates a new QueueController.
func NewQueueController(queue *FairRequestQueue, pmp PodMetricsProvider, opts QueueControllerOptions) (*QueueController, error) {
	if queue == nil {
		return nil, ErrNilQueue
	}
	if pmp == nil {
		return nil, ErrNilPodMetricsProvider
	}
	if err := opts.validateAndDefaultOptions(); err != nil {
		return nil, err
	}

	lowerBoundPerBackend := int(math.Floor(float64(opts.WatermarkPerBackend) * (1 - opts.PercentageDeviationForBounds)))
	upperBoundPerBackend := int(math.Ceil(float64(opts.WatermarkPerBackend) * (1 + opts.PercentageDeviationForBounds)))
	if opts.LowerBoundPerBackend != 0 {
		upperBoundPerBackend = opts.LowerBoundPerBackend
	}
	if opts.UpperBoundPerBackend != 0 {
		lowerBoundPerBackend = opts.UpperBoundPerBackend
	}

	return &QueueController{
		queue:               queue,
		watermark:           opts.WatermarkPerBackend,
		lowerBound:          lowerBoundPerBackend,
		upperBound:          upperBoundPerBackend,
		stock:               atomic.Int64{},
		numPoppers:          opts.NumPoppers,
		popperDone:          make(chan struct{}),
		popperWaitBase:      opts.PopperWaitBase,
		popperWaitJitterMax: opts.PopperWaitJitterMax,
		rand:                rand.New(rand.NewSource(time.Now().UnixNano())),
		podMetricsProvider:  pmp,
	}, nil
}

// Run starts the popper goroutines and blocks until the context is cancelled.
func (qc *QueueController) Run(ctx context.Context) {
	qc.poppersWG.Add(qc.numPoppers)
	for i := 0; i < qc.numPoppers; i++ {
		go qc.popper(ctx)
	}
	<-ctx.Done()
	close(qc.popperDone)
	qc.poppersWG.Wait()
}

// OnRequestScheduled increments the stock. Called when a request is scheduled
// to a backend.
func (qc *QueueController) OnRequestScheduled() {
	qc.stock.Add(1)
}

// OnRequestComplete decrements the stock. Called when a scheduled request
// commpletes (successfully or unsuccessfully).
func (qc *QueueController) OnRequestComplete() {
	qc.stock.Add(-1)
}

// TryEnqueue attempts to enqueue a request if the current stock is above the
// upper bound.  If no backends are ready, it always enqueues.  If the queue is
// not empty, the request will be enqueued regardless of stock level.  If
// enqueued, blocks until the request is dequeued.
// Returns an error only if the queue's Push operation fails.
func (qc *QueueController) TryEnqueue(req QueuedRequestContext) error {
	if qc.stock.Load() >= int64(qc.upperBound*qc.readyBackendCount()) || qc.queue.Len() > 0 {
		// qc.queue.Len() *may* drop to 0 between this check and Push. This is
		// an acceptable race condition since bypassing the queue is just a
		// greedy optimization to shave milliseconds off the enqueue/dequeue
		// process when unnecessary.
		if err := qc.queue.Push(req); err != nil {
			return err
		}
		req.Wait()
	}
	return nil
}

// tryDequeue attempts to dequeue a request if the current stock is below the
// lower bound.  If no backends are ready, it never dequeues.
// Returns req, nil if a request was dequeued.
// Returns nil, nil if stock is too high (not an error condition).
// Returns nil, err if a queue.Pop error occurs (including ErrQueueEmpty).
func (qc *QueueController) tryDequeue() (QueuedRequestContext, error) {
	if qc.stock.Load() < int64(qc.lowerBound*qc.readyBackendCount()) {
		return qc.queue.Pop()
	}
	return nil, nil
}

// popper dequeues and signals requests.
func (qc *QueueController) popper(ctx context.Context) {
	defer qc.poppersWG.Done()
	for {
		select {
		case <-qc.popperDone:
			return
		case <-ctx.Done():
			return
		default:
			req, err := qc.tryDequeue()
			if req != nil {
				req.Signal()
				continue
			}
			if err != nil && !errors.Is(err, ErrQueueEmpty) {
				klog.Error(err, "failed to dequeue request")
			}
			// TODO: consider exponential backoff with jitter to further reduce
			// thundering herd behavior.
			jitter := time.Duration(qc.rand.Int63n(int64(qc.popperWaitJitterMax)))
			time.Sleep(qc.popperWaitBase + jitter)
		}
	}
}

// readyBackendCount returns the number of ready backend pods.
// Returns 0 if no backends are ready.
func (qc *QueueController) readyBackendCount() int {
	return len(qc.podMetricsProvider.AllPodMetrics())
}
