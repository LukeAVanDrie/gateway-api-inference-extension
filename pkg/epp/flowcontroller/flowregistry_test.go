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
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/config"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/queue"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/testing/mocks"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"

	// Import default plugins to ensure they are registered for the tests.
	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/dispatch/interflow"
	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/dispatch/intraflow"
	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/preemption/interflow"
	_ "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/preemption/intraflow"
)

// --- Test Constants and Helpers ---

const (
	testPriorityCritical  uint = 0
	testPriorityStandard  uint = 1
	testPrioritySheddable uint = 2

	testFlowID1 = "test-flow-1"
	testFlowID2 = "test-flow-2"
	testFlowID3 = "test-flow-3"

	mockQueueNameForRegistryTests          = "MockQueueForRegistryTests"
	mockQueueNameForCapabilityMismatchTest = "MockQueueForCapabilityMismatchTest"
	failingQueueTypeForCreationFailureTest = "AlwaysFailingQueueTypeForTest"
	mockQueueItemHandleValuePrefix         = "mock-handle-"
)

var (
	logger = logr.Discard()
	// Standard test config with default ListQueues for all bands.
	defaultTestRegistryConfig = config.FlowRegistryConfig{
		PriorityBands: []config.PriorityBandConfig{
			{Priority: testPriorityCritical, PriorityName: "Critical"},
			{Priority: testPriorityStandard, PriorityName: "Standard"},
			{Priority: testPrioritySheddable, PriorityName: "Sheddable"},
		},
	}
	// Config using mock queues for specific tests.
	mockQueueTestRegistryConfig = config.FlowRegistryConfig{
		PriorityBands: []config.PriorityBandConfig{
			{Priority: testPriorityCritical, PriorityName: "Critical-MockQ", QueueType: mockQueueNameForRegistryTests},
			{Priority: testPriorityStandard, PriorityName: "Standard-MockQ", QueueType: mockQueueNameForRegistryTests},
			{Priority: testPrioritySheddable, PriorityName: "Sheddable-MockQ", QueueType: mockQueueNameForRegistryTests},
		},
	}
)

func init() {
	// Register a versatile mock queue.
	queue.RegisterQueue(mockQueueNameForRegistryTests,
		func(_ types.ItemComparator) (types.SafeQueue, error) {
			return newMockSafeQueue(mockQueueNameForRegistryTests,
				[]types.QueueCapability{
					types.CapabilityFIFO,
					types.CapabilityDoubleEnded,
					types.CapabilityPriorityConfigurable, // Assume it can take a comparator
				},
			), nil
		})
	// Register a mock queue with no capabilities for mismatch tests.
	queue.RegisterQueue(mockQueueNameForCapabilityMismatchTest,
		func(_ types.ItemComparator) (types.SafeQueue, error) {
			return newMockSafeQueue(mockQueueNameForCapabilityMismatchTest, nil), nil
		})
	// Register a queue type that always fails creation.
	queue.RegisterQueue(failingQueueTypeForCreationFailureTest,
		func(_ types.ItemComparator) (types.SafeQueue, error) {
			return nil, fmt.Errorf("queue factory deliberate failure for %s", failingQueueTypeForCreationFailureTest)
		})
}

// mockSafeQueue is a more functional mock for types.SafeQueue.
type mockSafeQueue struct {
	nameVal             string
	capabilitiesVal     []types.QueueCapability
	items               map[string]types.QueueItemAccessor // item handle string -> item
	itemOrder           []string                           // Simulates order for PeekHead/Tail
	mu                  sync.Mutex
	lenVal              int
	byteSizeVal         uint64
	comparator          types.ItemComparator // Store if configured
	cleanupExpiredError error
}

var _ types.SafeQueue = &mockSafeQueue{} // Compile-time validation

func newMockSafeQueue(name string, caps []types.QueueCapability) *mockSafeQueue {
	return &mockSafeQueue{
		nameVal:         name,
		capabilitiesVal: caps,
		items:           make(map[string]types.QueueItemAccessor),
	}
}

func (mq *mockSafeQueue) Len() int {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	return mq.lenVal
}

func (mq *mockSafeQueue) ByteSize() uint64 {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	return mq.byteSizeVal
}

func (mq *mockSafeQueue) Name() string                          { return mq.nameVal }
func (mq *mockSafeQueue) Capabilities() []types.QueueCapability { return mq.capabilitiesVal }

func (mq *mockSafeQueue) PeekHead() (types.QueueItemAccessor, error) {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	if len(mq.itemOrder) == 0 {
		return nil, types.ErrQueueEmpty
	}
	item, ok := mq.items[mq.itemOrder[0]]
	if !ok { // Should not happen if itemOrder is consistent
		return nil, types.ErrQueueItemNotFound
	}
	return item, nil
}

func (mq *mockSafeQueue) PeekTail() (types.QueueItemAccessor, error) {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	if len(mq.itemOrder) == 0 {
		return nil, types.ErrQueueEmpty
	}
	if !mq.hasCapability(types.CapabilityDoubleEnded) {
		return nil, types.ErrOperationNotSupported
	}
	item, ok := mq.items[mq.itemOrder[len(mq.itemOrder)-1]]
	if !ok { // Should not happen if itemOrder is consistent
		return nil, types.ErrQueueItemNotFound
	}
	return item, nil
}

