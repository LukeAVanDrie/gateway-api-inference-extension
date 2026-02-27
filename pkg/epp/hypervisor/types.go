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

// Package hypervisor implements a distributed capacity tracking and admission control plane
// designed specifically for continuous-batching LLM inference engines.
// By mapping dynamic traffic to physical GPU constraints (VRAM, FLOPs, HBM Bandwidth), it shifts
// Head-of-Line (HoL) blocking left to the Gateway. This minimizes scheduling regret and keeps local
// endpoint queues "just full enough" for optimal batch formation, preventing severe latency
// degradation caused by KV cache swap-thrashing or local queue collapse.
package hypervisor

import (
	"context"
	"errors"
	"sync/atomic"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
)

// TokenEstimator predicts the physical GPU cost of an incoming LLM request.
type TokenEstimator interface {
	// Estimate (Hot Path): Called by Flow Control BEFORE admission.
	// Returns the pessimistic worst-case ResourceVector.
	Estimate(flow flowcontrol.FlowKey, targetModel, baseModel string, promptTokens, maxNewTokens int64, blockSize int64) ResourceVector

	// Observe (Cold Path): Called by the OnResponseComplete lifecycle hook.
	// Feeds actual generated token counts back into the EMA learning models.
	Observe(flow flowcontrol.FlowKey, targetModel, baseModel string, actualGeneratedTokens int64)
}

// ResourceVector houses the four physical and logical currencies that govern LLM generation.
//
// LLM generation physics do not scale linearly. They are bottlenecked by either the GPU's Compute
// capacity (FLOPs) or its Memory Bandwidth (the bus to HBM), depending on the phase of generation.
// This vector allows the hypervisor to strictly pack continuous batching queues without violating
// the underlying hardware Roofline model.
type ResourceVector struct {
	// PrefillTokens bounds the GPU's Streaming Multiprocessor (SM) FLOP capacity.
	// This is a Compute limit heavily consumed during the prompt phase. It is released immediately at
	// Time-To-First-Token (TTFT) to allow the admission of the next prompt.
	PrefillTokens int64

	// DecodeTokens bounds the GPU Memory Bandwidth (HBM bus).
	// Because autoregressive generation requires reading the entire KV cache for every token,
	// bandwidth is the primary bottleneck during decoding. This is held for the lifetime of the
	// request to guarantee the hardware can finish what it starts, preventing "decode shock".
	DecodeTokens int64

	// KVBlocks bounds the strict spatial VRAM capacity of the GPU.
	// It represents the maximum number of PagedAttention blocks required to serve the request.
	// Tracking this at the Gateway prevents the endpoint from over-saturating its KV cache, which
	// would otherwise force it to preempt requests and swap blocks to CPU RAM, destroying generation
	// throughput.
	KVBlocks int64

	// ActiveRequests forces rigid logical concurrency limits.
	// It mirrors the endpoint's limit (e.g., vLLM's 'max_num_seqs') to prevent excessive context
	// switching.
	ActiveRequests int64
}

// HoldReceipt acknowledges a pessimistic, global reservation of capacity.
// It serves as a fast-path admission lock that prevents "thundering herds" from oversubscribing the
// aggregate pool capacity before the Scheduler can route requests.
// This struct is passed by value (32 bytes) to completely eliminate heap allocations on the hot path.
type HoldReceipt struct {
	Held ResourceVector
}

// CommitReceipt firmly binds a finalized execution to a specific endpoint and temporal epoch.
// It bridges the gap between network transit times and authoritative scraper baselines, allowing
// for precise net-transit debt calculation and lock-free lifecycle releases.
type CommitReceipt struct {
	ActualCost ResourceVector
	Epoch      uint64

	// PrefillReleased guarantees idempotency during the 2-stage lifecycle release.
	// Because asynchronous network streams or sidecar proxy headers can race or delay depending on
	// topology, this ensures Prefill capacity is never double-subtracted if TTFT and request
	// completion hooks fire concurrently.
	PrefillReleased atomic.Bool
}

