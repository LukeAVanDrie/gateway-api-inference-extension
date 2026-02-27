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
	"slices"
	"sync"
	"time"
)

// Bucket represents a single cumulative histogram bucket returned by the inference engine.
// It reflects total observable resource latency within bounded windows.
type Bucket struct {
	UpperBound float64 // The 'le' (less-than-or-equal) boundary. +Inf is math.Inf(1)
	Count      uint64  // Cumulative count of observations <= UpperBound
}

// HistogramSnapshot represents a raw cumulative scrape from the inference engine's metrics endpoint.
// Buckets MUST be ordered by UpperBound in strictly ascending order for the interpolation in
// CalculateQuantile to yield mathematically valid latencies.
type HistogramSnapshot struct {
	Buckets []Bucket // MUST be ordered by UpperBound ascending
	Count   uint64   // Total observations
	Sum     float64  // Sum of all observations
}

// EpochSnapshot holds the raw cumulative counters at a specific point in time.
// It acts as the immutable baseline from which the relative metric deltas are derived.
type EpochSnapshot struct {
	Timestamp             time.Time // Timestamp of the scrape
	TPOTHistogram         HistogramSnapshot
	TTFTHistogram         HistogramSnapshot
	PrefillHistogram      HistogramSnapshot
	GenerationTokensTotal uint64
	RequestSuccessTotal   uint64
}

// EpochDelta is the derived metric dataset passed down to the Auto-Tuner.
// It synthesizes throughput rates and latency measurements over a strictly timed interval window,
// effectively isolating execution bursts from steady-state inference.
// Note: Latency metrics may be math.NaN() if no traffic occurred during the duration.
type EpochDelta struct {
	P90TPOT             float64
	P50TTFT             float64
	P50Prefill          float64
	ThroughputTokensSec float64 // Tokens generated per second during this epoch
	DeltaRequestSuccess uint64  // Used by the Auto-Tuner to determine statistical confidence
	Duration            time.Duration
}

const maxWindowMultiplier = 3 // Force flush after 3x the epoch window.

// EndpointDeltaEngine maintains temporal boundaries and calculates epoch deltas for a single endpoint.
// It absorbs cumulative scraping jitter by advancing the execution window only once enough time has
// elapsed, converting raw cumulative metrics into rate deltas.
type EndpointDeltaEngine struct {
	mu          sync.Mutex
	lastEpoch   EpochSnapshot
	epochWindow time.Duration
	minSamples  uint64
}

// NewEndpointDeltaEngine initializes the time-series bridge for a newly discovered endpoint.
// epochWindow defines the observation period (e.g., 2 seconds) used to decouple the metrics
// analysis frequency from the raw Extractor plugin tick frequency, making control-theory
// calculations resilient to metrics collection jitter.
// minSamples defines the minimum sample volume required before a window is allowed to tumble,
// enabling Elastic Window behaviors.
func NewEndpointDeltaEngine(epochWindow time.Duration, minSamples uint64) *EndpointDeltaEngine {
	return &EndpointDeltaEngine{
		epochWindow: epochWindow,
		minSamples:  minSamples,
	}
}

// UpdateScrape is called on each Extractor plugin tick (e.g., every 50ms).
// It determines if a complete tumbling window has elapsed. If so, it computes the performance
// delta between the last epoch and this one, returning an EpochDelta.
func (e *EndpointDeltaEngine) UpdateScrape(scrape EpochSnapshot) *EpochDelta {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Handle absolute initialization.
	if e.lastEpoch.Timestamp.IsZero() {
		e.lastEpoch = scrape
		return nil
	}

	// Defend against counter resets (endpoint restarts) and metrics omissions.
	// We ignore strict transitions TO zero, as that usually indicates a scrape omission/error.
	isZeroTransition := (scrape.GenerationTokensTotal == 0 && e.lastEpoch.GenerationTokensTotal > 0) ||
		(scrape.RequestSuccessTotal == 0 && e.lastEpoch.RequestSuccessTotal > 0) ||
		(scrape.TPOTHistogram.Count == 0 && e.lastEpoch.TPOTHistogram.Count > 0) ||
		(scrape.TTFTHistogram.Count == 0 && e.lastEpoch.TTFTHistogram.Count > 0) ||
		(scrape.PrefillHistogram.Count == 0 && e.lastEpoch.PrefillHistogram.Count > 0)

	if isZeroTransition {
		return nil // Ignore omission/error, do not update baseline.
	}

	// If any cumulative counter goes backward without hitting absolute zero, the backend likely rebooted.
	isReset := (scrape.GenerationTokensTotal < e.lastEpoch.GenerationTokensTotal) ||
		(scrape.RequestSuccessTotal < e.lastEpoch.RequestSuccessTotal) ||
		(scrape.TPOTHistogram.Count < e.lastEpoch.TPOTHistogram.Count) ||
		(scrape.TTFTHistogram.Count < e.lastEpoch.TTFTHistogram.Count) ||
		(scrape.PrefillHistogram.Count < e.lastEpoch.PrefillHistogram.Count)

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

	deltaReqs := scrape.RequestSuccessTotal - e.lastEpoch.RequestSuccessTotal

	// The Elastic Window with Hard Timeout
	// - Tumble if we are completely idle (deltaReqs == 0) to allow idle resets.
	// - Tumble if we have statistical confidence (deltaReqs >= minSamples).
	// - Tumble if the window has been open for too long (elapsed >= maxWindowDuration) to prevent jams.
	// - Otherwise, extend the window and return nil.
	maxWindowDuration := e.epochWindow * maxWindowMultiplier
	if deltaReqs > 0 && deltaReqs < e.minSamples && elapsed < maxWindowDuration {
		return nil
	}

	// Tumble! Calculate latency percentiles and throughput...
	delta := &EpochDelta{
		P90TPOT:             CalculateQuantile(0.90, e.lastEpoch.TPOTHistogram, scrape.TPOTHistogram),
		P50TTFT:             CalculateQuantile(0.50, e.lastEpoch.TTFTHistogram, scrape.TTFTHistogram),
		P50Prefill:          CalculateQuantile(0.50, e.lastEpoch.PrefillHistogram, scrape.PrefillHistogram),
		DeltaRequestSuccess: deltaReqs,
		Duration:            elapsed,
	}

	// Calculate token throughput (tokens per second).
	// Using elapsed.Seconds() natively handles floating-point conversion and jitter.
	deltaTokens := scrape.GenerationTokensTotal - e.lastEpoch.GenerationTokensTotal
	delta.ThroughputTokensSec = float64(deltaTokens) / elapsed.Seconds()

	// Advance the window. This discards the previous baseline and establishes the current cumulative
	// scrape as the baseline for the next non-overlapping window.
	e.lastEpoch = scrape

	return delta
}

