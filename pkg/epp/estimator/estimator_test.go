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
	"testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
)

func TestEstimatorFallbackStaircase(t *testing.T) {
	t.Parallel()
	// Create Estimator with minSamples = 10, alpha = 0.5, safety = 1.2
	est, err := NewHierarchicalEstimator(1000, 0.5, 1.2, 10)
	if err != nil {
		t.Fatal(err)
	}

	flow := flowcontrol.FlowKey{ID: "premium", Priority: 5}

	// Test Cold Start (assumption of max bounds)
	res := est.Estimate(flow, "base", "target", 100, 1000, 16)
	if res.DecodeTokens != 100+(1000/2) { // max bounds: prompt + (generated / 2)
		t.Errorf("Expected fallback to max bounds, got %d", res.DecodeTokens)
	}

	// Warm just the Global layer (11 requests)
	for i := 0; i < 11; i++ {
		est.Observe(flowcontrol.FlowKey{ID: "dummy", Priority: 0}, "unrelatedTarget", "unrelatedBase", 500)
	}

	// Estimate should now use the 500 average from Global layer
	res = est.Estimate(flow, "base", "target", 100, 1000, 16)
	if res.DecodeTokens == 100+(1000/2) {
		t.Errorf("Still falling back to max bounds despite global warm state")
	}

	// Padded value with margin: 500 * 1.2 = 600
	expectedPaddedOut := int64(600)
	expectedDecode := 100 + (expectedPaddedOut / 2)
	if res.DecodeTokens != expectedDecode {
		t.Errorf("Expected global fallback estimate of %d, got %d", expectedDecode, res.DecodeTokens)
	}
}

func TestEstimatorEMADecay(t *testing.T) {
	t.Parallel()
	// Create with exact configuration (alpha 0.5, minSamples 2, safety 1.0)
	est, err := NewHierarchicalEstimator(1000, 0.5, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	flow := flowcontrol.FlowKey{ID: "premium", Priority: 1}

	// 1. Prime the pump with high values
	est.Observe(flow, "target", "base", 1000)
	est.Observe(flow, "target", "base", 1000)

	res := est.Estimate(flow, "base", "target", 0, 2000, 16)
	if res.KVBlocks != (1000+16-1)/16 { // approx 63 blocks
		t.Errorf("Expected warm history for high estimate, got %d blocks", res.KVBlocks)
	}

	// 2. Feed small values and observe decay
	est.Observe(flow, "target", "base", 100) // Expect: 0.5*100 + 0.5*1000 = 550
	est.Observe(flow, "target", "base", 100) // Expect: 0.5*100 + 0.5*550 = 325

	res = est.Estimate(flow, "base", "target", 0, 2000, 16)
	expectedPaddedOut := int64(325)
	expectedBlocks := (expectedPaddedOut + 16 - 1) / 16
	if res.KVBlocks != expectedBlocks {
		t.Errorf("EMA decayed incorrectly, expected %d blocks, got %d. Out was ~%d", expectedBlocks, res.KVBlocks, res.KVBlocks*16)
	}
}
