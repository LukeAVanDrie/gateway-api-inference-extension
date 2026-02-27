/*
Copyright 2026 The Kubernetes Authors.

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

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/datalayer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/requestcontrol"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/scheduling"
	attrprefix "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

const LedgerPluginType = "two-tier-ledger"

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

	endpointLedgers sync.Map     // map[string]*endpointLedger
	transitMu       sync.RWMutex // Protects temporal epoch transitions from late-binding racers

	estimator TokenEstimator // Injected to close the ML feedback loop on request completion
}

// NewTwoTierLedger initializes the ledger with its dependencies.
func NewTwoTierLedger(est TokenEstimator) *TwoTierLedger {
	return &TwoTierLedger{
		estimator: est,
	}
}

// Start initiates the background temporal epoch engine.
// It advances the sliding window every 50ms to permanently purge the oldest transit debt, keeping
// the hypervisor synchronized with the observability scraping engine.
// Implements manager.Runnable for the Kubernetes controller manager.
func (l *TwoTierLedger) Start(ctx context.Context) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			l.transitMu.Lock()
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
			l.transitMu.Unlock()
		}
	}
}

// --- Admission Control (Fast-Path) ---

// TryAcquireHold applies the O(1) global admission check synchronously.
// It serves as a fast-path admission lock that prevents Gateway-level thundering herds from
// oversubscribing the aggregate cluster capacity before the Scheduler can route requests.
// Returns HoldReceipt by value to prevent heap allocations on the hot path.
func (l *TwoTierLedger) TryAcquireHold(ctx context.Context, worstCase ResourceVector) (HoldReceipt, error) {
	epoch := l.globalEpoch.Load()
	idx := epoch % 3
	prevIdx := (epoch + 2) % 3 // strictly evaluates current and previous transit windows

	l.admissionMu.Lock()
	defer l.admissionMu.Unlock()

	// Proactively ensure at least one backend can theoretically fit the request to prevent holding
	// global capacity for a payload that cannot be scheduled.
	hasCapacity := false
	l.endpointLedgers.Range(func(key, value any) bool {
		endpoint := value.(*endpointLedger)
		limit := endpoint.limit.load()
		scraped := endpoint.scrapedBaseline.load()

		availKV := limit.KVBlocks - scraped.KVBlocks
		availActive := limit.ActiveRequests - scraped.ActiveRequests
		availPrefill := limit.PrefillTokens - endpoint.endpointTracking.PrefillTokens.Load()
		availDecode := limit.DecodeTokens - endpoint.endpointTracking.DecodeTokens.Load()

		endpoint.mu.Lock()
		availKV -= endpoint.transitBuckets[idx].KVBlocks + endpoint.transitBuckets[prevIdx].KVBlocks
		availActive -= endpoint.transitBuckets[idx].ActiveRequests + endpoint.transitBuckets[prevIdx].ActiveRequests
		endpoint.mu.Unlock()

		if worstCase.KVBlocks <= availKV && worstCase.ActiveRequests <= availActive &&
			worstCase.PrefillTokens <= availPrefill && worstCase.DecodeTokens <= availDecode {
			hasCapacity = true
			return false // short circuit
		}
		return true
	})

	if !hasCapacity {
		return HoldReceipt{}, ErrGlobalCapacityExceeded
	}

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
		return HoldReceipt{}, ErrGlobalCapacityExceeded
	}

	// Apply aggregation holds unconditionally upon success.
	l.globalHold.PrefillTokens.Add(worstCase.PrefillTokens)
	l.globalHold.DecodeTokens.Add(worstCase.DecodeTokens)
	l.globalHold.KVBlocks.Add(worstCase.KVBlocks)
	l.globalHold.ActiveRequests.Add(worstCase.ActiveRequests)

	return HoldReceipt{Held: worstCase}, nil
}

// ReleaseHold refunds un-committed resources safely back into the ephemeral pool.
func (l *TwoTierLedger) ReleaseHold(ctx context.Context, receipt HoldReceipt) {
	l.globalHold.PrefillTokens.Add(-receipt.Held.PrefillTokens)
	l.globalHold.DecodeTokens.Add(-receipt.Held.DecodeTokens)
	l.globalHold.KVBlocks.Add(-receipt.Held.KVBlocks)
	l.globalHold.ActiveRequests.Add(-receipt.Held.ActiveRequests)
}

// Commit elevates a global HoldReceipt into an accurate CommitReceipt inside the localized tracking,
// officially shifting capacity from the ephemeral pool into the dynamic temporal net-transit slice.
// Returns a CommitReceipt tying this workload to the exact temporal epoch of admission.
func (l *TwoTierLedger) Commit(ctx context.Context, endpointID string, actualCost ResourceVector, receipt HoldReceipt) (*CommitReceipt, error) {
	defer l.ReleaseHold(ctx, receipt)

	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return nil, ErrEndpointNotFound
	}
	endpoint := value.(*endpointLedger)

	endpoint.endpointTracking.PrefillTokens.Add(actualCost.PrefillTokens)
	endpoint.endpointTracking.DecodeTokens.Add(actualCost.DecodeTokens)
	l.globalTracking.PrefillTokens.Add(actualCost.PrefillTokens)
	l.globalTracking.DecodeTokens.Add(actualCost.DecodeTokens)

	l.transitMu.RLock()
	epoch := l.globalEpoch.Load()
	idx := epoch % 3

	// Apply localized dimensions safely.
	endpoint.mu.Lock()
	endpoint.transitBuckets[idx].KVBlocks += actualCost.KVBlocks
	endpoint.transitBuckets[idx].ActiveRequests += actualCost.ActiveRequests
	endpoint.mu.Unlock()

	l.globalTransit[idx].KVBlocks.Add(actualCost.KVBlocks)
	l.globalTransit[idx].ActiveRequests.Add(actualCost.ActiveRequests)
	l.transitMu.RUnlock()

	return &CommitReceipt{
		ActualCost: actualCost,
		Epoch:      epoch,
	}, nil
}

// ReleasePrefillCapacity securely and idempotently deducts PrefillTokens (Compute FLOPs).
// It should be called the exact moment Time-To-First-Token (TTFT) is achieved.
func (l *TwoTierLedger) ReleasePrefillCapacity(ctx context.Context, endpointID string, receipt *CommitReceipt) {
	if receipt == nil || !receipt.PrefillReleased.CompareAndSwap(false, true) {
		return // Idempotency check: Already released (prevents double-subtraction)
	}

	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return
	}
	endpoint := value.(*endpointLedger)

	// Deduct the compute rate only.
	endpoint.endpointTracking.PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
	l.globalTracking.PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
}

// ReleaseEndpointCapacity applies exact "net-transit" math to scale back Spatial metrics (KVBlocks,
// ActiveReqs) and unconditionally refunds the Bandwidth rate (DecodeTokens).
func (l *TwoTierLedger) ReleaseEndpointCapacity(ctx context.Context, endpointID string, receipt *CommitReceipt) {
	if receipt == nil {
		return
	}

	// Idempotent fallback: If streaming was disabled and ResponseReceived (TTFT) never fired, release
	// the Prefill tokens now to prevent a permanent FLOP capacity leak.
	if receipt.PrefillReleased.CompareAndSwap(false, true) {
		value, ok := l.endpointLedgers.Load(endpointID)
		if ok {
			endpoint := value.(*endpointLedger)
			endpoint.endpointTracking.PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
			l.globalTracking.PrefillTokens.Add(-receipt.ActualCost.PrefillTokens)
		}
	}

	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		return
	}
	endpoint := value.(*endpointLedger)
	idx := receipt.Epoch % 3

	// Unconditionally release Bandwidth (DecodeTokens).
	endpoint.endpointTracking.DecodeTokens.Add(-receipt.ActualCost.DecodeTokens)
	l.globalTracking.DecodeTokens.Add(-receipt.ActualCost.DecodeTokens)

	// Temporal window reconciliation for Spatial Occupancy (KVBlocks, ActiveRequests)
	l.transitMu.RLock()
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
		// Since subtraction logic might run an instant after Master Tick has zeroed that bucket, the
		// floor safeguards the vector from rolling negative values.
		subAtomicNoNegative(&l.globalTransit[idx].KVBlocks, receipt.ActualCost.KVBlocks)
		subAtomicNoNegative(&l.globalTransit[idx].ActiveRequests, receipt.ActualCost.ActiveRequests)
	}
	l.transitMu.RUnlock()
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

// --- Topology & Telemetry Control Plane ---

// UpdateEndpointConfig modifies high benchmarks to scale topology dimensions.
func (l *TwoTierLedger) UpdateEndpointConfig(ctx context.Context, endpointID string, patch EndpointConfigPatch) {
	value, ok := l.endpointLedgers.Load(endpointID)
	if !ok {
		value, _ = l.endpointLedgers.LoadOrStore(endpointID, &endpointLedger{})
	}
	endpoint := value.(*endpointLedger)

	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()

	if patch.Limits != nil {
		oldPrefill := endpoint.limit.PrefillTokens.Swap(patch.Limits.PrefillTokens)
		oldDecode := endpoint.limit.DecodeTokens.Swap(patch.Limits.DecodeTokens)
		oldKV := endpoint.limit.KVBlocks.Swap(patch.Limits.KVBlocks)
		oldActive := endpoint.limit.ActiveRequests.Swap(patch.Limits.ActiveRequests)

		l.globalLimit.PrefillTokens.Add(patch.Limits.PrefillTokens - oldPrefill)
		l.globalLimit.DecodeTokens.Add(patch.Limits.DecodeTokens - oldDecode)
		l.globalLimit.KVBlocks.Add(patch.Limits.KVBlocks - oldKV)
		l.globalLimit.ActiveRequests.Add(patch.Limits.ActiveRequests - oldActive)
	}

	if patch.TotalKVBlocks != nil {
		oldKV := endpoint.limit.KVBlocks.Swap(*patch.TotalKVBlocks)
		l.globalLimit.KVBlocks.Add(*patch.TotalKVBlocks - oldKV)
	}

	if patch.MaxActiveRequests != nil {
		oldActive := endpoint.limit.ActiveRequests.Swap(*patch.MaxActiveRequests)
		l.globalLimit.ActiveRequests.Add(*patch.MaxActiveRequests - oldActive)
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
func (l *TwoTierLedger) RemoveEndpoint(ctx context.Context, endpointID string) {
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

func (l *TwoTierLedger) GetGlobalHold(_ context.Context) ResourceVector {
	return l.globalHold.load()
}

func (l *TwoTierLedger) GetEndpointSnapshot(_ context.Context, endpointID string) (limits, committed, scraped ResourceVector, ok bool) {
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

// --- Gateway API Extension Plugins ---

var (
	_ manager.Runnable                = (*TwoTierLedger)(nil)
	_ scheduling.Filter               = (*TwoTierLedger)(nil)
	_ requestcontrol.PreRequest       = (*TwoTierLedger)(nil)
	_ requestcontrol.ResponseReceived = (*TwoTierLedger)(nil)
	_ requestcontrol.ResponseComplete = (*TwoTierLedger)(nil)
)

func (l *TwoTierLedger) TypedName() plugin.TypedName {
	return plugin.TypedName{
		Type: LedgerPluginType,
		Name: LedgerPluginType,
	}
}

// Filter prevents the Scheduler from routing to a pod that lacks physical capacity for the Hold.
func (l *TwoTierLedger) Filter(ctx context.Context, cycleState *scheduling.CycleState, request *scheduling.LLMRequest, pods []scheduling.Endpoint) []scheduling.Endpoint {
	logger := log.FromContext(ctx)

	worstCase := request.HoldReceipt.(HoldReceipt).Held

	var filtered []scheduling.Endpoint
	for _, pod := range pods {
		endpointID := pod.GetMetadata().NamespacedName.String()

		if value, ok := l.endpointLedgers.Load(endpointID); ok {
			endpoint := value.(*endpointLedger)
			limits := endpoint.limit.load()
			scraped := endpoint.scrapedBaseline.load()

			availKV := limits.KVBlocks - scraped.KVBlocks
			availActive := limits.ActiveRequests - scraped.ActiveRequests

			// Subtract inflight transit debt.
			epoch := l.globalEpoch.Load()
			idx := epoch % 3
			prevIdx := (epoch + 2) % 3

			endpoint.mu.Lock()
			availKV -= endpoint.transitBuckets[idx].KVBlocks + endpoint.transitBuckets[prevIdx].KVBlocks
			availActive -= endpoint.transitBuckets[idx].ActiveRequests + endpoint.transitBuckets[prevIdx].ActiveRequests
			endpoint.mu.Unlock()

			if worstCase.KVBlocks <= availKV && worstCase.ActiveRequests <= availActive {
				filtered = append(filtered, pod)
			}
		}
	}

	// Fail-Open to prevent strict shedding during TOCTOU concurrency races.
	// If the resulting slice is empty, a race stole the last drop of perfect capacity globally.
	// We return the unfiltered list, allowing Scorers to pick the best overloaded pod, treating the
	// the endpoint's local queue as a micro-burst shock absorber.
	if len(filtered) == 0 {
		logger.V(1).Info("TOCTOU race detected: candidate pods exhausted spatial capacity. Failing open to prevent shedding.")
		return pods
	}

	return filtered
}

// PreRequest locks the global hold to the specific backend chosen by the Scheduler.
func (l *TwoTierLedger) PreRequest(ctx context.Context, request *scheduling.LLMRequest, schedulingResult *scheduling.SchedulingResult) {
	primaryProfile := schedulingResult.ProfileResults[schedulingResult.PrimaryProfileName]
	if primaryProfile == nil || len(primaryProfile.TargetEndpoints) == 0 {
		return
	}

	targetEndpoint := primaryProfile.TargetEndpoints[0]
	endpointID := targetEndpoint.GetMetadata().NamespacedName.String()

	actualCost := request.HoldReceipt.(HoldReceipt).Held
	if matchInfoRaw, ok := targetEndpoint.Get(attrprefix.PrefixCacheMatchInfoKey); ok {
		if matchInfo, ok := matchInfoRaw.(*attrprefix.PrefixCacheMatchInfo); ok {
			totalBlocks := matchInfo.TotalBlocks()
			if totalBlocks > 0 {
				score := float64(matchInfo.MatchBlocks()) / float64(totalBlocks)
				log.FromContext(ctx).Info("Applying prefix cache match discount to actualCost", "score", score)
				actualCost.KVBlocks = int64(float64(actualCost.KVBlocks) * (1.0 - score))
			}
		}
	}

	commitReceipt, err := l.Commit(ctx, endpointID, actualCost, request.HoldReceipt.(HoldReceipt))
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to commit resources to ledger")
		return
	}
	request.CommitReceipt = commitReceipt
}

// ResponseReceived fires when the HTTP headers arrive (Time-To-First-Token).
// We release the Prefill FLOPs immediately so the backend can admit the next prompt.
func (l *TwoTierLedger) ResponseReceived(ctx context.Context, request *scheduling.LLMRequest, response *requestcontrol.Response, targetEndpoint *datalayer.EndpointMetadata) {
	if request.CommitReceipt != nil {
		endpointID := targetEndpoint.NamespacedName.String()
		_commitReceipt := request.CommitReceipt.(*CommitReceipt)
		l.ReleasePrefillCapacity(ctx, endpointID, _commitReceipt)
	}
}

// ResponseComplete fires when the request entirely finishes.
func (l *TwoTierLedger) ResponseComplete(ctx context.Context, request *scheduling.LLMRequest, response *requestcontrol.Response, targetEndpoint *datalayer.EndpointMetadata) {
	if request.CommitReceipt != nil {
		endpointID := targetEndpoint.NamespacedName.String()
		_commitReceipt := request.CommitReceipt.(*CommitReceipt)
		l.ReleaseEndpointCapacity(ctx, endpointID, _commitReceipt)
	}

	if response.Usage.CompletionTokens > 0 {
		flowKey := flowcontrol.FlowKey{Priority: request.Objectives.Priority}
		l.estimator.Observe(flowKey, request.TargetModel, request.BaseModel, int64(response.Usage.CompletionTokens))
	}
}
