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
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// roundBasedExpirationCheck provides a common, simple expiration logic for tests where the key is a monotonically
// increasing integer (a "round").
func roundBasedExpirationCheck(window, current, past uint64) bool {
	// Check for non-zero past to handle the initial key (0) correctly.
	if past == 0 {
		return false
	}
	return current > past && (current-past) >= window // The standard window check.
}

func TestWindowedExtremumFilter(t *testing.T) {
	t.Parallel()

	// testStep defines a single action (an update) and the expected observable state of the filter after that action is
	// performed.
	type testStep struct {
		key           uint64  // The key for the Update() call.
		value         float64 // The value for the Update() call.
		expectedValue float64 // The expected result of Get().
		expectedValid bool    // The expected validity from Get().
	}

	const (
		uninitializedMax = 0.0
		uninitializedMin = math.MaxFloat64
	)

	testCases := []struct {
		name         string
		constructor  func() *WindowedExtremumFilter[float64, uint64]
		steps        []testStep
		isMinVariant bool
	}{
		// --- Max Filter Test Cases ---
		{
			name: "max/initial_state_is_uninitialized",
			constructor: func() *WindowedExtremumFilter[float64, uint64] {
				return NewWindowedMaxFilter(10, 3, roundBasedExpirationCheck, uninitializedMax, 0)
			},
			steps: []testStep{
				// No updates, just check initial state.
				{key: 0, value: 0, expectedValue: uninitializedMax, expectedValid: false},
			},
		},
		{
			name: "max/new_absolute_extremum_resets_filter",
			constructor: func() *WindowedExtremumFilter[float64, uint64] {
				return NewWindowedMaxFilter(10, 3, roundBasedExpirationCheck, uninitializedMax, 0)
			},
			steps: []testStep{
				{key: 1, value: 100, expectedValue: 100, expectedValid: true},
				{key: 2, value: 90, expectedValue: 100, expectedValid: true},
				// This new max should reset all internal samples to 120.
				{key: 3, value: 120, expectedValue: 120, expectedValid: true},
				// Verify fallback works from the reset state: advance time to expire 120.
				{key: 13, value: 50, expectedValue: 50, expectedValid: true},
			},
		},
		{
			name: "max/fallback_samples_are_inserted_and_used",
			constructor: func() *WindowedExtremumFilter[float64, uint64] {
				return NewWindowedMaxFilter(10, 5, roundBasedExpirationCheck, uninitializedMax, 0)
			},
			steps: []testStep{
				{key: 1, value: 100, expectedValue: 100, expectedValid: true},
				{key: 2, value: 80, expectedValue: 100, expectedValid: true},
				{key: 3, value: 90, expectedValue: 100, expectedValid: true}, // Inserted between 100 and 80.
				{key: 4, value: 70, expectedValue: 100, expectedValid: true},
				{key: 5, value: 85, expectedValue: 100, expectedValid: true}, // Inserted between 90 and 80.
				// Expire the top value (100 @ key 1) by advancing key to 11.
				{key: 11, value: 50, expectedValue: 90, expectedValid: true}, // Should fall back to 90.
				// Expire the next value (90 @ key 3) by advancing key to 13.
				{key: 13, value: 50, expectedValue: 85, expectedValid: true}, // Should fall back to 85.
			},
		},
		{
			name: "max/cascading_expiration_resets_with_new_value",
			constructor: func() *WindowedExtremumFilter[float64, uint64] {
				return NewWindowedMaxFilter(10, 3, roundBasedExpirationCheck, uninitializedMax, 0)
			},
			steps: []testStep{
				{key: 1, value: 100, expectedValue: 100, expectedValid: true},
				{key: 2, value: 90, expectedValue: 100, expectedValid: true},
				{key: 3, value: 80, expectedValue: 100, expectedValid: true},
				// Advance key to 14, expiring all previous samples.
				// The filter becomes uninitialized, then the new value (50) is ingested, becoming the new extremum.
				{key: 14, value: 50, expectedValue: 50, expectedValid: true},
			},
		},
		{
			name: "max/equal_value_refreshes_key_not_resets",
			constructor: func() *WindowedExtremumFilter[float64, uint64] {
				return NewWindowedMaxFilter(10, 3, roundBasedExpirationCheck, uninitializedMax, 0)
			},
			steps: []testStep{
				{key: 1, value: 100, expectedValue: 100, expectedValid: true},
				{key: 2, value: 90, expectedValue: 100, expectedValid: true},
				// Advance key; without the refresh, 100 would expire at key 11.
				{key: 5, value: 100, expectedValue: 100, expectedValid: true},
				// Advance key past the original expiration window. It should not fall back.
				{key: 12, value: 50, expectedValue: 100, expectedValid: true},
			},
		},
		{
			name: "max/full_expiration_reverts_to_uninitialized",
			constructor: func() *WindowedExtremumFilter[float64, uint64] {
				return NewWindowedMaxFilter(10, 3, roundBasedExpirationCheck, uninitializedMax, 0)
			},
			steps: []testStep{
				{key: 1, value: 100, expectedValue: 100, expectedValid: true},
				// Advance key far into the future with a value that doesn't become the new max.
				{key: 12, value: uninitializedMax, expectedValue: uninitializedMax, expectedValid: false},
			},
		},

		// --- Min Filter Test Cases ---
		{
			name:         "min/new_absolute_extremum_resets_filter",
			isMinVariant: true,
			constructor: func() *WindowedExtremumFilter[float64, uint64] {
				return NewWindowedMinFilter(10, 3, roundBasedExpirationCheck, uninitializedMin, 0)
			},
			steps: []testStep{
				{key: 1, value: 100, expectedValue: 100, expectedValid: true},
				{key: 2, value: 110, expectedValue: 100, expectedValid: true},
				// This new min should reset all internal samples to 80.
				{key: 3, value: 80, expectedValue: 80, expectedValid: true},
				// Verify fallback works from the reset state: advance time to expire 80.
				{key: 13, value: 150, expectedValue: 150, expectedValid: true},
			},
		},
		{
			name:         "min/cascading_expiration_resets_with_new_value",
			isMinVariant: true,
			constructor: func() *WindowedExtremumFilter[float64, uint64] {
				return NewWindowedMinFilter(10, 3, roundBasedExpirationCheck, uninitializedMin, 0)
			},
			steps: []testStep{
				{key: 1, value: 80, expectedValue: 80, expectedValid: true},
				{key: 2, value: 90, expectedValue: 80, expectedValid: true},
				{key: 3, value: 100, expectedValue: 80, expectedValid: true},
				// Advance key to 14, expiring all previous samples.
				// The new value (120) becomes the new extremum.
				{key: 14, value: 120, expectedValue: 120, expectedValid: true},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			filter := tc.constructor()
			uninitializedVal := uninitializedMax
			if tc.isMinVariant {
				uninitializedVal = uninitializedMin
			}

			for i, step := range tc.steps {
				// For steps that don't perform an update, we still check state.
				if !(step.key == 0 && step.value == 0) {
					filter.Update(step.value, step.key)
				}

				gotValue, gotIsValid := filter.Get()

				require.Equal(t, step.expectedValid, gotIsValid,
					"Step %d: validity mismatch. After update with key=%d, value=%.2f",
					i, step.key, step.value)

				if step.expectedValid {
					require.InDelta(t, step.expectedValue, gotValue, 0.001,
						"Step %d: value mismatch. After update with key=%d, value=%.2f",
						i, step.key, step.value)
				} else {
					require.Equal(t, uninitializedVal, gotValue,
						"Step %d: value should be the uninitialized value when invalid",
						i)
				}
			}
		})
	}
}

