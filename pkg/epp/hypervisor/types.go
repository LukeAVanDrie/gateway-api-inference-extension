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
	"errors"
)

// ResourceVector houses the four physical and logical currencies utilized in Distributed GPU
// Hypervisor mechanisms.
//
// LLM generation is bottlenecked heavily on both physical geometry (Memory Limits) and physical
// bandwidth (Compute Limits).
//   - PrefillTokens are bounded by the SM's physical FLOP capacity.
//   - DecodeTokens are bottlenecked heavily by GPU Memory Bandwidth (the bus to HBM).
//   - KVBlocks is a spatially strict VRAM upper-limit for generation logic, preventing CUDA OOM (Out
//     Of Memory) scenarios and massive page swapping overhead.
//   - ActiveRequests forces rigid execution limits and represents the logical concurrency.
type ResourceVector struct {
	PrefillTokens  int64
	DecodeTokens   int64
	KVBlocks       int64
	ActiveRequests int64
}

// HoldReceipt acknowledges a locked estimation of needed capacity that has passed
// global aggregate admission checks. It prevents catastrophic cascading overhead
// by ensuring capacities are projected prior to actual execution schedules.
type HoldReceipt struct {
	Held ResourceVector
}

// CommitReceipt firmly binds a finalized execution to the temporal epoch it was admitted in.
// This allows flawless "Net-Transit" subtraction upon completion, bridging the temporal gap between
// network transit times and authoritative scraper baselines.
type CommitReceipt struct {
	ActualCost ResourceVector
	Epoch      uint64
}

// struct and unpack config.
type EndpointConfig struct {
	Limits            *ResourceVector
	TotalKVBlocks     *int64
	MaxActiveRequests *int64
}

// TokenLedger is the central registry integrating O(1) global estimations with O(N) localized
// cache-aware scheduling vectors.
// It acts as the primary admission controller mediating request-level routing vs node capacities.
type TokenLedger interface {
	// RunMasterTick initiates the background temporal epoch engine.
	// It must be launched in a background goroutine on startup to advance the sliding window and
	// permanently purge the oldest transit debt.
	RunMasterTick(ctx context.Context)

	// TryAcquireHold applies an O(1) synchronous global admission check.
	// It performs conservative fast-path reservation that is highly responsive and immune to
	// distributed thundering-herd livelocks.
	TryAcquireHold(worstCase ResourceVector) (*HoldReceipt, error)

	// ReleaseHold refunds un-committed resources without any adverse execution.
	// It must be called (often via defer) to prevent resource leaks if a request fails to route or
	// schedule after admission.
	ReleaseHold(receipt *HoldReceipt)

	// Commit applies an actual realized usage scaling for local and global vectors.
	// It reconciles the estimation, refunds the worst-case Hold, and returns a temporal CommitReceipt
	// for eventual netting.
	Commit(endpointID string, actualCost ResourceVector, receipt *HoldReceipt) (*CommitReceipt, error)

	// ReleaseEndpointCapacity scales back locally used metrics after request termination.
	// It uses the CommitReceipt's temporal epoch to execute strict Net-Transit math, preventing
	// capacity hallucinations during scraping intervals.
	ReleaseEndpointCapacity(endpointID string, receipt *CommitReceipt)

	// UpdateEndpointConfig pushes updated state configurations to securely adjust endpoints limits,
	// max concurrent sequences, and cache blocks.
	UpdateEndpointConfig(endpointID string, cfg EndpointConfig)

	// RemoveEndpoint safely unregisters a pod from the hypervisor and purget its usage vectors from
	// the aggregate pools.
	RemoveEndpoint(endpointID string)

	// ReconcileEndpointCapacity incorporates authoritative real-time state via a polled baseline
	// overwrite, propagating deltas synchronously upwards to the aggregate view.
	ReconcileEndpointCapacity(endpointID string, scrapedUsage ResourceVector)

	// GetGlobalHold returns the current active worldwide conservative reservation vector.
	GetGlobalHold() ResourceVector

	// GetEndpointSnapshot returns localized endpoint vectors for Prometheus scrape visualization.
	GetEndpointSnapshot(endpointID string) (limits, committed, scraped ResourceVector, ok bool)
}

var (
	// ErrGlobalCapacityExceeded is returned during admission if global utilization triggers aggregate
	// saturation across any physical or logical dimension.
	ErrGlobalCapacityExceeded = errors.New("global capacity exceeded")

	// ErrEndpointNotFound is returned if modifications are pushed or workloads are committed to a
	// backend that has not been initialized in the topology.
	ErrEndpointNotFound = errors.New("endpoint not found")
)
