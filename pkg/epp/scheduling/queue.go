package scheduling

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	v1alpha2 "sigs.k8s.io/gateway-api-inference-extension/api/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

var (
	ErrNilScheduler           = fmt.Errorf("scheduler cannot be nil")
	ErrNilRequest             = fmt.Errorf("scheduleableRequest.Request() cannot be nil")
	ErrMissingCriticalityBand = fmt.Errorf("no criticality band for request criticality")
	ErrEvicted                = fmt.Errorf("request evicted from queue")
	ErrModelAtCapacity        = errors.New("model is at capacity")
	ErrCannotFindBackend      = errors.New("cannot find suitable backend")
)

// EvictionReason represents the reason for a request's eviction from the
// queue.
type EvictionReason int

const (
	ReasonNotEvicted            EvictionReason = iota // Request was not evicted (or never enqueued to begin with).
	ReasonTTLExpiry                                   // Request evicted due to TTL expiry.
	ReasonExternalContextExpiry                       // Request evicted due to external context cancellation (timeout or cancellation).
	ReasonPreempted                                   // Request evicted due to preemption.
	ReasonCannotFindBackend                           // Request evicted due to failure to find a suitable backend.
)

// String implements the Stringer interface for EvictionReason, facilitating
// human readable logging and debugging with eviction reasons.
func (er EvictionReason) String() string {
	reasons := []string{"Not Evicted", "TTL Expiry", "External Context Expiry", "Preempted", "Cannot Find Backend"}
	if er < 0 || int(er) >= len(reasons) {
		return "Unknown Eviction Reason"
	}
	return reasons[er]
}

// Defines the order of criticalities. Higher priority criticalities should
// appear first. This ordering is *crucial* for correct criticality-based
// service differentiaiton.
var criticalities = []v1alpha2.Criticality{
	v1alpha2.Critical,
	v1alpha2.Standard,
	v1alpha2.Sheddable,
}

// QueueConfig enables overriding default behaviors for the Queue Controller.
type QueueConfig struct {
	TotalQueueCapacity    uint64        // Total capacity (in bytes) of the queue across all models and criticality bands. Defaults to 100MB.
	ModelQueueCapacity    uint64        // Capacity (in bytes) of the per-model queues. Defaults to 10MB.
	QueueTTL              time.Duration // TTL for requests in the queue. Defaults to 30 seconds.
	ExpiryCleanupInterval time.Duration // Interval for cleaning up expired requests from the queue. Defaults to 1 second.
}

// validateAndApplyDefaults validates the provided scheduling config assigns
// default values.
func (cfg *QueueConfig) validateAndApplyDefaults() error {
	if cfg.TotalQueueCapacity <= 0 {
		cfg.TotalQueueCapacity = DefaultTotalQueueCapacity
	}
	if cfg.ModelQueueCapacity <= 0 {
		cfg.ModelQueueCapacity = DefaultModelQueueCapacity
	}
	if cfg.QueueTTL <= 0 {
		cfg.QueueTTL = 30 * time.Second
	}
	if cfg.ExpiryCleanupInterval <= 0 {
		cfg.ExpiryCleanupInterval = DefaultExpiryCleanupInterval
	}
	if cfg.TotalQueueCapacity < cfg.ModelQueueCapacity {
		return fmt.Errorf("total queue capacity (%d) must be greater than or equal to model queue capacity (%d)", cfg.TotalQueueCapacity, cfg.ModelQueueCapacity)
	}
	return nil
}

const (
	DefaultTotalQueueCapacity    = 100 * 1024 * 1024 // 100MB
	DefaultModelQueueCapacity    = 10 * 1024 * 1024  // 10MB
	DefaultQueueTTL              = 30 * time.Second
	DefaultExpiryCleanupInterval = time.Second
)

// SchedulableRequest interface represents a request that can be scheduled.
type SchedulableRequest interface {
	Context() context.Context
	Request() *types.LLMRequest
	Size() uint64
}

// queueItem struct represents an item in the scheduler's queue.
type queueItem struct {
	request        SchedulableRequest
	enqueueTime    time.Time
	ttl            time.Duration // Eventually, we may want to consider dynamic TTL by use case or request characteristics.
	done           chan struct{}
	err            atomic.Value
	evictionReason atomic.Value
	targetPod      atomic.Pointer[types.Pod]
}

