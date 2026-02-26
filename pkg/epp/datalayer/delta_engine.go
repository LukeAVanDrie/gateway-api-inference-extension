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

// Bucket represents a single cumulative histogram bucket.
type Bucket struct {
	UpperBound float64 // The 'le' (less-than-or-equal) boundary. +Inf is math.Inf(1)
	Count      uint64  // Cumulative count of observations <= UpperBound
}

// HistogramSnapshot represents a raw cumulative scrape from the vLLM /metrics endpoint.
type HistogramSnapshot struct {
	Buckets []Bucket // MUST be ordered by UpperBound ascending
	Count   uint64   // Total observations
	Sum     float64  // Sum of all observations
}

// EpochSnapshot holds the raw cumulative counters at a specific point in time.
type EpochSnapshot struct {
	Timestamp             time.Time // Upgraded to native Go time
	TPOTHistogram         HistogramSnapshot
	TTFTHistogram         HistogramSnapshot
	PrefillHistogram      HistogramSnapshot
	GenerationTokensTotal uint64
	RequestSuccessTotal   uint64
}

// EpochDelta is the cleanly computed "shopping list" handed directly to the Phase 4 Auto-Tuner.
type EpochDelta struct {
	P90TPOT             float64
	P50TTFT             float64
	P50Prefill          float64
	ThroughputTokensSec float64 // Tokens generated per second during this epoch
	DeltaRequestSuccess uint64  // Used by the Auto-Tuner to determine statistical confidence
	Duration            time.Duration
}

// PodDeltaEngine maintains the temporal boundaries and calculates exact epoch math for a single endpoint.
type PodDeltaEngine struct {
	mu          sync.Mutex
	lastEpoch   EpochSnapshot
	epochWindow time.Duration // Configurable target (e.g., 2 * time.Second)
}

// NewPodDeltaEngine initializes the time-series bridge for a newly discovered pod.
func NewPodDeltaEngine(epochWindow time.Duration) *PodDeltaEngine {
	return &PodDeltaEngine{
		epochWindow: epochWindow,
	}
}

// UpdateScrape is called every 500ms by the Extractor plugin.
func (e *PodDeltaEngine) UpdateScrape(scrape EpochSnapshot) *EpochDelta {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Handle absolute initialization
	if e.lastEpoch.Timestamp.IsZero() {
		e.lastEpoch = scrape
		return nil
	}

	// 1. Defend against Prometheus Counter Resets (Pod Restarts)
	// If any cumulative counter goes backward, the backend rebooted.
	// We ignore strict transition TO zero, as that may indicate a scrape omission/error.
	isReset := (scrape.GenerationTokensTotal < e.lastEpoch.GenerationTokensTotal && scrape.GenerationTokensTotal > 0) ||
		(scrape.RequestSuccessTotal < e.lastEpoch.RequestSuccessTotal && scrape.RequestSuccessTotal > 0) ||
		(scrape.TPOTHistogram.Count < e.lastEpoch.TPOTHistogram.Count && scrape.TPOTHistogram.Count > 0) ||
		(scrape.TTFTHistogram.Count < e.lastEpoch.TTFTHistogram.Count && scrape.TTFTHistogram.Count > 0) ||
		(scrape.PrefillHistogram.Count < e.lastEpoch.PrefillHistogram.Count && scrape.PrefillHistogram.Count > 0)

	if isReset {
		// Reset our baseline and wait for the next tumbling window.
		e.lastEpoch = scrape
		return nil
	}

	elapsed := scrape.Timestamp.Sub(e.lastEpoch.Timestamp)

	// If the tumbling window hasn't elapsed, silently absorb the scrape and wait.
	if elapsed < e.epochWindow {
		return nil
	}

	// 2. Calculate the actual physical math over the delta
	delta := &EpochDelta{
		P90TPOT:             CalculateQuantile(0.90, e.lastEpoch.TPOTHistogram, scrape.TPOTHistogram),
		P50TTFT:             CalculateQuantile(0.50, e.lastEpoch.TTFTHistogram, scrape.TTFTHistogram),
		P50Prefill:          CalculateQuantile(0.50, e.lastEpoch.PrefillHistogram, scrape.PrefillHistogram),
		DeltaRequestSuccess: scrape.RequestSuccessTotal - e.lastEpoch.RequestSuccessTotal,
		Duration:            elapsed,
	}

	// 3. Calculate Token Throughput (Tokens per Second)
	// Using elapsed.Seconds() natively handles floating point conversion and jitter.
	deltaTokens := scrape.GenerationTokensTotal - e.lastEpoch.GenerationTokensTotal
	delta.ThroughputTokensSec = float64(deltaTokens) / elapsed.Seconds()

	// 4. Tumble the window strictly forward
	e.lastEpoch = scrape

	return delta
}

// CalculateQuantile computes a generic percentile (e.g., 0.90 for P90) over a delta histogram.
func CalculateQuantile(q float64, oldSnap, newSnap HistogramSnapshot) float64 {
	// Defend against mismatched scrape topologies (e.g., vLLM upgrades mid-flight or malformed JSON)
	if len(oldSnap.Buckets) != len(newSnap.Buckets) {
		return 0.0
	}

	deltaTotalCount := newSnap.Count - oldSnap.Count
	if deltaTotalCount == 0 {
		return 0.0 // No traffic in this epoch
	}

	rank := q * float64(deltaTotalCount)

	var prevDeltaCumCount uint64 = 0
	var lowerBound float64 = 0.0

	for i := 0; i < len(newSnap.Buckets); i++ {
		upperBound := newSnap.Buckets[i].UpperBound
		currDeltaCumCount := newSnap.Buckets[i].Count - oldSnap.Buckets[i].Count

		if float64(currDeltaCumCount) >= rank {
			countInBucket := currDeltaCumCount - prevDeltaCumCount

			if countInBucket == 0 {
				return lowerBound
			}

			if math.IsInf(upperBound, 1) {
				return lowerBound
			}

			bucketWidth := upperBound - lowerBound
			rankInBucket := rank - float64(prevDeltaCumCount)

			interpolatedValue := lowerBound + ((rankInBucket / float64(countInBucket)) * bucketWidth)
			return interpolatedValue
		}

		lowerBound = upperBound
		prevDeltaCumCount = currDeltaCumCount
	}

	if len(newSnap.Buckets) > 0 {
		lastIdx := len(newSnap.Buckets) - 1
		if math.IsInf(newSnap.Buckets[lastIdx].UpperBound, 1) && lastIdx > 0 {
			return newSnap.Buckets[lastIdx-1].UpperBound
		}
		return newSnap.Buckets[lastIdx].UpperBound
	}

	return 0.0
}
