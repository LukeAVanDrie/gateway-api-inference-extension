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

package registry

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	testclock "k8s.io/utils/clock/testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/contracts"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework/plugins/queue"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol/mocks"
)

// --- Test Harness ---

// registryTestHarness provides a fully initialized test harness for the `FlowRegistry`.
type registryTestHarness struct {
	t         *testing.T
	fr        *FlowRegistry
	config    Config
	fakeClock *testclock.FakeClock
}

// harnessOptions configures the test harness.
type harnessOptions struct {
	config            *Config
	initialShardCount int
	manualGC          bool
}

// newRegistryTestHarness creates and starts a new `FlowRegistry` for testing.
func newRegistryTestHarness(t *testing.T, opts harnessOptions) *registryTestHarness {
	t.Helper()

	var cfg *Config
	var err error

	if opts.config != nil {
		cfg = opts.config.Clone()
		if opts.initialShardCount > 0 {
			cfg.InitialShardCount = opts.initialShardCount
		}
	} else {
		shardCount := 1
		if opts.initialShardCount > 0 {
			shardCount = opts.initialShardCount
		}

		cfg, err = NewConfig(
			newTestPluginsHandle(t),
			WithInitialShardCount(shardCount),
			WithFlowGCTimeout(5*time.Minute),
			WithPriorityBand(&PriorityBandConfig{Priority: highPriority, PriorityName: "High"}),
			WithPriorityBand(&PriorityBandConfig{Priority: lowPriority, PriorityName: "Low"}),
		)
		require.NoError(t, err, "Test setup: failed to create default config")
	}

	fakeClock := testclock.NewFakeClock(time.Now())
	registryOpts := []RegistryOption{withClock(fakeClock)}
	fr, err := NewFlowRegistry(cfg, logr.Discard(), registryOpts...)
	require.NoError(t, err, "Test setup: NewFlowRegistry should not fail")

	if !opts.manualGC {
		// Start the GC loop in the background.
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() {
			fr.Run(ctx)
		})
		t.Cleanup(func() {
			cancel()
			wg.Wait()
		})
	}

	return &registryTestHarness{
		t:         t,
		fr:        fr,
		config:    *fr.config,
		fakeClock: fakeClock,
	}
}

// assertFlowExists synchronously checks if a flow's queue exists on the first shard.
func (h *registryTestHarness) assertFlowExists(key flowcontrol.FlowKey, msgAndArgs ...any) {
	h.t.Helper()
	shards := *h.fr.allShards.Load()
	require.NotEmpty(h.t, shards, "Cannot check for flow existence when no shards are present")
	_, err := shards[0].ManagedQueue(key)
	assert.NoError(h.t, err, msgAndArgs...)
}

// assertFlowDoesNotExist synchronously checks if a flow's queue does not exist.
func (h *registryTestHarness) assertFlowDoesNotExist(key flowcontrol.FlowKey, msgAndArgs ...any) {
	h.t.Helper()
	shards := *h.fr.allShards.Load()
	if len(shards) == 0 {
		assert.True(h.t, true, "Flow correctly does not exist because no shards exist")
		return
	}
	_, err := shards[0].ManagedQueue(key)
	require.Error(h.t, err, "Expected an error when getting a non-existent flow, but got none")
	assert.ErrorIs(h.t, err, contracts.ErrFlowInstanceNotFound, msgAndArgs...)
}

// openConnectionOnFlow ensures a flow is registered for the provided `key`.
func (h *registryTestHarness) openConnectionOnFlow(key flowcontrol.FlowKey) {
	h.t.Helper()
	err := h.fr.WithConnection(key, func() error { return nil })
	require.NoError(h.t, err, "Registering flow %s should not fail", key)
	h.assertFlowExists(key, "Flow %s should exist after registration", key)
}

// --- `FlowRegistryClient` API Tests ---

func TestFlowRegistry_WithConnection_AndHandle(t *testing.T) {
	t.Parallel()

	t.Run("ShouldJITRegisterFlow_OnFirstConnection", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})
		key := flowcontrol.FlowKey{ID: "jit-flow", Priority: highPriority}

		h.assertFlowDoesNotExist(key, "Flow should not exist before the first connection")

		err := h.fr.WithConnection(key, func() error {
			h.assertFlowExists(key, "Flow should exist immediately after JIT registration within the connection")
			return nil
		})

		require.NoError(t, err, "WithConnection should succeed for a new flow")
		h.assertFlowExists(key, "Flow should remain in the registry after the connection is closed")
	})

	t.Run("ShouldFail_WhenFlowIDIsEmpty", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})
		key := flowcontrol.FlowKey{ID: "", Priority: highPriority} // Invalid key

		err := h.fr.WithConnection(key, func() error {
			t.Fatal("Callback must not be executed when the provided flow key is invalid")
			return nil
		})

		require.Error(t, err, "WithConnection must return an error for an empty flow ID")
		assert.ErrorIs(t, err, contracts.ErrFlowIDEmpty, "The returned error must be of the correct type")
	})

	t.Run("ShouldFail_WhenJITFails", func(t *testing.T) {
		t.Parallel()

		handle := newTestPluginsHandle(t)
		badQueueName := queue.RegisteredQueueName("non-existent-queue")
		badBand, err := NewPriorityBandConfig(handle, highPriority, "High", WithQueue(badQueueName))
		require.NoError(t, err)

		// Create a Config that uses a mock checker to bypass the strict validation.
		// The default checker would reject "non-existent-policy", but our mock says it's fine.
		// This allows us to instantiate the Registry with a latent configuration bomb.
		cfg, err := NewConfig(
			handle,
			WithPriorityBand(badBand),
			withCapabilityChecker(&mockCapabilityChecker{
				checkCompatibilityFunc: func(flowcontrol.OrderingPolicy, queue.RegisteredQueueName) error {
					return nil // Approve everything.
				},
			}),
		)
		require.NoError(t, err)

		h := newRegistryTestHarness(t, harnessOptions{config: cfg})
		key := flowcontrol.FlowKey{ID: "test-flow", Priority: highPriority}

		err = h.fr.WithConnection(key, func() error {
			t.Fatal("Callback must not be executed when the flow fails to register JIT")
			return nil
		})

		require.Error(t, err, "WithConnection must return an error for a failed flow JIT registration")
		assert.ErrorContains(t, err, "no SafeQueue registered", "The returned error must propagate the reason")
	})

	t.Run("Handle_Shards_ShouldReturnAllActiveShardsAndBeACopy", func(t *testing.T) {
		t.Parallel()
		// Create a registry with a known mixed topology of Active and Draining shards.
		h := newRegistryTestHarness(t, harnessOptions{initialShardCount: 3})
		err := h.fr.updateShardCount(2) // This leaves one shard in the Draining state.
		require.NoError(t, err, "Test setup: scaling down to create a draining shard should not fail")
		shards := *h.fr.allShards.Load()
		require.Len(t, shards, 3, "Test setup: should have 2 active and 1 draining shard")

		key := flowcontrol.FlowKey{ID: "test-flow", Priority: highPriority}

		err = h.fr.WithConnection(key, func() error {
			shards := h.fr.ActiveShards()

			//nolint:prealloc // iterators don't have a computable length
			var active []contracts.RegistryShard
			for s := range shards {
				active = append(active, s)
			}

			assert.Len(t, active, 2, "ActiveShards() must only return the Active shards")

			return nil
		})
		require.NoError(t, err)
	})
}