func (mq *mockSafeQueue) Add(item types.QueueItemAccessor) (uint64, uint64, error) {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	if item == nil {
		return uint64(mq.lenVal), mq.byteSizeVal, types.ErrNilQueueItem
	}

	handleValue := fmt.Sprintf("%s%s-%d", mockQueueItemHandleValuePrefix, item.RequestID(), len(mq.items))
	handle := mocks.NewMockQueueItemHandle(handleValue)
	item.SetHandle(handle)

	mq.items[handleValue] = item
	mq.itemOrder = append(mq.itemOrder, handleValue) // Add to end for FIFO behavior
	mq.lenVal++
	mq.byteSizeVal += item.ByteSize()

	// If priority configurable, re-sort (simple sort for mock)
	if mq.comparator != nil && mq.hasCapability(types.CapabilityPriorityConfigurable) {
		sort.SliceStable(mq.itemOrder, func(i, j int) bool {
			itemI := mq.items[mq.itemOrder[i]]
			itemJ := mq.items[mq.itemOrder[j]]
			return mq.comparator.Func()(itemI, itemJ) // true if itemI is higher priority
		})
	}
	return uint64(mq.lenVal), mq.byteSizeVal, nil
}

func (mq *mockSafeQueue) Remove(handle types.QueueItemHandle) (types.QueueItemAccessor, uint64, uint64, error) {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	if handle == nil || handle.Handle() == nil {
		return nil, uint64(mq.lenVal), mq.byteSizeVal, types.ErrInvalidQueueItemHandle
	}
	handleStr, ok := handle.Handle().(string)
	if !ok {
		return nil, uint64(mq.lenVal), mq.byteSizeVal, types.ErrInvalidQueueItemHandle
	}

	item, exists := mq.items[handleStr]
	if !exists || handle.IsInvalidated() { // Check if already invalidated
		return nil, uint64(mq.lenVal), mq.byteSizeVal, types.ErrQueueItemNotFound
	}

	delete(mq.items, handleStr)
	newOrder := make([]string, 0, len(mq.itemOrder)-1) // Remove from itemOrder
	for _, h := range mq.itemOrder {
		if h != handleStr {
			newOrder = append(newOrder, h)
		}
	}
	mq.itemOrder = newOrder

	mq.lenVal--
	mq.byteSizeVal -= item.ByteSize()
	handle.Invalidate() // Mark handle as invalid after removal

	return item, uint64(mq.lenVal), mq.byteSizeVal, nil
}

func (mq *mockSafeQueue) CleanupExpired(
	currentTime time.Time,
	isItemExpired types.IsItemExpiredFunc,
) ([]types.ExpiredItemInfo, error) {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	if mq.cleanupExpiredError != nil {
		return nil, mq.cleanupExpiredError
	}

	var removedInfos []types.ExpiredItemInfo
	newOrder := make([]string, 0, len(mq.itemOrder))
	itemsActuallyRemoved := make(map[string]bool) // Track by handle string

	for _, handleStr := range mq.itemOrder {
		item, exists := mq.items[handleStr]
		if !exists {
			// Should not happen if itemOrder and items are consistent
			continue
		}

		expired, outcome, err := isItemExpired(item, currentTime)
		if expired {
			removedInfos = append(removedInfos, types.ExpiredItemInfo{
				Item:    item,
				Outcome: outcome,
				Error:   err,
			})
			itemsActuallyRemoved[handleStr] = true
			item.Handle().Invalidate() // Invalidate the handle of the expired item
			mq.lenVal--
			mq.byteSizeVal -= item.ByteSize()
			delete(mq.items, handleStr) // Remove from main map
		} else {
			newOrder = append(newOrder, handleStr) // Keep in order
		}
	}
	mq.itemOrder = newOrder
	return removedInfos, nil
}

func (mq *mockSafeQueue) hasCapability(cap types.QueueCapability) bool {
	for _, c := range mq.capabilitiesVal {
		if c == cap {
			return true
		}
	}
	return false
}

func (mq *mockSafeQueue) setCleanupExpiredError(err error) {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	mq.cleanupExpiredError = err
}

// Helper to create a FlowRegistry with a specific config for a test.
func newTestFlowRegistry(t *testing.T, cfg config.FlowRegistryConfig) *FlowRegistry {
	t.Helper()
	fr, err := NewFlowRegistry(cfg, logger)
	require.NoError(t, err, "NewFlowRegistry should not fail with valid test config")
	require.NotNil(t, fr)
	return fr
}

// Helper to assert basic flow instance properties.
func assertFlowInstance(
	t *testing.T,
	fr *FlowRegistry,
	flowID string,
	priority uint,
	expectedActive,
	expectedRegistered bool,
) *flowInstance {
	t.Helper()
	fr.mu.RLock() // RLock for inspection
	defer fr.mu.RUnlock()

	priorityMap, ok := fr.allFlowInstances[flowID]
	require.True(t, ok, "FlowID %s not found in allFlowInstances", flowID)
	instance, ok := priorityMap[priority]
	require.True(t, ok, "FlowID %s at priority %d not found in its priorityMap", flowID, priority)
	require.NotNil(t, instance)

	instance.instanceMu.Lock()
	defer instance.instanceMu.Unlock()
	assert.Equal(t, expectedActive, instance.isActive,
		"Instance isActive mismatch for flow %s, priority %d", flowID, priority)
	assert.Equal(t, expectedRegistered, instance.isRegistered,
		"Instance isRegistered mismatch for flow %s, priority %d", flowID, priority)
	assert.Equal(t, flowID, instance.id)
	assert.Equal(t, priority, instance.priority)
	require.NotNil(t, instance.queue, "Instance queue should not be nil")
	require.NotNil(t, instance.intraFlowDispatchPolicy, "Instance intra-dispatch policy should not be nil")
	require.NotNil(t, instance.intraFlowPreemptionPolicy, "Instance intra-preemption policy should not be nil")

	if expectedActive {
		activeInstance, activeOK := fr.activeFlowInstances[flowID]
		require.True(t, activeOK, "FlowID %s expected to be active but not in activeFlowInstances", flowID)
		assert.Same(t, instance, activeInstance, "Active instance in map does not match instance")
	} else {
		_, activeOK := fr.activeFlowInstances[flowID]
		assert.False(t, activeOK, "FlowID %s expected to be inactive but found in activeFlowInstances", flowID)
	}
	return instance
}

