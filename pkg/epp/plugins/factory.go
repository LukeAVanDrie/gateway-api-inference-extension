package plugins

import (
	"fmt"
)

// PluginFactory is a factory that can create new, transient instances of plugins at runtime.
type PluginFactory interface {
	// NewPlugin creates a fresh instance of a plugin from the blueprint identified by the given name.
	NewPlugin(name string) (Plugin, error)
}

// EPPPluginFactory uses the central EPP handle to access plugin blueprints and the central factory registry to create
// new instances.
type EPPPluginFactory struct {
	handle Handle
}

// NewEPPPluginFactory creates a new factory instance.
func NewEPPPluginFactory(handle Handle) *EPPPluginFactory {
	return &EPPPluginFactory{handle: handle}
}

// NewPlugin creates a new, transient instance of a plugin from its blueprint.
func (f *EPPPluginFactory) NewPlugin(name string) (Plugin, error) {
	spec := f.handle.PluginSpec(name)
	if spec == nil {
		return nil, fmt.Errorf("no plugin blueprint named %q is defined in the configuration", name)
	}

	reg, ok := Registry[spec.Type]
	if !ok {
		return nil, fmt.Errorf("no factory registered for plugin type %q (referenced by blueprint %q)", spec.Type, name)
	}

	plugin, err := reg.Factory(spec.Name, spec.Parameters, f.handle)
	if err != nil {
		return nil, fmt.Errorf(
			"factory for plugin type %q failed to create instance for blueprint %q: %w",
			spec.Type, name, err)
	}
	return plugin, nil
}

// NewPluginByType is a generic helper function that provides a type-safe way to create and retrieve a plugin from a
// PluginFactory.
func NewPluginByType[T Plugin](factory PluginFactory, name string) (T, error) {
	var zero T
	rawPlugin, err := factory.NewPlugin(name)
	if err != nil {
		return zero, err
	}

	plugin, ok := rawPlugin.(T)
	if !ok {
		return zero, fmt.Errorf(
			"plugin created from blueprint %q is of type %T, but expected type %T",
			name, rawPlugin, zero,
		)
	}
	return plugin, nil
}
