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

package benchmark

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/contracts"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/contracts/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/controller"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/fairness"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/ordering"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/registry"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
	testutils "sigs.k8s.io/gateway-api-inference-extension/test/utils"
)

// --- The Performance Hypercube (Matrix Parameters) ---

func init() {
	log.SetLogger(logr.Discard())
}

type (
	// EgressConcurrencyLimit (L) defines the maximum number of in-flight requests the simulated pool
	// can physically sustain.
	// - L = 0: Free-flow. Saturation is disabled. Isolates the raw overhead of request ingestion and
	//   routing without structural queueing.
	// - L > 0: Strict limits. Models a saturated backend, forcing the Flow Control layer to buffer
	//   excess load and continuously execute the dispatch cycle.
	EgressConcurrencyLimit int64

	// ShardCount (S) dictates the internal data parallelism of the Flow Control engine.
	// Sweeping S measures the efficacy of horizontally partitioning the system state.
	// It highlights the fundamental distributed systems tradeoff: reducing lock contention and
	// channel bottlenecks under high concurrency versus the overhead of distributing load and
	// aggregating statistics across multiple parallel execution units.
	ShardCount int

	// PriorityLevels (P) dictates the number of vertically cascading priority tiers.
	// Sweeping P isolates the systemic overhead of strict hierarchical service evaluation.
	// It stresses the necessity to evaluate higher-priority bands completely before yielding
	// dispatch opportunities to lower tiers.
	PriorityLevels int

	// FlowCount (F) dictates the number of unique multi-tenant flows actively competing.
	// Crucially, given a fixed global queue depth (W - L), sweeping F dynamically shifts the
	// topological shape of the buffered load, revealing a critical operational pivot:
	// - Low F: Redistributes the load into a few, very deep queues, aggressively stressing the
	// 	 memory locality and sorting efficiency of the Ordering policies.
	// - High F: Redistributes the load into many, very shallow queues, aggressively stressing the
	//   structural iteration limits of the Fairness policies.
	FlowCount int

	// IngressConcurrency (W) dictates the volume of simultaneous incoming HTTP streams.
	// Sweeping W directly profiles the Go runtime scheduler's capacity to manage connection storms.
	// It exposes the limits of the Flow Control layer's structural backpressure mechanisms
	// (channels, contexts) and the synchronization primitives guarding the hot path.
	IngressConcurrency int
)

// BenchMatrix defines a single coordinate in the benchmarking hypercube.
type BenchMatrix struct {
	Limit      EgressConcurrencyLimit
	Shards     ShardCount
	Priorities PriorityLevels
	Flows      FlowCount
	Clients    IngressConcurrency
}

// Name generates a compact, readable identifier for 'go test -bench' output.
func (m BenchMatrix) Name() string {
	return fmt.Sprintf("L=%03d/S=%03d/P=%03d/F=%06d/W=%05d",
		m.Limit, m.Shards, m.Priorities, m.Flows, m.Clients)
}

// --- Synchronous Concurrency Engine & SUT Mocks ---

// benchDetector models target saturation based strictly on active request counts.
type benchDetector struct {
	concurrencyLimit atomic.Int64
	// Padding prevents false sharing between atomic counters on multicore machines.
	_        [56]byte
	inFlight atomic.Int64
}

// Saturation is evaluated synchronously by ShardProcessor.dispatchCycle().
// It acts as an optimistic lock for the benchmark, reserving the slot atomically during the check
// to guarantee the standing queue remains strictly at the target depth.
func (d *benchDetector) Saturation(ctx context.Context, candidates []metrics.PodMetrics) float64 {
	limit := d.concurrencyLimit.Load()
	if limit <= 0 {
		return 0.0 // Free-flow
	}

	if d.inFlight.Add(1) <= limit {
		return 0.99 // Return a safe value < 1.0 so the dispatcher proceeds.
	}

	// Capacity exceeded; rollback the optimistic increment.
	d.inFlight.Add(-1)
	return 1.0 // Saturated - forces the proxy to hold the item
}

// Release is called by the benchmark client immediately after EnqueueAndWait unblocks, instantly
// freeing the simulated downstream backend to accept the next request.
func (d *benchDetector) Release() {
	if d.concurrencyLimit.Load() > 0 {
		d.inFlight.Add(-1)
	}
}

// benchRequest guarantees deep topological entropy for plugin algorithmic sorting.
type benchRequest struct {
	key      flowcontrol.FlowKey
	byteSize uint64
}

func (r *benchRequest) FlowKey() flowcontrol.FlowKey       { return r.key }
func (r *benchRequest) ByteSize() uint64                   { return r.byteSize }
func (r *benchRequest) InitialEffectiveTTL() time.Duration { return 5 * time.Minute }
func (r *benchRequest) ID() string                         { return "bench-req" }
func (r *benchRequest) GetMetadata() map[string]any        { return nil }
func (r *benchRequest) InferencePoolName() string          { return "bench-pool" }
func (r *benchRequest) ModelName() string                  { return "bench-model" }
func (r *benchRequest) TargetModelName() string            { return "bench-target" }