// --- Garbage Collection Tests ---

func TestFlowRegistry_GarbageCollection(t *testing.T) {
	t.Run("ShouldCollectIdleFlow", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})
		key := flowcontrol.FlowKey{ID: "idle-flow", Priority: highPriority}

		h.openConnectionOnFlow(key)                            // Create a flow, which is born Idle.
		h.fakeClock.Step(h.config.FlowGCTimeout + time.Second) // Advance the clock just past the GC timeout.
		h.fr.executeGCCycle()                                  // Manually and deterministically trigger a GC cycle.

		h.assertFlowDoesNotExist(key, "Idle flow should be collected by the GC")
	})

	t.Run("ShouldNotCollectActiveFlow", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})
		key := flowcontrol.FlowKey{ID: "active-flow", Priority: highPriority}

		var wg sync.WaitGroup
		leaseAcquired := make(chan struct{})
		releaseLease := make(chan struct{})

		wg.Go(func() {
			// This goroutine holds the lease. It will not exit until the main test goroutine calls `wg.Done()`.
			err := h.fr.WithConnection(key, func() error {
				close(leaseAcquired) // Signal to the main test that the lease is now active.
				<-releaseLease       // Block here, holding the lease, until signaled.

				return nil
			})
			require.NoError(t, err, "WithConnection in the background goroutine should not fail")
		})
		t.Cleanup(func() {
			close(releaseLease) // Unblock the goroutine.
			wg.Wait()           // Wait for the goroutine to fully exit.
		})

		<-leaseAcquired                              // Wait until the goroutine confirms that it has acquired the lease.
		h.fakeClock.Step(h.config.FlowGCTimeout * 2) // Advance the clock well past the GC timeout.
		h.fr.executeGCCycle()                        // Manually and deterministically trigger a GC cycle.

		h.assertFlowExists(key, "An active flow must not be garbage collected, even after a forced GC cycle")
	})

	t.Run("ShouldResetGCTimer_WhenFlowBecomesActive", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})
		key := flowcontrol.FlowKey{ID: "reactivated-flow", Priority: highPriority}
		h.openConnectionOnFlow(key)                            // Create an flow with a new idleness timer.
		h.fakeClock.Step(h.config.FlowGCTimeout - time.Second) // Advance the clock to just before the GC timeout.
		h.openConnectionOnFlow(key)                            // Open a new connection, resetting its idleness timer.
		h.fakeClock.Step(2 * time.Second)                      // Advance the clock again.
		h.fr.executeGCCycle()                                  // Manually and deterministically trigger a GC cycle.

		h.assertFlowExists(key, "Flow should survive GC because its idleness timer was reset")
	})

	t.Run("ShouldSkipGC_WhenIdleTimeoutExpired_ButActiveLeaseExists", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})
		key := flowcontrol.FlowKey{ID: "race-resurrected-flow", Priority: highPriority}
		h.openConnectionOnFlow(key)

		// Manually manipulate the state to simulate a race condition.
		// The flow is "Technically Idle" (timeout expired) ...
		val, ok := h.fr.flowStates.Load(key)
		require.True(t, ok)
		state := val.(*flowState)

		// Force the idle timestamp to be old.
		oldTime := h.fakeClock.Now().Add(-h.config.FlowGCTimeout * 2)

		state.mu.Lock()
		state.becameIdleAt = oldTime

		// ... BUT it has an active lease (simulating a request arriving just now).
		// Note: In the real code, these two updates happen atomically, but we force this
		// state to verify the GC's safety priority (Lease > Time).
		state.leaseCount = 1
		state.mu.Unlock()

		// Trigger GC.
		h.fr.executeGCCycle()

		// The GC should have seen the leaseCount > 0 and skipped the deletion, despite the expired timestamp.
		h.assertFlowExists(key, "Flow must not be collected if lease > 0, even if idle timer is expired")
	})

	t.Run("ShouldCollectDrainingShard_OnlyWhenEmpty", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{initialShardCount: 2})
		key := flowcontrol.FlowKey{ID: "flow-on-draining-shard", Priority: highPriority}
		h.openConnectionOnFlow(key)

		// Add an item to a queue on the soon-to-be Draining shard to keep it busy.
		drainingShard := h.fr.activeShards[1] // The shard that will become draining.
		mq, err := drainingShard.ManagedQueue(key)
		require.NoError(t, err, "Test setup: getting queue on draining shard failed")
		item := mocks.NewMockQueueItemAccessor(100, "req1", key)
		require.NoError(t, mq.Add(item), "Adding item to non-active shard should be allowed for in-flight requests")

		// Scale down to mark one shard as Draining.
		require.NoError(t, h.fr.updateShardCount(1), "Test setup: scale down should succeed")
		require.Len(t, h.fr.drainingShards, 1, "Test setup: one shard should be draining")

		// Trigger a GC cycle while the shard is not empty.
		h.fr.sweepDrainingShards()
		require.Len(t, h.fr.drainingShards, 1, "Draining shard should not be collected while it is not empty")

		// Empty the shard and trigger GC again.
		_, err = mq.Remove(item.Handle())
		require.NoError(t, err, "Test setup: removing item from draining shard failed")
		h.fr.sweepDrainingShards()
		assert.Empty(t, h.fr.drainingShards, "Draining shard should be collected after it becomes empty")
	})
}

