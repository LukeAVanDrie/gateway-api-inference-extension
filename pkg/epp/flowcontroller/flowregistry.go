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
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/config"
	interd "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/dispatch/interflow"
	intrad "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/dispatch/intraflow"
	interp "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/preemption/interflow"
	intrap "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/preemption/intraflow"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/plugins/queue"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

// flowInstance represents a specific instance of a flow within a priority band.
type flowInstance struct {
	spec types.FlowSpecification // The specification that led to this instance's creation/activation
	id   string                  // Cached from spec.ID() for convenience

	queue                     types.ManagedQueue
	intraFlowDispatchPolicy   types.IntraFlowDispatchPolicy
	intraFlowPreemptionPolicy types.IntraFlowPreemptionPolicy

	instanceMu   sync.Mutex
	isActive     bool // Is this the instance to which new requests for this FlowID are routed?
	isRegistered bool // Is the flow considered registered? (False if UnregisterFlow called)

	priority uint
}

// priorityBandState holds the state for a single priority band within the FlowRegistry.
type priorityBandState struct {
	priorityLevel uint
	config        config.PriorityBandConfig

	interFlowDispatchPolicy   types.InterFlowDispatchPolicy
	interFlowPreemptionPolicy types.InterFlowPreemptionPolicy
	accessor                  types.PriorityBandAccessor // Cached accessor for this band

	// Band aggregated stats
	bandByteSize atomic.Uint64 // Total byte size of all items in this band's queues
	bandLen      atomic.Uint64 // Total item count in this band's queues

	flowInstances map[string]*flowInstance
}

// FlowRegistry manages the lifecycle of flows, their queues, and associated policies.
type FlowRegistry struct {
	mu sync.RWMutex

	config config.FlowRegistryConfig
	logger logr.Logger

	priorityBands       map[uint]*priorityBandState
	activeFlowInstances map[string]*flowInstance
	allFlowInstances    map[string]map[uint]*flowInstance

	// Global aggregated stats
	globalByteSize atomic.Uint64 // Total byte size of all items queued items
	globalLen      atomic.Uint64 // Total queued items count
}

var _ types.FlowRegistry = &FlowRegistry{} // Compile-time validation

// NewFlowRegistry creates and initializes a new FlowRegistry.
func NewFlowRegistry(cfg config.FlowRegistryConfig, logger logr.Logger) (*FlowRegistry, error) {
	logger = logger.WithName("flow-registry")
	cfgCopy := cfg // Operate on a copy
	// Validate and apply defaults to the config.FlowRegistryConfig (which cascades to config.PriorityBandConfigs)
	if err := (&cfgCopy).ValidateAndApplyDefaults(logger.WithName("registry-config")); err != nil {
		return nil, fmt.Errorf("invalid config.FlowRegistryConfig: %w", err)
	}

	fr := &FlowRegistry{
		config:              cfgCopy,
		logger:              logger,
		priorityBands:       make(map[uint]*priorityBandState),
		activeFlowInstances: make(map[string]*flowInstance),
		allFlowInstances:    make(map[string]map[uint]*flowInstance),
	}

	if len(fr.config.PriorityBands) == 0 {
		logger.V(logutil.VERBOSE).Info("No priority bands defined in config; FlowRegistry will be empty initially.")
	}

	// First, create all priorityBandState objects.
	// The bandCfgs here have already had defaults applied by fr.config.validateAndApplyDefaults above.
	for _, bandCfg := range fr.config.PriorityBands {
		currentBandCfg := bandCfg
		priorityVal := currentBandCfg.Priority
		if _, ok := fr.priorityBands[priorityVal]; ok {
			return nil, fmt.Errorf("duplicate priority band level configured: %d", priorityVal)
		}
		fr.priorityBands[priorityVal] = &priorityBandState{
			priorityLevel: priorityVal,
			config:        currentBandCfg,
			flowInstances: make(map[string]*flowInstance),
			accessor:      fr.newInternalBandStateAccessor(priorityVal),
		}
	}

	// Then, iterate again to create accessors and instantiate inter-flow policies.
	for priorityVal, pbs := range fr.priorityBands {
		var err error
		pbs.interFlowDispatchPolicy, err = interd.NewPolicyFromName(pbs.config.InterFlowDispatchPolicy)
		if err != nil {
			return nil, fmt.Errorf("failed to create inter-flow dispatch policy '%s' for band %d (%s): %w", pbs.config.InterFlowDispatchPolicy, priorityVal, pbs.config.PriorityName, err)
		}
		pbs.interFlowPreemptionPolicy, err = interp.NewPolicyFromName(pbs.config.InterFlowPreemptionPolicy)
		if err != nil {
			return nil, fmt.Errorf("failed to create inter-flow preemption policy '%s' for band %d (%s): %w", pbs.config.InterFlowPreemptionPolicy, priorityVal, pbs.config.PriorityName, err)
		}
		logger.V(logutil.VERBOSE).Info("Initialized priority band", "priority", priorityVal, "priorityName", pbs.config.PriorityName)
	}
	return fr, nil
}

