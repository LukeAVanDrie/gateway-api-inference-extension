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

import "cmp"

// filterSample holds a single observed value and the key (e.g., a timestamp or round number) at which it was observed.
type filterSample[T cmp.Ordered, K comparable] struct {
	value T
	key   K
}

// WindowedExtremumFilter implements a generic, O(1) windowed max/min filter.
// It is designed for high-performance, allocation-sensitive signal processing and provides a noise-resilient estimate
// of an extremum value over a sliding window.
//
// The filter maintains a sorted list of the top N (configurable) extremum values.
// The primary value (at index 0) is the current best estimate. The secondary samples provide a robust fallback,
// ensuring the estimate degrades gracefully if the primary sample expires from the window.
//
// # Design Inspiration:
//
// The core logic of this filter is heavily inspired by the windowed-extremum estimation filters used in Google's BBR
// (Bottleneck Bandwidth and Round-trip propagation time) congestion control algorithm.
// Specifically, it mirrors how BBR estimates bottleneck bandwidth (BtlBw) with a windowed-max filter and propagation
// delay (RTprop) with a windowed-min filter to find stable signals in noisy, real-time environments.
//
// # Core Logic:
//  1. A new value that is more extreme than the current primary becomes the new primary, resetting the filter's
//     history. This represents a fundamental shift in the system being measured.
//  2. A value equal to the primary refreshes its key (timestamp), preventing expiration.
//  3. Less extreme values are inserted into the sorted list of secondary samples, displacing worse-performing samples
//     if the list is full.
//
// The zero value of this struct is not usable. It must be initialized via NewWindowedMaxFilter or NewWindowedMinFilter.
// This type is NOT safe for concurrent access. The caller must provide external synchronization.
type WindowedExtremumFilter[T cmp.Ordered, K comparable] struct {
	window             K
	samples            []filterSample[T, K]
	comparator         func(a, b T) bool
	expirationCheck    func(window, currentKey, pastKey K) bool
	uninitializedValue T
}

// newWindowedExtremumFilter is the internal constructor containing common initialization logic.
func newWindowedExtremumFilter[T cmp.Ordered, K comparable](
	window K,
	numSamples int,
	comparator func(a, b T) bool,
	expirationCheck func(window, currentKey, pastKey K) bool,
	uninitializedValue T,
	initialKey K,
) *WindowedExtremumFilter[T, K] {
	if numSamples <= 0 {
		numSamples = 1
	}
	f := &WindowedExtremumFilter[T, K]{
		window:             window,
		samples:            make([]filterSample[T, K], numSamples),
		comparator:         comparator,
		expirationCheck:    expirationCheck,
		uninitializedValue: uninitializedValue,
	}
	f.Reset(uninitializedValue, initialKey)
	return f
}

// NewWindowedMaxFilter creates a filter that tracks the N largest values in the window.
// The uninitializedValue should be a value that is guaranteed to be less than any possible valid sample (e.g., 0.0 for
// latency, -1 for queue depth).
func NewWindowedMaxFilter[T cmp.Ordered, K comparable](
	window K,
	numSamples int,
	expirationCheck func(window, currentKey, pastKey K) bool,
	uninitializedValue T,
	initialKey K,
) *WindowedExtremumFilter[T, K] {
	comparator := func(a, b T) bool { return a > b }
	return newWindowedExtremumFilter(window, numSamples, comparator, expirationCheck, uninitializedValue, initialKey)
}

// NewWindowedMinFilter creates a filter that tracks the N smallest values in the window.
// The uninitializedValue should be a value that is guaranteed to be greater than any possible valid sample (e.g.,
// math.MaxFloat64 for latency).
func NewWindowedMinFilter[T cmp.Ordered, K comparable](
	window K,
	numSamples int,
	expirationCheck func(window, currentKey, pastKey K) bool,
	uninitializedValue T,
	initialKey K,
) *WindowedExtremumFilter[T, K] {
	comparator := func(a, b T) bool { return a < b }
	return newWindowedExtremumFilter(window, numSamples, comparator, expirationCheck, uninitializedValue, initialKey)
}