// assertBandStats checks stats for a specific band.
func assertBandStats(t *testing.T, fr *FlowRegistry, priority uint, expectedLen, expectedSize uint64) {
	t.Helper()
	stats := fr.GetStats()
	bandStats, ok := stats.PerPriorityBandStats[priority]
	require.True(t, ok, "Stats for priority band %d not found", priority)
	assert.Equal(t, expectedLen, bandStats.Len, "Band %d item count mismatch", priority)
	assert.Equal(t, expectedSize, bandStats.ByteSize, "Band %d byte size mismatch", priority)
}

// assertGlobalStats checks global stats.
func assertGlobalStats(t *testing.T, fr *FlowRegistry, expectedLen, expectedSize uint64) {
	t.Helper()
	stats := fr.GetStats()
	assert.Equal(t, expectedLen, stats.GlobalLen, "Global item count mismatch")
	assert.Equal(t, expectedSize, stats.GlobalByteSize, "Global byte size mismatch")
}

// --- Test Cases ---

func TestFlowRegistry_NewFlowRegistry(t *testing.T) {
	t.Parallel()
	t.Run("ValidConfig_DefaultPolicies", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
		assert.Len(t, fr.priorityBands, 3)
		for _, priority := range []uint{testPriorityCritical, testPriorityStandard, testPrioritySheddable} {
			band, ok := fr.priorityBands[priority]
			require.True(t, ok)
			assert.NotNil(t, band.interFlowDispatchPolicy, "Priority %d inter-dispatch policy", priority)
			assert.NotNil(t, band.interFlowPreemptionPolicy, "Priority %d inter-preemption policy", priority)
			assert.NotNil(t, band.accessor, "Priority %d accessor", priority)
			assert.Equal(t, priority, band.priorityLevel)
			assert.NotEmpty(t, band.config.PriorityName)
			// Default policies are set by config.PriorityBandConfig.validateAndApplyDefaults.
			assert.NotEmpty(t, band.config.IntraFlowDispatchPolicy, "Priority %d intra-dispatch policy name", priority)
			assert.NotEmpty(t, band.config.IntraFlowPreemptionPolicy, "Priority %d intra-preemption policy name", priority)
			assert.NotEmpty(t, band.config.QueueType, "Priority %d queue type", priority)
		}
		assertGlobalStats(t, fr, 0, 0)
	})

	t.Run("EmptyConfig_NoBands", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, config.FlowRegistryConfig{PriorityBands: []config.PriorityBandConfig{}})
		assert.Empty(t, fr.priorityBands)
		assert.Empty(t, fr.AllOrderedPriorityLevels())
		assertGlobalStats(t, fr, 0, 0)
	})

	t.Run("Error_DuplicatePriorityBandLevels", func(t *testing.T) {
		t.Parallel()
		cfg := config.FlowRegistryConfig{
			PriorityBands: []config.PriorityBandConfig{
				{Priority: testPriorityStandard, PriorityName: "Std1"},
				{Priority: testPriorityStandard, PriorityName: "Std2Dup"},
			},
		}
		_, err := NewFlowRegistry(cfg, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate priority band level")
	})

	t.Run("Error_Invalidconfig.PriorityBandConfig_MissingName", func(t *testing.T) {
		t.Parallel()
		cfg := config.FlowRegistryConfig{PriorityBands: []config.PriorityBandConfig{{
			Priority:     testPriorityStandard,
			PriorityName: "",
		}}}
		_, err := NewFlowRegistry(cfg, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PriorityName cannot be empty")
	})

	t.Run("Error_PolicyCreationFailure", func(t *testing.T) {
		t.Parallel()
		bandCfg := config.PriorityBandConfig{
			Priority:                testPriorityStandard,
			PriorityName:            "Std",
			InterFlowDispatchPolicy: "NonExistentPolicy",
		}
		cfg := config.FlowRegistryConfig{PriorityBands: []config.PriorityBandConfig{bandCfg}}
		_, err := NewFlowRegistry(cfg, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create inter-flow dispatch policy 'NonExistentPolicy'")
	})
}

func TestFlowRegistry_RegisterOrUpdateFlow(t *testing.T) {
	t.Parallel()

	t.Run("RegisterNewFlow_Success", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
		spec := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
		err := fr.RegisterOrUpdateFlow(spec)
		require.NoError(t, err)

		assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)
		assertGlobalStats(t, fr, 0, 0) // Queue is empty
		assertBandStats(t, fr, testPriorityStandard, 0, 0)
	})

	t.Run("UpdateFlow_SamePriority_UpdatesSpec", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
		spec1 := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
		_ = fr.RegisterOrUpdateFlow(spec1)
		instance1 := assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)

		spec2 := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard) // Same ID, same priority
		err := fr.RegisterOrUpdateFlow(spec2)
		require.NoError(t, err)

		instance2 := assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)
		assert.Same(t, instance1, instance2, "Instance pointer should be the same")
		assert.Equal(t, spec2, instance2.spec, "Instance spec should be updated")
	})

	t.Run("UpdateFlow_DifferentPriority_MigratesAndDrains", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig) // Use mock queues
		specOld := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
		_ = fr.RegisterOrUpdateFlow(specOld)
		oldInstance := assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)

		// Add an item to the old queue to ensure it drains
		item := mocks.NewMockQueueItemAccessor("req1", testFlowID1, 100, time.Now())
		_, _, err := oldInstance.queue.Add(item)
		require.NoError(t, err)
		assertGlobalStats(t, fr, 1, 100)
		assertBandStats(t, fr, testPriorityStandard, 1, 100)

		specNew := mocks.NewMockFlowSpecification(testFlowID1, testPriorityCritical)
		err = fr.RegisterOrUpdateFlow(specNew)
		require.NoError(t, err)

		// Old instance should be inactive but registered (draining).
		assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, false, true)
		assert.Equal(t, 1, oldInstance.queue.Len(), "Old queue should retain its item for draining")

		// New instance should be active and registered at the new priority.
		newInstance := assertFlowInstance(t, fr, testFlowID1, testPriorityCritical, true, true)
		assert.Equal(t, 0, newInstance.queue.Len(), "New queue should be empty")
		assert.NotSame(t, oldInstance.queue, newInstance.queue, "Queues should be different instances")

		// Stats should reflect the item still in the old queue, and new queue being empty.
		assertGlobalStats(t, fr, 1, 100)
		assertBandStats(t, fr, testPriorityStandard, 1, 100)
		assertBandStats(t, fr, testPriorityCritical, 0, 0)

		// Simulate old queue becoming empty.
		_, _, _, err = oldInstance.queue.Remove(item.Handle())
		require.NoError(t, err)
		// managedQueueWrapper should signal FlowRegistry, leading to cleanup.
		// Check if old instance is cleaned up.
		fr.mu.RLock()
		_, stillExistsInAll := fr.allFlowInstances[testFlowID1][testPriorityStandard]
		fr.mu.RUnlock()
		assert.False(t, stillExistsInAll, "Old instance should be cleaned up after its queue empties")
		assertGlobalStats(t, fr, 0, 0) // All items gone
	})

	t.Run("UpdateFlow_ReactivateExistingInstance", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		specStd := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
		_ = fr.RegisterOrUpdateFlow(specStd)
		instanceStd := assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)
		item := mocks.NewMockQueueItemAccessor("req1", testFlowID1, 50, time.Now())
		_, _, _ = instanceStd.queue.Add(item) // Add item to standard queue

		specCrit := mocks.NewMockFlowSpecification(testFlowID1, testPriorityCritical)
		_ = fr.RegisterOrUpdateFlow(specCrit) // Migrate to critical

		specStdAgain := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
		err := fr.RegisterOrUpdateFlow(specStdAgain) // Migrate back to standard
		require.NoError(t, err)

		currentActive := assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)
		assert.Same(t, instanceStd, currentActive, "Should reactivate the original Standard instance")
		assert.Equal(t, 1, currentActive.queue.Len(), "Reactivated queue should retain its item")
		assert.Equal(t, specStdAgain, currentActive.spec, "Spec should be updated on reactivated instance")
	})

	t.Run("Error_EmptyFlowID", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
		err := fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification("", testPriorityStandard))
		assert.ErrorIs(t, err, types.ErrFlowIDEmpty)
	})

	t.Run("Error_InvalidPriority", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
		err := fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification(testFlowID1, 999))
		assert.ErrorIs(t, err, types.ErrInvalidFlowPriority)
	})

	t.Run("Error_QueueCreationFailure", func(t *testing.T) {
		t.Parallel()
		cfg := config.FlowRegistryConfig{PriorityBands: []config.PriorityBandConfig{{
			Priority:     testPriorityStandard,
			PriorityName: "Std-Fail",
			QueueType:    failingQueueTypeForCreationFailureTest,
		}}}
		fr := newTestFlowRegistry(t, cfg)
		err := fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create queue")
		assert.Contains(t, err.Error(), failingQueueTypeForCreationFailureTest)
	})

	t.Run("Error_PolicyCreationFailure", func(t *testing.T) {
		t.Parallel()
		cfg := config.FlowRegistryConfig{PriorityBands: []config.PriorityBandConfig{{
			Priority:                testPriorityStandard,
			PriorityName:            "Std-PolicyFail",
			IntraFlowDispatchPolicy: "NonExistentPolicy",
		}}}
		fr := newTestFlowRegistry(t, cfg)
		err := fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create intra-flow dispatch policy")
	})

	t.Run("Error_QueueCapabilityMismatch", func(t *testing.T) {
		t.Parallel()
		// Default FCFS dispatch policy requires CapabilityFIFO.
		// mockQueueNameForCapabilityMismatchTest provides no capabilities.
		cfg := config.FlowRegistryConfig{PriorityBands: []config.PriorityBandConfig{{
			Priority:     testPriorityStandard,
			PriorityName: "Std-CapMismatch",
			QueueType:    mockQueueNameForCapabilityMismatchTest,
		}}}
		fr := newTestFlowRegistry(t, cfg)
		err := fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "queue 'MockQueueForCapabilityMismatchTest' is missing capabilities")
		assert.Contains(t, err.Error(), string(types.CapabilityFIFO)) // Check that FIFO is mentioned as missing
	})
}

