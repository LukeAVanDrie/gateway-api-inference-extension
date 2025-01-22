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

// QueueControllerOptions allows configuring the RequestQueueController. Fields
// with zero values will be defaulted.
//
// Since the scheduler, not the queue controller, controls the distribution of
// scheduled requests across backends, the "per backend" options should be
// treated as averages across all backends.
type RequestQueueControllerOptions struct {
	WatermarkPerBackend          int           // Required: Desired stock level per backend.
	PercentageDeviationForBounds float64       // Percentage deviation from watermark for bounds. Defaults to 0.1 (10%). Must be between 0 and 1.
	LowerBoundPerBackend         int           // Optional: Override for derived lower bound. Not used if PercentageDeviationForBounds is specified.
	UpperBoundPerBackend         int           // Optional: Override for derived upper bound. Not used if PercentageDeviationForBounds is specified.
	ExpiryCleanupInterval        time.Duration // Interval for cleaning up expired requests from the request queue. Defaults to 1 second.
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
	// ErrInvalidLowerBoundPerBackend occurs when constructing queue controller
	// with invalid lower bound value.
	ErrInvalidLowerBoundPerBackend = errors.New("lowerBoundPerBackend must be non-negative and at most watermarkPerBackend")
	// ErrInvalidUpperBoundPerBackend occurs when constructing queue controller
	// with invalid upper bound value.
	ErrInvalidUpperBoundPerBackend = errors.New("upperBoundPerBackend must be at least watermarkPerBackend")
	// ErrInvalidPercentageDeviationForBounds occurs when percentage deviation is
	// not between 0 and 1.
	ErrInvalidPercentageDeviationForBounds = errors.New("percentageDeviationForBounds must be between 0 and 1")
	ErrExpiredTTL                          = errors.New("request evicted due to TTL")
	ErrNilCancelFunc                       = errors.New("QueueableRequestContext's CancelFunc cannot be nil")
)

const (
	defaultPercentageDeviationForBounds = 0.1
	defaultExpiryCleanupInterval        = time.Second
)

// requestQueue defines the minimal interface required by
// RequestQueueController.  Implementations of this interface MUST be
// thread-safe (for all exposed operations), as the RequestQueueController
// interacts with the queue concurrently.
type requestQueue interface {
	Push(x QueueableRequest) error
	Pop() (QueueableRequest, error)
	Len() int
	CleanupExpired()
}

type RequestContext interface {
	requestProperties
	Context() context.Context
	CancelFunc() context.CancelFunc
}

// requestContextAdapter adapts a RequestContext to a QueuableRequest.
type requestContextAdapter struct {
	RequestContext
	signal         chan struct{}
	isCancelled    bool
	evictionReason EvictionReason
	mu             sync.RWMutex
	pushErr        error
}

func newRequestContextAdater(reqCtx RequestContext) (*requestContextAdapter, error) {
	if reqCtx.CancelFunc() == nil {
		return nil, ErrNilCancelFunc
	}
	return &requestContextAdapter{
		RequestContext: reqCtx,
		signal:         make(chan struct{}, 1),
	}, nil
}

func (qra *requestContextAdapter) Wait() {
	for {
		select {
		case <-qra.Context().Done():
			// Instruct the expired request to be evicted from queue.
			qra.EvictAndCancel(ReasonExternalContextExpiry)
			return
		case <-qra.signal:
			return
		}
	}
}

func (qra *requestContextAdapter) Signal() {
	select {
	case qra.signal <- struct{}{}:
	case <-qra.Context().Done():
		<-qra.signal // Drain the channel to avoid a goroutine leak.
	}
}

func (qra *requestContextAdapter) EvictAndCancel(reason EvictionReason) {
	qra.mu.Lock()
	defer qra.mu.Unlock()

	qra.evictionReason = reason
	if !qra.isCancelled {
		qra.isCancelled = true
		qra.CancelFunc()()
	}
}

func (qra *requestContextAdapter) IsCancelled() bool {
	qra.mu.RLock()
	defer qra.mu.RUnlock()
	return qra.isCancelled
}

// RequestQueueController manages request queueing based on the current system
// load ("stock").  It implements a closed-loop control system with negative
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
// The RequestQueueController uses the following logic with the hysteresis band
// to balance the stock around the watermark.
//   - When the stock exceeds the upper bound or there are already requests in
//     the queue, new requests are enqueued (and their processing is delayed)
//     until the stock falls below the upper bound.
//   - When the stock falls below the lower bound, queued requests are dequeued
//     to increase stock.
type RequestQueueController struct {
	queue               requestQueue
	watermark           int                   // Desired stock level per backend.
	lowerBound          int                   // Lower bound of the hysteresis band per backend. Dequeueing is allowed below this level.
	upperBound          int                   // Upper bound of the hysteresis band per backend. Enqueuing is allowed above this level.
	stock               atomic.Int64          // Current stock of in-flight, scheduled requests.
	numPoppers          int                   // Number of concurrent popper goroutines.
	popperDone          chan struct{}         // Signals popper goroutines to exit.
	rand                *rand.Rand            // Random source for jitter.
	podMetricsProvider  PodMetricsProvider    // For tracking backend pods metrics.
	expiryCleanupTicker *time.Ticker          // For cleaning up expired requests from the request queue.
	enqueueChan            chan *requestContextAdapter // Channel for sequencing enqueue operations.
}