// scheduler interface selects the best backend (pod) for a request. It should
// return a scheduling.ErrBackendsSaturated error if the request cannot be
// assigned to a backend due to saturation and should continue waiting in the
// queue.
type scheduler interface {
	Schedule(ctx context.Context, req *types.LLMRequest) (targetPod types.Pod, err error)
}

// FairnessPolicy defines the fairness behavior within a criticality band. It
// does not control scheduling behavior across bands.
type FairnessPolicy interface {
	// SelectQueue selects the next queue within the band to dispatch a request
	// from. It returns nil if there are no requests to dispatch from any queue.
	SelectQueue(band *criticalityBand) *queue
	// PreemptReqeust selects the next request to preempt within the band,
	// returning its respective queue and list element.
	PreemptRequest(band *criticalityBand) (*queue, *list.Element, error)
}

// PolicyNone implements the FairnessPolicy interface using FCFS dispatching
// across models.
type PolicyNone struct{}

func (p *PolicyNone) SelectQueue(band *criticalityBand) *queue {
	band.mu.RLock()
	defer band.mu.RUnlock()
	var item *queueItem
	var queue *queue
	for _, q := range band.queues {
		if q == nil {
			continue
		}

		q.mu.RLock()
		defer q.mu.RUnlock()
		if q.requests != nil && q.requests.Len() > 0 {
			e := q.requests.Front()
			i := e.Value.(*queueItem)
			if item == nil || i.enqueueTime.Before(item.enqueueTime) {
				item = i
				queue = q
			}
		}
	}
	return queue
}

func (p *PolicyNone) PreemptRequest(band *criticalityBand) (queue *queue, element *list.Element, err error) {
	return nil, nil, nil // TODO
}

// PolicyRoundRobin the FairnessPolicy interface using round robin dispatching
// across models.
type PolicyRoundRobin struct {
	modelNames        []string
	index             int
	mu                sync.Mutex
	lastSelectedModel string
}

func (p *PolicyRoundRobin) SelectQueue(band *criticalityBand) *queue {
	band.mu.RLock()
	defer band.mu.RUnlock()

	if len(band.queues) == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// This works since we never remove a queue for a model once it has been
	// added. Even if no new requests come in, we still store a reference to the
	// empty queue meaning band.queues is monitonically increasing.
	if len(p.modelNames) != len(band.queues) {
		// TODO: new model additions disrupt the round robin order. This may be
		// solvable with a hash ring, but I am not sure if it warrants the added
		// complexity.
		p.modelNames = make([]string, 0, len(band.queues))
		for modelName := range band.queues {
			p.modelNames = append(p.modelNames, modelName)
		}
		p.index = 0
		p.lastSelectedModel = ""
	}

	startIndex := 0
	if p.lastSelectedModel != "" {
		for i, modelName := range p.modelNames {
			if modelName == p.lastSelectedModel {
				startIndex = i
				break
			}
		}
	}

	for i := 0; i < len(p.modelNames); i++ {
		index := (startIndex + p.index) % len(p.modelNames)
		modelName := p.modelNames[index]
		p.index = (p.index + 1) % len(p.modelNames)

		q, ok := band.queues[modelName]
		if !ok || q == nil {
			continue // This should never happen.
		}

		q.mu.RLock()
		if q.requests != nil && q.requests.Len() > 0 {
			q.mu.RUnlock()
			p.lastSelectedModel = modelName
			return q
		}
		q.mu.RUnlock()
	}
	return nil
}

func (*PolicyRoundRobin) PreemptRequest(band *criticalityBand) (queue *queue, element *list.Element, err error) {
	return nil, nil, nil // TODO
}

// queue represents a per-model, FIFO request queue.
type queue struct {
	requests *list.List
	size     atomic.Uint64
	mu       sync.RWMutex
}

// criticalityBand models a set of per-model request queues at a given
// criticality level.
type criticalityBand struct {
	queues map[string]*queue
	mu     sync.RWMutex
}

// QueueController manages the queuing and flow control of LLM requests.
type QueueController struct {
	criticalityBands    map[v1alpha2.Criticality]*criticalityBand
	fairnessPolicy      FairnessPolicy
	scheduler           scheduler
	clock               clock
	enqueueChan         chan *queueItem
	totalQueueCapacity  uint64
	modelQueueCapacity  uint64
	queueTTL            time.Duration
	expiryCleanupTicker *time.Ticker
}

