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

package intraflowdispatch

// import (
// 	"testing"
// 	"time"

// 	"github.com/stretchr/testify/assert"
// 	"github.com/stretchr/testify/require"
// 	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/testing/mocks"
// 	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
// )

// func TestFCFS_SelectItem(t *testing.T) {
// 	policy := NewFCFS()

// 	t.Run("NonEmptyQueue", func(t *testing.T) {
// 		flowSpec := mocks.NewMockFlowSpecification("fcfs-flow", 0)
// 		item1 := mocks.NewMockQueueItemAccessor("item1", flowSpec.ID(), 0, time.Now(), 0)
// 		item1.MockEnqueueTimeVal = time.Now() // Explicitly set for clarity, though mock constructor does it.

// 		mockQueue := mocks.NewMockFlowQueueAccessor(flowSpec, "fcfs-test-q", policy.PriorityScoreType(), policy.RequiredQueueCapabilities())
// 		mockQueue.MockLenVal = 1
// 		mockQueue.MockPeekHeadItemVal = item1

// 		selected := policy.SelectItem(mockQueue)
// 		require.NotNil(t, selected, "SelectItem from non-empty queue should return an item")
// 		assert.Equal(t, item1.RequestID(), selected.RequestID(), "SelectItem should return the item from PeekHead")
// 	})
// }

// func TestFCFS_PolicyProperties(t *testing.T) {
// 	policy := NewFCFS()

// 	assert.Nil(t, policy.ItemComparator(), "FCFS ItemComparator should be nil")
// 	assert.Equal(t, string(EnqueueTimePriorityScoreType), policy.PriorityScoreType(), "FCFS PriorityScoreType mismatch")
// 	assert.Contains(t, policy.RequiredQueueCapabilities(), types.CapabilityFIFO, "FCFS should require CapabilityFIFO")
// 	assert.Equal(t, string(FCFSDispatchPolicyName), policy.Name(), "FCFS Name mismatch")
// }
