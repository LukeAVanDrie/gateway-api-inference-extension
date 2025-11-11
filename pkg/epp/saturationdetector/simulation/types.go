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

package simulation

import (
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector"
)

// --- Configuration Types (Inputs) ---

// SimEnvConfig defines the static parameters for a simulation run.
type SimEnvConfig struct {
	// --- System Under Test (SUT) Config ---

	// ControllerConfig configures the "Brain" (Saturation Controller).
	// This includes Kp, Alphas, Windows, and Thresholds.
	ControllerConfig *saturationdetector.ControllerConfig

	// RecorderConfig configures the "Sensor" (Signal Recorder).
	// This defines the Tick Rate and Buffer sizes.
	RecorderConfig *saturationdetector.SignalRecorderConfig

	// --- Simulation Physics ---

	// Backends defines the initial static topology of the cluster.
	// Map Key: Pod Name (e.g., "pod-0").
	// Map Value: An initialized Backend instance (Ideal or HiFi).
	// Note: Dynamic scaling scenarios can add more backends at runtime via the Simulator interface.
	Backends map[string]Backend

	// ScrapeInterval controls the update frequency of the Mock Datastore (Prometheus extraction).
	// This introduces "Observability Lag" into the control loop.
	// Default: 50ms.
	ScrapeInterval time.Duration

	// Seed ensures deterministic Random Number Generation (RNG) for traffic arrival times.
	Seed int64
}

// --- Telemetry Types ---

// Snapshot captures the exact state of the system at a specific moment in virtual time.
// It is recorded internally by the Engine at every tick and projected into SimResult.
type Snapshot struct {
	Timestamp time.Time

	// --- The Brain (Controller Internals) ---
	ControllerState       saturationdetector.ControllerState
	FlowControlQueueDepth int

	// --- The Plant (Physics Truth) ---
	// Objective reality from the Backend.
	TotalInflight             int
	AverageBackendQueueDepth  float64
	AverageBackendUtilization float64
	PodPhysics                map[string]SystemState
}

// --- Output Types (Results) ---

// SimResult holds the complete, analyzeable history of a simulation run.
// It acts as the artifact for Verification Assertions and Plotting.
type SimResult struct {
	// Metadata
	Duration time.Duration

	// Counters (Cumulative)
	TotalRequests    int
	ShedRequestCount int

	// Timeline is the ordered sequence of state snapshots.
	Timeline []Snapshot

	// CompletedRequests is the log of all finished requests.
	CompletedRequests []*Request
}

// --- Helper Methods ---

// Last returns the final TimeStep in the simulation.
func (r *SimResult) Last() Snapshot {
	if len(r.Timeline) == 0 {
		return Snapshot{}
	}
	return r.Timeline[len(r.Timeline)-1]
}

// Slice returns a view of the timeline between start and end durations.
func (r *SimResult) Slice(start, end time.Duration) []Snapshot {
	var subset []Snapshot
	if len(r.Timeline) == 0 {
		return subset
	}

	startTime := r.Timeline[0].Timestamp.Add(start)
	endTime := r.Timeline[0].Timestamp.Add(end)

	for _, t := range r.Timeline {
		if (t.Timestamp.Equal(startTime) || t.Timestamp.After(startTime)) &&
			(t.Timestamp.Equal(endTime) || t.Timestamp.Before(endTime)) {
			subset = append(subset, t)
		}
	}
	return subset
}
