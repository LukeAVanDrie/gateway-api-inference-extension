package scheduling

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRequest struct {
	// Inherit blank implementations for properties we do not care about in the
	// tests.
	QueueableRequest
	model           string
	isCritical      bool
	virtualPushTime uint64 // For sorting in concurrent tests
	virtualPopTime  uint64 // For FIFO verification
}

func (fr *fakeRequest) Model() string {
	return fr.model
}

func (fr *fakeRequest) IsCritical() bool {
	return fr.isCritical
}

// TestFairRequestQueue tests basic single-threaded functionality.
func TestFairRequestQueue(t *testing.T) {
	tests := []struct {
		name            string
		totalCapacity   uint64
		flowCapacity    uint64
		requests        []*fakeRequest
		wantErr         error  // Expected error, if any
		wantVirtualTime uint64 // Expected final virtual time (# of total pops)
	}{
		{
			name:          "single flow",
			totalCapacity: 10,
			flowCapacity:  5,
			requests: []*fakeRequest{
				{model: "modelA", isCritical: true},
				{model: "modelA", isCritical: true},
				{model: "modelA", isCritical: true},
			},
			wantVirtualTime: 6,
		},
		{
			name:          "multiple flows",
			totalCapacity: 10,
			flowCapacity:  5,
			requests: []*fakeRequest{
				{model: "modelA", isCritical: true},
				{model: "modelB", isCritical: true},
				{model: "modelA", isCritical: true},
				{model: "modelB", isCritical: true},
			},
			wantVirtualTime: 8,
		},
		{
			name:          "flow capacity exceeded",
			totalCapacity: 10,
			flowCapacity:  2,
			requests: []*fakeRequest{
				{model: "modelA", isCritical: true},
				{model: "modelA", isCritical: true},
				{model: "modelA", isCritical: true},
			},
			wantErr: ErrInsufficientFlowCapacity,
		},
		{
			name:          "total capacity exceeded",
			totalCapacity: 2,
			flowCapacity:  1,
			requests: []*fakeRequest{
				{model: "modelA", isCritical: true},
				{model: "modelB", isCritical: true},
				{model: "modelC", isCritical: true},
			},
			wantErr: ErrTooManyFlows,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frq, err := NewFairRequestQueue(test.totalCapacity, test.flowCapacity, time.Second*10)
			if err != nil {
				t.Fatalf("Failed to create fair request queue: %v", err)
			}

			for _, req := range test.requests {
				err := frq.Push(req)
				if err != nil {
					if !errors.Is(err, test.wantErr) {
						t.Fatalf("Push() error = %v, wantErr %v", err, test.wantErr)
					}
					return
				}
			}

			if test.wantErr != nil {
				return // Expected error, no need to continue to pop().
			}

			reqsByModel := make(map[string][]*fakeRequest)
			for i := 0; i < len(test.requests); i++ {
				tsr, err := frq.popAndTimestamp()
				if err != nil {
					t.Fatalf("pop() unexpected error = %v", err)
				}
				req := tsr.request.(*fakeRequest)
				req.virtualPopTime = tsr.virtualPopTime
				reqsByModel[req.model] = append(reqsByModel[req.model], req)
			}

			if frq.virtualTime != test.wantVirtualTime {
				t.Errorf("Unexpected virtual time, got %v, want %v", frq.virtualTime, test.wantVirtualTime)
			}
			verifyFIFO(t, reqsByModel)
			verifyQueueEmpty(t, frq)
		})
	}
}

