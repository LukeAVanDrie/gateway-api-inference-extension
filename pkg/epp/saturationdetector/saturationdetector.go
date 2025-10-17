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
// # Core Design: A Proportional (P) Controller over Queuing Theory Models
//
// This package implements a Proportional (P) Controller (https://en.wikipedia.org/wiki/Proportional_control) to create
// a self-regulating system that smoothly manages backend utilization, moving beyond simple, static thresholds.
//
// The controller's intelligence is derived from queuing theory models (https://en.wikipedia.org/wiki/Queueing_theory),
// treating each backend pod as a black-box "server."
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
// increases, utilization (ρ = λ * E[S_eff]) rises, and the P-controller reacts.
//
// Consequence: The predictive latency metrics (PMST, W_q) derived from this abstraction have a conservative bias
// (they may overestimate absolute latency) but serve as robust relative indicators of congestion.
//
// # Key Models
//
//  1. Utilization Law (The Control Signal):
//     The P-controller's primary input is an aggregate "Subset Utilization" (ρ_subset). This is based on the Utilization
//     Law extended to a multi-server system: the ratio of offered load to available effective capacity
//     (ρ_subset = Σλᵢ / Σμᵢ), where μᵢ = 1 / E[S_effᵢ].
//
//  2. M/G/1 Queue Model (Predictive Analysis):
//     For per-pod analysis, the detector uses the Pollaczek-Khinchine formula based on the M/G/1 queue model
//     (https://en.wikipedia.org/wiki/M/G/1_queue).
//     This model is chosen because the 'G' (General distribution) correctly accounts for the high-variance service times
//     inherent in LLM workloads.
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
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

// overloadedLatency is a sentinel value used when a pod is in an unstable state (ρ >= 1), where latency theoretically
// trends towards infinity.
var overloadedLatency = time.Hour * 24

// PControllerInternals exposes the internal state of the Proportional (P) Controller for observability and tuning.
// It details the calculation used to determine the throttling action.
// See: https://en.wikipedia.org/wiki/Proportional_control
type PControllerInternals struct {
	// TargetUtilization is the configured goal state (Setpoint) for the system.
	// Usage: This is the primary knob for balancing throughput vs. latency.
	TargetUtilization float64

	// CurrentUtilization is the measured aggregate utilization (Process Variable) of the subset (ρ_subset).
	CurrentUtilization float64

	// ErrorSignal (e) is the difference between the target and current utilization.
	// Formula: e = TargetUtilization - CurrentUtilization.
	// Interpretation: A positive value indicates available capacity relative to the target. A negative value indicates
	// the system is over its target utilization.
	ErrorSignal float64

	// DispatchProbability is the calculated output of the P-loop (Control Output).
	// Formula: Kp * ErrorSignal, clamped to [0.0, 1.0].
	// Interpretation: The probability that a new request should be admitted. As utilization increases, this value
	// smoothly decreases, providing proactive throttling.
	DispatchProbability float64
}

// PodFullnessDetails encapsulates the calculated metrics for a single pod using the M/G/1 model and the Effective
// Service Time abstraction. It provides a predictive view of a pod's congestion state.
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

	// ControllerInternals exposes the state of the P-controller based on the SubsetUtilization.
	ControllerInternals PControllerInternals
}

// --- Detector ---

// Detector determines system saturation and provides detailed pod metrics using a P-Controller and Queuing Theory.
// It is designed as a global singleton and is safe for concurrent access.
type Detector struct {
	config Config

	// rand is the source of randomness for the probabilistic P-controller output.
	// Access must be synchronized using randMu, as math/rand.Rand with a local source is not concurrency-safe.
	rand   *rand.Rand
	randMu sync.Mutex

	// cache provides an internal, TTL-based cache of pod metrics.
	// This optimization decouples the high-frequency decision loop from the high-contention metrics write path.
	cache    map[string]cachedPodMetrics
	cacheTTL time.Duration
	// mu protects the cache map. RWMutex optimizes for the common read path.
	mu sync.RWMutex
}

