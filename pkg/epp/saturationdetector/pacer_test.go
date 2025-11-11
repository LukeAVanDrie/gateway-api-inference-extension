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
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
)

// forceReplenish is a test helper to trigger the pacer's lazy replenishment.
// It must be used after advancing the fake clock to observe the effect of time passing.
func forceReplenish(p *Pacer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.config.Load().(*pacerConfig)
	p.replenish(p.clock.Now(), cfg)
}

// Helper function to create a pacer with a fake clock for deterministic testing.
func newTestPacer(initialRate float64, burstDuration time.Duration, clock *testingclock.FakeClock) *Pacer {
	return NewPacer(initialRate, burstDuration, clock)
}

func TestNewPacer(t *testing.T) {
	t.Parallel()

	// Arrange
	testCases := []struct {
		name               string
		initialRate        float64
		burstDuration      time.Duration
		expectedRate       float64
		expectedCapacity   float64
		expectedInitTokens float64
	}{
		{
			name:               "standard_initialization/rate_5_burst_100ms",
			initialRate:        5.0,
			burstDuration:      100 * time.Millisecond,
			expectedRate:       5.0,
			expectedCapacity:   max(1.0, 5.0*0.1), // 1.0
			expectedInitTokens: 1.0,               // min(capacity, 1.0)
		},
		{
			name:               "initialization_with_large_burst/rate_10_burst_1s",
			initialRate:        10.0,
			burstDuration:      1 * time.Second,
			expectedRate:       10.0,
			expectedCapacity:   max(1.0, 10.0*1.0), // 10.0
			expectedInitTokens: 1.0,                // min(capacity, 1.0)
		},
		{
			name:               "initialization_with_zero_rate/clamps_rate_and_sets_min_capacity",
			initialRate:        0.0,
			burstDuration:      100 * time.Millisecond,
			expectedRate:       0.0,
			expectedCapacity:   1.0, // max(0.0*0.1, 1.0)
			expectedInitTokens: 1.0, // min(capacity, 1.0)
		},
		{
			name:               "initialization_with_negative_rate/clamps_rate_and_sets_min_capacity",
			initialRate:        -10.0,
			burstDuration:      100 * time.Millisecond,
			expectedRate:       0.0,
			expectedCapacity:   1.0, // max(0.0*0.1, 1.0)
			expectedInitTokens: 1.0, // min(capacity, 1.0)
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fakeClock := testingclock.NewFakeClock(time.Now())

			// Act
			p := newTestPacer(tc.initialRate, tc.burstDuration, fakeClock)

			// Assert
			p.mu.Lock()
			defer p.mu.Unlock()

			require.Equal(t, tc.expectedRate, p.GetRate(), "GetRate() should return the configured rate")

			cfg := p.config.Load().(*pacerConfig)
			require.Equal(t, tc.expectedRate, cfg.rate, "Internal config rate should match expected")
			require.Equal(t, tc.expectedCapacity, cfg.capacity, "Internal config capacity should match expected")
			require.Equal(t, tc.expectedInitTokens, p.tokens, "Initial tokens should be correctly set")
		})
	}
}

