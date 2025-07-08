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
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/intraflow/dispatch"
	typesmocks "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types/mocks"

	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/policies/intraflow/dispatch/fcfs"
)

// TestIntraFlowDispatchPolicy_Conformance is the main conformance test suite for `framework.IntraFlowDispatchPolicy`
// implementations.
// It iterates over all policy implementations registered via `dispatch.MustRegisterPolicy` and runs a series of
// sub-tests to ensure they adhere to the `framework.IntraFlowDispatchPolicy` contract.
func TestIntraFlowDispatchPolicy_Conformance(t *testing.T) {
	t.Parallel()

	for policyName, constructor := range dispatch.RegisteredPolicies {
		t.Run(string(policyName), func(t *testing.T) {
			t.Parallel()

			policy, err := constructor()
			require.NoError(t, err, "Policy constructor for %s failed", policyName)
			require.NotNil(t, policy, "Constructor for %s should return a non-nil policy instance", policyName)

			t.Run("Initialization", func(t *testing.T) {
				t.Parallel()
				comp := policy.Comparator()
				require.NotNil(t, comp, "Comparator() for %s should not return nil", policyName)
				assert.NotNil(t, comp.Func(), "Comparator().Func() for %s should not be nil", policyName)
				assert.NotEmpty(t, comp.ScoreType(), "Comparator().ScoreType() for %s should not be empty", policyName)

				caps := policy.RequiredQueueCapabilities()
				assert.NotNil(t, caps, "RequiredQueueCapabilities() for %s should not return a nil slice", policyName)
			})

			t.Run("SelectItem", func(t *testing.T) {
				t.Parallel()

				t.Run("FromNilQueue", func(t *testing.T) {
					t.Parallel()
					item, err := policy.SelectItem(nil)
					require.NoError(t, err, "SelectItem(nil) for %s should not return an error", policyName)
					assert.Nil(t, item, "SelectItem(nil) for %s should return a nil item", policyName)
				})

				t.Run("FromEmptyQueue", func(t *testing.T) {
					t.Parallel()
					mockQueue := &frameworkmocks.MockFlowQueueAccessor{
						PeekHeadErrV: framework.ErrQueueEmpty,
						LenV:         0,
					}
					item, err := policy.SelectItem(mockQueue)
					require.NoError(t, err, "SelectItem from an empty queue for %s should not return an error", policyName)
					assert.Nil(t, item, "SelectItem from an empty queue for %s should return a nil item", policyName)
				})

				t.Run("FromQueueWithItem", func(t *testing.T) {
					t.Parallel()
					expectedItem := typesmocks.NewMockQueueItemAccessor(100, "item", "test-flow")
					mockQueue := &frameworkmocks.MockFlowQueueAccessor{
						PeekHeadV: expectedItem,
						LenV:      1,
					}
					item, err := policy.SelectItem(mockQueue)
					require.NoError(t, err, "SelectItem from a non-empty queue for %s should not return an error", policyName)
					assert.Equal(t, expectedItem, item, "SelectItem for %s should return the item from PeekHead", policyName)
				})

				t.Run("NilQueue", func(t *testing.T) {
					t.Parallel()
					item, err := policy.SelectItem(nil)
					require.NoError(t, err, "SelectItem with a nil queue should not return an error")
					assert.Nil(t, item, "SelectItem with a nil queue should return a nil item")
				})
			})

			t.Run("Comparator", func(t *testing.T) {
				t.Parallel()
				comparator := policy.Comparator()
				require.NotNil(t, comparator, "Comparator for %s cannot be nil for this test", policyName)
				compareFunc := comparator.Func()
				require.NotNil(t, compareFunc, "Comparator function for %s cannot be nil", policyName)
				assert.NotEmpty(t, comparator.ScoreType(), "ScoreType for %s cannot be empty", policyName)
			})
		})
	}
}
