package simulation

import (
	"math/rand"
	"sync"
	"time"
)

// HighFidelityInferenceServer acts as a "Digital Twin" of a production inference server (e.g., vLLM, TGI).
//
// It simulates the non-linear physics of Large Language Model serving, specifically:
// 1. The Memory Wall: Throughput is bound by HBM bandwidth during decoding.
// 2. The Compute Spike: Latency is bound by Tensor Core TFLOPS during prefill.
// 3. The Capacity Cliff: OOMs occur based on KV Cache fragmentation.
//
// --- Simulation Fidelity Matrix ---
//
// Supported Features - High Fidelity
//
//   - Roofline Modeling: Dynamically switches between Compute-Bound (Prefill) and Memory-Bound (Decode) latency based
//     on instantaneous arithmetic intensity.
//   - PagedAttention: Allocates memory in fixed blocks to model internal fragmentation and  elimination of external
//     fragmentation.
//   - Chunked Prefill: Limits the number of prompt tokens processed per tick to prevent Head-of-Line blocking
//     (simulating vLLM --enable-chunked-prefill).
//   - Continuous Batching: Models the amortization of kernel overheads across the batch.
//   - Lossy Preemption: Evicts requests on OOM, enforcing a "Kill and Restart" penalty to test controller stability.
//
// Unsupported Features - Approximations
//
//   - Prefix Caching: We assume every request has a unique prompt (Radix attention is not modeled).
//   - Tensor Parallelism: We model multi-GPU setups as a single "Super GPU" (Summed Bandwidth/VRAM).
//     Interconnect latency (NVLink) is ignored.
//   - Pipeline Bubbles: We assume perfect pipeline utilization (no idle bubbles between layers).
//   - Speculative Decode: We assume standard autoregressive decoding (1 token per step).
//   - Beam Search: We assume Greedy Sampling (1 sequence per request).
type HighFidelityInferenceServer struct {
	config PhysicsConfig

	// jitterFactor controls the stochastic noise introduced into step timings (0.0 - 1.0).
	// Real GPU kernels have variance due to DRAM refresh cycles, thermal throttling, and OS jitter.
	jitterFactor float64

	// performanceFactor represents the current health of the node.
	// Default: 1.0
	performanceFactor float64

	// --- Internal State ---
	mu      sync.Mutex
	rng     *rand.Rand
	pending []*Request
	running []*Request
	done    []*Request
}

var _ Backend = &HighFidelityInferenceServer{}

// NewHighFidelityInferenceServer creates the realistic simulation backend.
func NewHighFidelityInferenceServer(cfg PhysicsConfig, seed int64, jitter float64) *HighFidelityInferenceServer {
	if cfg.MaxKVTokens <= 0 || cfg.BlockSize <= 0 {
		panic("invalid physics configuration: check MaxKVTokens and BlockSize")
	}
	return &HighFidelityInferenceServer{
		config:            cfg,
		jitterFactor:      jitter,
		performanceFactor: 1.0,
		rng:               rand.New(rand.NewSource(seed)),
		pending:           make([]*Request, 0),
		running:           make([]*Request, 0),
	}
}

func (e *HighFidelityInferenceServer) Submit(req *Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	req.State = StateQueued
	e.pending = append(e.pending, req)
}

// Tick executes one logical step of the inference engine.
func (e *HighFidelityInferenceServer) Tick(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.schedulePending(now)
	e.advanceState(now)
	e.checkPreemption()
}