// RegisterOrUpdateFlow handles registration or update of a flow.
func (fr *FlowRegistry) RegisterOrUpdateFlow(spec types.FlowSpecification) error {
	fr.mu.Lock()
	defer fr.mu.Unlock()

	flowID := spec.ID()
	if flowID == "" {
		return fmt.Errorf("%w: flow ID in spec is empty", types.ErrFlowIDEmpty)
	}

	targetPriority := spec.Priority()
	targetPriorityName := fr.getPriorityNameFromConfig(targetPriority)
	flowLogger := fr.logger.WithValues("operation", "RegisterOrUpdateFlow", "flowID", flowID,
		"priority", targetPriority, "priorityName", targetPriorityName)

	if _, ok := fr.allFlowInstances[flowID]; !ok {
		fr.allFlowInstances[flowID] = make(map[uint]*flowInstance)
	}

	if _, ok := fr.priorityBands[targetPriority]; !ok {
		return fmt.Errorf("%w: priority %d specified in flow spec '%s' is not a configured priority band", types.ErrInvalidFlowPriority, targetPriority, flowID)
	}

	currentActiveInstance, isCurrentlyActive := fr.activeFlowInstances[flowID]
	if !isCurrentlyActive {
		flowLogger.V(logutil.DEFAULT).Info("Flow not currently registered, proceeding with registration and activation")
		return fr.activateOrCreateInstanceInBand(spec, targetPriority, flowLogger)
	}

	currentPriority := currentActiveInstance.priority
	currentActiveInstance.instanceMu.Lock()
	currentPriorityName := fr.getPriorityNameFromConfig(currentPriority)

	updateFlowLogger := flowLogger.WithValues("oldPriority", currentPriority, "oldPriorityName", currentPriorityName)

	if currentPriority == targetPriority {
		updateFlowLogger.V(logutil.DEFAULT).Info("Priority unchanged, updating spec on active instance")
		currentActiveInstance.spec = spec
		currentActiveInstance.isRegistered = true
		currentActiveInstance.instanceMu.Unlock()
		return nil
	}

	updateFlowLogger.V(logutil.DEFAULT).Info("Priority changed, performing live migration")
	currentActiveInstance.isActive = false
	currentActiveInstance.isRegistered = true // Still registered, just draining
	currentActiveInstance.instanceMu.Unlock()
	delete(fr.activeFlowInstances, flowID)

	// Attempt to cleanup the instance that was just made inactive, if it's already empty.
	// This is a targeted, immediate cleanup attempt for the specific instance.
	fr.tryCleanupInstance(currentActiveInstance.id, currentActiveInstance.priority, updateFlowLogger)

	// Re-ensure the map for flowID exists in allFlowInstances, as tryCleanupInstance might have removed it if the
	// cleaned instance was the last one for that flowID.
	if _, ok := fr.allFlowInstances[flowID]; !ok {
		fr.allFlowInstances[flowID] = make(map[uint]*flowInstance)
	}
	return fr.activateOrCreateInstanceInBand(spec, targetPriority, flowLogger)
}

// activateOrCreateInstanceInBand internal helper. Assumes fr.mu is write-locked.
func (fr *FlowRegistry) activateOrCreateInstanceInBand(spec types.FlowSpecification, targetPriority uint, logger logr.Logger) error {
	flowID := spec.ID()
	bandState, ok := fr.priorityBands[targetPriority]
	if !ok {
		return fmt.Errorf("%w: target priority band %d for flow %s not configured", types.ErrPriorityBandNotFound, targetPriority, flowID)
	}

	var newActiveInstance *flowInstance
	if existingInstanceInTargetBand, ok := fr.allFlowInstances[flowID][targetPriority]; ok {
		logger.V(logutil.DEFAULT).Info("Re-activating existing instance in target band")
		existingInstanceInTargetBand.instanceMu.Lock()
		existingInstanceInTargetBand.spec = spec
		existingInstanceInTargetBand.isActive = true
		existingInstanceInTargetBand.isRegistered = true
		existingInstanceInTargetBand.instanceMu.Unlock()
		newActiveInstance = existingInstanceInTargetBand
	} else {
		logger.V(logutil.DEFAULT).Info("Creating new instance in target band")
		createdInstance, err := fr.createFlowInstance(spec, bandState)
		if err != nil {
			return fmt.Errorf("failed to create flow instance in band %d: %w", targetPriority, err)
		}
		createdInstance.isActive = true // isRegistered is set to true in createFlowInstance
		bandState.flowInstances[flowID] = createdInstance
		fr.allFlowInstances[flowID][targetPriority] = createdInstance
		initialQueueSize := createdInstance.queue.ByteSize()
		initialQueueLen := uint64(createdInstance.queue.Len())
		bandState.bandByteSize.Add(initialQueueSize)
		bandState.bandLen.Add(initialQueueLen)
		fr.globalByteSize.Add(initialQueueSize)
		fr.globalLen.Add(initialQueueLen)
		newActiveInstance = createdInstance
	}

	fr.activeFlowInstances[flowID] = newActiveInstance
	logger.V(logutil.DEFAULT).Info("Flow instance is now active in band")
	return nil
}

