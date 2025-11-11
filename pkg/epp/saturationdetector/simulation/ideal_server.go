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
	"container/list"
	"sync"
	"time"
)

// IdealServerConfig configures a simplified, deterministic server.
// It models the system as a standard M/G/c Queueing System.
type IdealServerConfig struct {
	// MaxConcurrency represents the hard limit of parallel requests (c).
	// In this model, Utilization = RunningRequests / MaxConcurrency.
	MaxConcurrency int

	// SecondsPerToken defines the deterministic service rate per token.
	// This simplifies the model to assume Prefill and Decode have identical costs.
	// Example: 0.02 (20ms) implies a generation speed of 50 tokens/sec.
	SecondsPerToken float64
}

// IdealServer provides a "Control Group" for validating controller logic.
// It acts as an M/G/c queueing system where service time is strictly proportional to the request payload (TotalTokens).
// It abstracts away GPU memory dynamics, batching overheads, and distinct prefill/decode performance characteristics.
type IdealServer struct {
	config IdealServerConfig

	mu                sync.Mutex
	performanceFactor float64
	pending           *list.List
	running           []*Request
	completed         []*Request
}

var _ Backend = &IdealServer{}

// NewIdealServer creates a deterministic backend for controller verification.
func NewIdealServer(cfg IdealServerConfig) *IdealServer {
	if cfg.MaxConcurrency <= 0 {
		panic("IdealServer: MaxConcurrency must be > 0")
	}
	return &IdealServer{
		config:  cfg,
		performanceFactor: 1.0,
		pending:           list.New(),
		running: make([]*Request, 0),
	}
}

func (s *IdealServer) Submit(req *Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req.State = StateQueued
	s.pending.PushBack(req)
}

func (s *IdealServer) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Drain Completed Work
	// Check which requests have logically finished based on the current clock.
	active := s.running[:0]
	for _, r := range s.running {
		if !now.Before(r.FinishTime) {
			r.State = StateDone
			s.completed = append(s.completed, r)
		} else {
			active = append(active, r)
		}
	}
	s.running = active

	// 2. Schedule New Work
	// Simple FCFS strategy constrained only by MaxConcurrency.
	freeSlots := s.config.MaxConcurrency - len(s.running)

	// Iterate logic updated for List
	for freeSlots > 0 && s.pending.Len() > 0 {
		front := s.pending.Front()
		r := front.Value.(*Request)
		s.pending.Remove(front)

		freeSlots--

		// Ideal Model Simplifications:
		// - Instant Prefill (infinite TFLOPS).
		// - Constant Decode Speed (no Bandwidth constraint).
		r.State = StateDecode
		r.ScheduleTime = now
		r.FirstTokenTime = now // TTFT = Queue Wait Time

		// Apply Degradation to the Service Rate
		// Duration = (Tokens * SecondsPerToken) / PerformanceFactor
		// Factor 0.5 -> Duration doubles.
		normalDurationSec := float64(r.TotalTokens()) * s.config.SecondsPerToken
		degradedDurationSec := normalDurationSec / s.performanceFactor
		r.FinishTime = now.Add(time.Duration(degradedDurationSec * float64(time.Second)))

		s.running = append(s.running, r)
	}
}

func (s *IdealServer) DrainCompletions() []*Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.completed) == 0 {
		return nil
	}
	drained := s.completed
	s.completed = nil
	return drained
}

func (s *IdealServer) GetState() SystemState {
	s.mu.Lock()
	defer s.mu.Unlock()
	util := float64(len(s.running)) / float64(s.config.MaxConcurrency)
	return SystemState{
		QueueDepth:      s.pending.Len(),
		RunningRequests: len(s.running),
		Utilization:     min(util, 1.0),
		TrueBatchCapacity: s.config.MaxConcurrency,
	}
}

func (s *IdealServer) SetTimeDilation(factor float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.performanceFactor = max(factor, 0.001)
}

func (s *IdealServer) NextStepDuration() time.Duration {
	// For the ideal model, we don't need micro-steps because service times are calculated deterministically upon
	// scheduling. Instead, we return a coarse tick to align with typical controller reconciliation loops.
	return 50 * time.Millisecond
}

// EstimateCapacity implements the Backend interface using M/G/c theory.
func (s *IdealServer) EstimateCapacity(profile WorkloadProfile) CapacityInfo {
	// 1. Calculate Average Request Cost
	avgTokens := profile.MeanInputTokens() + profile.MeanOutputTokens()
	avgServiceTimeSec := avgTokens * s.config.SecondsPerToken

	// 2. Calculate System Capacity
	// Throughput = Concurrency / ServiceTime
	maxQPS := float64(s.config.MaxConcurrency) / avgServiceTimeSec

	return CapacityInfo{
		MaxThroughputQPS: maxQPS,
		AverageLatency:   time.Duration(avgServiceTimeSec * float64(time.Second)),
	}
}
