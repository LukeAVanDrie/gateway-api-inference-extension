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

func TestTail_SelectVictim_NonEmptyQueueSupportingDoubleEnded(t *testing.T) {
	t.Parallel()
	policy := NewTail()

	tailItem := mocks.NewMockQueueItemAccessor("tailItem", "", 0, time.Now())
	mockQueue := mocks.NewMockFlowQueueAccessor(nil, "tail-test-q-nonempty",
		[]types.QueueCapability{types.CapabilityDoubleEnded}, nil)
	mockQueue.MockLenVal = 2 // Simulate a queue with items
	mockQueue.MockPeekTailItemVal = tailItem

	selected, err := policy.SelectVictim(mockQueue)
	require.NoError(t, err, "SelectVictim should not error for valid operations")
	require.NotNil(t, selected, "SelectVictim from non-empty queue should return an item")
	assert.Equal(t, tailItem.RequestID(), selected.RequestID(), "SelectVictim should return the item from PeekTail")
}

func TestTail_Properties(t *testing.T) {
	t.Parallel()
	policy := NewTail()

	assert.Equal(t, string(TailPreemptionPolicyName), policy.Name(), "Policy name should match constant")
	expectedCaps := []types.QueueCapability{types.CapabilityDoubleEnded}
	assert.ElementsMatch(t, expectedCaps, policy.RequiredQueueCapabilities(), "Required capabilities do not match")
}
