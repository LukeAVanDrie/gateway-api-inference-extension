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

package autotuner

import (
	"context"
	"math"
	"math/rand"
	"testing"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/hypervisor"
)

// Mock Ledger for capturing updates
type mockLedger struct {
	hypervisor.TopologyRegistry
	currentLimits hypervisor.ResourceVector
}

func (m *mockLedger) UpdateEndpointConfig(ctx context.Context, endpointID string, cfg hypervisor.EndpointConfigPatch) {
	if cfg.Limits != nil {
		m.currentLimits = *cfg.Limits
	}
	if cfg.TotalKVBlocks != nil {
		m.currentLimits.KVBlocks = *cfg.TotalKVBlocks
	}
	if cfg.MaxActiveRequests != nil {
		m.currentLimits.ActiveRequests = *cfg.MaxActiveRequests
	}
}

func simulateHardware(limit int64, noise float64, peakThroughput float64, baseTPOT float64) datalayer.EpochDelta {
	utilization := float64(limit) / peakThroughput

	// Cubic decay simulates catastrophic ceiling breach curve.
	simulatedTPOT := baseTPOT + (utilization * utilization * utilization * 0.150)

	achievedThroughput := math.Min(float64(limit), peakThroughput)

	// Add statistical noise jitter.
	jitter := (rand.Float64() * noise) - (noise / 2.0)

	return datalayer.EpochDelta{
		ThroughputTokensSec: achievedThroughput * (1.0 + jitter),
		P90TPOT:             simulatedTPOT * (1.0 + jitter),
		DeltaRequestSuccess: 100,
	}
}

func getBaseConfig() TunerConfig {
	return TunerConfig{
		TargetTPOT:           100 * time.Millisecond,
		MaxTargetQueueDelay:  200 * time.Millisecond,
		IncreaseRatio:        1.05,
		DecreaseRatio:        0.80,
		BackoffRatio:         0.98,
		DeadbandRatio:        0.02,
		UtilizationThreshold: 0.60,
		HighWaterDecay:       0.995,
		MinSuccessSamples:    5,
		SlowStartMultiplier:  1.50,
		IdleResetDuration:    5 * time.Minute,
		DefaultLimits:        hypervisor.ResourceVector{DecodeTokens: 1000},
	}
}

// Test 1: Exponential Discovery (Slow Start)
func TestSlowStartDiscovery(t *testing.T) {
	t.Parallel()
	ledger := &mockLedger{currentLimits: hypervisor.ResourceVector{DecodeTokens: 1000}}
	tuner := NewPodAutoTuner("test-ep", getBaseConfig(), ledger, hypervisor.ResourceVector{DecodeTokens: 1000})

	// Run without physical bounds for Slow Start
	peakThroughput := 50000.0
	baseTPOT := 0.035

	for i := 0; i < 5; i++ {
		// No noise for reliable deterministic testing on initialization scaling
		delta := simulateHardware(ledger.currentLimits.DecodeTokens, 0.0, peakThroughput, baseTPOT)
		tuner.EvaluateEpoch(&delta, hypervisor.ResourceVector{})
	}

	// In 5 epochs with 1.5x multiplier: 1000 * 1.5 ^ 5 = ~7593
	if ledger.currentLimits.DecodeTokens < 7000 {
		t.Errorf("Limit scaling slower than expected Slow Start curve: %d", ledger.currentLimits.DecodeTokens)
	}
}

// Test 2: Convergence and Dampening (AIMD)
func TestAIMDConvergence(t *testing.T) {
	t.Parallel()
	ledger := &mockLedger{currentLimits: hypervisor.ResourceVector{DecodeTokens: 1000}}
	tuner := NewPodAutoTuner("test-ep", getBaseConfig(), ledger, hypervisor.ResourceVector{DecodeTokens: 1000})

	peakThroughput := 50000.0
	baseTPOT := 0.035

	// Run 60 epochs to see if it clamps correctly.
	for i := 0; i < 60; i++ {
		delta := simulateHardware(ledger.currentLimits.DecodeTokens, 0.10, peakThroughput, baseTPOT)
		tuner.EvaluateEpoch(&delta, hypervisor.ResourceVector{})
	}

	finalLimit := ledger.currentLimits.DecodeTokens
	// Kleinrock's Power optimization converges naturally near maximum efficiency, rather than
	// physical wall. This math curve pushes peak efficiency roughly around the 25k - 33k limit range.
	if finalLimit < 25000 || finalLimit > 35000 {
		t.Errorf("Control loop failed to settle around efficiency peak. Final: %d", finalLimit)
	}
}

