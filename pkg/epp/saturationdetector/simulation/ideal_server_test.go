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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIdealServer_Lifecycle verifies the basic state transitions of a single request traversing the ideal backend.
func TestIdealServer_Lifecycle(t *testing.T) {
	t.Parallel()

	cfg := IdealServerConfig{
		MaxConcurrency:  1,
		SecondsPerToken: 1.0,
	}
	server := NewIdealServer(cfg)

	t0 := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	req := &Request{
		ID:        "req-1",
		Arrival:   t0,
		PromptLen: 10,
		OutputLen: 10, // Total 20 tokens -> 20 seconds duration
	}

	// 1. Submit
	server.Submit(req)
	state := server.GetState()
	assert.Equal(t, 1, state.QueueDepth, "Request should be queued immediately")
	assert.Equal(t, 0, state.RunningRequests, "Request should not run until Tick")

	// 2. Schedule (Tick at t0)
	server.Tick(t0)
	state = server.GetState()
	assert.Equal(t, 0, state.QueueDepth, "Queue should be drained")
	assert.Equal(t, 1, state.RunningRequests, "Request should be running")
	assert.Equal(t, StateDecode, req.State, "Request state should be active")
	assert.Equal(t, t0, req.ScheduleTime, "Schedule time should be recorded")
	expectedFinish := t0.Add(20 * time.Second) // 20 tokens * 1.0 sec/token = 20s.
	assert.Equal(t, expectedFinish, req.FinishTime, "Deterministic finish time should be set")

	// 3. Tick Mid-Flight (t0 + 10s)
	server.Tick(t0.Add(10 * time.Second))
	assert.Equal(t, 1, server.GetState().RunningRequests, "Request should still be running half-way through")
	assert.Empty(t, server.DrainCompletions(), "No requests should finish early")

	// 4. Tick Completion (t0 + 20s)
	server.Tick(expectedFinish)
	state = server.GetState()
	assert.Equal(t, 0, state.RunningRequests, "Request should be removed from running set")

	completed := server.DrainCompletions()
	require.Len(t, completed, 1, "Should return exactly one completed request")
	assert.Equal(t, "req-1", string(completed[0].ID))
	assert.Equal(t, StateDone, completed[0].State, "Final state should be StateDone")
}

