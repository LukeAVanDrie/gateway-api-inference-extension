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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewEndpointDeltaEngine(t *testing.T) {
	window := 2 * time.Second
	engine := NewEndpointDeltaEngine(window)
	require.NotNil(t, engine, "Engine should be initialized")
	assert.Equal(t, window, engine.epochWindow, "Target epoch window should be properly assigned")
}

func TestCalculateQuantile(t *testing.T) {
	testCases := []struct {
		name     string
		q        float64
		oldSnap  HistogramSnapshot
		newSnap  HistogramSnapshot
		expected float64 // Use math.NaN() for empty/invalid states
	}{
		{
			name: "Standard P50 interpolation",
			q:    0.50,
			oldSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 100.0, Count: 0}, {UpperBound: 200.0, Count: 0}},
				Count:   0,
			},
			newSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 100.0, Count: 10}, {UpperBound: 200.0, Count: 30}},
				Count:   30,
			},
			// Delta Total = 30. P50 rank = 15.
			// Bucket 1 (le=100) has 10 items. Bucket 2 (le=200) has 20 items.
			// Rank 15 falls in Bucket 2. It is 5 items into the 20 item bucket (25% through).
			// 25% of the distance between 100 and 200 is 125.0.
			expected: 125.0,
		},
		{
			name: "No traffic during window (zero delta total)",
			q:    0.90,
			oldSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 10.0, Count: 10}}, Count: 10,
			},
			newSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 10.0, Count: 10}}, Count: 10,
			},
			expected: math.NaN(),
		},
		{
			name:     "Topology mismatch (inference engine upgraded mid-scrape)",
			q:        0.90,
			oldSnap:  HistogramSnapshot{Buckets: []Bucket{{UpperBound: 5.0, Count: 0}}},
			newSnap:  HistogramSnapshot{Buckets: []Bucket{{UpperBound: 5.0, Count: 0}, {UpperBound: 10.0, Count: 0}}},
			expected: math.NaN(),
		},
		{
			name:     "Quantile out of bounds",
			q:        1.50,
			oldSnap:  HistogramSnapshot{Count: 0},
			newSnap:  HistogramSnapshot{Count: 10},
			expected: math.NaN(),
		},
		{
			name: "Malformed scrape causing bucket underflow",
			q:    0.90,
			oldSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 10.0, Count: 50}}, Count: 50,
			},
			newSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 10.0, Count: 10}}, Count: 60, // Total increased, but bucket dropped!
			},
			expected: math.NaN(), // Should safely abort instead of uint64 underflow.
		},
		{
			name: "Interpolation within +Inf bucket returns the highest finite boundary",
			q:    0.90,
			oldSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 5.0, Count: 0}, {UpperBound: math.Inf(1), Count: 0}}, Count: 0,
			},
			newSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 5.0, Count: 0}, {UpperBound: math.Inf(1), Count: 10}}, Count: 10,
			},
			expected: 5.0,
		},
		{
			name: "Fallback to highest bucket with actual deltas",
			q:    0.90,
			oldSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 5.0, Count: 0}, {UpperBound: 10.0, Count: 0}}, Count: 0,
			},
			newSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 5.0, Count: 5}, {UpperBound: 10.0, Count: 6}},
				Count:   100, // Explicitly mismatch sum of buckets to trigger fallback logic.
			},
			expected: 10.0,
		},
		{
			name: "Fallback traversal dropping infinity bounds",
			q:    0.90,
			oldSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 5.0, Count: 0}, {UpperBound: 10.0, Count: 0}, {UpperBound: math.Inf(1), Count: 0}},
				Count:   0,
			},
			newSnap: HistogramSnapshot{
				Buckets: []Bucket{{UpperBound: 5.0, Count: 5}, {UpperBound: 10.0, Count: 6}, {UpperBound: math.Inf(1), Count: 6}},
				Count:   100, // Explicitly mismatch sum of buckets to trigger fallback logic.
			},
			expected: 10.0, // Should snap to highest finite bucket, capping at 10.
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := CalculateQuantile(tc.q, tc.oldSnap, tc.newSnap)

			if math.IsNaN(tc.expected) {
				assert.True(t, math.IsNaN(result), "Expected NaN but got %f", result)
			} else {
				assert.InDelta(t, tc.expected, result, 0.0001, "Quantile calculation did not match expected delta")
			}
		})
	}
}

func TestUpdateScrape_TemporalBoundary(t *testing.T) {
	window := 2 * time.Second
	engine := NewEndpointDeltaEngine(window)

	now := time.Now()
	first := EpochSnapshot{
		Timestamp:             now,
		GenerationTokensTotal: 100,
	}
	require.Nil(t, engine.UpdateScrape(first), "First scrape should just establish the baseline")

	// Same-time or too-early scrape should return nil (swallowed to absorb jitter).
	early := EpochSnapshot{
		Timestamp:             now.Add(1 * time.Second),
		GenerationTokensTotal: 150,
	}
	require.Nil(t, engine.UpdateScrape(early), "Too early scrape should return nil")

	// Enough window elapsed scrape.
	success := EpochSnapshot{
		Timestamp:             now.Add(2 * time.Second),
		GenerationTokensTotal: 200,
	}
	delta := engine.UpdateScrape(success)
	require.NotNil(t, delta, "Scrape after the tumbling window must return a valid delta")
	assert.InDelta(t, 50.0, delta.ThroughputTokensSec, 0.0001, "Calculated ThroughputTokensSec incorrectly")
}