func TestConstructorValidation(t *testing.T) {
	t.Parallel()

	t.Run("numSamples_is_zero_defaults_to_one", func(t *testing.T) {
		t.Parallel()
		filter := NewWindowedMaxFilter(10, 0, roundBasedExpirationCheck, 0.0, 0)
		require.NotNil(t, filter, "Constructor should not return nil even for zero samples")
		require.Len(t, filter.samples, 1, "A numSamples of 0 must default to a slice length of 1")
	})
}

// BenchmarkWindowedExtremumFilter_StableState measures the hot-path performance in a common scenario where the extremum
// value is stable and most updates are for secondary, non-extremum values.
func BenchmarkWindowedExtremumFilter_StableState(b *testing.B) {
	filter := NewWindowedMaxFilter(uint64(b.N), 5, roundBasedExpirationCheck, 0.0, 0)

	// Pre-populate a set of values with a clear maximum.
	values := []float64{100.0, 50.0, 60.0, 75.0, 95.0, 80.0}
	filter.Update(values[0], 0) // Set the initial max.

	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		// Cycle through the non-max values while continuously incrementing the key.
		filter.Update(values[(i%5)+1], uint64(i+1))
	}
}

// BenchmarkWindowedExtremumFilter_FrequentResets measures the "worst-case" performance where every update is a new
// absolute extremum, forcing a full reset of the filter's internal state.
func BenchmarkWindowedExtremumFilter_FrequentResets(b *testing.B) {
	filter := NewWindowedMaxFilter(uint64(b.N), 5, roundBasedExpirationCheck, 0.0, 0)

	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		// A workload where every value is a new max, triggering the reset path.
		filter.Update(float64(i), uint64(i))
	}
}
