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
	"time"
)

// =====================================================================================================================
// System Physics & Control Loop Defaults (The Brain)
// =====================================================================================================================

// --- Controller-Level Defaults ---
const (
	// DefaultSaturationSetpoint represents the target ratio of Load to Capacity (L/C) for the feedback loop.
	// Derived from Kingman's Formula for G/G/1 queues, which approximates wait time as:
	//    E[W] ≈ ( ρ / (1-ρ) ) * ( (Ca^2 + Cs^2)/2 ) * τ
	//
	// As utilization (ρ) approaches 1.0, wait time grows asymptotically.
	//
	// Rationale:
	// 0.85 is the industry-standard "Knee of the Curve."
	//   - At ρ=0.85, the latency multiplier is ~6x.
	//   - At ρ=0.95, the latency multiplier jumps to ~19x.
	// Operating at 0.85 maximizes ROI (Throughput) while retaining a safety buffer to absorb variance (Ca, Cs) without
	// violating tail latency SLOs.
	DefaultSaturationSetpoint float64 = 0.85

	// DefaultSaturationHeadroom defines the "Deadband" between Regulation and Rejection.
	// This creates a hysteresis band to prevent "Chattering" (rapid oscillation between regulating and rejecting).
	// The Hard Rejection Threshold occurs at: Setpoint + Headroom.
	//
	// Rationale:
	// A 0.15 margin places the Hard Rejection threshold at exactly 1.0 (Physical Saturation).
	// This ensures the P-Controller has the full operating range (0.85 -> 1.0) to regulate dispatch rates via
	// backpressure before the failsafe filter kicks in to physically block traffic.
	DefaultSaturationHeadroom float64 = 0.15

	// DefaultProportionalGain (Kp) is the gain for the Feedback Control Loop.
	// It determines the stiffness of the controller's response to error, scaled by the system's capacity:
	//    u_fb(t) = Capacity * Kp * error(t)
	//
	// Rationale:
	// 1.0 provides a Critical Damping response.
	// Because the Error is normalized (e.g., 0.1 represents a 10% deviation), a gain of 1.0 produces a corrective rate
	// adjustment exactly proportional to the magnitude of the problem (e.g., reduce rate by 10% of total capacity).
	DefaultProportionalGain float64 = 1.0

	// DefaultMinDispatchRate is the "Pilot Light" for the controller.
	// This solves the "Dead Zone" problem where a system with 0 throughput and 0 error never generates a control signal
	// to restart.
	//
	// Rationale:
	// 1.0 QPS guarantees liveness, ensuring the system continuously probes for capacity recovery even when fully idle.
	DefaultMinDispatchRate float64 = 1.0

	// DefaultTickInterval is the Sampling Rate (Ts) of the discrete-time control loop.
	//
	// Rationale:
	// 50ms (20Hz) provides sufficient Control Authority to react to queue variance relative to the physical "heartbeat"
	// of the backend.
	//
	// 1. Physics (TPOT): The fundamental unit of work in an LLM is the Forward Pass (Time Per Output Token), typically
	//    ranging from 20ms to 50ms for standard deployments.
	// 2. Control Theory: To prevent "Aliasing" (missing a rapid queue buildup), the controller must sample state at a
	//     frequency comparable to the system's service rate.
	//
	// This interval ensures the controller detects and reacts to saturation events within 1-2 generation steps,
	// preventing large backlogs from accumulating unobserved between ticks, satisfying the Nyquist-Shannon sampling
	// theorem for inference workloads. It is also synchronized with the default data layer metric scrape interval to
	// ensure the controller acts on the freshest possible state without over-sampling noise.
	DefaultTickInterval time.Duration = 50 * time.Millisecond
)

// =====================================================================================================================
// Estimator Smoothing & Reactivity (The Signal Processors)
// =====================================================================================================================

const (
	// DefaultEffectiveBatchAlpha tunes the estimator for ^B_eff (Effective Batch Capacity).
	//
	// Rationale:
	// 0.2 equates to a "Sample Memory" (Center of Mass) of roughly 5 batches.
	// Since samples are only taken when the pod is physically saturated, this ensures the capacity estimate is dominated
	// by the most recent ~5 processing cycles. This allows the system to adapt to "Workload Phase Shifts" (e.g., a sudden
	// shift from Short Context to Long Context) within seconds, preventing sustained overload when the physical capacity
	// of the backend drops.
	DefaultEffectiveBatchAlpha float64 = 0.2

	// DefaultQueueDepthAlpha tunes the estimator for ^Q_t (Smoothed Queue Depth).
	//
	// Rationale:
	// 0.25 prioritizes responsiveness. Queue depth is the primary Error Signal (e(t)) for the feedback loop.
	// Excessive smoothing here introduces "Feedback Lag," which reduces the controller's Phase Margin and increases the
	// risk of oscillatory ringing (overshoot/undershoot) around the setpoint.
	DefaultQueueDepthAlpha float64 = 0.25

	// DefaultServiceRateWindow tunes the estimator for ^μ_t (Service Rate).
	//
	// Rationale:
	// 10s defines the "Trust Horizon" for throughput.
	// If a pod stops processing requests, this rate decays, forcing the Maturity Logic to invalidate the Feed-Forward
	// term and demote the pod (Mature -> Maturing). This prevents "Ghost Capacity" (historical high throughput that no
	// longer exists) from causing a dispatch stall when traffic resumes.
	DefaultServiceRateWindow time.Duration = 10 * time.Second
)

