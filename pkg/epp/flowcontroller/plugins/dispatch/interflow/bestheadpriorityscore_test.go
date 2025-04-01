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
// 	"testing"
// 	"time"

// 	"github.com/stretchr/testify/assert"
// 	"github.com/stretchr/testify/require"
// 	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/testing/mocks"
// 	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
// )

// func TestBestHeadPriorityScore_SelectQueue(t *testing.T) {
// 	policy := NewBestHeadPriorityScore()

// 	commonScoreType := "enqueue_time_ns"

// 	flowSpec1 := mocks.NewMockFlowSpecification("flow1", 0)
// 	flowSpec2 := mocks.NewMockFlowSpecification("flow2", 0)
// 	flowSpec3 := mocks.NewMockFlowSpecification("flow3", 0)

// 	item1_q1 := mocks.NewMockQueueItemAccessor("item1_q1", flowSpec1.ID(), 0, time.Now(), 100)
// 	item1_q2 := mocks.NewMockQueueItemAccessor("item1_q2", flowSpec2.ID(), 0, time.Now(), 50) // Best score (lowest)
// 	item1_q3 := mocks.NewMockQueueItemAccessor("item1_q3", flowSpec3.ID(), 0, time.Now(), 200)

// 	q1 := mocks.NewMockFlowQueueAccessor(flowSpec1, "q1", commonScoreType, nil)
// 	q1.MockLenVal = 1
// 	q1.MockPeekHeadItemVal = item1_q1

// 	q2 := mocks.NewMockFlowQueueAccessor(flowSpec2, "q2", commonScoreType, nil)
// 	q2.MockLenVal = 1
// 	q2.MockPeekHeadItemVal = item1_q2

// 	q3 := mocks.NewMockFlowQueueAccessor(flowSpec3, "q3", commonScoreType, nil)
// 	q3.MockLenVal = 1
// 	q3.MockPeekHeadItemVal = item1_q3

// 	t.Run("SelectsQueueWithLowestScore", func(t *testing.T) {
// 		bandQueues := map[string]types.FlowQueueAccessor{"flow1": q1, "flow2": q2, "flow3": q3}
// 		bandFlowIDs := []string{"flow1", "flow2", "flow3"}
// 		band := mocks.NewMockPriorityBandAccessor(0, "TestBand1", 0, bandQueues, bandFlowIDs)

// 		selected, err := policy.SelectQueue(band)
// 		require.NoError(t, err)
// 		require.NotNil(t, selected)
// 		assert.Same(t, q2, selected, "Should select q2 with the lowest score (50)")
// 	})

// 	t.Run("HandlesOneQueueEmpty", func(t *testing.T) {
// 		emptyQ1 := mocks.NewMockFlowQueueAccessor(flowSpec1, "emptyQ1", commonScoreType, nil)
// 		emptyQ1.MockLenVal = 0
// 		emptyQ1.MockPeekHeadErrorVal = types.ErrQueueEmpty

// 		bandQueues := map[string]types.FlowQueueAccessor{"flow1": emptyQ1, "flow2": q2, "flow3": q3}
// 		bandFlowIDs := []string{"flow1", "flow2", "flow3"}
// 		band := mocks.NewMockPriorityBandAccessor(0, "TestBandEmptyQ", 0, bandQueues, bandFlowIDs)
// 		selected, err := policy.SelectQueue(band)
// 		require.NoError(t, err)
// 		require.NotNil(t, selected)
// 		assert.Same(t, q2, selected)
// 	})

// 	t.Run("PriorityScoreTypeMismatchReturnsError", func(t *testing.T) {
// 		mismatchItem := mocks.NewMockQueueItemAccessor("mismatch_item", flowSpec3.ID(), 0, time.Now(), 10)
// 		qMismatch := mocks.NewMockFlowQueueAccessor(flowSpec3, "qMismatch", "different_score_type", nil)
// 		qMismatch.MockLenVal = 1
// 		qMismatch.MockPeekHeadItemVal = mismatchItem

// 		bandQueues := map[string]types.FlowQueueAccessor{"flow1": q1, "flow2": q2, "flow3_mismatch": qMismatch}
// 		// Order matters for when mismatch is detected by the policy's iteration.
// 		bandFlowIDs := []string{"flow1", "flow2", "flow3_mismatch"}
// 		band := mocks.NewMockPriorityBandAccessor(0, "TestBandMismatch", 0, bandQueues, bandFlowIDs)
// 		selected, err := policy.SelectQueue(band)

// 		assert.ErrorIs(t, err, types.ErrIncompatiblePriorityType, "Should return ErrIncompatiblePriorityType if PriorityScoreType mismatches")
// 		assert.Nil(t, selected, "Selected queue should be nil on error")
// 	})

// 	t.Run("SingleNonEmptyQueueIsSelected", func(t *testing.T) {
// 		bandQueues := map[string]types.FlowQueueAccessor{"flow2": q2}
// 		bandFlowIDs := []string{"flow2"}
// 		band := mocks.NewMockPriorityBandAccessor(0, "TestBandEmptyQ", 0, bandQueues, bandFlowIDs)
// 		selected, err := policy.SelectQueue(band)

// 		require.NoError(t, err)
// 		require.NotNil(t, selected)
// 		assert.Same(t, q2, selected)
// 	})

// 	t.Run("PeekHeadErrorOnOneQueueStillSelectsOther", func(t *testing.T) {
// 		qError := mocks.NewMockFlowQueueAccessor(flowSpec1, "qError", commonScoreType, nil)
// 		qError.MockLenVal = 1            // Has an item conceptually
// 		qError.MockPeekHeadItemVal = nil // but PeekHead will error
// 		qError.MockPeekHeadErrorVal = assert.AnError

// 		bandQueues := map[string]types.FlowQueueAccessor{"flowError": qError, "flow2": q2}
// 		bandFlowIDs := []string{"flowError", "flow2"}
// 		band := mocks.NewMockPriorityBandAccessor(0, "TestBandPeekError", 0, bandQueues, bandFlowIDs)

// 		selected, err := policy.SelectQueue(band)
// 		require.NoError(t, err, "Policy should not error out if one queue's PeekHead fails transiently")
// 		require.NotNil(t, selected, "Should still select q2 if qError.PeekHead fails")
// 		assert.Same(t, q2, selected)
// 	})
// }
