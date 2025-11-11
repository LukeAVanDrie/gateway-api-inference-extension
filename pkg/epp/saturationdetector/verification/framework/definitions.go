package framework

import (
	"testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector/simulation"
)

// --- The Environment (Where) ---

type LabID string

// LabFactory creates a fresh simulation environment for a specific test run.
type LabFactory func(traffic simulation.WorkloadProfile, overrides []ConfigParam) simulation.Simulator

// --- The Experiment (What) ---

type ScenarioID string

// Scenario represents a specific experimental procedure.
type Scenario struct {
	ID          ScenarioID
	Description string

	// Traffic defines the workload shape (Chat, RAG, etc.) used to initialize the Lab.
	Traffic simulation.WorkloadProfile

	// Execute is the script that drives the simulation timeline.
	// It uses the fluent Simulator API (e.g., sim.SetRelativeLoad(1.5)).
	Execute func(sim simulation.Simulator)

	// Assertions performs sanity checks that must pass for ANY run of this scenario, regardless of which Config Params
	// are being verified.
	// Example: "The simulator shouldn't panic", "Traffic should flow".
	Assertions func(t *testing.T, score *Scorecard)
}

// --- The Requirement (Why) ---

// ConfigParam links a specific production default value to a proof of correctness.
type ConfigParam struct {
	// Name matches the field name in ControllerConfig (e.g., "ProportionalGain").
	Name string

	// Value is the default value being verified (e.g., 2.0).
	// This serves as documentation and can be asserted against the actual config.
	Value any

	// Matrix Mapping: Which Cell proves this parameter?
	LabID      LabID
	ScenarioID ScenarioID

	// Apply injects this parameter into the Controller configuration.
	// This allows the test requirement to override the Lab's defaults.
	Apply func(b *saturationdetector.ControllerConfigBuilder)

	// Verify performs the specific acceptance check for this parameter.
	// Example: "RiseTime < 500ms".
	Verify func(t *testing.T, score *Scorecard)
}

// MatrixCell represents a unique intersection of Environment and Experiment.
type MatrixCell struct {
	LabID      LabID
	ScenarioID ScenarioID
}
