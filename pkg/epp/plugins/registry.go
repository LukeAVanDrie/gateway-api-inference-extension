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

// Package plugins provides the foundational types and the central registry for the EPP's pluggable architecture.
// It defines the contracts that all plugins must adhere to and the mechanism by which they are discovered and managed.
package plugins

import (
	"encoding/json"
	"fmt"
)

// Factory is the function signature for creating a new plugin instance.
type Factory func(name string, parameters json.RawMessage, handle Handle) (Plugin, error)

// PluginLifecycle defines the instantiation policy for a plugin.
type PluginLifecycle int

const (
	// LifecycleSingleton indicates that a single, shared instance of the plugin should be created at startup.
	// This is the default lifecycle for all plugins.
	LifecycleSingleton PluginLifecycle = iota

	// LifecycleTransient indicates that the plugin's configuration is a blueprint for creating multiple, independent
	// instances at runtime.
	LifecycleTransient
)

// PluginRegistration holds the complete, self-describing metadata for a registered plugin type.
type PluginRegistration struct {
	Factory   Factory
	Lifecycle PluginLifecycle
}

// Registry is the central, global map of all known plugin types.
var Registry = make(map[string]PluginRegistration)

// Register is a simple, Singleton plugin registration function.
// It is preserved for 100% backward compatibility with all existing third-party plugins.
// Plugins registered via this function will always be treated as Singletons.
func Register(pluginImplType string, factory Factory) {
	if _, exists := Registry[pluginImplType]; exists {
		panic(fmt.Sprintf("plugin type %q is already registered", pluginImplType))
	}
	Registry[pluginImplType] = PluginRegistration{
		Factory:   factory,
		Lifecycle: LifecycleSingleton, // All legacy plugins are singletons.
	}
}

// RegisterWithMetadata makes a plugin known to the registry.
func RegisterWithMetadata(pluginImplType string, reg PluginRegistration) {
	if _, exists := Registry[pluginImplType]; exists {
		panic(fmt.Sprintf("plugin type %q is already registered", pluginImplType))
	}
	Registry[pluginImplType] = reg
}

// ValidatePluginRef checks if a plugin reference is valid based on its type and lifecycle.
func ValidatePluginRef(pluginType string, expectedLifecycle PluginLifecycle) error {
	reg, ok := Registry[pluginType]
	if !ok {
		return fmt.Errorf("plugin type %q not found in registry", pluginType)
	}

	if reg.Lifecycle != expectedLifecycle {
		return fmt.Errorf("plugin type %q has lifecycle %v, but expected %v", pluginType, reg.Lifecycle, expectedLifecycle)
	}

	// TODO: This is a basic check. We might need a more sophisticated way to check interface implementation.
	// For now, we assume that if the type is registered, it's of the correct interface.
	// A more robust check would involve instantiating the plugin and type asserting.
	// However, that's not possible here without a Handle and parameters.
	// We can enhance this later if needed.
	return nil
}