// clock is used for time manipulation for TTL testing.
type clock interface {
	now() time.Time
}

// realClock struct implements the clock interface using time.Now().
type realClock struct{}

func (c realClock) now() time.Time {
	return time.Now()
}

// NewQueueController creates a new QueueController instance.
func NewQueueController(scheduler scheduler, cfg QueueConfig) (*QueueController, error) {
	if scheduler == nil {
		return nil, ErrNilScheduler
	}
	if err := cfg.validateAndApplyDefaults(); err != nil {
		return nil, err
	}

	q := &QueueController{
		criticalityBands:    make(map[v1alpha2.Criticality]*criticalityBand),
		fairnessPolicy:      &PolicyNone{},
		scheduler:           scheduler,
		clock:               realClock{},
		enqueueChan:         make(chan *queueItem),
		totalQueueCapacity:  cfg.TotalQueueCapacity,
		modelQueueCapacity:  cfg.ModelQueueCapacity,
		queueTTL:            cfg.QueueTTL,
		expiryCleanupTicker: time.NewTicker(cfg.ExpiryCleanupInterval),
	}
	for _, c := range criticalities {
		q.criticalityBands[c] = &criticalityBand{
			queues: make(map[string]*queue),
		}
	}
	return q, nil
}

type QueueSize struct {
	Size          uint64
	RequestCount  uint64
	CapacityUsage float32 // gauge in [0, 1]
}

func (qc *QueueController) QueueSize() *QueueSize {
	sizes := qc.QueueSizeByCriticality()
	size := uint64(0)
	requestCount := uint64(0)
	for _, q := range sizes {
		size += q.Size
		requestCount += q.RequestCount
	}
	return &QueueSize{
		Size:          size,
		RequestCount:  requestCount,
		CapacityUsage: float32(size) / float32(qc.totalQueueCapacity),
	}
}

func (qc *QueueController) QueueSizeByCriticality() map[v1alpha2.Criticality]*QueueSize {
	sizes := map[v1alpha2.Criticality]*QueueSize{}
	for _, c := range criticalities {
		if criticalityBand, ok := qc.criticalityBands[c]; ok {
			criticalityBand.mu.RLock()
			size := uint64(0)
			requestCount := uint64(0)
			for _, q := range criticalityBand.queues {
				size += q.size.Load()
				requestCount += uint64(q.requests.Len())
			}
			// The capacity usage denominator is calculated as the sum of the model
			// queue capacities because models can be added or removed from a band
			// dynamically.
			sizes[c] = &QueueSize{
				Size:          size,
				RequestCount:  requestCount,
				CapacityUsage: float32(size) / float32(qc.modelQueueCapacity*uint64(len(criticalityBand.queues))),
			}
			criticalityBand.mu.RUnlock()
		}
	}
	return sizes
}

func (qc *QueueController) QueueSizeByModelName() map[string]*QueueSize {
	sizes := map[string]*QueueSize{}
	for _, c := range criticalities {
		if criticalityBand, ok := qc.criticalityBands[c]; ok {
			criticalityBand.mu.RLock()
			for modelName, q := range criticalityBand.queues {
				sizes[modelName] = &QueueSize{
					Size:          q.size.Load(),
					RequestCount:  uint64(q.requests.Len()),
					CapacityUsage: float32(q.size.Load()) / float32(qc.modelQueueCapacity),
				}
			}
			criticalityBand.mu.RUnlock()
		}
	}
	return sizes
}