// --- Shard Management Tests ---

func TestFlowRegistry_UpdateShardCount(t *testing.T) {
	t.Parallel()
	const (
		globalCapacity = 100
		bandCapacity   = 50
	)
	testCases := []struct {
		name                string
		initialShardCount   int
		targetShardCount    int
		expectedActiveCount int
		expectErrIs         error // Optional
	}{
		{
			name:                "NoOp_ScaleToSameCount",
			initialShardCount:   2,
			targetShardCount:    2,
			expectedActiveCount: 2,
		},
		{
			name:                "Succeeds_ScaleUp_FromOne",
			initialShardCount:   1,
			targetShardCount:    4,
			expectedActiveCount: 4,
		},
		{
			name:                "Succeeds_ScaleDown_ToOne",
			initialShardCount:   3,
			targetShardCount:    1,
			expectedActiveCount: 1,
		},
		{
			name:                "Error_ScaleDown_ToZero",
			initialShardCount:   2,
			targetShardCount:    0,
			expectedActiveCount: 2,
			expectErrIs:         contracts.ErrInvalidShardCount,
		},
		{
			name:                "Error_ScaleDown_ToNegative",
			initialShardCount:   1,
			targetShardCount:    -1,
			expectedActiveCount: 1,
			expectErrIs:         contracts.ErrInvalidShardCount,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handle := newTestPluginsHandle(t)
			band, err := NewPriorityBandConfig(handle, highPriority, "A", WithBandMaxBytes(bandCapacity))
			require.NoError(t, err)

			config, err := NewConfig(
				handle,
				WithMaxBytes(globalCapacity),
				WithInitialShardCount(tc.initialShardCount),
				WithPriorityBand(band),
			)
			require.NoError(t, err, "Test setup: creating config should not fail")

			h := newRegistryTestHarness(t, harnessOptions{config: config})
			key := flowcontrol.FlowKey{ID: "flow", Priority: highPriority}
			h.openConnectionOnFlow(key)

			err = h.fr.updateShardCount(tc.targetShardCount)
			if tc.expectErrIs != nil {
				require.Error(t, err, "UpdateShardCount should have returned an error")
				assert.ErrorIs(t, err, tc.expectErrIs, "Error should be the expected type")
			} else {
				require.NoError(t, err, "UpdateShardCount should not have returned an error")
			}

			h.fr.mu.RLock()
			finalActiveCount := len(h.fr.activeShards)
			finalDrainingCount := len(h.fr.drainingShards)
			for _, shard := range h.fr.activeShards {
				h.assertFlowExists(key, "Shard %s should contain the existing flow", shard.ID())
			}
			h.fr.mu.RUnlock()

			expectedDrainingCount := 0
			if tc.initialShardCount > tc.expectedActiveCount {
				expectedDrainingCount = tc.initialShardCount - tc.expectedActiveCount
			}
			assert.Equal(t, tc.expectedActiveCount, finalActiveCount, "Final active shard count is incorrect")
			assert.Equal(t, expectedDrainingCount, finalDrainingCount, "Final draining shard count in registry is incorrect")

			// Prove capacity partitioning (Black-Box Probing)
			if finalActiveCount > 0 {
				shard := h.fr.activeShards[0]
				mq, err := shard.ManagedQueue(key)
				require.NoError(t, err)

				expectedShardCapacity := uint64(bandCapacity / finalActiveCount)
				itemSize := uint64(10)
				maxItems := expectedShardCapacity / itemSize

				var addedItems []flowcontrol.QueueItemHandle
				for i := range maxItems {
					item := mocks.NewMockQueueItemAccessor(itemSize, fmt.Sprintf("req-%d", i), key)
					require.True(t, shard.HasCapacity(key.Priority, itemSize), "Shard must report capacity for items within its partition limit")
					err := mq.Add(item)
					require.NoError(t, err)
					addedItems = append(addedItems, item.Handle())
				}

				require.False(t, shard.HasCapacity(key.Priority, itemSize), "Shard must report no capacity for items exceeding its partition limit")

				for _, h := range addedItems {
					_, _ = mq.Remove(h)
				}
			}
		})
	}
}

// --- Dynamic Provisioning Tests ---

