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

package simulation

import (
	"time"
)

const (
	// MBU (Memory Bandwidth Utilization) scales the theoretical Bandwidth.
	// Real-world value is ~60-70% due to non-contiguous access overheads.
	MBU = 0.65

	// MFU (Model Flop Utilization) scales the theoretical TFLOPS.
	// Real-world value is ~40-50% due to attention matrix shapes.
	MFU = 0.45 // 45% of Peak TFLOPS (Compute Bound phases)

	// OverheadDecode is the fixed latency penalty per simulation tick (step).
	//
	// Physics: Represents the aggregate cost of:
	// 1. Python/Controller Logic (moving data between CPU and GPU).
	// 2. PCIe/NVLink latency (submitting the kernel).
	// 3. Kernel Launch overhead (GPU warp scheduling).
	//
	// Impact: This constant enforces "Batch Efficiency".
	// - At Batch Size 1, this 2ms overhead might be 50% of the total step time (Inefficient).
	// - At Batch Size 64, this 2ms is amortized across 64 requests (Efficient).
	//
	// Value: 2ms is a conservative baseline for PyTorch-based engines (like vLLM).
	// C++ engines (TGI/TensorRT) might be lower (~0.5ms).
	OverheadDecode = 2 * time.Millisecond

	// OverheadPrefill is the initialization cost for processing a new prompt.
	//
	// Physics: Represents the cost of allocating new KV blocks, calculating attention masks, and setting up the workspace
	// for the Compute-Bound phase.
	//
	// Impact: This adds a "Startup Cost" to every request, penalizing rapid-fire arrival of many short prompts compared
	// to fewer long prompts.
	OverheadPrefill = 5 * time.Millisecond
)

// HardwareSpecs defines the physical constraints of the accelerator (GPU).
//
// Mental Model: The "Engine" and the "Pipe".
// - The "Engine" (TFLOPS) determines how fast we can crunch the prompt (Prefill).
// - The "Pipe" (Bandwidth) determines how fast we can generate text (Decode).
// - The "Tank" (HBM) determines how many requests we can fit in parallel (Batch Size).
type HardwareSpecs struct {
	Name string

	// MemoryBandwidthGBps is the High Bandwidth Memory (HBM) transfer rate.
	//
	// Role: The Bottleneck for the "Decode" phase (Token Generation).
	// Physics: Generating 1 token requires reading the entire active KV Cache and Model Weights from HBM to the Chip.
	//          The Arithmetic Intensity is low (few math ops per byte loaded).
	// Limit: MaxTokens/sec ~= Bandwidth / (ModelSize + KVCacheSize).
	MemoryBandwidthGBps float64

	// PeakTFLOPS is the theoretical compute throughput (Tensor Core performance).
	//
	// Role: The Bottleneck for the "Prefill" phase (Prompt Processing).
	// Physics: Processing a prompt is a dense matrix multiplication. The Arithmetic Intensity
	// is high (many math ops per byte loaded).
	// Limit: PrefillLatency ~= (2 * Params * Tokens) / TFLOPS.
	PeakTFLOPS float64

	// MaxHBMGB is the hard constraint on VRAM capacity.
	//
	// Role: The Limit on Concurrency (Batch Size * Context Length).
	// Physics: HBM holds three things:
	// 1. Model Weights (Static Tax).
	// 2. Temporary Activation Overhead (Dynamic Tax).
	// 3. KV Cache (The remaining space for user requests).
	// If usage > MaxHBMGB, the system crashes (OOM).
	MaxHBMGB float64
}

