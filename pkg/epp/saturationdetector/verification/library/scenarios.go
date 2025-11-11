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

package library

import (
	"testing"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector/simulation"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector/verification/framework"
)

const (
	// ScenSmokeTest is a minimal run to verify system stability.
	// Target: 1 Pod, 50% Load.
	ScenSmokeTest framework.ScenarioID = "Smoke-Test"
	ScenHoLBlocking framework.ScenarioID = "HoL-Blocking"
)

var StandardScenarios = map[framework.ScenarioID]framework.Scenario{
	// ScenSmokeTest: {
	// 	ID:          ScenSmokeTest,
	// 	Description: "Verifies that the simulation harness runs and produces valid telemetry without panicking.",
	// 	Traffic:     simulation.ProfileBalanced,
	// 	Execute: func(sim simulation.Simulator) {
	// 		sim.AddBackends(1)       // Provisioning
	// 		sim.SetRelativeLoad(1.5) // Warmup (Low Load)
	// 		sim.Run(60 * time.Second) // Run for enough time to see at least one snapshot.
	// 	},
	// 	Assertions: func(t *testing.T, score *framework.Scorecard) {
	// 		// Basic Liveness Checks
	// 		if score.Safety.ShedCount > 0 {
	// 			t.Errorf("Unexpected dropped requests in smoke test: %d", score.Safety.ShedCount)
	// 		}
	// 		if score.Efficiency.GlobalThroughput <= 0 {
	// 			t.Error("System appears dead: 0 throughput")
	// 		}
	// 		if score.Control.IAE == 0 {
	// 			t.Error(`
	// 				Control IAE is 0.0. This is statistically impossible for a dynamic simulation.
  //         Causes:
	// 				  1. Controller halted/crashed.
	// 					2. Metrics pipeline broken (reading 0s).
	// 					3. Trivial scenario (Load=0).`)
	// 		}
	// 	},
	// },
	ScenHoLBlocking: {
		ID:          ScenHoLBlocking,
		Description: "Showcases the benefits of HoL Shift-Left.",
		Traffic:     simulation.ProfileBalanced,
		Execute: func(sim simulation.Simulator) {
			sim.AddBackends(3)
			sim.SetRelativeLoad(1.5)
			sim.Run(60 * time.Second)
		},
		Assertions: func(t *testing.T, score *framework.Scorecard) {
			t.Skip()
			// // Basic Liveness Checks
			// if score.Safety.ShedCount > 0 {
			// 	t.Errorf("Unexpected dropped requests in smoke test: %d", score.Safety.ShedCount)
			// }
			// if score.Efficiency.GlobalThroughput <= 0 {
			// 	t.Error("System appears dead: 0 throughput")
			// }
			// if score.Control.IAE == 0 {
			// 	t.Error(`
			// 		Control IAE is 0.0. This is statistically impossible for a dynamic simulation.
      //     Causes:
			// 		  1. Controller halted/crashed.
			// 			2. Metrics pipeline broken (reading 0s).
			// 			3. Trivial scenario (Load=0).`)
			// }
		},
	},
}
