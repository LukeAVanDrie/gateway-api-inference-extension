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
	"time"

	"k8s.io/utils/clock/testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector/simulation"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector/verification/framework"
)

const (
	// LabIdeal uses deterministic M/D/c queuing theory backends.
	// Purpose: Control Logic Verification (PID tuning, FSM transitions) without GPU physics noise.
	LabIdeal framework.LabID = "Lab-Ideal"

	LabHiFi framework.LabID = "Lab-HiFi"
)

// StandardLabs is the registry of available test environments.
var StandardLabs = map[framework.LabID]framework.LabFactory{
	// LabIdeal: buildLabIdeal,
  LabHiFi: buildLabHiFi,
}

func buildLabIdeal(traffic simulation.WorkloadProfile, overrides []framework.ConfigParam) simulation.Simulator {
	// Define Backend Generator (The "Ideal" Plant)
	// 100 Concurrency, 10ms per token (Deterministic)
	idealBackendCfg := simulation.IdealServerConfig{
		MaxConcurrency:  100,
		SecondsPerToken: 0.01,
	}
	backendGen := func(podID string) simulation.Backend {
		return simulation.NewIdealServer(idealBackendCfg)
	}
	return buildLab(traffic, overrides, backendGen)
}

func buildLabHiFi(traffic simulation.WorkloadProfile, overrides []framework.ConfigParam) simulation.Simulator {
	backendGen := func(id string) simulation.Backend {
		physics, _ := simulation.NewStandardPhysics(simulation.H100_80GB_SXM5, simulation.Qwen3_32B_FP16, 1)
		return simulation.NewHighFidelityInferenceServer(physics, 101, 0.05)
	}

	return buildLab(traffic, overrides, backendGen)
}

func buildLab(traffic simulation.WorkloadProfile, overrides []framework.ConfigParam, backendGen func(podID string) simulation.Backend) simulation.Simulator {
	// 1. Time Control
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeClock := testing.NewFakeClock(start)

	// 2. System Under Test (SUT) Configuration
	// We use standard defaults for the Controller logic.
	builder := saturationdetector.NewControllerConfigBuilder().
		WithSignalRecorderPluginName("recorder").
		WithMaxQueueLatency(5 * time.Second).
		WithQueueDepthAlpha(0.8).
		WithProportionalGain(2.0).
		WithSaturationSetpoint(1).
    WithServiceRateWindow(30 * time.Second)

	// B. Apply Overrides (The Test Requirements)
	for _, p := range overrides {
		if p.Apply != nil {
			p.Apply(builder)
		}
	}

	ctrlConfig, _ := builder.Build()
	recorderConfig, _ := saturationdetector.NewSignalRecorderConfigBuilder().Build()

	// 3. Construct SUT Components
	recorder := saturationdetector.NewSaturationSignalRecorder(
		recorderConfig,
		saturationdetector.WithSaturationSignalRecorderClock(fakeClock),
	)

	ds := simulation.NewMockDatastore()
	buffer := simulation.NewFlowControlBuffer()
	qm := &simulation.MockQueueMonitor{Buffer: buffer}
	controller := saturationdetector.NewSaturationController(
		ctrlConfig,
		recorder,
		qm,
		ds,
		saturationdetector.WithClock(fakeClock),
	)

	picker := saturationdetector.NewProbePicker(controller, &simulation.GreedyPicker{})

	// 4. Simulation Environment Configuration
	simConfig := simulation.SimEnvConfig{
		RecorderConfig: recorderConfig,
		ControllerConfig: ctrlConfig,
		ScrapeInterval: 50 * time.Millisecond,
		Backends:       nil, // Started empty
	}

	// 6. Assemble
	return simulation.NewSimulator(
		simConfig,
		backendGen,
		traffic,
		controller,
		picker,
		recorder,
		buffer,
		ds,
		fakeClock,
	)
}
