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

// Package controller contains the concrete implementation of the FlowController engine responsible for orchestrating
// the flow control framework with its pluggable policies and queues.
package controller

import (
	"context"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
)

// FlowController is the top-level engine that manages a pool of shardProcessor workers and distributes incoming
// requests across them. It is the primary public entry point for the entire flow control system and is responsible for
// orchestrating the flow control framework with its pluggable policies and queues.
//
// NOTE: This is a temporary, conceptual interface for reviewing purposes. It will be replaced with its concrete
// implementation.
type FlowController interface {
	// EnqueueAndWait submits a request for flow control management. This method synchronously blocks the calling
	// goroutine until the request's processing is finalized.
	//
	// Finalization means the request has reached a terminal state:
	//   - Dispatch: The request passed all checks and was unblocked for the caller to proceed.
	//   - Rejection: The request was rejected before or during enqueueing (e.g., due to capacity limits, external context
	//     cancellation, etc.).
	//   - Eviction: The request was removed from a queue after being enqueued (e.g., due to TTL expiry, external context
	//     cancellation, displacement, etc.).
	//
	// Returns:
	//   - types.QueueOutcome: A concise enum indicating the final status of the request's lifecycle.
	//   - error: Non-nil if the outcome is not types.QueueOutcomeDispatched. The error will wrap either types.ErrRejected
	// 		 or types.ErrEvicted. Callers can use errors.Is() for inspection and then unwrap further for specific sentinel
	//     errors.
	EnqueueAndWait(req types.FlowControlRequest) (types.QueueOutcome, error)

	// Run starts the FlowController's main loop and the lifecycle management of its worker pool.
	//
	// Its primary responsibilities are:
	//   1. Request Distribution: Handling incoming requests from EnqueueAndWait and distributing them to the appropriate
	//      worker using a Join-the-Shortest-Queue-by-Bytes (JSQ-Bytes) algorithm to balance load.
	//   2. Worker Lifecycle Management: Monitoring the ports.ShardProvider for changes in active shards and dynamically
	//      adjusting the pool of workers by starting new ones or signaling existing ones for graceful shutdown.
	//
	// This method blocks until the provided context is cancelled. It is intended to be called once in its own goroutine.
	Run(ctx context.Context)
}

// shardProcessor defines the contract for a single, stateful worker within the FlowController's pool. Each instance is
// bound 1:1 with a specific ports.RegistryShard and is responsible for the entire request lifecycle on that shard.
//
// NOTE: This is an unexported interface intended for internal use and testing.
//
// # Error Handling Strategy
//
// A shardProcessor's run loop employs a two-tiered error handling strategy to ensure robustness by isolating failures
// to a specific operational context on a specific shard, maximizing overall system availability.
//
//  1. Priority Band Domain (Inter-Flow Operations - "Fail Open"): If an unrecoverable error occurs at the priority
//     band level (e.g., failing to retrieve or apply an InterFlow...Policy), the worker will log the error, skip
//     processing for that specific priority band in the current cycle, and continue to the next available priority
//     band. This promotes work conservation.
//
//  2. Queue Domain (Intra-Flow Operations - "Fail Close for Band"): Once an InterFlow...Policy successfully selects a
//     flow's queue, if an unrecoverable error occurs during the subsequent intra-flow stage (e.g., failing to retrieve
//     or apply an IntraFlow...Policy), the worker will "fail close" for the current priority band. It will log the error
//     and cease further attempts to process other queues from that same band during the current cycle, then move to the
//     next priority band. This prevents stateless inter-flow policies from repeatedly selecting a problematic queue.
type shardProcessor interface {
	// enqueue sends a request to this specific worker for processing. This is a non-blocking method called by the
	// top-level FlowController's central distribution loop.
	enqueue(req types.FlowControlRequest)

	// run starts this worker's main processing loop, which is on the request processing hot path. The loop interleaves
	// accepting new requests from its private channel with attempts to dispatch requests from its managed queues to
	// ensure responsiveness under load.
	run(ctx context.Context)
}
