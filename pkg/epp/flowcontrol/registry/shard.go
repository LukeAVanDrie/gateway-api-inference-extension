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
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"

	"iter"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/common/observability/logging"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/contracts"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/flowcontrol"
)

// priorityBand holds all managedQueues and configuration for a single priority level within a shard.
type priorityBand struct {
	// --- Immutable (set at construction) ---

	// fairnessPolicy is the singleton plugin instance governing this band.
	// It is duplicated here from the config to allow lock-free access on the hot path.
	fairnessPolicy flowcontrol.FairnessPolicy

	// policyState holds the opaque, mutable state for the fairness policy.
	// It is initialized once at creation via fairnessPolicy.NewState() and exposed via GetPolicyState().
	policyState any

	// --- State Protected by the parent shard's mu ---

	// config is the local copy of the band's definition.
	// It is updated during dynamic scaling events (updateConfig), protected by the parent shard's mutex.
	config PriorityBandConfig

	priority     int
	priorityName string

	// queues holds all managedQueue instances within this band, keyed by their logical ID string.
	// The priority is implicit from the parent priorityBand.
	queues sync.Map

	// --- Concurrent-Safe State (Atomics) ---

	// Band-level statistics, updated via lock-free propagation from child queues.
	byteSize atomic.Int64
	len      atomic.Int64
}

// registryShard implements the `contracts.RegistryShard` interface.
//
// # Role: The Data Plane Slice
//
// It represents a single, concurrent-safe slice of the registry's total state, acting as an independent, parallel
// execution unit. It provides a read-optimized view for a `controller.FlowController` worker, partitioning the overall
// system state to enable horizontal scalability.
//
// # Concurrency Model: Hybrid Lock/Lock-Free
//
// The registryShard optimizes for the hot path (request processing) while ensuring safety for the cold path (dynamic
// provisioning):
//
//   - sync.Map (Hot Path): The priorityBands map holds the active queues. It allows lock-free lookups during every
//     enqueue/dequeue operation and safe concurrent writes when new priority bands are dynamically provisioned.
//   - sync.RWMutex (Cold Path): Protects the shard's configuration state (`config`) and the orderedPriorityLevels
//     slice. These are read frequently by administrative processes (like stats scraping) but modified rarely (only
//     during scaling or dynamic provisioning).
//   - Atomics: Aggregated statistics and lifecycle flags use atomic operations for zero-contention updates.
type registryShard struct {
	// --- Immutable Identity & Dependencies (set at construction) ---
	id           string
	logger       logr.Logger
	onStatsDelta propagateStatsDeltaFunc

	// --- Configuration State (Protected by `mu`) ---

	// mu protects the shard's configuration and ordered topology lists.
	mu sync.RWMutex

	// config holds the partitioned configuration for this shard.
	config *ShardConfig

	// orderedPriorityLevels is a sorted list of active priority levels.
	// It is updated dynamically when new bands are provisioned.
	orderedPriorityLevels atomic.Pointer[[]int]

	// --- Operational State (Concurrent-Safe / Lock-Free) ---

	// priorityBands is the primary container for all managed queues on this shard.
	// We use sync.Map to allow lock-free lookups on the hot path (Stats/Propagation) while enabling safe dynamic addition
	// of new priority bands.
	// Key: int (priority), Value: *priorityBand
	priorityBands sync.Map

	// isDraining indicates if the shard is gracefully shutting down.
	isDraining atomic.Bool

	// Shard-level statistics, updated via lock-free propagation from child queues.
	totalByteSize atomic.Int64
	totalLen      atomic.Int64
}

var _ contracts.RegistryShard = &registryShard{}

// newShard creates a new `registryShard` instance from a partitioned configuration.
func newShard(
	id string,
	config *ShardConfig,
	logger logr.Logger,
	onStatsDelta propagateStatsDeltaFunc,
) *registryShard {
	shardLogger := logger.WithName("registry-shard").WithValues("shardID", id)
	s := &registryShard{
		id:           id,
		logger:       shardLogger,
		config:       config,
		onStatsDelta: onStatsDelta,
	}
	emptyPrios := make([]int, 0)
	s.orderedPriorityLevels.Store(&emptyPrios)

	for _, bandConfig := range config.PriorityBands {
		s.initPriorityBand(bandConfig)
	}

	s.logger.V(logging.DEFAULT).Info("Registry shard initialized successfully",
		"orderedPriorities", *s.orderedPriorityLevels.Load())
	return s
}

