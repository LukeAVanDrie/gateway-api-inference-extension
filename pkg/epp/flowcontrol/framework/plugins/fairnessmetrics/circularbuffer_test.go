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

package fairnessmetrics

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	testingclock "k8s.io/utils/clock/testing"
)

func TestCircularBuffer_AddAndGet(t *testing.T) {
	t.Parallel()

	startTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	const bucketResolution = time.Second
	const numBuckets = 5 // Total window size is 5 seconds.

	testCases := []struct {
		name        string
		actions     []func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock)
		expectValue NumericFloat64
	}{
		{
			name: "add once, get value",
			actions: []func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock){
				func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock) {
					cb.Add(10)
				},
			},
			expectValue: 10,
		},
		{
			name: "add multiple times within the same bucket",
			actions: []func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock){
				func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock) {
					cb.Add(10)
					clock.Step(100 * time.Millisecond)
					cb.Add(5)
				},
			},
			expectValue: 15,
		},
		{
			name: "add across a bucket boundary",
			actions: []func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock){
				func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock) {
					cb.Add(10)
					clock.Step(bucketResolution)
					cb.Add(5)
				},
			},
			expectValue: 15,
		},
		{
			name: "window slides partially, older values expire",
			actions: []func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock){
				func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock) {
					cb.Add(10) // t=0s, val=10
					clock.Step(bucketResolution)
					cb.Add(5) // t=1s, val=5
					clock.Step(bucketResolution)
					cb.Add(2) // t=2s, val=2
					clock.Step(3 * bucketResolution)
					// t=5s, window is now [1s, 5s]. The value 10 from t=0s should be gone.
				},
			},
			expectValue: 7, // 5 + 2
		},
		{
			name: "window slides completely, all values expire",
			actions: []func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock){
				func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock) {
					cb.Add(10)
					cb.Add(5)
					// Advance time far beyond the window size.
					clock.Step(time.Duration(numBuckets+1) * bucketResolution)
				},
			},
			expectValue: 0,
		},
		{
			name: "add after full window slide",
			actions: []func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock){
				func(cb *CircularBuffer[NumericFloat64], clock *testingclock.FakeClock) {
					cb.Add(10)
					clock.Step(time.Duration(numBuckets+1) * bucketResolution)
					cb.Add(3)
				},
			},
			expectValue: 3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fakeClock := testingclock.NewFakeClock(startTime)
			cb := NewCircularBuffer[NumericFloat64](numBuckets, bucketResolution, fakeClock)
			for _, action := range tc.actions {
				action(cb, fakeClock)
			}
			val := cb.Get()
			require.Equal(t, tc.expectValue, val, "the final value should match the expected value")
		})
	}
}

func TestCircularBuffer_Concurrency(t *testing.T) {
	const numGoroutines = 100
	const addsPerGoroutine = 100
	const numBuckets = 10
	const bucketResolution = 10 * time.Millisecond

	// --- Arrange ---
	// Use a real clock to simulate real-world timing contention.
	cb := NewCircularBuffer[NumericFloat64](numBuckets, bucketResolution, testingclock.NewFakeClock(time.Now()))
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// --- Act ---
	// Hammer the buffer's Add method from many goroutines concurrently.
	for range numGoroutines {
		go func() {
			defer wg.Done()
			for range addsPerGoroutine {
				cb.Add(1)
			}
		}()
	}
	wg.Wait()

	// --- Assert ---
	finalValue := cb.Get()
	expectedValue := NumericFloat64(numGoroutines * addsPerGoroutine)
	require.Equal(t, expectedValue, finalValue, "final count must be accurate after all concurrent additions complete")
}