// CalculateQuantile computes a generic percentile (e.g., 0.90 for P90) over a delta histogram.
// It linearly interpolates latencies across sparse cumulative datasets. If there is no data or the
// dataset is malformed, it returns math.NaN().
func CalculateQuantile(q float64, oldSnap, newSnap HistogramSnapshot) float64 {
	if q < 0 || q > 1 {
		return math.NaN()
	}

	// Defend against mismatched scrape topologies (e.g., inference engine upgrades mid-flight).
	if len(oldSnap.Buckets) != len(newSnap.Buckets) {
		return math.NaN()
	}

	deltaTotalCount := newSnap.Count - oldSnap.Count
	if deltaTotalCount == 0 {
		return math.NaN() // No traffic in this epoch
	}

	// Calculate the threshold rank representing the specific percentile within the sample frame.
	rank := q * float64(deltaTotalCount)

	var prevDeltaCumCount uint64
	var lowerBound float64

	for i, bucket := range newSnap.Buckets {
		oldBucketCount := oldSnap.Buckets[i].Count

		// Defend against uint64 underflow if a specific bucket drops uncharacteristically.
		if bucket.Count < oldBucketCount {
			return math.NaN()
		}

		upperBound := bucket.UpperBound
		currDeltaCumCount := bucket.Count - oldBucketCount

		// Defend against non-monotonic histograms causing underflow.
		if currDeltaCumCount < prevDeltaCumCount {
			return math.NaN()
		}

		if float64(currDeltaCumCount) >= rank {
			countInBucket := currDeltaCumCount - prevDeltaCumCount

			if countInBucket == 0 || math.IsInf(upperBound, 1) {
				return lowerBound
			}

			bucketWidth := upperBound - lowerBound
			rankInBucket := rank - float64(prevDeltaCumCount)

			return lowerBound + ((rankInBucket / float64(countInBucket)) * bucketWidth)
		}

		lowerBound = upperBound
		prevDeltaCumCount = currDeltaCumCount
	}

	// Closure to return the highest legal finite boundary, preventing the calculation from
	// incorrectly returning math.Inf for the infinite buckets.
	getUpperBound := func(idx int) float64 {
		if idx > 0 && math.IsInf(newSnap.Buckets[idx].UpperBound, 1) {
			return newSnap.Buckets[idx-1].UpperBound
		}
		return newSnap.Buckets[idx].UpperBound
	}

	if len(newSnap.Buckets) > 0 {
		// Walk backwards to find the highest bucket with an actual delta increment.
		// Reverse searching ensures we retrieve the true peak measurement interval rather than
		// snapping to the absolute maximum theoretical limit of the histogram.
		for i := range slices.Backward(newSnap.Buckets) {
			if newSnap.Buckets[i].Count < oldSnap.Buckets[i].Count {
				continue
			}
			currDiff := newSnap.Buckets[i].Count - oldSnap.Buckets[i].Count

			var prevDiff uint64
			if i > 0 && newSnap.Buckets[i-1].Count >= oldSnap.Buckets[i-1].Count {
				prevDiff = newSnap.Buckets[i-1].Count - oldSnap.Buckets[i-1].Count
			}

			if currDiff > prevDiff {
				return getUpperBound(i)
			}
		}
		// Fallback to absolute last finite bound if diff probe fails.
		return getUpperBound(len(newSnap.Buckets) - 1)
	}

	return math.NaN()
}
