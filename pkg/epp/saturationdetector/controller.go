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

package saturationdetector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"

	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

const SaturationControllerType = "SaturationController"

// pacerRateMax represents an effectively infinite rate for the Pacer.
// Used in the Probing regime to bypass rate-limiting, shifting control to the concurrency limit.
const pacerRateMax = math.MaxFloat64

// QueueMonitor provides a read-only view into the Flow Control layer's pending work.
type QueueMonitor interface {
	// GetTotalPendingRequests returns the instantaneous count of buffered requests.
	GetTotalPendingRequests() int
}

// Datastore provides a read-only view of ready pods.
type Datastore interface {
	// PodList returns a filtered view of ready pods.
	PodList(predicate func(backendmetrics.PodMetrics) bool) []backendmetrics.PodMetrics
}

func init() {
	plugins.RegisterWithMetadata(SaturationControllerType, plugins.PluginRegistration{
		Factory:   SaturationControllerFactory,
		Lifecycle: plugins.LifecycleSingleton,
	})
}

// SaturationControllerFactory defines the factory function for SaturationController.
func SaturationControllerFactory(name string, _ json.RawMessage, handle plugins.Handle) (plugins.Plugin, error) {
	config := &ControllerConfig{}
	config.setDefaults() // TODO: Bind from JSON parameters
	if err := config.validate(); err != nil {
		return nil, err
	}

	recorder, err := plugins.PluginByType[*SaturationSignalRecorder](handle, config.SignalRecorderPluginName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve dependency '%s': %w", config.SignalRecorderPluginName, err)
	}

	// TODO: Resolve QueueMonitor from handle
	var qm QueueMonitor

	return NewSaturationController(config, recorder, qm, handle, WithName(name)), nil
}

// poolMetrics provides a snapshot of the aggregate system state for the feedback loop.
type poolMetrics struct {
	// avgSaturation is the Process Variable (PV).
	// Represents average Supply Pressure (Load/Capacity).
	avgSaturation float64

	// aggregateEffectiveServiceRate is the Feed-Forward Term (u_ff).
	// Represents estimated total throughput (^μ_pool).
	aggregateEffectiveServiceRate float64

	// loadIndex is the Unified Pressure Signal [0, 1+].
	loadIndex float64
}

// podTickData holds a consistent atomic snapshot of a pod for a single reconcile tick.
type podTickData struct {
	state *podState
	lt    uint64 // Snapshot of Inflight (L_t)
}

// ControllerState captures the global state of the "Brain".
type ControllerState struct {
	// FSM State
	Regime      Regime
	MetricRound uint64

	// Global Metrics
	LoadIndex       float64
	AvgSaturation   float64 // The PV
	ErrorSignal     float64 // Setpoint - PV
	FeedForwardTerm float64 // u_ff
	FeedBackTerm    float64 // u_fb

	// Actuation Outputs
	PacerRate    float64 // Only valid in Regulating regime; otherwise, -1
	ProbingLimit float64 // Only valid in Probing regime; otherwise, -1

	// Per-Pod Visibility
	Pods map[string]PodSnapshot
}

// SaturationController is the "Brain" of the Cybernetic Flow Control system.
//
// It implements a Hybrid Control System that transitions between two regimes:
//  1. Probing (Discovery): A concurrency-limited "Hill Climbing" mode.
//  2. Regulating (Steady State): A 2-DOF (Feed-Forward + Feedback) Rate Controller.
type SaturationController struct {
	typedName    plugins.TypedName
	config       *ControllerConfig
	pacer        *Pacer
	recorder     *SaturationSignalRecorder
	queueMonitor QueueMonitor
	datastore    Datastore
	log          logr.Logger
	clock        clock.WithTicker

	// mu protects the shared control state.
	mu sync.RWMutex

	// pods holds the Internal Model for every backend replica.
	pods map[string]*podState

	// regime is the current state of the FSM.
	regime Regime

	// round is the logical clock for windowed estimators.
	round uint64

	// cachedProbingLimit stores the pre-calculated pool-wide concurrency limit.
	// O(1) access for ShouldDispatch.
	cachedProbingLimit float64

	// cachedLoadIndex stores the latest calculated system pressure.
	// O(1) access for AdaptiveScorer.
	cachedLoadIndex float64

	// cachedBaseProbeProb is the pre-calculated baseline probability (1/N).
	// O(1) access for ProbePicker.
	cachedBaseProbeProb float64

	// Cached control variables for O(1) access for Introspect.
	cachedFeedforwardTerm float64
	cachedFeedbackTerm    float64
	cachedSaturation      float64
}