// Test 3: Idle Reset Sequence
func TestIdleReset(t *testing.T) {
	t.Parallel()
	ledger := &mockLedger{currentLimits: hypervisor.ResourceVector{DecodeTokens: 1000}}
	tuner := NewPodAutoTuner("test-ep", getBaseConfig(), ledger, hypervisor.ResourceVector{DecodeTokens: 1000})

	peakThroughput := 50000.0
	baseTPOT := 0.035

	// 1. Prime state
	for i := 0; i < 20; i++ {
		delta := simulateHardware(ledger.currentLimits.DecodeTokens, 0.05, peakThroughput, baseTPOT)
		tuner.EvaluateEpoch(&delta, hypervisor.ResourceVector{})
	}

	if ledger.currentLimits.DecodeTokens < 15000 {
		t.Errorf("Limits failed to prime")
	}

	// 2. Mock clock into the future
	now := time.Now()
	tuner.clock = func() time.Time { return now.Add(10 * time.Minute) }

	// 3. Send 0 traffic
	zeroDelta := datalayer.EpochDelta{
		ThroughputTokensSec: 0,
		DeltaRequestSuccess: 0,
	}
	tuner.EvaluateEpoch(&zeroDelta, hypervisor.ResourceVector{})

	// Limits should be reset to default (1000)
	if ledger.currentLimits.DecodeTokens != 1000 {
		t.Errorf("Expected reset to DefaultLimits config (1000), got %d", ledger.currentLimits.DecodeTokens)
	}
}

// Test 4: Phase Shift (Workload Scale Bounds Transition)
func TestPhaseShiftSLACorrection(t *testing.T) {
	t.Parallel()
	ledger := &mockLedger{currentLimits: hypervisor.ResourceVector{DecodeTokens: 1000}}
	tuner := NewPodAutoTuner("test-ep", getBaseConfig(), ledger, hypervisor.ResourceVector{DecodeTokens: 1000})

	peakThroughput := 50000.0
	baseTPOT := 0.040

	// 1. Warm cycle - stabilize limits
	for i := 0; i < 30; i++ {
		delta := simulateHardware(ledger.currentLimits.DecodeTokens, 0.0, peakThroughput, baseTPOT)
		tuner.EvaluateEpoch(&delta, hypervisor.ResourceVector{})
	}

	highWaterLimit := ledger.currentLimits.DecodeTokens
	if highWaterLimit < 25000 {
		t.Fatalf("Initialization scaling failed, reached only: %d", highWaterLimit)
	}

	// 2. Phase Shift! Hardware constrained to roughly half.
	constrainedPeak := 25000.0
	constrainedTPOT := 0.080 // doubled

	for i := 0; i < 15; i++ {
		delta := simulateHardware(ledger.currentLimits.DecodeTokens, 0.0, constrainedPeak, constrainedTPOT)
		tuner.EvaluateEpoch(&delta, hypervisor.ResourceVector{})
	}

	// Verify limit plummeted into safe clamping bounds of constrained geometry.
	if ledger.currentLimits.DecodeTokens >= highWaterLimit {
		t.Errorf("Limits failed to decrease after constraint shift.")
	}
	if ledger.currentLimits.DecodeTokens > 22000 {
		t.Errorf("Limits did not clamp efficiently enough. Final: %d, Threshold ~20000", ledger.currentLimits.DecodeTokens)
	}
}