func TestFlowRegistry_DynamicProvisioning(t *testing.T) {
	t.Parallel()

	t.Run("ShouldCreateBand_WhenPriorityIsUnknown", func(t *testing.T) {
		t.Parallel()
		// Start with 2 shards to ensure propagation works across the cluster.
		h := newRegistryTestHarness(t, harnessOptions{initialShardCount: 2})
		dynamicPrio := 55
		key := flowcontrol.FlowKey{ID: "dynamic-flow", Priority: dynamicPrio}

		// Connect with a new priority.
		err := h.fr.WithConnection(key, func() error {
			return nil
		})
		require.NoError(t, err, "WithConnection should succeed for dynamic priority")

		h.fr.mu.RLock()
		_, existsInConfig := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.True(t, existsInConfig, "Dynamic priority must be added to global config definition")

		for _, shard := range h.fr.activeShards {
			_, err := shard.ManagedQueue(key)
			assert.NoError(t, err, "Dynamic band must be provisioned on shard %s", shard.ID())
		}
	})

	t.Run("ShouldHandleConcurrentDynamicCreation", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{initialShardCount: 2})
		dynamicPrio := 77
		key := flowcontrol.FlowKey{ID: "race-flow", Priority: dynamicPrio}

		var wg sync.WaitGroup
		concurrency := 10
		wg.Add(concurrency)

		for range concurrency {
			go func() {
				defer wg.Done()
				// Everyone tries to trigger provisioning simultaneously.
				_ = h.fr.WithConnection(key, func() error { return nil })
			}()
		}
		wg.Wait()

		h.fr.mu.RLock()
		_, exists := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.True(t, exists, "Band should exist after concurrent creation attempts")
	})

	t.Run("ShouldPersistDynamicBands_AcrossScalingEvents", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{initialShardCount: 1})
		dynamicPrio := 88
		key := flowcontrol.FlowKey{ID: "scaling-flow", Priority: dynamicPrio}

		// Create dynamic band on Shard 0.
		h.openConnectionOnFlow(key)

		// Scale Up -> Shard 1 created.
		err := h.fr.updateShardCount(2)
		require.NoError(t, err)

		// The repartition logic should have carried the dynamic band definition to the new shard config.
		h.fr.mu.RLock()
		newShard := h.fr.activeShards[1]
		h.fr.mu.RUnlock()

		_, policyErr := newShard.FairnessPolicy(dynamicPrio)
		assert.NoError(t, policyErr, "New shard must have the dynamic priority band configured")

		mq, err := newShard.ManagedQueue(key)
		require.NoError(t, err, "Existing flows should be auto-synced to new shards during scale-up")
		require.NotNil(t, mq)
	})
}

// --- Concurrency Tests ---

func TestFlowRegistry_Concurrency(t *testing.T) {
	t.Parallel()

	t.Run("ConcurrentJITRegistrations_ShouldBeSafe", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})
		key := flowcontrol.FlowKey{ID: "concurrent-flow", Priority: highPriority}
		numGoroutines := 50
		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		// Hammer the `WithConnection` method for the same key from many goroutines.
		for range numGoroutines {
			go func() {
				defer wg.Done()
				err := h.fr.WithConnection(key, func() error {
					// Do a small amount of work inside the connection.
					time.Sleep(1 * time.Millisecond)
					return nil
				})
				require.NoError(t, err, "Concurrent WithConnection calls must not fail")
			}()
		}
		wg.Wait()

		// The primary assertion is that this completes without the race detector firing.
		// We can also check that the flow state is consistent.
		h.assertFlowExists(key, "Flow must exist after concurrent JIT registration")
	})

	t.Run("ShouldRecover_WhenGCDeletesFlow_DuringConnectionAttempt", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})
		key := flowcontrol.FlowKey{ID: "zombie-race-flow", Priority: highPriority}

		// We want to force the specific race in pinActiveFlow where:
		// 1. User loads ptr A.
		// 2. GC deletes ptr A from Map.
		// 3. User checks map, sees nil or ptr B.
		// 4. User retries.

		var wg sync.WaitGroup
		stopCh := make(chan struct{})

		// Routine 1: The "User" - Constantly tries to connect.
		wg.Go(func() {
			for {
				select {
				case <-stopCh:
					return
				default:
					// This triggers the optimistic loop.
					err := h.fr.WithConnection(key, func() error {
						return nil
					})
					if err != nil {
						h.t.Logf("Connection failed during race: %v", err)
					}
				}
			}
		})

		// Routine 2: The "GC" - Constantly deletes the flow.
		wg.Go(func() {
			for {
				select {
				case <-stopCh:
					return
				default:
					// Forcefully delete the key to trigger the "Zombie" condition in Routine 1.
					h.fr.flowStates.Delete(key)
					time.Sleep(100 * time.Microsecond) // Yield briefly to let Routine 1 make progress
				}
			}
		})

		// Let the chaos run for a bit.
		time.Sleep(100 * time.Millisecond)
		close(stopCh)
		wg.Wait()

		// Final consistency check: Ensure that we can still connect successfully after the chaos.
		// If the optimistic loop works, the final state in the map should be valid.
		h.openConnectionOnFlow(key)
	})

	t.Run("ShouldBackOff_WhenFlowIsMarkedForDeletion_ButStillInMap", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})
		key := flowcontrol.FlowKey{ID: "doomed-flow", Priority: highPriority}
		h.openConnectionOnFlow(key)

		// Get the original flow state object.
		val, ok := h.fr.flowStates.Load(key)
		require.True(t, ok)
		originalState := val.(*flowState)

		// Manually poison it (simulate GC step: marked but not yet deleted from map).
		originalState.mu.Lock()
		originalState.markedForDeletion = true
		originalState.mu.Unlock()

		// Attempt to connect.
		// It will detect the deletion, proactively remove it, create a new flow, and succeed.
		err := h.fr.WithConnection(key, func() error {
			return nil
		})
		require.NoError(t, err, "WithConnection should recover and succeed")

		// Verification: Ensure we are using a fresh object, not the resurrected corpse.
		newVal, ok := h.fr.flowStates.Load(key)
		require.True(t, ok)
		assert.NotSame(t, originalState, newVal, "Should have created a new flow object, not reused the marked one")
	})

	t.Run("MixedAdminAndDataPlaneWorkload", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{initialShardCount: 1})
		const (
			numWorkers    = 10
			opsPerWorker  = 50
			maxShardCount = 4
		)

		var wg sync.WaitGroup
		wg.Add(numWorkers + 1) // +1 for the scaling goroutine

		// Data Plane Workers: Constantly creating new flows.
		for i := range numWorkers {
			workerID := i
			go func() {
				defer wg.Done()
				for j := range opsPerWorker {
					key := flowcontrol.FlowKey{ID: fmt.Sprintf("flow-%d-%d", workerID, j), Priority: highPriority}
					_ = h.fr.WithConnection(key, func() error { return nil })
				}
			}()
		}

		// Admin Worker: Constantly scaling the number of shards up and down.
		go func() {
			defer wg.Done()
			for i := 1; i < maxShardCount; i++ {
				_ = h.fr.updateShardCount(i + 1)
				_ = h.fr.updateShardCount(i)
			}
		}()

		wg.Wait()

		// The test completing without a race condition is the primary assertion.
		// We can also assert a consistent final state.
		assert.Len(t, h.fr.activeShards, maxShardCount-1, "Final active shard count should be consistent")
		flowCount := 0
		h.fr.flowStates.Range(func(_, _ any) bool {
			flowCount++
			return true
		})
		assert.Equal(t, numWorkers*opsPerWorker, flowCount, "All concurrently registered flows must be present")
	})

	t.Run("ConcurrentDynamicBandProvisioning_WithGC", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})

		const (
			numWorkers    = 10
			numPriorities = 20 // Create 20 different dynamic bands
			opsPerWorker  = 10
		)

		var wg sync.WaitGroup
		wg.Add(numWorkers + 1) // +1 for GC goroutine

		// Workers: Create flows at random priorities
		for i := range numWorkers {
			go func() {
				defer wg.Done()
				for j := range opsPerWorker {
					priority := 100 + (j % numPriorities) // Rotate through priorities
					key := flowcontrol.FlowKey{
						ID:       fmt.Sprintf("flow-%d-%d", i, j),
						Priority: priority,
					}
					_ = h.fr.WithConnection(key, func() error {
						time.Sleep(1 * time.Millisecond)
						return nil
					})
				}
			}()
		}

		// Wait for at least one band to be created.
		require.Eventually(t, func() bool {
			count := 0
			h.fr.priorityBandStates.Range(func(_, _ any) bool {
				count++
				return true
			})
			return count > 0
		}, 5*time.Second, 10*time.Millisecond, "Dynamic bands should be created")

		// Check bands were created before GC collects them all
		bandCount := 0
		h.fr.priorityBandStates.Range(func(_, _ any) bool {
			bandCount++
			return true
		})
		require.True(t, bandCount > 0, "Dynamic bands should be created during concurrent workload")

		// GC Worker: Constantly running GC cycles
		go func() {
			defer wg.Done()
			for range 10 {
				h.fakeClock.Step(h.config.FlowGCTimeout + time.Second)
				h.fr.executeGCCycle()
				time.Sleep(5 * time.Millisecond)
			}
		}()

		wg.Wait()

		// Primary assertion: no race detector failures
		// Test completing without races proves concurrent band provisioning/GC is safe
	})
}

