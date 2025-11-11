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
package saturationdetector

import (
	"sync"
	"sync/atomic"
)

// concurrencyTracker is a utility that provides thread-safe, real-time accounting of inflight requests to each pod.
// It is the single source of truth for instantaneous concurrency (L_t).
// It is designed for a "read-mostly" workload, where lookups are far more frequent than the creation of new entries.
type concurrencyTracker struct {
	// mu protects the concurrency map itself from concurrent reads and writes, which can happen when new pods are added
	// or removed.
	mu sync.RWMutex

	// concurrency is a map of pod IDs to their respective atomic inflight counters.
	concurrency map[string]*atomic.Uint64
}

// newConcurrencyTracker creates a new, safely initialized tracker.
func newConcurrencyTracker() *concurrencyTracker {
	return &concurrencyTracker{
		concurrency: make(map[string]*atomic.Uint64),
	}
}

// Get returns the current instantaneous inflight concurrency for a given pod.
// This is the primary method for the controller's reconciliation loop to get L_t.
// If no requests have ever been sent to the pod, it safely returns 0.
// This method is safe for concurrent use.
func (ct *concurrencyTracker) Get(podID string) uint64 {
	ct.mu.RLock()
	counter, exists := ct.concurrency[podID]
	ct.mu.RUnlock()

	if !exists {
		return 0
	}
	return counter.Load()
}

// getCounter retrieves the atomic counter for a pod.
// It is used for decrementing the inflight count on ResponseComplete.
//
// It assumes the pod's counter already exists. This is a safe assumption, as a pod cannot complete a request if one was
// never sent to it via PreRequest (which would have created the counter).
func (ct *concurrencyTracker) getCounter(podID string) *atomic.Uint64 {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	// No nil check is needed here due to the logical guarantee described above.
	// This keeps the hot path as lean as possible.
	return ct.concurrency[podID]
}

// getOrCreateCounter retrieves the atomic counter for a pod, creating it if it does not exist.
// This is necessary for PreRequest, as it might be the first time we've seen this pod (e.g., after a scale-up).
// It uses a double-checked locking pattern for high performance.
func (ct *concurrencyTracker) getOrCreateCounter(podID string) *atomic.Uint64 {
	// Fast Path: Check with a read lock first for performance.
	// This will be the case for >99% of calls in a stable system.
	ct.mu.RLock()
	counter, exists := ct.concurrency[podID]
	ct.mu.RUnlock()
	if exists {
		return counter
	}

	// The counter does not exist. Acquire a full write lock to safely create and insert it.
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Slow Path: It's possible another goroutine was also in the slow path and created the counter while we were waiting
	// for the write lock.
	// This check prevents us from overwriting it.
	if counter, exists = ct.concurrency[podID]; exists {
		return counter
	}

	newCounter := &atomic.Uint64{}
	ct.concurrency[podID] = newCounter
	return newCounter
}
