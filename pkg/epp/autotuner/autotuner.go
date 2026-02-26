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

// Package autotuner implements a distributed controller designed to dynamically adjust token
// throughput limits for the distributed GPU hypervisor.
//
// LLM generation physics (Memory Bandwidth, VRAM utilization) are highly sensitive to prompt
// geometry, and hard-coded concurrency maximums rapidly degrade across workloads.
// Autotuner acts as an autonomous Scaling Driver:
//  1. Data Plane Analysis: Aggregates Prometheus metrics via Epoch Engines to locate physical
//     hardware walls.
//  2. Controlled Scaling: Applies Proportional Increase / Multiplicative Decrease (PI/MD) logic to
//     accelerate throughput during peak efficiently without triggering catastrophic throughput and
//     latency collapse.
//
// The Autotuner handles dynamic scaling locally for each endpoint and ensures execution guarantees
// without relying on manual watermark tweaks.
package autotuner

import (
	"math"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/hypervisor"
)

type TunerConfig struct {
	// The SLA boundary for DecodeTokens AIMD
	TargetTPOT time.Duration

	// The queue depth boundary for PrefillTokens Controller
	MaxTargetQueueDelay time.Duration

	// AIMD parameters
	IncreaseRatio float64 // e.g., 1.05 (+5%)
	DecreaseRatio float64 // e.g., 0.80 (-20%)
	BackoffRatio  float64 // e.g., 0.98 (-2%) when hitting the "knee"
	DeadbandRatio float64 // e.g., 0.02 (+/- 2% power shift required to act)

	// Governors
	UtilizationThreshold float64 // e.g., 0.80 (80% of historical max throughput)
	HighWaterDecay       float64 // e.g., 0.995 (Decay max capacity to adapt to workload shifts)
	MinSuccessSamples    uint64  // e.g., 5 requests per epoch to trust the math

	// Slow-Start Configuration
	SlowStartMultiplier float64 // e.g., 1.50 (50% growth per epoch until first SLA breach)

	MaxLimitChangePerEpoch float64 // Maximum multiplier for any single auto-tuner epoch (e.g. 2.0 = 100% growth limit)

	// Overdrive Protection: Fixed Token Increment capping
	MaxLimitAdditiveIncrease hypervisor.ResourceVector // Maximum absolutely tokens to increment in a single epoch (e.g. at most +5000 tokens)

	// Idle Reset Configuration
	IdleResetDuration time.Duration             // e.g., 5 * time.Minute
	DefaultLimits     hypervisor.ResourceVector // The safe baseline to fall back to
}

