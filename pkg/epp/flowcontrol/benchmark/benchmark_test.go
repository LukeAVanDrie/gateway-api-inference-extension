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
	"errors"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/controller"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
)

// --- Benchmark 1: The Performance Hypercube (Steady-State Hot Path) ---

func BenchmarkFlowController_PerformanceMatrix(b *testing.B) {
	limits := []EgressConcurrencyLimit{0, 1, 100}
	shards := []ShardCount{1, 2, 8}
	priorities := []PriorityLevels{1, 4, 16}
	flows := []FlowCount{10, 10000, 100000}
	concurrencies := []IngressConcurrency{10, 5000, 50000}

	for _, L := range limits {
		for _, S := range shards {
			for _, P := range priorities {
				for _, F := range flows {
					for _, W := range concurrencies {
						// Prune illogical boundaries.
						if L == 0 && W > 100 {
							continue // Free-flow doesn't build a standing queue; high concurrency is redundant.
						}
						if L > 0 && int64(W) <= int64(L) {
							continue // W <= L generates zero structural backpressure.
						}

						matrix := BenchMatrix{Limit: L, Shards: S, Priorities: P, Flows: F, Clients: W}
						b.Run(matrix.Name(), func(b *testing.B) {
							run(b, matrix)
						})
					}
				}
			}
		}
	}
}

// BenchmarkFlowController_HighShardSort isolates the JSQ-Bytes scaling limits.
// It pushes ShardCount (S) extremely high to expose the O(S log S) sorting overhead on the hot
// path when the Flow Control layer evaluates the shortest active queue.
func BenchmarkFlowController_HighShardSort(b *testing.B) {
	shards := []ShardCount{16, 64, 128, 256}
	for _, S := range shards {
		matrix := BenchMatrix{Limit: 50, Shards: S, Priorities: 1, Flows: 100, Clients: 100}
		b.Run(matrix.Name(), func(b *testing.B) {
			run(b, matrix)
		})
	}
}

// run executes a single coordinate of the performance hypercube.
func run(b *testing.B, m BenchMatrix) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fc, detector := setupBenchmarkHarness(b, ctx, m.Shards, m.Priorities, int64(m.Limit), nil)

	// Yield briefly to allow the background supervisor to bootstrap the data plane.
	time.Sleep(10 * time.Millisecond)

	// Pre-allocate requests.
	reqs := make([]*benchRequest, m.Flows)
	for i := 0; i < int(m.Flows); i++ {
		hash := uint64(i) * 2654435761 % (1 << 32)
		reqs[i] = &benchRequest{
			key: flowcontrol.FlowKey{
				ID:       fmt.Sprintf("flow-%d", i),
				Priority: i % int(m.Priorities),
			},
			byteSize: 100 + (hash % 9000), // Payload entropy between 100B and 9KB.
		}
	}

	telemetry := newBenchmarkTelemetry(int(m.Flows))

	b.ResetTimer()
	b.ReportAllocs()

	// Scale physical execution threads to match simulated concurrency (W).
	procs := runtime.GOMAXPROCS(0)
	parallelism := max(int(m.Clients)/procs, 1)
	b.SetParallelism(parallelism)

	numFlows := int(m.Flows)

	// Initialize a Zipfian distribution generator.
	const zipfMask = 65535 // 0xFFFF
	zipfIndices := make([]int, zipfMask+1)

	if numFlows > 1 {
		// Use a single seed for the pre-computation.
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		// s=1.1 (skew), v=1.0. This heavily biases the random selections toward lower indices,
		// creating a realistic "hot tenant" imbalance that stresses the JSQ-Bytes load balancers.
		zipf := rand.NewZipf(rng, 1.1, 1.0, uint64(numFlows-1))
		for i := 0; i <= zipfMask; i++ {
			zipfIndices[i] = int(zipf.Uint64())
		}
	}

	// Explicitly clear memory profiles of all setup struct allocations before kicking off the actual
	// concurrent testing loop.
	b.ResetTimer()

	// Run parallel execution (the closed-loop harness).
	b.RunParallel(func(pb *testing.PB) {
		var localTelemetry threadTelemetry

		// Seed entropy outside the hot loop using the thread's distinct PRNG state address.
		localIdx := int(uintptr(unsafe.Pointer(&pb)))

		for pb.Next() {
			localIdx++

			// Select index via the pre-computed Zipfian array using a fast bitwise mask.
			flowIdx := zipfIndices[localIdx&zipfMask]

			sourceReq := reqs[flowIdx]

			// Ingress Phase
			outcome, err := fc.EnqueueAndWait(ctx, sourceReq)

			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				telemetry.recordError(&localTelemetry)
				if outcome != types.QueueOutcomeDispatched {
					telemetry.recordReject(&localTelemetry)
				}
				continue
			}

			// Egress Phase
			if outcome == types.QueueOutcomeDispatched {
				telemetry.recordDispatch(&localTelemetry)
				if m.Limit > 0 {
					// Instantly free the capacity slot to maintain the desired queue depth (W - L).
					detector.Release()
				}
			}
		}

		// Commit thread-local telemetry to global counters.
		telemetry.commit(&localTelemetry)
	})

	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	telemetry.report(b, elapsed)
}

