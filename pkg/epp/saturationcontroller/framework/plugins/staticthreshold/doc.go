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

// Package staticthreshold implements a Saturation Controller strategy based on fixed, operator-defined thresholds.
//
// # Overview
//
// This plugin acts as the default gatekeeper for the Flow Control system. It determines if the backend pool has
// sufficient capacity to accept new requests by comparing real-time backend metrics against static set points.
//
// # Saturation Signals
//
// The controller evaluates three distinct signals. If ANY signal indicates saturation for a specific pod, that pod is
// considered effectively unavailable. If ALL pods are unavailable, the system enforces Head-of-Line (HoL) blocking.
//
//  1. Queue Depth (The Set Point):
//     Compares `waiting_queue_size` against `QueueDepthThreshold`.
//     This is the primary control variable.
//
//  2. KV Cache Utilization (The Safety Ceiling):
//     Compares `kv_cache_usage_percent` against `KVCacheUtilThreshold`.
//     This prevents OOMs and performance degradation from cache thrashing.
//
//  3. Metric Staleness (The Watchdog):
//     Checks if `metrics_update_time` is older than `MetricsStalenessThreshold`.
//     This ensures the system fails closed (stops dispatching) if the metric pipeline breaks.
//
// # Tuning Strategies
//
// This plugin supports two distinct operational modes based on the `QueueDepthThreshold` configuration:
//
//  1. Throughput Mode (Threshold > 0):
//     Setting the threshold to a small integer (e.g., 5-10) allows a buffer of requests to accumulate on the model
//     server. This enables the batching engine (e.g., vLLM) to form larger, more efficient batches, maximizing GPU
//     throughput at the cost of slight queuing latency.
//
//  2. Latency/Fairness Mode (Threshold = 0):
//     Setting the threshold to 0 forces "Just-In-Time" dispatching. Requests are held in the Gateway's Flow Control
//     queues until a backend is strictly idle. This maximizes the efficacy of Gateway-level prioritization and fairness
//     policies but may reduce total GPU throughput due to smaller batches.
package staticthreshold
