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

// func TestInterDispatchPolicy_Conformance(t *testing.T) {
// 	for policyName, factory := range registeredInterFlowDispatchPolicies {
// 		policyName := policyName
// 		factory := factory

// 		t.Run(string(policyName), func(t *testing.T) {
// 			t.Run("Properties", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				require.NotNil(t, policy, "Policy factory returned nil")
// 				assert.NotEmpty(t, policy.Name(), "Policy Name() should not be empty")
// 				assert.Equal(t, string(policyName), policy.Name(), "Policy Name() should match registered name")
// 			})

// 			t.Run("SelectQueueFromEmptyBand", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				band := mocks.NewMockPriorityBandAccessor(0, "ConfPrioEmpty", 0, map[string]types.FlowQueueAccessor{}, []string{})
// 				selectedQueue, err := policy.SelectQueue(band)
// 				assert.NoError(t, err, "SelectQueue from an empty band should not error")
// 				assert.Nil(t, selectedQueue, "SelectQueue from an empty band should return nil queue")
// 			})

// 			t.Run("SelectQueueFromBandWithEmptyQueues", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				flowSpec := mocks.NewMockFlowSpecification("conf-flow-emptyq", 0)
// 				qEmpty := mocks.NewMockFlowQueueAccessor(flowSpec, "conf-q-empty", "conf-score-type", nil)
// 				qEmpty.MockLenVal = 0
// 				qEmpty.MockPeekHeadErrorVal = types.ErrQueueEmpty

// 				band := mocks.NewMockPriorityBandAccessor(0, "ConfPrioEmptyQs", 0, map[string]types.FlowQueueAccessor{"flow1": qEmpty}, []string{"flow1"})
// 				selectedQueue, err := policy.SelectQueue(band)
// 				assert.NoError(t, err, "SelectQueue from a band with only empty queues should not error")
// 				assert.Nil(t, selectedQueue, "SelectQueue from a band with only empty queues should return nil queue")
// 			})

// 			t.Run("SelectQueueFromBandWithNonEmptyQueues", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				flowSpec := mocks.NewMockFlowSpecification("conf-flow-nonempty", 0)
// 				item := mocks.NewMockQueueItemAccessor("conf-item", flowSpec.ID(), 0, time.Now(), 1.0)
// 				qNonEmpty := mocks.NewMockFlowQueueAccessor(flowSpec, "conf-q-nonempty", "conf-score-type", nil)
// 				qNonEmpty.MockLenVal = 1
// 				qNonEmpty.MockPeekHeadItemVal = item

// 				band := mocks.NewMockPriorityBandAccessor(0, "ConfPrioNonEmptyQs", 0, map[string]types.FlowQueueAccessor{"flow1": qNonEmpty}, []string{"flow1"})
// 				selectedQueue, err := policy.SelectQueue(band)

// 				// Error handling depends on the policy (e.g., BestHeadPriorityScore might error on type mismatch).
// 				// For basic conformance, if an error occurs, selectedQueue should be nil.
// 				if err != nil {
// 					assert.Nil(t, selectedQueue, "If SelectQueue errors, selected queue must be nil")
// 				} else if selectedQueue != nil {
// 					// If no error and a queue is selected, it must be one from the band.
// 					assert.Same(t, qNonEmpty, selectedQueue, "SelectQueue returned an unexpected queue")
// 				}
// 				// If selectedQueue is nil and err is nil, it means the policy chose not to select.
// 			})

// 			t.Run("SelectQueueWithNilBand", func(t *testing.T) {
// 				policy, err := factory()
// 				require.NoError(t, err, "Policy factory failed")
// 				selectedQueue, err := policy.SelectQueue(nil)
// 				assert.NoError(t, err, "SelectQueue with a nil band should not error")
// 				assert.Nil(t, selectedQueue, "SelectQueue with a nil band should return nil queue")
// 			})
// 		})
// 	}
// }
