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

package framework

import (
	"fmt"
	"strings"
	"time"
)

// Scorecard aggregates the final performance grades of a simulation scenario.
// It is hierarchically organized to verify the functional requirements of the specific lifecycle phases (Startup vs.
// Steady State).
type Scorecard struct {
	// Latency tracks the end-to-end response time statistics (P50/P99/Max).
	// Used for: Verifying SLO compliance (e.g., "99% of requests < 200ms queue wait").
	Latency LatencyMetrics

	// Startup assesses the "Cold Start" performance.
	// Used for: Verifying the Probing Regime, Hill-Climbing logic, and Time-to-Quorum.
	Startup StartupMetrics

	// Stability tracks the consistency of the Control Regime.
	// Used for: Verifying Lifecycle logic (Dormancy timeouts, Quorum thresholds).
	Stability RegimeStabilityMetrics

	// Control assesses the PID Loop during its Longest Continuous Active Window (Regulating Regime).
	// Used for: Tuning Gains (Kp), Alphas, and Headroom.
	// NOTE: These metrics are computed on a sliced timeline starting from the transition to Regulating.
	Control ControlMetrics

	// Safety assesses the system's ability to prevent catastrophic failure.
	// Used for: Validating Filters, Windows, and Fail-Open logic.
	Safety SafetyMetrics

	// Efficiency assesses resource utilization and orchestration intelligence.
	// Used for: Validating Adaptive Scorer, Probe Picker, and Bin-Packing.
	Efficiency EfficiencyMetrics

	// Accuracy assesses the fidelity of the Internal Physics Model.
	// Used for: Validating the "Digital Twin" claim (Estimator convergence).
	Accuracy EstimatorMetrics
}

// String produces a human-readable report of the simulation results.
func (s Scorecard) String() string {
	var sb strings.Builder

	sb.WriteString("\n====== [ Simulation Scorecard ] ======\n")

	// 1. Latency
	sb.WriteString("\n--- 📡 Latency (SLO) ---\n")
	fmt.Fprintf(&sb, "  P50:                %v\n", s.Latency.P50)
	fmt.Fprintf(&sb, "  P90:                %v\n", s.Latency.P90)
	fmt.Fprintf(&sb, "  P99:                %v\n", s.Latency.P99)
	fmt.Fprintf(&sb, "  Max:                %v\n", s.Latency.Max)
	fmt.Fprintf(&sb, "  Mean:               %v\n", s.Latency.MeanDispatchWait+s.Latency.MeanBackendService)
	fmt.Fprintf(&sb, "  Mean Dispatch Wait: %v (Flow Control Buffer)\n", s.Latency.MeanDispatchWait)
	fmt.Fprintf(&sb, "  Mean Service Time:  %v (Backend Queue+Prefill+Decode)\n", s.Latency.MeanBackendService)

	// 2. Startup
	sb.WriteString("\n--- 🚀 Startup & Discovery ---\n")
	if s.Startup.TimeToRegulation > 0 {
		fmt.Fprintf(&sb, "  Time to Regulation: %v\n", s.Startup.TimeToRegulation)
	} else {
		fmt.Fprintf(&sb, "  Time to Regulation: N/A (Never reached Quorum)\n")
	}
	fmt.Fprintf(&sb, "  Peak Discovery:     %.1f Concurrent Requests (L_peak)\n", s.Startup.PeakDiscoveryConcurrency)

	// 3. Stability
	sb.WriteString("\n--- 🏗️ Regime Stability ---\n")
	fmt.Fprintf(&sb, "  Transitions:        %d\n", s.Stability.TransitionCount)
	fmt.Fprintf(&sb, "  Regulating Uptime:  %.2f%%\n", s.Stability.RegulatingUptime*100)

	// 4. Control
	sb.WriteString("\n--- 🧠 Control Dynamics (Steady State) ---\n")
	if s.Control.Duration > 0 {
		fmt.Fprintf(&sb, "  Analysis Window:    %v\n", s.Control.Duration)
		fmt.Fprintf(&sb, "  Rise Time:          %v\n", s.Control.RiseTime)
		fmt.Fprintf(&sb, "  Settling Time:      %v\n", s.Control.SettlingTime)
		fmt.Fprintf(&sb, "  Overshoot:          %.2f%%\n", s.Control.Overshoot*100)
		fmt.Fprintf(&sb, "  IAE:                %.4f (Integrated Error)\n", s.Control.IAE)
		fmt.Fprintf(&sb, "  Steady Error:       %.4f\n", s.Control.SteadyStateError)
		fmt.Fprintf(&sb, "  Stability:          %.4f (Rate StdDev)\n", s.Control.RateStability)
	} else {
		sb.WriteString("  [N/A] System did not enter Regulating regime long enough to analyze PID dynamics.\n")
		sb.WriteString("  (Check: Is the workload sufficient to saturate the backend?)\n")
	}

	// 5. Safety
	sb.WriteString("\n--- 🛡️ Safety ---\n")
	fmt.Fprintf(&sb, "  Shed Count:         %d\n", s.Safety.ShedCount)
	fmt.Fprintf(&sb, "  Max Queue (Pod):    %.2f\n", s.Safety.MaxBackendQueueDepth)
	fmt.Fprintf(&sb, "  Sat Duration:       %v\n", s.Safety.SaturationDuration)

	// 6. Efficiency
	sb.WriteString("\n--- ⚖️ Efficiency ---\n")
	fmt.Fprintf(&sb, "  Global Throughput:  %.2f RPS (Completed)\n", s.Efficiency.GlobalThroughput)
	fmt.Fprintf(&sb, "  Avg Utilization:    %.2f%%\n", s.Efficiency.AverageUtilization*100)
	fmt.Fprintf(&sb, "  Load Imbalance:     %.2f (CV)\n", s.Efficiency.LoadImbalance)

	// 7. Accuracy
	sb.WriteString("\n--- 🔮 Estimator Accuracy ---\n")
	fmt.Fprintf(&sb, "  Batch MAPE:         %.2f%%\n", s.Accuracy.BatchEstimatorMAPE*100)
	fmt.Fprintf(&sb, "  Rate Lag:           %v\n", s.Accuracy.RateEstimatorLag)

	sb.WriteString("======================================\n")
	return sb.String()
}

