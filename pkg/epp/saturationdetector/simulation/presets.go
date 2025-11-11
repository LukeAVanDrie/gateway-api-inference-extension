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

import "fmt"

// --- Hardware Profiles ---

var (
	// H100_80GB_SXM5 represents the NVIDIA H100 80GB SXM5.
	// Datasheet: https://resources.nvidia.com/en-us-gpu-resources.
	//
	// Compute Note:
	// The datasheet lists "FP16 Tensor Core" at 1,979 TFLOPS (Sparse).
	// We use the Dense performance (1/2 of Sparse) = 989 TFLOPS.
	//
	// Why not FP8?
	// While H100 supports FP8 (3,958 TFLOPS Sparse), most current "Int4" serving setups (AWQ/GPTQ) dequantize weights to
	// FP16/BF16 for the actual matrix multiplication (W4A16).
	H100_80GB_SXM5 = HardwareSpecs{
		Name:                "H100-80GB-SXM5",
		MemoryBandwidthGBps: 3350.0, // 3.35 TB/s
		PeakTFLOPS:          989.0,  // FP16/BF16 Tensor Core (Dense)
		MaxHBMGB:            80.0,
	}

	// L4_24GB_PCIe represents the NVIDIA L4 24GB PCIe Gen4.
	// Datasheet: https://resources.nvidia.com/en-us-gpu-resources.
	//
	// Compute Note:
	// The datasheet lists "FP16 Tensor Core" at 242 TFLOPS (Sparse).
	// We use the Dense performance (1/2 of Sparse) = 121 TFLOPS.
	L4_24GB_PCIe = HardwareSpecs{
		Name:                "L4-24GB-PCIe",
		MemoryBandwidthGBps: 300.0,
		PeakTFLOPS:          121.0, // FP16/BF16 Tensor Core (Dense)
		MaxHBMGB:            24.0,
	}
)

// --- Model Profiles ---

