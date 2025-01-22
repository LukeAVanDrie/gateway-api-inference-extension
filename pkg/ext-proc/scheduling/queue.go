package scheduling

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
	"time"

	"inference.networking.x-k8s.io/gateway-api-inference-extension/api/v1alpha1"
	"k8s.io/klog/v2"
)

var (
	// ErrTooManyFlows occurs when attempting to push to a new flow, and adding
	// that flow would exceed the queue's total capacity. This indicates that the
	// totalCapacity is configured too low for the number of distinct flows.
	ErrTooManyFlows = errors.New("cumulative flow capacity exceeds total capacity")
	// ErrInvalidFlowCapacity occurs when the flow capacity is greater than the
	// total capacity. This is a configuration error.
	ErrInvalidFlowCapacity = errors.New("flow capacity cannot exceed total capacity")
	// ErrFlowAtCapacity occurs when a request cannot be pushed due to the target
	// flow's capacity being exceeded. This indicates that the flow capacity is
	// configured too low for the target flow's volume.
	ErrFlowAtCapacity = errors.New("flow is at capacity")
	// ErrQueueAtCapacity occurs when attempting to push a request to a queue
	// that is at its total capacity, preventing any further requests from being
	// enqueued, even if individual flows might have available capacity. This
	// often indicates that the totalCapacity is configured too low for the
	// current workload(s).
	ErrQueueAtCapacity = errors.New("queue is at total capacity")
	// ErrQueueEmpty occurs when attempting to pop from an empty queue.
	ErrQueueEmpty = errors.New("queue is empty")
	// ErrNoPreemptableRequests occurs when attempting to enqueue a request to a
	// full queue and no requests with lower priority can be preempted to make
	// room. This indicates that all queued requests have equal or higher
	// priority.
	ErrNoPreemptableRequests = errors.New("queue is full and no preemptable requests found")
)

// EvictionReason represents the reason for a request's eviction from the
// queue.
type EvictionReason int

const (
	ReasonNotEvicted            EvictionReason = iota // Request was not evicted.
	ReasonTTLExpiry                                   // Request evicted due to TTL expiry.
	ReasonExternalContextExpiry                       // Request evicted due to external context cancellation (timeout or cancellation).
	ReasonPreempted                                   // Request evicted due to preemption.
)

// Defines the order of crticialities. Higher priority crticialities should
// appear first.  This ordering is *crucial* for the correct functioning of the
// WFQ algorithm.
var crticialities = []v1alpha1.Criticality{
	v1alpha1.Critical,
	v1alpha1.Standard,
	v1alpha1.Sheddable,
}

// String implements the Stringer interface for EvictionReason, facilitating
// human readable logging and debugging with eviction reasons.
func (er EvictionReason) String() string {
	reasons := []string{"Not Evicted", "TTL Expiry", "External Context Expiry"}
	if er < 0 || int(er) > len(reasons) {
		return "Unknown Eviction Reason"
	}
	return reasons[er]
}

// requestProperties defines the properties of a request relevant to the queue.
type requestProperties interface {
	Model() string
	Criticality() v1alpha1.Criticality
	Size() uint64
}

// QueueableRequest defines the minimal interface required for requests
// supported in the FairRequestQueue.
type QueueableRequest interface {
	requestProperties
	EvictAndCancel(reason EvictionReason)
	IsCancelled() bool
}

// item wraps a QueueableRequest with data necessary for WFQ.
type item struct {
	request           QueueableRequest
	enqueueTime       time.Time
	virtualFinishTime uint64
	virtualPushTime   uint64 // For testing and debugging only
}

// timestampedQueueableRequest wraps a QueueableRequest with its virtual
// timestamps. This is used for testing purposes only.
type timestampedQueueableRequest struct {
	request         QueueableRequest
	virtualPushTime uint64
	virtualPopTime  uint64
}

// flow represents a queue for a specific model. It maintains a priority queue
// of requests and tracks the last virtual finish time for fairness
// calculations.
type flow struct {
	mu                sync.Mutex
	queue             *list.List
	lastVirFinishTime uint64
}