// Get returns the current extremum value across the window.
// It also returns a boolean, 'initialized', which is true if the filter has received at least one sample and has not
// expired back to its uninitialized state.
// If 'initialized' is false, the returned value is the uninitializedValue provided at construction and should be
// treated as invalid.
func (f *WindowedExtremumFilter[T, K]) Get() (value T, initialized bool) {
	value = f.samples[0].value
	initialized = value != f.uninitializedValue
	return value, initialized
}

// Reset forcefully sets all tracked samples to the provided value and key.
// This is useful for clearing stale history after a significant system state change.
func (f *WindowedExtremumFilter[T, K]) Reset(value T, key K) {
	newSample := filterSample[T, K]{value: value, key: key}
	for i := range f.samples {
		f.samples[i] = newSample
	}
}

// Update incorporates a new observation into the filter.
// It first evicts any expired samples based on the new key, then inserts the new value.
// This is the primary hot-path operation and is designed to be allocation-free.
func (f *WindowedExtremumFilter[T, K]) Update(value T, key K) {
	// Evict any samples that are stale relative to the new key.
	// This ensures we evaluate the new value against a correct, up-to-date window.
	f.evictExpired(key)

	// Now, evaluate the new value against the remaining (valid) samples.
	isUninitialized := f.samples[0].value == f.uninitializedValue
	if isUninitialized || f.comparator(value, f.samples[0].value) {
		// The filter is now empty, or the new value is a new absolute extremum.
		// Reset the filter's history to this new authoritative value.
		f.Reset(value, key)
		return
	}

	if value == f.samples[0].value {
		// The new value is identical to the current extremum.
		// Refresh its key to keep it "fresh" without disturbing secondary samples.
		f.samples[0].key = key
		return
	}

	// The new value is not an extremum.
	// Attempt to insert it into the sorted list of secondary samples.
	newSample := filterSample[T, K]{value: value, key: key}
	for i := 1; i < len(f.samples); i++ {
		// A new sample belongs in this slot if:
		// a) The slot is just a placeholder from a prior Reset (its value is the same as the primary).
		// b) The new sample's value is strictly better than the distinct secondary sample in this slot.
		isPlaceholderSlot := f.samples[i].value == f.samples[0].value
		if isPlaceholderSlot || f.comparator(value, f.samples[i].value) {
			copy(f.samples[i+1:], f.samples[i:])
			f.samples[i] = newSample
			return // Insertion is complete.
		}
	}
}

// evictExpired scans and removes expired samples using an in-place compaction algorithm to avoid memory allocations.
func (f *WindowedExtremumFilter[T, K]) evictExpired(currentKey K) {
	// Use an in-place slice compaction algorithm where 'writeIdx' tracks the position for the next non-expired sample.
	writeIdx := 0
	for readIdx := range f.samples {
		if !f.expirationCheck(f.window, currentKey, f.samples[readIdx].key) {
			// This sample is still valid. Move it to the write position if needed.
			if writeIdx != readIdx {
				f.samples[writeIdx] = f.samples[readIdx]
			}
			writeIdx++
		}
	}

	// If no samples were evicted, we are done.
	if writeIdx == len(f.samples) {
		return
	}

	// If samples were evicted, the tail of the slice (from writeIdx to the end) now contains stale data.
	// We must fill this space to avoid it being used.
	// We back-fill with the last known valid sample to ensure a graceful decay rather than a sudden reset to the
	// uninitialized value.
	lastValidSample := filterSample[T, K]{value: f.uninitializedValue, key: currentKey}
	if writeIdx > 0 {
		// The last valid sample is now at writeIdx-1.
		lastValidSample = f.samples[writeIdx-1]
		// Update its key to the current time to prevent it from expiring immediately on the next tick, ensuring it persists
		// until a new sample arrives.
		lastValidSample.key = currentKey
	}

	for i := writeIdx; i < len(f.samples); i++ {
		f.samples[i] = lastValidSample
	}
}