// BenchmarkFlowController_TopologyChurn evaluates the Cold Path (JIT Provisioning).
// It aggressively generates novel FlowKeys on the hot path, forcing the Registry to continually
// acquire sync.RWMutex write locks to provision new priority bands and queues across shards.
func BenchmarkFlowController_TopologyChurn(b *testing.B) {
	ctx := b.Context()

	cfg := &controller.Config{
		DefaultRequestTTL:               5 * time.Minute,
		ProcessorReconciliationInterval: 1 * time.Hour,
		ExpiryCleanupInterval:           1 * time.Hour,
		EnqueueChannelBufferSize:        100,
	}

	fc, detector := setupBenchmarkHarness(b, ctx, 4, 1, 100, cfg)

	const numKeys = 50000
	preAllocatedIDs := make([]string, numKeys)
	for i := range numKeys {
		preAllocatedIDs[i] = fmt.Sprintf("novel-flow-%d", i)
	}

	var uniqueFlowID atomic.Uint64
	var dispatchCount atomic.Int64

	b.ResetTimer()
	b.ReportAllocs()
	b.SetParallelism(100)

	b.RunParallel(func(pb *testing.PB) {
		var localDisp int64
		req := &benchRequest{byteSize: 1024}

		for pb.Next() {
			// Generate a bounded FlowKey to isolate topology sync overhead from heap exhaustion.
			id := uniqueFlowID.Add(1) % numKeys

			req.key = flowcontrol.FlowKey{
				ID:       preAllocatedIDs[id],
				Priority: 0,
			}

			outcome, _ := fc.EnqueueAndWait(ctx, req)
			if outcome == types.QueueOutcomeDispatched {
				localDisp++
				detector.Release()
			}
		}
		dispatchCount.Add(localDisp)
	})

	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 {
		b.ReportMetric(math.Round(float64(dispatchCount.Load())/elapsed), "d/s")
	}
}

