/*
Copyright 2026 The Kubernetes Authors.

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

// Package autotuner implements a distributed, autonomic controller that dynamically adjusts
// physical routing constraints (ResourceVectors) for the GPU hypervisor.
//
// Because LLM inference is highly sensitive to prompt geometry, static limits rapidly cause either
// hardware underutilization or queue collapse.
//
// The PodAutoTuner operates as a localized TCP BBR / MIMD controller for each endpoint:
//  1. Observability: Ingests extractor metrics to calculate true execution rates
//     (ThroughputTokensSec) and latency percentiles (P90 TPOT, P50 TTFT).
//  2. Continuous Calibration: Applies Multiplicative Increase / Multiplicative Decrease (MIMD)
//     logic bounded by Kleinrock's Power metric to locate the optimal continuous batching limit.
//
// By tracking actual SM FLOP boundaries (Prefill) and HBM bandwidth saturation (Decode), the
// Auto-Tuner ensures optimal batch formation without manual watermark configuration.
package autotuner

import (
	"context"
	"math"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/hypervisor"
)

// TunerConfig defines the configuration parameters for a PodAutoTuner.
type TunerConfig struct {
	// The tail-latency SLA boundary for DecodeTokens MIMD.
	TargetTPOT time.Duration

	// The queue depth boundary for PrefillTokens MIMD.
	MaxTargetQueueDelay time.Duration

	// MIMD Control Parameters
	IncreaseRatio float64 // e.g., 1.05 (+5%) gradient ascent step
	DecreaseRatio float64 // e.g., 0.80 (-20%) penalty on SLA breach
	BackoffRatio  float64 // e.g., 0.98 (-2%) micro-backoff when hitting the Power metric "knee"
	DeadbandRatio float64 // e.g., 0.05 (+/- 5% power shift required to trigger adjustment)

	// System Governors
	UtilizationThreshold float64 // e.g., 0.60 (Freeze tuning if demand drops below 60% of max capacity)
	HighWaterDecay       float64 // e.g., 0.995 (Slowly decay historical max capacity to un-stick the ceiling)
	MinSuccessSamples    uint64  // e.g., 30 requests per epoch required to trust the P90/P50 math

	// Slow-Start / Discovery Phase
	SlowStartMultiplier    float64 // e.g., 1.50 (50% growth sprint to find the initial hardware wall)
	MaxLimitChangePerEpoch float64 // Hard cap on multiplier to prevent catastrophic single-tick spikes

	// Absolute Overdrive Protection (Additive Caps)
	// Bounds the Multiplicative Increase to prevent runaway integer explosions on high base limits.
	MaxLimitAdditiveIncrease hypervisor.ResourceVector

	// Idle Reset Configuration
	IdleResetDuration time.Duration             // e.g., 5 * time.Minute
	DefaultLimits     hypervisor.ResourceVector // The safe baseline to fall back to upon wake
}

// DefaultTunerConfig returns standard production parameters validated for continuous batching.
func DefaultTunerConfig() TunerConfig {
	return TunerConfig{
		TargetTPOT:             100 * time.Millisecond,
		MaxTargetQueueDelay:    200 * time.Millisecond,
		IncreaseRatio:          1.05,
		DecreaseRatio:          0.80,
		BackoffRatio:           0.98,
		DeadbandRatio:          0.05,
		UtilizationThreshold:   0.60,
		HighWaterDecay:         0.995,
		MinSuccessSamples:      30,
		SlowStartMultiplier:    1.50,
		MaxLimitChangePerEpoch: 2.0,
		MaxLimitAdditiveIncrease: hypervisor.ResourceVector{
			DecodeTokens:  5000,
			PrefillTokens: 5000,
		},
		IdleResetDuration: 5 * time.Minute,
		DefaultLimits: hypervisor.ResourceVector{
			DecodeTokens:  10000,
			PrefillTokens: 10000,
		},
	}
}

type PodAutoTuner struct {
	endpointID string
	config     TunerConfig
	ledger     hypervisor.TopologyRegistry

	// Internal State
	currentLimits   hypervisor.ResourceVector
	maxThroughput   float64
	lastPower       float64
	inSlowStart     bool             // Tracks if the controller is sprinting to find the initial wall.
	lastTrafficTime time.Time        // Tracks idle phases for TCP-style state resets.
	clock           func() time.Time // Mockable time for testing
}

func NewPodAutoTuner(
	endpointID string,
	cfg TunerConfig,
	ledger hypervisor.TopologyRegistry,
	initialLimits hypervisor.ResourceVector,
) *PodAutoTuner {
	return &PodAutoTuner{
		endpointID:    endpointID,
		config:        cfg,
		ledger:        ledger,
		currentLimits: initialLimits,
		inSlowStart:   true,
		clock:         time.Now,
	}
}

func (t *PodAutoTuner) SetKVBlocks(totalKVBlocks int64) {
	t.currentLimits.KVBlocks = totalKVBlocks
}

func (t *PodAutoTuner) GetKVBlocks() int64 {
	return t.currentLimits.KVBlocks
}

func (t *PodAutoTuner) EvaluateEpoch(delta *datalayer.EpochDelta, currentUsed hypervisor.ResourceVector) {
	if t.processIdleState(delta, currentUsed) {
		return // Silently wait for traffic.
	}

	t.lastTrafficTime = t.clock()

	// Governor 1: Statistical Noise.
	// We require a minimum sample size to trust the P50/P90 math.
	// If the data is sparse, or if the metrics engine returned NaN, we freeze the tuning state.
	if delta.DeltaRequestSuccess < t.config.MinSuccessSamples {
		return
	}

	// Governor 2: Congestion Window Validation (CWV).
	// Are we demand-bound rather than hardware-bound?
	isUnderutilized := delta.ThroughputTokensSec < (t.maxThroughput * t.config.UtilizationThreshold)

	// Governor 3: Spatial VRAM Saturation.
	// Are we out of physical PagedAttention blocks?
	isKVBlocked := false
	if t.currentLimits.KVBlocks > 0 {
		isKVBlocked = (float64(currentUsed.KVBlocks) / float64(t.currentLimits.KVBlocks)) > 0.90
	}

	t.calibrateHighWaterMark(delta, isUnderutilized)

	currentPower := delta.ThroughputTokensSec / delta.P90TPOT
	newDecodeLimit, exitedSlowStart := t.calculateDecodeLimit(delta, currentPower, isUnderutilized, isKVBlocked)
	newPrefillLimit := t.calculatePrefillLimit(delta, isUnderutilized, isKVBlocked)

	t.lastPower = currentPower
	if exitedSlowStart {
		t.inSlowStart = false
	}

	// Floor limits to prevent division panics/deadlocks.
	// Note: KVBlocks and ActiveRequests are structurally locked and controlled via telemetry updates,
	// not scaled by the Autotuner.
	newLimits := hypervisor.ResourceVector{
		DecodeTokens:   max(1, newDecodeLimit),
		PrefillTokens:  max(1, newPrefillLimit),
		KVBlocks:       t.currentLimits.KVBlocks,
		ActiveRequests: t.currentLimits.ActiveRequests,
	}

	if newLimits != t.currentLimits {
		t.ledger.UpdateEndpointConfig(context.TODO(), t.endpointID, hypervisor.EndpointConfigPatch{Limits: &newLimits})
		t.currentLimits = newLimits
	}
}

// processIdleState manages TCP-style connection state resets upon extended periods of no traffic.
// Returns true if the controller is in the idle state and no further tuning should occur this tick.
func (t *PodAutoTuner) processIdleState(delta *datalayer.EpochDelta, currentUsed hypervisor.ResourceVector) bool {
	if delta.DeltaRequestSuccess == 0 && delta.ThroughputTokensSec == 0 && currentUsed.ActiveRequests == 0 {
		if !t.lastTrafficTime.IsZero() && t.clock().Sub(t.lastTrafficTime) > t.config.IdleResetDuration {
			if !t.inSlowStart {
				t.currentLimits = t.config.DefaultLimits
				t.inSlowStart = true
				t.lastPower = 0
				t.maxThroughput = 0

				// Push reset limits to the ledger immediately to bound the incoming "wake" burst.
				t.ledger.UpdateEndpointConfig(context.TODO(), t.endpointID, hypervisor.EndpointConfigPatch{Limits: &t.currentLimits})
			}
		}
		return true
	}
	return false
}

// calibrateHighWaterMark updates the historical maximum observed throughput, slowly decaying it to
// prevent the controller from getting trapped by past workload geometries.
func (t *PodAutoTuner) calibrateHighWaterMark(delta *datalayer.EpochDelta, isUnderutilized bool) {
	if delta.ThroughputTokensSec > t.maxThroughput {
		t.maxThroughput = delta.ThroughputTokensSec
	} else if !isUnderutilized {
		// Slowly decay the ceiling so the controller isn't permanently trapped by past glory if the
		// traffic shifts to a structurally heavier prompt geometry. We only decay when the system is
		// under active load to avoid penalizing the high-water mark during standard lulls.
		t.maxThroughput *= t.config.HighWaterDecay
	}
}

// calcMultiplicativeIncrease executes the bounded gradient ascent logic.
func calcMultiplicativeIncrease(currentLimit int64, ratio float64, additiveCap int64) int64 {
	proportionalLimit := float64(currentLimit) * ratio

	if additiveCap > 0 {
		additiveLimit := float64(currentLimit + additiveCap)
		proportionalLimit = math.Min(proportionalLimit, additiveLimit)
	}

	// math.Ceil prevents floating-point truncation from trapping the limit at low integers.
	return int64(math.Ceil(proportionalLimit))
}

// calculateDecodeLimit applies Bandwidth MIMD + Kleinrock's Power Deadband math to tune Decode Tokens.
// Returns the newly calculated limit and whether the controller has permanently exited Slow-Start.
func (t *PodAutoTuner) calculateDecodeLimit(
	delta *datalayer.EpochDelta,
	currentPower float64,
	isUnderutilized bool,
	isKVBlocked bool,
) (newLimit int64, slowStartExited bool) {
	currentTPOT := time.Duration(delta.P90TPOT * float64(time.Second))

	// SLA breach (hardware wall hit): Always triggers the Multiplicative Decrease penalty, regardless
	// of safety freezes, to ensure load shedding.
	if currentTPOT > t.config.TargetTPOT {
		return t.applyBandwidthPenalty(delta)
	}

	// Safety freeze: Do not scale up if demand is low (CWV) or VRAM is physically saturated.
	if isUnderutilized || isKVBlocked {
		return t.currentLimits.DecodeTokens, false
	}

	// Gradient ascent phases
	if t.inSlowStart {
		return t.applySlowStartGrowth(), false
	}

	return t.applyCongestionAvoidance(currentPower), false
}

// applyBandwidthPenalty executes the multiplicative decrease (standard MIMD penalty) upon SLA breach.
func (t *PodAutoTuner) applyBandwidthPenalty(delta *datalayer.EpochDelta) (newLimit int64, slowStartExited bool) {
	slowStartExited = t.inSlowStart // Hardware wall found. Permanently exit exponential Slow-Start.
	newLimit = t.currentLimits.DecodeTokens

	// HBM cross-talk governor: If Prefill FLOPs are heavily saturating the HBM bus, TTFT will
	// spike, inflating TPOT. Wait for the continuous-batching pipeline to clear before punishing
	// Decode bandwidth limits.
	queueDelaySeconds := max(delta.P50TTFT-delta.P50Prefill, 0.0)
	queueDelay := time.Duration(queueDelaySeconds * float64(time.Second))
	if queueDelay <= t.config.MaxTargetQueueDelay {
		newLimit = int64(float64(t.currentLimits.DecodeTokens) * t.config.DecreaseRatio)
	}
	return newLimit, slowStartExited
}

// applySlowStartGrowth executes Phase 1: TCP Slow-Start (Exponential Growth).
// Sprints towards the wall to discover the hardware's maximum capacity rapidly.
func (t *PodAutoTuner) applySlowStartGrowth() int64 {
	multiplier := t.config.SlowStartMultiplier
	if t.config.MaxLimitChangePerEpoch > 0 && multiplier > t.config.MaxLimitChangePerEpoch {
		multiplier = t.config.MaxLimitChangePerEpoch
	}

	// Multiplicative Increase based strictly on current limits to allow compounding growth.
	return calcMultiplicativeIncrease(
		t.currentLimits.DecodeTokens,
		multiplier,
		t.config.MaxLimitAdditiveIncrease.DecodeTokens,
	)
}

// applyCongestionAvoidance executes Phase 2: Congestion Avoidance (MIMD / Kleinrock's Power Deadband).
func (t *PodAutoTuner) applyCongestionAvoidance(currentPower float64) int64 {
	// Gradient ascents are safe here because the isUnderutilized check executed prior to this acts as
	// a strict Congestion Window Validation (CWV), preventing idle limit wind-up.
	if t.lastPower == 0 {
		return calcMultiplicativeIncrease(
			t.currentLimits.DecodeTokens,
			t.config.IncreaseRatio,
			t.config.MaxLimitAdditiveIncrease.DecodeTokens,
		)
	}

	powerDelta := (currentPower - t.lastPower) / t.lastPower

	if powerDelta > t.config.DeadbandRatio {
		// Power is increasing: Hardware has more bandwidth. Safe to scale up limits.
		return calcMultiplicativeIncrease(
			t.currentLimits.DecodeTokens,
			t.config.IncreaseRatio,
			t.config.MaxLimitAdditiveIncrease.DecodeTokens,
		)
	} else if powerDelta < -t.config.DeadbandRatio {
		// The Knee: Kleinrock's Power dropped. We hit the physical memory bandwidth wall.
		// Apply a micro-backoff to stabilize precisely at peak throughput without triggering an SLA breach.
		return int64(math.Floor(float64(t.currentLimits.DecodeTokens) * t.config.BackoffRatio))
	}

	// Power is stable within the deadband; maintain current limits.
	return t.currentLimits.DecodeTokens
}

// calculatePrefillLimit applies queue-heuristic MIMD math to tune the Prefill Compute Limits on the
// SM boundaries.
func (t *PodAutoTuner) calculatePrefillLimit(
	delta *datalayer.EpochDelta,
	isUnderutilized,
	isKVBlocked bool,
) (newLimit int64) {
	newLimit = t.currentLimits.PrefillTokens
	queueDelaySeconds := max(delta.P50TTFT-delta.P50Prefill, 0.0)
	queueDelay := time.Duration(queueDelaySeconds * float64(time.Second))

	if queueDelay > t.config.MaxTargetQueueDelay {
		// Local queue is backing up (SMs saturated): Multiplicative decrease always fires.
		newLimit = int64(float64(t.currentLimits.PrefillTokens) * 0.90)
	} else if queueDelay < (t.config.MaxTargetQueueDelay / 3) {
		// Queue is emptying (SMs starving): Multiplicative increase.
		// Safety freeze: Do not scale up if demand is low or VRAM is saturated.
		if isUnderutilized || isKVBlocked {
			return newLimit
		}
		newLimit = calcMultiplicativeIncrease(
			t.currentLimits.PrefillTokens,
			1.05,
			t.config.MaxLimitAdditiveIncrease.PrefillTokens,
		)
	}

	return newLimit
}