func TestFlowRegistry_UnregisterFlow(t *testing.T) {
	t.Parallel()

	t.Run("UnregisterActiveFlow_NotEmpty_BecomesInactiveUnregistered", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		spec := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
		_ = fr.RegisterOrUpdateFlow(spec)
		instance := assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)
		item := mocks.NewMockQueueItemAccessor("req1", testFlowID1, 10, time.Now())
		_, _, _ = instance.queue.Add(item) // Make queue non-empty

		err := fr.UnregisterFlow(testFlowID1)
		require.NoError(t, err)

		assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, false, false) // Now inactive and unregistered
		assert.Equal(t, 1, instance.queue.Len(), "Queue should retain item for draining")
		assertGlobalStats(t, fr, 1, 10) // Item still contributes to stats
	})

	t.Run("UnregisterActiveFlow_Empty_CleansUpImmediately", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, defaultTestRegistryConfig) // Uses ListQueue which is empty
		spec := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
		_ = fr.RegisterOrUpdateFlow(spec)
		assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)

		err := fr.UnregisterFlow(testFlowID1)
		require.NoError(t, err)

		fr.mu.RLock()
		_, ok := fr.allFlowInstances[testFlowID1]
		fr.mu.RUnlock()
		assert.False(t, ok, "Flow should be completely removed from allFlowInstances")
		assertGlobalStats(t, fr, 0, 0)
	})

	t.Run("UnregisterInactiveDrainingFlow_NotEmpty_MarksUnregistered", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		specStd := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
		_ = fr.RegisterOrUpdateFlow(specStd)
		instanceStd := assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, true, true)
		item := mocks.NewMockQueueItemAccessor("req1", testFlowID1, 10, time.Now())
		_, _, _ = instanceStd.queue.Add(item)

		_ = fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification(testFlowID1, testPriorityCritical)) // Migrate

		assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, false, true) // Now inactive, registered.

		err := fr.UnregisterFlow(testFlowID1)
		require.NoError(t, err)

		assertFlowInstance(t, fr, testFlowID1, testPriorityStandard, false, false) // Still inactive, now unregistered
		assert.Equal(t, 1, instanceStd.queue.Len(), "Queue should retain item")
	})

	t.Run("Error_NonExistentFlow", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
		err := fr.UnregisterFlow("non-existent")
		assert.ErrorIs(t, err, types.ErrFlowNotRegistered)
	})

	t.Run("Error_EmptyFlowID", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
		err := fr.UnregisterFlow("")
		assert.ErrorIs(t, err, types.ErrFlowIDEmpty)
	})
}

