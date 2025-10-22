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

package observer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGiniCoefficient(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		distribution []float64
		expectedGini float64
		expectErr    bool
		errContains  string
		tolerance    float64
	}{
		{
			name:         "empty distribution",
			distribution: []float64{},
			expectedGini: 0.0,
		},
		{
			name:         "single value distribution",
			distribution: []float64{100},
			expectedGini: 0.0,
		},
		{
			name:         "perfect equality (all same values)",
			distribution: []float64{10, 10, 10, 10},
			expectedGini: 0.0,
		},
		{
			name:         "perfect equality (all zero)",
			distribution: []float64{0, 0, 0, 0},
			expectedGini: 0.0,
		},
		{
			name:         "maximal inequality (one value has everything)",
			distribution: []float64{0, 0, 0, 100},
			expectedGini: 0.75, // (n-1)/n = 3/4
		},
		{
			name:         "simple two-value inequality",
			distribution: []float64{0, 10},
			expectedGini: 0.5, // (n-1)/n = 1/2
		},
		{
			name:         "textbook example distribution",
			distribution: []float64{1, 2, 3, 4, 5},
			expectedGini: 0.26666,
			tolerance:    0.00001,
		},
		{
			name:         "should not mutate input slice",
			distribution: []float64{5, 2, 3, 1, 4},
			expectedGini: 0.26666,
			tolerance:    0.00001,
		},
		{
			name:         "error on negative values",
			distribution: []float64{1, 2, -3, 4, 5},
			expectErr:    true,
			errContains:  "negative values",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a copy to verify non-mutation.
			original := make([]float64, len(tc.distribution))
			copy(original, tc.distribution)

			gini, err := giniCoefficient(tc.distribution)

			if tc.expectErr {
				require.Error(t, err, "expected an error")
				require.ErrorContains(t, err, tc.errContains, "error message mismatch")
			} else {
				require.NoError(t, err, "expected no error")
				require.InDelta(t, tc.expectedGini, gini, tc.tolerance, "gini coefficient mismatch")
				require.Equal(t, original, tc.distribution, "input slice should not be mutated")
			}
		})
	}
}