// schedulePending implements the Block Manager logic.
func (e *HighFidelityInferenceServer) schedulePending(now time.Time) {
	// Calculate blocks currently used by running requests.
	usedBlocks := 0
	for _, r := range e.running {
		// For PagedAttention, we conservatively assume the full prompt is reserved.
		currentSize := max(r.PromptLen, r.PrefillProgress+r.GeneratedTokens)
		usedBlocks += (currentSize + e.config.BlockSize - 1) / e.config.BlockSize
	}

	maxBlocks := e.config.MaxKVTokens / e.config.BlockSize
	newRunning := e.running
	remainingPending := make([]*Request, 0, len(e.pending))

	// Token Budget tracks how many prefill tokens we have processed in this tick.
	// This models "Chunked Prefill".
	admittedPrefillTokens := 0

	for _, r := range e.pending {
		// Scheduler Batch Limit
		if len(newRunning) >= e.config.MaxBatchSize {
			remainingPending = append(remainingPending, r)
			continue
		}

		// Global Prefill Token Budget (Chunking)
		if admittedPrefillTokens >= e.config.MaxSchedulerTokens {
			remainingPending = append(remainingPending, r)
			continue
		}

		// Memory Allocation (The "Memory Cliff")
		// We require "All-or-Nothing" allocation for the prompt to avoid prefill thrashing.
		neededBlocks := (r.PromptLen + e.config.BlockSize - 1) / e.config.BlockSize

		if usedBlocks+neededBlocks > maxBlocks {
			remainingPending = append(remainingPending, r)
			continue
		}

		// Admission Successful
		if r.State == StateQueued || r.State == StatePreempted {
			r.State = StatePrefill
			r.ScheduleTime = now

			// Fidelity Note on Preemption Recovery:
			// We simulate a "Kill and Restart" strategy rather than "CPU Swap".
			// This is a pessimistic assumption: the request must re-do all prefill work.
			// This is appropriate for testing Saturation Control stability (worst-case recovery).
			if r.State == StatePreempted {
				r.PrefillProgress = 0
			}
		}

		// Update Budgets
		nextChunk := min(r.PromptLen-r.PrefillProgress, e.config.MaxPrefillChunk)
		admittedPrefillTokens += nextChunk
		usedBlocks += neededBlocks
		newRunning = append(newRunning, r)
	}

	e.pending = remainingPending
	e.running = newRunning
}

func (e *HighFidelityInferenceServer) advanceState(now time.Time) {
	for _, r := range e.running {
		prefillBudget := e.config.MaxPrefillChunk
		switch r.State {
		case StatePrefill:
			remaining := r.PromptLen - r.PrefillProgress
			amount := min(remaining, prefillBudget)

			if amount > 0 {
				r.PrefillProgress += amount
				prefillBudget -= amount
			}

			if r.PrefillProgress >= r.PromptLen {
				r.State = StateDecode
			}

		case StateDecode:
			r.GeneratedTokens++
			if r.GeneratedTokens > 0 && r.FirstTokenTime.IsZero() {
				r.FirstTokenTime = now
			}
			if r.GeneratedTokens >= r.OutputLen {
				r.State = StateDone
				r.FinishTime = now
			}
		}
	}
}

// checkPreemption enforces the hard physical memory limit.
// If autoregressive generation causes the batch to grow beyond VRAM, we must Evict.
func (e *HighFidelityInferenceServer) checkPreemption() {
	usedBlocks := 0
	for _, r := range e.running {
		// Usage = Prompt + Generated
		total := r.PromptLen + r.GeneratedTokens
		if r.State == StatePrefill {
			total = r.PromptLen // During prefill, we reserve the full prompt.
		}
		usedBlocks += (total + e.config.BlockSize - 1) / e.config.BlockSize
	}

	maxBlocks := e.config.MaxKVTokens / e.config.BlockSize

	// Eviction Strategy: LIFO (Last In, First Out)
	// We evict the newest requests first because they have done the least work.
	// Evicting an "old" request wastes all the GPU time spent generating its hundreds of tokens.
	for usedBlocks > maxBlocks && len(e.running) > 0 {
		victimIdx := len(e.running) - 1
		victim := e.running[victimIdx]

		// Mark as preempted.
		victim.State = StatePreempted
		victim.PreemptionCount++
		e.running = e.running[:victimIdx]

		// Re-queue at the HEAD of the pending queue (High Priority).
		e.pending = append([]*Request{victim}, e.pending...)

		// Reclaim space. We reclaim the blocks used by this victim.
		victimTotal := victim.PromptLen + victim.GeneratedTokens
		reclaimed := (victimTotal + e.config.BlockSize - 1) / e.config.BlockSize
		usedBlocks -= reclaimed
	}
}

