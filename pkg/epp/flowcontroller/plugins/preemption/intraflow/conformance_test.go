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

package intraflowpreemption

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/testing/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

func TestIntraPreemptionPolicy_Conformance(t *testing.T) {
	t.Parallel()

	for policyName, factory := range registeredIntraFlowPreemptionPolicies {
		policyName := policyName
		factory := factory

		t.Run(string(policyName), func(t *testing.T) {
			t.Parallel()

			t.Run("Properties", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")
				require.NotNil(t, policy, "Policy factory returned nil")

				assert.NotEmpty(t, policy.Name(), "Policy Name() should not be empty")
				assert.Equal(t, string(policyName), policy.Name(), "Policy Name() should match registered name")
				assert.NotNil(t, policy.RequiredQueueCapabilities(), "RequiredQueueCapabilities() should not return nil")
			})

			t.Run("SelectVictim_EmptyQueue", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")

				mockQueue := mocks.NewMockFlowQueueAccessor(nil, "conf-empty-q", policy.RequiredQueueCapabilities(), nil)
				mockQueue.MockLenVal = 0

				victim, err := policy.SelectVictim(mockQueue)
				assert.NoError(t, err, "Policy's SelectVictim from an empty queue should not error")
				assert.Nil(t, victim, "SelectVictim from an empty or incompatible queue should return nil victim")
			})

			t.Run("SelectVictim_NonEmptyQueue", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")

				item1 := mocks.NewMockQueueItemAccessor("item1", "", 0, time.Now())
				mockQueue := mocks.NewMockFlowQueueAccessor(nil, "conf-nonempty-q", policy.RequiredQueueCapabilities(), nil)
				mockQueue.MockLenVal = 1
				// For a single item queue, both head and tail would point to this item.
				mockQueue.MockPeekHeadItemVal = item1
				mockQueue.MockPeekTailItemVal = item1

				victim, err := policy.SelectVictim(mockQueue)
				assert.NoError(t, err, "Policy's SelectVictim from a non-empty queue should not error")
				if victim != nil { // Policy may still chooses not to preempt
					assert.Equal(t, item1.RequestID(), victim.RequestID(), "SelectVictim returned an unexpected item")
				}
			})

			t.Run("SelectVictim_NilQueue", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")

				victim, err := policy.SelectVictim(nil)
				assert.NoError(t, err, "Policy's SelectVictim with a nil queue should not error")
				assert.Nil(t, victim, "SelectVictim with a nil queue should return nil victim")
			})

			t.Run("SelectVictim_QueueDoesNotSupportCapability", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")

				requiredCaps := policy.RequiredQueueCapabilities()
				if len(requiredCaps) == 0 {
					t.Skip("Policy does not require any queue capabilities, skipping capability check.")
				}

				// Mock a queue that explicitly has no capabilities.
				mockQueue := mocks.NewMockFlowQueueAccessor(nil, "conf-no-cap-q", []types.QueueCapability{}, nil)
				mockQueue.MockLenVal = 1 // Make it non-empty to distinguish from empty queue case
				mockQueue.MockPeekHeadErrorVal = types.ErrOperationNotSupported
				mockQueue.MockPeekTailErrorVal = types.ErrOperationNotSupported

				victim, err := policy.SelectVictim(mockQueue)
				assert.ErrorIs(t, err, types.ErrPolicyQueueMismatch,
					"Policy should return ErrPolicyQueueMismatch when queue lacks required capability")
				assert.Nil(t, victim, "SelectVictim should return nil victim when returning an error due to capability mismatch")
			})
		})
	}
}