// enqueue adds a request to the queue controller's queue.
func (qc *QueueController) enqueue(item *queueItem) error {
	r := item.request.Request()
	if r == nil {
		return ErrNilRequest
	}

	logger := log.FromContext(item.request.Context()).WithValues("request", r)
	criticalityBand, ok := qc.criticalityBands[r.Criticality]
	// We could automatically create the band here instead, but then we would
	// need another layer of synchronization on the Queue Controller itself. This
	// case should be unreachable in praxis.
	if !ok || criticalityBand == nil {
		logger.V(logutil.DEFAULT).Error(ErrMissingCriticalityBand, "Missing criticality band", "criticality", r.Criticality)
		return ErrMissingCriticalityBand
	}

	criticalityBand.mu.Lock()
	defer criticalityBand.mu.Unlock()
	if _, ok := criticalityBand.queues[r.Model]; !ok {
		logger.V(logutil.VERBOSE).Info("Creating new queue for model", "model", r.Model)
		criticalityBand.queues[r.Model] = &queue{requests: list.New()}
	}

	q := criticalityBand.queues[r.Model]
	q.mu.Lock()
	defer q.mu.Unlock()

	if qc.modelQueueCapacity-q.size.Load() < item.request.Size() {
		logger.V(logutil.VERBOSE).Info("Model queue is at or near capacity and cannot accomadate request, dropping request", "model", r.Model)
		return ErrModelAtCapacity
	}

	// overflow := (item.request.Size() + s.QueueLen()) - s.totalQueueCapacity
	// if overflow > 0 {
	// 	// TODO: preempt until space.
	// 	// rules:
	// 	// 1. Can only preempt from strictly lower criticality band (e.g., critical cannot preempt critical, sheddable cannot preempt standard, etc.)
	// 	// 2. We should keep preempting until the request can be accomodated. If the total capacity across the lower criticality bands is insufficient, we shouldn't bother trying at all. No reason to preempt if it will never fit.
	// }

	q.requests.PushBack(item)
	q.size.Add(item.request.Size())
	logger.V(logutil.VERBOSE).Info("Enqueued request", "model", r.Model, "model queue length", q.requests.Len())
	return nil
}

// runExpiryCleanup periodically executes cleanupExpired. This runs in a
// separate goroutine to avoid blocking the main scheduler loop.
func (qc *QueueController) runExpiryCleanup(ctx context.Context) {
	defer qc.expiryCleanupTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-qc.expiryCleanupTicker.C:
			qc.cleanupExpired()
		}
	}
}

// cleanupExpired removes all expired elements from the queue. This includes
// external context expiry as well as TTL detection.
func (qc *QueueController) cleanupExpired() {
	now := qc.clock.now()
	var wg sync.WaitGroup
	for _, band := range qc.criticalityBands {
		wg.Add(1)
		go func(band *criticalityBand) {
			defer wg.Done()

			band.mu.RLock()
			defer band.mu.RUnlock()
			for _, q := range band.queues {
				if q == nil {
					continue
				}

				q.mu.RLock()
				if q.requests == nil || q.requests.Len() == 0 {
					q.mu.RUnlock()
					continue
				}
				q.mu.RUnlock()

				q.mu.Lock()
				for e := q.requests.Front(); e != nil; {
					i := e.Value.(*queueItem)
					i.checkExpiry(now)
					next := e.Next()
					if err, ok := i.err.Load().(error); ok && errors.Is(err, ErrEvicted) {
						q.size.Add(^(i.request.Size() - 1))
						q.requests.Remove(e)
					}
					e = next
				}
				q.mu.Unlock()
			}
		}(band)
	}
	wg.Wait()
}

// checkExpiry checks and handles cases where the request has expired in the
// queue.
func (i *queueItem) checkExpiry(now time.Time) {
	if i.request.Context().Err() != nil {
		logger := log.FromContext(i.request.Context()).WithValues("request", i.request.Request())
		close(i.done)
		i.err.Store(errors.Join(ErrEvicted, i.request.Context().Err()))
		i.evictionReason.Store(ReasonExternalContextExpiry)
		logger.V(logutil.VERBOSE).Info("Evicting request from queue", "eviction reason", i.evictionReason.Load().(EvictionReason).String())
		return
	}
	if now.Sub(i.enqueueTime) > i.ttl {
		logger := log.FromContext(i.request.Context()).WithValues("request", i.request.Request())
		close(i.done)
		i.err.Store(ErrEvicted)
		i.evictionReason.Store(ReasonTTLExpiry)
		logger.V(logutil.VERBOSE).Info("Evicting request from queue", "eviction reason", i.evictionReason.Load().(EvictionReason).String(), "exceeded TTL by", now.Sub(i.enqueueTime)-i.ttl)
		return
	}
}