func TestPacer_Allow(t *testing.T) {
	t.Parallel()

	// Arrange
	type action struct {
		advanceTime time.Duration
		cost        float64
		expectAllow bool
	}
	testCases := []struct {
		name          string
		initialRate   float64
		burstDuration time.Duration
		actions       []action
	}{
		{
			name:          "steady_rate/allow_one_per_250ms_at_4qps",
			initialRate:   4.0,                    // 1 token every 250ms
			burstDuration: 100 * time.Millisecond, // Capacity = max(1, 4*0.1) = 1.0
			actions: []action{
				{cost: 1.0, expectAllow: true},                                       // Initial token is used
				{cost: 1.0, expectAllow: false},                                      // No time has passed, no new token
				{advanceTime: 249 * time.Millisecond, cost: 1.0, expectAllow: false}, // Not enough time passed for a full token
				{advanceTime: 1 * time.Millisecond, cost: 1.0, expectAllow: true},    // Total 250ms, now we have a token
				{advanceTime: 1 * time.Second, cost: 1.0, expectAllow: true},         // A full second passes, bucket is at capacity
			},
		},
		{
			name:          "burst/allow_burst_up_to_capacity",
			initialRate:   10.0,
			burstDuration: 500 * time.Millisecond, // Capacity = 10 * 0.5 = 5.0
			actions: []action{
				// Initial tokens are 1.0. Let's let the bucket fill up.
				{advanceTime: 1 * time.Second},  // Fill the bucket to its capacity of 5.0
				{cost: 1.0, expectAllow: true},  // Consume 1 (Need to trigger replenish first)
				{cost: 1.0, expectAllow: true},  // Consume 2
				{cost: 1.0, expectAllow: true},  // Consume 3
				{cost: 1.0, expectAllow: true},  // Consume 4
				{cost: 1.0, expectAllow: true},  // Consume 5
				{cost: 1.0, expectAllow: false}, // Capacity is exceeded
			},
		},
		{
			name:          "cost_parameter/handles_variable_costs",
			initialRate:   10.0,
			burstDuration: 200 * time.Millisecond, // Capacity = 10 * 0.2 = 2.0
			actions: []action{
				{advanceTime: 1 * time.Second},                                     // Fill bucket to capacity of 2.0
				{cost: 1.5, expectAllow: true},                                     // Should succeed, 0.5 tokens remain
				{advanceTime: 10 * time.Millisecond, cost: 0.6, expectAllow: true}, // 0.5 + (10 * 0.01) = 0.6. Should pass.
				{cost: 0.1, expectAllow: false},                                    // Bucket is now empty
			},
		},
		{
			name:          "edge_case/zero_and_negative_cost_always_allowed",
			initialRate:   1.0,
			burstDuration: 100 * time.Millisecond, // Capacity = 1.0
			actions: []action{
				{cost: 1.0, expectAllow: true},  // Empty the bucket
				{cost: 0.0, expectAllow: true},  // Zero cost should be allowed
				{cost: -1.0, expectAllow: true}, // Negative cost should be allowed
				{cost: 1.0, expectAllow: false}, // Bucket should still be empty
			},
		},
		{
			name:          "edge_case/clock_skew_backwards_is_safe",
			initialRate:   10,
			burstDuration: 1 * time.Second, // Capacity = 10
			actions: []action{
				{advanceTime: 1 * time.Second},                                      // Fill bucket to 10
				{cost: 1.0, expectAllow: true},                                      // Tokens are now 9 (after replenishment)
				{advanceTime: -2 * time.Second},                                     // Clock goes backward!
				{cost: 1.0, expectAllow: true},                                      // Should still allow, tokens are 9 (no replenishment)
				{cost: 8.0, expectAllow: true},                                      // Should consume rest of tokens. Tokens are 0.
				{cost: 1.0, expectAllow: false},                                     // Should fail, bucket is empty and no time passed since last update
				{advanceTime: 2 * time.Second},                                      // Clock goes back to original time
				{advanceTime: 100 * time.Millisecond, cost: 1.0, expectAllow: true}, // 0.1s * 10QPS = 1 token. Should pass.
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fakeClock := testingclock.NewFakeClock(time.Now())
			p := newTestPacer(tc.initialRate, tc.burstDuration, fakeClock)

			// In some tests, we advance time first to fill the bucket.
			// We must trigger a replenish to make this take effect. Allow(0) is a perfect no-op for this.
			if len(tc.actions) > 0 && tc.actions[0].advanceTime > 0 {
				fakeClock.Step(tc.actions[0].advanceTime)
				p.Allow(0)
			}

			// Act & Assert
			for i, action := range tc.actions {
				// Time advancement for the first action is handled above.
				if i > 0 && action.advanceTime != 0 {
					fakeClock.Step(action.advanceTime)
				}
				if action.cost != 0 {
					msg := fmt.Sprintf("action %d: expected Allow(%v) to be %v", i, action.cost, action.expectAllow)
					require.Equal(t, action.expectAllow, p.Allow(action.cost), msg)
				}
			}
		})
	}
}