func TestFlowRegistry_deletePriorityBand(t *testing.T) {
	t.Parallel()

	// highPriority (20) and lowPriority (10) come from package-level constants
	const dynamicPrio = 120

	t.Run("ShouldDeleteDynamicBandFromAllShards", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{initialShardCount: 3})

		// Create a dynamic priority band via JIT provisioning
		err := h.fr.ensurePriorityBand(dynamicPrio)
		require.NoError(t, err)

		// Verify band exists in registry config
		h.fr.mu.RLock()
		_, exists := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		require.True(t, exists, "Dynamic band should exist in registry config")

		// Verify band exists in all shards
		shards := *h.fr.allShards.Load()

		for i, shard := range shards {
			_, ok := shard.priorityBands.Load(dynamicPrio)
			require.True(t, ok, "Band should exist in shard %d", i)

			shard.mu.RLock()
			_, ok = shard.config.PriorityBands[dynamicPrio]
			shard.mu.RUnlock()
			require.True(t, ok, "Band should exist in shard %d config", i)
		}

		// Delete the band
		h.fr.deletePriorityBand(dynamicPrio)

		// Verify band removed from registry config
		h.fr.mu.RLock()
		_, exists = h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.False(t, exists, "Band should be removed from registry config")

		// Verify band removed from stats
		_, exists = h.fr.perPriorityBandStats.Load(dynamicPrio)
		assert.False(t, exists, "Band should be removed from stats tracking")

		// Verify band removed from all shards
		for i, shard := range shards {
			_, ok := shard.priorityBands.Load(dynamicPrio)
			assert.False(t, ok, "Band should be removed from shard %d", i)

			shard.mu.RLock()
			_, ok = shard.config.PriorityBands[dynamicPrio]
			shard.mu.RUnlock()
			assert.False(t, ok, "Band should be removed from shard %d config", i)

			// Verify removed from ordered list
			orderedList := *shard.orderedPriorityLevels.Load()
			for _, p := range orderedList {
				assert.NotEqual(t, dynamicPrio, p, "Band priority should be removed from ordered list in shard %d", i)
			}
		}
	})

	t.Run("ShouldNotAffectOtherBands", func(t *testing.T) {
		t.Parallel()
		// Note: this creates the highPriority, lowPriority bands
		h := newRegistryTestHarness(t, harnessOptions{})

		// Create a dynamic band
		err := h.fr.ensurePriorityBand(dynamicPrio)
		require.NoError(t, err)

		// Verify both static and dynamic bands exist
		h.fr.mu.RLock()
		_, highExists := h.fr.config.PriorityBands[highPriority]
		_, lowExists := h.fr.config.PriorityBands[lowPriority]
		_, dynamicExists := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		require.True(t, highExists && lowExists && dynamicExists, "All bands should exist")

		// Delete the dynamic band
		h.fr.deletePriorityBand(dynamicPrio)

		// Verify static bands still exist
		h.fr.mu.RLock()
		_, highExists = h.fr.config.PriorityBands[highPriority]
		_, lowExists = h.fr.config.PriorityBands[lowPriority]
		_, dynamicExists = h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()

		assert.True(t, highExists, "Static high priority band should still exist")
		assert.True(t, lowExists, "Static low priority band should still exist")
		assert.False(t, dynamicExists, "Dynamic band should be deleted")
	})

	t.Run("ShouldHandleNonExistentBand", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{})

		// Try to delete a band that doesn't exist - should not panic
		require.NotPanics(t, func() {
			h.fr.deletePriorityBand(999)
		})
	})
}

