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

// Package saturationdetector provides a dynamic, request-agnostic mechanism to manage load and prevent congestion on
// backend model servers.
//
// # Core Design: A Bang-Bang Controller over Queuing Theory Models
//
// This package implements a Bang-Bang Controller (https://en.wikipedia.org/wiki/Bang-bang_control), a type of feedback
// controller that acts as a circuit breaker. It provides a deterministic saturation signal to stabilize backend
// utilization without requiring complex tuning or apriori estimation of maximum serving capacity.
//
// The controller's intelligence is derived from queuing theory models (https://en.wikipedia.org/wiki/Queueing_theory),
// treating each backend pod as a black-box "server."
//
// # Key Mechanisms
//
//  1. Utilization Law (The Control Signal):
//     The controller's primary input is an aggregate "Subset Utilization" (ρ_subset). This is based on the Utilization
//     Law extended to a multi-server system: the ratio of offered load to available effective capacity
//     (ρ_subset = Σλᵢ / Σμᵢ), where μᵢ = 1 / E[S_effᵢ].
//
//  2. M/G/1 Queue Model (Predictive Analysis):
//     For per-pod analysis, the detector uses the Pollaczek-Khinchine formula based on the M/G/1 queue model
//     (https://en.wikipedia.org/wiki/M/G/1_queue).
//     This model is chosen because the 'G' (General distribution) correctly accounts for the high-variance service
//     times inherent in LLM workloads.
//
//  3. Hysteresis (Preventing Oscillations):
//     To prevent rapid oscillations (chatter) around the setpoint, the controller uses two thresholds:
//     - High Watermark (TargetUtilization): Blocking engages when ρ_subset exceeds this threshold.
//     - Low Watermark (ResumeUtilization): Dispatch resumes only when ρ_subset drops below this threshold.
//
//  4. Stateful Probing (Deadlock Prevention):
//     If the metrics used to calculate utilization are unreliable (stale or insufficient samples), the controller can
//     deadlock (due to metric freeze). A probing mechanism prevents this by periodically forcing a single
//     request (at a fixed ProbeInterval) to gather fresh data, ensuring recovery.
//
// # The Black-Box Abstraction and "Effective Service Time"
//
// We treat the entire backend pod (including its internal queues and batching mechanisms) as a black box. To model this
// complex system using standard queuing formulas, we employ an abstraction called "Effective Service Time" (E[S_eff]).
//
// We use the measured Mean Sojourn Time (end-to-end latency) as the input for E[S] (Mean Service Time). While this
// differs from the classical definition (which excludes waiting time), it is a standard technique for modeling complex
// systems, known technically as a "Flow Equivalent Server" model.
//
// This abstraction ensures the control loop is desirable: if the pod's responsiveness drops for any reason, E[S_eff]
// increases, utilization (ρ = λ * E[S_eff]) rises, and the controller reacts.
//
// Consequence: The predictive latency metrics (PMST, W_q) derived from this abstraction have a conservative bias
// (they may overestimate absolute latency) but serve as robust relative indicators of congestion.
//
// # Architectural Impact
//
// These models are used to generate a unified FullnessReport that serves:
//
//   - The Flow Controller layer's stability-oriented control loop (HoL blocking).
//   - The request control layer's request shedding admission control decisions.
//   - Optionally, the Scheduling layer's performance-oriented optimization logic (latency-aware routing).
//   - Autoscaling and load balancing decisions based on pool or pool-subset fullness.
package saturationdetector