func TestEdgeCases(t *testing.T) {
	t.Parallel()

	ledger := &mockLedger{currentLimits: hypervisor.ResourceVector{DecodeTokens: 1000, PrefillTokens: 1000, KVBlocks: 1000}}
	tuner := NewPodAutoTuner("test-ep", getBaseConfig(), ledger, hypervisor.ResourceVector{DecodeTokens: 1000, PrefillTokens: 1000, KVBlocks: 1000})

	tuner.inSlowStart = false    // Disable slow start for governor tests
	tuner.maxThroughput = 5000.0 // Give it a theoretical peak to test underutilization against

	// 1. Underutilization (Governor 2): Limits should not decrease or increase because it's in demand backoff.
	underutilDelta := datalayer.EpochDelta{
		DeltaRequestSuccess: 10,
		ThroughputTokensSec: 100, // Very low compared to current limit of 1000
		P90TPOT:             0.05,
	}
	tuner.EvaluateEpoch(&underutilDelta, hypervisor.ResourceVector{KVBlocks: 0})
	if ledger.currentLimits.DecodeTokens != 1000 {
		t.Errorf("Underutilization failure: limits changed from 1000, got %d", ledger.currentLimits.DecodeTokens)
	}

	// 2. KV Cache Safety Override (Governor 3): Freeze tuning when tightly memory bound
	kvSafetyDelta := datalayer.EpochDelta{
		DeltaRequestSuccess: 10,
		ThroughputTokensSec: 1000,
		P90TPOT:             0.05,
	}
	tuner.EvaluateEpoch(&kvSafetyDelta, hypervisor.ResourceVector{KVBlocks: 950}) // 95% utilization
	if ledger.currentLimits.DecodeTokens != 1000 {
		t.Errorf("KV Safety Override failure: limits tuned despite 95%% utilization, got %d", ledger.currentLimits.DecodeTokens)
	}

	// Reset utilization logic so we are NOT underutilized
	tuner.maxThroughput = 1000.0

	// 3. Queue backing up (Prefill PI Decrement): TTL exceeds limit
	queueDelta := datalayer.EpochDelta{
		DeltaRequestSuccess: 10,
		ThroughputTokensSec: 1000,
		P50TTFT:             5.0, // Massive queue delay
		P50Prefill:          0.1,
		P90TPOT:             0.05,
	}
	tuner.EvaluateEpoch(&queueDelta, hypervisor.ResourceVector{KVBlocks: 0})
	if ledger.currentLimits.PrefillTokens >= 1000 {
		t.Errorf("Queue backing up failure: Prefill tokens should have multiplicative decreased, got %d", ledger.currentLimits.PrefillTokens)
	}

	// Reset limits for the next test
	tuner.currentLimits = hypervisor.ResourceVector{DecodeTokens: 1, PrefillTokens: 1, KVBlocks: 1000}
	ledger.currentLimits = hypervisor.ResourceVector{DecodeTokens: 1, PrefillTokens: 1, KVBlocks: 1000}

	// Exiting Slow Start
	tuner.inSlowStart = false

	// Reset utilization logic so we are NOT underutilized
	tuner.maxThroughput = 1000.0

	// 4. Additive Increase via Math.Ceil (DecodeTokens = 1 -> should go to 2+)
	increaseDelta := datalayer.EpochDelta{
		DeltaRequestSuccess: 10,
		ThroughputTokensSec: 1000,
		P90TPOT:             0.035, // Below TargetTPOT
	}
	tuner.lastPower = 100.0 // Ensure we evaluate past dynamic seeding logic
	tuner.EvaluateEpoch(&increaseDelta, hypervisor.ResourceVector{KVBlocks: 0})
	if ledger.currentLimits.DecodeTokens <= 1 {
		t.Errorf("Additive Increase failure: Limits failed to scale upwards via Math.Ceil, got %d", ledger.currentLimits.DecodeTokens)
	}

	// 5. Absolute Floor Safe Limits (prevents DecodeTokens < 1, etc.)
	floorDelta := datalayer.EpochDelta{
		DeltaRequestSuccess: 10,
		ThroughputTokensSec: 1000,
		P90TPOT:             5.0, // High TPOT triggers decrease
	}

	tuner.currentLimits = hypervisor.ResourceVector{DecodeTokens: 0, PrefillTokens: -5}
	tuner.EvaluateEpoch(&floorDelta, hypervisor.ResourceVector{KVBlocks: 0})

	if ledger.currentLimits.DecodeTokens < 1 || ledger.currentLimits.PrefillTokens < 1 {
		t.Errorf("Floor failure: Limits were allowed to be less than 1. Decode: %d, Prefill: %d", ledger.currentLimits.DecodeTokens, ledger.currentLimits.PrefillTokens)
	}

	// 6. Statistical Noise Governor (Governor 1): Ignore epoch without enough requests
	noiseDelta := datalayer.EpochDelta{
		DeltaRequestSuccess: 1, // Below min threshold
		ThroughputTokensSec: 1000,
		P90TPOT:             0.05,
	}
	tuner.currentLimits = hypervisor.ResourceVector{DecodeTokens: 1000, PrefillTokens: 1000, KVBlocks: 1000}
	ledger.currentLimits = hypervisor.ResourceVector{DecodeTokens: 1000, PrefillTokens: 1000, KVBlocks: 1000}

	tuner.EvaluateEpoch(&noiseDelta, hypervisor.ResourceVector{KVBlocks: 0})

	if ledger.currentLimits.DecodeTokens != 1000 {
		t.Errorf("Statistical Noise Governor failure: Limits tuned despite low sample count, got %d", ledger.currentLimits.DecodeTokens)
	}
}
