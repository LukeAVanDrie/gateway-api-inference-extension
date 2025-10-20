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
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
)

// approxDurationComparer is a cmp.Option for comparing time.Duration with a small tolerance.
var approxDurationComparer = cmp.Comparer(func(x, y time.Duration) bool {
	delta := x - y
	if delta < 0 {
		delta = -delta
	}
	// Use a tolerance (e.g., 1 microsecond) for floating-point derived durations.
	return delta <= time.Microsecond
})

// float64EqualityOpt is used for comparing floating-point numbers with a tolerance.
var float64EqualityOpt = cmpopts.EquateApprox(0.00001, 0.0)

// --- Mock Implementations and Helpers ---

// mockPodMetrics is a test double for backendmetrics.PodMetrics that allows injecting specific metrics states and
// tracking access counts for cache validation.
type mockPodMetrics struct {
	datalayer.Endpoint
	Pod                *backend.Pod
	EWMAMetrics        *backendmetrics.EWMAMetrics
	MetricsState       *backendmetrics.MetricsState
	EWMAAccessCount    int
	MetricsAccessCount int
	mu                 sync.Mutex
}

// GetPod implements the backendmetrics.PodMetrics interface.
func (m *mockPodMetrics) GetPod() *backend.Pod {
	return m.Pod
}

// GetEWMAMetrics implements the backendmetrics.PodMetrics interface, tracking access.
func (m *mockPodMetrics) GetEWMAMetrics() *backendmetrics.EWMAMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EWMAAccessCount++
	return m.EWMAMetrics
}

// GetMetrics implements the backendmetrics.PodMetrics interface, tracking access.
func (m *mockPodMetrics) GetMetrics() *backendmetrics.MetricsState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MetricsAccessCount++
	return m.MetricsState
}

// mockPodOptions allows customizing the mock pod creation.
type mockPodOptions struct {
	Lambda            float64
	MeanSojourn       float64
	VarSojourn        float64
	QueueSize         int
	Samples           int64
	LastSojournUpdate time.Time
}

// newMockPodMetrics creates a fully initialized mockPodMetrics for testing.
func newMockPodMetrics(name string, opts mockPodOptions) *mockPodMetrics {
	ewma := backendmetrics.NewEWMAMetrics()
	// Initialize arrival rate.
	if opts.Lambda > 0 {
		// We must initialize the timestamp for the decay calculation to work correctly.
		ewma.UpdateArrivalRateEWMA(time.Now())
		// Adjust RawEWMA to match the desired lambda for testing purposes (White-box setup).
		ewma.ArrivalRateRawEWMA = opts.Lambda * datalayer.ArrivalRateEWMAWindow.Seconds()
	}
	ewma.MeanSojournTimeEWMA = time.Duration(opts.MeanSojourn * float64(time.Second))
	ewma.VarianceSojournTimeEWMA = opts.VarSojourn

	// Set stabilization inputs (requires setters on EWMAMetrics for testing).
	ewma.SetSojournTimeSamples(opts.Samples)
	ewma.SetLastSojournUpdate(opts.LastSojournUpdate)

	return &mockPodMetrics{
		Pod: &backend.Pod{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		},
		EWMAMetrics: ewma,
		MetricsState: &backendmetrics.MetricsState{
			WaitingQueueSize: opts.QueueSize,
		},
	}
}

// Helper to create a standard mock pod (μ=10) easily.
func newStandardMockPod(name string, lambda float64, samples int64) *mockPodMetrics {
	return newMockPodMetrics(name, mockPodOptions{
		Lambda:      lambda,
		MeanSojourn: 0.1,
		VarSojourn:  0.01, // CV=1
		Samples:     samples,
	})
}

// --- Unit Tests: Mathematical Formulas ---