var (
	// Llama3_8B_FP16 represents a standard uncompressed deployment.
	// HF ID: meta-llama/Meta-Llama-3-8B-Instruct
	// Source: https://huggingface.co/meta-llama/Meta-Llama-3-8B-Instruct/blob/main/config.json
	//
	// Architecture:
	// - 32 Layers, 4096 Hidden Size
	// - GQA: 32 Query Heads, 8 KV Heads (4:1 Ratio)
	// - Head Dim: 128 (4096 / 32)
	// - Compatibility: Fits on H100 (Single) and L4 (Single).
	Llama3_8B_FP16 = ModelSpecs{
		Name:                "Llama-3-8B-Instruct-FP16",
		Layers:              32,
		HiddenSize:          4096,
		ModelWeightsGB:      16.0, // 8.03B params * 2 bytes (FP16)
		ActiveParamsBillion: 8.0,  // Dense model: All params active
		// KV Calculation (FP16):
		// 2 (K+V) * 32 (Layers) * 8 (KV Heads) * 128 (Head Dim) * 2 (Bytes/FP16) = 131,072 bytes
		KVBytesPerToken: 131072.0,
	}

	// Llama3_70B_Int4 represents a high-performance compressed deployment.
	// HF ID: hugging-quants/Meta-Llama-3.1-70B-Instruct-AWQ-INT4
	// Source: https://huggingface.co/hugging-quants/Meta-Llama-3.1-70B-Instruct-AWQ-INT4/blob/main/config.json
	//
	// Architecture:
	// - 80 Layers, 8192 Hidden Size
	// - GQA: 64 Query Heads, 8 KV Heads (8:1 Ratio)
	// - Head Dim: 128 (8192 / 64)
	// - Compatibility: Fits on H100. Incompatible with single L4 (Needs ~35GB+).
	Llama3_70B_Int4 = ModelSpecs{
		Name:                "Llama-3-70B-Instruct-AWQ-Int4",
		Layers:              80,
		HiddenSize:          8192,
		ModelWeightsGB:      35.0, // ~70B * 0.5 bytes (4-bit) + Overhead
		ActiveParamsBillion: 70.0, // Dense model: All params active
		// KV Calculation (FP8):
		// Modern vLLM/TGI allows FP8 KV cache (e4m3) to save memory.
		// 2 (K+V) * 80 (Layers) * 8 (KV Heads) * 128 (Head Dim) * 1 (Byte/FP8) = 163,840 bytes
		KVBytesPerToken: 163840.0,
	}

	// Mixtral_8x7B_Int4 represents a Sparse Mixture-of-Experts (MoE) model.
	// HF ID: mistralai/Mixtral-8x7B-Instruct-v0.1 (Quantized)
	// Source: https://huggingface.co/mistralai/Mixtral-8x7B-Instruct-v0.1/blob/main/config.json
	//
	// Architecture:
	// - 32 Layers, 4096 Hidden Size
	// - 8 Experts total, Top-2 Routing (2 active per token)
	// - GQA: 32 Query Heads, 8 KV Heads
	// - Compatibility: Fits on H100. Incompatible with single L4 (Needs ~26GB+).
	Mixtral_8x7B_Int4 = ModelSpecs{
		Name:           "Mixtral-8x7B-v0.1-Int4",
		Layers:         32,
		HiddenSize:     4096,
		ModelWeightsGB: 26.0, // ~46.7B Total Params * 0.5 bytes + Overhead
		// Active Params Note:
		// Mixtral has ~12.9B active parameters per forward pass (Attention + 2 Experts).
		// This creates a high Memory-to-Compute ratio compared to dense models.
		ActiveParamsBillion: 12.9,
		// KV Calculation (FP8):
		// 2 (K+V) * 32 (Layers) * 8 (KV Heads) * 128 (Head Dim) * 1 (Byte/FP8) = 65,536 bytes
		KVBytesPerToken: 65536.0,
	}

	// Qwen3_32B_FP16 represents the standard dense deployment of the state-of-the-art mid-sized model.
	// HF ID: Qwen/Qwen3-32B
	// Source: https://huggingface.co/Qwen/Qwen3-32B/blob/main/config.json
	//
	// Architecture (Derived from Config):
	// - 64 Layers
	// - Hidden Size: 5120
	// - Intermediate Size: 25600 (SwiGLU)
	// - Attention: 64 Query Heads, 8 KV Heads (GQA 8:1)
	// - Head Dim: 128
	//   Note: Query projection width (64*128=8192) > Hidden Size (5120).
	// - Vocab: 151,936 (Untied Embeddings)
	// - Compatibility: Fits on H100. Incompatible with single L4 (Needs ~65.5GB+).
	Qwen3_32B_FP16 = ModelSpecs{
		Name:                "Qwen3-32B",
		Layers:              64,
		HiddenSize:          5120,
		ModelWeightsGB:      65.0, // ~32.5B params * 2 bytes (FP16) + Overhead
		ActiveParamsBillion: 32.5, // Dense model: All params active
		// KV Calculation (BF16):
		// 2 (K+V) * 64 (Layers) * 8 (KV Heads) * 128 (Head Dim) * 2 (Bytes) = 262,144 bytes
		KVBytesPerToken: 262144.0,
	}
)

// PhysicsOption allows for tuning the simulation parameters during initialization.
type PhysicsOption func(*PhysicsConfig)

// WithMaxBatchSize overrides the auto-tuned batch size.
// Use this if you want to simulate a specific misconfiguration or stress test.
func WithMaxBatchSize(n int) PhysicsOption {
	return func(p *PhysicsConfig) {
		p.MaxBatchSize = n
	}
}

// WithFP8KVCache enables 8-bit Key-Value caching.
// This effectively doubles the KV capacity of the GPU.
// Standard for H100/L4 deployments.
func WithFP8KVCache() PhysicsOption {
	return func(p *PhysicsConfig) {
		// We modify the internal model spec directly.
		p.Model.KVBytesPerToken /= 2.0
	}
}

// WithUtilizationOverrides allows expert users to tune the efficiency scalars.
// mbu: Memory Bandwidth Utilization (e.g., 0.65)
// mfu: Model Flop Utilization (e.g., 0.45)
func WithUtilizationOverrides(mbu, mfu float64) PhysicsOption {
	return func(p *PhysicsConfig) {
		p.UtilizationMBU = mbu
		p.UtilizationMFU = mfu
	}
}