// initPriorityBand constructs the runtime state for a single priority level and registers it within the shard.
// This is used by both newShard (initialization) and addPriorityBand (dynamic provisioning).
// The caller MUST hold s.mu (Write Lock) as this method modifies the orderedPriorityLevels slice.
func (s *registryShard) initPriorityBand(bandConfig *PriorityBandConfig) {
	policyState := bandConfig.FairnessPolicy.NewState(context.Background())
	band := &priorityBand{
		config:         *bandConfig,
		priority:       bandConfig.Priority,
		priorityName:   bandConfig.PriorityName,
		fairnessPolicy: bandConfig.FairnessPolicy,
		policyState:    policyState,
	}
	s.priorityBands.Store(bandConfig.Priority, band)

	// Copy-on-write update of orderedPriorityLevels
	var currentLevels []int
	if ptr := s.orderedPriorityLevels.Load(); ptr != nil {
		currentLevels = *ptr
	}

	newLevels := make([]int, len(currentLevels), len(currentLevels)+1)
	copy(newLevels, currentLevels)
	newLevels = append(newLevels, bandConfig.Priority)

	sort.Slice(newLevels, func(i, j int) bool {
		return newLevels[i] > newLevels[j]
	})

	s.orderedPriorityLevels.Store(&newLevels)
}

// addPriorityBand dynamically provisions a new priority band on this shard.
// It looks up the definition in s.config, which must have been updated by the Registry via updateConfig/repartition.
func (s *registryShard) addPriorityBand(priority int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotency check.
	if _, ok := s.priorityBands.Load(priority); ok {
		return
	}

	bandConfig := s.config.PriorityBands[priority]
	s.initPriorityBand(bandConfig)
	s.logger.Info("Dynamically added priority band", "priority", priority)
}

// deletePriorityBand removes a priority band from this shard.
// This method should only be called by FlowRegistry.deletePriorityBand with FlowRegistry.mu held.
func (s *registryShard) deletePriorityBand(priority int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove from sync.Map
	s.priorityBands.Delete(priority)

	// Remove from config
	delete(s.config.PriorityBands, priority)

	// Remove from ordered list (Copy-on-write)
	if ptr := s.orderedPriorityLevels.Load(); ptr != nil {
		currentLevels := *ptr
		var newLevels []int
		for _, p := range currentLevels {
			if p != priority {
				newLevels = append(newLevels, p)
			}
		}
		s.orderedPriorityLevels.Store(&newLevels)
	}

	s.logger.V(logging.DEBUG).Info("Removed priority band from shard", "priority", priority)
}

// ID returns the unique identifier for this shard.
func (s *registryShard) ID() string { return s.id }

// IsActive returns true if the shard is active and accepting new requests.
// This is a lock-free read, making it efficient for the hot path.
func (s *registryShard) IsActive() bool {
	return !s.isDraining.Load()
}

// ManagedQueue retrieves a specific `contracts.ManagedQueue` instance from this shard.
func (s *registryShard) ManagedQueue(key flowcontrol.FlowKey) (contracts.ManagedQueue, error) {
	val, ok := s.priorityBands.Load(key.Priority)
	if !ok {
		return nil, fmt.Errorf("failed to get managed queue for flow %q: %w", key, contracts.ErrPriorityBandNotFound)
	}
	band := val.(*priorityBand)

	mqVal, ok := band.queues.Load(key.ID)
	if !ok {
		return nil, fmt.Errorf("failed to get managed queue for flow %q: %w", key, contracts.ErrFlowInstanceNotFound)
	}
	return mqVal.(*managedQueue), nil
}

// FairnessPolicy retrieves a priority band's configured FairnessPolicy.
// This read is lock-free as the policy instance is immutable after the shard is initialized.
func (s *registryShard) FairnessPolicy(priority int) (flowcontrol.FairnessPolicy, error) {
	val, ok := s.priorityBands.Load(priority)
	if !ok {
		return nil, fmt.Errorf("failed to get fairness policy for priority %d: %w",
			priority, contracts.ErrPriorityBandNotFound)
	}
	return val.(*priorityBand).fairnessPolicy, nil
}

// PriorityBandState retrieves the state and iterator required by a FairnessPolicy.
// This accessor provides the state of all contending flows within the band (as seen by this shard) and serves as the
// primary input for FairnessPolicy execution.
func (s *registryShard) PriorityBandState(priority int) (any, iter.Seq[flowcontrol.FlowQueueAccessor], error) {
	val, ok := s.priorityBands.Load(priority)
	if !ok {
		return nil, nil, fmt.Errorf("failed to get state for priority band %d: %w", priority, contracts.ErrPriorityBandNotFound)
	}
	band := val.(*priorityBand)

	queues := func(yield func(flowcontrol.FlowQueueAccessor) bool) {
		band.queues.Range(func(key, value any) bool {
			mq := value.(*managedQueue)
			return yield(mq.FlowQueueAccessor())
		})
	}

	return band.policyState, queues, nil
}

