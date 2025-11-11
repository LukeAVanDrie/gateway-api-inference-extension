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

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

// Filter blocks traffic to pods that are physically saturated or exceeding their safety limits.
func (sc *SaturationController) Filter(
	_ context.Context,
	_ *types.CycleState,
	_ *types.LLMRequest,
	pods []types.Pod,
) []types.Pod {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	// The filter uses a higher, absolute threshold to act as a final safety gate, preventing thrashing with the
	// P-controller's regulating setpoint.
	hardLimit := sc.config.SaturationSetpoint + sc.config.SaturationHeadroom

	var filtered []types.Pod
	state := sc.Introspect()
	for _, pod := range pods {
		snap := state.Pods[pod.GetPod().NamespacedName.String()]
		if snap.ID.String() == "" {
			filtered = append(filtered, pod) // Fail open for unknown pods.
			continue
		}

		// A request has already been approved for dispatch by the pool-wide Pacer (during Regulating regime) or concurrency
		// limit (during Probing regime); this filter ensures it is not sent to a pod that is individually saturated.
		// This handles both Mature (Saturation < Limit) and Immature (Inflight < L_peak + 1) cases.
		if snap.SaturationPV < hardLimit {
			filtered = append(filtered, pod)
		}
	}

	// Fail open if all pods are saturated.
	// A request  has already been dequeued and approved for dispatch; dropping it here by returning an empty list would
	// be an undesirable, implicit load-shed.
	//
	// With sufficient SaturationHeadroom, this scenario should only occur in rare edge cases (e.g., heavy
	// oscillations), and the P-controller will quickly self-correct on the next tick by throttling traffic.
	// This logic prioritizes availability and prevents request loss.
	if len(filtered) == 0 {
		return pods
	}
	return filtered
}
