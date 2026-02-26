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

import "testing"

func TestPredictPromptTokens(t *testing.T) {
	e := NewHeuristicEstimator()

	tests := []struct {
		name        string
		reqByteSize uint64
		expected    int64
	}{
		{"Empty request", 0, 0},
		{"Very small request", 1, 1},
		{"Small request", 3, 1},
		{"Standard heuristic", 4, 1},
		{"Fractional heuristic floor", 5, 1},
		{"Fractional heuristic ceil", 7, 1},
		{"Standard heuristic match", 8, 2},
		{"Large request", 4096, 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := e.PredictPromptTokens(tt.reqByteSize)
			if got != tt.expected {
				t.Errorf("PredictPromptTokens() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestPredictMaxNewTokens(t *testing.T) {
	e := NewHeuristicEstimator()

	tests := []struct {
		name     string
		reqBody  map[string]any
		expected int64
	}{
		{"Nil body", nil, 4096},
		{"Empty body", map[string]any{}, 4096},
		{"Max tokens specified", map[string]any{"max_tokens": float64(1024)}, 1024},
		{"Max new tokens specified", map[string]any{"max_new_tokens": float64(2048)}, 2048},
		{"Max completion tokens specified", map[string]any{"max_completion_tokens": float64(4096)}, 4096},
		{"Max tokens precedence", map[string]any{"max_tokens": float64(1024), "max_new_tokens": float64(2048)}, 1024},
		{"Invalid type for max tokens", map[string]any{"max_tokens": "1024"}, 4096},
		{"No standard keys present", map[string]any{"unrelated": float64(1024)}, 4096},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := e.PredictMaxNewTokens(tt.reqBody)
			if got != tt.expected {
				t.Errorf("PredictMaxNewTokens() = %v, want %v", got, tt.expected)
			}
		})
	}
}
