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

package estimators

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	testclock "k8s.io/utils/clock/testing"
)

func TestRateEWMA(t *testing.T) {
	t.Parallel()

	// testStep defines a single action to perform on the RateEWMA and the expected outcome.
	type testStep struct {
		// Action to perform for this step.
		advance       time.Duration // Advance the clock by this much BEFORE other actions.
		add           float64       // If non-zero, call Add() with this value.
		expectedRate  float64
		expectedCount float64
	}

	// Use a fixed, easy-to-reason-about window for all tests.
	const testWindow = 10 * time.Second
	// Pre-calculate expected decay factors for common scenarios.
	decayOverHalfWindow := math.Exp(-0.5) // 5s / 10s
	decayOverOneWindow := math.Exp(-1.0)  // 10s / 10s

	testCases := []struct {
		name  string
		steps []testStep
	}{
		{
			name: "initial_state_is_zero",
			steps: []testStep{
				{advance: 1 * time.Minute, expectedRate: 0.0, expectedCount: 0.0},
			},
		},
		{
			name: "first_add_sets_initial_rate_and_count",
			steps: []testStep{
				{add: 50.0, expectedRate: 5.0, expectedCount: 1.0}, // 50.0 / 10s = 5.0 rate
			},
		},
		{
			name: "state_decays_correctly_over_half_window",
			steps: []testStep{
				{add: 100.0, expectedRate: 10.0, expectedCount: 1.0},
				{advance: testWindow / 2, expectedRate: 10.0 * decayOverHalfWindow, expectedCount: 1.0 * decayOverHalfWindow},
			},
		},
		{
			name: "state_decays_correctly_over_one_window",
			steps: []testStep{
				{add: 100.0, expectedRate: 10.0, expectedCount: 1.0},
				{advance: testWindow, expectedRate: 10.0 * decayOverOneWindow, expectedCount: 1.0 * decayOverOneWindow},
			},
		},
		{
			name: "state_decays_to_zero_over_long_duration",
			steps: []testStep{
				{add: 1000.0, expectedRate: 100.0, expectedCount: 1.0},
				{advance: testWindow * 100, expectedRate: 0.0, expectedCount: 0.0}, // Effectively zero
			},
		},
		{
			name: "multiple_adds_accumulate_with_decay",
			steps: []testStep{
				{add: 100.0, expectedRate: 10.0, expectedCount: 1.0}, // Internal value=100, count=1
				{
					advance:       testWindow,
					add:           50.0,                                                     // Decayed value = 100*exp(-1), decayed count = 1*exp(-1)
					expectedRate:  (100.0*decayOverOneWindow + 50.0) / testWindow.Seconds(), // New total value / window
					expectedCount: 1.0*decayOverOneWindow + 1.0,                             // New total count
				},
			},
		},
		{
			name: "reset_clears_state_and_resets_time",
			steps: []testStep{
				{add: 100.0, expectedRate: 10.0, expectedCount: 1.0},
				{
					advance:       testWindow, // Let it decay.
					expectedRate:  10.0 * decayOverOneWindow,
					expectedCount: 1.0 * decayOverOneWindow,
				},
				{
					// This step will perform the reset. The Add is just a sentinel value to force a state check.
					add:           -1, // Special value to trigger a reset in the test runner.
					expectedRate:  0.0,
					expectedCount: 0.0,
				},
				// After reset, it should behave as if new.
				{add: 20.0, expectedRate: 2.0, expectedCount: 1.0},
			},
		},
		{
			name: "add_with_past_timestamp_does_not_decay",
			steps: []testStep{
				{add: 100.0, expectedRate: 10.0, expectedCount: 1.0},
				{
					advance:       -5 * time.Second, // Go backwards in time.
					add:           50.0,             // This should NOT decay the previous value.
					expectedRate:  15.0,             // (100.0 + 50.0) / 10s
					expectedCount: 2.0,
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clock := testclock.NewFakeClock(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
			ewma := NewRateEWMA(testWindow, clock.Now())

			for i, step := range tc.steps {
				msg := fmt.Sprintf("test '%s', step %d", tc.name, i)

				if step.advance != 0 {
					clock.Step(step.advance)
				}

				// Special case for reset test.
				if step.add == -1 {
					ewma.Reset(clock.Now())
				} else if step.add != 0 {
					ewma.Add(clock.Now(), step.add)
				}

				rate := ewma.Rate(clock.Now())
				count := ewma.Count(clock.Now())

				const delta = 1e-9 // Use a small delta for float comparisons.
				require.InDelta(t, step.expectedRate, rate, delta, "%s: Rate() mismatch", msg)
				require.InDelta(t, step.expectedCount, count, delta, "%s: Count() mismatch", msg)
			}
		})
	}
}

func BenchmarkRateEWMA_Add(b *testing.B) {
	now := time.Now()
	ewma := NewRateEWMA(10*time.Second, now)
	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		// Simulate a steady stream of events arriving one second apart.
		now = now.Add(time.Second)
		ewma.Add(now, float64(i))
	}
}
