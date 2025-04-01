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

// Package queue defines interfaces and implementations for various queue data structures used by the FlowController.
package queue

import (
	"fmt"
	"sync"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontroller/types"
)

type RegisteredQueueName string

// QueueConstructor defines the function signature for creating a SafeQueue.
// It accepts the ItemComparator that will be optionally used to configure this queue (provided it declares
// CapabilityPriorityConfigurable).
type QueueConstructor func(policyDefinedOrder types.ItemComparator) (types.SafeQueue, error)

var (
	// mu guards the registration maps.
	mu sync.RWMutex
	// registeredQueues stores the constructors for all registered queues.
	registeredQueues = make(map[RegisteredQueueName]QueueConstructor)
)

// RegisterQueue registers a new SafeQueue constructor.
// This function is called by queue implementations in their init() function.
func RegisterQueue(name RegisteredQueueName, constructor QueueConstructor) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registeredQueues[name]; ok {
		panic(fmt.Sprintf("SafeQueue named %s already registered", name))
	}
	registeredQueues[name] = constructor
}

// NewQueueFromName creates a new SafeQueue given its registered name, its specification, and the ItemComparator that
// will be optionally used to configure the queue (provided it declares CapabilityPriorityConfigurable).
// This is called by the FlowRegistry during initialization of a flow's ManagedQueue.
func NewQueueFromName(name RegisteredQueueName, policyDefinedOrder types.ItemComparator) (types.SafeQueue, error) {
	mu.RLock()
	defer mu.RUnlock()

	constructor, ok := registeredQueues[name]
	if !ok {
		return nil, fmt.Errorf("no SafeQueue registered with name %q", name)
	}
	return constructor(policyDefinedOrder)
}
