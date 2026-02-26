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

package hypervisor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// atomicResourceVector simplifies high-concurrency mutation by utilizing memory fences.
//
// Hardware Optimization: We strictly pad this struct to 64 bytes. Without padding, the dense layout
// of global vectors causes "False Sharing", where independent vectors (e.g., globalHold and
// globalScraped) occupy the same CPU L3 Cache Line. High frequency writes to Hold would constantly
// invalidate the Cache Line for Scraped, crippling performance.
type atomicResourceVector struct {
	PrefillTokens  atomic.Int64
	DecodeTokens   atomic.Int64
	KVBlocks       atomic.Int64
	ActiveRequests atomic.Int64
	_              [32]byte // 32 bytes of payload + 32 bytes padding = 64 byte Cache Line
}

// endpointLedger maintains the localized O(N) cache-aware routing metrics.
type endpointLedger struct {
	scrapedBaseline atomicResourceVector
	limit           atomicResourceVector

	// mu protects the transitBuckets from zeroing races with the Master Tick.
	// Because this is localized to a single endpoint, lock contention is practically zero.
	mu             sync.Mutex
	transitBuckets [3]ResourceVector // Standard structs (non-atomic) to prevent dimension tearing
}

// TwoTierLedger executes O(1) global accounting with precise temporal netting to regulate
// multi-backend execution grids without hallucinating capacity.
type TwoTierLedger struct {
	// admissionMu serializes the Check-and-Add flow control step.
	// This directly prevents "Thundering Herd" spurious rejection livelocks where concurrent requests
	// mutually inflate bounds and universally reject themselves.
	admissionMu sync.Mutex

	globalHold          atomicResourceVector
	globalScraped       atomicResourceVector
	globalTransit       [3]atomicResourceVector
	globalLimit         atomicResourceVector
	globalMaxContiguous atomicResourceVector

	globalEpoch atomic.Uint64 // Monotonically increasing temporal epoch (1 tick = 50ms)

	endpointLedgers sync.Map // map[string]*endpointLedger
}

// RunMasterTick should be launched in a background goroutine on startup.
// It advances the temporal epoch every 50ms, permanently zeroing the oldest debt.
func (l *TwoTierLedger) RunMasterTick(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Monotonically advance the epoch to permanently prevent bucket aliasing.
			newEpoch := l.globalEpoch.Add(1)

			// The bucket that is logically "oldest" must be explicitly zeroed out.
			oldestIdx := (newEpoch + 1) % 3

			// 1. Zero Global Bucket (Lock-free, eventual consistency)
			l.globalTransit[oldestIdx].PrefillTokens.Store(0)
			l.globalTransit[oldestIdx].DecodeTokens.Store(0)
			l.globalTransit[oldestIdx].KVBlocks.Store(0)
			l.globalTransit[oldestIdx].ActiveRequests.Store(0)

			// 2. Zero Local Buckets (Lock-protected to prevent local netting races)
			l.endpointLedgers.Range(func(key, value any) bool {
				endpoint := value.(*endpointLedger)

				endpoint.mu.Lock()
				endpoint.transitBuckets[oldestIdx] = ResourceVector{}
				endpoint.mu.Unlock()

				return true
			})
		}
	}
}