// EndpointConfigPatch defines dynamic, top-down configuration overrides for a specific endpoint.
// It is used to propagate rigid physical limits (discovered via hardware metrics) into the
// hypervisor's tracking ledgers.
// Fields left nil denote no change to that dimension.
type EndpointConfigPatch struct {
	Limits            *ResourceVector
	TotalKVBlocks     *int64
	MaxActiveRequests *int64
}

// AdmissionLedger is the ultra-hot path interface used by request pipelines and the Scheduler.
// It integrates O(1) global admission checks with O(N) localized, prefix-cache-aware scheduling
// vectors, ensuring the proxy never hallucinates capacity while surviving extreme concurrency.
type AdmissionLedger interface {
	// TryAcquireHold performs a synchronous, O(1) global admission check.
	// It reserves pessimistic capacity across the pool. If the aggregate fleet is saturated, it
	// returns ErrGlobalCapacityExceeded to apply immediate backpressure.
	TryAcquireHold(ctx context.Context, worstCase ResourceVector) (HoldReceipt, error)

	// ReleaseHold refunds un-committed resources back to the pool.
	// It is used to revert reservations if a request is dropped, fails to route, or is immediately
	// rejected by the endpoint prior to a successful Commit.
	ReleaseHold(ctx context.Context, receipt HoldReceipt)

	// Commit atomically binds a global Hold to a specific, scheduled endpoint.
	// It reconciles the pessimistic Hold against actual calculated costs (e.g., after prefix cache
	// discounts are applied) and locks the temporal execution epoch.
	Commit(ctx context.Context, endpointID string, actualCost ResourceVector, receipt HoldReceipt) (*CommitReceipt, error)

	// ReleasePrefillCapacity frees the Compute (FLOP) reservation for a request.
	// This should be called the exact moment the endpoint achieves Time-To-First-Token (TTFT),
	// signaling that the GPU SMs are ready to ingest the next prompt into the continuous batch.
	ReleasePrefillCapacity(ctx context.Context, endpointID string, receipt *CommitReceipt)

	// ReleaseEndpointCapacity frees the Spatial (KV VRAM) and Bandwidth (Decode) reservations.
	// It must be called when the request fully terminates (success or client disconnect).
	// If the request terminated prematurely (prior to TTFT), this method will securely utilize
	// the CommitReceipt's atomic lock to clean up any dangling Prefill Compute reservations.
	// It executes strict net-transit math against the temporal epoch to prevent capacity
	// hallucinations during scraping intervals.
	ReleaseEndpointCapacity(ctx context.Context, endpointID string, receipt *CommitReceipt)
}

// TopologyRegistry is used by the cluster controller to manage capacity bounds over time.
type TopologyRegistry interface {
	// UpdateEndpointConfig pushes authoritative topology dimensions (e.g., physical VRAM limits)
	// directly into the routing state.
	UpdateEndpointConfig(ctx context.Context, endpointID string, cfg EndpointConfigPatch)

	// RemoveEndpoint safely unregisters a pod from the hypervisor, securely purging its limits and
	// transit debt from the global aggregate pools.
	RemoveEndpoint(ctx context.Context, endpointID string)
}

// TelemetryObserver is used by the TelemetryBridge to feed scraped data and check utilization.
type TelemetryObserver interface {
	ReconcileEndpointCapacity(endpointID string, scrapedUsage ResourceVector)

	// GetGlobalHold returns the current aggregate global ephemeral reservation.
	GetGlobalHold(ctx context.Context) ResourceVector

	// GetEndpointSnapshot returns a consistent view of a specific endpoint's limits,
	// committed capacity (tracking + transit), and the scraped baseline.
	GetEndpointSnapshot(ctx context.Context, endpointID string) (limits, committed, scraped ResourceVector, ok bool)
}

var (
	// ErrGlobalCapacityExceeded is returned when the aggregate pool is saturated across any of the
	// four physical/logical dimensions, triggering Gateway-level backpressure.
	ErrGlobalCapacityExceeded = errors.New("global capacity exceeded")

	// ErrEndpointNotFound is returned if modifications are pushed or workloads are committed to a
	// endpoint that has not been registered with the hypervisor.
	ErrEndpointNotFound = errors.New("endpoint not found")
)