// =====================================================================================================================
// Estimator Memory & History (The Windows)
// =====================================================================================================================

const (
	// DefaultPeakInflightConcurrencyWindow is the sliding window duration for the L_peak (Peak Concurrency) estimator.
	//
	// Rationale:
	// 5 minutes balances "Long-Term Memory" with "Lifecycle Agility."
	// It allows the system to remember the pod's proven capacity through typical traffic lulls or minor disruptions.
	// However, if the underlying infrastructure degrades (e.g., thermal throttling, noisy neighbors) and stays degraded
	// for >5m, the window expires, allowing the system to lower its expectations and re-discover the new, true limit.
	DefaultPeakInflightConcurrencyWindow time.Duration = 5 * time.Minute

	// DefaultPeakInflightConcurrencySamples is the sample retention count for the L_peak Windowed Max Filter.
	//
	// Rationale:
	// 3 is the industry standard derived from Google's BBR congestion control algorithm.
	// It provides a robust "Peak Hold" that remembers the highest successful concurrency over the window duration,
	// ensuring the system remains stable even if recent traffic has been light.
	DefaultPeakInflightConcurrencySamples int = 3

	// DefaultKVCacheWindow tunes the Max-Filter for U_kv (KV Cache Utilization).
	//
	// Rationale:
	// 200ms (approx 4 ticks) acts as a "Peak Hold" circuit.
	// Unlike Compute, Memory is a hard limit where peaks cause failure (OOM). This window ensures that transient memory
	// spikes are held long enough for the P-Controller to observe and react to them, preventing the system from driving
	// into a wall during high-frequency scrape jitter.
	DefaultKVCacheWindow time.Duration = 200 * time.Millisecond

	// DefaultKVCacheSamples is the sample retention count for the U_kv (Memory Pressure) Windowed Max Filter.
	//
	// Rationale:
	// 3 is the industry standard derived from Google's BBR congestion control algorithm.
	// It ensures that a memory spike (the "Hard Limit") remains visible to the controller for at least 3 window
	// rotations, preventing the P-Controller from aggressively ramping up traffic immediately after a near-OOM event.
	DefaultKVCacheSamples int = 3
)

// =====================================================================================================================
// Lifecycle & State Machine Dynamics (The Trust Model)
// =====================================================================================================================

const (
	// DefaultMaturityQuorumPercentage is the threshold for transitioning from Probing (Concurrency Limit) to Regulating
	// (Rate Limit).
	//
	// Rationale:
	// 0.75 (75%) ensures the controller relies on Aggregate System Identification (Feed-Forward) only when the majority
	// of the fleet is well-characterized. Switching to Rate Control too early (with a minority of mature pods) risks
	// setting a global rate based on outliers, potentially overloading or starving the pending majority.
	DefaultMaturityQuorumPercentage float64 = 0.75

	// DefaultDormantTimeout is the duration before an idle, Immature pod is moved to the Dormant state.
	//
	// Rationale:
	// 5m balances "Memory" vs. "Staleness."
	// It allows pods to retain their learned capacity model during typical traffic lulls (preventing unnecessary
	// re-probing), but eventually invalidates the model to force re-discovery. This protects against "Drift" where the
	// environment (neighbor noise, cache fragmentation) changes while the pod is idle.
	DefaultDormantTimeout time.Duration = 5 * time.Minute

	// DefaultMetricsStalenessThreshold is the watchdog timer for the sensing pipeline.
	// A pod with metrics older than this threshold is considered "Unknown" and excluded from the control loop.
	//
	// Rationale:
	// 150ms acts as a grace period to absorb normal jitter in a distributed system (network + serialization).
	// It is set to 3x the default TickInterval (50ms) to tolerate up to 2 missed scrapes/reports before failing closed.
	//
	// WARNING: This default requires a high-frequency metrics pipeline.
	DefaultMetricsStalenessThreshold time.Duration = 150 * time.Millisecond
)

// =====================================================================================================================
// Statistical Confidence & Sampling (The Filters)
// =====================================================================================================================