// TryAcquireHold applies the O(1) global admission check synchronously, and only adds to the
// un-committed pool upon success.
func (l *TwoTierLedger) TryAcquireHold(worstCase ResourceVector) (*HoldReceipt, error) {
	// If the request requires more space than the largest single contiguous hole available in the
	// pool, reject it instantly. Aggregate math doesn't matter if it physically cannot fit on any
	// single endpoint.
	if worstCase.PrefillTokens > l.globalMaxContiguous.PrefillTokens.Load() ||
		worstCase.DecodeTokens > l.globalMaxContiguous.DecodeTokens.Load() ||
		worstCase.KVBlocks > l.globalMaxContiguous.KVBlocks.Load() ||
		worstCase.ActiveRequests > l.globalMaxContiguous.ActiveRequests.Load() {
		return nil, ErrGlobalCapacityExceeded
	}

	l.admissionMu.Lock()
	defer l.admissionMu.Unlock()

	epoch := l.globalEpoch.Load()
	idx := epoch % 3
	prevIdx := (epoch + 2) % 3 // strictly evaluates current and previous transit windows

	checkAll := func(hold, scraped, t1, t2, limit *atomicResourceVector, worst ResourceVector) bool {
		check := func(h, s, tr1, tr2, l *atomic.Int64, w int64) bool {
			return (h.Load() + s.Load() + tr1.Load() + tr2.Load() + w) > l.Load()
		}

		return (check(
			&hold.PrefillTokens, &scraped.PrefillTokens,
			&t1.PrefillTokens, &t2.PrefillTokens,
			&limit.PrefillTokens, worst.PrefillTokens) ||
			check(
				&hold.DecodeTokens, &scraped.DecodeTokens,
				&t1.DecodeTokens, &t2.DecodeTokens,
				&limit.DecodeTokens, worst.DecodeTokens) ||
			check(
				&hold.KVBlocks, &scraped.KVBlocks,
				&t1.KVBlocks, &t2.KVBlocks,
				&limit.KVBlocks, worst.KVBlocks) ||
			check(
				&hold.ActiveRequests, &scraped.ActiveRequests,
				&t1.ActiveRequests, &t2.ActiveRequests,
				&limit.ActiveRequests, worst.ActiveRequests))
	}

	// Evaluate bounds and return early on saturation.
	if checkAll(
		&l.globalHold, &l.globalScraped,
		&l.globalTransit[idx], &l.globalTransit[prevIdx],
		&l.globalLimit, worstCase,
	) {
		return nil, ErrGlobalCapacityExceeded
	}

	// Unconditionally Add upon success.
	l.globalHold.PrefillTokens.Add(worstCase.PrefillTokens)
	l.globalHold.DecodeTokens.Add(worstCase.DecodeTokens)
	l.globalHold.KVBlocks.Add(worstCase.KVBlocks)
	l.globalHold.ActiveRequests.Add(worstCase.ActiveRequests)

	return &HoldReceipt{Held: worstCase}, nil
}

// ReleaseHold clears un-finalized estimations.
func (l *TwoTierLedger) ReleaseHold(receipt *HoldReceipt) {
	if receipt == nil {
		return
	}
	l.globalHold.PrefillTokens.Add(-receipt.Held.PrefillTokens)
	l.globalHold.DecodeTokens.Add(-receipt.Held.DecodeTokens)
	l.globalHold.KVBlocks.Add(-receipt.Held.KVBlocks)
	l.globalHold.ActiveRequests.Add(-receipt.Held.ActiveRequests)
}

// Commit converts a pessimistic global hold into an actual O(N) endpoint-specific allocation.
// Returns a CommitReceipt tying this workload to the exact temporal epoch of admission.
func (l *TwoTierLedger) Commit(
	endpointID string,
	actualCost ResourceVector,
	receipt *HoldReceipt,
) (*CommitReceipt, error) {
	// Ensure global capacity is always refunded to prevent resource leakage in the event that
	// endpoint resolution or routing fails.
	defer l.ReleaseHold(receipt)

	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return nil, ErrEndpointNotFound
	}
	endpoint := value.(*endpointLedger)

	epoch := l.globalEpoch.Load()
	idx := epoch % 3

	// Apply localized dimensions safely.
	endpoint.mu.Lock()
	endpoint.transitBuckets[idx].PrefillTokens += actualCost.PrefillTokens
	endpoint.transitBuckets[idx].DecodeTokens += actualCost.DecodeTokens
	endpoint.transitBuckets[idx].KVBlocks += actualCost.KVBlocks
	endpoint.transitBuckets[idx].ActiveRequests += actualCost.ActiveRequests
	endpoint.mu.Unlock()

	// Apply global dimensions entirely lock-free.
	l.globalTransit[idx].PrefillTokens.Add(actualCost.PrefillTokens)
	l.globalTransit[idx].DecodeTokens.Add(actualCost.DecodeTokens)
	l.globalTransit[idx].KVBlocks.Add(actualCost.KVBlocks)
	l.globalTransit[idx].ActiveRequests.Add(actualCost.ActiveRequests)

	return &CommitReceipt{
		ActualCost: actualCost,
		Epoch:      epoch,
	}, nil
}

