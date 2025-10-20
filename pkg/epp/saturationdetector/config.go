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

// Default configuration values for the SaturationDetector (Bang-Bang Controller with Hysteresis).
const (
	// --- Hysteresis Control Defaults ---

	// DefaultTargetUtilization (High Watermark) is the utilization threshold at which the controller engages blocking.
	// A value of 0.85 means the system aims to keep the aggregate backend utilization at 85%, leaving a 15% buffer to
	// absorb variance and prevent latency spikes.
	DefaultTargetUtilization = 0.85

	// DefaultResumeUtilization (Low Watermark) is the utilization threshold at which the controller resumes dispatch.
	// The gap between TargetUtilization and ResumeUtilization defines the Hysteresis band, preventing rapid oscillations
	// (chatter) around the setpoint.
	DefaultResumeUtilization = 0.75

	// DefaultCachingTTL is the default duration for which the detector's internal cache of pod metrics is considered
	// valid. 100ms balances data freshness and reducing lock contention.
	DefaultCachingTTL = 100 * time.Millisecond

	// --- Stabilization (Stateful Probing) Defaults ---

	// DefaultWarmUpSampleCount is the number of sojourn time samples required before EWMAs are considered stable.
	DefaultWarmUpSampleCount = 10

	// DefaultEWMAStalenessThreshold is the duration after which EWMAs are considered stale.
	DefaultEWMAStalenessThreshold = 15 * time.Second

	// DefaultProbeInterval is the minimum time between forced probes when metrics are unreliable (stale or unstable).
	// This controls the maximum probing rate (e.g., 500ms = 2 probes/sec), preventing floods during cold starts while
	// ensuring recovery from metric freeze induced deadlock.
	DefaultProbeInterval = 500 * time.Millisecond
)

// Config holds the configuration for the SaturationDetector's Bang-Bang controller.
type Config struct {
	// --- Bang-Bang Controller with Hysteresis Parameters ---

	// TargetUtilization is the High Watermark. Blocking engages when utilization exceeds this value.
	// Must be in (ResumeUtilization, 1.0).
	// Optional: Defaults to DefaultTargetUtilization.
	TargetUtilization float64

	// ResumeUtilization is the Low Watermark. Dispatch resumes when utilization drops below this value.
	// Must be in (0.0, TargetUtilization).
	// Optional: Defaults to DefaultResumeUtilization.
	ResumeUtilization float64

	// CachingTTL is the duration for which the detector's internal cache of pod metrics is considered valid.
	// Must be a positive duration.
	// Optional: Defaults to DefaultCachingTTL.
	CachingTTL time.Duration

	// --- Stabilization (Stateful Probing) Parameters ---

	// WarmUpSampleCount defines the number of samples required for stability.
	// Must be nonnegative.
	// Optional: Defaults to DefaultWarmUpSampleCount.
	WarmUpSampleCount int64

	// EWMAStalenessThreshold defines the duration for staleness.
	// Must be a positive duration.
	// Optional: Defaults to DefaultEWMAStalenessThreshold.
	EWMAStalenessThreshold time.Duration

	// ProbeInterval defines the minimum time between forced probes when metrics are unreliable.
	// Must be a positive duration.
	// Optional: Defaults to DefaultProbeInterval.
	ProbeInterval time.Duration
}

// ValidateAndApplyDefaults checks the configuration for validity and returns a new Config object
// with defaults applied. It does not mutate the receiver.
func (c *Config) ValidateAndApplyDefaults() (*Config, error) {
	cfg := c.clone()

	// --- Defaulting ---
	if cfg.TargetUtilization == 0 {
		cfg.TargetUtilization = DefaultTargetUtilization
	}
	if cfg.ResumeUtilization == 0 {
		cfg.ResumeUtilization = DefaultResumeUtilization
	}
	if cfg.CachingTTL == 0 {
		cfg.CachingTTL = DefaultCachingTTL
	}
	if cfg.WarmUpSampleCount == 0 {
		cfg.WarmUpSampleCount = DefaultWarmUpSampleCount
	}
	if cfg.EWMAStalenessThreshold == 0 {
		cfg.EWMAStalenessThreshold = DefaultEWMAStalenessThreshold
	}
	if cfg.ProbeInterval == 0 {
		cfg.ProbeInterval = DefaultProbeInterval
	}

	// --- Validation ---

	// Bang-Bang Controller Validation
	if cfg.TargetUtilization <= 0 || cfg.TargetUtilization >= 1.0 {
		return nil, fmt.Errorf("TargetUtilization must be in (0, 1), but got %f", cfg.TargetUtilization)
	}
	if cfg.ResumeUtilization <= 0 || cfg.ResumeUtilization >= 1.0 {
		return nil, fmt.Errorf("ResumeUtilization must be in (0, 1), but got %f", cfg.ResumeUtilization)
	}
	if cfg.TargetUtilization <= cfg.ResumeUtilization {
		return nil, fmt.Errorf(
			"TargetUtilization (%f) must be greater than ResumeUtilization (%f) to define a hysteresis band",
			cfg.TargetUtilization, cfg.ResumeUtilization)
	}
	if cfg.CachingTTL <= 0 {
		return nil, fmt.Errorf("CachingTTL must be a positive duration, but got %v", cfg.CachingTTL)
	}

	// Stabilization Validation
	if cfg.WarmUpSampleCount <= 0 {
		return nil, fmt.Errorf("WarmUpSampleCount must be nonnegative, but got %d", cfg.WarmUpSampleCount)
	}
	if cfg.EWMAStalenessThreshold <= 0 {
		return nil, fmt.Errorf("EWMAStalenessThreshold must be a positive duration, but got %v", cfg.EWMAStalenessThreshold)
	}
	if cfg.ProbeInterval <= 0 {
		return nil, fmt.Errorf("ProbeInterval must be a positive duration, but got %v", cfg.ProbeInterval)
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
