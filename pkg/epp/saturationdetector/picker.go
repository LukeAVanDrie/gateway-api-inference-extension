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
	"math/rand/v2"
	"sort"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

// ProbePicker implements Split-Horizon Routing.
//
// It acts as a strict, controller-aware tiering filter before delegating to decorated Picker.
// The horizons are processed in strict order:
//  1. Discovery Horizon (Immature): Probabilistic injection for learning.
//  2. Production Horizon (Mature/Maturing): The standard pool for traffic.
//  3. Standby Horizon (Dormant): The "Panic Valve". Only used if Production is empty.
type ProbePicker struct {
	controller *SaturationController
	delegate   framework.Picker

	// probeMu protects the Round-Robin state
	probeMu           sync.Mutex
	lastProbeTargetNN string
}

func NewProbePicker(controller *SaturationController, delegate framework.Picker) *ProbePicker {
	return &ProbePicker{
		controller: controller,
		delegate:   delegate,
	}
}

func (p *ProbePicker) TypedName() plugins.TypedName { return p.delegate.TypedName() }

func (p *ProbePicker) Pick(
	ctx context.Context,
	cycleState *types.CycleState,
	scoredPods []*types.ScoredPod,
) *types.ProfileRunResult {
	probeSet := p.controller.GetProbeCandidates()
	probes, primaries, dormants := p.classifyPods(scoredPods, probeSet)

	// If the controller has no state for any of these pods, fall back to the standard delegate behavior with all pods.
	if len(probes) == 0 && len(primaries) == 0 && len(dormants) == 0 {
		return p.delegate.Pick(ctx, cycleState, scoredPods)
	}

	// Horizon 1: Discovery (Probing)
	if len(probes) > 0 {
		// Select target via Round-Robin to ensure even characterization spread across the immature fleet.
		target := p.nextRoundRobinTarget(probes)

		// Check the "Admission Probability" for this specific target.
		prob := p.controller.GetProbeProbability(target.GetPod().NamespacedName.String())
		if rand.Float64() < prob || (len(primaries) == 0 && len(dormants) == 0) {
			// HIT: Route specifically to the probe target.
			// We short-circuit the delegate logic here because probing is a forced override.
			return p.delegate.Pick(ctx, cycleState, []*types.ScoredPod{target})
		}
		// MISS: Fall through to the Production Horizon.
	}

	// Horizon 2: Production (Steady State)
	if len(primaries) > 0 {
		return p.delegate.Pick(ctx, cycleState, primaries)
	}

	// Horizon 3: Standby (Picked Under Pressure)
	// If we reach here, we have no Probes (or missed the roll) and no Mature/Maturing pods.
	// We MUST wake up a Dormant pod to handle the traffic.
	return p.delegate.Pick(ctx, cycleState, dormants)
}

// classifyPods buckets the schedulable pods into the three Split-Horizon categories.
func (p *ProbePicker) classifyPods(
	scored []*types.ScoredPod,
	probeSet sets.Set[string],
) (probes, primaries, dormants []*types.ScoredPod) {
	state := p.controller.Introspect()
	for _, sp := range scored {
		id := sp.GetPod().NamespacedName.String()
		if probeSet.Has(id) {
			probes = append(probes, sp)
			continue
		}

		snap := state.Pods[id]
		if snap.ID.String() == "" {
			continue // Edge case: Controller doesn't know this pod yet.
		}

		switch snap.Maturity {
		case Maturing, Mature:
			primaries = append(primaries, sp)
		case Dormant:
			dormants = append(dormants, sp)
		}
	}
	return
}

// nextRoundRobinTarget selects a probe candidate in a deterministic rotation.
// This ensures that in the "Probing" regime (Parallel Bootstrap), we don't starve any specific Immature pod due to
// random selection noise.
func (p *ProbePicker) nextRoundRobinTarget(candidates []*types.ScoredPod) *types.ScoredPod {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].GetPod().NamespacedName.String() < candidates[j].GetPod().NamespacedName.String()
	})

	nextIdx := 0
	if p.lastProbeTargetNN != "" {
		for i, cand := range candidates {
			if cand.GetPod().NamespacedName.String() == p.lastProbeTargetNN {
				nextIdx = (i + 1) % len(candidates)
				break
			}
		}
	}

	target := candidates[nextIdx]
	p.lastProbeTargetNN = target.GetPod().NamespacedName.String()
	return target
}