// BenchmarkFlowController_MassCancellation evaluates the GC overhead of client abandonment.
// It mixes healthy requests with aggressively timing-out requests, forcing the FlowController to
// asynchronously finalize items, and the ShardProcessor to lock the queues and sweep the
// "zombies".
func BenchmarkFlowController_MassCancellation(b *testing.B) {
	ctx := b.Context()

	cfg := &controller.Config{
		DefaultRequestTTL:               5 * time.Minute,
		ProcessorReconciliationInterval: 1 * time.Hour,
		ExpiryCleanupInterval:           10 * time.Millisecond, // Hyper-aggressive sweep for benchmark
		EnqueueChannelBufferSize:        100,
	}

	// Use L=100 so items actually queue and rot.
	fc, detector := setupBenchmarkHarness(b, ctx, 4, 1, 100, cfg)

	// Leaky bucket prevents CAS deadlock computationally when clients abandon requests in the benchmark harness.
	// Note: In production, actual saturation is detached from this specific mock detector race condition.
	// This is a benchmarking artifact, not a SUT bug.
	go func() {
		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				detector.inFlight.Store(0)
			}
		}
	}()

	var dispatchCount, timeoutCount atomic.Int64

	b.ResetTimer()
	b.ReportAllocs()
	b.SetParallelism(100)

	b.RunParallel(func(pb *testing.PB) {
		var localDisp, localTimeout int64
		// Use thread address parity to segment clients: 50% zombies, 50% stable.
		isZombie := uintptr(unsafe.Pointer(&pb))%2 == 0
		req := &benchRequest{key: flowcontrol.FlowKey{ID: "mixed-flow", Priority: 0}}

		for pb.Next() {
			reqCtx := ctx
			var reqCancel context.CancelFunc

			if isZombie {
				// Use 50ms to ensure Context expires after the request completes Ingress Phase and
				// officially enters the managedQueue. This allows runCleanupSweep to physically evaluate
				// zombie sweeps.
				reqCtx, reqCancel = context.WithTimeout(ctx, 50*time.Millisecond)
			}

			outcome, err := fc.EnqueueAndWait(reqCtx, req)

			if reqCancel != nil {
				reqCancel()
			}

			if outcome == types.QueueOutcomeDispatched {
				localDisp++
				detector.Release()
			} else if errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, types.ErrTTLExpired) ||
				errors.Is(err, context.Canceled) {
				localTimeout++
			}
		}
		dispatchCount.Add(localDisp)
		timeoutCount.Add(localTimeout)
	})

	b.StopTimer()
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 {
		b.ReportMetric(math.Round(float64(dispatchCount.Load())/elapsed), "stable_d/s")
		b.ReportMetric(math.Round(float64(timeoutCount.Load())/elapsed), "zombies/s")
	}
}

// BenchmarkFlowController_BurstDrainRate verifies the Flow Control layer's recovery capability.
// If the Flow Control layer does not utilize a work-conserving loop upon ticker wakeup, this
// benchmark will mathematically fail (e.g., executing at exactly 1000 d/s bounded by a 1ms ticker).
func BenchmarkFlowController_BurstDrainRate(b *testing.B) {
	ctx := b.Context()

	cfg := &controller.Config{
		DefaultRequestTTL:               5 * time.Minute,
		ProcessorReconciliationInterval: 1 * time.Hour,
		ExpiryCleanupInterval:           1 * time.Hour,
		EnqueueChannelBufferSize:        1000,
	}

	// Initial limit ensures tests can saturate the queues before timing block.
	// We pass limit=1 since detector state is reset in the loop
	fc, detector := setupBenchmarkHarness(b, ctx, 1, 1, 1, cfg)
	detector.inFlight.Store(100) // 100 > 1 == Saturated

	const burstSize = 10000

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()

		// Reset Saturated State
		detector.concurrencyLimit.Store(1)
		detector.inFlight.Store(100)

		var readyWg, doneWg sync.WaitGroup
		readyWg.Add(burstSize)
		doneWg.Add(burstSize)

		startCh := make(chan struct{})

		for range burstSize {
			go func() {
				defer doneWg.Done()
				readyWg.Done()
				<-startCh // Wait for broadcast signal.
				reqCtx, reqCancel := context.WithTimeout(ctx, 30*time.Second)
				_, _ = fc.EnqueueAndWait(reqCtx, &benchRequest{
					key: flowcontrol.FlowKey{ID: "burst-flow", Priority: 0},
				})
				reqCancel() // Avoid context leak.
			}()
		}

		// Wait for all goroutines to signal they're ready to block.
		readyWg.Wait()

		// Broadcast start signal while limit is still 1 and inFlight is 100 (Saturated).
		// This forces all 10,000 requests to hit EnqueueAndWait and get trapped in the queues.
		close(startCh)

		// Allow goroutines to deeply schedule themselves into the managedQueues.
		time.Sleep(10 * time.Millisecond)

		b.StartTimer()

		// Instantly free the mock capacity.
		detector.concurrencyLimit.Store(0)

		// Await explicit drain success.
		doneWg.Wait()
	}

	// Calculate and report standard burst throughput
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 {
		throughput := float64(b.N*burstSize) / elapsed
		b.ReportMetric(throughput, "burst_dispatches/sec")
	}
}
