package scheduling

import (
	"container/heap"
	"errors"
	"sync"
)

var (
	// ErrTooManyFlows occurs when attempting to push to a new flow, and adding
	// that flow would exceed the queue's total capacity. This indicates that the
	// totalCapacity is configured too low for the number of distinct flows.
	ErrTooManyFlows = errors.New("cumulative flow capacity exceeds total capacity")
	// ErrInvalidFlowCapacity occurs when the flow capacity is greater than the
	// total capacity. This is a configuration error.
	ErrInvalidFlowCapacity = errors.New("flow capacity cannot exceed total capacity")
	// ErrInsufficientFlowCapacity occurs when a request cannot be pushed due to
	// the target flow's capacity being exceeded. This indicates that the flow
	// capacity is configured too low for the target flow's volume.
	ErrInsufficientFlowCapacity = errors.New("flow capacity cannot accomodate request")
	// ErrQueueEmpty occurs when attempting to pop from an empty queue.
	ErrQueueEmpty = errors.New("queue is empty")
)

var (
	// requestSize represents the unit size of a request (currently 1).
	// TODO: eventually support configurable size units (e.g., bytes, input
	// tokens, etc.).
	requestSize uint64 = 1
)

// QueuedRequestContext defines the minimal interface required for requests
// that can be added to the FairRequestQueue. The Model() method identifies the
// flow to which the request belongs, allowing for fair queuing across
// different models.
type QueuedRequestContext interface {
	Model() string
	IsCritical() bool
	Wait()
	Signal()
	Cancel()
}

// timestampedQueuedRequestContext wraps a QueuedRequestContext with its
// virtual pop time. This is used for testing purposes to verify
// FIFO ordering with concurrent popper routines.
type timestampedQueuedRequestContext struct {
	queuedRequestContext QueuedRequestContext
	virtualPushTime      uint64
	virtualPopTime       uint64
}

// request wraps a QueuedRequestContext with its virtual finish time, which
// is used for priority ordering in the queue.
type request struct {
	value         QueuedRequestContext
	virFinishTime uint64
	virPushTime   uint64 // For testing and debugging only
}

// flowKey uniquely identifies a flow (queue) based on the model name.
type flowKey struct {
	model string
}

// flow represents a queue for a specific model. It maintains a priority queue
// of requests and tracks the last virtual finish time for fairness
// calculations.
type flow struct {
	queue             requestQueue
	lastVirFinishTime uint64
}

// requestQueue is a min-heap of requests, prioritized by virtual finish time.
type requestQueue []*request

// Standard heap.Interface methods for requestQueue.

func (q requestQueue) Len() int {
	return len(q)
}

func (q requestQueue) Less(i, j int) bool {
	return q[i].virFinishTime < q[j].virFinishTime
}