func TestPacer_SetRate(t *testing.T) {
	t.Parallel()

	// Arrange
	type action struct {
		advanceTime  time.Duration
		allowCost    float64  // If > 0, call Allow
		setRate      *float64 // If not nil, call SetRate
		expectAllow  bool
		expectTokens *float64 // If not nil, assert internal token count
	}
	testCases := []struct {
		name          string
		initialRate   float64
		burstDuration time.Duration
		actions       []action
	}{
		{
			name:          "increase_rate/capacity_grows_and_tokens_are_preserved",
			initialRate:   2.0,
			burstDuration: 1 * time.Second, // Initial capacity = 2.0
			actions: []action{
				{advanceTime: 1 * time.Second},        // Fill to 2.0 tokens
				{allowCost: 1.5, expectAllow: true},   // 0.5 tokens remain
				{setRate: floatPtr(10.0)},             // New capacity = 10.0
				{expectTokens: floatPtr(0.5)},         // Tokens should be unchanged
				{advanceTime: 100 * time.Millisecond}, // Add 10.0 * 0.1 = 1.0 token
				{expectTokens: floatPtr(1.5)},         // Tokens should now be 1.5
			},
		},
		{
			name:          "decrease_rate/tokens_are_capped_at_new_lower_capacity",
			initialRate:   10.0,
			burstDuration: 1 * time.Second, // Initial capacity = 10.0
			actions: []action{
				{advanceTime: 1 * time.Second}, // Fill to 10.0 tokens
				{expectTokens: floatPtr(10.0)},
				{setRate: floatPtr(2.0)},      // New capacity = 2.0
				{expectTokens: floatPtr(2.0)}, // Tokens are immediately capped
				{allowCost: 2.0, expectAllow: true},
				{allowCost: 0.1, expectAllow: false}, // No tokens left
			},
		},
		{
			name:          "smooth_transition/replenishes_at_old_rate_before_switch",
			initialRate:   10.0,
			burstDuration: 1 * time.Second, // Initial capacity = 10.0
			actions: []action{
				{allowCost: 1.0, expectAllow: true},   // Use initial token, 0 left.
				{advanceTime: 500 * time.Millisecond}, // Time passes, but no replenishment call yet. 5 tokens are due.
				{setRate: floatPtr(1.0)},              // setRate call triggers replenishment at OLD rate.
				// Tokens become 0 + (10.0 * 0.5) = 5.0.
				// THEN, new capacity is 1.0, so tokens are capped at 1.0.
				{expectTokens: floatPtr(1.0)},
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fakeClock := testingclock.NewFakeClock(time.Now())
			pacer := newTestPacer(tc.initialRate, tc.burstDuration, fakeClock)

			// Act & Assert
			for i, action := range tc.actions {
				if action.advanceTime != 0 {
					fakeClock.Step(action.advanceTime)
					forceReplenish(pacer)
				}

				// After any time advancement or action that could change tokens, we must trigger a replenish before asserting
				// the token count.
				// Allow(0), SetRate, or a real Allow call will do this.
				if action.setRate != nil {
					pacer.SetRate(*action.setRate)
				}
				if action.allowCost > 0 {
					msg := fmt.Sprintf("action %d: expected Allow(%v) to be %v", i, action.allowCost, action.expectAllow)
					require.Equal(t, action.expectAllow, pacer.Allow(action.allowCost), msg)
				}

				if action.expectTokens != nil {
					pacer.mu.Lock()
					// Use a small delta for float comparison to avoid precision issues.
					msg := fmt.Sprintf("action %d: unexpected token count", i)
					require.InDelta(t, *action.expectTokens, pacer.tokens, 0.0001, msg)
					pacer.mu.Unlock()
				}
			}
		})
	}
}

// floatPtr is a test helper to easily create a pointer to a float64 literal.
func floatPtr(f float64) *float64 {
	return &f
}

func TestPacer_Concurrency(t *testing.T) {
	t.Parallel()

	// This is a targeted concurrency test to ensure thread-safety under load,
	// verifying that SetRate and Allow can be called concurrently without races
	// or incorrect behavior.
	const testDuration = 200 * time.Millisecond
	const allowRoutines = 16
	const setRateRoutines = 4
	const initialRate = 1000.0 // High rate to ensure most allows succeed

	// Arrange
	realClock := clock.RealClock{}
	pacer := NewPacer(initialRate, 10*time.Millisecond, realClock)
	var allowedCount atomic.Int64
	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), testDuration)
	defer cancel()

	// Act
	// Start goroutines that constantly try to get tokens
	for i := 0; i < allowRoutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					if pacer.Allow(1.0) {
						allowedCount.Add(1)
					}
					// Small sleep to prevent pure CPU spinning on failure, yielding to other goroutines.
					time.Sleep(100 * time.Microsecond)
				}
			}
		}()
	}

	// Start goroutines that constantly change the rate
	for i := 0; i < setRateRoutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rate := initialRate
			for {
				select {
				case <-ctx.Done():
					return
				default:
					// Fluctuate the rate up and down
					rate = initialRate + math.Sin(float64(realClock.Now().UnixNano())/1e9)*500
					pacer.SetRate(rate)
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()

	// Assert
	// The main goal is to ensure this test passes with the -race flag.
	// The assertion below is a sanity check that the pacer allowed a reasonable
	// number of requests, not a precise count.
	totalExpected := initialRate * testDuration.Seconds()
	// We expect the count to be in a reasonable range of the target.
	// It won't be exact due to goroutine scheduling and rate fluctuations.
	// We check for at least 75% of the expected minimum to account for contention.
	minExpected := totalExpected * 0.75
	finalAllowed := allowedCount.Load()

	require.Greater(t, float64(finalAllowed), minExpected,
		"Pacer should allow a significant number of requests under contention. Allowed: %d, Expected > %f", finalAllowed, minExpected)
	t.Logf("Concurrency test passed. Allowed %d requests in %v.", finalAllowed, testDuration)
}

func BenchmarkPacer_Allow(b *testing.B) {
	// High rate to ensure the benchmark measures lock contention and overhead of the Allow call,
	// rather than the throttling behavior.
	pacer := NewPacer(1_000_000, 10*time.Millisecond, clock.RealClock{})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			pacer.Allow(1.0)
		}
	})
}
