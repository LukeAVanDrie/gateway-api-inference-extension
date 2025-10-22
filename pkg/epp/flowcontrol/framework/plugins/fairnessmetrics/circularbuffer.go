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

// Package fairnessmetrics provides generic, reusable utility components for fairness metrics tracking.
package fairnessmetrics

import (
	"cmp"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// Numeric defines the set of operations required for a type to be used in the CircularBuffer.
// It is a generic interface that allows the buffer to operate on different kinds of numeric types (e.g., int64).
type Numeric[T any] interface {
	cmp.Ordered
	// Add returns the sum of the current value and the given value.
	Add(T) T
	// Sub returns the result of subtracting the given value from the current value.
	Sub(T) T
	// Zero returns the zero value for the type.
	Zero() T
}

// NumericFloat64 is a int64 type that satisfies the Numeric interface.
type NumericInt64 int64

func (n NumericInt64) Add(v NumericInt64) NumericInt64 { return n + v }
func (n NumericInt64) Sub(v NumericInt64) NumericInt64 { return n - v }
func (n NumericInt64) Zero() NumericInt64              { return 0 }

// NumericFloat64 is a float64 type that satisfies the Numeric interface.
type NumericFloat64 float64

func (n NumericFloat64) Add(v NumericFloat64) NumericFloat64 { return n + v }
func (n NumericFloat64) Sub(v NumericFloat64) NumericFloat64 { return n - v }
func (n NumericFloat64) Zero() NumericFloat64                { return 0 }

// CircularBuffer implements a generic, thread-safe circular buffer for tracking time-windowed metrics.
//
// It partitions a time window into a fixed number of buckets, with each bucket holding an aggregated value for its
// corresponding time slice. The buffer automatically clears stale buckets as time progresses.
//
// The type parameter T must satisfy the Numeric interface, which provides the necessary arithmetic operations and
// ensures the type is comparable.
//
// This type is safe for concurrent use by multiple goroutines.
type CircularBuffer[T Numeric[T]] struct {
	mu         sync.Mutex
	buckets    []T
	size       int // The total number of buckets in the buffer
	head       int
	headTime   time.Time
	resolution time.Duration // The time duration each bucket represents; the total window size is size * resolution
	overall    T
	clock      clock.Clock
}

// NewCircularBuffer creates and initializes a new CircularBuffer.
func NewCircularBuffer[T Numeric[T]](size int, resolution time.Duration, clock clock.Clock) *CircularBuffer[T] {
	now := clock.Now()
	var zero T
	zero = zero.Zero()

	buckets := make([]T, size)
	for i := range buckets {
		buckets[i] = zero
	}

	return &CircularBuffer[T]{
		buckets:    buckets,
		size:       size,
		head:       0,
		headTime:   now.Truncate(resolution),
		resolution: resolution,
		overall:    zero,
		clock:      clock,
	}
}

// Add adds a value to the bucket corresponding to the current time.
func (cb *CircularBuffer[T]) Add(val T) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Slide the window forward to the current time, clearing stale buckets.
	cb.advance()

	cb.buckets[cb.head] = cb.buckets[cb.head].Add(val)
	cb.overall = cb.overall.Add(val)
}

// Get returns the total sum of all values currently within the time window.
func (cb *CircularBuffer[T]) Get() T {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Ensure the view is up-to-date by clearing stale buckets before reading.
	cb.advance()
	return cb.overall
}

// advance moves the head of the buffer to the bucket corresponding to the current time, clearing any stale buckets in
// the process.
// This must be called under lock.
func (cb *CircularBuffer[T]) advance() {
	nowTruncated := cb.clock.Now().Truncate(cb.resolution)

	// Calculate how many buckets have passed since the last update.
	diff := int(nowTruncated.Sub(cb.headTime) / cb.resolution)
	if diff <= 0 {
		return // Still in the same bucket, no advancement needed
	}

	var zero T
	zero = zero.Zero()

	if diff >= cb.size {
		// The entire window is stale. Reset all buckets efficiently.
		for i := 0; i < cb.size; i++ {
			cb.buckets[i] = zero
		}
		cb.overall = zero
	} else {
		// Only some buckets are stale. Clear them one by one.
		for i := 1; i <= diff; i++ {
			staleIndex := (cb.head + i) % cb.size
			// Subtract the value of the stale bucket that is about to be overwritten.
			if cb.buckets[staleIndex] != zero {
				cb.overall = cb.overall.Sub(cb.buckets[staleIndex])
				cb.buckets[staleIndex] = zero
			}
		}
	}

	cb.head = (cb.head + diff) % cb.size
	cb.headTime = nowTruncated
}