func (e *HighFidelityInferenceServer) DrainCompletions() []*Request {
	e.mu.Lock()
	defer e.mu.Unlock()

	var stillRunning []*Request
	for _, r := range e.running {
		if r.State == StateDone {
			e.done = append(e.done, r)
		} else {
			stillRunning = append(stillRunning, r)
		}
	}
	e.running = stillRunning

	drained := e.done
	e.done = nil
	return drained
}

func (e *HighFidelityInferenceServer) GetState() SystemState {
	e.mu.Lock()
	defer e.mu.Unlock()

	usedTokens := 0
	for _, r := range e.running {
		// KV Cache Usage Logic:
		// 1. Queued: 0
		// 2. Prefill: PromptLen (We reserve the full block)
		// 3. Decode: PromptLen + GeneratedTokens
		if r.State == StatePrefill || r.State == StateDecode {
			usedTokens += r.PromptLen + r.GeneratedTokens
		}
	}

	util := float64(usedTokens) / float64(e.config.MaxKVTokens)
	return SystemState{
		QueueDepth:      len(e.pending),
		RunningRequests: len(e.running),
		Utilization:     min(util, 1.0),
		// The Physics Engine uses MaxBatchSize as the scheduler's soft limit.
		// This is the "Structural Capacity" the estimator should converge to.
		TrueBatchCapacity: e.config.MaxBatchSize,
	}
}

func (e *HighFidelityInferenceServer) SetTimeDilation(factor float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Clamp to avoid divide-by-zero or negative time
	if factor <= 0.001 {
		factor = 0.001
	}
	e.performanceFactor = factor
}

func (e *HighFidelityInferenceServer) NextStepDuration() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.running) == 0 {
		return 10 * time.Millisecond // Idle heartbeat
	}

	// 1. Accumulate Workloads
	var (
		totalActiveKVTokens = 0
		totalComputeFLOPs   = 0.0
		isDecodeStep        = false
	)

	for _, r := range e.running {
		// --- Memory Component ---
		// Every running request (Prefill or Decode) occupies memory that must be accessed or managed.
		// In pure prefill, we write KV. In decode, we read KV.
		currentContextLen := r.PromptLen + r.GeneratedTokens
		totalActiveKVTokens += currentContextLen

		// --- Compute Component ---
		switch r.State {
		case StatePrefill:
			remaining := r.PromptLen - r.PrefillProgress
			chunk := min(remaining, e.config.MaxPrefillChunk)

			// Linear: Projection layers
			totalComputeFLOPs += e.config.LinearFLOPs(float64(chunk))
			// Attention: Quadratic in relation to context (Chunk * SeenSoFar)
			totalComputeFLOPs += e.config.AttentionFLOPs(float64(chunk), float64(r.PrefillProgress+chunk))
		case StateDecode:
			isDecodeStep = true
			totalComputeFLOPs += e.config.LinearFLOPs(1.0) // Add marginal compute for decoding.
		}
	}

	basetime := e.config.CalculateStepDuration(totalActiveKVTokens, totalComputeFLOPs, isDecodeStep)
	jittered := applyJitter(basetime, e.jitterFactor, e.rng)

	// Apply Fault Injection (Deterministic Degradation)
	// If factor is 0.5 (50% speed), duration doubles.
	return time.Duration(float64(jittered) / e.performanceFactor)
}

func (e *HighFidelityInferenceServer) EstimateCapacity(profile WorkloadProfile) CapacityInfo {
	qps := e.config.EstimateThroughputQPS(profile)

	// Avg Latency = 1 / QPS (Little's Law at Utilization=1.0)
	latency := time.Duration((1.0 / qps) * float64(time.Second))
	return CapacityInfo{
		MaxThroughputQPS: qps,
		AverageLatency:   latency,
	}
}

func applyJitter(d time.Duration, factor float64, rng *rand.Rand) time.Duration {
	if factor <= 0 {
		return d
	}
	// Generate a Normal variation centered at 1.0.
	// Clamp minimal duration to 10% of base to prevent unrealistic speedups.
	variation := max(1.0+(rng.NormFloat64()*factor), 0.1)
	return time.Duration(float64(d) * variation)
}