// cachedPodMetrics holds a snapshot of the EWMA metrics and physical measurements, forming the basis of the detector's
// internal cache. By snapshotting both, we ensure temporal consistency for calculations like QueueMomentum.
type cachedPodMetrics struct {
	// --- EWMA Inputs (M/G/1 Model Inputs) ---
	arrivalRate                  float64
	meanEffectiveServiceTime     time.Duration // E[S_effective] (Historical Mean Sojourn Time)
	varianceEffectiveServiceTime float64       // Var(S_effective) (in seconds^2)

	// --- Physical Measurements ---
	measuredQueueSize int // Used for calculating QueueMomentum

	timestamp time.Time
}

// NewDetector creates a new SaturationDetector.
func NewDetector(config Config, logger logr.Logger) *Detector {
	logger.V(logutil.DEFAULT).WithName("SaturationDetector").Info(
		"Creating new P-Controller SaturationDetector with internal TTL cache",
		"targetUtilization", config.TargetUtilization,
		"proportionalGain (Kp)", config.ProportionalGain,
		"cachingTTL", config.CachingTTL.String())

	// Initialize the random source. We use a local source rather than the global one.
	//nolint:gosec // G404: Use of weak random number generator (math/rand) is acceptable for probabilistic throttling.
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	return &Detector{
		config:   config,
		rand:     r,
		cache:    make(map[string]cachedPodMetrics),
		cacheTTL: config.CachingTTL,
	}
}

// GetFullnessReport is the primary, unified entry point to the detector.
// It performs a single-pass calculation to produce a comprehensive report.
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
	// 1. Get temporally consistent snapshots of all inputs (cached).
	snapshots := d.getMetricSnapshots(candidatePods)

	// 2. Calculate aggregate metrics and per-pod details.
	var totalArrivalRate float64
	var totalServiceCapacity float64
	perPodDetails := make(map[string]PodFullnessDetails, len(snapshots))

	for i, snapshot := range snapshots {
		// We assume candidatePods[i] corresponds to snapshots[i].
		// If the snapshot is empty (timestamp is zero), it means the pod was invalid or metrics were missing.
		if snapshot.timestamp.IsZero() {
			continue
		}

		podID := candidatePods[i].GetPod().NamespacedName.String()

		// Per-Pod Predictive Calculation (M/G/1)
		details := d.calculatePodDetails(snapshot)
		perPodDetails[podID] = details

		// Aggregate Metric Calculation
		totalArrivalRate += snapshot.arrivalRate // Accumulate Σλ

		// Accumulate Σμ. μ = 1 / E[S]. A pod with no E[S] contributes no capacity.
		if snapshot.meanEffectiveServiceTime > 0 {
			totalServiceCapacity += 1.0 / snapshot.meanEffectiveServiceTime.Seconds()
		}
	}

	// 3. Calculate Utilization Factor: ρ_subset = Σλᵢ / Σμᵢ
	var subsetUtilization float64
	if totalServiceCapacity > 1e-9 { // Avoid division by zero
		subsetUtilization = totalArrivalRate / totalServiceCapacity
	} else if totalArrivalRate > 1e-9 {
		// If load is arriving (λ > 0) but capacity is effectively zero (μ ≈ 0, e.g., all pods are cold), utilization is
		// high. We use 1.5 as a strong signal > 1.0.
		subsetUtilization = 1.5
	}
	// If both are zero, utilization is 0.0.

	// 4. Calculate P-Controller State.
	controllerInternals := d.calculatePControllerOutput(subsetUtilization)

	return FullnessReport{
		SubsetUtilization:   subsetUtilization,
		PerPodDetails:       perPodDetails,
		ControllerInternals: controllerInternals,
	}
}