// AllOrderedPriorityLevels returns a cached, sorted slice of all configured priority levels for this shard.
// This is a lock-free read.
func (s *registryShard) AllOrderedPriorityLevels() iter.Seq[int] {
	ptr := s.orderedPriorityLevels.Load()
	if ptr == nil {
		// Return an empty sequence if not fully initialized
		return func(yield func(int) bool) {}
	}
	return slices.Values(*ptr)
}

// HasCapacity checks if the shard has enough capacity to admit a new item of the specified size at the given
// priority level. This validates both the global shard limit and the per-band limit in a lock-free manner.
func (s *registryShard) HasCapacity(priority int, itemByteSize uint64) bool {
	// Check global shard capacity if configured (0 means no limit).
	if s.config.MaxBytes > 0 {
		if uint64(s.totalByteSize.Load())+itemByteSize > s.config.MaxBytes {
			return false
		}
	}

	// Check per-band capacity. We read the band directly from the sync.Map.
	val, ok := s.priorityBands.Load(priority)
	if !ok {
		// If the band doesn't exist, we can't admit the item to it.
		return false
	}
	band := val.(*priorityBand)

	return uint64(band.byteSize.Load())+itemByteSize <= band.config.MaxBytes
}

//  --- Internal Administrative/Lifecycle Methods ---

// synchronizeFlow is the internal administrative method for creating a flow instance on this shard.
// It is an idempotent "create if not exists" operation.
func (s *registryShard) synchronizeFlow(
	key flowcontrol.FlowKey,
	policy flowcontrol.OrderingPolicy,
	q contracts.SafeQueue,
) {
	val, _ := s.priorityBands.Load(key.Priority)
	band := val.(*priorityBand)
	if _, ok := band.queues.Load(key.ID); ok {
		return // Fast path: queue already exists
	}

	// We don't hold s.mu.Lock() anymore because both priorityBands and queues are sync.Map
	// However, we need to ensure exactly-once initialization of the queue state if multiple goroutines race
	// to synchronize the same flow. We use LoadOrStore for this.

	// Create a closure that captures the shard's `isDraining` atomic field.
	isDrainingFunc := func() bool {
		return s.isDraining.Load()
	}

	mq := newManagedQueue(q, policy, key, s.logger, s.propagateStatsDelta, isDrainingFunc)

	if actual, loaded := band.queues.LoadOrStore(key.ID, mq); loaded {
		// Another goroutine beat us to it.
		// The newly instantiated `mq` will be garbage collected.
		_ = actual
		return
	}

	s.logger.V(logging.TRACE).Info("Created new queue for flow instance.",
		"flowKey", key, "queueType", q.Name())
}

// deleteFlow removes a queue instance from the shard and drains it.
func (s *registryShard) deleteFlow(key flowcontrol.FlowKey) {
	s.logger.Info("Deleting queue instance.", "flowKey", key)
	if val, ok := s.priorityBands.Load(key.Priority); ok {
		band := val.(*priorityBand)
		if mqVal, ok := band.queues.LoadAndDelete(key.ID); ok {
			mqVal.(contracts.ManagedQueue).Drain()
		}
	}
}

// markAsDraining transitions the shard to a Draining state. This method is lock-free.
func (s *registryShard) markAsDraining() {
	s.isDraining.Store(true)
	s.logger.V(logging.DEBUG).Info("Shard marked as Draining")
}

// updateConfig atomically replaces the shard's configuration. This is used during scaling events to re-partition
// capacity allocations.
func (s *registryShard) updateConfig(newConfig *ShardConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.config = newConfig
	s.priorityBands.Range(func(key, value any) bool {
		priority := key.(int)
		band := value.(*priorityBand)
		newBandConfig := newConfig.PriorityBands[priority]
		band.config = *newBandConfig
		band.priority = newBandConfig.Priority
		band.priorityName = newBandConfig.PriorityName
		return true
	})
	s.logger.Info("Shard configuration updated")
}

// --- Internal Callback ---

// propagateStatsDelta is the single point of entry for all statistics changes within the shard.
// It atomically updates the relevant band's stats, the shard's total stats, and propagates the delta to the parent
// registry.
func (s *registryShard) propagateStatsDelta(priority int, lenDelta, byteSizeDelta int64) {
	val, _ := s.priorityBands.Load(priority)
	band := val.(*priorityBand)
	band.len.Add(lenDelta)
	band.byteSize.Add(byteSizeDelta)
	s.totalLen.Add(lenDelta)
	s.totalByteSize.Add(byteSizeDelta)

	// Propagate the delta up to the parent registry. This propagation is lock-free and eventually consistent.
	s.onStatsDelta(priority, lenDelta, byteSizeDelta)
}