// validateAndDefaultOptions validates the provided
// RequestQueueControllerOptions and sets default values for optional fields.
// It returns an error if any of the options are invalid.
func (opts *RequestQueueControllerOptions) validateAndDefaultOptions() error {
	if opts.WatermarkPerBackend <= 0 {
		return ErrInvalidWatermarkPerBackend
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
	if opts.ExpiryCleanupInterval <= 0 {
		opts.ExpiryCleanupInterval = defaultExpiryCleanupInterval
	}
	return nil
}

// NewRequestQueueController creates a new RequestQueueController.
func NewRequestQueueController(queue requestQueue, pmp PodMetricsProvider, opts RequestQueueControllerOptions) (*RequestQueueController, error) {
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

	return &RequestQueueController{
		queue:               queue,
		watermark:           opts.WatermarkPerBackend,
		lowerBound:          lowerBoundPerBackend,
		upperBound:          upperBoundPerBackend,
		stock:               atomic.Int64{},
		popperDone:          make(chan struct{}),
		rand:                rand.New(rand.NewSource(time.Now().UnixNano())),
		podMetricsProvider:  pmp,
		expiryCleanupTicker: time.NewTicker(opts.ExpiryCleanupInterval),
		enqueueChan:            make(chan *requestContextAdapter),
	}, nil
}

// Run starts the RequestQueueController. It launches the expiry cleanup
// goroutine and the specified number of popper goroutines.  It blocks until
// the provided context is cancelled, at which point it gracefully shuts down
// the cleanup and popper goroutines.
//
// The expiry cleanup goroutine periodically removes expired requests from the
// queue. The popper goroutines dequeue requests and signal them to proceed
// when the "stock" is below the lower bound.
//
// This method should be called once during the RequestQueueController's
// lifecycle to initiate its operation.  The provided context controls the
// lifetime of the controller and its associated goroutines.  Cancelling the
// context will gracefully shut down the controller.
func (qc *RequestQueueController) Run(ctx context.Context) {
	go qc.runExpiryCleanup(ctx)
	go func() {
		defer close(qc.enqueueChan)
		// Alternate between enqueue and dequeue operations, but do not block
		// dequeue operations if there are no pending enqueue operations. This
		// alternating behavior offers queue contention fairness between between
		// enqueuing and dequeuing.
		for {
			select {
			case <-ctx.Done():
				return
			case qra := <-qc.enqueueChan:
				if err := qc.queue.Push(qra); err != nil {
					qra.mu.Lock()
					qra.pushErr = err
					qra.mu.Unlock()
				}
			default:
			}
			// Always try to dequeue (regardless of whether an enqueue occurred).
			if qc.stock.Load() <= int64(qc.lowerBound*qc.readyBackendCount()) {
				req, err := qc.queue.Pop()
				if req != nil {
					req.(*requestContextAdapter).Signal()
				} else if err != nil && !errors.Is(err, ErrQueueEmpty) {
					klog.Error(err, "failed to dequeue request")
				}
			}
		}
	}()
}

// OnRequestScheduled increments the stock by the request size.  Called when a
// request is scheduled to a backend.
func (qc *RequestQueueController) OnRequestScheduled(requestSize int64) {
	qc.stock.Add(requestSize)
}

// OnRequestComplete decrements the stock by the request size.  Called when a
// scheduled request commpletes (successfully or unsuccessfully).
func (qc *RequestQueueController) OnRequestComplete(requestSize int64) {
	qc.stock.Add(-requestSize)
}

// TryEnqueue attempts to enqueue a request. If no backends are ready, it
// always enqueues.  It bypasses the queue if the stock + request size is less
// than upperBound, and the queue is empty. If the queue is not empty, it is
// always enqueued, regardless of stock.  If enqueued, blocks until the request
// is dequeued or cancelled.
//
// Returns nil if the request was handled successfully (either bypassed or
// dequeued).
// Returns ErrExpiredTTL if the request expired while waiting in the queue.
// Returns the request's context error if cancelled while waiting.
// Returns an error if the queue's Push operation fails or if creating the
// Request Context Adapter fails.
func (qc *RequestQueueController) TryEnqueue(req RequestContext) error {
	qra, err := newRequestContextAdater(req)
	if err != nil {
		return err
	}

	if qc.queue.Len() > 0 || qc.stock.Load()+int64(req.Size()) >= int64(qc.upperBound*qc.readyBackendCount()) {
		// qc.queue.Len() *may* drop to 0 by the time this enqueue operation is
		// processed. This is an acceptable race condition since bypassing the
		// queue is just a greedy optimization to shave milliseconds off the
		// enqueue/dequeue process when unnecessary.
		qc.enqueueChan <- qra
		if qra.pushErr != nil {
			return qra.pushErr
		}
		qra.Wait() // Blocks until signaled or cancelled.
		if qra.IsCancelled() {
			if qra.evictionReason == ReasonTTLExpiry {
				return ErrExpiredTTL
			}
			return qra.Context().Err()
		}
	}
	return nil
}

// tryDequeue attempts to dequeue a request if the current stock is at or below
// the lower bound.  If no backends are ready, it never dequeues.
// Returns req, nil if a request was dequeued.
// Returns nil, nil if stock is too high (not an error condition).
// Returns nil, err if a queue.Pop error occurs (including ErrQueueEmpty).
func (qc *RequestQueueController) tryDequeue() (QueueableRequest, error) {
	if qc.stock.Load() <= int64(qc.lowerBound*qc.readyBackendCount()) {
		return qc.queue.Pop()
	}
	return nil, nil
}

// readyBackendCount returns the number of ready backend pods.
// Returns 0 if no backends are ready.
func (qc *RequestQueueController) readyBackendCount() int {
	return len(qc.podMetricsProvider.AllPodMetrics())
}

// runExpiryCleanup periodically checks for and removes expired requests from
// the queue. It handles both TTL-based expiry and external context
// cancellations.
func (qc *RequestQueueController) runExpiryCleanup(ctx context.Context) {
	defer qc.expiryCleanupTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-qc.expiryCleanupTicker.C:
			qc.queue.CleanupExpired()
		}
	}
}
