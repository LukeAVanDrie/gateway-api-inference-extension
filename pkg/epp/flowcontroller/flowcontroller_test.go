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

package flowcontroller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/config"
	mocks "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/testing/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

// Test Constants
const (
	testAsyncProcessingWait = 100 * time.Millisecond
)

// --- Mock Saturation Detectors ---

type mockSaturationDetector struct {
	isSaturated bool
	mu          sync.RWMutex
}

func newMockSaturationDetector(initialSaturation bool) *mockSaturationDetector {
	return &mockSaturationDetector{isSaturated: initialSaturation}
}

func (m *mockSaturationDetector) IsSaturated() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isSaturated
}

func (m *mockSaturationDetector) SetSaturated(s bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isSaturated = s
}

var _ SaturationDetector = &mockSaturationDetector{}

// --- Mock Clock ---
type mockClock struct {
	mu          sync.Mutex
	currentTime time.Time
}

func newMockClock(initialTime time.Time) *mockClock {
	return &mockClock{currentTime: initialTime}
}

func (mc *mockClock) Now() time.Time {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return mc.currentTime
}

var _ clock = &mockClock{}

// --- Mock FlowRegistry ---
type mockFlowRegistry struct {
	types.FlowRegistry               //  For methods not directly called by FlowController, will panic if called
	GetActiveManagedQueueFunc        func(flowID string) (types.ManagedQueue, error)
	GetPriorityBandAccessorFunc      func(priority uint) (types.PriorityBandAccessor, error)
	GetManagedQueueFunc              func(flowID string, priority uint) (types.ManagedQueue, error)
	GetInterFlowDispatchPolicyFunc   func(priority uint) (types.InterFlowDispatchPolicy, error)
	GetInterFlowPreemptionPolicyFunc func(priority uint) (types.InterFlowPreemptionPolicy, error)
	GetIntraFlowDispatchPolicyFunc   func(flowID string, priority uint) (types.IntraFlowDispatchPolicy, error)
	GetIntraFlowPreemptionPolicyFunc func(flowID string, priority uint) (types.IntraFlowPreemptionPolicy, error)
	GetAllOrderedPriorityLevelsFunc  func() []uint
	GetGetStatsFunc                  func() types.FlowRegistryStats
}

func (mfr *mockFlowRegistry) ActiveManagedQueue(flowID string) (types.ManagedQueue, error) {
	if mfr.GetActiveManagedQueueFunc != nil {
		return mfr.GetActiveManagedQueueFunc(flowID)
	}
	panic("mockFlowRegistry.ActiveManagedQueue not implemented")
}

func (mfr *mockFlowRegistry) PriorityBandAccessor(priority uint) (types.PriorityBandAccessor, error) {
	if mfr.GetPriorityBandAccessorFunc != nil {
		return mfr.GetPriorityBandAccessorFunc(priority)
	}
	panic("mockFlowRegistry.PriorityBandAccessor not implemented")
}

func (mfr *mockFlowRegistry) ManagedQueue(flowID string, priority uint) (types.ManagedQueue, error) {
	if mfr.GetManagedQueueFunc != nil {
		return mfr.GetManagedQueueFunc(flowID, priority)
	}
	panic("mockFlowRegistry.ManagedQueue not implemented")
}

func (mfr *mockFlowRegistry) InterFlowDispatchPolicy(priority uint) (types.InterFlowDispatchPolicy, error) {
	if mfr.GetInterFlowDispatchPolicyFunc != nil {
		return mfr.GetInterFlowDispatchPolicyFunc(priority)
	}
	panic("mockFlowRegistry.InterFlowDispatchPolicy not implemented")
}

func (mfr *mockFlowRegistry) InterFlowPreemptionPolicy(priority uint) (types.InterFlowPreemptionPolicy, error) {
	if mfr.GetInterFlowPreemptionPolicyFunc != nil {
		return mfr.GetInterFlowPreemptionPolicyFunc(priority)
	}
	panic("mockFlowRegistry.InterFlowPreemptionPolicy not implemented")
}

