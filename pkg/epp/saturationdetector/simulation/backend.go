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
	"fmt"
	"time"
)

// RequestID is a unique correlation ID for tracing a request through the system.
type RequestID string

// RequestState tracks the discrete phase of an LLM request.
// The simulator uses this state to apply the correct Roofline constraint (Compute vs. Memory).
type RequestState int

const (
	// StateQueued: Buffered in the pending queue. No GPU memory is allocated yet.
	StateQueued RequestState = iota

	// StatePrefill: Processing the input prompt (The "Context" Phase).
	// Characteristics: High Arithmetic Intensity (Compute Bound).
	// Bottleneck: TFLOPS (Tensor Cores).
	StatePrefill

	// StateDecode: Generating output tokens autoregressively (The "Generation" Phase).
	// Characteristics: Low Arithmetic Intensity (Memory Bound).
	// Bottleneck: HBM Bandwidth.
	StateDecode

	// StatePreempted: Forcefully evicted from GPU memory to resolve an OOM condition.
	// The request is paused and must wait to be re-scheduled.
	// NOTE: In this simulation, preemption is "Lossy"; the KV cache is dropped, forcing a re-computation.
	StatePreempted

	// StateDone: Generation completed (EOS token or length limit reached).
	StateDone
)

// Request models a single inference job traversing the system.
type Request struct {
	ID      RequestID
	Arrival time.Time

	// --- Statistical Shape ---
	PromptLen int // Input token count (Context Window)
	OutputLen int // Desired output token count (Generation Length)

	// --- Execution State ---
	State           RequestState
	GeneratedTokens int // Count of tokens generated so far
	PrefillProgress int // Count of prompt tokens processed (used for Chunked Prefill)

	// --- Telemetry ---
	ScheduleTime    time.Time // t_dispatch: Time of first memory allocation
	FirstTokenTime  time.Time // t_ttft: Time first output token was generated
	FinishTime      time.Time // t_completion: Time request left the system
	PreemptionCount int       // Tracks how many times this specific request was evicted.
}

// TotalTokens returns the aggregate computational cost (Input + Output).
func (r *Request) TotalTokens() int {
	return r.PromptLen + r.OutputLen
}

func (r *Request) String() string {
	return fmt.Sprintf("[%s] St=%d P=%d G=%d/%d", r.ID, r.State, r.PromptLen, r.GeneratedTokens, r.OutputLen)
}

// SystemState captures the instantaneous observability signals of the backend.
// This serves as the sensor input (the "Plant State") for the Saturation Controller.
type SystemState struct {
	// QueueDepth is the count of requests waiting for resources.
	QueueDepth int

	// RunningRequests is the count of request actively being processed.
	RunningRequests int

	// Utilization represents the saturation level of the critical bottleneck [0.0 - 1.0].
	Utilization float64

	// TrueBatchCapacity represents the physical or configured concurrency limit.
	// Used as Ground Truth to verify the Controller's ^B_eff estimator.
	TrueBatchCapacity int

	TrueServiceRate float64
}

// CapacityInfo captures the theoretical limits of the backend.
type CapacityInfo struct {
	// MaxThroughputQPS is the sustained request rate limit.
	MaxThroughputQPS float64
	// AverageLatency is the expected end-to-end time for a request.
	AverageLatency time.Duration
}

// Backend is the polymorphic abstraction of the inference plant.
type Backend interface {
	// Submit enqueues a request for processing.
	// Contract: The backend must assign a State of StateQueued immediately.
	Submit(req *Request)

	// Tick advances the internal simulation clock to 'now'.
	// It performs scheduling, execution, and resource reclamation.
	Tick(now time.Time)

	// DrainCompletions returns requests that finished exactly at or before the last Tick.
	DrainCompletions() []*Request

	// GetState returns the current metrics for the control loop.
	GetState() SystemState

	// NextStepDuration advises the simulation driver on the optimal time-step size.
	NextStepDuration() time.Duration

	// EstimateCapacity calculates theoretical performance for a given workload.
	// This is useful for normalizing load (e.g., "Run at 80% capacity").
	EstimateCapacity(profile WorkloadProfile) CapacityInfo

	// SetTimeDilation scales the execution speed of the backend.
	// factor 1.0 = Normal Speed.
	// factor 0.5 = 50% Speed (2x Slower).
	// factor 0.1 = Severe Degradation (10x Slower).
	SetTimeDilation(factor float64)
}
