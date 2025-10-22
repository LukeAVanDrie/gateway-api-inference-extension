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

package besthead

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	frameworkmocks "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types/mocks"
)

// --- Test Doubles (Fakes) for BestHead ---

// enqueueTimeComparatorFunc is a test utility. An earlier enqueue time is "better".
func enqueueTimeComparatorFunc(a, b types.QueueItemAccessor) bool {
	return a.EnqueueTime().Before(b.EnqueueTime())
}

// newTestBand creates a new MockPriorityBandAccessor based with the provided queues.
func newTestBand(t *testing.T, queues ...framework.FlowQueueAccessor) *frameworkmocks.MockPriorityBandAccessor {
	t.Helper()
	flowKeys := make([]types.FlowKey, 0, len(queues))
	queuesByID := make(map[string]framework.FlowQueueAccessor, len(queues))
	for _, q := range queues {
		key := q.FlowKey()
		flowKeys = append(flowKeys, key)
		queuesByID[key.ID] = q
	}
	return &frameworkmocks.MockPriorityBandAccessor{
		FlowKeysFunc: func() []types.FlowKey { return flowKeys },
		QueueFunc: func(id string) framework.FlowQueueAccessor {
			return queuesByID[id]
		},
		IterateQueuesFunc: func(iterator func(queue framework.FlowQueueAccessor) bool) {
			for _, key := range flowKeys {
				if !iterator(queuesByID[key.ID]) {
					break
				}
			}
		},
	}
}

// --- Tests ---

func TestBestHead_New(t *testing.T) {
	t.Parallel()
	plugin, err := newBestHead(PolicyNameBestHead, nil, nil)
	require.NoError(t, err, "NewBestHead should not return an error for a valid configuration")
	require.Equal(t, PolicyNameBestHead, plugin.TypedName().Name, "plugin name should match the policy's constant")
	require.Equal(t, framework.InterFlowDispatchPolicyType, plugin.TypedName().Type,
		"plugin type should be InterFlowDispatchPolicy")
}

func TestBestHead_SelectQueue(t *testing.T) {
	t.Parallel()
	now := time.Now()

	key1 := types.FlowKey{ID: "flow1"}
	key2 := types.FlowKey{ID: "flow2"}
	item1 := mocks.NewMockQueueItemAccessor(10, "item1", key1) // Better item
	item2 := mocks.NewMockQueueItemAccessor(5, "item2", key2)  // Worse item
	item1.EnqueueTimeV = now.Add(-10 * time.Second)
	item2.EnqueueTimeV = now.Add(-5 * time.Second)

	validComparator := &frameworkmocks.MockItemComparator{
		FuncV:      enqueueTimeComparatorFunc,
		ScoreTypeV: "enqueue_time",
	}
	incompatibleComparator := &frameworkmocks.MockItemComparator{
		FuncV:      enqueueTimeComparatorFunc,
		ScoreTypeV: "incompatible_score_type",
	}
	nilFuncComparator := &frameworkmocks.MockItemComparator{
		FuncV:      nil,
		ScoreTypeV: "enqueue_time",
	}

	// --- Mock Queue Definitions ---
	queue1Good := &frameworkmocks.MockFlowQueueAccessor{
		FlowKeyV:    key1,
		ComparatorV: validComparator,
		LenV:        1,
		PeekHeadV:   item1,
	}
	queue2Good := &frameworkmocks.MockFlowQueueAccessor{
		FlowKeyV:    key2,
		ComparatorV: validComparator,
		LenV:        1,
		PeekHeadV:   item2,
	}
	queue1Empty := &frameworkmocks.MockFlowQueueAccessor{
		FlowKeyV:    key1,
		ComparatorV: validComparator,
		LenV:        0,
	}

	testCases := []struct {
		name                 string
		band                 framework.PriorityBandAccessor
		expectedSelectionKey *types.FlowKey
		expectErr            bool
		errContains          string
	}{
		{
			name:                 "selects queue with the best head item from two valid queues with best first",
			band:                 newTestBand(t, queue1Good, queue2Good),
			expectedSelectionKey: &key1,
		},
		{
			name:                 "selects queue with the best head item from two valid queues with best last",
			band:                 newTestBand(t, queue2Good, queue1Good),
			expectedSelectionKey: &key1,
		},
		{
			name:                 "ignores empty queues and selects the only non-empty one",
			band:                 newTestBand(t, queue1Empty, queue2Good),
			expectedSelectionKey: &key2,
		},
		{
			name: "returns nil when all queues are empty",
			band: newTestBand(t, queue1Empty, &frameworkmocks.MockFlowQueueAccessor{
				FlowKeyV:    key2,
				ComparatorV: validComparator,
				LenV:        0,
			}),
			expectedSelectionKey: nil,
		},
		{
			name:                 "returns nil when the band is nil",
			band:                 nil,
			expectedSelectionKey: nil,
		},
		{
			name: "returns error for incompatible comparators",
			band: newTestBand(t, queue1Good, &frameworkmocks.MockFlowQueueAccessor{
				FlowKeyV:    key2,
				ComparatorV: incompatibleComparator,
				LenV:        1,
				PeekHeadV:   item2,
			}),
			expectErr:   true,
			errContains: framework.ErrIncompatiblePriorityType.Error(),
		},
		{
			name: "handles queues with nil items gracefully and selects the valid one",
			band: newTestBand(t, queue2Good, &frameworkmocks.MockFlowQueueAccessor{
				FlowKeyV:    key1,
				ComparatorV: validComparator,
				LenV:        1,
				PeekHeadV:   nil,
			}),
			expectedSelectionKey: &key2,
		},
		{
			name: "returns error for nil comparator",
			band: newTestBand(t, queue2Good, &frameworkmocks.MockFlowQueueAccessor{
				FlowKeyV:    key1,
				ComparatorV: nil,
				LenV:        1,
				PeekHeadV:   item1,
			}),
			expectErr:   true,
			errContains: "comparator",
		},
		{
			name: "returns error for nil comparator function",
			band: newTestBand(t, queue1Good, &frameworkmocks.MockFlowQueueAccessor{
				FlowKeyV:    key2,
				ComparatorV: nilFuncComparator,
				LenV:        1,
				PeekHeadV:   item2,
			}),
			expectErr:   true,
			errContains: "comparator function",
		},
		{
			name: "skips queue with PeekHead error and selects other",
			band: newTestBand(t, queue2Good, &frameworkmocks.MockFlowQueueAccessor{
				FlowKeyV:     key1,
				ComparatorV:  validComparator,
				LenV:         1,
				PeekHeadErrV: errors.New("internal peek error"),
			}),
			expectedSelectionKey: &key2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plugin, err := newBestHead(PolicyNameBestHead, nil, nil)
			require.NoError(t, err, "test setup failed: could not create policy")
			policy := plugin.(framework.InterFlowDispatchPolicy)

			selected, err := policy.SelectQueue(tc.band)

			if tc.expectErr {
				require.Error(t, err, "expected an error from SelectQueue")
				if tc.errContains != "" {
					require.ErrorContains(t, err, tc.errContains, "error message did not contain expected text")
				}
				require.Nil(t, selected, "no queue should be selected when an error occurs")
			} else {
				require.NoError(t, err, "expected no error from SelectQueue")
				if tc.expectedSelectionKey == nil {
					require.Nil(t, selected, "expected no queue to be selected")
				} else {
					require.NotNil(t, selected, "expected a queue to be selected")
					require.Equal(t, *tc.expectedSelectionKey, selected.FlowKey(), "the wrong queue was selected")
				}
			}
		})
	}
}
