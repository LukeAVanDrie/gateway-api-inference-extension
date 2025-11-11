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

// Regime defines the operational mode of the SaturationController's Finite State Machine.
//
// It reflects the collective maturity of the backend pool (Aggregate Trust) and dictates the active control strategy:
//
//   - Dispatch Logic: Determines HOW traffic enters the pool (Concurrency Caps vs. Rate Regulation).
//   - Scheduling Logic: Determines WHERE traffic is placed (Discovery vs. Optimization).
type Regime int

const (
	// Halted indicates the pool has no ready or viable pods (e.g., Scale-from-Zero or full-pool failure).
	//
	// Control Strategy: "Physical Blockade".
	//   - Dispatch: All traffic is blocked at the gate to prevent queuing delays.
	//   - Scheduling: N/A.
	//   - Trigger: AvailableReplicas == 0.
	Halted Regime = iota

	// Probing indicates the pool has not yet met the maturity quorum required for stable rate estimation.
	//
	// Control Strategy: "Parallel Bootstrap".
	//   - Dispatch: Rate Limiter is disabled. Global concurrency is limited by the sum of individual Safety Caps
	//     (Sum(L_peak + 1)).
	//   - Scheduling: The ProbePicker distributes traffic to ALL Immature pods simultaneously, aiming to characterize the
	//     fleet parameters as fast as possible.
	//   - Trigger: MaturePods / TotalPods < QuorumThreshold.
	Probing

	// Regulating indicates the pool is mature, characterized, and stable.
	//
	// Control Strategy: "Steady State Optimization".
	//   - Dispatch: The 2-DOF (Feed-Forward + Feedback) P-Controller actively regulates the admission rate to pin
	//     saturation to the Saturation Setpoint.
	//   - Scheduling: New pods are onboarded serially (one at a time) to minimize variance injection.
	//     The AdaptiveScorer optimizes placement for Cost (Bin Packing) or Latency (Load Balancing).
	//   - Trigger: MaturePods / TotalPods >= QuorumThreshold.
	Regulating
)

// String makes Regime human-readable for logging and metrics.
func (r Regime) String() string {
	switch r {
	case Halted:
		return "Halted"
	case Probing:
		return "Probing"
	case Regulating:
		return "Regulating"
	default:
		return "Unknown"
	}
}

// MaturityState defines the "Trust Level" of an individual pod's internal physics model.
//
// It determines the confidence level of the estimators and dictates how the pod is treated by the split-horizon
// scheduling logic.
type MaturityState int

const (
	// Immature indicates the pod's Effective Batch Capacity (^B_eff) is unknown or statistically insignificant.
	//
	// Strategy: "Hill Climbing".
	//   - Capacity Estimate: Untrusted.
	//   - Safety Limit: Strictly capped at (L_peak + 1).
	//     This allows safe, incremental discovery of concurrency limits without risking overload.
	//   - Scheduling: Prioritized by the ProbePicker for discovery traffic.
	Immature MaturityState = iota

	// Maturing indicates the pod's Batch Capacity (^B_eff) is known, but its Service Rate (^μ_t) is not yet stable.
	//
	// Strategy: "Peer Seeding" (Synthetic Estimation).
	//   - Capacity Estimate: Trusted (^B_eff).
	//   - Rate Estimate: Synthetic. We apply Little's Law using the fleet's characteristic latency:
	//     μ_est = ^B_eff / W_pool_avg.
	//   - Scheduling: Treated as a standard candidate, but monitoring continues to verify the rate.
	Maturing

	// Mature indicates the pod is fully characterized. Both Capacity (^B_eff) and Rate (^μ_t) are trusted.
	//
	// Strategy: "Self-Measurement" (Closed Loop).
	//   - Capacity Estimate: Trusted.
	//   - Rate Estimate: Measured real-time throughput (^μ_t).
	//   - Scheduling: Fully utilized based on the Load Index.
	Mature

	// Dormant indicates a pod was Immature but failed to mature within a timeout (e.g., due to low traffic).
	//
	// Strategy: "Standby / Decay Handling".
	//   - Capacity Estimate: Stale/Unknown.
	//   - Safety Limit: Reverted to (L_peak + 1) to prevent "Ghost Capacity" assumptions.
	//   - Scheduling: Deprioritized. Only activated if the primary fleet is saturated ("Picked Under Pressure").
	//     (Note: These pods are ideal candidates for scale-down).
	Dormant
)

// String makes MaturityState human-readable for logging and metrics.
func (m MaturityState) String() string {
	switch m {
	case Immature:
		return "Immature"
	case Maturing:
		return "Maturing"
	case Mature:
		return "Mature"
	case Dormant:
		return "Dormant"
	default:
		return "Unknown"
	}
}
