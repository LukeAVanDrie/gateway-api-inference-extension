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
	EWMAAlpha = 0.1
)

// EWMAMetrics holds calculated Exponentially Weighted Moving Average metrics for a pod.
// These metrics are updated on each request lifecycle event.
type EWMAMetrics struct {
	mu                      sync.RWMutex
	MeanSojournTimeEWMA     time.Duration // EWMA of the mean sojourn time
	VarianceSojournTimeEWMA float64       // EWMA of the variance of sojourn time (in seconds squared)
	m2SojournTimeEWMA       float64       // EWMA of the sum of squares of differences from the mean
	sojournTimeSamples      int64         // Count of samples contributing to the current EWMA
	ArrivalRateEWMA         float64       // EWMA of the arrival rate (requests per second)
	lastArrivalTime         time.Time     // To calculate inter-arrival times
	arrivalSamples          int64
}

// NewEWMAMetrics creates a new EWMAMetrics instance.
func NewEWMAMetrics() *EWMAMetrics {
	return &EWMAMetrics{}
}

// UpdateArrivalRateEWMA updates the EWMA of the arrival rate.
func (m *EWMAMetrics) UpdateArrivalRateEWMA(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.arrivalSamples++
	if !m.lastArrivalTime.IsZero() {
		interArrivalTime := now.Sub(m.lastArrivalTime).Seconds()
		if interArrivalTime > 1e-9 { // Avoid division by zero or extremely small intervals.
			instantRate := 1.0 / interArrivalTime
			if m.arrivalSamples <= 1 {
				m.ArrivalRateEWMA = instantRate
			} else {
				m.ArrivalRateEWMA = EWMAAlpha*instantRate + (1-EWMAAlpha)*m.ArrivalRateEWMA
			}
		}
	}
	m.lastArrivalTime = now
}

// UpdateSojournTimeEWMA updates the EWMA of the mean and variance of sojourn time.
func (m *EWMAMetrics) UpdateSojournTimeEWMA(sojournTime time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sojournTimeSamples++
	stSeconds := sojournTime.Seconds()

	if m.sojournTimeSamples == 1 {
		m.MeanSojournTimeEWMA = sojournTime
		m.m2SojournTimeEWMA = 0
		m.VarianceSojournTimeEWMA = 0
		return
	}

	// Update Mean EWMA.
	delta := stSeconds - m.MeanSojournTimeEWMA.Seconds()
	newMeanSeconds := m.MeanSojournTimeEWMA.Seconds() + EWMAAlpha*delta
	m.MeanSojournTimeEWMA = time.Duration(newMeanSeconds * float64(time.Second))

	// Update Variance EWMA using Welford's method adapted for EWMA.
	delta2 := stSeconds - newMeanSeconds
	m.m2SojournTimeEWMA = (1-EWMAAlpha)*m.m2SojournTimeEWMA + EWMAAlpha*delta*delta2
	m.VarianceSojournTimeEWMA = m.m2SojournTimeEWMA
}

// GetMeanSojournTimeEWMA returns the EWMA of the mean sojourn time.
func (m *EWMAMetrics) GetMeanSojournTimeEWMA() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.MeanSojournTimeEWMA
}

// GetVarianceSojournTimeEWMA returns the EWMA of the variance of sojourn time.
func (m *EWMAMetrics) GetVarianceSojournTimeEWMA() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.VarianceSojournTimeEWMA
}

// GetArrivalRateEWMA returns the EWMA of the arrival rate.
func (m *EWMAMetrics) GetArrivalRateEWMA() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ArrivalRateEWMA
}