// UnregisterFlow marks a flow as inactive and eligible for cleanup once its queues are empty.
func (fr *FlowRegistry) UnregisterFlow(flowID string) error {
	fr.mu.Lock()
	defer fr.mu.Unlock()

	if flowID == "" {
		return fmt.Errorf("%w: flow ID for unregistration is empty", types.ErrFlowIDEmpty)
	}

	flowLogger := fr.logger.WithValues("operation", "UnregisterFlow", "flowID", flowID)
	foundAndModified := false
	var instancesToCheckForCleanup []*flowInstance

	if flowPriorityMap, ok := fr.allFlowInstances[flowID]; ok {
		for _, instance := range flowPriorityMap {
			instance.instanceMu.Lock()
			if instance.isRegistered || instance.isActive {
				instancePriorityName := fr.getPriorityNameFromConfig(instance.priority)
				logger := flowLogger.WithValues("priority", instance.priority, "priorityName", instancePriorityName)
				logger.V(logutil.DEFAULT).Info("Marking instance as unregistered and inactive")
				instance.isRegistered = false
				instance.isActive = false
				foundAndModified = true
				instancesToCheckForCleanup = append(instancesToCheckForCleanup, instance)
			}
			instance.instanceMu.Unlock()
		}
	}

	if _, isActive := fr.activeFlowInstances[flowID]; isActive {
		delete(fr.activeFlowInstances, flowID)
		foundAndModified = true
	}

	if !foundAndModified {
		return fmt.Errorf("%w: flow %s not found or already fully unregistered/inactive", types.ErrFlowNotRegistered, flowID)
	}

	// Attempt immediate cleanup for instances that were just modified and might be empty.
	for _, instance := range instancesToCheckForCleanup {
		fr.tryCleanupInstance(instance.id, instance.priority, flowLogger)
	}

	flowLogger.V(logutil.DEFAULT).Info("Flow marked as unregistered; all its instances are now inactive and will drain")
	return nil
}

// createFlowInstance internal helper. Assumes fr.mu is write-locked.
func (fr *FlowRegistry) createFlowInstance(spec types.FlowSpecification, bandState *priorityBandState) (*flowInstance, error) {
	intraFlowDispatchPolicy, err := intrad.NewPolicyFromName(bandState.config.IntraFlowDispatchPolicy)
	if err != nil {
		return nil, fmt.Errorf("failed to create intra-flow dispatch policy '%s' for band %d (%s): %w",
			bandState.config.IntraFlowDispatchPolicy, bandState.priorityLevel, bandState.config.PriorityName, err)
	}
	intraFlowPreemptionPolicy, err := intrap.NewPolicyFromName(bandState.config.IntraFlowPreemptionPolicy)
	if err != nil {
		return nil, fmt.Errorf("failed to create intra-flow preemption policy '%s' for band %d (%s): %w",
			bandState.config.IntraFlowPreemptionPolicy, bandState.priorityLevel, bandState.config.PriorityName, err)
	}

	itemComparator := intraFlowDispatchPolicy.Comparator()
	safeQ, err := queue.NewQueueFromName(bandState.config.QueueType, itemComparator)
	if err != nil {
		return nil, fmt.Errorf("failed to create queue '%s' for band %d (%s): %w",
			bandState.config.QueueType, bandState.priorityLevel, bandState.config.PriorityName, err)
	}

	queueCapabilitiesMap := make(map[types.QueueCapability]bool)
	for _, capability := range safeQ.Capabilities() {
		queueCapabilitiesMap[capability] = true
	}

	var missingDispatchCapabilities []types.QueueCapability
	for _, requiredCapability := range intraFlowDispatchPolicy.RequiredQueueCapabilities() {
		if !queueCapabilitiesMap[requiredCapability] {
			missingDispatchCapabilities = append(missingDispatchCapabilities, requiredCapability)
		}
	}
	var missingPreemptionCapabilities []types.QueueCapability
	for _, requiredCapability := range intraFlowPreemptionPolicy.RequiredQueueCapabilities() {
		if !queueCapabilitiesMap[requiredCapability] {
			missingPreemptionCapabilities = append(missingPreemptionCapabilities, requiredCapability)
		}
	}

	if len(missingDispatchCapabilities) > 0 || len(missingPreemptionCapabilities) > 0 {
		return nil, fmt.Errorf("queue '%s' is missing capabilities. For dispatch policy '%s': %v. For preemption policy '%s': %v",
			bandState.config.QueueType,
			bandState.config.IntraFlowDispatchPolicy, missingDispatchCapabilities,
			bandState.config.IntraFlowPreemptionPolicy, missingPreemptionCapabilities)
	}

	managedQ := newManagedQueueWrapper(safeQ, fr, spec, intraFlowDispatchPolicy.Comparator())

	return &flowInstance{
		spec:                      spec,
		id:                        spec.ID(),
		queue:                     managedQ,
		intraFlowDispatchPolicy:   intraFlowDispatchPolicy,
		intraFlowPreemptionPolicy: intraFlowPreemptionPolicy,
		priority:                  bandState.priorityLevel,
		isRegistered:              true, // Created instances start as registered
	}, nil
}

