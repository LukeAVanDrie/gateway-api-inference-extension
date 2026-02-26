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

// HeuristicEstimator predicts token requirements for cold-starts based on raw request context.
// In the future, this can be an extension point to inject dynamic or config-driven prediction parameters.
type HeuristicEstimator interface {
	PredictPromptTokens(requestByteSize uint64) int64
	PredictMaxNewTokens(reqBody map[string]any) int64
}

// TokenLengthHeuristicEstimator implements HeuristicEstimator using pessimistic standard guidelines.
type TokenLengthHeuristicEstimator struct{}

var _ HeuristicEstimator = &TokenLengthHeuristicEstimator{}

// NewHeuristicEstimator returns a default implementation using TokenLength guidelines.
func NewHeuristicEstimator() HeuristicEstimator {
	return &TokenLengthHeuristicEstimator{}
}

// PredictPromptTokens estimates prompt token count from the raw request size in bytes.
// It uses a pessimistic heuristic of RequestSize / 4, clamping to a minimum of 1.
func (e *TokenLengthHeuristicEstimator) PredictPromptTokens(requestByteSize uint64) int64 {
	if requestByteSize == 0 {
		return 0
	}
	return max(int64(requestByteSize/4), 1)
}

// PredictMaxNewTokens extracts the expected max generation lengths from well-known fields in the
// raw generic request body (e.g. max_completion_tokens).
//
// If not specified, it defaults to a deterministic safe ceiling of 4096.
// Note: These fallbacks are safe because they are temporary. They only safeguard the cluster during
// "cold starts" until the Hierarchical Estimator has collected enough observability snapshots (at
// any level) to replace these defaults with the learned exponential moving average (EMA).
func (e *TokenLengthHeuristicEstimator) PredictMaxNewTokens(reqBody map[string]any) int64 {
	if reqBody != nil {
		for _, key := range []string{"max_tokens", "max_new_tokens", "max_completion_tokens"} {
			if v, ok := reqBody[key]; ok {
				if f, ok := v.(float64); ok {
					return int64(f)
				}
			}
		}
	}
	return 4096 // Default safe fallback
}
