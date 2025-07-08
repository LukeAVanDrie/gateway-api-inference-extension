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

package dispatch

import (
	"fmt"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
)

type RegisteredPolicyName string

type PolicyConstructor func() (framework.InterFlowDispatchPolicy, error)

var (
	mu                 sync.RWMutex
	RegisteredPolicies = make(map[RegisteredPolicyName]PolicyConstructor)
)

func MustRegisterPolicy(name RegisteredPolicyName, constructor PolicyConstructor) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := RegisteredPolicies[name]; ok {
		panic(fmt.Sprintf("InterFlowDispatchPolicy already registered with name %q", name))
	}
	RegisteredPolicies[name] = constructor
}

func NewPolicyFromName(name RegisteredPolicyName) (framework.InterFlowDispatchPolicy, error) {
	mu.RLock()
	defer mu.RUnlock()
	constructor, ok := RegisteredPolicies[name]
	if !ok {
		return nil, fmt.Errorf("no InterFlowDispatchPolicy registered with name %q", name)
	}
	return constructor()
}