type SaturationControllerOption func(*SaturationController)

func NewSaturationController(
	config *ControllerConfig,
	recorder *SaturationSignalRecorder,
	qm QueueMonitor,
	ds Datastore,
	options ...SaturationControllerOption,
) *SaturationController {
	sc := &SaturationController{
		typedName:    plugins.TypedName{Type: SaturationControllerType, Name: SaturationControllerType},
		config:       config,
		recorder:     recorder,
		queueMonitor: qm,
		datastore:    ds,
		pods:         make(map[string]*podState),
		clock:        clock.RealClock{},
	}
	for _, opt := range options {
		opt(sc)
	}
	sc.pacer = NewPacer(sc.config.MinDispatchRate, DefaultPacerBurstDuration, sc.clock)
	return sc
}

func WithName(name string) SaturationControllerOption {
	return func(sc *SaturationController) {
		sc.typedName.Name = name
	}
}

func WithClock(clock clock.WithTicker) SaturationControllerOption {
	return func(sc *SaturationController) {
		sc.clock = clock
	}
}

// TypedName returns the type and name of the plugin instance.
func (sc *SaturationController) TypedName() plugins.TypedName {
	return sc.typedName
}

// Start begins the main Control Loop.
func (sc *SaturationController) Start(ctx context.Context) {
	interval := sc.recorder.TickInterval()
	sc.log.Info("Starting Saturation Controller", "tick_interval", interval)
	go func() {
		ticker := sc.clock.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				sc.log.Info("Stopping Saturation Controller")
				return
			case <-ticker.C():
				sc.Reconcile()
			}
		}
	}()
}

// Reconcile executes one tick of the Control Loop (Slow Path).
func (sc *SaturationController) Reconcile() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.round++

	now := sc.clock.Now()
	allReadyPods := sc.datastore.PodList(backendmetrics.AllPodsPredicate)

	livePodsData := make(map[string]podTickData)
	currentPodSet := sets.New[string]()

	// 1. Ingest Sensor Data
	completionsByPod := sc.ingestRecorderData()

	// 2. Update Internal Model
	for _, m := range allReadyPods {
		podID := m.GetPod().NamespacedName.String()
		currentPodSet.Insert(podID)

		if now.Sub(m.GetMetrics().UpdateTime) > sc.config.MetricsStalenessThreshold {
			sc.log.V(1).Info("Excluding pod due to stale metrics", "pod", podID)
			continue
		}

		state, exists := sc.pods[podID]
		if !exists {
			state = newPodState(m.GetPod().NamespacedName, sc.config, sc.recorder.TickInterval(), now, sc.round)
			sc.pods[podID] = state
		}

		// Snapshot instantaneous state.
		lt := sc.recorder.ConcurrencyTracker().Get(podID)
		qt := float64(m.GetMetrics().WaitingQueueSize)
		livePodsData[podID] = podTickData{state: state, lt: lt}

		// Update Estimators.
		sc.updatePodState(state, now, lt, qt, completionsByPod[podID])
	}

	// 3. Garbage Collection
	sc.garbageCollectPods(currentPodSet)

	// 4. Calculate Control Output
	sc.updateControllerState(now, livePodsData)
}

// ingestRecorderData drains the completion event buffer from the signal recorder.
func (sc *SaturationController) ingestRecorderData() map[string][]completionEvent {
	completions := sc.recorder.DrainCompletions()
	droppedCounts := sc.recorder.DrainDroppedCounts()

	for podID, count := range droppedCounts {
		sc.log.V(0).Info("Critical: Dropped completion events", "pod", podID, "count", count)
	}

	completionsByPod := make(map[string][]completionEvent)
	for _, event := range completions {
		completionsByPod[event.podID] = append(completionsByPod[event.podID], event)
	}
	return completionsByPod
}

