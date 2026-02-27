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

package estimator

import (
	"math"
	"strconv"
	"sync"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru/v2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/hypervisor"
)

// TokenEstimator predicts the physical GPU cost of an incoming LLM request.
type TokenEstimator interface {
	// Estimate (Hot Path): Called by Flow Control BEFORE admission.
	// Returns the pessimistic worst-case ResourceVector.
	Estimate(flow flowcontrol.FlowKey, targetModel, baseModel string, promptTokens, maxNewTokens int64, blockSize int64) hypervisor.ResourceVector

	// Observe (Cold Path): Called by the OnResponseComplete lifecycle hook.
	// Feeds actual generated token counts back into the EMA learning models.
	Observe(flow flowcontrol.FlowKey, targetModel, baseModel string, actualGeneratedTokens int64)
}

// emaState holds the lock-free state for a single entity's history.
type emaState struct {
	// average packs a float64 into an atomic.Uint64 using math.Float64bits().
	// This allows lock-free CAS (Compare-And-Swap) updates of the moving average.
	average atomic.Uint64

	// samples tracks how many observations have been recorded.
	// Used to determine if the sample size is statistically mature enough to trust.
	samples atomic.Int64
}

// HierarchicalEstimator implements an O(1) prediction engine using an Exponential Moving Average (EMA).
// It safely bridges the gap between raw HTTP bounds (maxNewTokens) and physical cluster routing.
type HierarchicalEstimator struct {
	// L1: Flow + TargetModel Level (Unbounded - Requires LRU to prevent Heap OOM)
	// Key format: "flowID:priority:targetModel"
	l1LRU *lru.Cache[string, *emaState]

	// L2: TargetModel Level (Bounded - Safe for sync.Map as target models are finite)
	l2TargetModels sync.Map // map[string]*emaState

	// L3: BaseModel Level (Bounded - Base models underlying the target models)
	l3BaseModels sync.Map // map[string]*emaState

	// L4: Global Pool Baseline (Single lock-free state)
	l4Global emaState

	// Tunables
	minSamples   int64   // The trust threshold (e.g., 10 requests)
	ewmaAlpha    float64 // The learning rate (e.g., 0.1 for slow decay, 0.3 for fast reaction)
	safetyMargin float64 // The pessimistic buffer (e.g., 1.5x padding)
}

// NewHierarchicalEstimator initializes the memory-safe prediction engine.
func NewHierarchicalEstimator(
	l1CacheSize int,
	alpha float64,
	safetyMargin float64,
	minSamples int64,
) (*HierarchicalEstimator, error) {
	cache, err := lru.New[string, *emaState](l1CacheSize)
	if err != nil {
		return nil, err
	}

	return &HierarchicalEstimator{
		l1LRU:        cache,
		ewmaAlpha:    alpha,
		safetyMargin: safetyMargin,
		minSamples:   minSamples,
	}, nil
}