func (mfr *mockFlowRegistry) IntraFlowDispatchPolicy(
	flowID string,
	priority uint,
) (types.IntraFlowDispatchPolicy, error) {
	if mfr.GetIntraFlowDispatchPolicyFunc != nil {
		return mfr.GetIntraFlowDispatchPolicyFunc(flowID, priority)
	}
	panic("mockFlowRegistry.IntraFlowDispatchPolicy not implemented")
}

func (mfr *mockFlowRegistry) IntraFlowPreemptionPolicy(
	flowID string,
	priority uint,
) (types.IntraFlowPreemptionPolicy, error) {
	if mfr.GetIntraFlowPreemptionPolicyFunc != nil {
		return mfr.GetIntraFlowPreemptionPolicyFunc(flowID, priority)
	}
	panic("mockFlowRegistry.IntraFlowPreemptionPolicy not implemented")
}

func (mfr *mockFlowRegistry) AllOrderedPriorityLevels() []uint {
	if mfr.GetAllOrderedPriorityLevelsFunc != nil {
		return mfr.GetAllOrderedPriorityLevelsFunc()
	}
	panic("mockFlowRegistry.AllOrderedPriorityLevels not implemented")
}

func (mfr *mockFlowRegistry) GetStats() types.FlowRegistryStats {
	if mfr.GetGetStatsFunc != nil {
		return mfr.GetGetStatsFunc()
	}
	panic("mockFlowRegistry.GetStats not implemented")
}

var _ types.FlowRegistry = &mockFlowRegistry{}

// --- Mock ManagedQueue ---

type mockManagedQueueAddImpl func(item types.QueueItemAccessor) (newLen uint64, newByteSize uint64, err error)
type mockManagedQueueRemoveImpl func(handle types.QueueItemHandle) (
	removedItem types.QueueItemAccessor, newLen uint64, newByteSize uint64, err error,
)
type mockManagedQueueCleanupExpiredImpl func(currentTime time.Time, isItemExpired types.IsItemExpiredFunc) (
	removedItemsInfo []types.ExpiredItemInfo, err error,
)

type mockManagedQueue struct {
	types.ManagedQueue // For methods not directly called by FlowController, will panic if called
	MockFlowSpecVal    types.FlowSpecification
	MockNameVal        string
	AddImpl            mockManagedQueueAddImpl
	RemoveImpl         mockManagedQueueRemoveImpl
	CleanupExpiredImpl mockManagedQueueCleanupExpiredImpl
}

func (mmq *mockManagedQueue) FlowSpec() types.FlowSpecification { return mmq.MockFlowSpecVal }
func (mmq *mockManagedQueue) Name() string                      { return mmq.MockNameVal }

func (mmq *mockManagedQueue) Add(item types.QueueItemAccessor) (newLen uint64, newByteSize uint64, err error) {
	if mmq.AddImpl != nil {
		return mmq.AddImpl(item)
	}
	panic("mockManagedQueue.AddImpl not set")
}

func (mmq *mockManagedQueue) Remove(
	handle types.QueueItemHandle,
) (
	removedItem types.QueueItemAccessor, newLen uint64, newByteSize uint64, err error) {
	if mmq.RemoveImpl != nil {
		return mmq.RemoveImpl(handle)
	}
	panic("mockManagedQueue.RemoveImpl not set")
}

func (mmq *mockManagedQueue) CleanupExpired(
	currentTime time.Time,
	isItemExpired types.IsItemExpiredFunc,
) (removedItemsInfo []types.ExpiredItemInfo, err error) {
	if mmq.CleanupExpiredImpl != nil {
		return mmq.CleanupExpiredImpl(currentTime, isItemExpired)
	}
	panic("mockManagedQueue.CleanupExpiredImpl not set")
}

var _ types.ManagedQueue = &mockManagedQueue{}

// --- Mock Policies ---
type mockInterFlowDispatchPolicy struct {
	SelectQueueFunc func(band types.PriorityBandAccessor) (selectedQueue types.FlowQueueAccessor, err error)
	NameFunc        func() string
}