// ReleaseEndpointCapacity applies the exact "Net-Transit" math.
// If a request ends quickly, it refunds its original transit bucket. If it takes longer than the
// sliding window, it refunds the scrapedBaseline as the telemetry has proven to have naturally
// absorbed the request's footprint.
func (l *TwoTierLedger) ReleaseEndpointCapacity(endpointID string, receipt *CommitReceipt) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return
	}
	endpoint := value.(*endpointLedger)

	isOld := false
	idx := receipt.Epoch % 3

	// Securely determine temporal state while preventing race conditions against the Master Tick.
	endpoint.mu.Lock()
	currentEpoch := l.globalEpoch.Load()
	if (currentEpoch - receipt.Epoch) >= 2 {
		isOld = true
	} else {
		endpoint.transitBuckets[idx].PrefillTokens -= receipt.ActualCost.PrefillTokens
		endpoint.transitBuckets[idx].DecodeTokens -= receipt.ActualCost.DecodeTokens
		endpoint.transitBuckets[idx].KVBlocks -= receipt.ActualCost.KVBlocks
		endpoint.transitBuckets[idx].ActiveRequests -= receipt.ActualCost.ActiveRequests
	}
	endpoint.mu.Unlock()

	if isOld {
		endpoint.scrapedBaseline.PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
		endpoint.scrapedBaseline.DecodeTokens.Add(-receipt.ActualCost.DecodeTokens)
		endpoint.scrapedBaseline.KVBlocks.Add(-receipt.ActualCost.KVBlocks)
		endpoint.scrapedBaseline.ActiveRequests.Add(-receipt.ActualCost.ActiveRequests)

		l.globalScraped.PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
		l.globalScraped.DecodeTokens.Add(-receipt.ActualCost.DecodeTokens)
		l.globalScraped.KVBlocks.Add(-receipt.ActualCost.KVBlocks)
		l.globalScraped.ActiveRequests.Add(-receipt.ActualCost.ActiveRequests)
	} else {
		l.globalTransit[idx].PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
		l.globalTransit[idx].DecodeTokens.Add(-receipt.ActualCost.DecodeTokens)
		l.globalTransit[idx].KVBlocks.Add(-receipt.ActualCost.KVBlocks)
		l.globalTransit[idx].ActiveRequests.Add(-receipt.ActualCost.ActiveRequests)
	}
}

// UpdateEndpointLimits modifies high benchmarks to scale topology dimensions.
func (l *TwoTierLedger) UpdateEndpointLimits(endpointID string, newLimits ResourceVector) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		value, _ = l.endpointLedgers.LoadOrStore(endpointID, &endpointLedger{})
	}
	endpoint := value.(*endpointLedger)

	oldPrefill := endpoint.limit.PrefillTokens.Swap(newLimits.PrefillTokens)
	oldDecode := endpoint.limit.DecodeTokens.Swap(newLimits.DecodeTokens)
	oldKV := endpoint.limit.KVBlocks.Swap(newLimits.KVBlocks)
	oldActive := endpoint.limit.ActiveRequests.Swap(newLimits.ActiveRequests)

	l.globalLimit.PrefillTokens.Add(newLimits.PrefillTokens - oldPrefill)
	l.globalLimit.DecodeTokens.Add(newLimits.DecodeTokens - oldDecode)
	l.globalLimit.KVBlocks.Add(newLimits.KVBlocks - oldKV)
	l.globalLimit.ActiveRequests.Add(newLimits.ActiveRequests - oldActive)
}

// UpdateEndpointKVBlocks propagates newly scraped physical KV Cache capacities.
func (l *TwoTierLedger) UpdateEndpointKVBlocks(endpointID string, totalKVBlocks int64) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		value, _ = l.endpointLedgers.LoadOrStore(endpointID, &endpointLedger{})
	}
	endpoint := value.(*endpointLedger)

	oldKV := endpoint.limit.KVBlocks.Swap(totalKVBlocks)
	l.globalLimit.KVBlocks.Add(totalKVBlocks - oldKV)
}

