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

package interflowdispatch

// import (
// 	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
// )

const BestHeadPriorityScoreDispatchPolicyName RegisteredInterFlowDispatchPolicyName = "BestHeadPriorityScore"

// func init() {
// 	RegisterPolicy(BestHeadPriorityScoreDispatchPolicyName, func() (types.InterFlowDispatchPolicy, error) {
// 		return NewBestHeadPriorityScore(), nil
// 	})
// }

// // BestHeadPriorityScore implements the types.InterFlowDispatchPolicy interface.
// // It selects the flow queue whose head item has the "best" (numerically lowest score as established by convention;
// // e.g., earliest enqueue time) PriorityScore.
// // It requires all compared queues to have the same PriorityScoreType.
// type BestHeadPriorityScore struct{}

// var _ types.InterFlowDispatchPolicy = &BestHeadPriorityScore{} // Compile-time validation

// // NewBestHeadPriorityScore creates a new BestHeadPriorityScore InterFlowDispatchPolicy policy.
// func NewBestHeadPriorityScore() *BestHeadPriorityScore {
// 	return &BestHeadPriorityScore{}
// }

// // SelectQueue inspects the queues within the band and returns the QueueAccessor of the flow queue whose head item has
// // the best (numerically lowest) PriorityScore.
// // It returns (nil, ErrIncompatiblePriorityType) if a mismatch in PriorityScoreType is detected among the queues being
// // compared.
// // It returns (nil, nil) if no suitable queue is found (e.g., all queues are empty).
// func (p *BestHeadPriorityScore) SelectQueue(band types.PriorityBandAccessor) (types.FlowQueueAccessor, error) {
// 	if band == nil {
// 		return nil, nil
// 	}

// 	var bestQueue types.FlowQueueAccessor
// 	var bestScore float64
// 	var firstScoreType string
// 	initialized := false

// 	var iterationErr error
// 	band.IterateQueues(func(q types.FlowQueueAccessor) bool {
// 		if q == nil || q.Len() == 0 {
// 			return true // Skip nil or empty queues
// 		}

// 		headItem, err := q.PeekHead()
// 		if err != nil || headItem == nil {
// 			// This is a transient issue with one queue; continue to check other queues.
// 			return true // Skip if can't peek head
// 		}

// 		currentScoreType := q.PriorityScoreType()
// 		currentScore := headItem.PriorityScore()

// 		if !initialized {
// 			// First valid queue encountered, set it as the current best
// 			bestQueue = q
// 			bestScore = currentScore
// 			firstScoreType = currentScoreType
// 			initialized = true
// 			return true
// 		}

// 		// Check for PriorityScoreType compatibility
// 		if currentScoreType != firstScoreType {
// 			bestQueue = nil // Invalidate current selection
// 			iterationErr = types.ErrIncompatiblePriorityType
// 			return false // Stop iteration
// 		}

// 		if currentScore < bestScore {
// 			bestScore = currentScore
// 			bestQueue = q
// 		}
// 		return true
// 	})

// 	if iterationErr != nil {
// 		return nil, iterationErr
// 	}
// 	return bestQueue, nil
// }

// // Name returns the unique string identifier for this policy implementation.
// func (p *BestHeadPriorityScore) Name() string {
// 	return string(BestHeadPriorityScoreDispatchPolicyName)
// }
