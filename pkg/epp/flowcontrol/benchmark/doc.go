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

// Package benchmark implements a high-fidelity, synchronous steady-state pipeline
// for load-testing the Flow Control layer.
//
// # Benchmark Methodology: The Synchronous Steady-State Pipeline
//
// Benchmarking the Flow Control layer presents a fundamental impedance mismatch: data plane
// routing executes in microseconds, while downstream LLM inference takes O(seconds). Simulating
// this downstream latency via arbitrary thread sleeps or I/O inherently poisons the CPU profile.
// The Go runtime parks the blocked goroutines, artificially idling the CPU and reducing the
// benchmark to a measure of scheduler wake-up latency rather than the Flow Control layer's true
// computational throughput.
//
// To achieve a structurally pure measurement, this harness implements a Synchronous Steady-State
// Pipeline:
//
//  1. Structural Backpressure (W > L): By driving Ingress Concurrency (W) against a strict Egress
//     Capacity Limit (L), exactly (W - L) requests are deterministically forced into the Flow
//     Control layer's queues. This physically simulates a sustained, heavy-tail LLM load spike.
//
//  2. Optimistic Capacity Locking: To prevent causality races where the Flow Control layer outruns
//     the benchmark clients, the mock SaturationDetector acts as an optimistic lock. It grants
//     capacity atomically precisely when the Flow Control layer evaluates the gate.
//
//  3. Immediate Egress Draining: When a client goroutine unblocks (Dispatched), it immediately
//     frees the capacity slot. This triggers the Flow Control layer to dispatch exactly one more
//     item, driving the entire system at absolute maximum CPU speed without a single thread sleep.
//
//  4. Algorithmic Isolation: By artificially holding the queues at a deep, rigid steady-state, the
//     Flow Control layer is forced to continuously evaluate its multi-tenant fairness, strict
//     priority, and saturation gates at hardware limits. This cleanly isolates the algorithmic
//     scaling complexity and memory contention from synthetic sleep jitter.
//
// # Interpreting Metrics in b.RunParallel
//
// In a highly concurrent queuing system, the standard Go benchmark metrics require careful
// interpretation:
//
//  1. ns/op (System-Wide Amortized Time): Because this is a b.RunParallel benchmark, `ns/op` is
//     not latency. It represents inverse throughput. If the system processes 1,000,000 requests
//     in 1 second, it reports 1,000,000 ns/op (meaning the system as a whole completes one
//     operation every 1,000,000 nanoseconds). To get RPS: 1,000,000,000 / ns_per_op.
//     (The `d/s` custom metric does this automatically).
//
//  2. ops (The Definition of an Operation): One "op" is the complete lifecycle of a single
//     simulated request: Ingress, classification, queuing, policy evaluation (Fairness/Ordering),
//     dispatch, and Egress.
//
//  3. allocs/op and B/op (GC Pressure): Crucial for tail-latency. High allocations per request
//     mean the Go Garbage Collector will thrash under load, causing jitter. The ideal state in the
//     hot-path is 0-1 allocs/op.
//
//  4. Saturated Coordinates (W > L): When Concurrency (W) exceeds Capacity (L), `EnqueueAndWait`
//     blocks. Because we utilize a Synchronous Pipeline (immediate release upon dispatch), the
//     duration of an "op" is now strictly governed by the Flow Control layer's CPU overhead. If
//     the CPU is pegged at 100% without lock starvation, the Flow Control layer is operating at
//     its algorithmic speed-of-light.
//
// # Custom Metrics Reported
//
//   - d/s:                  (Dispatches/sec) The primary throughput metric.
//   - r/s:                  (Rejects/sec) Rate of requests rejected due to capacity or timeouts.
//   - errors:               Total unexpected runtime errors encountered during the coordinate.
//   - stable_d/s:           Throughput of healthy requests during mass-cancellation/zombie events.
//   - zombies/s:            Rate of requests hitting context deadlines/TTL during cancellation
//     benchmarks.
//   - burst_dispatches/sec: Drain rate when capacity is instantaneously freed.
package benchmark