// tryDequeue attempts to remove the head of line from the queue if and only if
// the is a backend with sufficient capacity to process it.
func (qc *QueueController) tryDequeue() {
	for _, c := range criticalities {
		if criticalityBand, ok := qc.criticalityBands[c]; ok {
			q := qc.fairnessPolicy.SelectQueue(criticalityBand)

			if q == nil {
				continue
			}

			q.mu.Lock()
			if q.requests == nil || q.requests.Len() == 0 {
				q.mu.Unlock()
				continue
			}

			e := q.requests.Front()
			i := e.Value.(*queueItem)

			logger := log.FromContext(i.request.Context()).WithValues("request", i.request.Request())
			i.checkExpiry(qc.clock.now())
			if err, ok := i.err.Load().(error); ok && errors.Is(err, ErrEvicted) {
				q.size.Add(^(i.request.Size() - 1))
				q.requests.Remove(e)
				q.mu.Unlock()
				logger.V(logutil.VERBOSE).Info("Request found expired during dequeue attempt")
				continue
			}

			logger.V(logutil.VERBOSE).Info("Finding suitable backend for request")
			pod, err := qc.scheduler.Schedule(i.request.Context(), i.request.Request())
			if err != nil {
				q.mu.Unlock()
				if errors.Is(err, ErrBackendsSaturated) {
					logger.V(logutil.VERBOSE).Info("Backends saturated, leaving request in queue", "model", i.request.Request().Model)
					return
				} else {
					// Currently, ErrBackendsSaturated is the only recoverable error from
					// the balancer. All other errors are considered fatal for the\
					// request.
					logger.Error(err, "Balancer failed for non-saturation reason, dropping request", "model", i.request.Request().Model, "balancer_error", err.Error())
					i.err.Store(errors.Join(ErrCannotFindBackend, err))
					i.evictionReason.Store(ReasonCannotFindBackend)
					q.size.Add(^(i.request.Size() - 1))
					q.requests.Remove(e)
					close(i.done)
					q.mu.Unlock()
				}
			}

			logger.V(logutil.VERBOSE).Info("Dequeueing request to be dispatched to target pod", "target pod", pod.GetPod().NamespacedName)
			q.size.Add(^(i.request.Size() - 1))
			q.requests.Remove(e)
			i.targetPod.Store(&pod)
			i.evictionReason.Store(ReasonNotEvicted)
			q.mu.Unlock()
			close(i.done)
			return
		}
	}
}

// Run starts the routines for periodic expried request cleanup and interleaved
// enqueue and dequeue operation attempts.
func (qc *QueueController) Run(ctx context.Context) {
	go qc.runExpiryCleanup(ctx)
	go func() {
		defer close(qc.enqueueChan)
		for {
			select {
			case <-ctx.Done():
				return
			case item := <-qc.enqueueChan:
				if err := qc.enqueue(item); err != nil {
					item.err.Store(err)
				}
			default:
				// No enqueue operation, continue to tryDequeue; this ensures we
				// interleave enqueue and dequeue attempts while not blocking dequeuing
				// when there are no pending enqueue operations. This is important to
				// ensure that we do not starve the dequeue operation.
			}
			qc.tryDequeue()
		}
	}()
}

// Schedule adds a new request to the Queue Controller's queue and blocks until
// it's processed or evicted.
func (qc *QueueController) Schedule(req SchedulableRequest) (types.Pod, EvictionReason, error) {
	item := &queueItem{
		request:     req,
		enqueueTime: qc.clock.now(),
		ttl:         qc.queueTTL,
		done:        make(chan struct{}),
	}

	logger := log.FromContext(req.Context()).WithValues("request", req.Request())
	logger.V(logutil.VERBOSE).Info("Scheduling request")

	// Blocking enqueue attempt until enqueue operation processed or external
	// context expiry:
	select {
	case qc.enqueueChan <- item:
		if err := item.err.Load().(error); err != nil {
			logger.Error(err, "Failed to enqueue request")
			item.evictionReason.Store(ReasonNotEvicted)
			return nil, ReasonNotEvicted, err
		}
		logger.V(logutil.VERBOSE).Info("Enqueued request")
	case <-req.Context().Done():
		item.checkExpiry(qc.clock.now())
		return nil, item.evictionReason.Load().(EvictionReason), item.err.Load().(error)
	}

	// Blocking until successful dequeue, eviction, or external context expiry:
	select {
	case <-item.done:
		logger.V(logutil.VERBOSE).Info("Unblocking request to be dispatched to target pod", "target pod", (*item.targetPod.Load()).GetPod().NamespacedName)
		return *item.targetPod.Load(), item.evictionReason.Load().(EvictionReason), item.err.Load().(error)
	case <-req.Context().Done():
		close(item.done)
		return nil, item.evictionReason.Load().(EvictionReason), item.err.Load().(error)
	}
}