// updatePodState refines the statistical model for a single pod.
func (sc *SaturationController) updatePodState(
	state *podState,
	now time.Time,
	requestsInFlight uint64,
	queueDepth float64,
	completions []completionEvent,
) {
	// 1. Activity Detection
	isActive := (requestsInFlight > 0) || (len(completions) > 0)
	state.UpdateMaturity(now, sc.config.DormantTimeout, isActive)

	// 2. Signal Updates
	state.queueDepthEWMA.Add(queueDepth)
	state.peakInflightConcurrency.Update(float64(requestsInFlight), sc.round)

	for _, event := range completions {
		state.serviceRateEWMA.Add(event.timestamp, 1.0)
	}

	// 3. Effective Batch Capacity Sampling
	// Sample only when saturated to capture true physical limits.
	// Enforce cooldown to ensure statistical independence.
	if queueDepth > 0 && now.Sub(state.lastBatchSampleTime) > sc.config.MinBatchSampleInterval {
		if bEffSample := float64(requestsInFlight) - queueDepth; bEffSample > 0 {
			state.effectiveBatchEWMA.Add(bEffSample)
			state.lastBatchSampleTime = now
		}
	}
}

func (sc *SaturationController) garbageCollectPods(currentPods sets.Set[string]) {
	for podID := range sc.pods {
		if !currentPods.Has(podID) {
			delete(sc.pods, podID)
			// TODO: Clean up concurrency tracker
		}
	}
}

// updateControllerState executes the decision logic for the FSM.
func (sc *SaturationController) updateControllerState(now time.Time, livePodsData map[string]podTickData) {
	newRegime := sc.determineRegime(livePodsData)
	if newRegime != sc.regime {
		sc.log.Info("Regime Transition", "from", sc.regime.String(), "to", newRegime.String())
		sc.regime = newRegime
	}

	metrics := sc.calculatePoolMetrics(now, livePodsData)
	sc.cachedLoadIndex = metrics.loadIndex
	sc.cachedSaturation = metrics.avgSaturation
	sc.cachedFeedforwardTerm = metrics.aggregateEffectiveServiceRate

	switch sc.regime {
	case Halted:
		sc.pacer.SetRate(0.0)

	case Probing:
		sc.pacer.SetRate(pacerRateMax)
		var totalLimit float64
		for _, data := range livePodsData {
			totalLimit += getPodConcurrencyLimit(data.state, sc.config)
		}
		sc.cachedProbingLimit = totalLimit

	case Regulating:
		// 1. Feedback Error: e(t) = SP - PV
		errorSignal := sc.config.SaturationSetpoint - metrics.avgSaturation

		// 2. Correction: u_fb(t) = Capacity * Kp * e(t)
		correction := metrics.aggregateEffectiveServiceRate * sc.config.ProportionalGain * errorSignal

		// 3. Control Output: u(t) = u_ff + u_fb
		targetRate := metrics.aggregateEffectiveServiceRate + correction
		sc.pacer.SetRate(max(sc.config.MinDispatchRate, targetRate))

		sc.cachedFeedbackTerm = correction
	}
}

// determineRegime checks if the pool has sufficient maturity to run the P-Controller.
func (sc *SaturationController) determineRegime(livePodsData map[string]podTickData) Regime {
	if len(livePodsData) == 0 {
		return Halted
	}

	var numQuorumPods, numActivePods int
	for _, data := range livePodsData {
		if data.state.maturity != Dormant {
			numActivePods++
		}
		if data.state.maturity == Mature || data.state.maturity == Maturing {
			numQuorumPods++
		}
	}

	// Calculate Base Probe Probability (Floor at 5%)
	sc.cachedBaseProbeProb = max(0.05, 1.0/float64(max(1, numQuorumPods)))

	if numActivePods == 0 {
		return Halted
	}

	maturityRatio := float64(numQuorumPods) / float64(numActivePods)
	if maturityRatio >= sc.config.MaturityQuorumPercentage {
		return Regulating
	}
	return Probing
}