// IsSaturated applies the P-controller logic probabilistically based on the FullnessReport.
//
// It returns true if the current request should be throttled. This probabilistic approach ensures smooth throttling as
// the system approaches its TargetUtilization.
//
// # The P-Control Loop
//
// The control loop operates on these core concepts:
//
//   - Process Variable (The Current State): The `SubsetUtilization` (ρ_subset).
//   - Setpoint (The Desired State): The configured `TargetUtilization` (e.g., 0.85) that balances throughput with a
//     sufficient capacity buffer to absorb variance and prevent latency spikes.
//   - Control Output (The Action): The difference ("error") is multiplied by the Proportional Gain (Kp) to calculate a
//     "dispatch probability" [0.0, 1.0].
//
// As the system approaches its target utilization, this probability smoothly decreases, effectively throttling the rate
// of new requests proactively.
func (d *Detector) IsSaturated(ctx context.Context, candidatePods []backendmetrics.PodMetrics) bool {
	report := d.GetFullnessReport(ctx, candidatePods)
	dispatchProbability := report.ControllerInternals.DispatchProbability

	// We must lock the RNG as math/rand.Rand (with local source) is not safe for concurrent use.
	d.randMu.Lock()
	randVal := d.rand.Float64()
	d.randMu.Unlock()

	// If the random number is greater than or equal to the dispatch probability, the request is throttled.
	isSaturated := randVal >= dispatchProbability

	// Observability: Log the internal state of the P-controller at trace level for tuning.
	log.FromContext(ctx).V(logutil.TRACE).Info("P-Controller Decision",
		"isSaturated", isSaturated,
		"subsetUtilization", report.SubsetUtilization,
		"targetUtilization", report.ControllerInternals.TargetUtilization,
		"errorSignal", report.ControllerInternals.ErrorSignal,
		"Kp", d.config.ProportionalGain,
		"dispatchProbability", dispatchProbability,
	)

	return isSaturated
}

// calculatePControllerOutput implements the core logic of the P-controller.
func (d *Detector) calculatePControllerOutput(currentUtilization float64) PControllerInternals {
	// Error Signal (e) = Setpoint - Process Variable
	errSignal := d.config.TargetUtilization - currentUtilization

	// Control Output = Kp * e
	dispatchProbability := d.config.ProportionalGain * errSignal

	// Clamp the output to the valid range [0.0, 1.0].
	dispatchProbability = clamp(dispatchProbability, 0.0, 1.0)

	return PControllerInternals{
		TargetUtilization:   d.config.TargetUtilization,
		CurrentUtilization:  currentUtilization,
		ErrorSignal:         errSignal,
		DispatchProbability: dispatchProbability,
	}
}

// calculatePodDetails interprets a pod's metrics using the Pollaczek-Khinchine formula for an M/G/1 queue to predict
// its end-to-end latency, employing the Effective Service Time abstraction.
func (d *Detector) calculatePodDetails(snapshot cachedPodMetrics) PodFullnessDetails {
	meanServiceSec := snapshot.meanEffectiveServiceTime.Seconds()
	if meanServiceSec <= 1e-9 {
		return PodFullnessDetails{} // Insufficient data (cold start)
	}

	// --- 1. Calculate Utilization (ρ) and CV ---
	// ρ = λ * E[S_effective]
	utilization := snapshot.arrivalRate * meanServiceSec
	cv := calculateCV(meanServiceSec, snapshot.varianceEffectiveServiceTime)

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
	secondMoment := snapshot.varianceEffectiveServiceTime + (meanServiceSec * meanServiceSec)

	// Safety check against division by zero if utilization is extremely close to 1.0 due to float precision.
	if (1 - utilization) < 1e-9 {
		return PodFullnessDetails{
			Utilization:              utilization,
			PredictedMeanSojournTime: overloadedLatency,
			IsOverloaded:             true,
			CoefficientOfVariation:   cv,
		}
	}

	congestionDelaySec := (snapshot.arrivalRate * secondMoment) / (2 * (1 - utilization))

	// Ensure delay is non-negative (can slightly drift negative due to float precision or EWMA lag).
	if congestionDelaySec < 0 {
		congestionDelaySec = 0
	}

	// --- 3. Calculate Predicted Queue Length (L_q) and PMST ---
	predictedQueueLength := snapshot.arrivalRate * congestionDelaySec // L_q = λ * W_q (Little's Law)
	pmstSec := meanServiceSec + congestionDelaySec                    // PMST = E[S] + W_q

	// --- 4. Calculate Queue Momentum ---
	// Momentum = L_q - MeasuredQueueSize. We use the temporally consistent snapshot value.
	queueMomentum := predictedQueueLength - float64(snapshot.measuredQueueSize)

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
	// Ensure non-negative variance due to potential floating point inaccuracies in EWMA updates.
	if mean <= 1e-9 || variance <= 0 {
		return 0
	}
	// CV = StandardDeviation / Mean = sqrt(Variance) / Mean
	return math.Sqrt(variance) / mean
}

