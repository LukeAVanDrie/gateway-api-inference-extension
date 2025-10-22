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
	"fmt"
	"sort"
)

// giniCoefficient calculates the Gini coefficient for a given distribution of non-negative values.
// It returns a value between 0.0 (perfect equality, where all values are the same) and 1.0 (maximal inequality, where
// single value holds the entire sum).
//
// The function uses an efficient O(n log n) algorithm based on the sorted values of the distribution, making it
// suitable for periodic observability calculations.
// For more details on the formula, see: https://en.wikipedia.org/wiki/Gini_coefficient
//
// An error is returned if the distribution contains any negative values, as the Gini coefficient is undefined for such
// cases. The input slice is never mutated.
func giniCoefficient(distribution []float64) (float64, error) {
	n := len(distribution)
	if n < 2 {
		// A single value or an empty set has no inequality by definition.
		return 0.0, nil
	}

	// The Gini coefficient calculation requires a sorted distribution.
	// A copy is made to ensure the caller's slice is not mutated.
	sorted := make([]float64, n)
	copy(sorted, distribution)
	sort.Float64s(sorted)

	var sumOfValues float64
	var weightedSumOfValues float64
	for i, v := range sorted {
		if v < 0 {
			// This is a contract violation; the Gini coefficient is only meaningful for non-negative quantities.
			return 0.0, fmt.Errorf("gini coefficient is undefined for distributions with negative values (found: %f)", v)
		}
		sumOfValues += v
		// The formula uses a 1-based index (i+1) for the weight of each sorted value.
		weightedSumOfValues += float64(i+1) * v
	}

	if sumOfValues == 0 {
		// If all values are zero, there is perfect equality.
		return 0.0, nil
	}

	nFloat := float64(n)
	// This is the computationally efficient formula for the Gini coefficient:
	// G = (2 * Σ(i * yᵢ) / (n * Σyᵢ)) - (n+1)/n
	gini := (2.0*weightedSumOfValues)/(nFloat*sumOfValues) - (nFloat+1.0)/nFloat
	return gini, nil
}