func (m *mockInterFlowDispatchPolicy) SelectQueue(band types.PriorityBandAccessor) (types.FlowQueueAccessor, error) {
	if m.SelectQueueFunc != nil {
		return m.SelectQueueFunc(band)
	}
	panic("mockInterFlowDispatchPolicy.SelectQueueFunc not set")
}
func (m *mockInterFlowDispatchPolicy) Name() string {
	if m.NameFunc != nil {
		return m.NameFunc()
	}
	return "mockInterFlowDispatchPolicy"
}

var _ types.InterFlowDispatchPolicy = &mockInterFlowDispatchPolicy{}

type mockIntraFlowDispatchPolicy struct {
	SelectItemFunc         func(queue types.FlowQueueAccessor) (selectedItem types.QueueItemAccessor)
	ComparatorFunc         func() types.ItemComparator
	RequiredQueuesCapsFunc func() []types.QueueCapability
	NameFunc               func() string
}

func (m *mockIntraFlowDispatchPolicy) SelectItem(queue types.FlowQueueAccessor) types.QueueItemAccessor {
	if m.SelectItemFunc != nil {
		return m.SelectItemFunc(queue)
	}
	panic("mockIntraFlowDispatchPolicy.SelectItemFunc not set")
}

func (m *mockIntraFlowDispatchPolicy) Comparator() types.ItemComparator {
	if m.ComparatorFunc != nil {
		return m.ComparatorFunc()
	}
	// Return a default mock comparator if not set, as FlowController might indirectly access this via registry.
	return mocks.NewMockItemComparator(nil, "default-mock-comparator")
}

func (m *mockIntraFlowDispatchPolicy) RequiredQueueCapabilities() []types.QueueCapability {
	if m.RequiredQueuesCapsFunc != nil {
		return m.RequiredQueuesCapsFunc()
	}
	return nil // Default: no specific capabilities required by mock
}

func (m *mockIntraFlowDispatchPolicy) Name() string {
	if m.NameFunc != nil {
		return m.NameFunc()
	}
	return "mockIntraFlowDispatchPolicy"
}

var _ types.IntraFlowDispatchPolicy = &mockIntraFlowDispatchPolicy{}

type mockInterFlowPreemptionPolicy struct {
	SelectVictimQueueFunc func(victimBand types.PriorityBandAccessor) (victimQueue types.FlowQueueAccessor, err error)
	NameFunc              func() string
}

func (m *mockInterFlowPreemptionPolicy) SelectVictimQueue(
	victimBand types.PriorityBandAccessor,
) (types.FlowQueueAccessor, error) {
	if m.SelectVictimQueueFunc != nil {
		return m.SelectVictimQueueFunc(victimBand)
	}
	panic("mockInterFlowPreemptionPolicy.SelectVictimQueueFunc not set")
}
func (m *mockInterFlowPreemptionPolicy) Name() string {
	if m.NameFunc != nil {
		return m.NameFunc()
	}
	return "mockInterFlowPreemptionPolicy"
}

var _ types.InterFlowPreemptionPolicy = &mockInterFlowPreemptionPolicy{}

type mockIntraFlowPreemptionPolicy struct {
	SelectVictimFunc       func(queue types.FlowQueueAccessor) (victimItem types.QueueItemAccessor, err error)
	RequiredQueuesCapsFunc func() []types.QueueCapability
	NameFunc               func() string
}

func (m *mockIntraFlowPreemptionPolicy) SelectVictim(queue types.FlowQueueAccessor) (types.QueueItemAccessor, error) {
	if m.SelectVictimFunc != nil {
		return m.SelectVictimFunc(queue)
	}
	panic("mockIntraFlowPreemptionPolicy.SelectVictimFunc not set")
}
func (m *mockIntraFlowPreemptionPolicy) RequiredQueueCapabilities() []types.QueueCapability {
	if m.RequiredQueuesCapsFunc != nil {
		return m.RequiredQueuesCapsFunc()
	}
	return nil
}
func (m *mockIntraFlowPreemptionPolicy) Name() string {
	if m.NameFunc != nil {
		return m.NameFunc()
	}
	return "mockIntraFlowPreemptionPolicy"
}