func TestFlowRegistry_Accessors(t *testing.T) {
	t.Parallel()
	fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
	specStd := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
	_ = fr.RegisterOrUpdateFlow(specStd)
	instanceStd := fr.activeFlowInstances[testFlowID1]

	specCrit := mocks.NewMockFlowSpecification(testFlowID2, testPriorityCritical)
	_ = fr.RegisterOrUpdateFlow(specCrit)

	t.Run("ActiveManagedQueue", func(t *testing.T) {
		t.Parallel()
		mq, err := fr.ActiveManagedQueue(testFlowID1)
		require.NoError(t, err)
		assert.Same(t, instanceStd.queue, mq)
		_, err = fr.ActiveManagedQueue("non-existent")
		assert.ErrorIs(t, err, types.ErrFlowNotRegistered)
	})

	t.Run("ManagedQueue", func(t *testing.T) {
		t.Parallel()
		mq, err := fr.ManagedQueue(testFlowID1, testPriorityStandard)
		require.NoError(t, err)
		assert.Same(t, instanceStd.queue, mq)
		_, err = fr.ManagedQueue(testFlowID1, testPriorityCritical) // testFlowID1 not at critical
		assert.ErrorIs(t, err, types.ErrFlowInstanceNotFound)
	})

	// Test policy accessors.
	for _, policyType := range []string{"IntraDispatch", "IntraPreemption", "InterDispatch", "InterPreemption"} {
		policyType := policyType // Capture range variable
		t.Run(policyType+"PolicyAccess", func(t *testing.T) {
			t.Parallel()
			var err error
			var policy any
			switch policyType {
			case "IntraDispatch":
				policy, err = fr.IntraFlowDispatchPolicy(testFlowID1, testPriorityStandard)
			case "IntraPreemption":
				policy, err = fr.IntraFlowPreemptionPolicy(testFlowID1, testPriorityStandard)
			case "InterDispatch":
				policy, err = fr.InterFlowDispatchPolicy(testPriorityStandard)
			case "InterPreemption":
				policy, err = fr.InterFlowPreemptionPolicy(testPriorityStandard)
			}
			require.NoError(t, err)
			assert.NotNil(t, policy)

			// Test error cases.
			switch policyType {
			case "IntraDispatch":
				_, err = fr.IntraFlowDispatchPolicy("non-existent", testPriorityStandard)
			case "IntraPreemption":
				_, err = fr.IntraFlowPreemptionPolicy("non-existent", testPriorityStandard)
			case "InterDispatch":
				_, err = fr.InterFlowDispatchPolicy(999)
			case "InterPreemption":
				_, err = fr.InterFlowPreemptionPolicy(999)
			}
			if policyType == "InterDispatch" || policyType == "InterPreemption" {
				assert.ErrorIs(t, err, types.ErrPriorityBandNotFound)
			} else {
				assert.ErrorIs(t, err, types.ErrFlowInstanceNotFound)
			}
		})
	}

	t.Run("PriorityBandAccessor", func(t *testing.T) {
		t.Parallel()
		acc, err := fr.PriorityBandAccessor(testPriorityStandard)
		require.NoError(t, err)
		assert.NotNil(t, acc)
		assert.Equal(t, testPriorityStandard, acc.Priority())
		_, err = fr.PriorityBandAccessor(999)
		assert.ErrorIs(t, err, types.ErrPriorityBandNotFound)
	})

	t.Run("AllOrderedPrioritys", func(t *testing.T) {
		t.Parallel()
		levels := fr.AllOrderedPriorityLevels()
		expected := []uint{testPriorityCritical, testPriorityStandard, testPrioritySheddable}
		assert.Equal(t, expected, levels)
	})

	t.Run("GetStats", func(t *testing.T) {
		t.Parallel()
		stats := fr.GetStats()
		assertGlobalStats(t, fr, 0, 0) // Initially empty
		assert.Len(t, stats.PerPriorityBandStats, 3)
	})
}