// DefaultTunerConfig returns the standard fallback and initialization production settings
// for the Autotuner based on validation benchmarks.
func DefaultTunerConfig() TunerConfig {
	return TunerConfig{
		TargetTPOT:             100 * time.Millisecond,
		MaxTargetQueueDelay:    200 * time.Millisecond,
		IncreaseRatio:          1.05,
		DecreaseRatio:          0.80,
		BackoffRatio:           0.98,
		DeadbandRatio:          0.02,
		UtilizationThreshold:   0.60,
		HighWaterDecay:         0.995,
		MinSuccessSamples:      5,
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
	ledger     hypervisor.TokenLedger

	// Internal State
	currentLimits   hypervisor.ResourceVector
	maxThroughput   float64
	lastPower       float64
	inSlowStart     bool             // Tracks which control phase we are in
	lastTrafficTime time.Time        // Tracks when traffic was last observed
	clock           func() time.Time // Used to mock time for idle tracking tests
}

func NewPodAutoTuner(
	endpointID string,
	cfg TunerConfig,
	ledger hypervisor.TokenLedger,
	initialLimits hypervisor.ResourceVector,
) *PodAutoTuner {
	return &PodAutoTuner{
		endpointID:    endpointID,
		config:        cfg,
		ledger:        ledger,
		currentLimits: initialLimits,
		inSlowStart:   true, // Every new pod starts in exponential discovery phase
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
	// --- Idle Tracking & TCP Slow-Start Reset ---
	if delta.DeltaRequestSuccess == 0 && delta.ThroughputTokensSec == 0 && currentUsed.ActiveRequests == 0 {
		// If we've been idle for longer than the threshold, scrub the state and arm the Slow-Start
		// sequence for the next wave of traffic.
		if !t.lastTrafficTime.IsZero() && t.clock().Sub(t.lastTrafficTime) > t.config.IdleResetDuration {
			if !t.inSlowStart { // Only perform the reset sequence once per idle phase.
				t.currentLimits = t.config.DefaultLimits
				t.inSlowStart = true
				t.lastPower = 0
				t.maxThroughput = 0

				// Push the reset limits to the ledger so the Gateway stops admitting at the old high-water
				// mark.
				t.ledger.UpdateEndpointConfig(t.endpointID, hypervisor.EndpointConfig{Limits: &t.currentLimits})
			}
		}
		return // Silently wait for traffic.
	}

	// We have active traffic. Update the heartbeat.
	t.lastTrafficTime = t.clock()

	// --- Governor 1: Statistical Noise ---
	if delta.DeltaRequestSuccess < t.config.MinSuccessSamples {
		return
	}

	// --- Governor 2: Adaptive High-Water Mark & Underutilization Freeze ---
	isUnderutilized := delta.ThroughputTokensSec < (float64(t.currentLimits.DecodeTokens) * t.config.UtilizationThreshold)
	if delta.ThroughputTokensSec > t.maxThroughput {
		t.maxThroughput = delta.ThroughputTokensSec
	} else if !isUnderutilized {
		// Slowly decay the high-water mark so we don't get permanently trapped by past glory if the
		// prompt geometry shifts to a heavier workload. Only decay if we are under load but failing to
		// reach past maximums; do not decay during traffic lulls.
		t.maxThroughput *= t.config.HighWaterDecay
	}

	// If throughput is less than X% of our known max, we are demand-bound, not hardware-bound.
	// Do not tune limits based on low client demand.
	if isUnderutilized {
		return
	}

	// --- Governor 3: KV Cache Safety Override ---
	if t.currentLimits.KVBlocks > 0 {
		kvUtil := float64(currentUsed.KVBlocks) / float64(t.currentLimits.KVBlocks)
		if kvUtil > 0.90 {
			return // Memory bound. Freeze compute tuning to prevent OOM.
		}
	}

	newLimits := t.currentLimits

	// --- Controller A: DecodeTokens (AIMD + Kleinrock's Power Deadband) ---
	currentTPOT := time.Duration(delta.P90TPOT * float64(time.Second))

	if currentTPOT > t.config.TargetTPOT {
		// --- Congestion Event (SLA Breach) ---

		if t.inSlowStart {
			// We found the hardware wall! Permanently exit Slow-Start.
			t.inSlowStart = false
		}

		// Multiplicative Decrease (Standard AIMD penalty)

		// --- HBM Cross-Talk Governor ---
		// If Prefill is clogging the HBM bandwidth, do NOT punish the Decode limits due to high latency.
		// Wait for the Prefill bottleneck to clear.
		queueDelaySeconds := max(delta.P50TTFT-delta.P50Prefill, 0)
		queueDelay := time.Duration(queueDelaySeconds * float64(time.Second))
		if queueDelay <= t.config.MaxTargetQueueDelay {
			newLimits.DecodeTokens = int64(float64(t.currentLimits.DecodeTokens) * t.config.DecreaseRatio)
		}
	} else {
		// --- Gradient Ascent ---
		currentPower := delta.ThroughputTokensSec / delta.P90TPOT

		if t.inSlowStart {
			// Phase 1: TCP Slow-Start (Exponential Growth)
			// Sprint towards the wall to clear the Gateway queue debt rapidly.
			multiplier := t.config.SlowStartMultiplier
			if t.config.MaxLimitChangePerEpoch > 0 && multiplier > t.config.MaxLimitChangePerEpoch {
				multiplier = t.config.MaxLimitChangePerEpoch
			}

			// Multiplicative baseline
			proportionalLimit := float64(t.currentLimits.DecodeTokens) * multiplier

			// Additive bound
			if t.config.MaxLimitAdditiveIncrease.DecodeTokens > 0 {
				additiveLimit := float64(t.currentLimits.DecodeTokens + t.config.MaxLimitAdditiveIncrease.DecodeTokens)
				proportionalLimit = math.Min(proportionalLimit, additiveLimit)
			}

			newLimits.DecodeTokens = int64(math.Ceil(proportionalLimit))
			t.lastPower = currentPower // Seed the power metric for a smooth handoff
		} else {
			// Phase 2: PI Control / Kleinrock's Power Deadband (Congestion Avoidance)
			// Proportional Increase / Decrements are still bounded by absolute token step increase, to ensure
			// stability during rapid convergence phases.
			if t.lastPower == 0 {
				t.lastPower = currentPower
				proportionalLimit := float64(t.currentLimits.DecodeTokens) * t.config.IncreaseRatio
				if t.config.MaxLimitAdditiveIncrease.DecodeTokens > 0 {
					additiveLimit := float64(t.currentLimits.DecodeTokens + t.config.MaxLimitAdditiveIncrease.DecodeTokens)
					proportionalLimit = math.Min(proportionalLimit, additiveLimit)
				}
				newLimits.DecodeTokens = int64(math.Ceil(proportionalLimit))
			} else {
				powerDelta := (currentPower - t.lastPower) / t.lastPower

				if powerDelta > t.config.DeadbandRatio {
					// Proportional Increase is safe here because the High-Water Underutilization governor
					// prevents runaway scaling when there is no actual demand.
					//
					// math.Ceil prevents getting trapped at bottom via truncation
					proportionalLimit := float64(t.currentLimits.DecodeTokens) * t.config.IncreaseRatio
					if t.config.MaxLimitAdditiveIncrease.DecodeTokens > 0 {
						additiveLimit := float64(t.currentLimits.DecodeTokens + t.config.MaxLimitAdditiveIncrease.DecodeTokens)
						proportionalLimit = math.Min(proportionalLimit, additiveLimit)
					}
					newLimits.DecodeTokens = int64(math.Ceil(proportionalLimit))
				} else if powerDelta < -t.config.DeadbandRatio {
					// The Knee: We hit the physical memory bandwidth wall. Back off slightly.
					newLimits.DecodeTokens = int64(math.Floor(float64(t.currentLimits.DecodeTokens) * t.config.BackoffRatio))
				}
				t.lastPower = currentPower
			}
		}
	}

	// --- Controller B: PrefillTokens (PI / Queue Delay Heuristic) ---
	queueDelaySeconds := delta.P50TTFT - delta.P50Prefill

	// Histogram interpolation artifact protection
	if queueDelaySeconds < 0 {
		queueDelaySeconds = 0
	}
	queueDelay := time.Duration(queueDelaySeconds * float64(time.Second))

	if queueDelay > t.config.MaxTargetQueueDelay {
		// Queue backing up: Multiplicative Decrease
		newLimits.PrefillTokens = int64(float64(t.currentLimits.PrefillTokens) * 0.90)
	} else if queueDelay < (t.config.MaxTargetQueueDelay / 3) {
		// Queue empty (SMs starving): Proportional Increase (with Ceil protection)
		proportionalLimit := float64(t.currentLimits.PrefillTokens) * 1.05
		if t.config.MaxLimitAdditiveIncrease.PrefillTokens > 0 {
			additiveLimit := float64(t.currentLimits.PrefillTokens + t.config.MaxLimitAdditiveIncrease.PrefillTokens)
			proportionalLimit = math.Min(proportionalLimit, additiveLimit)
		}
		newLimits.PrefillTokens = int64(math.Ceil(proportionalLimit))
	}

	// --- Apply Limits (The Output) ---
	// Absolute floor to prevent division panics/deadlocks
	if newLimits.DecodeTokens < 1 {
		newLimits.DecodeTokens = 1
	}
	if newLimits.PrefillTokens < 1 {
		newLimits.PrefillTokens = 1
	}

	if newLimits != t.currentLimits {
		t.ledger.UpdateEndpointConfig(t.endpointID, hypervisor.EndpointConfig{Limits: &newLimits})
		t.currentLimits = newLimits
	}
}