// clock is used for time manipulation for TTL testing.
type clock interface {
	now() time.Time
}

// realClock simply exposes time.Now()
type realClock struct{}

func (c realClock) now() time.Time {
	return time.Now()
}

// FairRequestQueueOption configures a FairRequestQueue instance during
// initialization.
type FairRequestQueueOption func(*FairRequestQueue)

// withClock sets the clock on the FairRequestQueue.  This should be used for
// testing only.
func withClock(c clock) FairRequestQueueOption {
	return func(s *FairRequestQueue) {
		s.clock = c
	}
}

// FairRequestQueue implements a weighted fair queuing (WFQ) algorithm.
// WFQ ensures proportional service across different flows (identified by the
// request's model) by using virtual time to schedule requests.
//
// Each request is assigned a virtual finish time based on its arrival time
// (represented by the queue's virtual time) and a fixed request size. The
// queue prioritizes requests with earlier virtual finish times, ensuring
// fairness among flows with varying request sizes or arrival rates.
//
// Concurrency Safety: This implementation is thread-safe.  Push() and Pop()
// are protected by a mutex.
//
// Current Limitation(s)ß:
//   - All flows currently have the same capacity.
type FairRequestQueue struct {
	mu            sync.RWMutex
	totalCapacity uint64
	flowCapacity  uint64
	flows         map[v1alpha1.Criticality]map[string]*flow
	virtualTime   uint64
	queueTTL      time.Duration
	clock         clock
}

// NewFairRequestQueue creates a new FairRequestQueue.
//
// totalCapacity defines the maximum number of requests across all flows.
// flowCapacity defines the maximum number of requests for a single flow.
//
// It returns an error if flowCapacity is greater than totalCapacity.
func NewFairRequestQueue(totalCapacity, flowCapacity uint64, queueTTL time.Duration, opts ...FairRequestQueueOption) (*FairRequestQueue, error) {
	if flowCapacity > totalCapacity {
		return nil, ErrInvalidFlowCapacity
	}
	frq := &FairRequestQueue{
		totalCapacity: totalCapacity,
		flowCapacity:  flowCapacity,
		flows:         make(map[v1alpha1.Criticality]map[string]*flow, 1),
		queueTTL:      queueTTL,
		clock:         new(realClock),
	}
	for _, opt := range opts {
		opt(frq)
	}
	return frq, nil
}

// Len returns the total number of queued requests across all flows.
func (frq *FairRequestQueue) Len() int {
	frq.mu.RLock()
	defer frq.mu.RUnlock()
	l := 0
	for _, flows := range frq.flows {
		for _, f := range flows {
			f.mu.Lock()
			l += f.queue.Len()
			f.mu.Unlock()
		}
	}
	return l
}

// Push adds a request to the queue, associating it with the appropriate flow
// based on its model.  If the queue is full and a higher-priority request is
// being added, it preempts lower-priority requests until there is enough
// capacity. It returns an error if the flow capacity is exceeded, if adding
// a new flow would exceed the total queue capacity, or if the queue is at
// total capacity and no lower-priority requests can be preempted.
func (frq *FairRequestQueue) Push(x QueueableRequest) error {
	frq.mu.Lock()
	defer frq.mu.Unlock()

	f, err := frq.chooseFlow(x)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if uint64(f.queue.Len())+x.Size() > frq.flowCapacity {
		return fmt.Errorf("flow %s at capacity: %d, cannot accommodate request of size %d: %w", x.Model(), frq.flowCapacity, x.Size(), ErrFlowAtCapacity)
	}

	// frq.Len() acquires its own read lock which would result in deadlock, so we
	// need to calculate the length directly inside Push's write lock.
	currentQueueLength := 0
	for _, flowMap := range frq.flows {
		for _, f := range flowMap {
			currentQueueLength += f.queue.Len()
		}
	}

	// Only preempt if necessary (total capacity exceeded).
	for uint64(currentQueueLength)+x.Size() > frq.totalCapacity {
		preemptedReq, err := frq.preempt(x.Criticality())
		if err != nil {
			return fmt.Errorf("cannot enqueue request of criticality %s: %w", x.Criticality(), ErrQueueAtCapacity)
		}
		klog.V(4).Infof("Preempted request: model=%s, size=%d, criticality=%s, by incoming request with criticality %s, current queue length=%d ", preemptedReq.Model(), preemptedReq.Size(), preemptedReq.Criticality(), x.Criticality(), frq.Len())
		currentQueueLength--
	}

	// Enqueue the request.
	r := &item{
		request:           x,
		virtualFinishTime: max(frq.virtualTime, f.lastVirFinishTime) + x.Size(),
		virtualPushTime:   frq.virtualTime, // Capture virtual time at push
		enqueueTime:       frq.clock.now(), // Record enqueue time for expiry
	}
	f.queue.PushBack(r)

	// Update virtual times.
	frq.virtualTime += x.Size()
	f.lastVirFinishTime = r.virtualFinishTime

	klog.V(4).Infof("Enqueued request: model=%s, size=%d, criticality=%s, virtualFinishTime=%d, current queue length=%d", x.Model(), x.Size(), x.Criticality(), r.virtualFinishTime, frq.Len())
	return nil
}

