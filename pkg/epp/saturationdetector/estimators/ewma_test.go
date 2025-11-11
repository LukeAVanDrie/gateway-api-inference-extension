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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEWMA(t *testing.T) {
	t.Parallel()

	// expectedState defines the complete observable state of the EWMA at a specific point in time.
	type expectedState[T Number] struct {
		value       T
		hasSample   bool
		sampleCount uint64
	}

	// --- Test cases for float64 ---
	t.Run("float64", func(t *testing.T) {
		t.Parallel()

		testCases := []struct {
			name     string
			alpha    float64
			samples  []float64
			expected []expectedState[float64] // Expected state after each corresponding sample is added.
		}{
			{
				name:  "initial_state_is_zero_and_invalid",
				alpha: 0.5,
				// No samples added
				samples:  []float64{},
				expected: []expectedState[float64]{{value: 0.0, hasSample: false, sampleCount: 0}},
			},
			{
				name:     "first_sample_sets_initial_value",
				alpha:    0.5,
				samples:  []float64{100.0},
				expected: []expectedState[float64]{{value: 100.0, hasSample: true, sampleCount: 1}},
			},
			{
				name:    "alpha_of_1_has_no_memory",
				alpha:   1.0,
				samples: []float64{100.0, 50.0, 200.0},
				expected: []expectedState[float64]{
					{value: 100.0, hasSample: true, sampleCount: 1},
					{value: 50.0, hasSample: true, sampleCount: 2},
					{value: 200.0, hasSample: true, sampleCount: 3},
				},
			},
			{
				name:    "low_alpha_smooths_aggressively",
				alpha:   0.1, // High weight to historical average
				samples: []float64{10.0, 20.0, 30.0},
				expected: []expectedState[float64]{
					{value: 10.0, hasSample: true, sampleCount: 1}, // 10
					{value: 11.0, hasSample: true, sampleCount: 2}, // 0.1*20 + 0.9*10 = 2+9=11
					{value: 12.9, hasSample: true, sampleCount: 3}, // 0.1*30 + 0.9*11 = 3+9.9=12.9
				},
			},
			{
				name:    "high_alpha_is_highly_responsive",
				alpha:   0.9, // High weight to new samples
				samples: []float64{10.0, 20.0, 30.0},
				expected: []expectedState[float64]{
					{value: 10.0, hasSample: true, sampleCount: 1}, // 10
					{value: 19.0, hasSample: true, sampleCount: 2}, // 0.9*20 + 0.1*10 = 18+1=19
					{value: 28.9, hasSample: true, sampleCount: 3}, // 0.9*30 + 0.1*19 = 27+1.9=28.9
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				ewma := NewEWMA[float64](tc.alpha)

				// Check initial state if no samples are provided.
				if len(tc.samples) == 0 {
					want := tc.expected[0]
					require.Equal(t, want.hasSample, ewma.HasSample(), "Initial HasSample() state should be correct")
					require.Equal(t, want.sampleCount, ewma.SampleCount(), "Initial SampleCount() should be correct")
					require.InDelta(t, want.value, ewma.Get(), 0.001, "Initial Get() value should be correct")
					return
				}

				// Add samples and check state after each one.
				for i, sample := range tc.samples {
					ewma.Add(sample)
					want := tc.expected[i]
					msg := fmt.Sprintf("after adding sample #%d (%v)", i+1, sample)

					require.Equal(t, want.hasSample, ewma.HasSample(), "HasSample() state mismatch %s", msg)
					require.Equal(t, want.sampleCount, ewma.SampleCount(), "SampleCount() mismatch %s", msg)
					require.InDelta(t, want.value, ewma.Get(), 0.001, "Get() value mismatch %s", msg)
				}
			})
		}
	})

	// --- Test cases specifically for integer types ---
	t.Run("integer_truncation", func(t *testing.T) {
		t.Parallel()
		ewma := NewEWMA[int](0.5)

		// 1. Add 100 -> value = 100.0
		ewma.Add(100)
		require.Equal(t, int(100), ewma.Get(), "First integer sample should set the value directly")
		require.Equal(t, uint64(1), ewma.SampleCount(), "Sample count should be 1")

		// 2. Add 101 -> value = 0.5*101 + 0.5*100 = 50.5 + 50 = 100.5
		ewma.Add(101)
		require.Equal(t, int(100), ewma.Get(), "Get() for integer types must truncate the internal float64 value")
		require.Equal(t, uint64(2), ewma.SampleCount(), "Sample count should be 2")
	})

	// --- Test case for lifecycle methods ---
	t.Run("reset_clears_state", func(t *testing.T) {
		t.Parallel()
		ewma := NewEWMA[float64](0.5)

		// Arrange: Add data to put the EWMA into a used state.
		ewma.Add(100.0)
		ewma.Add(200.0)
		require.True(t, ewma.HasSample(), "Pre-condition: EWMA should have a sample")
		require.NotEqual(t, uint64(0), ewma.SampleCount(), "Pre-condition: Sample count should not be zero")

		// Act: Reset the EWMA.
		ewma.Reset()

		// Assert: The state is identical to a newly created instance.
		require.False(t, ewma.HasSample(), "HasSample() must be false after Reset()")
		require.Equal(t, uint64(0), ewma.SampleCount(), "SampleCount() must be 0 after Reset()")
		require.Equal(t, 0.0, ewma.Get(), "Get() must return the zero value after Reset()")

		// Act again: Ensure it works correctly after being reset.
		ewma.Add(50.0)
		require.True(t, ewma.HasSample(), "EWMA must be usable after Reset()")
		require.Equal(t, uint64(1), ewma.SampleCount(), "SampleCount() should start from 1 after Reset()")
		require.Equal(t, 50.0, ewma.Get(), "First sample after Reset() should set the value directly")
	})
}

func BenchmarkEWMA_Add(b *testing.B) {
	ewma := NewEWMA[float64](0.5)
	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		ewma.Add(float64(i))
	}
}