func TestFlowRegistry_Panics(t *testing.T) {
	t.Parallel()
	fr := newTestFlowRegistry(t, defaultTestRegistryConfig)
	spec := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
	_ = fr.RegisterOrUpdateFlow(spec)

	t.Run("ActiveManagedQueue_InvariantViolation", func(t *testing.T) {
		t.Parallel()
		// Corrupt state: make active instance inactive internally
		fr.mu.Lock()
		instance := fr.activeFlowInstances[testFlowID1]
		instance.instanceMu.Lock()
		instance.isActive = false // Inconsistency
		instance.instanceMu.Unlock()
		fr.mu.Unlock()

		assert.Panics(t, func() { _, _ = fr.ActiveManagedQueue(testFlowID1) })
	})

	t.Run("ManagedQueue_NilQueueInvariant", func(t *testing.T) {
		t.Parallel()
		fr.mu.Lock()
		instance := fr.allFlowInstances[testFlowID1][testPriorityStandard]
		instance.queue = nil // Corrupt
		fr.mu.Unlock()

		assert.Panics(t, func() { _, _ = fr.ManagedQueue(testFlowID1, testPriorityStandard) })
	})
}

func TestManagedQueueWrapper_Operations(t *testing.T) {
	t.Parallel()
	spec := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
	item1 := mocks.NewMockQueueItemAccessor("req1", testFlowID1, 100, time.Now())
	item2 := mocks.NewMockQueueItemAccessor("req2", testFlowID1, 50, time.Now())

	t.Run("Add_Success", func(t *testing.T) {
		t.Parallel()
		// Need a fresh registry and queue for parallel stat testing.
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		_ = fr.RegisterOrUpdateFlow(spec)
		mq, _ := fr.ActiveManagedQueue(testFlowID1)

		newLen, newSize, err := mq.Add(item1)
		require.NoError(t, err)
		assert.Equal(t, uint64(1), newLen)
		assert.Equal(t, uint64(100), newSize)
		assertGlobalStats(t, fr, 1, 100)
		assertBandStats(t, fr, testPriorityStandard, 1, 100)
		assert.NotNil(t, item1.Handle(), "Item handle should be set by mockSafeQueue.Add via ManagedQueue")
	})

	t.Run("Add_ToNonExistentInstance_Fails", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		_ = fr.RegisterOrUpdateFlow(spec)
		mq, _ := fr.ActiveManagedQueue(testFlowID1)
		_ = fr.UnregisterFlow(testFlowID1) // Make instance "non-existent" for new operations

		_, _, err := mq.Add(mocks.NewMockQueueItemAccessor("req-fail", testFlowID1, 10, time.Now()))
		assert.ErrorIs(t, err, types.ErrFlowInstanceNotFound)
	})

	t.Run("Remove_Success", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		_ = fr.RegisterOrUpdateFlow(spec)
		mqLocal, _ := fr.ActiveManagedQueue(testFlowID1)

		_, _, _ = mqLocal.Add(item1)
		_, _, _ = mqLocal.Add(item2) // Global: 2, 150. Band: 2, 150

		removedItem, newLen, newSize, err := mqLocal.Remove(item1.Handle())
		require.NoError(t, err)
		assert.Same(t, item1, removedItem)
		assert.Equal(t, uint64(1), newLen)
		assert.Equal(t, uint64(50), newSize) // item2 (50) remains
		assertGlobalStats(t, fr, 1, 50)
		assertBandStats(t, fr, testPriorityStandard, 1, 50)
		assert.True(t, item1.Handle().IsInvalidated(), "Removed item's handle should be invalidated")

		// Test remove last item, triggers signalQueueEmptied.
		_, _, _, err = mqLocal.Remove(item2.Handle())
		require.NoError(t, err)
		assertGlobalStats(t, fr, 0, 0)
		// Instance should be cleaned up if it was inactive/unregistered (not the case here).
		// For an active, registered instance, it remains.
		_, instanceStillExists := fr.allFlowInstances[testFlowID1]
		assert.True(t, instanceStillExists, "Active, registered instance should not be cleaned up by signalQueueEmptied")
	})

	t.Run("Remove_InvalidHandle_Fails", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		_ = fr.RegisterOrUpdateFlow(spec)
		mqLocal, _ := fr.ActiveManagedQueue(testFlowID1)
		_, _, _ = mqLocal.Add(item1)

		invalidHandle := mocks.NewMockQueueItemHandle("invalid-raw-handle")
		_, _, _, err := mqLocal.Remove(invalidHandle)
		assert.ErrorIs(t, err, types.ErrQueueItemNotFound) // Mock returns NotFound for unrecognised handles
	})

	t.Run("Remove_FromNonExistentInstance_Fails", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		_ = fr.RegisterOrUpdateFlow(spec)
		mq, _ := fr.ActiveManagedQueue(testFlowID1)
		// Add an item so we have a valid handle, then unregister.
		tempItem := mocks.NewMockQueueItemAccessor("temp", testFlowID1, 10, time.Now())
		_, _, _ = mq.Add(tempItem)
		_ = fr.UnregisterFlow(testFlowID1) // Make instance "non-existent" for new operations

		_, _, _, err := mq.Remove(tempItem.Handle())
		assert.ErrorIs(t, err, types.ErrFlowInstanceNotFound)
	})

	t.Run("CleanupExpired_Success", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		_ = fr.RegisterOrUpdateFlow(spec) // testFlowID1, testPriorityStandard
		mq, _ := fr.ActiveManagedQueue(testFlowID1)

		itemToExpire := mocks.NewMockQueueItemAccessor("reqExpire", testFlowID1, 70, time.Now().Add(-time.Hour)) // TTL
		itemToKeep := mocks.NewMockQueueItemAccessor("reqKeep", testFlowID1, 30, time.Now())

		_, _, _ = mq.Add(itemToExpire)
		_, _, _ = mq.Add(itemToKeep)
		assertGlobalStats(t, fr, 2, 100)
		assertBandStats(t, fr, testPriorityStandard, 2, 100)

		// Define an isItemExpiredFunc for the test.
		testIsItemExpired := func(item types.QueueItemAccessor, currentTime time.Time) (bool, types.QueueOutcome, error) {
			if item.RequestID() == "reqExpire" {
				return true, types.QueueOutcomeEvictedTTL, types.ErrTTLExpired
			}
			return false, types.QueueOutcomeDispatched, nil
		}

		removedInfos, err := mq.CleanupExpired(time.Now(), testIsItemExpired)
		require.NoError(t, err)

		require.Len(t, removedInfos, 1, "Expected one item to be removed by CleanupExpired")
		assert.Same(t, itemToExpire, removedInfos[0].Item)
		assert.Equal(t, types.QueueOutcomeEvictedTTL, removedInfos[0].Outcome)
		assert.ErrorIs(t, removedInfos[0].Error, types.ErrTTLExpired)

		assert.True(t, itemToExpire.Handle().IsInvalidated(), "Expired item's handle should be invalidated")
		assert.False(t, itemToKeep.Handle().IsInvalidated(), "Kept item's handle should not be invalidated")

		assert.Equal(t, 1, mq.Len(), "ManagedQueue length after CleanupExpired")
		assert.Equal(t, uint64(30), mq.ByteSize(), "ManagedQueue byte size after CleanupExpired")

		assertGlobalStats(t, fr, 1, 30)
		assertBandStats(t, fr, testPriorityStandard, 1, 30)

		// Test cleanup that empties the queue (if flow was unregistered).
		_ = fr.UnregisterFlow(testFlowID1)          // Mark for cleanup
		_, _, _, _ = mq.Remove(itemToKeep.Handle()) // This will make the queue empty

		fr.mu.RLock()
		_, stillExists := fr.allFlowInstances[testFlowID1]
		fr.mu.RUnlock()
		assert.False(t, stillExists, "Flow instance should be cleaned up after queue empties and flow is unregistered")
	})

	t.Run("CleanupExpired_OnNonExistentInstance_Fails", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		_ = fr.RegisterOrUpdateFlow(spec)
		mq, _ := fr.ActiveManagedQueue(testFlowID1)
		_ = fr.UnregisterFlow(testFlowID1) // Make instance "non-existent" for new operations

		_, err := mq.CleanupExpired(time.Now(), func(item types.QueueItemAccessor, currentTime time.Time) (bool, types.QueueOutcome, error) {
			return false, types.QueueOutcomeDispatched, nil
		})
		assert.ErrorIs(t, err, types.ErrFlowInstanceNotFound)
	})

	t.Run("CleanupExpired_SafeQueueReturnsError", func(t *testing.T) {
		t.Parallel()
		fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
		_ = fr.RegisterOrUpdateFlow(spec)
		mq, _ := fr.ActiveManagedQueue(testFlowID1)

		// Add an item so the queue is not empty.
		_, _, _ = mq.Add(mocks.NewMockQueueItemAccessor("item-err", testFlowID1, 10, time.Now()))
		initialGlobalLen, initialGlobalSize := fr.globalLen.Load(), fr.globalByteSize.Load()
		initialBandLen, initialBandSize := fr.priorityBands[testPriorityStandard].bandLen.Load(), fr.priorityBands[testPriorityStandard].bandByteSize.Load()

		// Configure the underlying mockSafeQueue to return an error.
		underlyingSafeQ, ok := mq.(*managedQueueWrapper).safeQ.(*mockSafeQueue)
		require.True(t, ok, "Failed to cast to *mockSafeQueue")
		testError := fmt.Errorf("simulated SafeQueue.CleanupExpired error")
		underlyingSafeQ.setCleanupExpiredError(testError)

		_, err := mq.CleanupExpired(time.Now(), func(item types.QueueItemAccessor, currentTime time.Time) (bool, types.QueueOutcome, error) {
			return false, types.QueueOutcomeDispatched, nil // Callback will not be hit if mock errors early
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, testError, "Error from ManagedQueue should wrap the SafeQueue's error")

		// Verify stats remain unchanged as the operation failed before any items were processed by the mock.
		assertGlobalStats(t, fr, initialGlobalLen, initialGlobalSize)
		assertBandStats(t, fr, testPriorityStandard, initialBandLen, initialBandSize)
	})
}