func TestUpdateScrape_CounterReset(t *testing.T) {
	window := 2 * time.Second
	engine := NewEndpointDeltaEngine(window)

	now := time.Now()
	first := EpochSnapshot{
		Timestamp:             now,
		GenerationTokensTotal: 500,
	}
	require.Nil(t, engine.UpdateScrape(first), "First scrape establishes baseline")

	// Reset counter (GenerationTokensTotal went backward)
	reset := EpochSnapshot{
		Timestamp:             now.Add(3 * time.Second),
		GenerationTokensTotal: 10,
	}
	delta := engine.UpdateScrape(reset)
	require.Nil(t, delta, "Reset scrape must discard calculation and return nil delta")

	// Re-establish baseline.
	success := EpochSnapshot{
		Timestamp:             now.Add(6 * time.Second),
		GenerationTokensTotal: 15,
	}
	deltaNow := engine.UpdateScrape(success)
	require.NotNil(t, deltaNow, "Tick after a reset baseline should return delta")
	assert.InDelta(t, 1.6666666, deltaNow.ThroughputTokensSec, 0.0001, "Should compute correctly on post-reset tokens")
}

func TestUpdateScrape_TransitionToZeroIgnoresOmission(t *testing.T) {
	window := 2 * time.Second
	engine := NewEndpointDeltaEngine(window)

	now := time.Now()
	require.Nil(t, engine.UpdateScrape(makeSnapshot(now, 500)), "Initial scrape establishes baseline")

	// Transition exactly to zero (simulate scraping omission/network drop).
	omission := EpochSnapshot{
		Timestamp:             now.Add(3 * time.Second),
		GenerationTokensTotal: 0,
	}
	require.Nil(t, engine.UpdateScrape(omission), "Omission scrape must be ignored entirely")

	// Next scrape recovers to 505 (5 tokens generated since the original baseline).
	success := EpochSnapshot{
		Timestamp:             now.Add(6 * time.Second),
		GenerationTokensTotal: 505,
	}
	deltaNow := engine.UpdateScrape(success)
	require.NotNil(t, deltaNow, "Tick after omission should return valid delta math")

	expectedTokensPerSec := float64(505-500) / 6.0 // 6 seconds elapsed since baseline.
	assert.InDelta(t, expectedTokensPerSec, deltaNow.ThroughputTokensSec, 0.0001,
		"Delta should represent the full window, ignoring the 0-omission")
}

func TestUpdateScrape_Concurrency(t *testing.T) {
	window := 2 * time.Second
	engine := NewEndpointDeltaEngine(window)

	now := time.Now()
	first := EpochSnapshot{
		Timestamp:             now,
		GenerationTokensTotal: 100,
	}
	require.Nil(t, engine.UpdateScrape(first))

	var wg sync.WaitGroup
	workers := 10
	wg.Add(workers)

	// Tests the mutex locks. Go's race detector (`go test -race`) will fail this if there is a data race.
	for i := range workers {
		go func(id int) {
			defer wg.Done()
			scrape := EpochSnapshot{
				Timestamp:             now.Add(time.Duration(id+2) * time.Second),
				GenerationTokensTotal: uint64(100 + (id+1)*10),
			}
			_ = engine.UpdateScrape(scrape)
		}(i)
	}

	wg.Wait()
}

func makeSnapshot(timestamp time.Time, tokens uint64) EpochSnapshot {
	return EpochSnapshot{
		Timestamp:             timestamp,
		GenerationTokensTotal: tokens,
	}
}

func BenchmarkUpdateScrape(b *testing.B) {
	engine := NewEndpointDeltaEngine(2 * time.Second)
	now := time.Now()

	first := makeSnapshot(now, 100)
	engine.UpdateScrape(first)

	// Pre-allocate bucket layout to avoid benchmark pollution.
	buckets := []Bucket{
		{UpperBound: 10.0, Count: 0},
		{UpperBound: 20.0, Count: 0},
	}

	for i := 0; b.Loop(); i++ {
		// Advance the clock strictly forward on every loop to bypass the jitter guard.
		now = now.Add(3 * time.Second)

		buckets[0].Count += 5
		buckets[1].Count += 10

		next := EpochSnapshot{
			Timestamp:             now,
			GenerationTokensTotal: uint64(200 + i*10),
			TPOTHistogram:         HistogramSnapshot{Buckets: buckets, Count: uint64(15 * (i + 1))},
		}

		engine.UpdateScrape(next)
	}
}