// TestFairRequestQueue_Concurrent tests the FairRequestQueue under concurrent
// push and pop operations.
// It verifies that FIFO ordering is maintained within each flow, even with
// multiple goroutines pushing and popping requests concurrently.
// The test uses a separate goroutine for pushing requests for each flow and a
// separate goroutine for each pop operation.
// Push order is tracked and used for FIFO verification within each flow.
// The test will exit on the first push or pop error.
func TestFairRequestQueue_Concurrent(t *testing.T) {
	const (
		totalCapacity = 100
		flowCapacity  = 10
		numRequests   = 50
		numFlows      = 10
	)

	// Uniform requests per flow.
	requestsPerFlow := make(map[string]int)
	for i := 0; i < numFlows; i++ {
		requestsPerFlow[fmt.Sprintf("model%d", i)] = numRequests / numFlows
	}

	frq, _ := NewFairRequestQueue(totalCapacity, flowCapacity, time.Second*10)

	var startWg, pushWg, popWg sync.WaitGroup
	pushersDone := make(chan struct{})
	poppedRequests := make(chan *timestampedQueueableRequest, numRequests)
	testDone := make(chan struct{}) // Signals test completion/failure
	defer close(testDone)

	// Add for ALL pushers and poppers *before* any Wait() calls.
	startWg.Add(numRequests + numRequests)
	runConcurrentPushes(t, frq, requestsPerFlow, &pushWg, &startWg, testDone)
	runConcurrentPops(t, frq, &popWg, &startWg, poppedRequests, testDone, pushersDone, numRequests /* one popper per request */, numRequests)

	pushWg.Wait()
	close(pushersDone)

	popWg.Wait()
	close(poppedRequests)

	select {
	case <-testDone:
		return // Exit the test early if there was an error
	default:
	}

	// Collect popped requests and organize by model.
	reqsByModel := make(map[string][]*fakeRequest)
	for tsr := range poppedRequests {
		fr := tsr.request.(*fakeRequest)
		fr.virtualPopTime = tsr.virtualPopTime
		reqsByModel[fr.model] = append(reqsByModel[fr.model], fr)
	}

	for m, reqs := range reqsByModel {
		if len(reqs) != numRequests/numFlows {
			t.Errorf("Flow %s: Expected %d requests, but got %d", m, numRequests/numFlows, len(reqs))
		}
	}
	if frq.virtualTime != numRequests * 2 {
		t.Errorf("Unexpected virtual time, got %v, want %v", frq.virtualTime, numRequests)
	}
	verifyFIFO(t, reqsByModel)
	verifyQueueEmpty(t, frq)
}

// TestFairRequestQueue_ProportionalService verifies proportional service
// across flows under relatively uniform load.
func TestFairRequestQueue_ProportionalService(t *testing.T) {
	const (
		numFlows             = 10
		baseRequestsPerFlow  = 1000 // High value needed for statistical stability.
		deltaRequestsPerFlow = 50   // Request delta between each successive flow.
		flowCapacity         = baseRequestsPerFlow + numFlows*deltaRequestsPerFlow
		totalCapacity        = flowCapacity * numFlows
		// This value cannot be >> the # of pushers (currently numFlows). If there
		// are way more poppers than pushers, the first pusher to get scheduled
		// has a very high change of having all its requests drained by poppers
		// before any other pushers get a change to add requests to the queue. This
		// contention results in unfairness even if our WFQ algorithm is behaivng
		// as intended.
		numPoppers = 2
		// f we pop too few requests, there will be too much variance, resulting in
		// flaky tests. If we pop too many requests, we risk exhausting a flow
		// which will guarantee uneven service even if the fair request queue is
		// behaving correctly. Setting this to baseRequestsPerFlow avoid the former
		// (with a sufficiently large value for baseRequestsPerFlow) and guarantees
		// the latter is not possible.
		popLimit                   = baseRequestsPerFlow
		expectedPoppedCountPerFlow = popLimit / numFlows
		// Allow for a 15% deviation from uniform fairness. Adjust as necessary to
		// reduce flakiness. This test is asserting on a statistical property of
		// the queue, so we need some tolerance for variance.
		deviationTolerance = 0.15
	)

	totalRequests := 0
	requestsPerFlow := make(map[string]int)
	for i := 0; i < numFlows; i++ {
		flowRequests := baseRequestsPerFlow + i*deltaRequestsPerFlow
		requestsPerFlow[fmt.Sprintf("model%d", i)] = flowRequests
		totalRequests += flowRequests
	}

	frq, _ := NewFairRequestQueue(totalCapacity, flowCapacity, time.Second*10)

	var startWg, pushWg, popWg sync.WaitGroup
	pushersDone := make(chan struct{})
	poppedRequests := make(chan *timestampedQueueableRequest, popLimit)
	testDone := make(chan struct{}) // Signals test completion/failure
	defer close(testDone)

	startWg.Add(totalRequests + numPoppers)
	runConcurrentPushes(t, frq, requestsPerFlow, &pushWg, &startWg, testDone)
	runConcurrentPops(t, frq, &popWg, &startWg, poppedRequests, testDone, pushersDone, popLimit, numPoppers)

	pushWg.Wait()
	close(pushersDone)

	popWg.Wait()
	close(poppedRequests)

	select {
	case <-testDone:
		return // Exit the test early if there was an error
	default:
	}

	// Collect popped request count per flow
	reqsByModel := make(map[string][]*fakeRequest)
	for tsr := range poppedRequests {
		fr := tsr.request.(*fakeRequest)
		fr.virtualPopTime = tsr.virtualPopTime
		reqsByModel[fr.model] = append(reqsByModel[fr.model], fr)
	}

	// Fairness verification:
	totalPops := 0
	for model, reqs := range reqsByModel {
		poppedCount := len(reqs)
		totalPops += poppedCount
		deviation := float64(poppedCount-expectedPoppedCountPerFlow) / float64(expectedPoppedCountPerFlow)
		if deviation > deviationTolerance || deviation < -deviationTolerance {
			t.Errorf("Fairness violated for flow %s: expected ~%d requests (%.2f%%), got %d (deviation: %.2f%%)",
				model, expectedPoppedCountPerFlow, 100*float64(expectedPoppedCountPerFlow)/float64(popLimit), poppedCount, 100*deviation)
		}
	}

	// Not necessary, but this provides good coverage that FIFO is still
	// preserved under varying push loads.
	verifyFIFO(t, reqsByModel)
}