// Estimate resolves the hierarchy to predict execution cost in O(1) time.
func (e *HierarchicalEstimator) Estimate(
	flow flowcontrol.FlowKey,
	baseModel string,
	targetModel string,
	promptTokens,
	maxNewTokens int64,
	blockSize int64,
) hypervisor.ResourceVector {
	var expectedOut int64 = -1

	getEMA := func(state *emaState) (int64, bool) {
		if state != nil && state.samples.Load() >= e.minSamples {
			bits := state.average.Load()
			return int64(math.Float64frombits(bits)), true
		}
		return 0, false
	}

	priorityStr := strconv.Itoa(flow.Priority)
	l1Key := flow.ID + ":" + priorityStr + ":" + targetModel

	// 1. Try L1 (Flow + TargetModel)
	if state, ok := e.l1LRU.Get(l1Key); ok {
		if val, trusted := getEMA(state); trusted {
			expectedOut = val
		}
	}

	// 2. Try L2 (TargetModel Fallback)
	if expectedOut == -1 {
		if val, ok := e.l2TargetModels.Load(targetModel); ok {
			if val, trusted := getEMA(val.(*emaState)); trusted {
				expectedOut = val
			}
		}
	}

	// 3. Try L3 (BaseModel Fallback)
	if expectedOut == -1 {
		if val, ok := e.l3BaseModels.Load(baseModel); ok {
			if val, trusted := getEMA(val.(*emaState)); trusted {
				expectedOut = val
			}
		}
	}

	// 4. Try L4 (Global Fallback) or panic to maxNewTokens
	if expectedOut == -1 {
		if val, trusted := getEMA(&e.l4Global); trusted {
			expectedOut = val
		} else {
			// Absolute Cold Start: We must assume the worst to protect the cluster.
			expectedOut = maxNewTokens
		}
	}

	// Apply safety margin to prevent OOMs on slight deviations.
	paddedOut := int64(float64(expectedOut) * e.safetyMargin)

	// Never predict more than the hard limit imposed by the request / engine.
	if paddedOut > maxNewTokens {
		paddedOut = maxNewTokens
	}

	// Translate to physical hypervisor currencies
	return hypervisor.ResourceVector{
		ActiveRequests: 1,
		PrefillTokens:  promptTokens,

		// DecodeTokens represents memory bandwidth pressure.
		// Since decode is autoregressive, the KV Cache grows sequentially.
		// The average bandwidth cost over the generation is: Prompt + (Generated / 2).
		DecodeTokens: promptTokens + (paddedOut / 2),

		// KVBlocks represents physical spatial memory bounds.
		// It must contain the entire sequence (Prompt + Output) divided by the block size.
		// We use integer ceiling math to account for partial blocks.
		KVBlocks: (promptTokens + paddedOut + blockSize - 1) / blockSize,
	}
}

// Observe updates all hierarchical levels lock-free via Compare-And-Swap.
func (e *HierarchicalEstimator) Observe(
	flow flowcontrol.FlowKey,
	targetModel string,
	baseModel string,
	actualGeneratedTokens int64,
) {
	// 1. Update L4 (Global)
	updateEMA(&e.l4Global, actualGeneratedTokens, e.ewmaAlpha)

	// 2. Update L3 (BaseModel)
	l3State, _ := e.l3BaseModels.LoadOrStore(baseModel, &emaState{})
	updateEMA(l3State.(*emaState), actualGeneratedTokens, e.ewmaAlpha)

	// 3. Update L2 (TargetModel)
	l2State, _ := e.l2TargetModels.LoadOrStore(targetModel, &emaState{})
	updateEMA(l2State.(*emaState), actualGeneratedTokens, e.ewmaAlpha)

	// 4. Update L1 (Flow + TargetModel)
	priorityStr := strconv.Itoa(flow.Priority)
	l1Key := flow.ID + ":" + priorityStr + ":" + targetModel

	newState := &emaState{}
	present, _ := e.l1LRU.ContainsOrAdd(l1Key, newState)
	var l1State *emaState
	if present {
		existing, ok := e.l1LRU.Get(l1Key)
		if ok {
			l1State = existing
		} else {
			// Extreme edge case: it was evicted between ContainsOrAdd and Get.
			// Safe to fall back and add the new one.
			e.l1LRU.Add(l1Key, newState)
			l1State = newState
		}
	} else {
		l1State = newState
	}

	updateEMA(l1State, actualGeneratedTokens, e.ewmaAlpha)
}

// updateEMA performs a lock-free optimistic update of the float64 moving average.
func updateEMA(state *emaState, actual int64, alpha float64) {
	for range 10 {
		oldBits := state.average.Load()
		var newVal float64

		if oldBits == 0 {
			// Deterministically the first observation (unseeded). Seed it exactly.
			newVal = float64(actual)
		} else {
			// Apply standard Exponential Moving Average math.
			oldVal := math.Float64frombits(oldBits)
			newVal = (alpha * float64(actual)) + ((1.0 - alpha) * oldVal)
		}

		newBits := math.Float64bits(newVal)

		// CompareAndSwap ensures that if another request updated the EMA while we were calculating, we
		// throw away our calculation and try again.
		// Under extreme concurrency, multiple updates will fail and simply drop this sample to prevent
		// infinite CPU pinning. This dynamic sample filtering is acceptable as the Law of Large Numbers
		// ensures the moving average remains statistically representative.
		if state.average.CompareAndSwap(oldBits, newBits) {
			state.samples.Add(1)
			break
		}
	}
}
