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
	"golang.org/x/exp/constraints"
)

// Number is a constraint that permits any standard integer or floating-point type.
// It is used to improve the readability of generic function signatures.
type Number interface {
	constraints.Integer | constraints.Float
}

// EWMA implements a classic, non-decaying Exponentially Weighted Moving Average.
//
// This filter is designed to produce a smoothed average of a sequence of values.
// It does not decay on its own over time; it only updates when a new sample is added.
// It is ideal for smoothing signals where the interval between samples is irrelevant.
//
// The underlying mathematical model is:
//
//	Avg_n = α * Sample_n + (1 - α) * Avg_{n-1}
//
// Where:
//   - Avg is the current smoothed average.
//   - α (alpha) is the smoothing factor.
//
// The zero value of this struct is not usable. It must be initialized via NewEWMA.
// This type is NOT safe for concurrent access. The caller must provide external synchronization.
type EWMA[T Number] struct {
	alpha       float64 // The smoothing factor (α).
	value       float64 // The current smoothed average (Avg).
	hasSample   bool    // Tracks if at least one sample has been added.
	sampleCount uint64  // Tracks the total number of samples added.
}

// NewEWMA creates a new, safely initialized EWMA.
//
//   - alpha: The smoothing factor, which must be in the range (0, 1].
//     A higher alpha (e.g., 0.9) makes the average highly responsive to the latest samples.
//     A lower alpha (e.g., 0.1) makes the average smoother and more resistant to short-term fluctuations.
func NewEWMA[T Number](alpha float64) *EWMA[T] {
	return &EWMA[T]{alpha: alpha}
}

// Add incorporates a new sample into the smoothed average.
func (e *EWMA[T]) Add(sample T) {
	val := float64(sample)
	if !e.hasSample {
		e.value = val
		e.hasSample = true
	} else {
		e.value = (e.alpha * val) + ((1 - e.alpha) * e.value)
	}
	e.sampleCount++
}

// Get returns the current smoothed average.
// If no samples have been added, it returns the zero value of the type.
//
// The internal average is stored as a float64. If this EWMA was instantiated for an integer type, the returned value
// will be truncated (the fractional part will be discarded).
func (e *EWMA[T]) Get() T {
	if !e.hasSample {
		var zero T
		return zero
	}
	return T(e.value)
}

// HasSample returns true if at least one sample has been added to the EWMA.
func (e *EWMA[T]) HasSample() bool {
	return e.hasSample
}

// SampleCount returns the total number of samples that have been added to the EWMA since it was created or last reset.
func (e *EWMA[T]) SampleCount() uint64 {
	return e.sampleCount
}

// Reset clears all historical data and returns the EWMA to its initial, uninitialized state.
// The configured alpha value is retained.
func (e *EWMA[T]) Reset() {
	e.hasSample = false
	e.value = 0.0
	e.sampleCount = 0
}