// calculatePoolMetrics computes the Feed-Forward term, Feedback term, and Load Index.
func (sc *SaturationController) calculatePoolMetrics(now time.Time, livePodsData map[string]podTickData) poolMetrics {
	if len(livePodsData) == 0 {
		return poolMetrics{}
	}

	// 1. Calculate Pool Characteristic Batch Latency (W_pool)
	// Used for seeding Maturing pods.
	// Formula: W_pool = Sum(^B_eff) / Sum(^μ)
	//
	// This calculates intrinsic Service Latency (time processing), NOT Response Latency (time processing + waiting).
	// By using ^B_eff (active batch) rather than Total Inflight, we exclude the time spent waiting in the queue.
	// This prevents "Queue Pollution" where a congested pool would falsely appear to have slow hardware.
	var totalMatureB, totalMatureCr float64
	for _, data := range livePodsData {
		if data.state.IsEffectiveBatchMature() && data.state.IsServiceRateMature(now) {
			if bEff := data.state.effectiveBatchEWMA.Get(); bEff > 0 {
				totalMatureB += bEff
				totalMatureCr += data.state.serviceRateEWMA.Rate(now)
			}
		}
	}

	var poolAvgBatchLatency float64
	if totalMatureB > 0 && totalMatureCr > 0 {
		poolAvgBatchLatency = totalMatureB / totalMatureCr
	}

	// 2. Calculate Control Variables
	var totalSaturation, aggregateServiceRate float64
	for _, data := range livePodsData {
		// PV: Supply Pressure (Raw pressure, can be > 1.0 for strong error signals)
		totalSaturation += data.state.Saturation(data.lt)
		// FF: Aggregate Estimated Capacity
		aggregateServiceRate += data.state.EffectiveServiceRate(now, poolAvgBatchLatency)
	}

	avgSaturation := totalSaturation / float64(len(livePodsData))

	// 3. Calculate Load Index (LI)
	// LI = (w_s * S_p) + (w_q * P_q)

	// Demand Pressure (P_q) = WaitTime / Budget
	pq := 0.0
	pending := sc.queueMonitor.GetTotalPendingRequests()
	if pending > 0 && aggregateServiceRate > 0 {
		wExpected := float64(pending) / aggregateServiceRate // Expected Wait Time: W = L / λ
		tBudget := sc.config.MaxQueueLatency.Seconds()       // Budget: Max Configured Wait Time (Contract)
		pq = wExpected / tBudget
	}

	// Weighted Sum Blend (0.5/0.5)
	// S_p is clamped to 1.0 for the Load Index to maintain normalization.
	loadIndex := (0.5 * min(1.0, avgSaturation)) + (0.5 * pq)

	return poolMetrics{
		avgSaturation:                 avgSaturation,
		aggregateEffectiveServiceRate: aggregateServiceRate,
		loadIndex:                     loadIndex,
	}
}

// --- External Accessors (Thread-Safe) ---

// Introspect returns a thread-safe, deep copy of the controller's internal state.
func (sc *SaturationController) Introspect() ControllerState {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	// 1. Snapshot Pods
	podSnapshots := make(map[string]PodSnapshot, len(sc.pods))
	for id, state := range sc.pods {
		lt := sc.recorder.ConcurrencyTracker().Get(id)
		podSnapshots[id] = state.Snapshot(sc.clock.Now(), sc.config, lt)
	}

	// 2. Snapshot Global State
	return ControllerState{
		Regime:          sc.regime,
		MetricRound:     sc.round,
		LoadIndex:       sc.cachedLoadIndex,
		AvgSaturation:   sc.cachedSaturation,
		ErrorSignal:     sc.config.SaturationSetpoint - sc.cachedSaturation,
		FeedForwardTerm: sc.cachedFeedforwardTerm,
		FeedBackTerm:    sc.cachedFeedbackTerm,
		PacerRate:       sc.pacer.GetRate(),
		ProbingLimit:    sc.cachedProbingLimit,
		Pods:            podSnapshots,
	}
}

func (sc *SaturationController) GetPacer() *Pacer {
	return sc.pacer
}

// GetLoadIndex returns the current system pressure score [0.0, 1.0+].
func (sc *SaturationController) GetLoadIndex() float64 {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.cachedLoadIndex
}