// TestCalculateCV validates the Coefficient of Variation helper function.
func TestCalculateCV(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mean       float64
		variance   float64
		expectedCV float64
	}{
		{
			name:       "standard_calculation/CV=1",
			mean:       10.0,
			variance:   100.0, // StdDev = 10
			expectedCV: 1.0,
		},
		{
			name:       "standard_calculation/CV<1",
			mean:       10.0,
			variance:   25.0, // StdDev = 5
			expectedCV: 0.5,
		},
		{
			name:       "standard_calculation/CV>1",
			mean:       5.0,
			variance:   100.0, // StdDev = 10
			expectedCV: 2.0,
		},
		{
			name:       "edge_case/zero_mean",
			mean:       0.0,
			variance:   10.0,
			expectedCV: 0.0,
		},
		{
			name:       "edge_case/tiny_mean",
			mean:       1e-10,
			variance:   10.0,
			expectedCV: 0.0,
		},
		{
			name:       "edge_case/zero_variance",
			mean:       10.0,
			variance:   0.0,
			expectedCV: 0.0,
		},
		{
			name:       "edge_case/negative_variance",
			mean:       10.0,
			variance:   -5.0,
			expectedCV: 0.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotCV := calculateCV(tc.mean, tc.variance)
			assert.InDelta(t, tc.expectedCV, gotCV, 0.00001,
				"calculateCV() should produce the expected Coefficient of Variation for mean=%v, variance=%v",
				tc.mean, tc.variance)
		})
	}
}

