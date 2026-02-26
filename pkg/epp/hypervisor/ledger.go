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

func (v *atomicResourceVector) load() ResourceVector {
	return ResourceVector{
		PrefillTokens:  v.PrefillTokens.Load(),
		DecodeTokens:   v.DecodeTokens.Load(),
		KVBlocks:       v.KVBlocks.Load(),
		ActiveRequests: v.ActiveRequests.Load(),
	}
}

// endpointLedger maintains the localized O(N) cache-aware routing metrics.
type endpointLedger struct {
	scrapedBaseline  atomicResourceVector
	endpointTracking atomicResourceVector
	limit            atomicResourceVector

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

	globalHold     atomicResourceVector
	globalScraped  atomicResourceVector
	globalTracking atomicResourceVector
	globalTransit  [3]atomicResourceVector
	globalLimit    atomicResourceVector

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
	epoch := l.globalEpoch.Load()
	idx := epoch % 3
	prevIdx := (epoch + 2) % 3 // strictly evaluates current and previous transit windows

	hasCapacity := false
	l.endpointLedgers.Range(func(key, value any) bool {
		endpoint := value.(*endpointLedger)
		limit := endpoint.limit.load()
		scraped := endpoint.scrapedBaseline.load()

		availKV := limit.KVBlocks - scraped.KVBlocks
		availActive := limit.ActiveRequests - scraped.ActiveRequests

		endpoint.mu.Lock()
		availKV -= endpoint.transitBuckets[idx].KVBlocks + endpoint.transitBuckets[prevIdx].KVBlocks
		availActive -= endpoint.transitBuckets[idx].ActiveRequests + endpoint.transitBuckets[prevIdx].ActiveRequests
		endpoint.mu.Unlock()

		if worstCase.KVBlocks <= availKV && worstCase.ActiveRequests <= availActive {
			hasCapacity = true
			return false // short circuit
		}
		return true
	})

	if !hasCapacity {
		return nil, ErrGlobalCapacityExceeded
	}

	l.admissionMu.Lock()
	defer l.admissionMu.Unlock()

	checkSpatialAll := func(hold, scraped, t1, t2, limit *atomicResourceVector, worst ResourceVector) bool {
		_hold := hold.load()
		_scraped := scraped.load()
		_t1 := t1.load()
		_t2 := t2.load()
		_limit := limit.load()

		return ((_hold.KVBlocks+_scraped.KVBlocks+_t1.KVBlocks+_t2.KVBlocks+worst.KVBlocks) > _limit.KVBlocks ||
			(_hold.ActiveRequests+_scraped.ActiveRequests+_t1.ActiveRequests+_t2.ActiveRequests+worst.ActiveRequests) > _limit.ActiveRequests)
	}

	checkThroughputAll := func(hold, tracked, limit *atomicResourceVector, worst ResourceVector) bool {
		_hold := hold.load()
		_tracked := tracked.load()
		_limit := limit.load()

		return ((_hold.PrefillTokens+_tracked.PrefillTokens+worst.PrefillTokens) > _limit.PrefillTokens ||
			(_hold.DecodeTokens+_tracked.DecodeTokens+worst.DecodeTokens) > _limit.DecodeTokens)
	}

	// Evaluate bounds and return early on saturation.
	if checkSpatialAll(
		&l.globalHold, &l.globalScraped,
		&l.globalTransit[idx], &l.globalTransit[prevIdx],
		&l.globalLimit, worstCase,
	) || checkThroughputAll(
		&l.globalHold, &l.globalTracking,
		&l.globalLimit, worstCase,
	) {
		return nil, ErrGlobalCapacityExceeded
	}

	// Apply aggregation holds unconditionally upon success.
	l.globalHold.PrefillTokens.Add(worstCase.PrefillTokens)
	l.globalHold.DecodeTokens.Add(worstCase.DecodeTokens)
	l.globalHold.KVBlocks.Add(worstCase.KVBlocks)
	l.globalHold.ActiveRequests.Add(worstCase.ActiveRequests)

	return &HoldReceipt{Held: worstCase}, nil
}

// ReleaseHold releases projections safely back into the ephemeral pool.
func (l *TwoTierLedger) ReleaseHold(receipt *HoldReceipt) {
	if receipt == nil {
		return
	}
	l.globalHold.PrefillTokens.Add(-receipt.Held.PrefillTokens)
	l.globalHold.DecodeTokens.Add(-receipt.Held.DecodeTokens)
	l.globalHold.KVBlocks.Add(-receipt.Held.KVBlocks)
	l.globalHold.ActiveRequests.Add(-receipt.Held.ActiveRequests)
}

