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

package interflowdispatch

import (
	"fmt"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

type RegisteredInterFlowDispatchPolicyName string

// InterFlowDispatchPolicyConstructor defines the function signature for creating an InterFlowDispatchPolicy.
// It can accept configuration parameters in the future if needed.
type InterFlowDispatchPolicyConstructor func() (types.InterFlowDispatchPolicy, error)

var (
	// mu guards the registration maps.
	mu sync.RWMutex
	// registeredInterFlowDispatchPolicies stores the constructors for all registered inter-flow dispatch policies.
	registeredInterFlowDispatchPolicies = make(map[RegisteredInterFlowDispatchPolicyName]InterFlowDispatchPolicyConstructor)
)

// RegisterPolicy registers a new inter-flow dispatch policy constructor.
// This function is called by policy implementations in their init() function.
func RegisterPolicy(name RegisteredInterFlowDispatchPolicyName, constructor InterFlowDispatchPolicyConstructor) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registeredInterFlowDispatchPolicies[name]; ok {
		panic(fmt.Sprintf("InterFlowDispatchPolicy named %s already registered", name))
	}
	registeredInterFlowDispatchPolicies[name] = constructor
}

// NewPolicyFromName creates a new InterFlowDispatchPolicy given its registered name.
// This is called by the FlowRegistry during initialization.
// It can be extended to pass configuration to the constructor if policies become configurable.
func NewPolicyFromName(name RegisteredInterFlowDispatchPolicyName) (types.InterFlowDispatchPolicy, error) {
	mu.RLock()
	defer mu.RUnlock()
	constructor, ok := registeredInterFlowDispatchPolicies[name]
	if !ok {
		return nil, fmt.Errorf("no InterFlowDispatchPolicy registered with name %q", name)
	}
	return constructor()
}