import (
	"context"
	"maps"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

// overloadedLatency is a sentinel value used when a pod is in an unstable state (ρ >= 1), where latency theoretically
// trends towards infinity.
var overloadedLatency = time.Hour * 24

// BangBangControllerInternals exposes the internal state of the Bang-Bang Controller for observability.
type BangBangControllerInternals struct {
	// TargetUtilization is the High Watermark (Upper Threshold).
	TargetUtilization float64
	// ResumeUtilization is the Low Watermark (Lower Threshold).
	ResumeUtilization float64
	// CurrentUtilization is the measured aggregate utilization of the subset (ρ_subset).
	CurrentUtilization float64
	// IsSaturated is the deterministic state of the controller (true if blocking is engaged).
	IsSaturated bool
}

// PodFullnessDetails encapsulates the calculated metrics for a single pod using the M/G/1 model.
// It provides a predictive view of a pod's congestion state.
type PodFullnessDetails struct {
	// --- Core Predictive Metrics (M/G/1 Model Outputs) ---

	// PredictedMeanSojournTime (PMST) is the model's prediction of the total end-to-end latency for a statistically
	// average request if dispatched to this pod now.
	//
	// Formula: PMST = E[S_effective] + W_q
	//
	// Interpretation: This is the most comprehensive single indicator of the pod's expected latency performance.
	//
	// Caveat: Due to the black-box abstraction (using historical sojourn time as E[S_effective]), this metric may
	// overestimate absolute latency. It should be treated as a robust relative indicator of congestion.
	//
	// Usage: Ideal for latency-aware scheduling decisions (e.g., choosing the pod with the lowest PMST).
	PredictedMeanSojournTime time.Duration

	// PredictedCongestionDelay (W_q) is the "virtual queue time" calculated by the Pollaczek-Khinchine formula.
	// See: https://en.wikipedia.org/wiki/Pollaczek-Khinchine_formula
	//
	// Interpretation: It represents the *additional* queuing delay (the "congestion penalty") a new request is predicted
	// to experience due to the current utilization and variance, on top of the baseline latency (E[S_effective]).
	PredictedCongestionDelay time.Duration

	// PredictedQueueLength (L_q) is the expected number of requests in the "virtual" congestion queue.
	//
	// Formula: L_q = λ * W_q (Little's Law).
	// See: https://en.wikipedia.org/wiki/Little's_law
	//
	// Usage: Primarily used to calculate QueueMomentum.
	PredictedQueueLength float64

	// Utilization (ρ) is the utilization of the pod's black-box system based on its effective throughput.
	//
	// Formula: ρ = λ * E[S_effective].
	//
	// Interpretation: A value of 1.0 indicates the arrival rate equals the pod's maximum sustainable effective service
	// rate.
	Utilization float64

	// IsOverloaded is true if Utilization >= 1.0, indicating an unstable state where latency will grow unbounded.
	IsOverloaded bool

	// --- Observability Enhancements ---

	// CoefficientOfVariation (CV) quantifies the burstiness or variability of the workload's Effective Service Time.
	// See: https://en.wikipedia.org/wiki/Coefficient_of_variation
	//
	// Formula: CV = StandardDeviation(S_effective) / E[S_effective].
	//
	// Interpretation:
	//   - CV ≈ 0: Deterministic service times (e.g., fixed computation).
	//   - CV ≈ 1: Memoryless (Exponential) distribution (M/M/1).
	//   - CV > 1: High variance (typical for LLMs). High CV drastically increases queue lengths even at moderate
	//     utilization, validating the need for the M/G/1 model.
	CoefficientOfVariation float64

	// QueueMomentum is a leading indicator of the system's trajectory.
	//
	// Formula: QueueMomentum = PredictedQueueLength (L_q) - MeasuredQueueSize.
	//
	// Interpretation: It measures the discrepancy between the theoretical prediction and the physical reality.
	//   - Momentum > 0: The model predicts the queue should be longer than it currently is, suggesting the system is
	//     rapidly trending towards congestion.
	//   - Momentum < 0: The physical queue is longer than predicted, suggesting the system is recovering (λ has recently
	//     dropped, but the physical queue hasn't drained yet).
	//   - Magnitude: The magnitude indicates the speed of the trajectory change.
	QueueMomentum float64
}

// FullnessReport is the comprehensive, unified output of the detector.
type FullnessReport struct {
	// SubsetUtilization (ρ_subset) is the aggregate, capacity-weighted utilization of the entire candidate pod subset.
	//
	// Formula: ρ_subset = Total Offered Load / Total Effective Service Capacity = Σλᵢ / Σμᵢ.
	//
	// Interpretation: This is a normalized, dimensionless value [0.0, ∞). A value of 1.0 indicates that the rate of
	// incoming work equals the subset's maximum sustainable effective processing capacity.
	//
	// Usage: This is the primary control signal for the P-loop and an ideal metric for autoscaling and load balancing.
	SubsetUtilization float64

	// PerPodDetails provides the detailed M/G/1 breakdown for each pod in the subset.
	// Usage: Used for advanced scheduling decisions and detailed observability.
	PerPodDetails map[string]PodFullnessDetails // Key: pod namespaced name

	// ControllerInternals exposes the state of the Bang-Bang controller.
	ControllerInternals BangBangControllerInternals

	// isReliable is true if and only if every pod in the report is stable and fresh.
	isReliable bool
}

// --- Detector ---

// Detector determines system saturation using a Bang-Bang Controller and Queuing Theory.
// It is designed as a global singleton and is safe for concurrent access.
type Detector struct {
	config Config

	// --- Controller State ---

	// isSaturated tracks the current state of the Bang-Bang controller (engaged or disengaged).
	// Used to implement hysteresis. Accessed atomically.
	isSaturated atomic.Bool

	// --- Stabilization State (Stateful Probing) ---

	// probeMu protects lastProbeTime.
	probeMu sync.Mutex
	// lastProbeTime tracks the timestamp of the last forced probe. Used to enforce the ProbeInterval.
	lastProbeTime time.Time

	// --- Caching ---

	// cache stores the comprehensive state (inputs and derived outputs) for each pod.
	cache    map[string]cachedPodState
	cacheTTL time.Duration
	// mu protects the cache map. RWMutex optimizes for the common read path.
	mu sync.RWMutex
}

// rawInputs captures the metrics fetched directly from the source.
type rawInputs struct {
	arrivalRate                  float64
	meanEffectiveServiceTime     time.Duration // E[S_effective] (Historical Mean Sojourn Time)
	varianceEffectiveServiceTime float64       // Var(S_effective) (in seconds^2)
	measuredQueueSize            int           // Used for calculating QueueMomentum
	sojournTimeSamples           int64
	lastSojournUpdate            time.Time
}

// cachedPodState holds the comprehensive, temporally consistent state of a pod, including inputs and derived outputs.
// This structure enables eager calculation during cache refresh, minimizing work on the hot path.
type cachedPodState struct {
	details           PodFullnessDetails // the derived M/G/1 metrics
	effectiveCapacity float64            // (μ) is the calculated service rate (1 / E[S_eff])
	arrivalRate       float64            //  (λ) is the measured arrival rate
	isReliable        bool               // reliability status of the metrics
	timestamp         time.Time          // the time when this state was calculated
}

// NewDetector creates a new SaturationDetector.
func NewDetector(config Config, logger logr.Logger) *Detector {
	logger.V(logutil.DEFAULT).WithName("SaturationDetector").Info(
		"Creating new SaturationDetector",
		"targetUtilization (Upper)", config.TargetUtilization,
		"resumeUtilization (Lower)", config.ResumeUtilization,
		"cachingTTL", config.CachingTTL.String(),
		"warmUpSampleCount", config.WarmUpSampleCount,
		"ewmaStalenessThreshold", config.EWMAStalenessThreshold.String(),
		"probeInterval", config.ProbeInterval.String(),
	)

	return &Detector{
		config:   config,
		cache:    make(map[string]cachedPodState),
		cacheTTL: config.CachingTTL,
		// isSaturated initializes to false (allow traffic).
	}
}

// GetFullnessReport is the primary, unified entry point to the detector.
// It aggregates the pre-computed pod states and updates the controller state.
//
// # The Utilization Calculation
//
// The key metric, SubsetUtilization, is derived from the Utilization Law extended to a Multi-Server Queuing System with
// heterogeneous servers:
//
//	ρ_subset = Total Offered Load / Total Effective Service Capacity = Σλᵢ / Σμᵢ
//
// Where μᵢ (Effective Service Rate) = 1 / E[S_effectiveᵢ].
//
// This approach has several strengths:
//   - Normalized: It produces a dimensionless value [0.0, ∞) that is comparable across different workloads and
//     hardware. A value of 1.0 indicates the offered load equals the subset's maximum sustainable capacity.
//   - Handles Heterogeneity: A more powerful pod will have a higher service rate (μ) and thus correctly contributes
//     more to the total service capacity in the denominator.
//   - Latency-Aware: Because capacity (μ) is based on latency (E[S_effective]), the signal directly reflects the
//     system's responsiveness and provides a robust signal for the control loop.
func (d *Detector) GetFullnessReport(ctx context.Context, candidatePods []backendmetrics.PodMetrics) FullnessReport {
	// 1. Get temporally consistent, comprehensive states (cached and eagerly calculated).
	states := d.getPodStates(candidatePods)

	// 2. Aggregate results from the eagerly computed states.
	var totalArrivalRate float64
	var totalServiceCapacity float64
	perPodDetails := make(map[string]PodFullnessDetails, len(states))
	isReliable := true

	for i, state := range states {
		// If the snapshot is empty (timestamp is zero), it means the pod was invalid.
		if state.timestamp.IsZero() {
			continue
		}

		podID := candidatePods[i].GetPod().NamespacedName.String()
		perPodDetails[podID] = state.details
		totalArrivalRate += state.arrivalRate
		totalServiceCapacity += state.effectiveCapacity
		isReliable = isReliable && state.isReliable
	}

	// 3. Calculate Utilization Factor: ρ_subset = Σλᵢ / Σμᵢ
	var subsetUtilization float64
	if totalServiceCapacity > 1e-9 { // Avoid division by zero
		subsetUtilization = totalArrivalRate / totalServiceCapacity
	} else if totalArrivalRate > 1e-9 {
		// If load is arriving (λ > 0) but capacity is effectively zero (μ ≈ 0), utilization is high.
		subsetUtilization = 1.5 // Sentinel value > 1.0.
	}
	// If both are zero, utilization is 0.0.

	// 4. Evaluate Bang-Bang Controller State (with Hysteresis).
	isSaturated := d.evaluateHysteresis(subsetUtilization)

	controllerInternals := BangBangControllerInternals{
		TargetUtilization:  d.config.TargetUtilization,
		ResumeUtilization:  d.config.ResumeUtilization,
		CurrentUtilization: subsetUtilization,
		IsSaturated:        isSaturated,
	}

	return FullnessReport{
		SubsetUtilization:   subsetUtilization,
		PerPodDetails:       perPodDetails,
		ControllerInternals: controllerInternals,
		isReliable:          isReliable,
	}
}

// IsSaturated provides the deterministic signal of pool subset saturation.
//
// Control Logic:
// 1. Analyze State: Get report (which updates cache and controller state).
// 2. Stateful Probing: If unreliable, check if a periodic probe is due (deterministic override).
// 3. Bang-Bang Control: Otherwise, return the state determined by the hysteresis logic.
func (d *Detector) IsSaturated(ctx context.Context, candidatePods []backendmetrics.PodMetrics) bool {
	logger := log.FromContext(ctx).V(logutil.TRACE)
	report := d.GetFullnessReport(ctx, candidatePods)
	if !report.isReliable {
		if d.shouldForceProbe() {
			logger.Info("Metrics unreliable and probe interval elapsed; forcing periodic probe (dispatch).")
			return false // Deterministic override: Dispatch the request.
		}
		// Metrics unreliable, but probe not due yet. Block traffic to prevent overload on potentially cold backends.
		logger.Info("Metrics unreliable, probe interval not yet elapsed; blocking dispatch.")
		return true
	}

	isSaturated := report.ControllerInternals.IsSaturated
	if logger.Enabled() {
		logger.Info("Bang-Bang Evaluation",
			"isSaturated", isSaturated,
			"subsetUtilization", report.SubsetUtilization,
			"upperThreshold", report.ControllerInternals.TargetUtilization,
			"lowerThreshold", report.ControllerInternals.ResumeUtilization,
		)
	}
	return isSaturated
}

// evaluateHysteresis implements the core logic of the Bang-Bang controller with hysteresis.
func (d *Detector) evaluateHysteresis(currentUtilization float64) bool {
	if d.isSaturated.Load() {
		// System is currently blocked. Only resume if utilization drops below the Low Watermark.
		if currentUtilization <= d.config.ResumeUtilization {
			d.isSaturated.Store(false)
			return false
		}
		return true
	} else {
		// System is currently allowing traffic. Block if utilization exceeds the High Watermark.
		if currentUtilization >= d.config.TargetUtilization {
			d.isSaturated.Store(true)
			return true
		}
		return false
	}
}

// shouldForceProbe determines if a probe is due based on the ProbeInterval.
// This assumes the caller has already determined that metrics are unreliable.
func (d *Detector) shouldForceProbe() bool {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	now := time.Now()
	if now.Sub(d.lastProbeTime) > d.config.ProbeInterval {
		d.lastProbeTime = now
		return true
	}
	return false
}

// getPodStates is the core of the internal caching logic. It implements eager calculation.
func (d *Detector) getPodStates(pods []backendmetrics.PodMetrics) []cachedPodState {
	states := make([]cachedPodState, len(pods))
	var podsToRefreshIndices []int
	now := time.Now()

	// Phase 1: Read Lock (Optimized for Cache Hits)
	d.mu.RLock()
	for i, podMetric := range pods {
		if podMetric == nil || podMetric.GetPod() == nil {
			continue
		}
		podID := podMetric.GetPod().NamespacedName.String()
		cached, found := d.cache[podID]
		if found && now.Sub(cached.timestamp) < d.cacheTTL {
			states[i] = cached // Cache HIT and VALID
		} else {
			podsToRefreshIndices = append(podsToRefreshIndices, i) // Cache MISS or STALE
		}
	}
	d.mu.RUnlock()

	// Phase 2: Refresh and Eager Calculation (No Lock Held)
	if len(podsToRefreshIndices) > 0 {
		refreshed := make(map[string]cachedPodState)
		for _, index := range podsToRefreshIndices {
			podMetric := pods[index]
			podID := podMetric.GetPod().NamespacedName.String()

			// Fetch raw inputs from the source (high contention path).
			inputs, ok := d.fetchRawInputs(podMetric)
			if !ok {
				continue
			}

			// Calculate the comprehensive state (M/G/1, Reliability, Capacity).
			state := d.calculatePodState(inputs, now)
			refreshed[podID] = state
			states[index] = state
		}

		// Phase 3: Write Lock (Update Cache)
		if len(refreshed) > 0 {
			d.mu.Lock()
			// In high-concurrency scenarios, another thread might have updated the cache during Phase 2.
			// Stomping with slightly newer data is acceptable.
			maps.Copy(d.cache, refreshed)
			d.mu.Unlock()
		}
	}

	return states
}

// fetchRawInputs retrieves the metrics from the high-contention source.
func (d *Detector) fetchRawInputs(podMetric backendmetrics.PodMetrics) (rawInputs, bool) {
	ewmaMetrics := podMetric.GetEWMAMetrics()
	if ewmaMetrics == nil {
		return rawInputs{}, false
	}

	physicalMetrics := podMetric.GetMetrics()
	measuredQueueSize := 0
	if physicalMetrics != nil {
		measuredQueueSize = physicalMetrics.WaitingQueueSize
	}

	// Requires the accessors added to EWMAMetrics.
	return rawInputs{
		arrivalRate:                  ewmaMetrics.GetArrivalRateEWMA(),
		meanEffectiveServiceTime:     ewmaMetrics.GetMeanSojournTimeEWMA(),
		varianceEffectiveServiceTime: ewmaMetrics.GetVarianceSojournTimeEWMA(),
		measuredQueueSize:            measuredQueueSize,
		sojournTimeSamples:           ewmaMetrics.GetSojournTimeSamples(),
		lastSojournUpdate:            ewmaMetrics.GetLastSojournUpdate(),
	}, true
}

// calculatePodState performs the eager calculation of derived metrics and reliability assessment.
func (d *Detector) calculatePodState(inputs rawInputs, now time.Time) cachedPodState {
	// Check stability (samples).
	unstable := inputs.sojournTimeSamples < d.config.WarmUpSampleCount

	// Check staleness (time since last update).
	// We can have sufficient samples but still be stale (e.g., post-idle burst).
	stale := !inputs.lastSojournUpdate.IsZero() && time.Since(inputs.lastSojournUpdate) > d.config.EWMAStalenessThreshold

	// Calculate M/G/1 Details and Capacity (μ).
	details := d.calculatePodDetails(inputs)
	effectiveCapacity := 0.0
	if inputs.meanEffectiveServiceTime > 0 {
		effectiveCapacity = 1.0 / inputs.meanEffectiveServiceTime.Seconds()
	}

	return cachedPodState{
		details:           details,
		isReliable:        !unstable && !stale,
		effectiveCapacity: effectiveCapacity,
		arrivalRate:       inputs.arrivalRate,
		timestamp:         now,
	}
}

// calculatePodDetails interprets a pod's metrics using the Pollaczek-Khinchine formula for an M/G/1 queue to predict
// its end-to-end latency, employing the Effective Service Time abstraction.
func (d *Detector) calculatePodDetails(inputs rawInputs) PodFullnessDetails {
	meanServiceSec := inputs.meanEffectiveServiceTime.Seconds()
	if meanServiceSec <= 1e-9 {
		return PodFullnessDetails{} // Insufficient data (cold start)
	}

	// --- 1. Calculate Utilization (ρ) and CV ---
	// ρ = λ * E[S_effective]
	utilization := inputs.arrivalRate * meanServiceSec
	cv := calculateCV(meanServiceSec, inputs.varianceEffectiveServiceTime)

	if utilization >= 1.0 {
		return PodFullnessDetails{
			Utilization:              utilization,
			PredictedMeanSojournTime: overloadedLatency,
			IsOverloaded:             true,
			CoefficientOfVariation:   cv,
			// QueueMomentum is less meaningful when utilization >= 1 as L_q is theoretically infinite.
		}
	}

	// --- 2. Calculate Predicted Congestion Delay (W_q) ---
	// Pollaczek-Khinchine formula: W_q = (λ * E[S^2]) / (2 * (1 - ρ))
	// E[S^2] (Second Moment) = Var(S) + E[S]^2
	secondMoment := inputs.varianceEffectiveServiceTime + (meanServiceSec * meanServiceSec)

	// Safety check against division by zero if utilization is extremely close to 1.0 due to float precision.
	if (1 - utilization) < 1e-9 {
		return PodFullnessDetails{
			Utilization:              utilization,
			PredictedMeanSojournTime: overloadedLatency,
			IsOverloaded:             true,
			CoefficientOfVariation:   cv,
		}
	}

	congestionDelaySec := (inputs.arrivalRate * secondMoment) / (2 * (1 - utilization))

	// Ensure delay is non-negative (can slightly drift negative due to float precision or EWMA lag).
	if congestionDelaySec < 0 {
		congestionDelaySec = 0
	}

	// --- 3. Calculate Predicted Queue Length (L_q), PMST, and Queue Momentum ---
	predictedQueueLength := inputs.arrivalRate * congestionDelaySec           // L_q = λ * W_q (Little's Law)
	pmstSec := meanServiceSec + congestionDelaySec                            // PMST = E[S] + W_q
	queueMomentum := predictedQueueLength - float64(inputs.measuredQueueSize) // Momentum = L_q - MeasuredQueueSize.

	return PodFullnessDetails{
		Utilization:              utilization,
		PredictedCongestionDelay: time.Duration(congestionDelaySec * float64(time.Second)),
		PredictedMeanSojournTime: time.Duration(pmstSec * float64(time.Second)),
		PredictedQueueLength:     predictedQueueLength,
		IsOverloaded:             false,
		CoefficientOfVariation:   cv,
		QueueMomentum:            queueMomentum,
	}
}

// calculateCV computes the Coefficient of Variation (CV).
func calculateCV(mean, variance float64) float64 {
	// Ensure non-negative variance and non-zero mean due to potential floating point inaccuracies in EWMA updates.
	if mean <= 1e-9 || variance <= 0 {
		return 0
	}
	// CV = StandardDeviation / Mean = sqrt(Variance) / Mean
	return math.Sqrt(variance) / mean
}