// Commit elevates a global HoldReceipt into an accurate CommitReceipt inside of the local endpoint
// execution tracking, officially shifting capacity from the ephemeral pool into the dynamic
// temporal net-transit bucket slice.
// Returns a CommitReceipt tying this workload to the exact temporal epoch of admission.
func (l *TwoTierLedger) Commit(endpointID string, actualCost ResourceVector, receipt *HoldReceipt) (*CommitReceipt, error) {
	defer l.ReleaseHold(receipt)

	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return nil, ErrEndpointNotFound
	}
	endpoint := value.(*endpointLedger)

	epoch := l.globalEpoch.Load()
	idx := epoch % 3

	endpoint.endpointTracking.PrefillTokens.Add(actualCost.PrefillTokens)
	endpoint.endpointTracking.DecodeTokens.Add(actualCost.DecodeTokens)
	l.globalTracking.PrefillTokens.Add(actualCost.PrefillTokens)
	l.globalTracking.DecodeTokens.Add(actualCost.DecodeTokens)

	// Apply localized dimensions safely.
	endpoint.mu.Lock()
	endpoint.transitBuckets[idx].KVBlocks += actualCost.KVBlocks
	endpoint.transitBuckets[idx].ActiveRequests += actualCost.ActiveRequests
	endpoint.mu.Unlock()

	// Apply global dimensions entirely lock-free.
	l.globalTransit[idx].KVBlocks.Add(actualCost.KVBlocks)
	l.globalTransit[idx].ActiveRequests.Add(actualCost.ActiveRequests)

	return &CommitReceipt{
		ActualCost: actualCost,
		Epoch:      epoch,
	}, nil
}

// ReleaseEndpointCapacity applies the exact "Net-Transit" math.
//   - Spatial state (KVBlocks, ActiveReqs) dynamically reconciles.
//   - Throughput state (PrefillTokens, DecodeTokens) is unconditionally refunded without transit
//     interval windowing.
func (l *TwoTierLedger) ReleaseEndpointCapacity(endpointID string, receipt *CommitReceipt) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return
	}
	endpoint := value.(*endpointLedger)

	idx := receipt.Epoch % 3

	endpoint.endpointTracking.PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
	endpoint.endpointTracking.DecodeTokens.Add(-receipt.ActualCost.DecodeTokens)
	l.globalTracking.PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
	l.globalTracking.DecodeTokens.Add(-receipt.ActualCost.DecodeTokens)

	// Temporal window reconciliation
	endpoint.mu.Lock()
	currentEpoch := l.globalEpoch.Load()
	shouldSubtract := (currentEpoch - receipt.Epoch) < 2
	if shouldSubtract {
		endpoint.transitBuckets[idx].KVBlocks -= receipt.ActualCost.KVBlocks
		endpoint.transitBuckets[idx].ActiveRequests -= receipt.ActualCost.ActiveRequests
	}
	endpoint.mu.Unlock()

	if shouldSubtract {
		// Subtract from standard vectors using CAS to prevent negative Temporary Allocations.
		// Since subtraction logic might run an instant after Master Tick has zeroed that bucket,
		// the floor safeguards the vector from rolling negative values.
		subAtomicNoNegative(&l.globalTransit[idx].KVBlocks, receipt.ActualCost.KVBlocks)
		subAtomicNoNegative(&l.globalTransit[idx].ActiveRequests, receipt.ActualCost.ActiveRequests)
	}
}

func subAtomicNoNegative(val *atomic.Int64, delta int64) {
	for {
		old := val.Load()
		new := max(old-delta, 0)
		if val.CompareAndSwap(old, new) {
			break
		}
	}
}

// UpdateEndpointConfig modifies high benchmarks to scale topology dimensions.
func (l *TwoTierLedger) UpdateEndpointConfig(endpointID string, cfg EndpointConfig) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		value, _ = l.endpointLedgers.LoadOrStore(endpointID, &endpointLedger{})
	}
	endpoint := value.(*endpointLedger)

	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()

	if cfg.Limits != nil {
		oldPrefill := endpoint.limit.PrefillTokens.Swap(cfg.Limits.PrefillTokens)
		oldDecode := endpoint.limit.DecodeTokens.Swap(cfg.Limits.DecodeTokens)
		oldKV := endpoint.limit.KVBlocks.Swap(cfg.Limits.KVBlocks)
		oldActive := endpoint.limit.ActiveRequests.Swap(cfg.Limits.ActiveRequests)

		l.globalLimit.PrefillTokens.Add(cfg.Limits.PrefillTokens - oldPrefill)
		l.globalLimit.DecodeTokens.Add(cfg.Limits.DecodeTokens - oldDecode)
		l.globalLimit.KVBlocks.Add(cfg.Limits.KVBlocks - oldKV)
		l.globalLimit.ActiveRequests.Add(cfg.Limits.ActiveRequests - oldActive)
	}

	if cfg.TotalKVBlocks != nil {
		oldKV := endpoint.limit.KVBlocks.Swap(*cfg.TotalKVBlocks)
		l.globalLimit.KVBlocks.Add(*cfg.TotalKVBlocks - oldKV)
	}

	if cfg.MaxActiveRequests != nil {
		oldActive := endpoint.limit.ActiveRequests.Swap(*cfg.MaxActiveRequests)
		l.globalLimit.ActiveRequests.Add(*cfg.MaxActiveRequests - oldActive)
	}
}

