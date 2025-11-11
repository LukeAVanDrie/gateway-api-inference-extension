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

package simulation

import (
	"container/heap"
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock/testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector"
	schedtypes "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

// Simulator defines the high-level control surface for the test harness.
// Scenarios interact with this interface to drive the timeline.
type Simulator interface {
	// Run advances the simulation clock by the specified duration.
	// It executes the Discrete Event Simulation (DES) loop.
	Run(duration time.Duration)

	// CurrentTime returns the current virtual time.
	CurrentTime() time.Time

	// SetTrafficProfile updates the statistical shape of requests.
	SetTrafficProfile(p WorkloadProfile)

	// SetRelativeLoad scales the traffic generator rate.
	SetRelativeLoad(loadFactor float64)

	// AddBackends provisions N new pods into the cluster.
	AddBackends(count int)

	// DegradeBackend applies a performance penalty to a specific pod.
	DegradeBackend(podID string, factor float64)

	// GetPodIDs returns a sorted list of all active backend IDs.
	GetPodIDs() []string

	// GetResults returns the full telemetry history of the simulation.
	GetResults() *SimResult
}

// BackendGenerator is a factory function injected at startup.
type BackendGenerator func(podID string) Backend

// simBackend wraps the generic Backend interface to store simulation-specific metadata locally.
// This improves CPU cache locality and removes the need for map lookups during the hot loop.
type simBackend struct {
	Backend
	// Pre-allocated Pod object to avoid re-creating it every tick.
	podObj *backend.Pod
	// Flag to debounce wakeup events (replaces the pendingWakeups map).
	isPending bool
}

// SimulationEnvironment is the concrete implementation of the Simulator.
// It orchestrates the interaction between the Plant (Backends), Traffic, and Controller.
type SimulationEnvironment struct {
	// --- Configuration ---
	cfg       SimEnvConfig
	generator BackendGenerator

	// --- Components ---
	clock           *testing.FakeClock
	trafficGen      TrafficGenerator
	trafficInitDone bool
	trafficProfile  WorkloadProfile
	targetLoadRatio float64
	buffer          *FlowControlBuffer

	// --- System Under Test (SUT) ---
	Controller *saturationdetector.SaturationController
	Recorder   *saturationdetector.SaturationSignalRecorder
	Picker     *saturationdetector.ProbePicker

	// --- Internal State ---
	// Backends map PodName -> Wrapper (The Plant)
	backends     map[string]*simBackend
	cachedPodIDs []string
	datastore    *MockDatastore

	// OPTIMIZATION: Event Loop Memory Management
	eventQueue *EventQueue
	eventPool  []*SimEvent // Stack for recycling events to avoid GC

	// OPTIMIZATION: Scratch Buffers
	// Reused slices to avoid allocations in attemptDispatch()
	scratchCandidates []schedtypes.Pod
	scratchScored     []*schedtypes.ScoredPod

	// --- Telemetry ---
	startTime         time.Time
	history           []Snapshot
	shedRequests      int
	completedRequests []*Request
}

// NewSimulator creates a ready-to-run simulation environment.
func NewSimulator(
	cfg SimEnvConfig,
	backendGen BackendGenerator,
	initialProfile WorkloadProfile,
	controller *saturationdetector.SaturationController,
	picker *saturationdetector.ProbePicker,
	recorder *saturationdetector.SaturationSignalRecorder,
	buffer *FlowControlBuffer,
	datastore *MockDatastore,
	clock *testing.FakeClock,
) Simulator {
	sim := &SimulationEnvironment{
		cfg:               cfg,
		generator:         backendGen,
		clock:             clock,
		trafficGen:        NewConstantGenerator("main", 0, initialProfile, 0),
		trafficProfile:    initialProfile,
		targetLoadRatio:   0.0,
		buffer:            buffer,
		Controller:        controller,
		Recorder:          recorder,
		Picker:            picker,
		backends:          make(map[string]*simBackend),
		cachedPodIDs:      make([]string, 0),
		datastore:         datastore,
		eventQueue:        &EventQueue{},
		eventPool:         make([]*SimEvent, 0, 1024),
		startTime:         clock.Now(),
		history:           make([]Snapshot, 0),
		completedRequests: make([]*Request, 0),
		// Pre-allocate scratch buffers with reasonable capacity
		scratchCandidates: make([]schedtypes.Pod, 0, 100),
		scratchScored:     make([]*schedtypes.ScoredPod, 0, 100),
	}

	for id := range cfg.Backends {
		sim.addExistingBackend(id, cfg.Backends[id])
	}
	return sim
}

// --- Lifecycle Implementation ---

func (s *SimulationEnvironment) Run(duration time.Duration) {
	endTime := s.clock.Now().Add(duration)

	s.ensureScheduled(EventControllerTick, s.cfg.RecorderConfig.TickInterval)
	s.ensureScheduled(EventScrape, s.cfg.ScrapeInterval)

	if !s.trafficInitDone {
		s.trafficGen.Init(s.clock.Now())
		s.trafficInitDone = true
	}

	var nextEventTime time.Time
	var isTrafficEvent bool

	for {
		now := s.clock.Now()
		if now.After(endTime) {
			break
		}

		// A. Determine Next Event Time
		// We compete between the Heap Head and the Traffic Generator.

		heapEventTime := time.Time{}
		if s.eventQueue.Len() > 0 {
			heapEventTime = s.eventQueue.Peek().Timestamp
		}

		trafficTime := s.trafficGen.PeekNextArrival()

		// Selection Logic: Min(Heap, Traffic)
		if !heapEventTime.IsZero() && !trafficTime.IsZero() {
			if trafficTime.Before(heapEventTime) {
				nextEventTime = trafficTime
				isTrafficEvent = true
			} else {
				nextEventTime = heapEventTime
				isTrafficEvent = false
			}
		} else if !heapEventTime.IsZero() {
			nextEventTime = heapEventTime
			isTrafficEvent = false
		} else if !trafficTime.IsZero() {
			nextEventTime = trafficTime
			isTrafficEvent = true
		} else {
			break // No events left
		}

		if nextEventTime.After(endTime) {
			s.clock.SetTime(endTime)
			break
		}

		// If time is about to advance, it means we are done with the current batch of simultaneous events.
		// This is the most efficient time to dispatch.
		if nextEventTime.After(s.clock.Now()) {
			s.attemptDispatch()
		}

		// B. Advance Clock
		s.clock.SetTime(nextEventTime)

		// C. Process Event
		if isTrafficEvent {
			req := s.trafficGen.GenerateNext()
			s.buffer.Push(req)
		} else {
			evt := heap.Pop(s.eventQueue).(*SimEvent)
			s.processSystemEvent(evt)
			s.freeEvent(evt)
		}

		// D. Fast Path Dispatch Check
		s.attemptDispatch()
	}
}

func (s *SimulationEnvironment) CurrentTime() time.Time {
	return s.clock.Now()
}

// --- Event Processing Logic ---

func (s *SimulationEnvironment) processSystemEvent(evt *SimEvent) {
	switch evt.Type {
	case EventBackendFinish:
		if b, ok := s.backends[evt.SourceID]; ok {
			b.isPending = false
			s.processBackendFinish(b)
		}
	case EventControllerTick:
		s.Controller.Reconcile()
		s.scheduleEvent(EventControllerTick, s.clock.Now().Add(s.cfg.RecorderConfig.TickInterval), "")
	case EventScrape:
		s.processScrape()
		s.scheduleEvent(EventScrape, s.clock.Now().Add(s.cfg.ScrapeInterval), "")
	case EventPacerPoll:
		// No-op. Just wakes up the loop.
	}
}

// processBackendFinish now takes the direct pointer to avoid map lookups
func (s *SimulationEnvironment) processBackendFinish(backend *simBackend) {
	// 1. Harvest Completions
	done := backend.DrainCompletions()
	if len(done) > 0 {
		ctx := context.Background()
		for _, req := range done {
			s.Recorder.ResponseComplete(ctx, nil, nil, backend.podObj)
			s.completedRequests = append(s.completedRequests, req)
		}
	}

	// 2. Advance Physics
	backend.Tick(s.clock.Now())

	// 3. Schedule next wake-up only if work remains
	state := backend.GetState()
	if state.RunningRequests > 0 || state.QueueDepth > 0 {
		stepDuration := max(backend.NextStepDuration(), 1*time.Millisecond)
		s.scheduleEvent(EventBackendFinish, s.clock.Now().Add(stepDuration), backend.podObj.NamespacedName.Name)
	}
}

func (s *SimulationEnvironment) processScrape() {
	for id, b := range s.backends {
		sysState := b.GetState()
		s.datastore.Update(id, sysState.QueueDepth, sysState.Utilization, s.clock.Now())
	}
	s.recordSnapshot()
}

// --- Dispatch Logic ---

func (s *SimulationEnvironment) attemptDispatch() {
	for s.buffer.Len() > 0 {
		// 1. Global Rate Limiting
		if !s.Controller.ShouldDispatch(1.0) {
			delay := s.Controller.GetPacer().TimeUntilReady(1.0)
			if delay <= 0 {
				delay = 10 * time.Millisecond
			}
			s.scheduleEvent(EventPacerPoll, s.clock.Now().Add(delay), "")
			return
		}

		req := s.buffer.Pop()

		// 2. Candidate Selection
		// Reset scratch buffer length, keep capacity
		s.scratchCandidates = s.scratchCandidates[:0]

		// Iterate sorted keys for deterministic behavior.
		for _, id := range s.cachedPodIDs {
			b := s.backends[id]
			s.scratchCandidates = append(s.scratchCandidates, &backendmetrics.FakePodMetrics{
				Pod: b.podObj,
			})
		}

		// // 3. Safety Filter
		ctx := context.Background()
		filtered := s.Controller.Filter(ctx, nil, nil, s.scratchCandidates)

		if len(filtered) == 0 {
			s.shedRequests++
			return
		}

		// 4. Scoring
		scores := s.Controller.Score(ctx, nil, nil, filtered)
		s.scratchScored = s.scratchScored[:0]
		for pod, score := range scores {
			s.scratchScored = append(s.scratchScored, &schedtypes.ScoredPod{
				Pod:   pod,
				Score: score,
			})
		}

		// 5. Selection
		result := s.Picker.Pick(ctx, nil, s.scratchScored)
		if result == nil || len(result.TargetPods) == 0 {
			s.shedRequests++
			return
		}

		targetID := result.TargetPods[0].GetPod().NamespacedName.Name

		// Notify controller.
		s.Recorder.PreRequest(ctx, nil, &schedtypes.SchedulingResult{
			ProfileResults: map[string]*schedtypes.ProfileRunResult{
				"default": result,
			},
			PrimaryProfileName: "default",
		})

		// Submit to plant.
		req.ScheduleTime = s.clock.Now()
		s.backends[targetID].Submit(req)

		// Wake up backend immediately.
		s.scheduleEvent(EventBackendFinish, s.clock.Now().Add(10*time.Microsecond), targetID)
	}
}

// --- Injection Implementation ---

func (s *SimulationEnvironment) SetTrafficProfile(p WorkloadProfile) {
	s.trafficProfile = p
	s.recalcGeneratorRate()
}

func (s *SimulationEnvironment) SetRelativeLoad(loadFactor float64) {
	s.targetLoadRatio = loadFactor
	s.recalcGeneratorRate()
}

func (s *SimulationEnvironment) AddBackends(count int) {
	currentCount := len(s.backends)
	for i := 0; i < count; i++ {
		podID := fmt.Sprintf("sim-pod-%d", currentCount+i)
		b := s.generator(podID)
		s.addExistingBackend(podID, b)
	}
	s.refreshCachedIDs()
	s.recalcGeneratorRate()
}

func (s *SimulationEnvironment) DegradeBackend(podID string, factor float64) {
	if b, ok := s.backends[podID]; ok {
		b.SetTimeDilation(factor)
	}
}

// --- Telemetry ---

func (s *SimulationEnvironment) recordSnapshot() {
	ctrlState := s.Controller.Introspect()

	podPhysics := make(map[string]SystemState, len(s.backends))
	totalInflight := 0
	totalQueueDepth := 0
	totalUtilization := 0.0

	for id, b := range s.backends {
		state := b.GetState()
		state.TrueServiceRate = b.EstimateCapacity(s.trafficProfile).MaxThroughputQPS
		podPhysics[id] = state
		totalInflight += state.RunningRequests
		totalQueueDepth += state.QueueDepth
		totalUtilization += state.Utilization
	}

	avgBackendQueue := 0.0
	avgBackendUtil := 0.0
	if len(s.backends) > 0 {
		count := float64(len(s.backends))
		avgBackendQueue = float64(totalQueueDepth) / count
		avgBackendUtil = totalUtilization / count
	}

	s.history = append(s.history, Snapshot{
		Timestamp:                 s.clock.Now(),
		ControllerState:           ctrlState,
		FlowControlQueueDepth:     s.buffer.Len(),
		TotalInflight:             totalInflight,
		AverageBackendQueueDepth:  avgBackendQueue,
		AverageBackendUtilization: avgBackendUtil,
		PodPhysics:                podPhysics,
	})
}

func (s *SimulationEnvironment) GetResults() *SimResult {
	return &SimResult{
		Duration:          s.clock.Now().Sub(s.startTime),
		TotalRequests:     len(s.completedRequests) + s.shedRequests + s.buffer.Len(),
		ShedRequestCount:  s.shedRequests,
		Timeline:          s.history,
		CompletedRequests: s.completedRequests,
	}
}

// --- Helpers ---

func (s *SimulationEnvironment) addExistingBackend(id string, b Backend) {
	wrapper := &simBackend{
		Backend:   b,
		podObj:    newBackendPod(id),
		isPending: false,
	}
	s.backends[id] = wrapper
	s.refreshCachedIDs()

	// Ensure scratch buffers can accommodate new size.
	if cap(s.scratchCandidates) < len(s.backends) {
		s.scratchCandidates = make([]schedtypes.Pod, 0, len(s.backends)*2)
	}

	s.datastore.Update(id, 0, 0.0, s.clock.Now())
	s.scheduleEvent(EventBackendFinish, s.clock.Now(), id)
}

func (s *SimulationEnvironment) refreshCachedIDs() {
	keys := make([]string, 0, len(s.backends))
	for k := range s.backends {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s.cachedPodIDs = keys
}

func (s *SimulationEnvironment) GetPodIDs() []string {
	return s.cachedPodIDs
}

func (s *SimulationEnvironment) recalcGeneratorRate() {
	totalCapacity := 0.0
	for _, b := range s.backends {
		// Ask the backend for its theoretical limit given the current profile.
		totalCapacity += b.EstimateCapacity(s.trafficProfile).MaxThroughputQPS
	}
	// If no backends or zero capacity, default to a safe low number to prevent divide-by-zero.
	if totalCapacity == 0 {
		totalCapacity = 1.0
	}
	targetQPS := totalCapacity * s.targetLoadRatio
	s.trafficGen.SetRate(targetQPS)
}

// --- Event Management (Pooling & Heap) ---

func (s *SimulationEnvironment) scheduleEvent(kind SimEventType, at time.Time, sourceID string) {
	// 1. Debounce Check
	if kind == EventBackendFinish && sourceID != "" {
		if b, ok := s.backends[sourceID]; ok {
			if b.isPending {
				return // Already scheduled
			}
			b.isPending = true
		}
	}

	// 2. Allocation-Free Event Creation
	evt := s.newEvent()
	evt.Timestamp = at
	evt.Type = kind
	evt.SourceID = sourceID

	heap.Push(s.eventQueue, evt)
}

func (s *SimulationEnvironment) ensureScheduled(kind SimEventType, interval time.Duration) {
	s.scheduleEvent(kind, s.clock.Now().Add(interval), "")
}

func (s *SimulationEnvironment) newEvent() *SimEvent {
	if len(s.eventPool) == 0 {
		return &SimEvent{}
	}
	// Pop from stack.
	n := len(s.eventPool)
	evt := s.eventPool[n-1]
	s.eventPool = s.eventPool[:n-1]
	return evt
}

func (s *SimulationEnvironment) freeEvent(evt *SimEvent) {
	// Zero out fields to avoid dangling data.
	evt.SourceID = ""
	s.eventPool = append(s.eventPool, evt)
}

type SimEventType int

const (
	EventBackendFinish SimEventType = iota
	EventControllerTick
	EventScrape
	EventPacerPoll
)

type SimEvent struct {
	Timestamp time.Time
	Type      SimEventType
	SourceID  string
	index     int
}

type EventQueue []*SimEvent

func (pq EventQueue) Len() int           { return len(pq) }
func (pq EventQueue) Less(i, j int) bool { return pq[i].Timestamp.Before(pq[j].Timestamp) }
func (pq EventQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}
func (pq *EventQueue) Push(x interface{}) {
	item := x.(*SimEvent)
	item.index = len(*pq)
	*pq = append(*pq, item)
}
func (pq *EventQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*pq = old[0 : n-1]
	return item
}

func (pq *EventQueue) Peek() *SimEvent {
	if len(*pq) == 0 {
		return nil
	}
	return (*pq)[0]
}

// --- MockQueueMonitor ---

type MockQueueMonitor struct {
	Buffer *FlowControlBuffer
}

func (m *MockQueueMonitor) GetTotalPendingRequests() int {
	return m.Buffer.Len()
}

// --- GreedyPicker (Simple Delegate) ---

type GreedyPicker struct{}

func (p *GreedyPicker) TypedName() plugins.TypedName {
	return plugins.TypedName{Type: "GreedyPicker", Name: "GreedyPicker"}
}

func (p *GreedyPicker) Pick(ctx context.Context, state *schedtypes.CycleState, scored []*schedtypes.ScoredPod) *schedtypes.ProfileRunResult {
	if len(scored) == 0 {
		return nil
	}
	bestPod := scored[0]
	bestScore := scored[0].Score
	for i := 1; i < len(scored); i++ {
		if scored[i].Score > bestScore {
			bestScore = scored[i].Score
			bestPod = scored[i]
		}
	}
	return &schedtypes.ProfileRunResult{TargetPods: []schedtypes.Pod{bestPod.Pod}}
}

// FlowControlBuffer using a Ring Buffer to replace container/list.
type FlowControlBuffer struct {
	buffer []*Request
	head   int
	tail   int
	count  int
	size   int
}

func NewFlowControlBuffer() *FlowControlBuffer {
	initialSize := 1024
	return &FlowControlBuffer{
		buffer: make([]*Request, initialSize),
		size:   initialSize,
	}
}

func (b *FlowControlBuffer) Push(req *Request) {
	if b.count == b.size {
		b.resize()
	}
	b.buffer[b.tail] = req
	b.tail = (b.tail + 1) % b.size
	b.count++
}

func (b *FlowControlBuffer) Len() int { return b.count }

func (b *FlowControlBuffer) Peek() *Request {
	if b.count == 0 {
		return nil
	}
	return b.buffer[b.head]
}

func (b *FlowControlBuffer) Pop() *Request {
	if b.count == 0 {
		return nil
	}
	req := b.buffer[b.head]
	b.buffer[b.head] = nil // Aid GC
	b.head = (b.head + 1) % b.size
	b.count--
	return req
}

func (b *FlowControlBuffer) resize() {
	newSize := b.size * 2
	newBuf := make([]*Request, newSize)
	if b.head < b.tail {
		copy(newBuf, b.buffer[b.head:b.tail])
	} else {
		copy(newBuf, b.buffer[b.head:])
		copy(newBuf[b.size-b.head:], b.buffer[:b.tail])
	}
	b.head = 0
	b.tail = b.count
	b.size = newSize
	b.buffer = newBuf
}

// --- Mocks ---

func newBackendPod(id string) *backend.Pod {
	return &backend.Pod{
		NamespacedName: types.NamespacedName{Name: id, Namespace: "default"},
	}
}

type MockDatastore struct {
	metrics map[string]backendmetrics.PodMetrics
}

func NewMockDatastore() *MockDatastore {
	return &MockDatastore{metrics: make(map[string]backendmetrics.PodMetrics)}
}

func (m *MockDatastore) Update(podID string, qDepth int, kvUtil float64, now time.Time) {
	m.metrics[podID] = &backendmetrics.FakePodMetrics{
		Pod: newBackendPod(podID),
		Metrics: &backendmetrics.MetricsState{
			WaitingQueueSize:    qDepth,
			KVCacheUsagePercent: kvUtil,
			UpdateTime:          now,
		},
	}
}

func (m *MockDatastore) PodList(_ func(backendmetrics.PodMetrics) bool) []backendmetrics.PodMetrics {
	return slices.Collect(maps.Values(m.metrics))
}
