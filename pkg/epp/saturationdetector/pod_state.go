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
	"math"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector/estimators"
)

// saturationSentinelOverload represents a Definite Overload signal (Saturation > 1.0).
//
// We cap the error signal at 2.0 to provide a deterministic "Strong Push" for the P-Controller.
//
//	error = Setpoint - PV = 0.85 - 2.0 = -1.15
//
// Multiplied by Kp=1.0, this forces the controller to reduce the dispatch rate by 1.15x Capacity per tick, rapidly
// shedding load without inducing numerical instability.
const saturationSentinelOverload = 2.0

// PodSnapshot provides a deep-copy view of the pod's internal physics model.
type PodSnapshot struct {
	// Identity
	ID types.NamespacedName

	// Trust Model
	Maturity MaturityState

	// Estimators (The Internal Model)
	EstimatedCapacity float64 // ^B_eff
	EstimatedRate     float64 // ^μ_t
	EstimatedQueue    float64 // ^Q_t
	PeakConcurrency   float64 // L_peak

	// Calculated Pressure
	SaturationPV float64 // The raw pressure signal used for the Max() aggregation

	// Decision Outputs
	IsSaturated      bool    // Is PV > Setpoint?
	ConcurrencyLimit float64 // The calculated limit (if Probing)
}

// podState maintains the Estimated Internal Physics Model of a single backend pod.
//
// It fuses noisy telemetry into a coherent 3-dimensional view of the backend:
//  1. Capacity (Supply): Compute (^B_eff) and Memory (U_kv) constraints.
//  2. Throughput (Flow): Service Rate (^μ_t).
//  3. Congestion (Demand): Queue Depth (^Q_t).
//
// This struct is NOT thread-safe.
// It effectively acts as the "Process Control Block" for the pod and must be accessed exclusively by the
// ingle-threaded Controller Reconciliation Loop.
type podState struct {
	namespacedName types.NamespacedName
	maturity       MaturityState

	// --- Estimators (The Internal Model) ---

	// peakInflightConcurrency (L_peak) estimates the Peak Work In Progress.
	//
	// Physics (Little's Law):
	// In the absence of a queue, L = λW. Concurrency scales linearly with throughput until saturation.
	// We use a Windowed Max Filter to "Hill Climb" and discover the physical concurrency limit before significant queuing
	// occurs.
	peakInflightConcurrency *estimators.WindowedExtremumFilter[float64, uint64]

	// effectiveBatchEWMA (^B_eff) estimates the Effective Batch Capacity.
	//
	// Physics (Compute Bound):
	// This represents the "Soft Limit" of the GPU's compute capability.
	// It is the estimated number of requests the pod can process in parallel within a single forward pass without latency
	// degradation.
	// Ideally, this converges to the max batch size where T_step remains constant.
	effectiveBatchEWMA *estimators.EWMA[float64]

	// queueDepthEWMA (^Q_t) estimates the Smoothed Queue Depth.
	//
	// Physics (Error Signal):
	// This is the primary input for the Feedback Loop.
	// Smoothing is critical here to prevent the controller from reacting to high-frequency Poisson noise in the arrival
	// process.
	queueDepthEWMA *estimators.EWMA[float64]

	// serviceRateEWMA (^μ_t) estimates the Service Rate.
	//
	// Physics (Flow Control):
	// This provides the Feed-Forward Control term (u_ff). It tracks the completion rate (Requests/Sec).
	// It decays over time so that "Ghost Capacity" (historical high throughput from a previous burst) does not
	// erroneously justify high dispatch rates during a cold start.
	serviceRateEWMA *estimators.RateEWMA

	// kvCacheMaxFilter (U_kv) estimates KV Cache Utilization.
	//
	// Physics (Memory Bound):
	// This represents the "Hard Limit" of the GPU's HBM capacity. Unlike Compute, Memory is binary (Allocated/OOM).
	// We use a Max Filter to hold the "Peak Danger" signal, ensuring we never average out a near-OOM event.
	kvCacheMaxFilter *estimators.WindowedExtremumFilter[float64, uint64]

	// --- Internal State ---

	// lastBatchSampleTime tracks the cooldown for B_eff sampling to ensure statistical independence.
	lastBatchSampleTime time.Time

	// enteredImmatureStateAt tracks the timestamp of the last reset to Immature.
	// Used to trigger the "Dormant" timeout for idle pods.
	// It is set to time.Zero for all other maturity states.
	enteredImmatureStateAt time.Time

	// --- Configuration Cache ---
	minSamplesForEffectiveBatchMaturity     uint64
	minEffectiveCountForServiceRateMaturity float64
}

