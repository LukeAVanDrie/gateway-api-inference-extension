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

// Package registry provides the registration and instantiation mechanisms for Flow Control plugins.
package registry

import (
	"encoding/json"
	"fmt"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

// FactoryFunc is the generic function signature for plugin factories.
type FactoryFunc func(name string, parameters json.RawMessage, handle plugins.Handle) (plugins.Plugin, error)

var (
	mu                          sync.RWMutex
	safeQueueRegistry               = make(map[string]FactoryFunc)
	itemComparatorRegistry          = make(map[string]FactoryFunc)
	intraFlowDispatchPolicyRegistry = make(map[string]FactoryFunc)
	interFlowDispatchPolicyRegistry = make(map[string]FactoryFunc)
)

// RegisterSafeQueue registers a SafeQueue plugin factory.
func RegisterSafeQueue(name string, factory FactoryFunc) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := safeQueueRegistry[name]; exists {
		panic(fmt.Sprintf("SafeQueue plugin %q already registered", name))
	}
	safeQueueRegistry[name] = factory
}

// GetSafeQueue instantiates a SafeQueue plugin by name.
func GetSafeQueue(name string, parameters json.RawMessage, handle plugins.Handle) (framework.SafeQueue, error) {
	mu.RLock()
	factory, exists := safeQueueRegistry[name]
	mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("SafeQueue plugin %q not found", name)
	}
	plugin, err := factory(name, parameters, handle)
	if err != nil {
		return nil, err
	}
	sq, ok := plugin.(framework.SafeQueue)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not a framework.SafeQueue", name)
	}
	return sq, nil
}

// RegisterItemComparator registers an ItemComparator plugin factory.
func RegisterItemComparator(name string, factory FactoryFunc) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := itemComparatorRegistry[name]; exists {
		panic(fmt.Sprintf("ItemComparator plugin %q already registered", name))
	}
	itemComparatorRegistry[name] = factory
}

// GetItemComparator instantiates an ItemComparator plugin by name.
func GetItemComparator(name string, parameters json.RawMessage, handle plugins.Handle) (framework.ItemComparator, error) {
	mu.RLock()
	factory, exists := itemComparatorRegistry[name]
	mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("ItemComparator plugin %q not found", name)
	}
	plugin, err := factory(name, parameters, handle)
	if err != nil {
		return nil, err
	}
	ic, ok := plugin.(framework.ItemComparator)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not a framework.ItemComparator", name)
	}
	return ic, nil
}

// RegisterIntraFlowDispatchPolicy registers an IntraFlowDispatchPolicy plugin factory.
func RegisterIntraFlowDispatchPolicy(name string, factory FactoryFunc) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := intraFlowDispatchPolicyRegistry[name]; exists {
		panic(fmt.Sprintf("IntraFlowDispatchPolicy plugin %q already registered", name))
	}
	intraFlowDispatchPolicyRegistry[name] = factory
}

// GetIntraFlowDispatchPolicy instantiates an IntraFlowDispatchPolicy plugin by name.
func GetIntraFlowDispatchPolicy(name string, parameters json.RawMessage, handle plugins.Handle) (framework.IntraFlowDispatchPolicy, error) {
	mu.RLock()
	factory, exists := intraFlowDispatchPolicyRegistry[name]
	mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("IntraFlowDispatchPolicy plugin %q not found", name)
	}
	plugin, err := factory(name, parameters, handle)
	if err != nil {
		return nil, err
	}
	intra, ok := plugin.(framework.IntraFlowDispatchPolicy)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not a framework.IntraFlowDispatchPolicy", name)
	}
	return intra, nil
}

// RegisterInterFlowDispatchPolicy registers an InterFlowDispatchPolicy plugin factory.
func RegisterInterFlowDispatchPolicy(name string, factory FactoryFunc) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := interFlowDispatchPolicyRegistry[name]; exists {
		panic(fmt.Sprintf("InterFlowDispatchPolicy plugin %q already registered", name))
	}
	interFlowDispatchPolicyRegistry[name] = factory
}

// GetInterFlowDispatchPolicy instantiates an InterFlowDispatchPolicy plugin by name.
func GetInterFlowDispatchPolicy(name string, parameters json.RawMessage, handle plugins.Handle) (framework.InterFlowDispatchPolicy, error) {
	mu.RLock()
	factory, exists := interFlowDispatchPolicyRegistry[name]
	mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("InterFlowDispatchPolicy plugin %q not found", name)
	}
	plugin, err := factory(name, parameters, handle)
	if err != nil {
		return nil, err
	}
	inter, ok := plugin.(framework.InterFlowDispatchPolicy)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not a framework.InterFlowDispatchPolicy", name)
	}
	return inter, nil
}