func TestManagedQueueWrapper_Accessors(t *testing.T) {
	t.Parallel()
	fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
	spec := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
	_ = fr.RegisterOrUpdateFlow(spec)
	mq, _ := fr.ActiveManagedQueue(testFlowID1)
	require.NotNil(t, mq)

	assert.Equal(t, spec, mq.FlowSpec(), "ManagedQueue.FlowSpec() mismatch")

	fqa := mq.FlowQueueAccessor()
	require.NotNil(t, fqa)
	assert.Equal(t, spec, fqa.FlowSpec(), "FlowQueueAccessor.FlowSpec() mismatch")
	assert.NotNil(t, fqa.Comparator(), "FlowQueueAccessor.Comparator() should not be nil")

	// Test embedded SafeQueue inspection methods
	assert.Contains(t, mq.Name(), mockQueueNameForRegistryTests)
	assert.NotEmpty(t, mq.Capabilities())
	_, err := mq.PeekHead() // Mock returns ErrQueueEmpty or ErrOpNotSupported
	assert.Error(t, err)    // Exact error depends on mock's PeekHead
}

func TestFlowQueueAccessorImpl_Methods(t *testing.T) {
	t.Parallel()
	fr := newTestFlowRegistry(t, mockQueueTestRegistryConfig)
	spec := mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard)
	_ = fr.RegisterOrUpdateFlow(spec)
	mq, _ := fr.ActiveManagedQueue(testFlowID1)
	fqa := mq.FlowQueueAccessor()
	require.NotNil(t, fqa)

	assert.Equal(t, spec, fqa.FlowSpec())
	assert.NotNil(t, fqa.Comparator())
	assert.Equal(t, 0, fqa.Len()) // Initially empty
	assert.Equal(t, uint64(0), fqa.ByteSize())
	assert.Contains(t, fqa.Name(), mockQueueNameForRegistryTests)
	assert.NotEmpty(t, fqa.Capabilities())
}