const (
	// DefaultMinSamplesForEffectiveBatchMaturity is the minimum number of saturated batch samples required to trust the
	// ^B_eff_t (Effective Batch Capacity) estimate.
	//
	// Rationale:
	// 10 samples provide a statistically significant baseline to filter out noise from transient bottlenecks.
	// This defines the duration of the "Hill Climbing" phase (Immature State).
	DefaultMinSamplesForEffectiveBatchMaturity uint64 = 10

	// DefaultMinEffectiveCountForServiceRateMaturity is the minimum accumulated weight in the RateEWMA required to trust
	// the ^μ_t (Service Rate) estimate.
	//
	// Rationale:
	// 3.0 ensures the rate is based on a cluster of recent completions, not a single lucky fast request.
	// This prevents "Aliasing" where a sparse arrival pattern might momentarily look like infinite throughput.
	DefaultMinEffectiveCountForServiceRateMaturity float64 = 3.0

	// DefaultMinBatchSampleInterval is the mandatory cooldown between sampling ^B_eff.
	//
	// Rationale:
	// 500ms second ensures Statistical Independence.
	// Since the Controller ticks (50ms) may be faster than a Batch event (forward pass) (e.g., 20-200ms), sampling on
	// every tick could sample the *same* active batch many times. This Autocorrelation would bias the average towards
	// long-running requests. The cooldown forces the estimator to sample distinct processing cycles (a few forward
	// passes).
	DefaultMinBatchSampleInterval time.Duration = 500 * time.Millisecond
)

// =====================================================================================================================
// System Resource Sizing
// =====================================================================================================================

const (
	// DefaultMaxExpectedCompletionsQPS is the tuning parameter used to calculate the size of the internal
	// non-blocking completion event buffers.
	//
	// Rationale:
	// 1000 QPS represents a realistic high-throughput ceiling for a single Inference Pool (e.g., a cluster of 20+
	// high-performance GPUs).
	//
	// This value is NOT a rate limit. It is used in conjunction with the TickInterval and a safety multiplier (4x)
	// to pre-allocate the completion channel. Correct sizing is critical to absorb "Micro-Bursts" of completions
	// that occur between controller ticks (50ms), ensuring the Fast Path (Request Lifecycle) never blocks or drops
	// telemetry because the Slow Path (Control Loop) hasn't woken up yet.
	DefaultMaxExpectedCompletionsQPS int = 1000
)

// setDefaults applies the standard physics-based defaults to any optional fields that are not explicitly set.
func (c *SignalRecorderConfig) setDefaults() {
	if c.TickInterval == 0 {
		c.TickInterval = DefaultTickInterval
	}
	if c.MaxExpectedCompletionsQPS == 0 {
		c.MaxExpectedCompletionsQPS = DefaultMaxExpectedCompletionsQPS
	}
}

// setDefaults applies the standard physics-based defaults to any optional fields that are not explicitly set.
func (c *ControllerConfig) setDefaults() {
	// --- Control Loop Parameters ---
	if c.SaturationSetpoint == 0 {
		c.SaturationSetpoint = DefaultSaturationSetpoint
	}
	if c.SaturationHeadroom == 0 {
		c.SaturationHeadroom = DefaultSaturationHeadroom
	}
	if c.ProportionalGain == 0 {
		c.ProportionalGain = DefaultProportionalGain
	}
	if c.MinDispatchRate == 0 {
		c.MinDispatchRate = DefaultMinDispatchRate
	}

	// --- Estimator Smoothing ---
	if c.EffectiveBatchAlpha == 0 {
		c.EffectiveBatchAlpha = DefaultEffectiveBatchAlpha
	}
	if c.QueueDepthAlpha == 0 {
		c.QueueDepthAlpha = DefaultQueueDepthAlpha
	}
	if c.ServiceRateWindow == 0 {
		c.ServiceRateWindow = DefaultServiceRateWindow
	}

	// --- Estimator Memory ---
	if c.PeakInflightConcurrencyWindow == 0 {
		c.PeakInflightConcurrencyWindow = DefaultPeakInflightConcurrencyWindow
	}
	if c.PeakInflightConcurrencySamples == 0 {
		c.PeakInflightConcurrencySamples = DefaultPeakInflightConcurrencySamples
	}
	if c.KVCacheWindow == 0 {
		c.KVCacheWindow = DefaultKVCacheWindow
	}
	if c.KVCacheSamples == 0 {
		c.KVCacheSamples = DefaultKVCacheSamples
	}

	// --- Lifecycle & Trust ---
	if c.MaturityQuorumPercentage == 0 {
		c.MaturityQuorumPercentage = DefaultMaturityQuorumPercentage
	}
	if c.DormantTimeout == 0 {
		c.DormantTimeout = DefaultDormantTimeout
	}
	if c.MetricsStalenessThreshold == 0 {
		c.MetricsStalenessThreshold = DefaultMetricsStalenessThreshold
	}

	// --- Statistical Confidence ---
	if c.MinSamplesForEffectiveBatchMaturity == 0 {
		c.MinSamplesForEffectiveBatchMaturity = DefaultMinSamplesForEffectiveBatchMaturity
	}
	if c.MinEffectiveCountForServiceRateMaturity == 0 {
		c.MinEffectiveCountForServiceRateMaturity = DefaultMinEffectiveCountForServiceRateMaturity
	}

	// --- Sampling Physics ---
	if c.MinBatchSampleInterval == 0 {
		c.MinBatchSampleInterval = DefaultMinBatchSampleInterval
	}
}
