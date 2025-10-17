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

package datalayer

import (
	"math"
	"sync"
	"time"
)

const (
	// EWMAAlpha is the smoothing factor for the Sojourn Time Mean and Variance Exponentially Weighted Moving Averages.
	// A value of 0.1 provides a good balance between responsiveness and noise reduction.
	EWMAAlpha = 0.1

	// ArrivalRateEWMAWindow defines the effective time window over which the arrival rate EWMA is calculated.
	// The rate decays automatically over time. A shorter window is more responsive but noisier.
	// 5 seconds is a reasonable default for stabilizing the arrival rate signal.
	ArrivalRateEWMAWindow = 5 * time.Second
)

// EWMAMetrics holds calculated Exponentially Weighted Moving Average metrics for a pod.
// These metrics are updated on each request lifecycle event and form the inputs for the SaturationDetector's queuing
// models. The implementation is safe for concurrent access.
type EWMAMetrics struct {
	mu sync.RWMutex

	// --- Sojourn Time Metrics (Effective Service Time) ---
	// Calculated using standard EWMA and Welford's method adapted for EWMA.
	MeanSojournTimeEWMA     time.Duration // EWMA of the mean sojourn time (E[S])
	VarianceSojournTimeEWMA float64       // EWMA of the variance of sojourn time (Var[S], in seconds squared)
	m2SojournTimeEWMA       float64       // Internal state for Welford's method (sum of squares of differences)
	sojournTimeSamples      int64         // Count of samples contributing to the current EWMA

	// --- Arrival Rate Metrics (Time-Aware Decaying EWMA) ---
	// Calculated using a time-aware decaying EWMA to ensure the rate decays when arrivals stop (preventing lockout).
	ArrivalRateRawEWMA float64   // Raw EWMA value (weighted count)
	lastRateUpdate     time.Time // Timestamp of the last update (arrival event)
}

// NewEWMAMetrics creates a new EWMAMetrics instance.
func NewEWMAMetrics() *EWMAMetrics {
	return &EWMAMetrics{}
}

// UpdateArrivalRateEWMA updates the arrival rate using a time-aware decaying EWMA.
// This should be called immediately upon a request being dispatched to the pod (PreRequest hook).
// It returns the newly calculated arrival rate (λ) for safe logging.
func (m *EWMAMetrics) UpdateArrivalRateEWMA(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	// The weight (increment) added by a single new request.
	const increment = 1.0
	var currentRaw float64
	if !m.lastRateUpdate.IsZero() {
		currentRaw = m.getDecayedRawRateLocked(now)
	}

	// Apply decay and add the increment for the new arrival.
	// Raw_new = Raw_old * decay + increment
	m.ArrivalRateRawEWMA = currentRaw + increment
	m.lastRateUpdate = now
	return m.normalizeRate(m.ArrivalRateRawEWMA)
}

// UpdateSojournTimeEWMA updates the EWMA of the mean and variance of sojourn time.
// It uses Welford's method adapted for EWMA for numerical stability.
// This should be called immediately after a response is received from the pod (PostResponse hook).
// It returns the newly calculated mean (E[S]) and variance (Var(S)) for safe logging.
func (m *EWMAMetrics) UpdateSojournTimeEWMA(sojournTime time.Duration) (time.Duration, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sojournTimeSamples++
	stSeconds := sojournTime.Seconds()

	if m.sojournTimeSamples == 1 {
		// Initialize EWMA with the first sample.
		m.MeanSojournTimeEWMA = sojournTime
		m.m2SojournTimeEWMA = 0
		m.VarianceSojournTimeEWMA = 0
		return m.MeanSojournTimeEWMA, m.VarianceSojournTimeEWMA
	}

	// Update Mean EWMA.
	// NewMean = OldMean + α*(NewSample - OldMean)
	delta := stSeconds - m.MeanSojournTimeEWMA.Seconds()
	newMeanSeconds := m.MeanSojournTimeEWMA.Seconds() + EWMAAlpha*delta
	m.MeanSojournTimeEWMA = time.Duration(newMeanSeconds * float64(time.Second))

	// Update Variance EWMA (Welford's adapted method).
	// M2_new = (1-α)*M2_old + α*(x_k - M_{k-1})*(x_k - M_k)
	// where delta = (x_k - M_{k-1}) and delta2 = (x_k - M_k)
	delta2 := stSeconds - newMeanSeconds
	m.m2SojournTimeEWMA = (1-EWMAAlpha)*m.m2SojournTimeEWMA + EWMAAlpha*delta*delta2
	// The variance estimate is the smoothed M2 value.
	m.VarianceSojournTimeEWMA = m.m2SojournTimeEWMA
	return m.MeanSojournTimeEWMA, m.VarianceSojournTimeEWMA
}

// GetMeanSojournTimeEWMA returns the EWMA of the mean sojourn time (E[S]).
func (m *EWMAMetrics) GetMeanSojournTimeEWMA() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.MeanSojournTimeEWMA
}

// GetVarianceSojournTimeEWMA returns the EWMA of the variance of sojourn time (Var[S]).
func (m *EWMAMetrics) GetVarianceSojournTimeEWMA() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Ensure variance is non-negative (can slightly dip below zero due to floating point inaccuracies).
	if m.VarianceSojournTimeEWMA < 0 {
		return 0
	}
	return m.VarianceSojournTimeEWMA
}

// GetArrivalRateEWMA returns the current EWMA of the arrival rate (λ) in requests per second.
// It calculates the decayed rate based on the current time before returning the value.
func (m *EWMAMetrics) GetArrivalRateEWMA() float64 {
	// We use RLock.
	// We calculate the decayed rate without updating the internal state (ArrivalRateRawEWMA, lastRateUpdate).
	// This is a standard pattern for implementing decaying counters efficiently in concurrent systems, prioritizing read
	// performance.
	m.mu.RLock()
	defer m.mu.RUnlock()
	decayedRawRate := m.getDecayedRawRateLocked(time.Now())
	return m.normalizeRate(decayedRawRate)
}

// getDecayedRawRateLocked calculates the current decayed raw rate.
// It MUST be called under at least a read lock.
func (m *EWMAMetrics) getDecayedRawRateLocked(now time.Time) float64 {
	// Optimization: If the rate is zero or uninitialized, return 0.
	if m.ArrivalRateRawEWMA == 0 || m.lastRateUpdate.IsZero() {
		return 0
	}

	// Safety check in case window is configured near zero (should be prevented by config validation).
	window := ArrivalRateEWMAWindow.Seconds()
	if window <= 1e-9 {
		return m.ArrivalRateRawEWMA
	}

	// Calculate decay factor based on elapsed time (ΔT) and the configured window (W).
	// decay = exp(-ΔT / W)
	timeSinceLastUpdate := now.Sub(m.lastRateUpdate).Seconds()
	decay := math.Exp(-timeSinceLastUpdate / window)
	return m.ArrivalRateRawEWMA * decay
}

// normalizeRate converts the raw decayed value to requests per second.
func (m *EWMAMetrics) normalizeRate(rawRate float64) float64 {
	window := ArrivalRateEWMAWindow.Seconds()
	if window <= 1e-9 {
		return 0 // Avoid division by zero.
	}
	return rawRate / window
}