func TestInternalBandStateAccessor_Methods(t *testing.T) {
	t.Parallel()
	cfgWithCapacity := config.FlowRegistryConfig{
		PriorityBands: []config.PriorityBandConfig{
			{
				Priority:     testPriorityStandard,
				PriorityName: "StdBandWithCap",
				MaxBytes:     1024,
				QueueType:    mockQueueNameForRegistryTests,
			},
			{
				Priority:     testPriorityCritical,
				PriorityName: "CritBand",
				QueueType:    mockQueueNameForRegistryTests,
			},
		},
	}
	fr := newTestFlowRegistry(t, cfgWithCapacity)
	_ = fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification(testFlowID1, testPriorityStandard))
	_ = fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification(testFlowID2, testPriorityStandard))
	_ = fr.RegisterOrUpdateFlow(mocks.NewMockFlowSpecification(testFlowID3, testPriorityCritical)) // Different band

	accessor, err := fr.PriorityBandAccessor(testPriorityStandard)
	require.NoError(t, err)
	require.NotNil(t, accessor)

	assert.Equal(t, testPriorityStandard, accessor.Priority())
	assert.Equal(t, "StdBandWithCap", accessor.PriorityName())
	assert.Equal(t, uint64(1024), accessor.CapacityBytes())

	flowIDs := accessor.FlowIDs()
	sort.Strings(flowIDs)
	assert.Equal(t, []string{testFlowID1, testFlowID2}, flowIDs)

	q1Accessor := accessor.Queue(testFlowID1)
	require.NotNil(t, q1Accessor)
	assert.Equal(t, testFlowID1, q1Accessor.FlowSpec().ID())
	assert.Nil(t, accessor.Queue(testFlowID3), "FlowID3 should not be in standard band accessor")

	iteratedCount := 0
	accessor.IterateQueues(func(q types.FlowQueueAccessor) bool {
		iteratedCount++
		assert.Contains(t, []string{testFlowID1, testFlowID2}, q.FlowSpec().ID())
		return true // Continue iterating
	})
	assert.Equal(t, 2, iteratedCount, "IterateQueues should visit 2 queues in standard band")

	// Test iteration stop.
	iteratedCount = 0
	accessor.IterateQueues(func(q types.FlowQueueAccessor) bool {
		iteratedCount++
		return false // Stop after first
	})
	assert.Equal(t, 1, iteratedCount, "IterateQueues should stop after first if callback returns false")

	// Panic on non-existent band (whitebox test for internal accessor).
	nonExistentAccessor := &internalBandStateAccessor{registry: fr, bandPriority: 999}
	assert.Panics(t, func() { _ = nonExistentAccessor.PriorityName() })
}