// setupRealRegistry provisions the concrete FlowRegistry ensuring realistic routing.
func setupRealRegistry(b *testing.B, handle plugin.Handle, s ShardCount, p PriorityLevels) contracts.FlowRegistry {
	b.Helper()

	cfgOpts := []registry.ConfigOption{
		registry.WithInitialShardCount(int(s)),
		registry.WithMaxBytes(0), // Capacity restricted strictly via Concurrency (L).
	}

	for i := 0; i < int(p); i++ {
		band, err := registry.NewPriorityBandConfig(
			handle, i, fmt.Sprintf("band-%d", i),
			registry.WithBandMaxBytes(10_000_000_000), // Prevent capacity-based rejections.
		)
		if err != nil {
			b.Fatalf("Failed to init priority band %d: %v", i, err)
		}
		cfgOpts = append(cfgOpts, registry.WithPriorityBand(band))
	}

	regCfg, err := registry.NewConfig(handle, cfgOpts...)
	if err != nil {
		b.Fatalf("Failed to create registry config: %v", err)
	}

	reg, err := registry.NewFlowRegistry(regCfg, logr.Discard())
	if err != nil {
		b.Fatalf("Failed to initialize concrete registry: %v", err)
	}

	return reg
}

// setupBenchmarkHarness creates the standard SUT environment for all benchmarks.
func setupBenchmarkHarness(
	b *testing.B,
	ctx context.Context,
	s ShardCount,
	p PriorityLevels,
	limit int64,
	customCfg *controller.Config,
) (*controller.FlowController, *benchDetector) {
	b.Helper()
	handle := testutils.NewTestHandle(ctx)

	fPolicy, err := fairness.GlobalStrictFairnessPolicyFactory(registry.DefaultFairnessPolicyRef, nil, handle)
	if err != nil {
		b.Fatalf("Failed to create fairness policy: %v", err)
	}
	handle.AddPlugin(registry.DefaultFairnessPolicyRef, fPolicy)

	oPolicy, err := ordering.FCFSOrderingPolicyFactory(registry.DefaultOrderingPolicyRef, nil, handle)
	if err != nil {
		b.Fatalf("Failed to create ordering policy: %v", err)
	}
	handle.AddPlugin(registry.DefaultOrderingPolicyRef, oPolicy)

	reg := setupRealRegistry(b, handle, s, p)
	detector := &benchDetector{}
	detector.concurrencyLimit.Store(limit)

	var cfg *controller.Config
	if customCfg != nil {
		cfg = customCfg
	} else {
		// Default matrix config.
		bufferSize := min(2000/int(s), 10)
		cfg = &controller.Config{
			DefaultRequestTTL:               5 * time.Minute,
			ProcessorReconciliationInterval: 1 * time.Hour,
			ExpiryCleanupInterval:           1 * time.Hour,
			EnqueueChannelBufferSize:        bufferSize,
		}
	}

	fc, err := controller.NewFlowController(ctx, cfg, reg, detector, &mocks.MockPodLocator{})
	if err != nil {
		b.Fatalf("Failed to init FlowController: %v", err)
	}

	return fc, detector
}

// --- Telemetry Aggregator ---

// benchmarkTelemetry provides lock-free aggregation of benchmark statistics.
// It uses a two-phase commit model: threads mutate local telemetry structs during the hot loop to
// avoid false sharing on atomic cache-lines, and commit their totals to this global struct exactly
// once when the b.RunParallel loop terminates.
type benchmarkTelemetry struct {
	Flows         int
	errorCount    atomic.Int64
	dispatchCount atomic.Int64
	rejectCount   atomic.Int64
}

// newBenchmarkTelemetry provisions the global telemetry aggregator.
func newBenchmarkTelemetry(flows int) *benchmarkTelemetry {
	t := &benchmarkTelemetry{
		Flows: flows,
	}
	return t
}

// threadTelemetry is a lock-free thread-local accumulator.
// It is instantiated once per b.RunParallel physical thread to eliminate atomic contention on the
// hot path.
type threadTelemetry struct {
	errs, disp, rej int64
	// Prevent false sharing out to the 64-byte L1 cache-line boundary.
	_ [40]byte
}

// recordDispatch logs a successful EnqueueAndWait dequeue for a specific flow.
func (t *benchmarkTelemetry) recordDispatch(local *threadTelemetry) {
	local.disp++
}

// recordError tracks system evaluation errors (excluding expected context cancellations).
func (t *benchmarkTelemetry) recordError(local *threadTelemetry) {
	local.errs++
}

// recordReject logs explicit QueueOutcomeRejected and QueueOutcomeDrop events.
func (t *benchmarkTelemetry) recordReject(local *threadTelemetry) {
	local.rej++
}

// commit transfers thread-local statistics into the globally atomic telemetry object.
// It is intended to be called exactly once per physical thread at the end of b.RunParallel.
func (t *benchmarkTelemetry) commit(local *threadTelemetry) {
	if local.errs > 0 {
		t.errorCount.Add(local.errs)
	}
	if local.disp > 0 {
		t.dispatchCount.Add(local.disp)
	}
	if local.rej > 0 {
		t.rejectCount.Add(local.rej)
	}
}

// report aggregates the committed globals and issues standard b.ReportMetric calls into the Go
// benchmarking framework, ensuring formatted console and JSON output.
func (t *benchmarkTelemetry) report(b *testing.B, elapsed float64) {
	if elapsed <= 0 {
		return
	}

	b.ReportMetric(math.Round(float64(t.dispatchCount.Load())/elapsed), "d/s")

	if rejects := t.rejectCount.Load(); rejects > 0 {
		b.ReportMetric(math.Round(float64(rejects)/elapsed), "r/s")
	}
	if errs := t.errorCount.Load(); errs > 0 {
		b.ReportMetric(float64(errs), "errors")
	}
}
