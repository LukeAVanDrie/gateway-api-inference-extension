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
	"time"
)

// RateEWMA implements a time-aware Exponentially Weighted Moving Average for tracking both a value rate and an
// effective sample count.
//
// It is designed to track the rate of events (e.g., requests per second, cost per second, etc.) over a configurable
// time window. Its key feature is time-awareness: if no events are added, both the rate and the effective count
// naturally decay towards zero. This provides a more realistic "current" state compared to simple counters.
//
// The underlying mathematical model for decay is:
//
//	V_new = V_old * exp(-ΔT / W)
//
// The zero value of this struct is not usable. It must be initialized via NewRateEWMA.
// This type is NOT safe for concurrent access. The caller must provide external synchronization.
type RateEWMA struct {
	window           float64   // The EWMA time window in seconds.
	rawValue         float64   // The raw, un-normalized value accumulator.
	countAccumulator float64   // The raw, un-normalized count accumulator.
	lastUpdateTime   time.Time // The timestamp of the last Add() operation.
	startTime        time.Time // The creation time of the EWMA.
}

// NewRateEWMA creates a new, safely initialized RateEWMA.
//
//   - window: The time window over which the average is calculated. A larger window makes the rate smoother and less
//     responsive to short bursts.
//   - initialTime: The creation time of the EWMA, used to correctly handle the decay calculation for the very first
//     event.
func NewRateEWMA(window time.Duration, initialTime time.Time) *RateEWMA {
	return &RateEWMA{
		window:         window.Seconds(),
		lastUpdateTime: initialTime,
		startTime:      initialTime,
	}
}

// Add records a new event with a given value.
// The internal value and count accumulators are first decayed based on the time elapsed since the last call to Add, and
// then the new value and a count of 1 are added.
func (e *RateEWMA) Add(now time.Time, value float64) {
	decayFactor := e.calculateDecayFactor(now)
	e.rawValue = (e.rawValue * decayFactor) + value
	e.countAccumulator = (e.countAccumulator * decayFactor) + 1.0 // Each event adds 1 to the count.
	e.lastUpdateTime = now
}

// Rate returns the current rate in units per second.
// It calculates the rate based on the current time, ensuring the value has decayed appropriately.
func (e *RateEWMA) Rate(now time.Time) float64 {
	decayFactor := e.calculateDecayFactor(now)
	decayedValue := e.rawValue * decayFactor

	// Bias Correction:
	// If we haven't been running for a full window yet, normalize by the ACTUAL elapsed time.
	// We clamp to a small epsilon to avoid divide-by-zero.
	elapsed := now.Sub(e.startTime).Seconds()
	effectiveWindow := math.Min(e.window, math.Max(elapsed, 0.1))
	return decayedValue / effectiveWindow // The rate is the decayed total value normalized by the effective window size.
}

// Count returns the effective number of samples in the current window.
// This value represents the number of samples that would need to arrive at this exact moment to produce the same
// internal count accumulator.
//
// A higher number indicates that the current rate is based on more recent, significant data.
// It can be used to determine if the rate is "mature" enough to be trusted (e.g., trust Rate() only if Count() > 5.0).
func (e *RateEWMA) Count(now time.Time) float64 {
	decayFactor := e.calculateDecayFactor(now)
	return e.countAccumulator * decayFactor
}

// Reset clears all historical data and returns the RateEWMA to its initial state, as if it were newly created at the
// given time. The configured window is retained.
func (e *RateEWMA) Reset(now time.Time) {
	e.rawValue = 0.0
	e.countAccumulator = 0.0
	e.lastUpdateTime = now
}

// calculateDecayFactor computes the decay multiplier based on the time elapsed since the last update.
// This is a private helper to ensure both value and count decay by the exact same amount.
func (e *RateEWMA) calculateDecayFactor(now time.Time) float64 {
	// Protect against clock skew or out-of-order events, which could cause the rate to incorrectly grow instead of decay.
	elapsedSeconds := max(now.Sub(e.lastUpdateTime).Seconds(), 0)
	// The decay factor is derived from the continuous-time EWMA formula: exp(-ΔT / W).
	return math.Exp(-elapsedSeconds / e.window)
}