// --- Priority Band Garbage Collection Tests ---

func TestFlowRegistry_PriorityBandGarbageCollection(t *testing.T) {
	const dynamicPrio = 99

	t.Run("ShouldCollectIdleDynamicBand", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})
		key := flowcontrol.FlowKey{ID: "test-flow", Priority: dynamicPrio}

		// Create dynamic band via JIT provisioning
		h.openConnectionOnFlow(key)

		// Verify band exists
		h.fr.mu.RLock()
		_, exists := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		require.True(t, exists, "Dynamic band should exist after flow creation")

		// Step 1: Collect the flow (makes band empty)
		h.fakeClock.Step(h.config.FlowGCTimeout + time.Second)
		h.fr.executeGCCycle()
		h.assertFlowDoesNotExist(key, "Flow should be collected")

		// Band should still exist (in grace period)
		h.fr.mu.RLock()
		_, exists = h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.True(t, exists, "Band should still exist during grace period")

		// Step 2: Wait for band GC timeout
		h.fakeClock.Step(h.config.PriorityBandGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Band should be collected
		h.fr.mu.RLock()
		_, exists = h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.False(t, exists, "Dynamic band should be collected after timeout")
	})

	t.Run("ShouldNotCollectStaticBands", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})

		// Advance time well past any GC timeout
		h.fakeClock.Step(h.config.FlowGCTimeout + h.config.PriorityBandGCTimeout + time.Hour)
		h.fr.executeGCCycle()

		// Static bands should still exist
		h.fr.mu.RLock()
		_, highExists := h.fr.config.PriorityBands[highPriority]
		_, lowExists := h.fr.config.PriorityBands[lowPriority]
		h.fr.mu.RUnlock()

		assert.True(t, highExists, "Static high priority band should never be collected")
		assert.True(t, lowExists, "Static low priority band should never be collected")
	})

	t.Run("ShouldCollectMultipleBands_InOneCycle", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})

		// Create 3 dynamic bands
		prio1, prio2, prio3 := 101, 102, 103
		h.openConnectionOnFlow(flowcontrol.FlowKey{ID: "flow-1", Priority: prio1})
		h.openConnectionOnFlow(flowcontrol.FlowKey{ID: "flow-2", Priority: prio2})
		h.openConnectionOnFlow(flowcontrol.FlowKey{ID: "flow-3", Priority: prio3})

		// Verify all bands exist
		h.fr.mu.RLock()
		_, exists1 := h.fr.config.PriorityBands[prio1]
		_, exists2 := h.fr.config.PriorityBands[prio2]
		_, exists3 := h.fr.config.PriorityBands[prio3]
		h.fr.mu.RUnlock()
		require.True(t, exists1 && exists2 && exists3, "All dynamic bands should exist")

		// Collect all flows (all bands become empty)
		h.fakeClock.Step(h.config.FlowGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Wait for band GC timeout
		h.fakeClock.Step(h.config.PriorityBandGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// All bands should be collected in a single GC cycle
		h.fr.mu.RLock()
		_, exists1 = h.fr.config.PriorityBands[prio1]
		_, exists2 = h.fr.config.PriorityBands[prio2]
		_, exists3 = h.fr.config.PriorityBands[prio3]
		h.fr.mu.RUnlock()

		assert.False(t, exists1, "Band 1 should be collected")
		assert.False(t, exists2, "Band 2 should be collected")
		assert.False(t, exists3, "Band 3 should be collected")
	})

	t.Run("ShouldCollectBand_AcrossMultipleShards", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{initialShardCount: 3})
		key := flowcontrol.FlowKey{ID: "test-flow", Priority: dynamicPrio}

		// Create flow on all shards
		h.openConnectionOnFlow(key)

		// Verify band exists on all shards
		shards := *h.fr.allShards.Load()
		require.Len(t, shards, 3, "Should have 3 shards")

		for i, shard := range shards {
			_, ok := shard.priorityBands.Load(dynamicPrio)
			require.True(t, ok, "Band should exist on shard %d", i)
		}

		// Collect the flow
		h.fakeClock.Step(h.config.FlowGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Collect the band
		h.fakeClock.Step(h.config.PriorityBandGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Verify band is removed from registry
		h.fr.mu.RLock()
		_, exists := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.False(t, exists, "Band should be removed from registry config")

		// Verify band is removed from all shards
		for i, shard := range shards {
			_, ok := shard.priorityBands.Load(dynamicPrio)
			assert.False(t, ok, "Band should be removed from shard %d", i)

			// Verify config also cleaned up
			shard.mu.RLock()
			_, configExists := shard.config.PriorityBands[dynamicPrio]
			shard.mu.RUnlock()
			assert.False(t, configExists, "Band should be removed from shard %d config", i)
		}
	})

	t.Run("ShouldHandleConcurrentFlowCreation_DuringBandGC", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})
		key := flowcontrol.FlowKey{ID: "test-flow", Priority: dynamicPrio}

		// Create and collect flow (band becomes empty)
		h.openConnectionOnFlow(key)
		h.fakeClock.Step(h.config.FlowGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Advance past band timeout to make it a GC candidate
		h.fakeClock.Step(h.config.PriorityBandGCTimeout + time.Second)

		// Now band dynamicPrio still exists, but it is candidate for GC.
		val, ok := h.fr.priorityBandStates.Load(dynamicPrio)
		require.True(t, ok, "Band state should exist")
		state := val.(*priorityBandState)

		// Verify band is idle and eligible for GC
		state.mu.Lock()
		require.False(t, state.becameIdleAt.IsZero(), "Band should be idle")
		require.Equal(t, 0, state.leaseCount, "Band should have no active leases")
		state.mu.Unlock()

		// Create a new flow _before_ running GC
		// This will pin the band (increment leaseCount during provisioning)
		newKey := flowcontrol.FlowKey{ID: "new-flow", Priority: dynamicPrio}
		h.openConnectionOnFlow(newKey)

		// Run GC - it should NOT collect the band because:
		// 1. The band now has an active flow (not empty)
		// 2. updateIdleBands will reset becameIdleAt because the band is no longer empty
		h.fr.executeGCCycle()

		// Band should NOT be collected (new flow exists)
		h.fr.mu.RLock()
		_, exists := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.True(t, exists, "Band should not be collected when it has active flows")

		// Verify band is no longer idle
		state.mu.Lock()
		isIdle := !state.becameIdleAt.IsZero()
		state.mu.Unlock()
		assert.False(t, isIdle, "Band should not be idle when it has flows")
	})

	t.Run("ShouldReleaseBandLease_OnJITFailure", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})

		key := flowcontrol.FlowKey{ID: "jit-fail-flow", Priority: dynamicPrio}

		// Manually create the priority band
		err := h.fr.ensurePriorityBand(dynamicPrio)
		require.NoError(t, err)

		// Corrupt the config to make buildFlowComponents fail
		// We set an invalid queue name AFTER the band is created but BEFORE the first flow tries to use it
		h.fr.mu.Lock()
		h.fr.config.PriorityBands[dynamicPrio].Queue = "NonExistentQueue"
		h.fr.mu.Unlock()

		// Attempt to open connection - JIT should fail during buildFlowComponents
		err = h.fr.WithConnection(key, func() error {
			t.Fatal("Should not reach callback when JIT fails")
			return nil
		})

		require.Error(t, err, "WithConnection should fail when buildFlowComponents fails")
		require.Contains(t, err.Error(), "NonExistentQueue", "Error should mention the invalid queue")

		// Verify the flow was cleaned up
		h.assertFlowDoesNotExist(key, "Flow should not exist after JIT failure")

		// Verify band lease was released - band should have zero leaseCount
		val, ok := h.fr.priorityBandStates.Load(dynamicPrio)
		require.True(t, ok, "Band state should still exist")
		state := val.(*priorityBandState)

		state.mu.Lock()
		leaseCount := state.leaseCount
		state.mu.Unlock()

		assert.Equal(t, 0, leaseCount, "Band lease should be released after JIT failure")
	})

	t.Run("ShouldMaintainBandLeaseCount_MatchingActiveFlowCount", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})

		// Create 3 flows at the same priority
		key1 := flowcontrol.FlowKey{ID: "flow-1", Priority: dynamicPrio}
		key2 := flowcontrol.FlowKey{ID: "flow-2", Priority: dynamicPrio}
		key3 := flowcontrol.FlowKey{ID: "flow-3", Priority: dynamicPrio}

		h.openConnectionOnFlow(key1)
		h.openConnectionOnFlow(key2)
		h.openConnectionOnFlow(key3)

		// Verify band leaseCount is 3
		val, ok := h.fr.priorityBandStates.Load(dynamicPrio)
		require.True(t, ok, "Band state should exist")
		state := val.(*priorityBandState)

		state.mu.Lock()
		leaseCount := state.leaseCount
		state.mu.Unlock()

		assert.Equal(t, 3, leaseCount, "Band should have 3 leases (one per flow)")

		// Create flow-1 earlier than the other two, so it expires first
		// Manipulate becameIdleAt for flow-1 to make it older
		val1, ok1 := h.fr.flowStates.Load(key1)
		require.True(t, ok1)
		flow1 := val1.(*flowState)

		oldTime := h.fakeClock.Now().Add(-(h.config.FlowGCTimeout + time.Second))
		flow1.mu.Lock()
		flow1.becameIdleAt = oldTime
		flow1.mu.Unlock()

		// Collect only flow-1 (the old one)
		h.fr.executeGCCycle()

		// Verify band leaseCount is now 2
		state.mu.Lock()
		leaseCount = state.leaseCount
		state.mu.Unlock()

		assert.Equal(t, 2, leaseCount, "Band should have 2 leases after one flow is collected")

		// Now age the remaining flows and collect them
		h.fakeClock.Step(h.config.FlowGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Verify band leaseCount is now 0
		state.mu.Lock()
		leaseCount = state.leaseCount
		state.mu.Unlock()

		assert.Equal(t, 0, leaseCount, "Band should have 0 leases after all flows are collected")
	})

	t.Run("ShouldNotCollectBand_WhileAnyFlowExists", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})

		// Create 3 flows at the same priority
		key1 := flowcontrol.FlowKey{ID: "flow-1", Priority: dynamicPrio}
		key2 := flowcontrol.FlowKey{ID: "flow-2", Priority: dynamicPrio}
		key3 := flowcontrol.FlowKey{ID: "flow-3", Priority: dynamicPrio}

		h.openConnectionOnFlow(key1)
		h.openConnectionOnFlow(key2)
		h.openConnectionOnFlow(key3)

		// Manually age flow-1 and flow-2 to make them eligible for GC, but leave flow-3 young
		oldTime := h.fakeClock.Now().Add(-(h.config.FlowGCTimeout + time.Second))

		val1, _ := h.fr.flowStates.Load(key1)
		flow1 := val1.(*flowState)
		flow1.mu.Lock()
		flow1.becameIdleAt = oldTime
		flow1.mu.Unlock()

		val2, _ := h.fr.flowStates.Load(key2)
		flow2 := val2.(*flowState)
		flow2.mu.Lock()
		flow2.becameIdleAt = oldTime
		flow2.mu.Unlock()

		// Collect flow-1 and flow-2, leaving flow-3 active
		h.fr.executeGCCycle()

		// Verify flow-3 still exists
		h.assertFlowExists(key3, "Flow-3 should still exist")

		// Verify band still exists (has flow-3)
		h.fr.mu.RLock()
		_, exists := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.True(t, exists, "Band should not be collected while any flow exists")

		// Advance time, but NOT enough to make flow-3 eligible for GC
		// (We need to avoid advancing past FlowGCTimeout from flow-3's creation time)
		h.fakeClock.Step(time.Second)
		h.fr.executeGCCycle()

		// Band should still NOT be collected (still has flow-3)
		h.fr.mu.RLock()
		_, exists = h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.True(t, exists, "Band should not be collected while any flow exists")

		// Verify band leaseCount is 1
		val, ok := h.fr.priorityBandStates.Load(dynamicPrio)
		require.True(t, ok, "Band state should exist")
		state := val.(*priorityBandState)

		state.mu.Lock()
		leaseCount := state.leaseCount
		state.mu.Unlock()

		assert.Equal(t, 1, leaseCount, "Band should still have 1 lease")

		// Collect the last flow
		h.fakeClock.Step(h.config.FlowGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Verify band leaseCount is now 0
		state.mu.Lock()
		leaseCount = state.leaseCount
		state.mu.Unlock()

		assert.Equal(t, 0, leaseCount, "Band should have 0 leases after last flow is collected")

		// Now advance past band timeout and collect
		h.fakeClock.Step(h.config.PriorityBandGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Band should be collected now
		h.fr.mu.RLock()
		_, exists = h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.False(t, exists, "Band should be collected after all flows are gone")
	})

	t.Run("ShouldNotCollectBand_WhenLeaseCount_NonZero_DespiteEmpty", func(t *testing.T) {
		t.Parallel()
		h := newRegistryTestHarness(t, harnessOptions{manualGC: true})

		key := flowcontrol.FlowKey{ID: "test-flow", Priority: dynamicPrio}
		h.openConnectionOnFlow(key)

		// Manually manipulate the state to simulate a race condition.
		// Verify band exists and has 1 flow lease
		val, ok := h.fr.priorityBandStates.Load(dynamicPrio)
		require.True(t, ok, "Band state should exist")
		state := val.(*priorityBandState)

		state.mu.Lock()
		require.Equal(t, 1, state.leaseCount, "Band should have 1 flow lease")
		state.mu.Unlock()

		// Collect the flow so the band becomes empty
		h.fakeClock.Step(h.config.FlowGCTimeout + time.Second)
		h.fr.executeGCCycle()

		// Verify band leaseCount is now 0 and band is idle
		state.mu.Lock()
		require.Equal(t, 0, state.leaseCount, "Band should have 0 leases")
		require.False(t, state.becameIdleAt.IsZero(), "Band should be idle")
		state.mu.Unlock()

		// Advance past band timeout
		oldTime := h.fakeClock.Now().Add(-h.config.PriorityBandGCTimeout * 2)
		h.fakeClock.Step(h.config.PriorityBandGCTimeout + time.Second)

		// Manually force a race: band is "Technically Idle" (timeout expired and empty)...
		state.mu.Lock()
		state.becameIdleAt = oldTime

		// ... BUT it has a lease (simulating a flow creation happening just now).
		// Note: In real code, these updates happen atomically during flow creation,
		// but we force this state to verify the GC's safety priority (Lease > Empty + Time).
		state.leaseCount = 1
		state.mu.Unlock()

		// Trigger GC
		h.fr.executeGCCycle()

		// The GC should have seen leaseCount > 0 and skipped deletion, despite
		// the band being empty and the idle timer being expired.
		h.fr.mu.RLock()
		_, exists := h.fr.config.PriorityBands[dynamicPrio]
		h.fr.mu.RUnlock()
		assert.True(t, exists, "Band must not be collected if leaseCount > 0, even if empty and idle timer expired")
	})
}