// ActiveManagedQueue returns the queue of the currently active instance for the given flowID.
func (fr *FlowRegistry) ActiveManagedQueue(flowID string) (types.ManagedQueue, error) {
	fr.mu.RLock()
	defer fr.mu.RUnlock()

	instance, ok := fr.activeFlowInstances[flowID]
	if !ok {
		return nil, fmt.Errorf("%w: no active instance for flow %s", types.ErrFlowNotRegistered, flowID)
	}

	instance.instanceMu.Lock()
	defer instance.instanceMu.Unlock()
	instancePriorityName := fr.getPriorityNameFromConfig(instance.priority)
	if !instance.isActive || !instance.isRegistered {
		errMsg := fmt.Sprintf("invariant violation: flow instance %s (priority %d, name %s) in activeFlowInstances map is not active/registered (isActive: %t, isRegistered: %t)",
			flowID, instance.priority, instancePriorityName, instance.isActive, instance.isRegistered)
		fr.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error in ActiveManagedQueue")
		panic(errMsg)
	}
	return instance.queue, nil
}

// ManagedQueue retrieves a specific flow instance's queue, regardless of its active status.
func (fr *FlowRegistry) ManagedQueue(flowID string, priority uint) (types.ManagedQueue, error) {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	if priorityMap, ok := fr.allFlowInstances[flowID]; ok {
		if instance, ok2 := priorityMap[priority]; ok2 {
			if instance.queue == nil {
				errMsg := fmt.Sprintf("invariant violation: flow instance %s at priority %d found but its queue is nil",
					flowID, priority)
				fr.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error in ManagedQueue")
				panic(errMsg)
			}
			return instance.queue, nil
		}
	}
	return nil, fmt.Errorf("%w: for flow %s at priority %d", types.ErrFlowInstanceNotFound, flowID, priority)
}

// IntraFlowDispatchPolicy retrieves a specific flow instance's intra-flow dispatch policy.
func (fr *FlowRegistry) IntraFlowDispatchPolicy(flowID string, priority uint) (types.IntraFlowDispatchPolicy, error) {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	if priorityMap, ok := fr.allFlowInstances[flowID]; ok {
		if instance, ok2 := priorityMap[priority]; ok2 {
			if instance.intraFlowDispatchPolicy == nil {
				errMsg := fmt.Sprintf("invariant violation: flow instance %s at priority %d found but IntraFlowDispatchPolicy is nil",
					flowID, priority)
				fr.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
				panic(errMsg)
			}
			return instance.intraFlowDispatchPolicy, nil
		}
	}
	return nil, fmt.Errorf("%w: for flow %s at priority %d (dispatch policy)", types.ErrFlowInstanceNotFound, flowID, priority)
}

// IntraFlowPreemptionPolicy retrieves a specific flow instance's intra-flow preemption policy.
func (fr *FlowRegistry) IntraFlowPreemptionPolicy(flowID string, priority uint) (types.IntraFlowPreemptionPolicy, error) {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	if priorityMap, ok := fr.allFlowInstances[flowID]; ok {
		if instance, ok2 := priorityMap[priority]; ok2 {
			if instance.intraFlowPreemptionPolicy == nil {
				errMsg := fmt.Sprintf("invariant violation: flow instance %s at priority %d found but IntraFlowPreemptionPolicy is nil",
					flowID, priority)
				fr.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
				panic(errMsg)
			}
			return instance.intraFlowPreemptionPolicy, nil
		}
	}
	return nil, fmt.Errorf("%w: for flow %s at priority %d (preemption policy)", types.ErrFlowInstanceNotFound, flowID, priority)
}

// InterFlowDispatchPolicy retrieves a priority band's inter-flow dispatch policy.
func (fr *FlowRegistry) InterFlowDispatchPolicy(priority uint) (types.InterFlowDispatchPolicy, error) {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	if bandState, ok := fr.priorityBands[priority]; ok {
		if bandState.interFlowDispatchPolicy == nil {
			errMsg := fmt.Sprintf("invariant violation: priority band %d found but InterFlowDispatchPolicy is nil", priority)
			fr.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
			panic(errMsg)
		}
		return bandState.interFlowDispatchPolicy, nil
	}
	return nil, fmt.Errorf("%w: level %d (dispatch policy)", types.ErrPriorityBandNotFound, priority)
}

// InterFlowPreemptionPolicy retrieves a priority band's inter-flow preemption policy.
func (fr *FlowRegistry) InterFlowPreemptionPolicy(priority uint) (types.InterFlowPreemptionPolicy, error) {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	if bandState, ok := fr.priorityBands[priority]; ok {
		if bandState.interFlowPreemptionPolicy == nil {
			errMsg := fmt.Sprintf("invariant violation: priority band %d found but InterFlowPreemptionPolicy is nil", priority)
			fr.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
			panic(errMsg)
		}
		return bandState.interFlowPreemptionPolicy, nil
	}
	return nil, fmt.Errorf("%w: level %d (preemption policy)", types.ErrPriorityBandNotFound, priority)
}

