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
package saturationdetector

import (
	"fmt"
	"time"
)

// Default configuration values for the SaturationDetector's P-controller.
const (
	// DefaultTargetUtilization is the default goal state (Setpoint) for the P-controller.
	// A value of 0.85 means the system aims to keep the aggregate backend utilization at 85%,
	// leaving a 15% buffer to absorb variance and prevent latency spikes.
	DefaultTargetUtilization = 0.85

	// DefaultProportionalGain (Kp) is the default tuning constant for the P-controller.
	// This value (10.0) provides a reasonably aggressive response. For example:
	// - If error = 0.1 (10% capacity available below target), dispatch probability = 1.0.
	// - If error = 0.05 (5% capacity available below target), dispatch probability = 0.5.
	DefaultProportionalGain = 10.0

	// DefaultCachingTTL is the default duration for which the detector's internal cache of pod metrics
	// is considered valid. 100ms is a balance between data freshness and reducing lock contention.
	DefaultCachingTTL = 100 * time.Millisecond
)

// Config holds the configuration for the SaturationDetector's P-controller.
// These values are critical for tuning the behavior of the control loop.
type Config struct {
	// TargetUtilization is the goal state (Setpoint) for the P-controller.
	// The system will modulate its dispatch rate to keep the backend utilization stable at this target.
	// Must be between 0.0 and 1.0.
	// Optional: Defaults to DefaultTargetUtilization.
	TargetUtilization float64

	// ProportionalGain (Kp) is the tuning constant for the P-controller.
	// It determines how aggressively the controller reacts to the "error" (TargetUtilization - CurrentUtilization).
	// A higher value leads to a faster but potentially less stable response. Must be non-negative.
	// Optional: Defaults to DefaultProportionalGain.
	ProportionalGain float64

	// CachingTTL is the duration for which the detector's internal cache of pod metrics is considered valid.
	// This value represents a direct trade-off between data freshness and performance (lock contention).
	// Must be a positive duration.
	// Optional: Defaults to DefaultCachingTTL.
	CachingTTL time.Duration
}

// ValidateAndApplyDefaults checks the configuration for validity and returns a new Config object
// with defaults applied. It does not mutate the receiver.
func (c *Config) ValidateAndApplyDefaults() (*Config, error) {
	cfg := c.clone()

	// --- Defaulting ---
	if cfg.TargetUtilization == 0 {
		cfg.TargetUtilization = DefaultTargetUtilization
	}
	if cfg.ProportionalGain == 0 {
		cfg.ProportionalGain = DefaultProportionalGain
	}
	if cfg.CachingTTL == 0 {
		cfg.CachingTTL = DefaultCachingTTL
	}

	// --- Validation ---
	if cfg.TargetUtilization < 0 || cfg.TargetUtilization > 1.0 {
		return nil, fmt.Errorf("TargetUtilization must be between 0.0 and 1.0, but got %f", cfg.TargetUtilization)
	}
	if cfg.ProportionalGain < 0 {
		return nil, fmt.Errorf("ProportionalGain cannot be negative, but got %f", cfg.ProportionalGain)
	}
	if cfg.CachingTTL <= 0 {
		return nil, fmt.Errorf("CachingTTL must be a positive duration, but got %v", cfg.CachingTTL)
	}

	return cfg, nil
}

// clone creates a shallow copy of the Config object.
// Since all fields are value types, a shallow copy is sufficient.
func (c *Config) clone() *Config {
	if c == nil {
		return nil
	}
	newCfg := *c
	return &newCfg
}