// getMetricSnapshots is the core of the internal caching logic. It safely orchestrates reads from the cache and lazy,
// batched refreshes from the underlying high-contention metrics source.
// It returns a slice of snapshots aligned (index-by-index) with the input pods slice.
// If a pod is invalid or metrics are missing, the corresponding snapshot will be empty (zero value).
func (d *Detector) getMetricSnapshots(pods []backendmetrics.PodMetrics) []cachedPodMetrics {
	snapshots := make([]cachedPodMetrics, len(pods))
	var podsToRefreshIndices []int
	now := time.Now()

	// Phase 1: Read Lock (Optimized for Cache Hits)
	// Check the cache for all pods.
	d.mu.RLock()
	for i, podMetric := range pods {
		if podMetric.GetPod() == nil {
			continue
		}
		podID := podMetric.GetPod().NamespacedName.String()
		cached, found := d.cache[podID]
		if found && now.Sub(cached.timestamp) < d.cacheTTL {
			snapshots[i] = cached // Cache HIT and VALID
		} else {
			podsToRefreshIndices = append(podsToRefreshIndices, i) // Cache MISS or STALE
		}
	}
	d.mu.RUnlock()

	// Phase 2: Refresh (No Lock Held During Metric Fetch)
	// If all pods were handled, return. Otherwise, refresh only the missing or stale entries.
	if len(podsToRefreshIndices) > 0 {
		refreshed := make(map[string]cachedPodMetrics)
		for _, index := range podsToRefreshIndices {
			podMetric := pods[index]
			podID := podMetric.GetPod().NamespacedName.String()

			// Fetch metrics from the source. This involves contention on the source's lock.
			ewmaMetrics := podMetric.GetEWMAMetrics()

			// We require EWMA metrics to model the pod.
			if ewmaMetrics == nil {
				continue
			}

			// Fetch instantaneous physical metrics (for comparison/momentum).
			// This ensures temporal consistency: the queue size is captured alongside the EWMA data.
			physicalMetrics := podMetric.GetMetrics()
			measuredQueueSize := 0
			if physicalMetrics != nil {
				measuredQueueSize = physicalMetrics.WaitingQueueSize
			}

			snapshot := cachedPodMetrics{
				// We use Sojourn Time EWMAs as the Effective Service Time inputs.
				arrivalRate:                  ewmaMetrics.GetArrivalRateEWMA(),
				meanEffectiveServiceTime:     ewmaMetrics.GetMeanSojournTimeEWMA(),
				varianceEffectiveServiceTime: ewmaMetrics.GetVarianceSojournTimeEWMA(),
				measuredQueueSize:            measuredQueueSize,
				timestamp:                    now,
			}
			refreshed[podID] = snapshot
			snapshots[index] = snapshot
		}

		// Phase 3: Write Lock (Update Cache)
		// Atomically update the cache with the newly fetched data.
		if len(refreshed) > 0 {
			d.mu.Lock()
			for podID, snapshot := range refreshed {
				d.cache[podID] = snapshot
			}
			d.mu.Unlock()
		}
	}

	return snapshots
}

// clamp restricts a value to be within a specified range [min, max].
func clamp(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