// PriorityBandAccessor retrieves a types.PriorityBandAccessor for a given priority level.
func (fr *FlowRegistry) PriorityBandAccessor(priority uint) (types.PriorityBandAccessor, error) {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	bandState, ok := fr.priorityBands[priority]
	if !ok {
		return nil, fmt.Errorf("%w: level %d (accessor)", types.ErrPriorityBandNotFound, priority)
	}
	if bandState.accessor == nil {
		errMsg := fmt.Sprintf("invariant violation: priority band %d found but its accessor is nil", priority)
		fr.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
		panic(errMsg)
	}
	return bandState.accessor, nil
}

// AllOrderedPriorityLevels returns configured priority levels in sorted order (highest to lowest priority where lowest
// numeric value means highest priority).
func (fr *FlowRegistry) AllOrderedPriorityLevels() []uint {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	levels := make([]uint, 0, len(fr.priorityBands))
	for level := range fr.priorityBands {
		levels = append(levels, level)
	}
	// Sort ascending for uint (lower value = higher priority).
	sort.Slice(levels, func(i, j int) bool { return levels[i] < levels[j] })
	return levels
}

// doesFlowInstanceExist checks if a flow instance for the given flowID and priority still exists in the registry
// (i.e., it has not been fully cleaned up).
// This is used by ManagedQueue wrappers to ensure they are not operating on a stale instance.
func (fr *FlowRegistry) doesFlowInstanceExist(flowID string, priority uint) bool {
	fr.mu.RLock()
	defer fr.mu.RUnlock()

	if priorityMap, flowExists := fr.allFlowInstances[flowID]; flowExists {
		if _, instanceExists := priorityMap[priority]; instanceExists {
			return true
		}
	}
	fr.logger.V(logutil.DEBUG).Info("Flow instance does not exist in registry", "flowID", flowID, "priority", priority)
	return false
}

// tryCleanupInstance attempts to clean up a specific instance if it's eligible (unregistered, inactive, empty).
// It assumes fr.mu is write-locked.
// Returns true if cleanup occurred.
func (fr *FlowRegistry) tryCleanupInstance(flowID string, priority uint, logger logr.Logger) bool {
	// Cannot use fr.doesFlowInstanceExist here because we already have the write lock.
	priorityMap, flowExists := fr.allFlowInstances[flowID]
	if !flowExists {
		return false
	}
	instance, instanceExists := priorityMap[priority]
	if !instanceExists {
		return false
	}

	instance.instanceMu.Lock()
	isReg := instance.isRegistered
	isAct := instance.isActive
	instance.instanceMu.Unlock()

	if !isReg || !isAct { // Not registered OR not the active instance for this FlowID
		if instance.queue != nil && instance.queue.Len() == 0 {
			logger.V(logutil.DEFAULT).Info("Cleaning up eligible flow instance (empty, and either unregistered or inactive)",
				"instanceIsRegistered", isReg, "instanceIsActive", isAct, "queueLen", instance.queue.Len())
			if bandState, ok := fr.priorityBands[priority]; ok {
				delete(bandState.flowInstances, flowID)
			}
			delete(priorityMap, priority)
			if len(priorityMap) == 0 {
				delete(fr.allFlowInstances, flowID)
			}
			return true
		}
	}
	return false
}

// signalQueueEmptied is called by a ManagedQueue when its underlying SafeQueue becomes empty.
// This method attempts to clean up the corresponding flow instance if it's inactive or unregistered.
func (fr *FlowRegistry) signalQueueEmptied(flowID string, priority uint) {
	fr.mu.Lock()
	defer fr.mu.Unlock()

	flowLogger := fr.logger.WithName("signalQueueEmptied").WithValues("flowID", flowID, "priority", priority)
	cleanedUp := fr.tryCleanupInstance(flowID, priority, flowLogger)
	if cleanedUp {
		flowLogger.V(logutil.DEFAULT).Info("Successfully cleaned up flow instance after queue emptied signal.")
	} else {
		flowLogger.V(logutil.DEBUG).Info("Flow instance not cleaned up after queue emptied signal (might be active, registered, or already gone).")
	}
}

// GetStats returns aggregated statistics for the FlowRegistry.
func (fr *FlowRegistry) GetStats() types.FlowRegistryStats {
	fr.mu.RLock()
	defer fr.mu.RUnlock()

	stats := types.FlowRegistryStats{
		GlobalByteSize:       fr.globalByteSize.Load(),
		GlobalLen:            fr.globalLen.Load(),
		PerPriorityBandStats: make(map[uint]types.PriorityBandStats),
	}

	for priority, bandState := range fr.priorityBands {
		stats.PerPriorityBandStats[priority] = types.PriorityBandStats{
			PriorityLevel: bandState.priorityLevel,
			PriorityName:  bandState.config.PriorityName,
			ByteSize:      bandState.bandByteSize.Load(),
			Len:           bandState.bandLen.Load(),
		}
	}
	return stats
}