// newPodState initializes a new Internal Model of a pod.
func newPodState(
	namespacedName types.NamespacedName,
	config *ControllerConfig,
	tickInterval time.Duration,
	initialTime time.Time,
	initialRound uint64,
) *podState {
	// Convert time-windows to discrete "Rounds".
	lPeakRounds := max(1, uint64(config.PeakInflightConcurrencyWindow.Seconds()/tickInterval.Seconds()))
	kvCacheRounds := max(1, uint64(config.KVCacheWindow.Seconds()/tickInterval.Seconds()))

	return &podState{
		namespacedName:         namespacedName,
		maturity:               Immature,
		enteredImmatureStateAt: initialTime,
		peakInflightConcurrency: estimators.NewWindowedMaxFilter(
			lPeakRounds,
			config.PeakInflightConcurrencySamples,
			expirationCheck,
			0.0,
			initialRound,
		),
		effectiveBatchEWMA: estimators.NewEWMA[float64](config.EffectiveBatchAlpha),
		queueDepthEWMA:     estimators.NewEWMA[float64](config.QueueDepthAlpha),
		serviceRateEWMA:    estimators.NewRateEWMA(config.ServiceRateWindow, initialTime),
		kvCacheMaxFilter: estimators.NewWindowedMaxFilter(
			kvCacheRounds,
			config.KVCacheSamples,
			expirationCheck,
			0.0,
			initialRound,
		),
		lastBatchSampleTime:                     time.Time{},
		minSamplesForEffectiveBatchMaturity:     config.MinSamplesForEffectiveBatchMaturity,
		minEffectiveCountForServiceRateMaturity: config.MinEffectiveCountForServiceRateMaturity,
	}
}

// IsEffectiveBatchMature returns true if the Compute Capacity estimate (^B_eff) is statistically significant.
func (p *podState) IsEffectiveBatchMature() bool {
	return p.effectiveBatchEWMA.SampleCount() >= p.minSamplesForEffectiveBatchMaturity
}

// IsServiceRateMature returns true if the Throughput estimate (^μ_t) is significant and fresh.
func (p *podState) IsServiceRateMature(now time.Time) bool {
	return p.serviceRateEWMA.Count(now) >= p.minEffectiveCountForServiceRateMaturity
}

// IsSaturated acts as the Boolean Guard for the SaturationFilter.
// It returns true if the Process Variable (PV) exceeds the target Setpoint (SP).
func (p *podState) IsSaturated(currentInflight uint64, sTarget float64) bool {
	return p.Saturation(currentInflight) >= sTarget
}

// Saturation calculates the Process Variable (PV) for the Feedback Control Loop.
//
// The PV is a dimensionless "Pressure Index" [0.0, Inf) derived from the critical constraint.
//
// Formula:
//
//	PV = max(ComputePressure, MemoryPressure)
//
// Where:
//
//	ComputePressure = ^Q_t / ^B_eff_t (Ratio of Demand to Supply)
//	MemoryPressure  = U_kv_t          (KV Cache Utilization)
//
// By fusing these signals via the Max operator, the controller automatically adapts its behavior to protect against
// whichever resource (Compute or Memory) is currently the bottleneck.
func (p *podState) Saturation(currentInflight uint64) float64 {
	// 1. Check Memory Pressure (The Hard Limit)
	memoryPressure, _ := p.kvCacheMaxFilter.Get()

	// 2. Calculate Compute Pressure (The Soft Limit)
	computePressure := 0.0

	if p.IsEffectiveBatchMature() {
		// Regime: Steady State
		// We trust ^B_eff as the true physical capacity.
		bEff := p.effectiveBatchEWMA.Get()

		if bEff < 1.0 {
			// Edge Case: Capacity Collapse (e.g., Network partition or Process stall).
			// If we have queue but no capacity, signal maximum overload.
			if p.queueDepthEWMA.Get() > 0 {
				computePressure = saturationSentinelOverload
			} else {
				computePressure = 0.0
			}
		} else {
			// Standard Operation: Load = Queue / Capacity
			computePressure = p.queueDepthEWMA.Get() / bEff
		}
	} else {
		// Regime: Discovery (Hill Climbing)
		// We lack a trusted ^B_eff. We fallback to a heuristic based on L_peak.
		// We conservatively assume capacity is "Best_Seen + 1".
		lPeak, _ := p.peakInflightConcurrency.Get()

		// Heuristic Fusion:
		// We take the MAX of Instantaneous Inflight and Smoothed Queue.
		// During probing, we want to react instantly to a concurrency spike (Safety), but we also want to respect the
		// smoothed trend if the instant count drops momentarily.
		demand := math.Max(float64(currentInflight), p.queueDepthEWMA.Get())
		computePressure = demand / (lPeak + 1.0)
	}

	return math.Max(computePressure, memoryPressure)
}