// UpdateEndpointActiveRequests propagates newly scraped rigid concurrency capacities.
func (l *TwoTierLedger) UpdateEndpointActiveRequests(endpointID string, maxActiveRequests int64) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		value, _ = l.endpointLedgers.LoadOrStore(endpointID, &endpointLedger{})
	}
	endpoint := value.(*endpointLedger)

	oldActive := endpoint.limit.ActiveRequests.Swap(maxActiveRequests)
	l.globalLimit.ActiveRequests.Add(maxActiveRequests - oldActive)
}

// ReconcileEndpointCapacity incorporates official real-time state via a polled baseline overwrite.
func (l *TwoTierLedger) ReconcileEndpointCapacity(endpointID string, scrapedUsage ResourceVector) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return
	}
	endpoint := value.(*endpointLedger)

	// Swap local scraped baseline and dynamically inject the accurate delta mathematics.
	deltaPrefill := scrapedUsage.PrefillTokens - endpoint.scrapedBaseline.PrefillTokens.Swap(scrapedUsage.PrefillTokens)
	deltaDecode := scrapedUsage.DecodeTokens - endpoint.scrapedBaseline.DecodeTokens.Swap(scrapedUsage.DecodeTokens)
	deltaKV := scrapedUsage.KVBlocks - endpoint.scrapedBaseline.KVBlocks.Swap(scrapedUsage.KVBlocks)
	deltaActive := scrapedUsage.ActiveRequests - endpoint.scrapedBaseline.ActiveRequests.Swap(scrapedUsage.ActiveRequests)

	// Flow the resulting delta synchronously upwards into the aggregate view.
	l.globalScraped.PrefillTokens.Add(deltaPrefill)
	l.globalScraped.DecodeTokens.Add(deltaDecode)
	l.globalScraped.KVBlocks.Add(deltaKV)
	l.globalScraped.ActiveRequests.Add(deltaActive)
}

// RecalculateMaxContiguous evaluates all endpoints to find the largest single-endpoint contiguous
// capacity for each dimension.
// This should be called exactly once at the end of every 50ms telemetry extraction cycle, directly
// after calling ReconcileEndpointCapacity for all active endpoints.
func (l *TwoTierLedger) RecalculateMaxContiguous() {
	var maxPrefill, maxDecode, maxKV, maxActive int64

	l.endpointLedgers.Range(func(key, value any) bool {
		endpoint := value.(*endpointLedger)

		availPrefill := endpoint.limit.PrefillTokens.Load() - endpoint.scrapedBaseline.PrefillTokens.Load()
		availDecode := endpoint.limit.DecodeTokens.Load() - endpoint.scrapedBaseline.DecodeTokens.Load()
		availKV := endpoint.limit.KVBlocks.Load() - endpoint.scrapedBaseline.KVBlocks.Load()
		availActive := endpoint.limit.ActiveRequests.Load() - endpoint.scrapedBaseline.ActiveRequests.Load()

		endpoint.mu.Lock()
		for i := range 3 {
			availPrefill -= endpoint.transitBuckets[i].PrefillTokens
			availDecode -= endpoint.transitBuckets[i].DecodeTokens
			availKV -= endpoint.transitBuckets[i].KVBlocks
			availActive -= endpoint.transitBuckets[i].ActiveRequests
		}
		endpoint.mu.Unlock()

		// Track the largest hole for each dimension across the pool.
		maxPrefill = max(maxPrefill, availPrefill)
		maxDecode = max(maxDecode, availDecode)
		maxKV = max(maxKV, availKV)
		maxActive = max(maxActive, availActive)

		return true
	})

	// Commit the new maximums.
	l.globalMaxContiguous.PrefillTokens.Store(maxPrefill)
	l.globalMaxContiguous.DecodeTokens.Store(maxDecode)
	l.globalMaxContiguous.KVBlocks.Store(maxKV)
	l.globalMaxContiguous.ActiveRequests.Store(maxActive)
}
