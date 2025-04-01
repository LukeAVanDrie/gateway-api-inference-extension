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
	"fmt"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

type RegisteredIntraFlowPreemptionPolicyName string

type IntraFlowPreemptionPolicyConstructor func() (types.IntraFlowPreemptionPolicy, error)

var (
	// mu guards the registration maps.
	mu sync.RWMutex
	// registeredIntraFlowPreemptionPolicies stores the constructors for all registered intra-flow preemption policies.
	registeredIntraFlowPreemptionPolicies = make(map[RegisteredIntraFlowPreemptionPolicyName]IntraFlowPreemptionPolicyConstructor)
)

// RegisterPolicy registers a new intra-flow preemption policy constructor.
// This function is called by policy implementations in their init() function.
func RegisterPolicy(name RegisteredIntraFlowPreemptionPolicyName, constructor IntraFlowPreemptionPolicyConstructor) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registeredIntraFlowPreemptionPolicies[name]; ok {
		panic(fmt.Sprintf("IntraFlowPreemptionPolicy named %s already registered", name))
	}
	registeredIntraFlowPreemptionPolicies[name] = constructor
}

// NewPolicyFromName creates a new IntraFlowPreemptionPolicy given its registered name.
// This is called by the FlowRegistry during initialization.
// It can be extended to pass configuration to the constructor if policies become configurable.
func NewPolicyFromName(name RegisteredIntraFlowPreemptionPolicyName) (types.IntraFlowPreemptionPolicy, error) {
	mu.RLock()
	defer mu.RUnlock()
	constructor, ok := registeredIntraFlowPreemptionPolicies[name]
	if !ok {
		return nil, fmt.Errorf("no IntraFlowPreemptionPolicy registered with name %q", name)
	}
	return constructor()
}
