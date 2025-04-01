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

// func TestIntraDispatchPolicy_Conformance(t *testing.T) {
// 	for policyName, factory := range registeredIntraFlowDispatchPolicies {
// 		policyName := policyName
// 		factory := factory

// 		t.Run(string(policyName), func(t *testing.T) {
// 			t.Parallel()

// 			t.Run("Properties", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				require.NotNil(t, policy, "Policy factory returned nil")

// 				assert.NotEmpty(t, policy.Name(), "Policy Name() should not be empty")
// 				assert.Equal(t, string(policyName), policy.Name(), "Policy Name() should match registered name")

// 				assert.NotNil(t, policy.RequiredQueueCapabilities(), "RequiredQueueCapabilities() should not return nil (can be empty slice)")
// 				assert.NotEmpty(t, policy.PriorityScoreType(), "PriorityScoreType() should not be empty")

// 				// ItemComparator can be nil (e.g., for FCFS relying on queue order).
// 				_ = policy.ItemComparator() // Just call it to ensure it doesn't panic
// 			})

// 			t.Run("SelectItemFromEmptyQueue", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				mockQueue := mocks.NewMockFlowQueueAccessor(nil, "conf-empty-q", policy.PriorityScoreType(), policy.RequiredQueueCapabilities())
// 				mockQueue.MockLenVal = 0
// 				mockQueue.MockPeekHeadErrorVal = types.ErrQueueEmpty // FCFS uses PeekHead

// 				selectedItem := policy.SelectItem(mockQueue)
// 				assert.Nil(t, selectedItem, "SelectItem from an empty queue should return nil")
// 			})

// 			t.Run("SelectItemFromNonEmptyQueue", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				flowSpec := mocks.NewMockFlowSpecification("conf-flow", 0)
// 				item1 := mocks.NewMockQueueItemAccessor("item1", flowSpec.ID(), 0, time.Now(), 0)

// 				mockQueue := mocks.NewMockFlowQueueAccessor(flowSpec, "conf-nonempty-q", policy.PriorityScoreType(), policy.RequiredQueueCapabilities())
// 				mockQueue.MockLenVal = 1
// 				mockQueue.MockPeekHeadItemVal = item1 // FCFS uses PeekHead

// 				selectedItem := policy.SelectItem(mockQueue)
// 				// It's okay if it returns nil (e.g. policy decides not to select yet), but if it returns an item, it should be
// 				// one from the queue.
// 				if selectedItem != nil {
// 					assert.Equal(t, item1.RequestID(), selectedItem.RequestID(), "SelectItem returned an unexpected item")
// 				}
// 			})

// 			t.Run("SelectItemWithNilQueue", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				selectedItem := policy.SelectItem(nil)
// 				assert.Nil(t, selectedItem, "SelectItem with a nil queue should return nil")
// 			})
// 		})
// 	}
// }