// ModelSpecs defines the topology and size of the Transformer model.
//
// Mental Model: The "Shape" of the workload.
// - Weights determine the static memory cost.
// - Layers/HiddenSize determine the dynamic memory cost (KV Cache growth rate).
// - ActiveParams determine the compute cost.
type ModelSpecs struct {
	Name string

	// Layers is the number of Transformer blocks.
	// Impact: Linearly scales the KV Cache size. More layers = More VRAM per token.
	Layers int

	// HiddenSize is the dimension of the embedding vector (d_model).
	// Impact: Linearly scales the KV Cache size and quadratically scales the compute cost.
	HiddenSize int

	// ModelWeightsGB is the static VRAM "Tax" paid just to load the model.
	// Example: Llama-3-70B (Int4) takes ~35GB. On an 80GB GPU, you have 45GB left for traffic.
	ModelWeightsGB float64

	// KVBytesPerToken is the specific memory cost to store one token's history.
	// Formula: 2 (K+V) * Layers * HiddenSize * PrecisionBytes (e.g., 2 for FP16, 1 for FP8).
	// Impact: Determines how quickly a long context fills up the GPU.
	KVBytesPerToken float64

	// ActiveParamsBillion is the parameter count used for the forward pass.
	//
	// Distinction:
	// - Dense Models (Llama 3): ActiveParams == TotalParams. Every weight is used for every token.
	// - MoE Models (Mixtral): ActiveParams << TotalParams. Only "routed" experts (e.g., 2 of 8) are used.
	//
	// Impact:
	// - TotalParams determines VRAM usage (ModelWeightsGB).
	// - ActiveParams determines Latency (Compute time).
	ActiveParamsBillion float64
}

// PhysicsConfig aggregates the simulation parameters.
//
// Mental Model: "Theory vs. Reality".
// Hardware specs are theoretical maximums. Real performance is lower due to overheads,
// memory fragmentation, and scheduling inefficiencies. This config tunes those penalties.
type PhysicsConfig struct {
	Hardware HardwareSpecs
	Model    ModelSpecs

	// --- Efficiency Scalars (The "Reality Check") ---

	// UtilizationMBU (Memory Bandwidth Utilization) scales the theoretical Bandwidth.
	// Why < 1.0? Non-contiguous memory access patterns (gathering KV cache blocks) and DRAM refresh overheads prevent
	// hitting 100% peak.
	// Typical: 0.60 - 0.75.
	UtilizationMBU float64

	// UtilizationMFU (Model Flop Utilization) scales the theoretical TFLOPS.
	// Why < 1.0? Tensor Cores need large, aligned matrices. Attention heads often produce "skinny" matrices that
	// under-saturate the cores. FlashAttention improves this.
	// Typical: 0.40 - 0.55.
	UtilizationMFU float64

	// --- PagedAttention & Scheduling (The "OS" Logic) ---

	// BlockSize is the allocation unit for the KV Cache (e.g., 16 tokens).
	// Impact: Solves "External Fragmentation". We don't need a contiguous 1GB line for a request; we just need scattered
	// small blocks.
	BlockSize int

	// MaxKVTokens is the total capacity of the Block Table.
	// Derivation: (MaxHBMGB - ModelWeightsGB - Overhead) / KVBytesPerToken.
	// Impact: The hard limit on the total number of tokens (across all users) the system can remember.
	MaxKVTokens int

	// MaxBatchSize is the scheduler's soft limit on concurrent requests.
	// Impact: Prevents the scheduling loop overhead from becoming dominant.
	MaxBatchSize int

	// MaxSchedulerTokens is the global limit on prefill tokens processed per tick.
	// Feature: "Chunked Prefill".
	// Impact: Without this, a request with 100k tokens would lock the GPU for seconds, stalling all other small chat
	// requests ("Head-of-Line Blocking").
	MaxSchedulerTokens int

	// MaxPrefillChunk is the per-request limit on prefill tokens per tick.
	// Impact: Forces large prompts to be processed in multiple steps, allowing decoding requests to "interleave" in
	// between chunks.
	MaxPrefillChunk int

	// PerRequestOverheadMB is the non-KV memory cost per active request.
	// Includes: Logits buffers, Sampling state, CUDA pointer metadata.
	// Typical: 10MB - 50MB.
	PerRequestOverheadMB float64
}

// CalculateStepDuration determines the time required to process a specific amount of work in a single simulation tick
// (Mixed Batch).
//
// It encapsulates the Roofline Model (Compute vs Memory) and fixed Overheads.
//
// Parameters:
// - activeKVTokens: Total number of tokens in the KV cache for all running requests.
// - computeFLOPs: Total floating point operations required (Prefill Linear + Attn).
// - isDecodeStep: If true, we apply Memory Bandwidth constraints.
func (p PhysicsConfig) CalculateStepDuration(activeKVTokens int, computeFLOPs float64, isDecodeStep bool) time.Duration {
	overhead := time.Duration(0)
	tCompute := time.Duration(0)
	if computeFLOPs > 0 {
		overhead += OverheadPrefill
		tCompute = p.ComputeLatency(computeFLOPs)
	}

	tMemory := time.Duration(0)
	if isDecodeStep {
		overhead += OverheadDecode
		tMemory = p.MemoryLatency(activeKVTokens)
	}

	// --- Roofline Aggregation ---
	// We use the "Additive" model (Serialization) to be robust.
	// StepTime = Max(Memory, Compute) + Overheads
	// Note: In a mixed batch, Prefill takes Compute, Decode takes Memory.
	// We assume they do not perfectly overlap due to contention.
	return max(tMemory, tCompute) + overhead
}