func (q requestQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

func (q *requestQueue) Push(x interface{}) {
	*q = append(*q, x.(*request))
}

func (q *requestQueue) Pop() interface{} {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[0 : n-1]
	return item
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
// This implementation uses a min-heap to efficiently manage the request queue
// and provides methods to push new requests (Push) and pop the highest
// priority request (Pop).
//
// Concurrency Safety: This implementation is thread-safe.  Push() and Pop()
// are protected by a mutex.
//
// Current Limitations:
//   - All flows currently have the same capacity.
//   - Request size is currently fixed.
type FairRequestQueue struct {
	mu            sync.RWMutex
	totalCapacity uint64
	flowCapacity  uint64
	flows         map[flowKey]*flow
	virtualTime   uint64
}

// NewFairRequestQueue creates a new FairRequestQueue.
//
// totalCapacity defines the maximum number of requests across all flows.
// flowCapacity defines the maximum number of requests for a single flow.
//
// It returns an error if flowCapacity is greater than totalCapacity.
func NewFairRequestQueue(totalCapacity, flowCapacity uint64) (*FairRequestQueue, error) {
	if flowCapacity > totalCapacity {
		return nil, ErrInvalidFlowCapacity
	}
	return &FairRequestQueue{
		totalCapacity: totalCapacity,
		flowCapacity:  flowCapacity,
		flows:         make(map[flowKey]*flow),
	}, nil
}

// Len returns the total number of queued requests across all flows.
func (frq *FairRequestQueue) Len() int {
	frq.mu.RLock()
	defer frq.mu.RUnlock()
	l := 0
	for _, f := range frq.flows {
		l += f.queue.Len()
	}
	return l
}

// Push adds a request to the queue, associating it with the appropriate flow
// based on its model.
//
// Returns ErrInsufficientFlowCapacity if the flow's capacity is exceeded or
// ErrTooManyFlows if adding a new flow would exceed the queue's total
// capacity.
func (frq *FairRequestQueue) Push(x QueuedRequestContext) error {
	frq.mu.Lock()
	defer frq.mu.Unlock()

	f, err := frq.chooseFlow(x)
	if err != nil {
		return err
	}
	if uint64(f.queue.Len())+requestSize > frq.flowCapacity {
		return ErrInsufficientFlowCapacity
	}
	r := &request{
		value:         x,
		virFinishTime: max(frq.virtualTime, f.lastVirFinishTime) + requestSize,
		virPushTime:   frq.virtualTime,
	}
	heap.Push(&f.queue, r)
	f.lastVirFinishTime = r.virFinishTime
	return nil
}

// Pop removes and returns the highest-priority request from the queue, based
// on its virtual finish time.
//
// Returns ErrQueueEmpty if the queue is empty.
func (frq *FairRequestQueue) Pop() (QueuedRequestContext, error) {
	r, err := frq.popAndTimestamp()
	if err != nil {
		return nil, err
	}
	return r.queuedRequestContext, nil
}

// popAndTimestamp retrieves, removes, and timestamps the highest priority
// request. This method is used internally by Pop() and also directly in tests
// to avoid race conditions when verifying FIFO order in concurrent tests.
//
// It returns a timestampedQueuedRequestContext, which includes the request
// and its virtual pop time.
func (frq *FairRequestQueue) popAndTimestamp() (*timestampedQueuedRequestContext, error) {
	frq.mu.Lock()
	defer frq.mu.Unlock()

	f := frq.selectFlow()
	if f == nil {
		return nil, ErrQueueEmpty
	}
	r := heap.Pop(&f.queue).(*request)
	frq.virtualTime += requestSize
	return &timestampedQueuedRequestContext{
		queuedRequestContext: r.value,
		virtualPushTime:      r.virPushTime,
		virtualPopTime:       frq.virtualTime - 1,
	}, nil
}

// chooseFlow selects the flow for the given request. If the flow does not
// exists, it creates it if there is sufficient totalCapacity, otherwise
// returning an error.
func (frq *FairRequestQueue) chooseFlow(x QueuedRequestContext) (*flow, error) {
	k := flowKey{model: x.Model()}
	if f, ok := frq.flows[k]; ok {
		return f, nil
	}
	if uint64(len(frq.flows)+1)*frq.flowCapacity > frq.totalCapacity {
		return nil, ErrTooManyFlows
	}
	f := &flow{
		queue:             make(requestQueue, 0, frq.flowCapacity),
		lastVirFinishTime: frq.virtualTime,
	}
	heap.Init(&f.queue)
	frq.flows[k] = f
	return f, nil
}

// selectFlow selects the flow with the earliest virtual finish time among the
// requests at the head of each flow's queue. Returns nil if all flows are
// empty.
func (frq *FairRequestQueue) selectFlow() *flow {
	var selectedFlow *flow
	for _, f := range frq.flows {
		if f.queue.Len() > 0 {
			if selectedFlow == nil || f.queue[0].virFinishTime < selectedFlow.queue[0].virFinishTime {
				selectedFlow = f
			}
		}
	}
	return selectedFlow
}