// TestIdealServer_ConcurrencyAndOrdering validates the M/G/c queuing behavior.
// It ensures that:
// 1. No more than MaxConcurrency requests run at once.
// 2. Requests are processed in FIFO order.
// 3. Slots open up immediately after a request finishes.
func TestIdealServer_ConcurrencyAndOrdering(t *testing.T) {
	t.Parallel()

	cfg := IdealServerConfig{
		MaxConcurrency:  2,
		SecondsPerToken: 1.0,
	}
	server := NewIdealServer(cfg)
	t0 := time.Now()

	reqs := []*Request{
		{ID: "A", PromptLen: 1, OutputLen: 1, Arrival: t0}, // 2 tokens (2s duration)
		{ID: "B", PromptLen: 1, OutputLen: 4, Arrival: t0}, // 5 tokens (5s duration)
		{ID: "C", PromptLen: 1, OutputLen: 0, Arrival: t0}, // 1 token  (1s duration)
	}

	for _, r := range reqs {
		server.Submit(r)
	}

	// --- Step 1: Initial Schedule ---
	// Time: T+0
	// Expected: A and B run (Slots full). C queued.
	server.Tick(t0)
	state := server.GetState()
	assert.Equal(t, 2, state.RunningRequests, "Slots should be full (A, B)")
	assert.Equal(t, 1, state.QueueDepth, "C should remain in queue")
	assert.Equal(t, 1.0, state.Utilization, "Utilization should be 100%")

	assert.Equal(t, StateDecode, reqs[0].State, "A should be running")
	assert.Equal(t, StateDecode, reqs[1].State, "B should be running")
	assert.Equal(t, StateQueued, reqs[2].State, "C should be queued")

	// --- Step 2: A Finishes ---
	// Time: T+2s.
	// A (2s) finishes. B (5s) has 3s left. Slot opens for C.
	t2 := t0.Add(2 * time.Second)
	server.Tick(t2)

	completed := server.DrainCompletions()
	require.Len(t, completed, 1)
	assert.Equal(t, "A", string(completed[0].ID))

	state = server.GetState()
	assert.Equal(t, 2, state.RunningRequests, "C should have immediately filled the empty slot")
	assert.Equal(t, 0, state.QueueDepth, "Queue should be empty")

	assert.Equal(t, StateDecode, reqs[2].State, "C should now be running")
	assert.Equal(t, t2, reqs[2].ScheduleTime, "C should be scheduled exactly when A finished")

	// --- Step 3: C Finishes ---
	// Time: T+3s (C takes 1s, started at T+2s).
	// B (5s) still running (started at T+0, finishes at T+5).
	t3 := t0.Add(3 * time.Second)
	server.Tick(t3)

	completed = server.DrainCompletions()
	require.Len(t, completed, 1)
	assert.Equal(t, "C", string(completed[0].ID))

	state = server.GetState()
	assert.Equal(t, 1, state.RunningRequests, "Only B remains running")
	assert.Equal(t, 0.5, state.Utilization, "Utilization should be 50% (1/2)")

	// --- Step 4: B Finishes ---
	// Time: T+5s.
	t5 := t0.Add(5 * time.Second)
	server.Tick(t5)

	completed = server.DrainCompletions()
	require.Len(t, completed, 1)
	assert.Equal(t, "B", string(completed[0].ID))
	assert.Equal(t, 0, server.GetState().RunningRequests)
}

// TestIdealServer_ConfigurationValidation ensures invalid configs are rejected.
func TestIdealServer_ConfigurationValidation(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() {
		NewIdealServer(IdealServerConfig{MaxConcurrency: 0})
	}, "Should panic if concurrency is zero")

	assert.Panics(t, func() {
		NewIdealServer(IdealServerConfig{MaxConcurrency: -1})
	}, "Should panic if concurrency is negative")
}

// TestIdealServer_QueueOrdering ensures FIFO behavior is preserved over multiple cycles.
func TestIdealServer_QueueOrdering(t *testing.T) {
	t.Parallel()

	server := NewIdealServer(IdealServerConfig{
		MaxConcurrency:  1,
		SecondsPerToken: 0.0, // Instant completion for simpler testing.
	})
	t0 := time.Now()

	// Submit 5 requests.
	ids := []string{"1", "2", "3", "4", "5"}
	for _, id := range ids {
		server.Submit(&Request{ID: RequestID(id), Arrival: t0})
	}

	// 1. First Tick (Initial Schedule)
	server.Tick(t0)
	require.Empty(t, server.DrainCompletions(), "Tick 0 should only schedule, not drain")
	require.Equal(t, 1, server.GetState().RunningRequests)

	// 2. Subsequent Ticks (Drain previous, Schedule next)
	// Each tick should process exactly 1 request in order.
	for i, id := range ids {
		server.Tick(t0.Add(time.Duration(i) * time.Second))
		drained := server.DrainCompletions()
		require.Len(t, drained, 1, "Tick %d should drain exactly 1 request", i)
		assert.Equal(t, id, string(drained[0].ID), "Requests should emerge in FIFO order")
	}
}

// TestIdealServer_ZeroUtilization checks behavior when empty.
func TestIdealServer_ZeroUtilization(t *testing.T) {
	t.Parallel()
	server := NewIdealServer(IdealServerConfig{MaxConcurrency: 10})

	state := server.GetState()
	assert.Equal(t, 0.0, state.Utilization)
	assert.Equal(t, 0, state.RunningRequests)
	assert.Empty(t, server.DrainCompletions())
}