// Pop removes and returns the highest-priority request from the queue, based
// on its virtual finish time across flows. Within a flow, requests are
// dequeued FIFO.
//
// Returns ErrQueueEmpty if the queue is empty.
func (frq *FairRequestQueue) Pop() (QueueableRequest, error) {
	r, err := frq.popAndTimestamp()
	if err != nil {
		return nil, err
	}
	return r.request, nil
}

// popAndTimestamp retrieves, removes, and timestamps the highest priority
// request. This method is used internally by Pop() and also directly in tests
// to avoid race conditions when verifying FIFO order in concurrent tests.
//
// It returns a timestampedQueueableRequest, which includes the request and its
// virtual timestamps.
func (frq *FairRequestQueue) popAndTimestamp() (*timestampedQueueableRequest, error) {
	frq.mu.Lock()
	defer frq.mu.Unlock()

	f := frq.selectHighestPriorityFlow(crticialities)
	if f == nil {
		return nil, ErrQueueEmpty
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.queue.Remove(f.queue.Front()).(*item)
	frq.virtualTime += r.request.Size()
	return &timestampedQueueableRequest{
		request:         r.request,
		virtualPushTime: r.virtualPushTime,
		virtualPopTime:  frq.virtualTime - r.request.Size(),
	}, nil
}

// chooseFlow selects the flow for the given request. If the flow does not
// exists, it creates it if there is sufficient totalCapacity, otherwise
// returning an error.
func (frq *FairRequestQueue) chooseFlow(x QueueableRequest) (*flow, error) {
	k := x.Model()
	if f, ok := frq.flows[x.Criticality()][k]; ok {
		return f, nil
	}
	if uint64(len(frq.flows)+1)*frq.flowCapacity > frq.totalCapacity {
		return nil, ErrTooManyFlows
	}
	f := &flow{
		mu:                sync.Mutex{},
		queue:             list.New(),
		lastVirFinishTime: frq.virtualTime,
	}
	frq.flows[x.Criticality()][k] = f
	return f, nil
}

// preempt finds and removes the lowest priority request from the queue (this
// is the request with the *maximum* virtual finish time from the *lowest*
// criticality preemptable flow).
// It cancels the preempted request's context.
// It returns the preempted request or an error if no request can be preempted
// or if the queue is empty.
func (frq *FairRequestQueue) preempt(incomingRequestCriticality v1alpha1.Criticality) (QueueableRequest, error) {
	frq.mu.Lock()
	defer frq.mu.Unlock()
	f := frq.selectLowestPriorityFlow(preemptableCriticalities(incomingRequestCriticality))
	if f == nil {
		return nil, fmt.Errorf("no preemptable request found for incoming criticality %s: %w", incomingRequestCriticality, ErrNoPreemptableRequests) // Return the new error
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	req := f.queue.Remove(f.queue.Back()).(*item).request
	req.EvictAndCancel(ReasonPreempted)
	klog.V(4).Infof("Preempted request model=%s, size=%d, criticality=%s by incoming request with criticality %s", req.Model(), req.Size(), req.Criticality(), incomingRequestCriticality)
	return req, nil
}

// selectHighestPriorityFlow selects the flow with the *minimum* head-of-line
// virtual finish time from the *highest* criticality flow.  Returns nil if no
// suitable flow is found.  This function assumes the caller holds the
// necessary locks (e.g., frq.mu).
func (frq *FairRequestQueue) selectHighestPriorityFlow(criticalities []v1alpha1.Criticality) *flow {
	var selectedFlow *flow
	for _, c := range criticalities {
		if flowMap, ok := frq.flows[c]; ok {
			for _, f := range flowMap {
				f.mu.Lock()
				if f.queue.Len() > 0 {
					if selectedFlow == nil || f.queue.Front().Value.(*item).virtualFinishTime < selectedFlow.queue.Front().Value.(*item).virtualFinishTime {
						selectedFlow = f
					}
				}
				f.mu.Unlock()
			}
			if selectedFlow != nil {
				return selectedFlow
			}
		}
	}
	return nil
}

// selectLowestPriorityFlow selects the flow with the *maximum* back-of-line
// virtual finish time from the *lowest* criticality flow.  Returns nil if no
// suitable flow is found.  This function assumes the caller holds the
// necessary locks (e.g., frq.mu).
func (frq *FairRequestQueue) selectLowestPriorityFlow(criticalities []v1alpha1.Criticality) *flow {
	var selectedFlow *flow
	for i := len(criticalities) - 1; i >= 0; i-- {
		c := criticalities[i]
		if flowMap, ok := frq.flows[c]; ok {
			for _, f := range flowMap {
				f.mu.Lock()
				if f.queue.Len() > 0 {
					if selectedFlow == nil || f.queue.Back().Value.(*item).virtualFinishTime > selectedFlow.queue.Back().Value.(*item).virtualFinishTime {
						selectedFlow = f
					}
				}
				f.mu.Unlock()
			}
			if selectedFlow != nil {
				return selectedFlow
			}
		}
	}
	return nil
}

// preemptablePriorities returns a filtered, ordered list of criticalities (by
// criticality descending), only including criticalities *lower* than that of
// the incoming request.
func preemptableCriticalities(incomingRequestCriticality v1alpha1.Criticality) []v1alpha1.Criticality {
	for i, p := range crticialities {
		if p == incomingRequestCriticality {
			return crticialities[i+1:]
		}
	}
	return []v1alpha1.Criticality{}
}

// CleanupExpired removes expired requests from all flows in the queue.  It
// iterates through each flow, identifies expired requests based on TTL and
// external context cancellation, and removes them from the queue.  The
// function handles both TTL-based expiry and external context cancellation
// concurrently for each flow.
func (frq *FairRequestQueue) CleanupExpired() {
	frq.mu.RLock()
	defer frq.mu.RUnlock()
	var wg sync.WaitGroup
	for _, priority := range crticialities {
		flowMap, ok := frq.flows[priority]
		if !ok {
			continue
		}
		for k, f := range flowMap {
			wg.Add(1)
			go func(k string, f *flow) {
				defer wg.Done()
				f.mu.Lock()
				defer f.mu.Unlock()
				var next *list.Element
				for e := f.queue.Front(); e != nil; e = next {
					r := e.Value.(*item)
					next = e.Next()
					if frq.clock.now().Sub(r.enqueueTime) > frq.queueTTL || r.request.IsCancelled() {
						f.queue.Remove(e)
						if !r.request.IsCancelled() {
							r.request.EvictAndCancel(ReasonTTLExpiry)
							klog.V(4).Info("Removed request from queue due to TTL expiry")
						} else {
							klog.V(4).Info("Removed request from queue due external context expiry")
						}
					}
				}
			}(k, f)
		}
	}
	wg.Wait()
}
