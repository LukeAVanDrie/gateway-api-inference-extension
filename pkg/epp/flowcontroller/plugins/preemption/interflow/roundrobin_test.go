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

func TestRoundRobin_SelectVictimQueue(t *testing.T) {
	t.Parallel()
	policy := NewRoundRobin()

	// Use shared mocks for FlowSpecification
	flowSpecA := mocks.NewMockFlowSpecification("flowA", 0)
	flowSpecB := mocks.NewMockFlowSpecification("flowB", 0)
	flowSpecC := mocks.NewMockFlowSpecification("flowC", 0)
	flowSpecEmpty := mocks.NewMockFlowSpecification("flowEmpty", 0)

	// Use shared mocks for QueueAccessor
	qA := mocks.NewMockFlowQueueAccessor(flowSpecA, "qA", nil, nil)
	qA.MockLenVal = 1
	qB := mocks.NewMockFlowQueueAccessor(flowSpecB, "qB", nil, nil)
	qB.MockLenVal = 1
	qC := mocks.NewMockFlowQueueAccessor(flowSpecC, "qC", nil, nil)
	qC.MockLenVal = 1
	qEmpty := mocks.NewMockFlowQueueAccessor(flowSpecEmpty, "qEmpty", nil, nil)
	qEmpty.MockLenVal = 0

	t.Run("SelectsInRoundRobinOrder", func(t *testing.T) {
		t.Parallel()

		// The RoundRobin policy sorts FlowIDs internally.
		// We provide MockFlowIDsInOrder sorted for clarity.
		bandQueues := map[string]types.FlowQueueAccessor{"flowA": qA, "flowB": qB, "flowC": qC}
		bandFlowIDs := []string{"flowA", "flowB", "flowC"} // Expected sorted order
		band := mocks.NewMockPriorityBandAccessor(0, "TestBand1", 0, bandQueues, bandFlowIDs)

		// Expected order based on sorted flowIDs: flowA, flowB, flowC.
		selected, err := policy.SelectVictimQueue(band)
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, "flowA", selected.FlowSpec().ID())

		selected, err = policy.SelectVictimQueue(band)
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, "flowB", selected.FlowSpec().ID())

		selected, err = policy.SelectVictimQueue(band)
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, "flowC", selected.FlowSpec().ID())

		// Wraps around
		selected, err = policy.SelectVictimQueue(band)
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, "flowA", selected.FlowSpec().ID())
	})

	t.Run("SkipsEmptyQueues", func(t *testing.T) {
		t.Parallel()

		policy := NewRoundRobin() // Fresh policy for clean state
		// The RoundRobin policy sorts FlowIDs internally.
		// We provide MockFlowIDsInOrder sorted for clarity.
		bandQueues := map[string]types.FlowQueueAccessor{"flowA": qA, "flowEmpty": qEmpty, "flowC": qC}
		bandFlowIDs := []string{"flowA", "flowC", "flowEmpty"} // Expected sorted order of all flow IDs
		band := mocks.NewMockPriorityBandAccessor(0, "TestBand2", 0, bandQueues, bandFlowIDs)

		// Expected order: flowA, flowC (flowEmpty is skipped).
		selected, err := policy.SelectVictimQueue(band) // Should pick flowA
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, "flowA", selected.FlowSpec().ID())

		selected, err = policy.SelectVictimQueue(band) // Should pick flowC
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, "flowC", selected.FlowSpec().ID())

		selected, err = policy.SelectVictimQueue(band) // Wraps around to flowA
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, "flowA", selected.FlowSpec().ID())
	})
}