// reconcileStats atomically updates band and global statistics by the given deltas.
func (fr *FlowRegistry) reconcileStats(priority uint, deltaLen int64, deltaByteSize int64) {
	pb, ok := fr.priorityBands[priority]
	if !ok {
		errMsg := fmt.Sprintf("invariant violation: flow instance at priority %d found but priority band not configured",
			priority)
		fr.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error in reconcileStats")
		panic(errMsg)
	}
	// atomic.Add handles positive and negative deltas correctly when cast to uint64.
	// e.g., Add(uint64(-5)) is equivalent to Add(^(uint64(5)-1)).
	pb.bandLen.Add(uint64(deltaLen))
	pb.bandByteSize.Add(uint64(deltaByteSize))
	fr.globalLen.Add(uint64(deltaLen))
	fr.globalByteSize.Add(uint64(deltaByteSize))
}

// getPriorityNameFromConfig is a helper to safely get the canonical priority name.
// Assumes fr.mu might be RLocked or Locked by caller if necessary for bandConfig consistency.
func (fr *FlowRegistry) getPriorityNameFromConfig(priority uint) string {
	if bandState, ok := fr.priorityBands[priority]; ok {
		return bandState.config.PriorityName
	}
	// This should ideally not happen if priorities are validated upstream.
	fr.logger.Error(fmt.Errorf("priority level %d not found in configured bands", priority), "Failed to get priority name")
	return "UnknownPriority"
}

// --- internalBandStateAccessor ---

// internalBandStateAccessor implements types.PriorityBandAccessor.
type internalBandStateAccessor struct {
	registry     *FlowRegistry
	bandPriority uint
}

var _ types.PriorityBandAccessor = &internalBandStateAccessor{} // Compile-time validation

// newInternalBandStateAccessor creates an accessor for inter-flow policies for a specific priority band.
func (fr *FlowRegistry) newInternalBandStateAccessor(level uint) *internalBandStateAccessor {
	return &internalBandStateAccessor{registry: fr, bandPriority: level}
}

func (iba *internalBandStateAccessor) CapacityBytes() uint64 {
	iba.registry.mu.RLock()
	defer iba.registry.mu.RUnlock()
	bandState, ok := iba.registry.priorityBands[iba.bandPriority]
	if !ok {
		errMsg := fmt.Sprintf("invariant violation: internalBandStateAccessor.CapacityBytes() called for non-existent band %d", iba.bandPriority)
		iba.registry.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
		panic(errMsg)
	}
	return bandState.config.MaxBytes
}

func (iba *internalBandStateAccessor) Priority() uint {
	return iba.bandPriority
}

func (iba *internalBandStateAccessor) PriorityName() string {
	iba.registry.mu.RLock()
	defer iba.registry.mu.RUnlock()
	bandState, ok := iba.registry.priorityBands[iba.bandPriority]
	if !ok {
		errMsg := fmt.Sprintf("invariant violation: internalBandStateAccessor.PriorityName() called for non-existent band %d", iba.bandPriority)
		iba.registry.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
		panic(errMsg)
	}
	return bandState.config.PriorityName
}

func (iba *internalBandStateAccessor) FlowIDs() []string {
	iba.registry.mu.RLock()
	defer iba.registry.mu.RUnlock()

	bandState, ok := iba.registry.priorityBands[iba.bandPriority]
	if !ok {
		errMsg := fmt.Sprintf("invariant violation: internalBandStateAccessor.FlowIDs() called for non-existent band %d", iba.bandPriority)
		iba.registry.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
		panic(errMsg)
	}

	ids := make([]string, 0, len(bandState.flowInstances))
	for flowID := range bandState.flowInstances {
		// Include all flows (registered/unregisterd active/draining).
		// This is a crucial part of enabling the flow queues to completely drain.
		ids = append(ids, flowID)
	}
	return ids
}

func (iba *internalBandStateAccessor) Queue(flowID string) types.FlowQueueAccessor {
	iba.registry.mu.RLock()
	defer iba.registry.mu.RUnlock()

	bandState, ok := iba.registry.priorityBands[iba.bandPriority]
	if !ok {
		errMsg := fmt.Sprintf("invariant violation: internalBandStateAccessor.Queue() called for non-existent band %d", iba.bandPriority)
		iba.registry.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
		panic(errMsg)
	}

	instance, ok := bandState.flowInstances[flowID]
	if !ok { // This is not an invariant; a flowID might not be in this specific band.
		return nil
	}

	if instance.queue == nil {
		errMsg := fmt.Sprintf("invariant violation: flow instance %s in band %d has a nil queue", flowID, iba.bandPriority)
		iba.registry.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
		panic(errMsg)
	}

	// It's okay to return queue of an unregistered or draining instance if the policy needs to inspect it.
	// This is a crucial part of enabling the flow queues to completely drain.
	return instance.queue.FlowQueueAccessor()
}

func (iba *internalBandStateAccessor) IterateQueues(callback func(q types.FlowQueueAccessor) (keepIterating bool)) {
	iba.registry.mu.RLock()
	defer iba.registry.mu.RUnlock()

	bandState, ok := iba.registry.priorityBands[iba.bandPriority]
	if !ok {
		errMsg := fmt.Sprintf("invariant violation: internalBandStateAccessor.IterateQueues() called for non-existent band %d", iba.bandPriority)
		iba.registry.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
		panic(errMsg)
	}

	// Iterate through all flow instances in the band (register/unregisterd active/draining).
	// This is a crucial part of enabling the flow queues to completely drain.
	for _, instance := range bandState.flowInstances {
		if instance.queue == nil {
			errMsg := fmt.Sprintf("invariant violation: flow instance %s in band %d has a nil queue during IterateQueues", instance.id, iba.bandPriority)
			iba.registry.logger.Error(fmt.Errorf("%s", errMsg), "Critical internal state error")
			panic(errMsg)
		}
		if !callback(instance.queue.FlowQueueAccessor()) {
			return // Stop iteration if callback returns false
		}
	}
}