// GetProbeCandidates returns the list of pods eligible for probing.
func (sc *SaturationController) GetProbeCandidates() sets.Set[string] {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	immaturePods := sets.New[string]()
	for id, state := range sc.pods {
		if state.maturity == Immature {
			immaturePods.Insert(id)
		}
	}

	if immaturePods.Len() == 0 {
		return nil
	}

	switch sc.regime {
	case Probing:
		// Parallel Mode: Return all for Round-Robin discovery.
		return immaturePods
	case Regulating:
		// Serial Mode: Focus fire on the oldest immature pod.
		immaturePodsList := immaturePods.UnsortedList()
		oldest := immaturePodsList[0]
		oldestTime := sc.pods[oldest].enteredImmatureStateAt
		for _, id := range immaturePodsList[1:] {
			t := sc.pods[id].enteredImmatureStateAt
			if t.Before(oldestTime) {
				oldest = id
				oldestTime = t
			}
		}
		return sets.New(oldest)
	}
	return sets.New[string]()
}

// GetProbeProbability calculates the admission probability for a specific probe target.
//
// The logic implements "Confidence-Based Acceleration":
//  1. Start with a "Fair Share" baseline (1/N) to avoid overwhelming the cold pod.
//  2. Enforce a "Starvation Floor" (5%) to guarantee minimum discovery velocity.
//  3. Apply a "Confidence Ramp": As we collect valid samples (^B_eff), we linearly increase traffic up to 2x the
//     baseline. This creates a positive feedback loop that accelerates finalization as the pod proves its stability.
//
// Formula: P = BaseProb * (1.0 + (Progress * (Aggressiveness - 1.0)))
func (sc *SaturationController) GetProbeProbability(podID string) float64 {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	state, ok := sc.pods[podID]
	if !ok {
		return 0.0
	}

	// 1. Retrieve Cached Baseline (Calculated in Slow Path)
	// This represents the "Natural Duty Cycle" of a pod in this pool, clamped to 5%.
	baseProb := sc.cachedBaseProbeProb

	// 2. Calculate Maturity Progress (0.0 -> 1.0)
	// Progress = CurrentSamples / RequiredSamples
	samples := state.effectiveBatchEWMA.SampleCount()
	required := sc.config.MinSamplesForEffectiveBatchMaturity
	progress := math.Min(1.0, float64(samples)/float64(required))

	// 3. Apply Confidence Ramp
	// We use a multiplier of 2.0.
	// - At 0 samples: Multiplier = 1.0 (Conservative).
	// - At N samples: Multiplier = 2.0 (Aggressive).
	// This ensures that as we approach graduation, we push harder to cross the finish line.
	confidenceMultiplier := 2.0
	ramp := 1.0 + (progress * (confidenceMultiplier - 1.0))
	return baseProb * ramp
}

// --- Dispatch Logic (Fast Path) ---

func (sc *SaturationController) ShouldDispatch(cost float64) bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	switch sc.regime {
	case Halted:
		return false
	case Probing:
		// TODO: Optimize O(N) loop with atomic global tracker
		var ltPool float64
		for podID := range sc.pods {
			ltPool += float64(sc.recorder.ConcurrencyTracker().Get(podID))
		}
		return (ltPool + cost) <= sc.cachedProbingLimit
	case Regulating:
		return sc.pacer.Allow(cost)
	default:
		sc.log.Error(nil, "Entered unknown regime", "regime", sc.regime)
		return false
	}
}

// getPodConcurrencyLimit calculates the current safe concurrency limit for a single pod based on its maturity.
func getPodConcurrencyLimit(p *podState, config *ControllerConfig) float64 {
	switch p.maturity {
	case Mature, Maturing:
		// Headroom = Capacity * (1 + Setpoint)
		return p.effectiveBatchEWMA.Get() * (1 + config.SaturationSetpoint)
	case Immature, Dormant:
		// Discovery Mode (Hill Climbing):
		// We use the "Best Seen" concurrency (L_peak) as the baseline.
		limit, _ := p.peakInflightConcurrency.Get()

		// SAFETY BRAKE:
		// If the pod has a sustained queue, it indicates we have exceeded the physical service rate of the backend
		// (Little's Law violation).
		// We stop climbing while waiting for the maturity samples to collect.
		if p.queueDepthEWMA.Get() > 3.0 {
			// Hold the current limit (L_peak).
			return limit
		}

		// If the queue is empty, we assume there is still headroom.
		// Incrementally explore higher concurrency.
		return limit + 1.0
	}
	return 1.0 // Failsafe
}
