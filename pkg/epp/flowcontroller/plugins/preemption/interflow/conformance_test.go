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

package interflowpreemption

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/testing/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

func TestInterFlowPreemptionPolicy_Conformance(t *testing.T) {
	t.Parallel()

	for policyName, factory := range registeredInterFlowPreemptionPolicies {
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
			})

			t.Run("SelectVictimQueue_EmptyBand", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")
				band := mocks.NewMockPriorityBandAccessor(0, "conf-empty-band", 0, map[string]types.FlowQueueAccessor{}, []string{})
				selectedQueue, err := policy.SelectVictimQueue(band)
				assert.NoError(t, err, "SelectVictimQueue from an empty band should not error")
				assert.Nil(t, selectedQueue, "SelectVictimQueue from an empty band should return nil queue")
			})

			t.Run("SelectVictimQueue_BandWithEmptyQueues", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")
				flowSpecEmpty := mocks.NewMockFlowSpecification("conf-q-empty", 0)
				qEmpty := mocks.NewMockFlowQueueAccessor(flowSpecEmpty, "q-empty", nil, nil)
				qEmpty.MockLenVal = 0

				bandQueues := map[string]types.FlowQueueAccessor{"conf-q-empty": qEmpty}
				bandFlowIDs := []string{"conf-q-empty"}
				band := mocks.NewMockPriorityBandAccessor(0, "conf-band-q-empty", 0, bandQueues, bandFlowIDs)

				selectedQueue, err := policy.SelectVictimQueue(band)
				assert.NoError(t, err, "SelectVictimQueue from a band with only empty queues should not error")
				assert.Nil(t, selectedQueue, "SelectVictimQueue from a band with only empty queues should return nil queue")
			})

			t.Run("SelectVictimQueue_BandWithNonEmptyQueues", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")
				flowSpecNonEmpty := mocks.NewMockFlowSpecification("conf-q-non-empty", 0)
				qNonEmpty := mocks.NewMockFlowQueueAccessor(flowSpecNonEmpty, "q-non-empty", nil, nil)
				qNonEmpty.MockLenVal = 1

				bandQueues := map[string]types.FlowQueueAccessor{"conf-q-non-empty": qNonEmpty}
				bandFlowIDs := []string{"conf-q-non-empty"}
				band := mocks.NewMockPriorityBandAccessor(0, "conf-band-q-non-empty", 0, bandQueues, bandFlowIDs)

				selectedQueue, err := policy.SelectVictimQueue(band)
				assert.NoError(t, err, "SelectVictimQueue from a band with non-empty queues should not error")
				// Policy might still return nil if it decides not to select, but if it does, it should be from the band.
				if selectedQueue != nil {
					assert.Same(t, qNonEmpty, selectedQueue, "SelectVictimQueue returned an unexpected queue")
				}
			})

			t.Run("SelectVictimQueue_NilBand", func(t *testing.T) {
				t.Parallel()
				policy, err := factory()
				require.NoError(t, err, "Policy factory failed")
				selectedQueue, err := policy.SelectVictimQueue(nil)
				assert.NoError(t, err, "SelectVictimQueue with a nil band should not error")
				assert.Nil(t, selectedQueue, "SelectVictimQueue with a nil band should return nil queue")
			})
		})
	}
}