// --- managedQueueWrapper ---

// managedQueueWrapper implements types.ManagedQueue.
// It wraps a types.SafeQueue and handles atomic statistics updates with the FlowRegistry.
type managedQueueWrapper struct {
	safeQ      types.SafeQueue
	registry   *FlowRegistry
	flowSpec   types.FlowSpecification
	comparator types.ItemComparator
	byteSize   atomic.Uint64
	len        atomic.Uint64
	logger     logr.Logger
}

var _ types.ManagedQueue = &managedQueueWrapper{} // Compile-time validation

func newManagedQueueWrapper(
	safeQ types.SafeQueue,
	registry *FlowRegistry,
	spec types.FlowSpecification,
	comparator types.ItemComparator,
) *managedQueueWrapper {
	queueLogger := registry.logger.WithName("managed-queue").WithValues(
		"flowID", spec.ID(),
		"priority", spec.Priority(),
		"queueType", safeQ.Name(),
	)
	mqw := &managedQueueWrapper{
		safeQ:      safeQ,
		registry:   registry,
		flowSpec:   spec,
		comparator: comparator,
		logger:     queueLogger,
	}
	mqw.len.Store(uint64(safeQ.Len()))
	mqw.byteSize.Store(safeQ.ByteSize())
	return mqw
}

// FlowQueueAccessor returns a new flowQueueAccessorImpl instance.
func (mqw *managedQueueWrapper) FlowQueueAccessor() types.FlowQueueAccessor {
	return &flowQueueAccessorImpl{
		managedQueue: mqw,
		flowSpec:     mqw.flowSpec,
		comparator:   mqw.comparator,
	}
}

// Add wraps SafeQueue.Add and updates registry statistics.
func (mqw *managedQueueWrapper) Add(item types.QueueItemAccessor) (newLen uint64, newByteSize uint64, err error) {
	if !mqw.registry.doesFlowInstanceExist(mqw.flowSpec.ID(), mqw.flowSpec.Priority()) {
		err := fmt.Errorf("%w: flow instance %s (priority %d) no longer exists in registry",
			types.ErrFlowInstanceNotFound, mqw.flowSpec.ID(), mqw.flowSpec.Priority())
		mqw.logger.Error(err, "Cannot Add item to a non-existent/cleaned-up flow instance.")
		return mqw.len.Load(), mqw.byteSize.Load(), err
	}

	len, byteSize := mqw.len.Load(), mqw.byteSize.Load()
	newLen, newByteSize, err = mqw.safeQ.Add(item)

	deltaLen, deltaByteSize := int64(newLen)-int64(len), int64(newByteSize)-int64(byteSize)
	if err == nil && item != nil {
		if deltaLen != 1 || deltaByteSize != int64(item.ByteSize()) {
			mqw.logger.V(logutil.DEBUG).Info("Inconsistent queue stats after Add",
				"expectedLenDelta", 1, "expectedByteSizeDelta", item.ByteSize(),
				"actualLenDelta", deltaLen, "actualByteSizeDelta", deltaByteSize)
		}
	} else { // Add failed or item was nil
		if deltaLen != 0 || deltaByteSize != 0 {
			mqw.logger.V(logutil.DEBUG).Info("Inconsistent queue stats after failed Add",
				"expectedLenDelta", 0, "expectedByteSizeDelta", 0,
				"actualLenDelta", deltaLen, "actualByteSizeDelta", deltaByteSize)
		}
	}

	mqw.len.Store(newLen)
	mqw.byteSize.Store(newByteSize)
	mqw.registry.reconcileStats(mqw.flowSpec.Priority(), deltaLen, deltaByteSize)
	return newLen, newByteSize, err
}

