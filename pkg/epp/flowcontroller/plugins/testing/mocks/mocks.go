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

// Package mocks provides shared mock implementations of core flowcontroller interfaces for use in plugin testing.
package mocks

import (
	"context"
	"sort"
	"sync"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

// --- MockFlowSpecification ---

type MockFlowSpecification struct {
	MockID       string
	MockPriority uint
}

var _ types.FlowSpecification = &MockFlowSpecification{}

func NewMockFlowSpecification(id string, priority uint) *MockFlowSpecification {
	return &MockFlowSpecification{id, priority}
}

func (m *MockFlowSpecification) ID() string     { return m.MockID }
func (m *MockFlowSpecification) Priority() uint { return m.MockPriority }

// --- MockFlowControlRequest ---

type MockFlowControlRequest struct {
	MockCtx                 context.Context
	MockFlowIDVal           string
	MockSizeVal             uint64
	MockInitialEffectiveTTL time.Duration
	MockIDVal               string
}

var _ types.FlowControlRequest = &MockFlowControlRequest{}

func NewMockFlowControlRequest(reqID, flowID string, size uint64, ttl time.Duration) *MockFlowControlRequest {
	return &MockFlowControlRequest{
		MockCtx:                 context.Background(), // Default, can be overridden
		MockIDVal:               reqID,
		MockFlowIDVal:           flowID,
		MockSizeVal:             size,
		MockInitialEffectiveTTL: ttl,
	}
}

func (m *MockFlowControlRequest) Context() context.Context { return m.MockCtx }
func (m *MockFlowControlRequest) ID() string               { return m.MockIDVal }
func (m *MockFlowControlRequest) FlowID() string           { return m.MockFlowIDVal }
func (m *MockFlowControlRequest) ByteSize() uint64         { return m.MockSizeVal }

func (m *MockFlowControlRequest) InitialEffectiveTTL() time.Duration {
	return m.MockInitialEffectiveTTL
}

// --- MockQueueItemHandle ---

type MockQueueItemHandle struct {
	RawHandle      any
	Invalidated    bool
	invalidateLock sync.Mutex
}

var _ types.QueueItemHandle = &MockQueueItemHandle{}

func NewMockQueueItemHandle(raw any) *MockQueueItemHandle {
	return &MockQueueItemHandle{RawHandle: raw}
}

func (m *MockQueueItemHandle) Handle() any {
	return m.RawHandle
}

func (m *MockQueueItemHandle) Invalidate() {
	m.invalidateLock.Lock()
	defer m.invalidateLock.Unlock()
	m.Invalidated = true
}

func (m *MockQueueItemHandle) IsInvalidated() bool {
	m.invalidateLock.Lock()
	defer m.invalidateLock.Unlock()
	return m.Invalidated
}

// --- MockQueueItemAccessor ---

type MockQueueItemAccessor struct {
	MockEnqueueTimeVal     time.Time
	MockSizeVal            uint64
	MockFlowIDVal          string
	MockEffectiveTTLVal    time.Duration
	MockRequestIDVal       string
	MockOriginalRequestVal types.FlowControlRequest
	MockHandleVal          types.QueueItemHandle
}

var _ types.QueueItemAccessor = &MockQueueItemAccessor{}

func NewMockQueueItemAccessor(reqID, flowID string, size uint64, enqueueTime time.Time) *MockQueueItemAccessor {
	if flowID == "" {
		flowID = "default-flow"
	}
	return &MockQueueItemAccessor{
		MockRequestIDVal:       reqID,
		MockFlowIDVal:          flowID,
		MockSizeVal:            size,
		MockEnqueueTimeVal:     enqueueTime,
		MockOriginalRequestVal: NewMockFlowControlRequest(reqID, flowID, size, 0), // Basic mock original request
		MockHandleVal:          NewMockQueueItemHandle(nil),                       // Basic mock handle
	}
}

func (m *MockQueueItemAccessor) EnqueueTime() time.Time            { return m.MockEnqueueTimeVal }
func (m *MockQueueItemAccessor) ByteSize() uint64                  { return m.MockSizeVal }
func (m *MockQueueItemAccessor) FlowID() string                    { return m.MockFlowIDVal }
func (m *MockQueueItemAccessor) EffectiveTTL() time.Duration       { return m.MockEffectiveTTLVal }
func (m *MockQueueItemAccessor) RequestID() string                 { return m.MockRequestIDVal }
func (m *MockQueueItemAccessor) Handle() types.QueueItemHandle     { return m.MockHandleVal }
func (m *MockQueueItemAccessor) SetHandle(h types.QueueItemHandle) { m.MockHandleVal = h }

func (m *MockQueueItemAccessor) OriginalRequest() types.FlowControlRequest {
	return m.MockOriginalRequestVal
}

// --- MockItemComparator ---
type MockItemComparator struct {
	MockFunc      types.ItemComparatorFunc
	MockScoreType string
}

func NewMockItemComparator(f types.ItemComparatorFunc, scoreType string) *MockItemComparator {
	if f == nil {
		f = func(a, b types.QueueItemAccessor) bool { return a.EnqueueTime().Before(b.EnqueueTime()) } // Default FCFS
	}
	if scoreType == "" {
		scoreType = "mock_fcfs_enqueue_time_ns_asc" // Default
	}
	return &MockItemComparator{MockFunc: f, MockScoreType: scoreType}
}

var _ types.ItemComparator = &MockItemComparator{}

func (m *MockItemComparator) Func() types.ItemComparatorFunc { return m.MockFunc }
func (m *MockItemComparator) ScoreType() string              { return m.MockScoreType }

// --- MockFlowQueueAccessor ---

type MockFlowQueueAccessor struct {
	MockLenVal           int
	MockByteSizeVal      uint64
	MockNameVal          string
	MockComparatorVal    types.ItemComparator
	MockFlowSpecVal      types.FlowSpecification
	MockCapabilitiesVal  []types.QueueCapability
	MockPeekHeadItemVal  types.QueueItemAccessor
	MockPeekHeadErrorVal error
	MockPeekTailItemVal  types.QueueItemAccessor
	MockPeekTailErrorVal error
}

var _ types.FlowQueueAccessor = &MockFlowQueueAccessor{}

func NewMockFlowQueueAccessor(
	flowSpec types.FlowSpecification,
	name string,
	capabilities []types.QueueCapability,
	comparator types.ItemComparator,
) *MockFlowQueueAccessor {
	if flowSpec == nil {
		flowSpec = NewMockFlowSpecification("default-flow-for-queue", 0)
	}
	return &MockFlowQueueAccessor{
		MockFlowSpecVal:     flowSpec,
		MockComparatorVal:   comparator,
		MockNameVal:         name,
		MockCapabilitiesVal: capabilities,
	}
}

func (m *MockFlowQueueAccessor) Len() int                              { return m.MockLenVal }
func (m *MockFlowQueueAccessor) ByteSize() uint64                      { return m.MockByteSizeVal }
func (m *MockFlowQueueAccessor) Name() string                          { return m.MockNameVal }
func (m *MockFlowQueueAccessor) FlowSpec() types.FlowSpecification     { return m.MockFlowSpecVal }
func (m *MockFlowQueueAccessor) Capabilities() []types.QueueCapability { return m.MockCapabilitiesVal }

func (m *MockFlowQueueAccessor) PeekHead() (types.QueueItemAccessor, error) {
	if m.MockPeekHeadErrorVal != nil {
		return nil, m.MockPeekHeadErrorVal
	}
	if m.MockLenVal == 0 && m.MockPeekHeadItemVal == nil {
		return nil, types.ErrQueueEmpty
	}
	return m.MockPeekHeadItemVal, nil
}

func (m *MockFlowQueueAccessor) PeekTail() (types.QueueItemAccessor, error) {
	if m.MockPeekTailErrorVal != nil {
		return nil, m.MockPeekTailErrorVal
	}
	if m.MockLenVal == 0 && m.MockPeekTailItemVal == nil {
		return nil, types.ErrQueueEmpty
	}
	return m.MockPeekTailItemVal, nil
}

func (m *MockFlowQueueAccessor) Comparator() types.ItemComparator {
	if m.MockComparatorVal != nil {
		return m.MockComparatorVal
	}
	// Return a default comparator if none was provided during construction.
	return NewMockItemComparator(nil, "")
}

// --- MockPriorityBandAccessor ---

type MockPriorityBandAccessor struct {
	MockPriorityVal     uint
	MockPriorityNameVal string
	MockCapacityBytes   uint64
	MockQueues          map[string]types.FlowQueueAccessor // flowID -> FlowQueueAccessor
	MockFlowIDsInOrder  []string                           // Controls iteration order for IterateQueues
}

var _ types.PriorityBandAccessor = &MockPriorityBandAccessor{}

func NewMockPriorityBandAccessor(
	priority uint,
	priorityName string,
	capacityBytes uint64,
	queues map[string]types.FlowQueueAccessor,
	flowOrder []string,
) *MockPriorityBandAccessor {
	return &MockPriorityBandAccessor{
		MockPriorityVal:     priority,
		MockPriorityNameVal: priorityName,
		MockQueues:          queues,
		MockFlowIDsInOrder:  flowOrder,
	}
}

func (m *MockPriorityBandAccessor) Priority() uint        { return m.MockPriorityVal }
func (m *MockPriorityBandAccessor) PriorityName() string  { return m.MockPriorityNameVal }
func (m *MockPriorityBandAccessor) CapacityBytes() uint64 { return m.MockCapacityBytes }

func (m *MockPriorityBandAccessor) FlowIDs() []string {
	if len(m.MockFlowIDsInOrder) > 0 {
		// Return a copy to prevent modification.
		ids := make([]string, len(m.MockFlowIDsInOrder))
		copy(ids, m.MockFlowIDsInOrder)
		return ids
	}
	// Fallback if order not specified for mock, iterate map (order undefined by Go spec but sort for predictability).
	ids := make([]string, 0, len(m.MockQueues))
	for id := range m.MockQueues {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids

}
func (m *MockPriorityBandAccessor) Queue(flowID string) types.FlowQueueAccessor {
	return m.MockQueues[flowID]
}

func (m *MockPriorityBandAccessor) IterateQueues(callback func(q types.FlowQueueAccessor) bool) {
	iterOrder := m.MockFlowIDsInOrder
	if len(iterOrder) == 0 { // Fallback if order not specified for mock
		iterOrder = m.FlowIDs() // This will give sorted keys
	}
	for _, flowID := range iterOrder {
		if q, ok := m.MockQueues[flowID]; ok {
			if !callback(q) {
				return
			}
		}
	}
}