// Starts concurrent push routines per request.
func runConcurrentPushes(t *testing.T, frq *FairRequestQueue, requestsPerFlow map[string]int, pushWg, startWg *sync.WaitGroup, testDone chan struct{}) {
	t.Helper()
	for flowID, numRequests := range requestsPerFlow {
		for i := 0; i < numRequests; i++ {
			pushWg.Add(1)
			go func(flowID string) {
				defer pushWg.Done()
				startWg.Done() // Signal this pusher is created and ready
				startWg.Wait() // Wait for all other routines to be ready
				select {
				case <-testDone: // Test failed
					return
				default:
				}

				req := &fakeRequest{model: flowID}
				if err := frq.Push(req); err != nil {
					close(testDone)
					t.Errorf("push flow %s error, request %+v: %v", flowID, req, err)
					return
				}
			}(flowID)
		}
	}
}

// Starts concurrent pop routines that pop until popLimit is reached
// (collectively).
func runConcurrentPops(t *testing.T, frq *FairRequestQueue, popWg, startWg *sync.WaitGroup, poppedRequests chan *timestampedQueueableRequest, testDone, pushersDone chan struct{}, popLimit, numPoppers int) {
	t.Helper()
	var poppedCount int32 // Shared atomic counter
	for i := 0; i < numPoppers; i++ {
		popWg.Add(1)
		go func() {
			defer popWg.Done()
			startWg.Done() // Signal this popper is created and ready
			startWg.Wait() // Wait for all other routines to be ready
			for atomic.LoadInt32(&poppedCount) < int32(popLimit) {
				tsr, err := frq.popAndTimestamp()
				if err != nil {
					if errors.Is(err, ErrQueueEmpty) {
						select {
						case <-testDone: // Test failed
							return
						case <-pushersDone: // All pushes done; exit if queue empty
							return
						default:
							continue // Retry pop if queue temporarily empty
						}
					}
					t.Errorf("pop %d error: %v", atomic.LoadInt32(&poppedCount), err)
					return
				}

				select {
				case poppedRequests <- tsr: // Send if channel has capacity
					atomic.AddInt32(&poppedCount, 1) // Increment ONLY after send
				default:
					return // Exit the popper if the channel is already full
				}
			}
		}()
	}
}

// Verify FIFO is preserved within a flow using virtualPopTime.
func verifyFIFO(t *testing.T, requestsByModel map[string][]*fakeRequest) {
	t.Helper()
	for model, reqs := range requestsByModel {
		if len(reqs) <= 1 {
			return // Nothing to compare
		}

		sort.Slice(reqs, func(i, j int) bool {
			return reqs[i].virtualPushTime < reqs[j].virtualPushTime
		})

		for i := 1; i < len(reqs); i++ {
			if reqs[i].virtualPopTime < reqs[i-1].virtualPopTime {
				t.Errorf("FIFO violated within flow %s: request %+v popped before request %+v", model, reqs[i], reqs[i-1])
			}
		}
	}
}

func verifyQueueEmpty(t *testing.T, frq *FairRequestQueue) {
	t.Helper()
	_, err := frq.Pop()
	if !errors.Is(err, ErrQueueEmpty) {
		t.Error("Expected queue empty")
	}
}