// Remove wraps SafeQueue.Remove and updates registry statistics.
func (mqw *managedQueueWrapper) Remove(handle types.QueueItemHandle) (removedItem types.QueueItemAccessor, newLen uint64, newByteSize uint64, err error) {
	if !mqw.registry.doesFlowInstanceExist(mqw.flowSpec.ID(), mqw.flowSpec.Priority()) {
		err := fmt.Errorf("%w: flow instance %s (priority %d) no longer exists in registry",
			types.ErrFlowInstanceNotFound, mqw.flowSpec.ID(), mqw.flowSpec.Priority())
		mqw.logger.Error(err, "Cannot Remove item from a non-existent/cleaned-up flow instance.")
		return nil, mqw.len.Load(), mqw.byteSize.Load(), err
	}

	len, byteSize := mqw.len.Load(), mqw.byteSize.Load()
	removedItem, newLen, newByteSize, err = mqw.safeQ.Remove(handle)

	deltaLen, deltaByteSize := int64(newLen)-int64(len), int64(newByteSize)-int64(byteSize)
	if err == nil && removedItem != nil {
		if deltaLen != -1 || deltaByteSize != -int64(removedItem.ByteSize()) {
			mqw.logger.V(logutil.DEBUG).Info("Inconsistent queue stats after Remove",
				"expectedLenDelta", -1, "expectedByteSizeDelta", -int64(removedItem.ByteSize()),
				"actualLenDelta", deltaLen, "actualByteSizeDelta", deltaByteSize)
		}
	} else { // Removal failed or queue was empty
		if deltaLen != 0 || deltaByteSize != 0 {
			mqw.logger.V(logutil.DEBUG).Info("Inconsistent queue stats after failed Remove",
				"expectedLenDelta", 0, "expectedByteSizeDelta", 0,
				"actualLenDelta", deltaLen, "actualByteSizeDelta", deltaByteSize)
		}
	}

	mqw.len.Store(newLen)
	mqw.byteSize.Store(newByteSize)
	mqw.registry.reconcileStats(mqw.flowSpec.Priority(), deltaLen, deltaByteSize)

	if newLen == 0 {
		mqw.registry.signalQueueEmptied(mqw.flowSpec.ID(), mqw.flowSpec.Priority())
	}
	return removedItem, newLen, newByteSize, err
}

// CleanupExpired wraps SafeQueue.CleanupExpired and updates registry statistics.
func (mqw *managedQueueWrapper) CleanupExpired(currentTime time.Time, isItemExpired types.IsItemExpiredFunc) (removedItemsInfo []types.ExpiredItemInfo, err error) {
	len, byteSize := mqw.len.Load(), mqw.byteSize.Load()
	removedItemsInfo, err = mqw.safeQ.CleanupExpired(currentTime, isItemExpired)
	newLen, newByteSize := mqw.safeQ.Len(), mqw.safeQ.ByteSize()

	deltaLen, deltaByteSize := int64(newLen)-int64(len), int64(newByteSize)-int64(byteSize)
	// Skip tracking delta against our expectations here since that would involve iterating through all removed items
	// solely for logging purposes.

	mqw.len.Store(uint64(newLen))
	mqw.byteSize.Store(newByteSize)
	mqw.registry.reconcileStats(mqw.flowSpec.Priority(), deltaLen, deltaByteSize)

	if newLen == 0 {
		mqw.registry.signalQueueEmptied(mqw.flowSpec.ID(), mqw.flowSpec.Priority())
	}
	return removedItemsInfo, err
}

func (mqw *managedQueueWrapper) Len() int {
	return mqw.safeQ.Len()
}

func (mqw *managedQueueWrapper) ByteSize() uint64 {
	return mqw.safeQ.ByteSize()
}

func (mqw *managedQueueWrapper) Name() string {
	return mqw.safeQ.Name()
}

func (mqw *managedQueueWrapper) Capabilities() []types.QueueCapability {
	return mqw.safeQ.Capabilities()
}

func (mqw *managedQueueWrapper) PeekHead() (types.QueueItemAccessor, error) {
	return mqw.safeQ.PeekHead()
}

func (mqw *managedQueueWrapper) PeekTail() (types.QueueItemAccessor, error) {
	return mqw.safeQ.PeekTail()
}

// FlowSpec returns the flow specification associated with this managed queue.
func (mqw *managedQueueWrapper) FlowSpec() types.FlowSpecification {
	// Note: No explicit check for doesFlowInstanceExist here. If the instance is gone, this returns the cached spec.
	// Mutating operations on ManagedQueue *will* check and fail if the instance is gone.
	// This method fulfills its non-nil contract.
	return mqw.flowSpec
}

// --- flowQueueAccessorImpl ---

// flowQueueAccessorImpl implements types.FlowQueueAccessor.
// It provides a read-only view for policies.
type flowQueueAccessorImpl struct {
	managedQueue *managedQueueWrapper // To access underlying SafeQueue's inspection methods
	flowSpec     types.FlowSpecification
	comparator   types.ItemComparator
}

var _ types.FlowQueueAccessor = &flowQueueAccessorImpl{} // Compile-time validation

func (fqa *flowQueueAccessorImpl) Comparator() types.ItemComparator {
	return fqa.comparator
}

func (fqa *flowQueueAccessorImpl) FlowSpec() types.FlowSpecification {
	return fqa.flowSpec
}

func (fqa *flowQueueAccessorImpl) Len() int {
	return fqa.managedQueue.Len()
}

func (fqa *flowQueueAccessorImpl) ByteSize() uint64 {
	return fqa.managedQueue.ByteSize()
}

func (fqa *flowQueueAccessorImpl) Name() string {
	return fqa.managedQueue.Name()
}

func (fqa *flowQueueAccessorImpl) Capabilities() []types.QueueCapability {
	return fqa.managedQueue.Capabilities()
}

func (fqa *flowQueueAccessorImpl) PeekHead() (types.QueueItemAccessor, error) {
	return fqa.managedQueue.PeekHead()
}

func (fqa *flowQueueAccessorImpl) PeekTail() (types.QueueItemAccessor, error) {
	return fqa.managedQueue.PeekTail()
}