// TestFlowRegistry_JITErrorScoping ensures that JIT provisioning errors are correctly propagated to all concurrent
// requests waiting on the same flow initialization.
func TestFlowRegistry_JITErrorScoping(t *testing.T) {
	t.Parallel()
	handle := newTestPluginsHandle(t)

	// Create a registry with a capability checker that passes validation but using a queue name that doesn't exist.
	// This ensures NewConfig succeeds, but JIT (ensureFlowInfrastructure) fails when trying to instantiate the queue.
	failQueueName := queue.RegisteredQueueName("NonExistentQueue")
	mockChecker := &mockCapabilityChecker{
		checkCompatibilityFunc: func(p flowcontrol.OrderingPolicy, q queue.RegisteredQueueName) error {
			return nil // Bypass validation.
		},
	}

	// We create a custom band config that uses this failing queue.
	// We set it as the default band so that dynamic provisioning is used.
	failingBand, err := NewPriorityBandConfig(handle, 0, "FailingBand", WithQueue(failQueueName))
	require.NoError(t, err)

	cfg, err := NewConfig(handle, withCapabilityChecker(mockChecker), WithDefaultPriorityBand(failingBand))
	require.NoError(t, err)

	registry, err := NewFlowRegistry(cfg, logr.Discard())
	require.NoError(t, err)

	key := flowcontrol.FlowKey{
		Priority: 100, // Dynamic, will trigger ensurePriorityBand
		ID:       "flow-should-fail",
	}

	// Simulate contention:
	// We acquire the registry RLock.
	// JIT provisioning (dynamic band) requires registry Lock (Write Lock).
	// So the first thread to reach ensurePriorityBand will block until we release this lock.
	// All other threads will pile up behind it on sync.Once.
	registry.mu.RLock()

	const concurrency = 10
	var successCount atomic.Int32
	var errorCount atomic.Int32
	wg := sync.WaitGroup{}
	wg.Add(concurrency)

	start := make(chan struct{})

	for range concurrency {
		go func() {
			defer wg.Done()
			<-start
			err := registry.WithConnection(key, func() error {
				return nil
			})
			if err == nil {
				successCount.Add(1)
			} else {
				errorCount.Add(1)
			}
		}()
	}

	close(start)

	// Wait a bit to ensure everyone is stuck.
	time.Sleep(100 * time.Millisecond)

	// Release the lock, letting the winner proceed (and fail).
	registry.mu.RUnlock()

	wg.Wait()

	// Assertion: all requests should fail.
	assert.Equal(t, int32(concurrency), errorCount.Load(), "All requests should fail JIT provisioning")
	assert.Equal(t, int32(0), successCount.Load(), "No request should succeed if JIT failed")
}