// LinearFLOPs calculates the floating point operations for dense projections (FeedForward + QKV Projections).
// Formula: 2 * Parameters * Tokens
func (p PhysicsConfig) LinearFLOPs(tokens float64) float64 {
	return 2.0 * (p.Model.ActiveParamsBillion * 1e9) * tokens
}

// AttentionFLOPs calculates the floating point operations for Self-Attention.
// Formula: 4 * Layers * HiddenSize * QueryTokens * ContextLength
// Note: This accounts for the quadratic scaling of attention with context length.
func (p PhysicsConfig) AttentionFLOPs(tokens, contextLen float64) float64 {
	return float64(4*p.Model.Layers*p.Model.HiddenSize) * tokens * contextLen
}

// ComputeLatency estimates latency for a compute-bound step.
// Roofline Model: T = FLOPs / Effective_TFLOPS
func (p PhysicsConfig) ComputeLatency(computeFLOPs float64) time.Duration {
	// Compute Constraint
	effectiveFLOPS := (p.Hardware.PeakTFLOPS * 1e12) * p.UtilizationMFU

	// Latency
	seconds := computeFLOPs / effectiveFLOPS
	return time.Duration(seconds * float64(time.Second))
}

// MemoryLatency estimates latency for a memory-bound step.
// Roofline Model: T = Data_Moved / Effective_Bandwidth
func (p PhysicsConfig) MemoryLatency(activeKVTokens int) time.Duration {
	// Data Movement: Model Weights + KV Cache for the active tokens
	kvSizeGB := (float64(activeKVTokens) * p.Model.KVBytesPerToken) / 1e9
	totalDataGB := p.Model.ModelWeightsGB + kvSizeGB

	// Bandwidth Constraint
	effectiveBW := p.Hardware.MemoryBandwidthGBps * p.UtilizationMBU

	// Latency
	seconds := totalDataGB / effectiveBW
	return time.Duration(seconds * float64(time.Second))
}

// EstimateThroughputQPS estimates the theoretical capacity limit of the system.
// It simulates a "Steady State" batch processing average-sized requests.
func (p PhysicsConfig) EstimateThroughputQPS(profile WorkloadProfile) float64 {
	batchSize := p.MaxBatchSize
	avgInput := profile.MeanInputTokens()
	avgOutput := profile.MeanOutputTokens()

	// 1. Calculate Prefill Time (Per Request)
	// We estimate the time to prefill one average prompt.
	tPrefill := p.CalculateStepDuration(0, p.LinearFLOPs(avgInput)+p.AttentionFLOPs(avgInput, avgInput), false)

	// 2. Calculate Decode Time (Per Step of the Batch)
	avgContext := int(avgInput + (avgOutput / 2))
	totalActiveKV := avgContext * batchSize

	// Time for ONE decode step across the whole batch
	tStep := p.CalculateStepDuration(totalActiveKV, p.LinearFLOPs(float64(batchSize)), true)

	// 3. Amortize per Request
	// Each request "pays" for 1/BatchSize of the step, repeated Output times.
	tDecode := (tStep.Seconds() * avgOutput) / float64(batchSize)

	totalTime := tPrefill.Seconds() + tDecode
	return 1.0 / totalTime
}

// EstimateRidgePointBatchSize calculates the theoretical batch size where compute cost equals memory cost.
func (p PhysicsConfig) EstimateRidgePointBatchSize() float64 {
	// 1. Memory Time (Fixed cost to load weights per step)
	tMem := p.Model.ModelWeightsGB / p.Hardware.MemoryBandwidthGBps

	// 2. Compute Time (Marginal cost per token per request)
	tCompute := p.ComputeLatency(p.LinearFLOPs(1.0)).Seconds()

	// 3. Ridge Point: T_mem = B * T_compute
	return tMem / tCompute
}