var _ types.IntraFlowPreemptionPolicy = &mockIntraFlowPreemptionPolicy{}

// --- Test Rig ---
type flowControllerTestRig struct {
	fc               *FlowController
	cfg              config.FlowControllerConfig
	mockRegistry     *mockFlowRegistry
	mockSatDetector  SaturationDetector
	mockClock        *mockClock
	logger           logr.Logger
	t                *testing.T
	cancelRunContext context.CancelFunc
}

func defaultTestFlowControllerConfig() config.FlowControllerConfig {
	return config.FlowControllerConfig{
		DefaultQueueTTL:       30 * time.Second,
		ExpiryCleanupInterval: 100 * time.Millisecond,
		MaxGlobalBytes:        1024 * 1024, // 1MB
	}
}

func setupTestRig(
	t *testing.T,
	cfg config.FlowControllerConfig,
	registry *mockFlowRegistry,
	satDetector SaturationDetector,
) (*flowControllerTestRig, func()) {
	t.Helper()

	if cfg.ExpiryCleanupInterval == 0 {
		cfg.ExpiryCleanupInterval = 100 * time.Millisecond // Ensure a default for tests
	}

	rig := &flowControllerTestRig{
		cfg:             cfg,
		mockRegistry:    registry,
		mockSatDetector: satDetector,
		mockClock:       newMockClock(time.Now()),
		logger:          logr.Discard(),
		t:               t,
	}

	var err error
	rig.fc, err = NewFlowController(rig.mockSatDetector, rig.mockRegistry, cfg, rig.logger)
	require.NoError(t, err, "NewFlowController failed")
	rig.fc.clock = rig.mockClock

	runCtx, cancelRunCtx := context.WithCancel(context.Background())
	rig.cancelRunContext = cancelRunCtx

	// Start FC's Run loop.
	fcDone := make(chan struct{})
	go func() {
		defer close(fcDone)
		rig.fc.Run(runCtx)
	}()

	cleanup := func() {
		t.Log("TestRig: Initiating cleanup, cancelling Run context.")
		cancelRunCtx()
		select {
		case <-fcDone:
			t.Log("TestRig: FlowController Run loop completed.")
		case <-time.After(2 * time.Second): // Timeout for graceful shutdown
			t.Error("TestRig: Timeout waiting for FlowController Run loop to complete.")
		}
		// Additional check for stopCh, though fcDone should be sufficient
		select {
		case <-rig.fc.stopCh:
			t.Log("TestRig: FlowController stopCh is closed.")
		default:
			t.Log("TestRig: FlowController stopCh was not closed (or already checked).")
		}
	}

	return rig, cleanup
}

// --- Test Helper Functions ---

func newTestRequest(
	t *testing.T,
	reqID,
	flowID string,
	size uint64,
	ttl time.Duration,
	ctx context.Context,
) types.FlowControlRequest {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	return &mocks.MockFlowControlRequest{
		MockCtx:                 ctx,
		MockIDVal:               reqID,
		MockFlowIDVal:           flowID,
		MockSizeVal:             size,
		MockInitialEffectiveTTL: ttl,
	}
}

// Helper to wait for an item to be finalized and check its outcome.
func expectOutcome(
	t *testing.T,
	itemDone <-chan struct{},
	timeout time.Duration,
	getFinalStateFunc func() (types.QueueOutcome, error),
	expectedOutcome types.QueueOutcome,
	expectError bool,
	errorWraps []error, // Ensure the errors.Is evaluates to true for *all* of these
) {
	t.Helper()
	select {
	case <-itemDone:
		outcome, err := getFinalStateFunc()
		assert.Equal(t, expectedOutcome, outcome, "Unexpected QueueOutcome")
		if expectError {
			require.Error(t, err, "Expected an error but got nil")
			for _, expectedErr := range errorWraps {
				require.ErrorIs(t, err, expectedErr, "Expected error to wrap %v, but it did not", expectedErr)
			}
		} else {
			assert.NoError(t, err, "Expected no error but got one")
		}
	case <-time.After(timeout):
		t.Fatalf("Timeout waiting for item to be finalized (expected outcome: %s)", expectedOutcome.String())
	}
}

