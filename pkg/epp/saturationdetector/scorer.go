package saturationdetector

import (
	"context"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

// --- Defaults Derived from Operational Experience ---
// TODO: make this configurable.
const (
	packThreshold   = 0.4 // < 40%: Plenty of space. Pack tight to save money.
	spreadThreshold = 0.8 // > 80%: Danger zone. Spread aggressively to protect SLOs.
)

// Score implements an adaptive, Load-Aware Scheduling Strategy that transitions continuously between Cost Optimization
// (Bin Packing) and Latency Optimization (Load Balancing).
//
// Physics:
//   - Low Load: We want to pack requests to maximize KV Cache Reuse (Prefix Caching) and allow idle pods to go Dormant
//     (Scale Down).
//   - High Load: We want to spread requests to minimize Queue Wait Time (Kingman's Formula), avoiding the
//     "Latency Cliff" at high utilization.
func (sc *SaturationController) Score(
	ctx context.Context,
	_ *types.CycleState,
	_ *types.LLMRequest,
	pods []types.Pod,
) map[types.Pod]float64 {
	// 1. Determine Global Pressure (The Mixing Signal)
	loadIndex := sc.GetLoadIndex()

	// 2. Calculate Mixing Factor (Alpha)
	// Linear interpolation from Pack -> Spread based on LoadIndex.
	// Alpha = 0.0 -> Pure Bin Packing
	// Alpha = 1.0 -> Pure Load Balancing
	alpha := 0.0
	if delta := spreadThreshold - packThreshold; delta > 0 {
		alpha = (loadIndex - packThreshold) / delta
	}
	alpha = max(0.0, min(1.0, alpha))

	scores := make(map[types.Pod]float64)
	setpoint := sc.config.SaturationSetpoint

	state := sc.Introspect()
	for _, pod := range pods {
		snap := state.Pods[pod.GetPod().NamespacedName.String()]
		if snap.ID.String() == "" {
			scores[pod] = 0.5 // Neutral score for unknown
			continue
		}

		// 3. Normalize Saturation
		// We normalize against the Setpoint because that represents "Effective Full".
		// Range: [0.0, 1.0] (Clamped)
		saturation := 0.0
		if setpoint > 0 {
			saturation = snap.SaturationPV / setpoint
		}
		saturation = min(1.0, saturation)

		// 4. Calculate Component Scores

		// Strategy A: Bin Packing (Cost)
		// Score increases with Saturation. "Fill the fullest bucket first."
		scoreBinPack := saturation

		// Strategy B: Load Balancing (Latency)
		// Score decreases with Saturation. "Pick the emptiest bucket."
		// We use a Quadratic Penalty ((1-s)^2) to model the inverse of Kingman's Curve.
		// This penalizes high load much more severely than moderate load.
		inverseSat := 1.0 - saturation
		scoreSpread := inverseSat * inverseSat

		// 5. Blend
		finalScore := ((1.0 - alpha) * scoreBinPack) + (alpha * scoreSpread)
		scores[pod] = finalScore
	}

	return scores
}