// TestCalculatePodDetails_MG1 validates the core M/G/1 calculations (Pollaczek-Khinchine formula).
func TestCalculatePodDetails_MG1(t *testing.T) {
	t.Parallel()

	const (
		E_S_100ms  = 100 * time.Millisecond
		Var_S_Zero = 0.0
	)

	tests := []struct {
		name     string
		input    rawInputs
		expected PodFullnessDetails
	}{
		{
			name: "M/M/1 equivalent (CV=1) at 80% utilization",
			// λ=8 req/s, E[S]=0.1s (μ=10 req/s). ρ=0.8. Var(S)=E[S]^2=0.01 for M/M/1.
			input: rawInputs{
				arrivalRate:                  8.0,
				meanEffectiveServiceTime:     E_S_100ms,
				varianceEffectiveServiceTime: 0.01,
				measuredQueueSize:            1,
			},
			// Expected W_q for M/M/1 = ρ*E[S]/(1-ρ) = 0.8*0.1/0.2 = 0.4s.
			// PMST = 0.1 + 0.4 = 0.5s. L_q = λ*W_q = 8*0.4 = 3.2.
			expected: PodFullnessDetails{
				Utilization:              0.8,
				PredictedCongestionDelay: 400 * time.Millisecond,
				PredictedMeanSojournTime: 500 * time.Millisecond,
				PredictedQueueLength:     3.2,
				CoefficientOfVariation:   1.0,
				QueueMomentum:            2.2, // 3.2 - 1
			},
		},
		{
			name: "High Variance (CV=2) at 80% utilization",
			// ρ=0.8. CV=2. Var(S)=0.04.
			input: rawInputs{
				arrivalRate:                  8.0,
				meanEffectiveServiceTime:     E_S_100ms,
				varianceEffectiveServiceTime: 0.04,
			},
			// P-K formula involves E[S^2] = Var(S)+E[S]^2 = 0.04+0.01 = 0.05.
			// W_q = (λ*E[S^2])/(2*(1-ρ)) = (8*0.05)/(2*0.2) = 0.4/0.4 = 1.0s.
			// PMST = 0.1 + 1.0 = 1.1s. L_q = 8*1.0 = 8.0.
			expected: PodFullnessDetails{
				Utilization:              0.8,
				PredictedCongestionDelay: 1000 * time.Millisecond,
				PredictedMeanSojournTime: 1100 * time.Millisecond,
				PredictedQueueLength:     8.0,
				CoefficientOfVariation:   2.0,
				QueueMomentum:            8.0,
			},
		},
		{
			name: "Low Variance (Deterministic, CV=0) at 80% utilization",
			// ρ=0.8, CV=0. Var(S)=0.
			input: rawInputs{
				arrivalRate:                  8.0,
				meanEffectiveServiceTime:     E_S_100ms,
				varianceEffectiveServiceTime: Var_S_Zero,
			},
			// E[S^2] = 0+0.01 = 0.01.
			// W_q = (8*0.01)/(2*0.2) = 0.08/0.4 = 0.2s. (Half of M/M/1 delay)
			expected: PodFullnessDetails{
				Utilization:              0.8,
				PredictedCongestionDelay: 200 * time.Millisecond,
				PredictedMeanSojournTime: 300 * time.Millisecond,
				PredictedQueueLength:     1.6,
				CoefficientOfVariation:   0.0,
				QueueMomentum:            1.6,
			},
		},
		{
			name: "Idle system (λ=0)",
			input: rawInputs{
				arrivalRate:                  0.0,
				meanEffectiveServiceTime:     E_S_100ms,
				varianceEffectiveServiceTime: 0.01,
			},
			// ρ=0. W_q=0. PMST=E[S].
			expected: PodFullnessDetails{
				Utilization:              0.0,
				PredictedCongestionDelay: 0,
				PredictedMeanSojournTime: E_S_100ms,
				PredictedQueueLength:     0.0,
				CoefficientOfVariation:   1.0,
				QueueMomentum:            0.0,
			},
		},
		{
			name: "Negative Queue Momentum (recovering system)",
			// λ=4 (ρ=0.4), E[S]=0.1, Var(S)=0.01 (CV=1). MeasuredQueue=10.
			input: rawInputs{
				arrivalRate:                  4.0,
				meanEffectiveServiceTime:     E_S_100ms,
				varianceEffectiveServiceTime: 0.01,
				measuredQueueSize:            10,
			},
			// W_q = 0.4*0.1/0.6 ≈ 0.0667s. L_q = 4 * 0.0667 ≈ 0.2667.
			expected: PodFullnessDetails{
				Utilization:              0.4,
				PredictedMeanSojournTime: 166666666 * time.Nanosecond, // ≈ 166.67ms
				PredictedCongestionDelay: 66666666 * time.Nanosecond,
				PredictedQueueLength:     0.2666666666666667,
				CoefficientOfVariation:   1.0,
				QueueMomentum:            -9.733333333333333, // 0.2667 - 10
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Detector{} // Stateless for this calculation
			got := d.calculatePodDetails(tc.input)
			if diff := cmp.Diff(tc.expected, got, approxDurationComparer, float64EqualityOpt); diff != "" {
				t.Errorf("calculatePodDetails() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCalculatePodDetails_EdgeCases validates handling of overload and invalid inputs.
func TestCalculatePodDetails_EdgeCases(t *testing.T) {
	t.Parallel()
	d := &Detector{}
	const E_S_100ms = 100 * time.Millisecond

	tests := []struct {
		name     string
		input    rawInputs
		expected PodFullnessDetails
	}{
		{
			name: "Overload (ρ > 1.0)",
			// λ=12, E[S]=0.1 (μ=10). ρ=1.2.
			input: rawInputs{
				arrivalRate:              12.0,
				meanEffectiveServiceTime: E_S_100ms,
			},
			expected: PodFullnessDetails{
				Utilization:              1.2,
				PredictedMeanSojournTime: overloadedLatency,
				IsOverloaded:             true,
			},
		},
		{
			name: "Saturation (ρ = 1.0)",
			// λ=10, E[S]=0.1 (μ=10). ρ=1.0.
			input: rawInputs{
				arrivalRate:              10.0,
				meanEffectiveServiceTime: E_S_100ms,
			},
			expected: PodFullnessDetails{
				Utilization:              1.0,
				PredictedMeanSojournTime: overloadedLatency,
				IsOverloaded:             true,
			},
		},
		{
			name: "Near Saturation (ρ ≈ 1.0, floating point safety)",
			// ρ = 1 - 1e-10. (1 - ρ) = 1e-10.
			input: rawInputs{
				// Use precise definition of the input utilization
				arrivalRate:              10.0 * (1.0 - 1e-10),
				meanEffectiveServiceTime: E_S_100ms,
			},
			// The internal check is (1-utilization) < 1e-9. Since 1e-10 < 1e-9, it should trigger overload handling.
			expected: PodFullnessDetails{
				Utilization:              1.0 - 1e-9, // Match the calculation 10.0 * (1.0 - 1e-10) * 0.1
				PredictedMeanSojournTime: overloadedLatency,
				IsOverloaded:             true,
			},
		},
		{
			name: "Cold Start (E[S]=0)",
			input: rawInputs{
				arrivalRate:              5.0,
				meanEffectiveServiceTime: 0,
			},
			expected: PodFullnessDetails{}, // Should be empty
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := d.calculatePodDetails(tc.input)
			// Use slightly looser tolerance for the utilization comparison near 1.0.
			if diff := cmp.Diff(tc.expected, got, approxDurationComparer, cmpopts.EquateApprox(1e-8, 0.0)); diff != "" {
				t.Errorf("calculatePodDetails() edge case mismatch for input=%+v (-want +got):\n%s", tc.input, diff)
			}
		})
	}
}

// --- Behavioral Tests: Bang-Bang Controller (Hysteresis) ---

// TestDetector_HysteresisBehavior validates the state transitions of the Bang-Bang controller.
func TestDetector_HysteresisBehavior(t *testing.T) {
	// We do not use t.Parallel() as this test manipulates the internal state of a single detector instance sequentially.

	const (
		HWM = 0.85 // High Watermark (TargetUtilization)
		LWM = 0.75 // Low Watermark (ResumeUtilization)
	)

	config := Config{
		TargetUtilization: HWM,
		ResumeUtilization: LWM,
		CachingTTL:        time.Hour, // Ensure caching doesn't interfere.
		WarmUpSampleCount: 1,         // Ensure metrics are reliable immediately.
	}
	// Use a real clock (nil) as time control is not needed for hysteresis behavior.
	d := NewDetector(config, nil, logr.Discard())
	ctx := context.Background()

	// Helper to create a pod pool resulting in a specific utilization.
	// We use 1 pod with μ=10 (E[S]=0.1s). We set λ such that λ/10 = utilization.
	createPool := func(utilization float64) []backendmetrics.PodMetrics {
		lambda := utilization * 10.0
		return []backendmetrics.PodMetrics{
			newStandardMockPod("P1", lambda, 10),
		}
	}

	tests := []struct {
		name              string
		utilization       float64
		expectSaturated   bool
		expectStateChange bool
	}{
		{"1_Start_Below_LWM", 0.5, false, false},
		{"2_Ramp_To_HWM_Boundary", HWM - 0.001, false, false},
		{"3_Cross_HWM_Engage", HWM + 0.001, true, true},
		{"4_Saturated_Ramp_Higher", 0.95, true, false},
		{"5_Saturated_Drop_To_LWM_Boundary", LWM + 0.001, true, false}, // Still in Hysteresis band
		{"6_Cross_LWM_Resume", LWM - 0.001, false, true},
		{"7_Resumed_Ramp_In_Band", 0.80, false, false},
	}

	// Track the internal state across iterations.
	previousInternalState := d.isSaturated.Load()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := createPool(tc.utilization)
			isSaturated := d.IsSaturated(ctx, pool)

			assert.Equal(t, tc.expectSaturated, isSaturated, "IsSaturated() output mismatch for utilization")

			// Verify the internal state matches the output.
			internalState := d.isSaturated.Load()
			assert.Equal(t, isSaturated, internalState, "Internal atomic state mismatch")

			// Verify if the state changed as expected.
			stateChanged := internalState != previousInternalState
			assert.Equal(t, tc.expectStateChange, stateChanged, "State transition (hysteresis) did not occur as expected")

			previousInternalState = internalState
		})
	}
}

// --- Behavioral Tests: Stabilization (Stateful Probing) ---

// TestDetector_StatefulProbing validates the deadlock prevention mechanism using a FakeClock.
func TestDetector_StatefulProbing(t *testing.T) {
	// We do not use t.Parallel() as this test relies on precise control over the FakeClock and sequential state updates.

	const (
		ProbeInt      = 500 * time.Millisecond
		StaleThr      = 30 * time.Second
		WarmUpSamples = 10
	)

	// Setup FakeClock for deterministic time control.
	startTime := time.Now()
	fakeClock := clocktesting.NewFakeClock(startTime)

	config := Config{
		TargetUtilization:      0.85,
		ResumeUtilization:      0.75,
		CachingTTL:             time.Microsecond, // Force cache refresh on every call for immediate metric updates.
		ProbeInterval:          ProbeInt,
		EWMAStalenessThreshold: StaleThr,
		WarmUpSampleCount:      WarmUpSamples,
	}
	d := NewDetector(config, fakeClock, logr.Discard())
	ctx := context.Background()

	// 1. Initial State (Cold Start - Unstable)
	// Pod has 0 samples.
	pod := newStandardMockPod("P1", 5.0, 0)
	pool := []backendmetrics.PodMetrics{pod}

	// First call should trigger a probe because metrics are unstable (0 samples < 10).
	isSaturated1 := d.IsSaturated(ctx, pool)
	require.False(t, isSaturated1, "Should force probe (return false) on initial cold start")

	// Verify probe time was updated.
	d.probeMu.Lock()
	lastProbe := d.lastProbeTime
	d.probeMu.Unlock()
	require.Equal(t, startTime, lastProbe, "Probe timestamp should be updated on forced probe")

	// 2. Subsequent calls (Still Unstable, Interval not elapsed)
	// Advance time slightly, but less than ProbeInterval.
	fakeClock.Step(ProbeInt / 2)
	isSaturated2 := d.IsSaturated(ctx, pool)
	require.True(t, isSaturated2, "Should block (return true) if unstable and probe interval has not elapsed")

	// 3. Interval Elapsed (Still Unstable)
	// Advance time past the interval.
	fakeClock.Step(ProbeInt/2 + time.Millisecond)
	isSaturated3 := d.IsSaturated(ctx, pool)
	require.False(t, isSaturated3, "Should force probe again when interval elapses while still unstable")

	// 4. Transition to Stable but Stale
	// Update pod metrics to be stable (>= 10 samples), but set the update time far in the past relative to the clock.
	pod.EWMAMetrics.SetSojournTimeSamples(WarmUpSamples)
	// Set the last update time significantly before the current fakeClock time.
	pod.EWMAMetrics.SetLastSojournUpdate(fakeClock.Now().Add(-StaleThr * 2))

	// Advance time past the next interval.
	fakeClock.Step(ProbeInt + time.Millisecond)
	isSaturated4 := d.IsSaturated(ctx, pool)
	require.False(t, isSaturated4, "Should force probe if metrics are stale")

	// 5. Subsequent call (Stale, Interval not elapsed)
	fakeClock.Step(ProbeInt / 2)
	isSaturated5 := d.IsSaturated(ctx, pool)
	require.True(t, isSaturated5, "Should block if stale and probe interval has not elapsed")

	// 6. Transition to Reliable (Stable and Fresh)
	// Update metrics to be fresh relative to the clock.
	pod.EWMAMetrics.SetLastSojournUpdate(fakeClock.Now())

	// The system should now revert to Bang-Bang control. Utilization is 0.5 (5.0/10.0), so it should not block.
	isSaturated6 := d.IsSaturated(ctx, pool)
	require.False(t, isSaturated6, "Should use Bang-Bang control (return false) when metrics are reliable and utilization is low")

	// 7. Reliable and Saturated
	// Increase load to force saturation.
	pod = newStandardMockPod("P1", 9.5, WarmUpSamples) // Util=0.95
	pod.EWMAMetrics.SetLastSojournUpdate(fakeClock.Now())
	pool = []backendmetrics.PodMetrics{pod}

	isSaturated7 := d.IsSaturated(ctx, pool)
	require.True(t, isSaturated7, "Should use Bang-Bang control (return true) when metrics are reliable and utilization is high")
}

// --- Concurrency and Caching Tests ---

// TestDetector_CachingBehavior validates the internal TTL cache mechanism using a FakeClock.
func TestDetector_CachingBehavior(t *testing.T) {
	// We do not use t.Parallel() here because we require precise control over the FakeClock.

	const cacheTTL = 50 * time.Millisecond
	startTime := time.Now()
	fakeClock := clocktesting.NewFakeClock(startTime)

	config := Config{
		CachingTTL:        cacheTTL,
		WarmUpSampleCount: 1, // Ensure reliability doesn't interfere.
	}
	d := NewDetector(config, fakeClock, logr.Discard())

	// Initialize mocks.
	pod1 := newStandardMockPod("Pod1", 1.0, 10) // Util=0.1
	pod1.MetricsState.WaitingQueueSize = 5
	pods := []backendmetrics.PodMetrics{pod1}
	ctx := context.Background()

	// 1. Initial Call (Cache Miss)
	report1 := d.GetFullnessReport(ctx, pods)
	require.NotEmpty(t, report1.PerPodDetails, "Report should not be empty on initial call")
	assert.Equal(t, 1, pod1.EWMAAccessCount, "Pod1 metrics should be accessed once on cache miss")

	// Verify data is correctly cached (Eager Calculation).
	d.mu.RLock()
	cached1, ok1 := d.cache["default/Pod1"]
	d.mu.RUnlock()
	require.True(t, ok1, "Pod1 must be cached after the initial fetch")
	assert.InDelta(t, 0.1, cached1.Details.Utilization, 1e-5, "Pod1 utilization should be eagerly calculated and cached")
	assert.Equal(t, startTime, cached1.timestamp, "Cache timestamp should match the clock time")

	// 2. Subsequent Call (Cache Hit)
	// Update the underlying metrics to verify cache is working.
	pod1.EWMAMetrics.ArrivalRateRawEWMA = 99.0 * datalayer.ArrivalRateEWMAWindow.Seconds() // Force high utilization
	pod1.MetricsState.WaitingQueueSize = 99

	fakeClock.Step(cacheTTL / 2) // Advance time, but stay within TTL.

	report2 := d.GetFullnessReport(ctx, pods)
	assert.Equal(t, 1, pod1.EWMAAccessCount, "Pod1 metrics should NOT be accessed again on cache hit")

	// Verify the report reflects the original (cached) data.
	details2 := report2.PerPodDetails["default/Pod1"]
	assert.InDelta(t, 0.1, details2.Utilization, 1e-5, "Report should reflect cached utilization (Temporal Consistency)")

	// 3. Call after TTL Expiry (Cache Stale)
	fakeClock.Step(cacheTTL/2 + time.Millisecond) // Advance time past TTL.

	report3 := d.GetFullnessReport(ctx, pods)
	assert.Equal(t, 2, pod1.EWMAAccessCount, "Pod1 metrics should be accessed again after TTL expiry")

	// Verify the report reflects the new underlying data.
	details3 := report3.PerPodDetails["default/Pod1"]
	assert.Greater(t, details3.Utilization, 5.0, "Utilization after refresh should reflect the updated metrics")
}

// TestDetector_ConcurrentAccess validates thread safety using the race detector.
func TestDetector_ConcurrentAccess(t *testing.T) {
	// This test relies on being run with `go test -race`.
	t.Parallel()

	// Use a real clock (nil) here as we want to test real-world contention.
	d := NewDetector(Config{
		TargetUtilization: 0.85,
		ResumeUtilization: 0.75,
		CachingTTL:        5 * time.Millisecond, // Short TTL to force frequent cache updates (WLock contention).
		ProbeInterval:     10 * time.Millisecond, // Frequent probing interval.
	}, nil, logr.Discard())

	// Create a diverse set of pods to maximize contention on different cache keys.
	pods := make([]backendmetrics.PodMetrics, 20)
	for i := range pods {
		pods[i] = newStandardMockPod(t.Name()+strconv.Itoa(i), float64(i%5)+1, 10)
	}

	const (
		numGoroutines = 100
		numIterations = 50
	)
	wg := sync.WaitGroup{}
	ctx := context.Background()

	// Hammer the detector concurrently.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(routineID int) {
			defer wg.Done()
			for j := 0; j < numIterations; j++ {
				// Alternate between IsSaturated (uses probeMu and Cache RLock/WLock) and GetFullnessReport.
				if routineID%2 == 0 {
					_ = d.IsSaturated(ctx, pods)
				} else {
					_ = d.GetFullnessReport(ctx, pods)
				}
				// Small sleep to yield scheduler and increase contention likelihood.
				if j%10 == 0 {
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(i)
	}

	wg.Wait()

	// Assert: If the test completes without race detector failures, it passes.
}

// --- Benchmarks ---

// BenchmarkDetector measures the performance overhead on the hot path.
func BenchmarkDetector(b *testing.B) {
	// Create detectors with different caching strategies.
	cfgNoCache := Config{CachingTTL: 1 * time.Nanosecond, WarmUpSampleCount: 1}
	cfgWithCache := Config{CachingTTL: time.Hour, WarmUpSampleCount: 1}

	// Use real clock (nil) for benchmarks.
	detectorNoCache := NewDetector(cfgNoCache, nil, logr.Discard())
	detectorWithCache := NewDetector(cfgWithCache, nil, logr.Discard())

	// Create mock pod data.
	podCounts := []int{1, 10, 100}
	for _, count := range podCounts {
		pods := make([]backendmetrics.PodMetrics, count)
		for i := 0; i < count; i++ {
			pods[i] = newMockPodMetrics(
				"pod-"+strconv.Itoa(i),
				mockPodOptions{Lambda: 5.0, MeanSojourn: 0.1, VarSojourn: 0.01, QueueSize: 2, Samples: 10},
			)
		}

		ctx := context.Background()

		// Benchmark: GetFullnessReport (No Cache / Cache Miss)
		b.Run(fmt.Sprintf("GetFullnessReport/Pods=%d/CacheMiss", count), func(b *testing.B) {
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				// By using the NoCache detector, we force a refresh (fetch + calculation) every time.
				_ = detectorNoCache.GetFullnessReport(ctx, pods)
			}
		})

		// Benchmark: GetFullnessReport (Cache Hit)
		b.Run(fmt.Sprintf("GetFullnessReport/Pods=%d/CacheHit", count), func(b *testing.B) {
			// Prime the cache once before the benchmark loop.
			_ = detectorWithCache.GetFullnessReport(ctx, pods)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				// Measures the overhead of the RLock and aggregation logic.
				_ = detectorWithCache.GetFullnessReport(ctx, pods)
			}
		})

		// Benchmark: IsSaturated (Cache Hit)
		b.Run(fmt.Sprintf("IsSaturated/Pods=%d/CacheHit", count), func(b *testing.B) {
			// Prime the cache.
			_ = detectorWithCache.GetFullnessReport(ctx, pods)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				// Measures the overhead of GetFullnessReport (Cache Hit) + stabilization/control logic.
				_ = detectorWithCache.IsSaturated(ctx, pods)
			}
		})
	}
}