// NewStandardPhysics creates a calibrated PhysicsConfig.
//
// Parameters:
//   - numGPUs: Aggregates VRAM/Bandwidth (e.g., 8 for HGX H100).
//   - opts: Optional overrides (MaxBatchSize, FP8 Cache, etc).
//
// It automatically handles:
//  1. Hardware Aggregation: Summing VRAM/Bandwidth for multi-GPU nodes.
//  2. Overhead Reservation: Reserving ~10% VRAM for CUDA contexts and activation workspaces.
//  3. Ride Point Optimization: If WithMaxBatchSize is not provided, this function constructs the config, calculates the
//     hardware's "Ridge Point" (optimal efficiency batch size), and applies it automatically.
//  4. Feasibility Checks
func NewStandardPhysics(
	hw HardwareSpecs,
	model ModelSpecs,
	numGPUs int,
	opts ...PhysicsOption,
) (PhysicsConfig, error) {
	if numGPUs < 1 {
		return PhysicsConfig{}, fmt.Errorf("numGPUs %d must be >= 1", numGPUs)
	}

	// 1. Aggregate Hardware Resources
	// We treat a multi-GPU node as a single "Super GPU" with summed specs.
	// Fidelity Note: We ignore the ~10-15% penalty of NVLink/PCIe communication overhead for Tensor Parallelism.
	// This is acceptable for capacity planning.
	totalHBM := hw.MaxHBMGB * float64(numGPUs)
	totalBW := hw.MemoryBandwidthGBps * float64(numGPUs)
	totalTFLOPS := hw.PeakTFLOPS * float64(numGPUs)
	aggregatedHW := HardwareSpecs{
		Name:                fmt.Sprintf("%dx %s", numGPUs, hw.Name),
		MemoryBandwidthGBps: totalBW,
		PeakTFLOPS:          totalTFLOPS,
		MaxHBMGB:            totalHBM,
	}

	// 2. Initialize Base Config with Defaults
	p := PhysicsConfig{
		Hardware:       aggregatedHW,
		Model:          model,
		UtilizationMBU: MBU,
		UtilizationMFU: MFU,
		BlockSize:      16, // vLLM default
		// Chunked Prefill: Limit prefill to ~25% of the max sequence length per tick to maintain interactivity for decoding
		// requests. A safe default is 512 or 1024 tokens per step.
		MaxPrefillChunk: 512,
	}

	// 3. Apply User Options
	for _, opt := range opts {
		opt(&p)
	}

	// 4. Auto-Tune Batch Size
	// If the user didn't set a hard limit, we ask the Physics Engine for the Ridge Point.
	// We floor at 1, and cap at a reasonable safety limit (e.g. 512) to prevent scheduler thrashing in extreme cases.
	if p.MaxBatchSize <= 0 {
		p.MaxBatchSize = int(max(1.0, min(p.EstimateRidgePointBatchSize(), 512.0)))
	}
	p.MaxSchedulerTokens = p.MaxBatchSize * p.MaxPrefillChunk

	// 5. Memory Feasibility & Derivations
	// Note: p.Model might have been modified by WithFP8KVCache, so KV cost is correct.
	const (
		// We reserve 10% of VRAM for non-KV overheads (CUDA context, temporary activation buffers, fragmentation).
		reservedRatio = 0.90 // vLLM default
		// vLLM tends to have ~20MB overhead per sequence slot.
		perReqOverhead = 20.0
	)
	maxRequestOverhead := float64(p.MaxBatchSize) * (perReqOverhead / 1024.0)
	usableHBM := (totalHBM * reservedRatio) - model.ModelWeightsGB - maxRequestOverhead
	p.MaxKVTokens = int(usableHBM * 1e9 / model.KVBytesPerToken)
	if usableHBM <= 0 {
		return PhysicsConfig{}, fmt.Errorf(
			"SimConfiguration Error: OOM at Boot.\n"+
				"Hardware: %d x %s (Total %.1f GB)\n"+
				"Model: %s (Weights %.1f GB)\n"+
				"Resolution: Increase numGPUs or choose a smaller model",
			numGPUs, hw.Name, totalHBM,
			model.Name, model.ModelWeightsGB,
		)
	}

	return p, nil
}