// ReconcileEndpointCapacity incorporates official real-time state via a polled baseline overwrite.
func (l *TwoTierLedger) ReconcileEndpointCapacity(endpointID string, scrapedUsage ResourceVector) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return
	}
	endpoint := value.(*endpointLedger)

	// Reconcile Baseline (Spatially Tracked Dimensions Only)
	deltaKV := scrapedUsage.KVBlocks - endpoint.scrapedBaseline.KVBlocks.Swap(scrapedUsage.KVBlocks)
	deltaActive := scrapedUsage.ActiveRequests - endpoint.scrapedBaseline.ActiveRequests.Swap(scrapedUsage.ActiveRequests)

	// Flow the resulting delta synchronously upwards into the aggregate view.
	l.globalScraped.KVBlocks.Add(deltaKV)
	l.globalScraped.ActiveRequests.Add(deltaActive)
}

// RemoveEndpoint safely unregisters a pod from the hypervisor.
// It subtracts the endpoint's configured limits, active baselines, and ephemeral transit debt
// from the global aggregate vectors to prevent Flow Control from hallucinating ghost capacity.
func (l *TwoTierLedger) RemoveEndpoint(endpointID string) {
	value, loaded := l.endpointLedgers.LoadAndDelete(endpointID)
	if !loaded {
		return
	}
	endpoint := value.(*endpointLedger)

	// Remove limits from global pool.
	l.globalLimit.PrefillTokens.Add(-endpoint.limit.PrefillTokens.Load())
	l.globalLimit.DecodeTokens.Add(-endpoint.limit.DecodeTokens.Load())
	l.globalLimit.KVBlocks.Add(-endpoint.limit.KVBlocks.Load())
	l.globalLimit.ActiveRequests.Add(-endpoint.limit.ActiveRequests.Load())

	// Remove currently scraped active usage from global pool (Spatial Only).
	l.globalScraped.KVBlocks.Add(-endpoint.scrapedBaseline.KVBlocks.Load())
	l.globalScraped.ActiveRequests.Add(-endpoint.scrapedBaseline.ActiveRequests.Load())

	// Remove long-term tracking counters from global pool (Throughput Only).
	l.globalTracking.PrefillTokens.Add(-endpoint.endpointTracking.PrefillTokens.Load())
	l.globalTracking.DecodeTokens.Add(-endpoint.endpointTracking.DecodeTokens.Load())

	// Remove any inflight transit debt from global pool safely (Spatial Only).
	endpoint.mu.Lock()
	for i := range endpoint.transitBuckets {
		l.globalTransit[i].KVBlocks.Add(-endpoint.transitBuckets[i].KVBlocks)
		l.globalTransit[i].ActiveRequests.Add(-endpoint.transitBuckets[i].ActiveRequests)
	}
	endpoint.mu.Unlock()
}

func (l *TwoTierLedger) GetGlobalHold() ResourceVector {
	return l.globalHold.load()
}

func (l *TwoTierLedger) GetEndpointSnapshot(endpointID string) (limits, committed, scraped ResourceVector, ok bool) {
	value, exists := l.endpointLedgers.Load(endpointID)
	if !exists {
		return ResourceVector{}, ResourceVector{}, ResourceVector{}, false
	}
	endpoint := value.(*endpointLedger)

	limits = endpoint.limit.load()
	scraped = endpoint.scrapedBaseline.load()

	endpoint.mu.Lock()
	// Only sum transit buckets that haven't expired beyond the temporal netting window.
	// We aggregate the entire array since obsolete buckets are actively zeroed by the master tick.
	for i := range 3 {
		committed.KVBlocks += endpoint.transitBuckets[i].KVBlocks
		committed.ActiveRequests += endpoint.transitBuckets[i].ActiveRequests
	}
	endpoint.mu.Unlock()

	// Throughput accounting has no transit debt logic - Commits immediately deduct from dynamic state.
	// But we expose the tracking counters over the endpoint to see throughput utilization history.
	committed.PrefillTokens = endpoint.endpointTracking.PrefillTokens.Load()
	committed.DecodeTokens = endpoint.endpointTracking.DecodeTokens.Load()

	return limits, committed, scraped, true
}
