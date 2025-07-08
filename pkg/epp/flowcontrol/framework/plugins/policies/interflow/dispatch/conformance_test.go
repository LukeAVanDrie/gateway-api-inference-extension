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

package dispatch_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	frameworkmocks "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/interflow/dispatch"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"

	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/interflow/dispatch/besthead"
	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/interflow/dispatch/roundrobin"
)

// TestInterFlowDispatchPolicy_Conformance is the main conformance test suite for `framework.InterFlowDispatchPolicy`
// implementations.
// It iterates over all policy implementations registered via `dispatch.MustRegisterPolicy` and runs a series of
// sub-tests to ensure they adhere to the `framework.InterFlowDispatchPolicy` contract.
func TestInterFlowDispatchPolicy_Conformance(t *testing.T) {
	t.Parallel()

	for policyName, constructor := range dispatch.RegisteredPolicies {
		t.Run(string(policyName), func(t *testing.T) {
			t.Parallel()

			policy, err := constructor()
			require.NoError(t, err, "Policy constructor for %s failed", policyName)
			require.NotNil(t, policy, "Constructor for %s should return a non-nil policy instance", policyName)

			t.Run("SelectQueue", func(t *testing.T) {
				t.Parallel()

				t.Run("WithNilPriorityBandAccessor", func(t *testing.T) {
					t.Parallel()
					selectedQueue, err := policy.SelectQueue(nil)
					require.NoError(t, err, "SelectQueue(nil) for %s should not return an error", policyName)
					assert.Nil(t, selectedQueue, "SelectQueue(nil) for %s should return a nil queue", policyName)
				})

				t.Run("WithEmptyPriorityBandAccessor", func(t *testing.T) {
					t.Parallel()
					mockBand := &frameworkmocks.MockPriorityBandAccessor{
						FlowIDsV:       []string{},
						IterateQueuesV: func(callback func(queue framework.FlowQueueAccessor) bool) { /* no-op */ },
					}
					selectedQueue, err := policy.SelectQueue(mockBand)
					require.NoError(t, err, "SelectQueue from an empty band for %s should not return an error", policyName)
					assert.Nil(t, selectedQueue, "SelectQueue from an empty band for %s should return a nil queue", policyName)
				})

				t.Run("WithBandHavingOneEmptyQueue", func(t *testing.T) {
					t.Parallel()
					flowID := "flow-empty"
					mockQueue := &frameworkmocks.MockFlowQueueAccessor{
						LenV:         0,
						PeekHeadErrV: framework.ErrQueueEmpty, // Expected when Len is 0
						FlowSpecV:    types.FlowSpecification{ID: flowID},
					}
					mockBand := &frameworkmocks.MockPriorityBandAccessor{
						FlowIDsV: []string{flowID},
						QueueFuncV: func(fID string) framework.FlowQueueAccessor {
							if fID == flowID {
								return mockQueue
							}
							return nil
						},
					}
					selectedQueue, err := policy.SelectQueue(mockBand)
					require.NoError(t, err, "SelectQueue from a band with one empty queue for %s should not return an error",
						policyName)
					assert.Nil(t, selectedQueue, "SelectQueue from a band with one empty queue for %s should return a nil queue",
						policyName)
				})
			})
		})
	}
}