// newBasicManagedQueueAddImpl creates a default AddImpl for mockManagedQueue that captures the enqueued item.
// This is useful for tests that need to wait for an item to be enqueued before proceeding.
func newBasicManagedQueueAddImpl(
	t *testing.T,
	captureItemChan chan<- *flowItem, // Optional channel to send the captured *flowItem
) func(item types.QueueItemAccessor) (uint64, uint64, error) {
	return func(item types.QueueItemAccessor) (uint64, uint64, error) {
		t.Logf("basicManagedQueueAddImpl: Add called for item: %s", item.RequestID())
		fi, ok := item.(*flowItem) // FlowController creates flowItem
		require.True(t, ok, "Item added to ManagedQueue should be *flowItem, got %T", item)

		mockHandle := mocks.NewMockQueueItemHandle(fi.RequestID()) // Use item ID as raw handle
		fi.SetHandle(mockHandle)

		if captureItemChan != nil {
			captureItemChan <- fi
		}
		return 1, item.ByteSize(), nil
	}
}

// --- Test Cases ---

func TestFlowController_NewFlowController(t *testing.T) {
	t.Parallel()

	logger := logr.Discard()
	cfg := defaultTestFlowControllerConfig()
	mockSatDet := newMockSaturationDetector(false)
	mockReg := &mockFlowRegistry{}

	t.Run("ValidInitialization", func(t *testing.T) {
		t.Parallel()
		fc, err := NewFlowController(mockSatDet, mockReg, cfg, logger)
		require.NoError(t, err)
		require.NotNil(t, fc)
		assert.Equal(t, cfg.DefaultQueueTTL, fc.config.DefaultQueueTTL)
		assert.NotNil(t, fc.clock) // Should default to realClock
		assert.NotNil(t, fc.enqueueChan)
		assert.NotNil(t, fc.stopCh)
	})

	t.Run("NilSaturationDetector", func(t *testing.T) {
		t.Parallel()
		_, err := NewFlowController(nil, mockReg, cfg, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SaturationDetector cannot be nil")
	})

	t.Run("NilFlowRegistry", func(t *testing.T) {
		t.Parallel()
		_, err := NewFlowController(mockSatDet, nil, cfg, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "FlowRegistry cannot be nil")
	})

	t.Run("InvalidConfig_DefaultTTL", func(t *testing.T) {
		t.Parallel()
		invalidCfg := cfg
		invalidCfg.DefaultQueueTTL = 0 // Will be defaulted by validateAndApplyDefaults
		fc, err := NewFlowController(mockSatDet, mockReg, invalidCfg, logger)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultFCQueueTTL, fc.config.DefaultQueueTTL, "DefaultQueueTTL should be defaulted")
	})
}

// func TestFlowController_EnqueueAndWait_InputValidation(t *testing.T) {
// 	t.Parallel()

// 	t.Run("EnqueueAndWait_Reject_NilRequest", func(t *testing.T) {
// 		t.Parallel()
// 		cfg := defaultTestFlowControllerConfig()
// 		mockReg := &mockFlowRegistry{}
// 		rig, cleanup := setupTestRig(t, cfg, mockReg, newMockSaturationDetector(false))
// 		defer cleanup()

// 		outcome, err := rig.fc.EnqueueAndWait(nil)
// 		assert.Equal(t, types.QueueOutcomeRejectedOther, outcome)
// 		require.Error(t, err)
// 		assert.ErrorIs(t, err, types.ErrRejected)
// 		assert.ErrorIs(t, err, types.ErrNilRequest)
// 	})

// 	t.Run("EnqueueAndWait_Reject_EmptyFlowID", func(t *testing.T) {
// 		t.Parallel()
// 		cfg := defaultTestFlowControllerConfig()
// 		mockReg := &mockFlowRegistry{}
// 		// rig, cleanup := setupTestRig(t, cfg, mockReg, newMockSaturationDetector(false))

