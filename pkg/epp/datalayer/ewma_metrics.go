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
	"sync"
	"time"
)

const (
	// EWMAAlpha is the smoothing factor for the Exponentially Weighted Moving Average.
	// A value of 0.1 provides a good balance between responsiveness to recent changes and stability against noise.
	EWMAAlpha = 0.1
)

// EWMAMetrics holds calculated Exponentially Weighted Moving Average metrics for a pod.
// These metrics are updated on each request lifecycle event and form the inputs for the SaturationDetector's queuing
// models. The implementation is safe for concurrent access.
type EWMAMetrics struct {
	mu sync.RWMutex

	// --- Sojourn Time Metrics (Effective Service Time) ---
	MeanSojournTimeEWMA     time.Duration // E[S]. EWMA of the mean sojourn time.
	VarianceSojournTimeEWMA float64       // Var(S). EWMA of the variance of sojourn time (in seconds squared).
	m2SojournTimeEWMA       float64       // Internal state for EWMV calculation (Welford's adaptation).
	sojournTimeSamples      int64         // Count of samples contributing to the current EWMA.

	// --- Arrival Rate Metrics ---
	// We track the EWMA of the *inter-arrival times* (E[T]) rather than the instantaneous rates (1/T).
	// This is mathematically more robust against high variance, preventing short bursts (T->0) from causing massive
	// spikes (1/T->∞) in the EWMA. The arrival rate (λ) is derived as 1 / E[T].
	MeanInterArrivalTimeEWMA time.Duration // E[T]. EWMA of the time between arrivals.
	ArrivalRateEWMA          float64       // λ = 1 / E[T] (requests per second).
	lastArrivalTime          time.Time     // Timestamp of the previous arrival.
	arrivalSamples           int64
}

// NewEWMAMetrics creates a new EWMAMetrics instance.
func NewEWMAMetrics() *EWMAMetrics {
	return &EWMAMetrics{}
}

// UpdateArrivalMetrics updates the EWMA of the inter-arrival time and recalculates the arrival rate.
// This should be called immediately upon a request being dispatched to the pod (PreRequest hook).
// It returns the newly calculated arrival rate (λ) for safe logging.
func (m *EWMAMetrics) UpdateArrivalMetrics(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.arrivalSamples++

	if !m.lastArrivalTime.IsZero() {
		interArrivalTime := now.Sub(m.lastArrivalTime)

		// Update Mean Inter-Arrival Time EWMA (E[T]).
		if m.arrivalSamples <= 1 {
			m.MeanInterArrivalTimeEWMA = interArrivalTime
		} else {
			// Standard EWMA formula applied to the inter-arrival time.
			// NewMean = OldMean + Alpha * (NewValue - OldMean)
			delta := interArrivalTime - m.MeanInterArrivalTimeEWMA
			m.MeanInterArrivalTimeEWMA += time.Duration(float64(delta) * EWMAAlpha)
		}

		// Recalculate Arrival Rate (λ = 1 / E[T]).
		meanIATSeconds := m.MeanInterArrivalTimeEWMA.Seconds()
		if meanIATSeconds > 1e-9 { // Avoid division by zero or extremely small intervals.
			m.ArrivalRateEWMA = 1.0 / meanIATSeconds
		}
		// If E[T] is effectively zero (e.g., massive burst), the rate is very high. We retain the previous rate or let it
		// remain 0 if the system is cold, avoiding infinities.
	}

	m.lastArrivalTime = now
	return m.ArrivalRateEWMA
}

// UpdateSojournTimeEWMA updates the EWMA of the mean and variance of sojourn time using an adaptation of Welford's
// method (EWMVar).
// This should be called immediately after a response is received from the pod (PostResponse hook).
// It returns the newly calculated mean (E[S]) and variance (Var(S)) for safe logging.
func (m *EWMAMetrics) UpdateSojournTimeEWMA(sojournTime time.Duration) (time.Duration, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sojournTimeSamples++
	stSeconds := sojournTime.Seconds()

	if m.sojournTimeSamples == 1 {
		// Initialize state on the first sample.
		m.MeanSojournTimeEWMA = sojournTime
		m.m2SojournTimeEWMA = 0
		m.VarianceSojournTimeEWMA = 0
		return m.MeanSojournTimeEWMA, m.VarianceSojournTimeEWMA
	}

	// 1. Update Mean EWMA (E[S]).
	// Formula: NewMean = OldMean + Alpha * (NewValue - OldMean)
	delta := stSeconds - m.MeanSojournTimeEWMA.Seconds()
	newMeanSeconds := m.MeanSojournTimeEWMA.Seconds() + EWMAAlpha*delta
	m.MeanSojournTimeEWMA = time.Duration(newMeanSeconds * float64(time.Second))

	// 2. Update Variance EWMA (Var(S)).
	// Formula: EWMV = (1-Alpha) * EWMV_{old} + Alpha * (x_n - Mean_{old}) * (x_n - Mean_{new})
	delta2 := stSeconds - newMeanSeconds
	m.m2SojournTimeEWMA = (1-EWMAAlpha)*m.m2SojournTimeEWMA + EWMAAlpha*delta*delta2
	// Variance is represented by M2 in this EWMA adaptation.
	m.VarianceSojournTimeEWMA = m.m2SojournTimeEWMA

	return m.MeanSojournTimeEWMA, m.VarianceSojournTimeEWMA
}

// GetMeanSojournTimeEWMA returns the EWMA of the mean sojourn time (E[S]).
func (m *EWMAMetrics) GetMeanSojournTimeEWMA() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.MeanSojournTimeEWMA
}

// GetVarianceSojournTimeEWMA returns the EWMA of the variance of sojourn time (Var(S)).
func (m *EWMAMetrics) GetVarianceSojournTimeEWMA() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.VarianceSojournTimeEWMA
}

// GetArrivalRateEWMA returns the EWMA of the arrival rate (λ).
func (m *EWMAMetrics) GetArrivalRateEWMA() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ArrivalRateEWMA
}