// EffectiveServiceRate calculates the Feed-Forward Control Term (u_ff).
//
// In a 2-DOF controller, u(t) = u_ff(t) + u_fb(t).
// This term sets the baseline dispatch rate based on estimated capacity, allowing the system to match demand instantly
// without waiting for an error (queue) to accumulate.
func (p *podState) EffectiveServiceRate(now time.Time, poolAvgEndToEndRequestLatency float64) float64 {
	// Priority 1: Measured Reality (Mature)
	// We have high confidence in the measured throughput.
	if p.IsServiceRateMature(now) {
		return p.serviceRateEWMA.Rate(now)
	}

	// Priority 2: Optimistic Seeding (Maturing)
	// We have a Capacity estimate (^B_eff) but no reliable Rate history.
	// Apply Little's Law: λ = L / W
	//    L = ^B_eff (Estimated concurrency capacity)
	//    W = Pool_Avg_Latency (Characteristic time to service a request)
	if p.IsEffectiveBatchMature() && poolAvgEndToEndRequestLatency > 0 {
		bEff := p.effectiveBatchEWMA.Get()
		return bEff / poolAvgEndToEndRequestLatency
	}

	// Priority 3: Blind (Immature/Dormant)
	// We know nothing. Return a conservative rate 1.0.
	return 1.0
}

// UpdateMaturity advances the Trust Model state machine.
func (p *podState) UpdateMaturity(now time.Time, dormantTimeout time.Duration, isActive bool) {
	switch p.maturity {
	case Immature:
		// Transition: Discovery -> Maturing
		// We have found a stable Batch Capacity baseline.
		if p.IsEffectiveBatchMature() {
			p.maturity = Maturing
			p.enteredImmatureStateAt = time.Time{}
			return
		}
		// Transition: Idle -> Dormant
		// We have been idle too long. The partial model is likely stale.
		if !p.enteredImmatureStateAt.IsZero() && now.Sub(p.enteredImmatureStateAt) > dormantTimeout {
			p.maturity = Dormant
			p.enteredImmatureStateAt = time.Time{}
			return
		}
	case Maturing:
		// Transition: Seeding -> Mature (Steady State)
		// We have accumulated enough recent throughput data to trust ^μ_t.
		if p.IsServiceRateMature(now) {
			p.maturity = Mature
			return
		}
	case Mature:
		// Demotion: Mature -> Maturing
		// The rate signal has decayed (idle). We lose confidence in ^μ_t and fallback to Seeding.
		if !p.IsServiceRateMature(now) {
			p.maturity = Maturing
			return
		}
	case Dormant:
		if isActive {
			p.maturity = Immature
			p.enteredImmatureStateAt = now
			return
		}
	}
}

// Snapshot creates a frozen view of the pod state.
// config is required to calculate the current concurrency limit logic.
func (p *podState) Snapshot(now time.Time, config *ControllerConfig, currentInflight uint64) PodSnapshot {
	lPeak, _ := p.peakInflightConcurrency.Get()
	saturation := p.Saturation(currentInflight)
	return PodSnapshot{
		ID:                p.namespacedName,
		Maturity:          p.maturity,
		EstimatedCapacity: p.effectiveBatchEWMA.Get(),
		EstimatedRate:     p.serviceRateEWMA.Rate(now),
		EstimatedQueue:    p.queueDepthEWMA.Get(),
		PeakConcurrency:   lPeak,
		SaturationPV:      saturation,
		IsSaturated:       saturation >= config.SaturationSetpoint,
		ConcurrencyLimit:  getPodConcurrencyLimit(p, config),
	}
}

// expirationCheck verifies if a sample in the Windowed Filter has aged out.
func expirationCheck(window, current, past uint64) bool {
	// Sample is expired if the current round is strictly greater than the sample's round AND the delta exceeds the window
	// size.
	return current > past && (current-past) >= window
}