// 		outcome, err := rig.fc.EnqueueAndWait(newTestRequest(t, "req-1", "", 100, cfg.DefaultQueueTTL, nil))

// 		assert.Equal(t, types.QueueOutcomeRejectedOther, outcome)
// 		require.Error(t, err)
// 		assert.ErrorIs(t, err, types.ErrRejected)
// 		assert.ErrorIs(t, err, types.ErrFlowIDEmpty)
// 	})
// }

func TestFlowController_EnqueueAndWait_Lifecycle(t *testing.T) {
	t.Parallel()

	t.Run("EnqueueAndWait_Dispatch", func(t *testing.T) {
		t.Parallel()
		cfg := defaultTestFlowControllerConfig()

		// Setup Mocks
		flowID := "test-flow"
		priority := uint(0)
		reqID := "req-1"
		itemByteSize := uint64(100)

		// enqueuedItemPtr will hold the address of the flowItem once captured.
		// This allows other mock funcs to safely access the item after it's captured.
		var enqueuedItemPtr **flowItem
		captureItemChan := make(chan *flowItem)
		enqueuedItemPtrReady := make(chan struct{})

		mockReg := &mockFlowRegistry{}
		mockMQ := &mockManagedQueue{
			MockFlowSpecVal: mocks.NewMockFlowSpecification(flowID, priority),
			MockNameVal:     "mock-mq",
			AddImpl:         newBasicManagedQueueAddImpl(t, captureItemChan),
			RemoveImpl: func(handle types.QueueItemHandle) (types.QueueItemAccessor, uint64, uint64, error) {
				require.NotNil(t, enqueuedItemPtr, "enqueuedItemPtr should not be nil in RemoveImpl")
				item := *enqueuedItemPtr
				require.NotNil(t, item, "enqueuedItem should have been set by AddImpl")
				require.NotNil(t, item.Handle(), "enqueuedItem handle should not be nil before comparison")
				require.NotNil(t, handle, "passed handle to RemoveImpl should not be nil")
				assert.Equal(t, item.Handle().Handle(), handle.Handle(), "Handle mismatch in Remove")
				t.Logf("mockManagedQueue.Remove called for item: %s", item.RequestID())
				return item, 0, 0, nil
			},
		}

		mockReg.GetActiveManagedQueueFunc = func(fID string) (types.ManagedQueue, error) {
			require.Equal(t, flowID, fID)
			return mockMQ, nil
		}
		mockReg.GetPriorityBandAccessorFunc = func(prio uint) (types.PriorityBandAccessor, error) {
			require.Equal(t, priority, prio)
			return &mocks.MockPriorityBandAccessor{MockCapacityBytes: 200}, nil
		}
		mockReg.GetAllOrderedPriorityLevelsFunc = func() []uint { return []uint{priority} }
		mockReg.GetGetStatsFunc = func() types.FlowRegistryStats {
			return types.FlowRegistryStats{
				GlobalByteSize: 0,
				GlobalLen:      0,
				PerPriorityBandStats: map[uint]types.PriorityBandStats{
					priority: {ByteSize: 0, Len: 0},
				},
			}
		}
		mockFQA := mocks.NewMockFlowQueueAccessor(mockMQ.MockFlowSpecVal, "mock-fqa", nil, nil)
		mockReg.GetInterFlowDispatchPolicyFunc = func(prio uint) (types.InterFlowDispatchPolicy, error) {
			return &mockInterFlowDispatchPolicy{
				SelectQueueFunc: func(band types.PriorityBandAccessor) (types.FlowQueueAccessor, error) {
					return mockFQA, nil
				},
			}, nil
		}
		mockReg.GetIntraFlowDispatchPolicyFunc = func(fID string, prio uint) (types.IntraFlowDispatchPolicy, error) {
			return &mockIntraFlowDispatchPolicy{
				SelectItemFunc: func(queue types.FlowQueueAccessor) types.QueueItemAccessor {
					// Wait until the main test goroutine signals that enqueuedItemPtr is ready.
					select {
					case <-enqueuedItemPtrReady:
						// enqueuedItemPtr is now guaranteed to be set by the main test goroutine
						require.NotNil(t, enqueuedItemPtr, "enqueuedItemPtr should be non-nil after ready signal")
						item := *enqueuedItemPtr
						require.NotNil(t, item, "enqueuedItem should be available for SelectItem")
						t.Logf("mockIntraFlowDispatchPolicy.SelectItem returning item: %s", item.RequestID())
						return item
					case <-time.After(testAsyncProcessingWait):
						t.Errorf("Timeout waiting for enqueuedItemPtr to be ready in SelectItemFunc")
						// To avoid panicking the FC's goroutine, return nil, which will cause dispatch to fail for this cycle.
						return nil
					}
				},
			}, nil
		}
		mockReg.GetManagedQueueFunc = func(fID string, prio uint) (types.ManagedQueue, error) {
			return mockMQ, nil // Needed by dispatchItem to remove
		}

		rig, cleanup := setupTestRig(t, cfg, mockReg, newMockSaturationDetector(false))
		defer cleanup()

		// Create request
		req := newTestRequest(t, reqID, flowID, itemByteSize, cfg.DefaultQueueTTL, nil)

		// Call EnqueueAndWait in a goroutine as it blocks
		enqueueDone := make(chan struct{})
		go func() {
			defer close(enqueueDone)
			rig.fc.EnqueueAndWait(req)
		}()

		// Block here until AddImpl sends the item.
		// Once received, store its address in enqueuedItemPtr.
		itemFromChan := <-captureItemChan
		enqueuedItemPtr = &itemFromChan // Now enqueuedItemPtr points to the captured item.
		close(enqueuedItemPtrReady)     // Signal that enqueuedItemPtr is now set and ready for use.

		expectOutcome(t, (*enqueuedItemPtr).done, testAsyncProcessingWait, (*enqueuedItemPtr).getFinalState,
			types.QueueOutcomeDispatched, false, nil)
		require.NotNil(t, *enqueuedItemPtr, "enqueuedItem (dereferenced) should be set if dispatch occurred")
	})

	t.Run("EnqueueAndWait_Evicted_ContextCancelled", func(t *testing.T) {
		t.Skip()
	})

	t.Run("EnqueueAndWait_Evicted_TTL", func(t *testing.T) {
		t.Parallel()
		cfg := defaultTestFlowControllerConfig()
		cfg.ExpiryCleanupInterval = 20 * time.Millisecond
		itemTTL := 50 * time.Millisecond

		flowID := "ttl-flow"
		priority := uint(1)
		reqID := "req-ttl"
		itemByteSize := uint64(50)

		var enqueuedItemPtr **flowItem
		captureItemChan := make(chan *flowItem)
		enqueuedItemPtrReady := make(chan struct{})
		itemCleanedUpSignal := make(chan struct{})

		mockReg := &mockFlowRegistry{}
		mockMQ := &mockManagedQueue{
			MockFlowSpecVal: mocks.NewMockFlowSpecification(flowID, priority),
			MockNameVal:     "mock-ttl-mq",
			AddImpl:         newBasicManagedQueueAddImpl(t, captureItemChan),
			CleanupExpiredImpl: func(
				currentTime time.Time,
				isItemExpired types.IsItemExpiredFunc,
			) ([]types.ExpiredItemInfo, error) {
				t.Logf("mockManagedQueue.CleanupExpiredImpl called at mock time: %v", currentTime)
				var removed []types.ExpiredItemInfo
				// This mock assumes only one item is in it for simplicity.
				// A real queue would iterate its items.
				if enqueuedItemPtr != nil && *enqueuedItemPtr != nil {
					item := *enqueuedItemPtr
					// Use the FC's provided expiry check logic
					expired, outcome, err := isItemExpired(item, currentTime)
					if expired {
						t.Logf("mockManagedQueue.CleanupExpiredImpl: Item %s determined to be expired. Outcome: %s, Err: %v",
							item.RequestID(), outcome, err)
						removed = append(removed, types.ExpiredItemInfo{
							Item:    item,
							Outcome: outcome,
							Error:   err,
						})
						// Signal that cleanup has processed this item.
						// Do this before item.Handle().Invalidate() if the handle is checked by the test.
						close(itemCleanedUpSignal)
						if item.Handle() != nil {
							item.Handle().Invalidate()
						}
					}
				}
				return removed, nil
			},
		}

		mockReg.GetActiveManagedQueueFunc = func(fID string) (types.ManagedQueue, error) { return mockMQ, nil }
		mockReg.GetPriorityBandAccessorFunc = func(prio uint) (types.PriorityBandAccessor, error) {
			return &mocks.MockPriorityBandAccessor{MockCapacityBytes: 200, MockPriorityVal: prio}, nil
		}
		mockReg.GetAllOrderedPriorityLevelsFunc = func() []uint { return []uint{priority} }
		mockReg.GetGetStatsFunc = func() types.FlowRegistryStats { return types.FlowRegistryStats{} }
		// No dispatch policies needed as we expect TTL expiry.
		mockReg.GetInterFlowDispatchPolicyFunc = func(uint) (types.InterFlowDispatchPolicy, error) { return &mockInterFlowDispatchPolicy{}, nil }
		mockReg.GetIntraFlowDispatchPolicyFunc = func(string, uint) (types.IntraFlowDispatchPolicy, error) { return &mockIntraFlowDispatchPolicy{}, nil }
		mockReg.GetManagedQueueFunc = func(string, uint) (types.ManagedQueue, error) { return mockMQ, nil }

		rig, cleanup := setupTestRig(t, cfg, mockReg, newMockSaturationDetector(false))
		defer cleanup()

		req := newTestRequest(t, reqID, flowID, itemByteSize, itemTTL, nil)

		go func() { rig.fc.EnqueueAndWait(req) }()

		itemFromChan := <-captureItemChan
		enqueuedItemPtr = &itemFromChan
		close(enqueuedItemPtrReady) // Not strictly needed for SelectItem in this test, but good practice.

		// Advance time past TTL and cleanup interval
		rig.mockClock.currentTime = rig.mockClock.currentTime.Add(itemTTL + cfg.ExpiryCleanupInterval + (5 * time.Millisecond))
		t.Logf("Advanced mock clock to: %v", rig.mockClock.currentTime)

		expectOutcome(t, (*enqueuedItemPtr).done, testAsyncProcessingWait*3, (*enqueuedItemPtr).getFinalState,
			types.QueueOutcomeEvictedTTL, true, []error{types.ErrEvicted, types.ErrTTLExpired})
	})

	t.Run("EnqueueAndWait_Evicted_Preemption", func(t *testing.T) {
		t.Skip()
	})

	t.Run("EnqueueAndWait_Evicted_FlowControllerShutdown", func(t *testing.T) {
		t.Skip()
	})
}

// func TestFlowController_Run_Shutdown(t *testing.T) {
// 	t.Parallel()
// 	cfg := defaultTestFlowControllerConfig()
// 	mockReg := &mockFlowRegistry{}
// 	// Ensure registry is minimally mocked for shutdown processing, specifically AllOrderedPriorityLevels for
// 	// evictAllOnShutdown.
// 	mockReg.GetAllOrderedPriorityLevelsFunc = func() []uint { return []uint{} } // No bands, simplest case
// 	rig, cleanup := setupTestRig(t, cfg, mockReg, newMockSaturationDetector(false))

// 	// Call cleanup, which cancels the context for rig.fc.Run
// 	cleanup() // This will block until fcDone or timeout

// 	// Additional assertion: check if stopCh is closed (idempotent check)
// 	select {
// 	case <-rig.fc.stopCh:
// 		// Success, stopCh is closed
// 	default:
// 		t.Error("FlowController stopCh was not closed after shutdown")
// 	}

// 	// Needed assertions:
// 	// - evictAllOnShutdown was called
// 	// - fc.wg.Wait() is honored (ensuring runExpiryCleanup finishes).
// }
