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

package intraflowdispatch

import (
	"fmt"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

type RegisteredIntraFlowDispatchPolicyName string

type PriorityScoreType string

// Common priority score types shared between policies.
// Policy-specific priority score types should be declared in their respective implementation files.
const (
	// EnqueueTimePriorityScoreType is a common priority score type for FCFS queues.
	EnqueueTimePriorityScoreType PriorityScoreType = "enqueue_time_ns"
)

type IntraFlowDispatchPolicyConstructor func() (types.IntraFlowDispatchPolicy, error)

var (
	// mu guards the registration maps.
	mu sync.RWMutex
	// registeredInterFlowDispatchPolicies stores the constructors for all registered intra-flow dispatch policies.
	registeredIntraFlowDispatchPolicies = make(map[RegisteredIntraFlowDispatchPolicyName]IntraFlowDispatchPolicyConstructor)
)

// RegisterPolicy registers a new intra-flow dispatch policy constructor.
// This function is called by policy implementations in their init() function.
func RegisterPolicy(name RegisteredIntraFlowDispatchPolicyName, constructor IntraFlowDispatchPolicyConstructor) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registeredIntraFlowDispatchPolicies[name]; ok {
		panic(fmt.Sprintf("IntraFlowDispatchPolicy named %s already registered", name))
	}
	registeredIntraFlowDispatchPolicies[name] = constructor
}

// NewPolicyFromName creates a new IntraFlowDispatchPolicy given its registered name.
// This is called by the FlowRegistry during initialization.
// It can be extended to pass configuration to the constructor if policies become configurable.
func NewPolicyFromName(name RegisteredIntraFlowDispatchPolicyName) (types.IntraFlowDispatchPolicy, error) {
	mu.RLock()
	defer mu.RUnlock()
	constructor, ok := registeredIntraFlowDispatchPolicies[name]
	if !ok {
		return nil, fmt.Errorf("no IntraFlowDispatchPolicy registered with name %q", name)
	}
	return constructor()
}