// --- 1. Latency (SLO Verification) ---
type LatencyMetrics struct {
	P50 time.Duration
	P90 time.Duration
	P99 time.Duration
	Max time.Duration

	// MeanDispatchWait is the time spent in the Flow Control Buffer.
	// This tracks the "Cost of Control" (how much delay the controller induces).
	MeanDispatchWait time.Duration

	// MeanBackendService is the time spent in the Backend (Prefill + Decode).
	// This tracks the "Cost of Physics".
	MeanBackendService time.Duration
}

// --- 2. Startup & Discovery (Phase 1: Probing) ---
type StartupMetrics struct {
	// TimeToRegulation is the duration from T=0 until the Controller first enters the Regulating Regime.
	// It measures the speed of the "Hill Climbing" and "Peer Seeding" logic.
	// Value is -1 or 0 if Regulation was never reached.
	TimeToRegulation time.Duration

	// PeakDiscoveryConcurrency is the maximum Concurrency Limit (L_peak) reached during the Probing phase.
	// Verifies that the Hill Climber successfully explored available capacity.
	PeakDiscoveryConcurrency float64
}

// --- 3. Regime Stability ---
type RegimeStabilityMetrics struct {
	// TransitionCount is the number of times the FSM changed state.
	// Target: 1 (Probing -> Regulating). Higher implies instability.
	TransitionCount int

	// RegulatingUptime is the % of total time spent in Regulating.
	// Target: > 90% for steady-state workloads.
	RegulatingUptime float64
}

// --- 4. Control Dynamics (Phase 2: Regulating) ---
type ControlMetrics struct {
	// Duration is the length of the valid analysis window (Time in Regulating).
	// If 0, the remaining metrics are invalid.
	Duration time.Duration

	// RiseTime is the time to reach 90% of the Setpoint during a step.
	// Target: < 500ms (Agility).
	RiseTime time.Duration

	// SettlingTime is the time to stay within +/- 5% of Setpoint.
	// Target: < 5s (Convergence).
	SettlingTime time.Duration

	// Overshoot is the max % deviation above Setpoint.
	// Target: < 15% (Damping).
	Overshoot float64

	// IAE (Integral Absolute Error): Sum(|SP - PV| * dt).
	// Target: Minimize (Tracking Quality).
	IAE float64

	// SteadyStateError: Average (SP - PV) in the final window.
	// Target: ~0.0 (No Bias). Positive means chronically under-utilized.
	SteadyStateError float64

	// RateStability is the Standard Deviation of the Dispatch Rate (Pacer Output).
	// Target: Low. High values indicate "Chattering" (rapid control output oscillation).
	RateStability float64
}

// --- 5. Safety & Reliability (The "Do No Harm" Checks) ---
type SafetyMetrics struct {
	// ShedCount is the number of requests dropped by the Flow Control Safety Filter.
	// Target: 0 (in steady state), >0 (during transient overload shocks).
	ShedCount int

	// MaxBackendQueueDepth is the worst-case queue observed on any single backend.
	// Target: < 1.5 * BatchSize (Prevention of Standing Queues).
	MaxBackendQueueDepth float64

	// SaturationDuration is the total time the system spent > 1.0 Saturation.
	// Target: Minimize.
	SaturationDuration time.Duration
}

// --- 6. Efficiency & Orchestration (The Scheduler) ---
type EfficiencyMetrics struct {
	// GlobalThroughput is the aggregate completions per second.
	// Calculated as: Count(CompletedRequests) / TotalDuration.
	// Target: Match Nominal Capacity.
	GlobalThroughput float64

	// LoadImbalance is the average Coefficient of Variation (CV) of utilization across pods.
	// Formula: StdDev(Utilization) / Mean(Utilization).
	// Target (Spread Mode): < 0.1 (Perfect Balance).
	// Target (BinPack Mode): > 0.5 (Intentional Imbalance).
	LoadImbalance float64

	// AverageUtilization is the mean GPU usage across the fleet.
	// Target: Close to Setpoint (0.85).
	AverageUtilization float64
}

// --- 7. Estimator Health (The Digital Twin) ---
type EstimatorMetrics struct {
	// BatchEstimatorMAPE is the Mean Absolute Percentage Error of ^B_eff vs True B_eff.
	// Only calculated for pods in the "Mature" state.
	// Target: < 10%. Checks if the "Pipe Width" logic is sound.
	BatchEstimatorMAPE float64

	// RateEstimatorLag represents how many seconds ^u_t trails the real throughput
	// during ramp-up events.
	// Target: < ServiceWindow/2. Checks if EWMAs are too slow.
	RateEstimatorLag time.Duration
}
