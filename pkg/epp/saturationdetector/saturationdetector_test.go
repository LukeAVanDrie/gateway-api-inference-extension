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

// newMockPodMetrics creates a fully initialized mockPodMetrics for testing.
// meanSojourn and varSojourn are expected in seconds (or seconds^2 for variance).
func newMockPodMetrics(name string, lambda float64, meanSojourn, varSojourn float64, queueSize int) *mockPodMetrics {
	ewma := backendmetrics.NewEWMAMetrics()
	ewma.ArrivalRateEWMA = lambda
	ewma.MeanSojournTimeEWMA = time.Duration(meanSojourn * float64(time.Second))
	ewma.VarianceSojournTimeEWMA = varSojourn

	return &mockPodMetrics{
		Pod: &backend.Pod{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		},
		EWMAMetrics: ewma,
		MetricsState: &backendmetrics.MetricsState{
			WaitingQueueSize: queueSize,
		},
	}
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

// TestCalculatePControllerOutput validates the P-controller logic (Error Signal and Dispatch Probability).
func TestCalculatePControllerOutput(t *testing.T) {
	t.Parallel()

	const (
		Target80 = 0.8
		Kp_5     = 5.0
		Kp_10    = 10.0
	)

	tests := []struct {
		name               string
		config             Config
		currentUtilization float64
		expected           PControllerInternals
	}{
		{
			name:               "Well below target (Kp=10)",
			config:             Config{TargetUtilization: Target80, ProportionalGain: Kp_10},
			currentUtilization: 0.5,
			// Error = 0.8 - 0.5 = 0.3. Prob = 10 * 0.3 = 3.0 (clamped to 1.0).
			expected: PControllerInternals{
				TargetUtilization:   Target80,
				CurrentUtilization:  0.5,
				ErrorSignal:         0.3,
				DispatchProbability: 1.0,
			},
		},
		{
			name:               "Slightly below target (Kp=10)",
			config:             Config{TargetUtilization: Target80, ProportionalGain: Kp_10},
			currentUtilization: 0.75,
			// Error = 0.8 - 0.75 = 0.05. Prob = 10 * 0.05 = 0.5.
			expected: PControllerInternals{
				TargetUtilization:   Target80,
				CurrentUtilization:  0.75,
				ErrorSignal:         0.05,
				DispatchProbability: 0.5,
			},
		},
		{
			name:               "At target (Kp=10)",
			config:             Config{TargetUtilization: Target80, ProportionalGain: Kp_10},
			currentUtilization: 0.8,
			// Error = 0. Prob = 0.
			expected: PControllerInternals{
				TargetUtilization:   Target80,
				CurrentUtilization:  0.8,
				ErrorSignal:         0.0,
				DispatchProbability: 0.0,
			},
		},
		{
			name:               "Above target (Kp=10)",
			config:             Config{TargetUtilization: Target80, ProportionalGain: Kp_10},
			currentUtilization: 0.9,
			// Error = -0.1. Prob = -1.0 (clamped to 0.0).
			expected: PControllerInternals{
				TargetUtilization:   Target80,
				CurrentUtilization:  0.9,
				ErrorSignal:         -0.1,
				DispatchProbability: 0.0,
			},
		},
		{
			name:               "Lower Kp (Kp=5), slightly below target",
			config:             Config{TargetUtilization: Target80, ProportionalGain: Kp_5},
			currentUtilization: 0.75,
			// Error = 0.05. Prob = 5 * 0.05 = 0.25. (Less aggressive response)
			expected: PControllerInternals{
				TargetUtilization:   Target80,
				CurrentUtilization:  0.75,
				ErrorSignal:         0.05,
				DispatchProbability: 0.25,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewDetector(tc.config, logr.Discard())
			got := d.calculatePControllerOutput(tc.currentUtilization)
			if diff := cmp.Diff(tc.expected, got, float64EqualityOpt); diff != "" {
				t.Errorf("calculatePControllerOutput() mismatch for currentUtilization=%v (-want +got):\n%s",
					tc.currentUtilization, diff)
			}
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
		input    cachedPodMetrics
		expected PodFullnessDetails
	}{
		{
			name: "M/M/1 equivalent (CV=1) at 80% utilization",
			// λ=8 req/s, E[S]=0.1s (μ=10 req/s). ρ=0.8. Var(S)=E[S]^2=0.01 for M/M/1.
			input: cachedPodMetrics{
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
			name: "High Variance (CV=2) at 80% utilization (LLM-like)",
			// Same ρ=0.8, but CV=2. Var(S) = (CV*E[S])^2 = (2*0.1)^2 = 0.04.
			input: cachedPodMetrics{
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
			input: cachedPodMetrics{
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
			input: cachedPodMetrics{
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
			input: cachedPodMetrics{
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
			d := &Detector{} // Detector instance is stateless for this calculation
			got := d.calculatePodDetails(tc.input)
			if diff := cmp.Diff(tc.expected, got, approxDurationComparer, float64EqualityOpt); diff != "" {
				t.Errorf("calculatePodDetails() mismatch for input=%+v (-want +got):\n%s", tc.input, diff)
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
		input    cachedPodMetrics
		expected PodFullnessDetails
	}{
		{
			name: "Overload (ρ > 1.0)",
			// λ=12, E[S]=0.1 (μ=10). ρ=1.2.
			input: cachedPodMetrics{
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
			input: cachedPodMetrics{
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
			input: cachedPodMetrics{
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
			input: cachedPodMetrics{
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
			if diff := cmp.Diff(tc.expected, got, approxDurationComparer, cmpopts.EquateApprox(1e-8, 0.0)); diff != "" {
				t.Errorf("calculatePodDetails() edge case mismatch for input=%+v (-want +got):\n%s", tc.input, diff)
			}
		})
	}
}

// --- Unit Tests: Aggregation (GetFullnessReport) ---

// TestGetFullnessReport_AggregateUtilization validates the aggregate utilization calculation (Σλᵢ / Σμᵢ).
func TestGetFullnessReport_AggregateUtilization(t *testing.T) {
	t.Parallel()

	// Standard pod definitions based on E[S] (Effective Service Time):
	// PodA: μ=10 req/s (E[S]=0.1s)
	// PodB: μ=5 req/s (E[S]=0.2s)
	// PodC: μ=20 req/s (E[S]=0.05s)

	tests := []struct {
		name        string
		pods        []backendmetrics.PodMetrics
		expectedRho float64
	}{
		{
			name: "Homogeneous Pool",
			// 3x PodA. Total μ = 30. λ=5+5+5=15. ρ = 15/30 = 0.5.
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("A1", 5.0, 0.1, 0.01, 0),
				newMockPodMetrics("A2", 5.0, 0.1, 0.01, 0),
				newMockPodMetrics("A3", 5.0, 0.1, 0.01, 0),
			},
			expectedRho: 0.5,
		},
		{
			name: "Heterogeneous Pool (Capacity Weighting)",
			// PodA (μ=10), PodB (μ=5), PodC (μ=20). Total μ = 35.
			// λ=8+2+10=20. ρ = 20/35 ≈ 0.5714.
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("A1", 8.0, 0.1, 0.01, 0),
				newMockPodMetrics("B1", 2.0, 0.2, 0.04, 0),
				newMockPodMetrics("C1", 10.0, 0.05, 0.0025, 0),
			},
			expectedRho: 20.0 / 35.0,
		},
		{
			name: "Mixed State (One Overloaded)",
			// P1 (μ=10, λ=5, ρ=0.5), P2 (μ=10, λ=15, ρ=1.5)
			// Σλ = 20. Σμ = 20. ρ = 1.0. The aggregate utilization is correct even if individual pods are overloaded.
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("P1", 5.0, 0.1, 0.01, 0),
				newMockPodMetrics("P2_over", 15.0, 0.1, 0.01, 0),
			},
			expectedRho: 1.0,
		},
		{
			name: "Pool with Cold/Unresponsive Pod (μ=0)",
			// PodA (μ=10), PodB (μ=5), ColdPod (μ=0). Total μ = 15.
			// λ=5+5+5=15. ρ = 15/15 = 1.0.
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("A1", 5.0, 0.1, 0.01, 0),
				newMockPodMetrics("B1", 5.0, 0.2, 0.04, 0),
				newMockPodMetrics("Cold1", 5.0, 0.0, 0.0, 0), // E[S]=0
			},
			expectedRho: 1.0,
		},
		{
			name: "Pool with Missing Metrics",
			// PodA (μ=10), Missing (μ=0). Total μ = 10. λ=5. ρ = 5/10 = 0.5.
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("A1", 5.0, 0.1, 0.01, 0),
				// Mock with no EWMAMetrics initialized.
				&mockPodMetrics{Pod: &backend.Pod{NamespacedName: types.NamespacedName{Name: "Missing", Namespace: "default"}}},
			},
			expectedRho: 0.5,
		},
		{
			name:        "Empty Pool",
			pods:        []backendmetrics.PodMetrics{},
			expectedRho: 0.0,
		},
		{
			name: "Zero Capacity Pool, No Arrivals",
			// Total μ=0. λ=0. ρ=0.
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("Cold1", 0.0, 0.0, 0.0, 0),
			},
			expectedRho: 0.0,
		},
		{
			name: "Zero Capacity Pool, With Arrivals (Sentinel Value)",
			// Total μ=0. λ=5. ρ = sentinel 1.5.
			pods: []backendmetrics.PodMetrics{
				newMockPodMetrics("Cold1", 5.0, 0.0, 0.0, 0),
			},
			expectedRho: 1.5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Config values (TargetUtilization, Kp, TTL) do not affect the utilization calculation itself.
			d := NewDetector(Config{}, logr.Discard())
			report := d.GetFullnessReport(context.Background(), tc.pods)
			assert.InDelta(t, tc.expectedRho, report.SubsetUtilization, 1e-9,
				"GetFullnessReport() Aggregate Utilization (ρ_subset) mismatch for pods=%v",
				tc.pods)
		})
	}
}

// --- Behavioral Tests: P-Controller Application (IsSaturated) ---

// TestIsSaturated_ProbabilisticBehavior validates the statistical behavior of the P-controller's probabilistic output.
func TestIsSaturated_ProbabilisticBehavior(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		targetUtil      float64
		kp              float64
		actualUtil      float64
		expectedSatRate float64 // Expected saturation rate (1 - DispatchProbability)
	}{
		// Kp=10, Target=0.8
		{
			name:            "P=1.0 (Util=0.5)",
			targetUtil:      0.8,
			kp:              10.0,
			actualUtil:      0.5,
			expectedSatRate: 0.0, // Error=0.3, Prob=3.0 (clamped 1.0)
		},
		{
			name:            "P=0.5 (Util=0.75)",
			targetUtil:      0.8,
			kp:              10.0,
			actualUtil:      0.75,
			expectedSatRate: 0.5, // Error=0.05, Prob=0.5
		},
		{
			name:            "P=0.0 (Util=0.8)",
			targetUtil:      0.8,
			kp:              10.0,
			actualUtil:      0.8,
			expectedSatRate: 1.0, // Error=0.0, Prob=0.0
		},
		{
			name:            "P=0.0 (Util=0.9)",
			targetUtil:      0.8,
			kp:              10.0,
			actualUtil:      0.9,
			expectedSatRate: 1.0, // Error=-0.1, Prob=-1.0 (clamped 0.0)
		},
	}

	const (
		iterations = 5000
		tolerance  = 0.05 // Allow a 5% margin of error for statistical validation.
	)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := Config{
				TargetUtilization: tc.targetUtil,
				ProportionalGain:  tc.kp,
				CachingTTL:        time.Hour, // Ensure caching doesn't interfere.
			}
			d := NewDetector(config, logr.Discard())

			// Create a mock scenario that results in the desired utilization.
			// We use 1 pod with μ=10 (E[S]=0.1s). We set λ such that λ/10 = actualUtil.
			lambda := tc.actualUtil * 10.0
			pods := []backendmetrics.PodMetrics{
				newMockPodMetrics("P1", lambda, 0.1, 0.01, 0),
			}

			saturatedCount := 0
			for range iterations {
				if d.IsSaturated(context.Background(), pods) {
					saturatedCount++
				}
			}

			observedSatRate := float64(saturatedCount) / float64(iterations)

			// For deterministic cases (0.0 or 1.0), we expect an exact match.
			if tc.expectedSatRate == 0.0 || tc.expectedSatRate == 1.0 {
				assert.Equal(t, tc.expectedSatRate, observedSatRate, "Expected deterministic outcome (P=0.0 or P=1.0)")
			} else {
				assert.InDelta(t, tc.expectedSatRate, observedSatRate, tolerance,
					"Statistical validation failed: observed saturation rate outside expected margin")
			}
		})
	}
}

// --- Concurrency and Caching Tests ---

// TestDetector_CachingBehavior validates the internal TTL cache mechanism and temporal consistency.
func TestDetector_CachingBehavior(t *testing.T) {
	// We cannot use t.Parallel() here because we are testing time-sensitive behavior (TTL expiry) on shared state.

	const cacheTTL = 50 * time.Millisecond
	config := Config{CachingTTL: cacheTTL}
	d := NewDetector(config, logr.Discard())

	// Initialize mocks with specific values and access counters (starting at 0).
	pod1 := newMockPodMetrics("Pod1", 1.0, 0.1, 0.01, 5)
	pod2 := newMockPodMetrics("Pod2", 2.0, 0.2, 0.04, 10)
	pods := []backendmetrics.PodMetrics{pod1, pod2}

	ctx := context.Background()

	// 1. Initial Call (Cache Miss)
	report1 := d.GetFullnessReport(ctx, pods)
	require.NotEmpty(t, report1.PerPodDetails, "Report should not be empty on initial call")
	assert.Equal(t, 1, pod1.EWMAAccessCount, "Pod1 EWMA metrics should be accessed once on cache miss")
	assert.Equal(t, 1, pod1.MetricsAccessCount, "Pod1 physical metrics should be accessed once on cache miss")
	assert.Equal(t, 1, pod2.EWMAAccessCount, "Pod2 EWMA metrics should be accessed once on cache miss")

	// Verify data is correctly cached (Temporal Consistency Check)
	d.mu.RLock()
	cached1, ok1 := d.cache["default/Pod1"]
	d.mu.RUnlock()
	require.True(t, ok1, "Pod1 should be in the cache")
	assert.Equal(t, 5, cached1.measuredQueueSize, "Pod1 measuredQueueSize should be cached correctly")

	// 2. Subsequent Call (Cache Hit)
	// Update the underlying metrics to ensure the cached values are returned if the cache works.
	pod1.EWMAMetrics.ArrivalRateEWMA = 99.0
	pod1.MetricsState.WaitingQueueSize = 99

	report2 := d.GetFullnessReport(ctx, pods)
	assert.Equal(t, 1, pod1.EWMAAccessCount, "Pod1 EWMA metrics should NOT be accessed again on cache hit")
	assert.Equal(t, 1, pod1.MetricsAccessCount, "Pod1 physical metrics should NOT be accessed again on cache hit")

	// Verify the report reflects the original (cached) data, not the updated underlying data.
	details1 := report2.PerPodDetails["default/Pod1"]
	// Original ρ = 1.0 * 0.1 = 0.1
	assert.InDelta(t, 0.1, details1.Utilization, 1e-9, "Report should reflect cached utilization")
	// Original momentum calculation depends on original L_q and original queue size (5).
	assert.Greater(t, details1.QueueMomentum, -10.0,
		"Report should reflect cached measuredQueueSize (Temporal Consistency)")

	// 3. Call after TTL Expiry (Cache Stale)
	// We use time.Sleep here to test the actual time-based expiry.
	time.Sleep(cacheTTL + 5*time.Millisecond)

	report3 := d.GetFullnessReport(ctx, pods)
	assert.Equal(t, 2, pod1.EWMAAccessCount, "Pod1 EWMA metrics should be accessed again after TTL expiry")
	assert.Equal(t, 2, pod1.MetricsAccessCount, "Pod1 physical metrics should be accessed again after TTL expiry")

	// Verify the report reflects the new underlying data.
	details3 := report3.PerPodDetails["default/Pod1"]
	// New ρ = 99.0 * 0.1 = 9.9
	assert.InDelta(t, 9.9, details3.Utilization, 1e-9, "Report should reflect new utilization after refresh")
}

// TestDetector_Caching_MixedPool validates batch updates when some pods hit and others miss the cache.
func TestDetector_Caching_MixedPool(t *testing.T) {
	// We cannot use t.Parallel() here as we manipulate the internal state (cache timestamps) non-atomically for testing.

	const cacheTTL = 100 * time.Millisecond
	d := NewDetector(Config{CachingTTL: cacheTTL}, logr.Discard())
	ctx := context.Background()

	podA := newMockPodMetrics("PodA", 1.0, 0.1, 0.01, 0)
	podB := newMockPodMetrics("PodB", 1.0, 0.1, 0.01, 0)
	podC := newMockPodMetrics("PodC", 1.0, 0.1, 0.01, 0)

	// Prime the cache for A and B.
	d.GetFullnessReport(ctx, []backendmetrics.PodMetrics{podA, podB})
	require.Equal(t, 1, podA.EWMAAccessCount, "Initial access count for PodA should be 1")
	require.Equal(t, 1, podB.EWMAAccessCount, "Initial access count for PodB should be 1")

	// Manually adjust timestamp of A to be just outside the TTL window (making it stale).
	// This is a white-box technique used here to avoid relying on time.Sleep for deterministic testing of staleness.
	d.mu.Lock()
	if cachedA, ok := d.cache["default/PodA"]; ok {
		cachedA.timestamp = time.Now().Add(-cacheTTL - time.Millisecond)
		d.cache["default/PodA"] = cachedA
	}
	d.mu.Unlock()

	// Request report for A (Stale), B (Hit), C (Miss).
	d.GetFullnessReport(ctx, []backendmetrics.PodMetrics{podA, podB, podC})

	assert.Equal(t, 2, podA.EWMAAccessCount, "PodA (Stale) should be refreshed")
	assert.Equal(t, 1, podB.EWMAAccessCount, "PodB (Hit) should NOT be refreshed")
	assert.Equal(t, 1, podC.EWMAAccessCount, "PodC (Miss) should be fetched and cached")

	d.mu.RLock()
	_, okC := d.cache["default/PodC"]
	d.mu.RUnlock()
	assert.True(t, okC, "PodC should be added to cache")
}

// TestDetector_HandlingInvalidInputs validates robustness against nil inputs.
func TestDetector_HandlingInvalidInputs(t *testing.T) {
	t.Parallel()

	d := NewDetector(Config{CachingTTL: time.Hour}, logr.Discard())
	ctx := context.Background()

	validPod := newMockPodMetrics("Valid", 1.0, 0.1, 0.01, 0)
	nilPodStruct := &mockPodMetrics{Pod: nil} // Invalid: GetPod() returns nil
	nilEWMAMetrics := newMockPodMetrics("NoEWMA", 0, 0, 0, 0)
	nilEWMAMetrics.EWMAMetrics = nil // Invalid: GetEWMAMetrics() returns nil

	pods := []backendmetrics.PodMetrics{
		validPod,
		nilPodStruct,
		nilEWMAMetrics,
	}

	report := d.GetFullnessReport(ctx, pods)

	assert.Len(t, report.PerPodDetails, 1, "Report should only contain details for the valid pod")
	_, okValid := report.PerPodDetails["default/Valid"]
	assert.True(t, okValid, "Valid pod details should be present")

	// Verify utilization calculation correctly excludes invalid pods.
	// ValidPod: μ=10, λ=1. Total μ=10, Total λ=1. ρ = 1/10 = 0.1.
	assert.InDelta(t, 0.1, report.SubsetUtilization, 1e-9, "Utilization should only factor in valid pods")

	// Verify invalid pods were handled correctly during access.
	assert.Equal(t, 0, nilPodStruct.EWMAAccessCount,
		"Nil pod struct should not have metrics accessed (checked in Phase 1)")
	// The pod with nil EWMA metrics will be accessed once during the refresh phase (Phase 2) before being discarded.
	assert.Equal(t, 1, nilEWMAMetrics.EWMAAccessCount,
		"Pod with nil EWMA should be accessed once during refresh attempt")

	d.mu.RLock()
	_, okNoEWMA := d.cache["default/NoEWMA"]
	d.mu.RUnlock()
	assert.False(t, okNoEWMA, "Pod with nil EWMA should not be cached")
}

// TestDetector_ConcurrentAccess validates thread safety using the race detector.
func TestDetector_ConcurrentAccess(t *testing.T) {
	// This test relies on being run with `go test -race`.
	t.Parallel()

	d := NewDetector(Config{
		TargetUtilization: 0.8,
		ProportionalGain:  10.0,
		CachingTTL:        5 * time.Millisecond, // Use a short TTL to force frequent cache updates (WLock contention).
	}, logr.Discard())

	// Create a diverse set of pods to maximize contention on different cache keys.
	pods := make([]backendmetrics.PodMetrics, 20)
	for i := range pods {
		// Use t.Name() to ensure unique keys if tests run in parallel.
		pods[i] = newMockPodMetrics(t.Name()+strconv.Itoa(i), float64(i%5)+1, 0.1, 0.01, 0)
	}

	const (
		numGoroutines = 100
		numIterations = 50
	)
	wg := sync.WaitGroup{}
	ctx := context.Background()

	// Hammer the detector concurrently.
	for i := range numGoroutines {
		wg.Add(1)
		go func(routineID int) {
			defer wg.Done()
			for j := range numIterations {
				// Alternate between IsSaturated (uses RNG lock and Cache RLock/WLock) and GetFullnessReport (uses Cache
				// RLock/WLock).
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
	detectorNoCache := NewDetector(Config{CachingTTL: 1 * time.Nanosecond}, logr.Discard()) // Effectively disables cache.
	detectorWithCache := NewDetector(Config{CachingTTL: time.Hour}, logr.Discard())

	// Create mock pod data.
	podCounts := []int{1, 10, 100}
	for _, count := range podCounts {
		pods := make([]backendmetrics.PodMetrics, count)
		for i := range count {
			// Initialize pods with realistic-looking data.
			pods[i] = newMockPodMetrics(
				"pod-"+strconv.Itoa(i),
				5.0,  // λ=5
				0.1,  // E[S]=0.1s
				0.01, // Var(S)=0.01
				2,    // Measured Queue
			)
		}

		ctx := context.Background()

		// Benchmark: GetFullnessReport (No Cache / Cache Miss)
		b.Run(fmt.Sprintf("GetFullnessReport/Pods=%d/CacheMiss", count), func(b *testing.B) {
			b.ResetTimer()
			for b.Loop() {
				// By using the NoCache detector, we force a refresh every time.
				_ = detectorNoCache.GetFullnessReport(ctx, pods)
			}
		})

		// Benchmark: GetFullnessReport (Cache Hit)
		b.Run(fmt.Sprintf("GetFullnessReport/Pods=%d/CacheHit", count), func(b *testing.B) {
			// Prime the cache once before the benchmark loop,
			_ = detectorWithCache.GetFullnessReport(ctx, pods)
			b.ResetTimer()
			for b.Loop() {
				_ = detectorWithCache.GetFullnessReport(ctx, pods)
			}
		})

		// Benchmark: IsSaturated (Cache Hit)
		// This includes the overhead of the RNG locking.
		b.Run(fmt.Sprintf("IsSaturated/Pods=%d/CacheHit", count), func(b *testing.B) {
			// Prime the cache.
			_ = detectorWithCache.GetFullnessReport(ctx, pods)
			b.ResetTimer()
			for b.Loop() {
				_ = detectorWithCache.IsSaturated(ctx, pods)
			}
		})
	}
}
