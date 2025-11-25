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
	"errors"
	"fmt"
	"time"
)

// SignalRecorderConfig holds the configuration for the SaturationSignalRecorder plugin.
// This configuration defines the "Sensor Resolution" of the system.
type SignalRecorderConfig struct {
	// TickInterval defines the fundamental sampling resolution of the system.
	// It dictates both the buffer size of the Recorder (Fast Path) and the reconciliation frequency of the Saturation
	// Controller (Slow Path). The Controller automatically synchronizes its loop to this interval.
	//
	// [Optional] Tuning Guidance:
	//   - Default: 50ms. Balanced for standard LLM inference (TPOT ~20-50ms).
	//   - Lower (e.g., 10ms): Required for ultra-low latency or very fast models to prevent aliasing.
	// 		 Increases CPU usage.
	//   - Higher (e.g., 100ms): Acceptable for low-throughput, high-latency batch workloads.
	TickInterval time.Duration

	// MaxExpectedCompletionsQPS tunes the memory footprint of the internal non-blocking completion buffers.
	//
	// [Optional] Tuning Guidance:
	//   - Default: 1000.
	//   - Set this to the theoretical peak throughput of the backend pool (e.g., BatchSize * 1/TPOT * NumReplicas).
	//   - It does not limit traffic, but ensures the recorder allocates enough memory to absorb micro-bursts without
	//     dropping telemetry.
	MaxExpectedCompletionsQPS int
}

// ControllerConfig holds the tuning parameters for the SaturationController's logic.
//
// The default values for [Optional] and [Advanced] fields have been rigorously derived from Queuing Theory (Kingman's
// Formula), Control Theory (2-DOF PID), and the physics of LLM hardware. They are designed to be robust across a wide
// range of models and hardware topologies, workload characteristics, and usage patterns.
//
// Most operators should ONLY configure the [Required] fields:
//  1. MaxQueueLatency (Your Business/SLO Contract)
//  2. SignalRecorderPluginName (Your Infrastructure Wiring)
type ControllerConfig struct {
	// --- Control Loop Parameters (The "Brain") ---

	// SaturationSetpoint is the target Process Variable (PV) value for the Feedback Loop.
	// It represents the desired ratio of Load to Capacity (L/C).
	//
	// [Optional] Tuning Guidance:
	//   - Range: (0.0, 1.0]. Default: 0.85.
	//   - Lower values (0.5 - 0.7): Trade wasted capacity for strict latency determinism (keeping the queue empty).
	//   - Higher values (0.9 - 0.95): Ride the "latency cliff" to maximize throughput, risking tail latency spikes.
	SaturationSetpoint float64

	// SaturationHeadroom defines the safety margin between the Regulation Setpoint and the Hard Rejection Threshold.
	// Traffic is physically blocked when PV > (Setpoint + Headroom).
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 0.15.
	//   - Increase: If the system oscillates between "Regulating" and "Rejecting" too frequently (Chattering).
	//   - Decrease: To enforce a harder ceiling on saturation, at the risk of abrupt rejections.
	SaturationHeadroom float64

	// ProportionalGain (Kp) determines the aggressiveness of the P-Controller's response to error.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 1.0.
	//   - Increase (> 1.0): For faster reaction to load spikes, but increases risk of ringing/overshoot.
	//   - Decrease (< 1.0): For a smoother, more damped response, but may react too slowly to overload.
	ProportionalGain float64

	// MinDispatchRate is the "Pilot Light" floor for the dispatch rate.
	//
	// [Optional] Tuning Guidance:
	//   - Default: 1.0 QPS.
	//   - Increase: To force a faster restart after an idle period, at the risk of admitting traffic to a down pool.
	MinDispatchRate float64

	// MaxQueueLatency defines the operational contract for the Flow Control Queue.
	// It represents the maximum duration a request is allowed to wait in the EPP before dispatch.
	//
	// [Required] Tuning Guidance:
	//   - (Suggested) Formula: MaxQueueLatency = (Target TTFT SLO) - (P99 Backend Prefill Time).
	//   - Example: If SLO is 500ms and Prefill is 300ms, set this to 200ms.
	//   - This acts as the "Budget" for the QueuePressure (P_q) calculation.
	MaxQueueLatency time.Duration

	// --- Estimator Smoothing (The Signal Processors) ---

	// EffectiveBatchAlpha is the smoothing factor for the ^B_eff (Effective Batch Capacity) estimator.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Range: (0.0, 1.0]. Default: 0.2.
	//   - Higher: Faster adaptation to "Workload Phase Shifts" (e.g., context length changes).
	//   - Lower: More stable estimate, less jitter from individual batches.
	EffectiveBatchAlpha float64

	// QueueDepthAlpha is the smoothing factor for the ^Q_t (Queue Depth) estimator.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Range: (0.0, 1.0]. Default: 0.25.
	//   - Higher: Faster reaction to queue buildup (Feedback).
	//   - Lower: Smoother signal, but introduces Feedback Lag which destabilizes the control loop.
	QueueDepthAlpha float64

	// ServiceRateWindow is the time window for the ^μ_t (Service Rate) estimator decay.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 10s.
	//   - Increase: To remember capacity estimates longer during idle periods (risks Ghost Capacity).
	//   - Decrease: To force faster demotion of idle pods to the "Maturing" state.
	ServiceRateWindow time.Duration

	// --- Estimator Memory (The Windows) ---

	// PeakInflightConcurrencyWindow is the sliding window duration for L_peak (Hill Climbing).
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 5m.
	//   - Increase: To be more optimistic, remembering past peaks for longer.
	//   - Decrease: To be more adaptive, forgetting past peaks quickly if the environment degrades.
	PeakInflightConcurrencyWindow time.Duration

	// PeakInflightConcurrencySamples is the number of peak samples to retain for L_peak.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 3.
	PeakInflightConcurrencySamples int

	// KVCacheWindow is the sliding window duration for the U_kv (Memory Pressure) Max Filter.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 200ms.
	//   - Must be >= MetricsStalenessThreshold.
	//   - Increase: To hold onto "Memory Spike" signals longer (safer).
	KVCacheWindow time.Duration

	// KVCacheSamples is the number of samples to retain for U_kv.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 3.
	KVCacheSamples int

	// --- Lifecycle & Trust (The State Machine) ---

	// MaturityQuorumPercentage is the percentage of Mature pods required to enter the Regulating regime.
	//
	// [Optional] Tuning Guidance:
	//   - Default: 0.75.
	//   - Increase: To be more conservative, waiting for almost all pods to be characterized before rate limiting.
	//   - Decrease: To be more aggressive, switching to rate limiting earlier during a scale-up.
	MaturityQuorumPercentage float64

	// DormantTimeout is the duration before an idle, Immature pod is moved to the Dormant state.
	//
	// [Optional] Tuning Guidance:
	//   - Default: 5m.
	//   - Lower: Aggressively ignores trickle-traffic pods in the Regime calculation.
	DormantTimeout time.Duration

	// MetricsStalenessThreshold is the max allowed age for metrics before a pod is excluded.
	//
	// [Optional, Infrastructure-Dependent] Tuning Guidance:
	//   - Default: 150ms.
	//   - CRITICAL: Must be > 1.5x the actual metric collection interval.
	MetricsStalenessThreshold time.Duration

	// --- Statistical Confidence ---

	// MinSamplesForEffectiveBatchMaturity is the confidence threshold for the Batch Capacity estimator.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 10.
	//   - Higher: Stays in "Hill Climbing" (Concurrency Limit) mode longer. Safer.
	//   - Lower: Transitions to "Peer Seeding" (Rate Limit) mode faster. Riskier.
	MinSamplesForEffectiveBatchMaturity uint64

	// MinEffectiveCountForServiceRateMaturity is the confidence threshold for the Service Rate estimator.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 3.0.
	//   - Higher: Requires more sustained throughput to reach "Mature" state.
	MinEffectiveCountForServiceRateMaturity float64

	// --- Sampling Physics ---

	// MinBatchSampleInterval is the cooldown between ^B_eff samples.
	//
	// [Optional, Advanced] Tuning Guidance:
	//   - Default: 1s.
	//   - Should be approx 0.5x to 1.0x the typical batch execution time to ensure sample independence.
	MinBatchSampleInterval time.Duration

	// --- Dependencies ---
	SignalRecorderPluginName string
}

// SignalRecorderConfigBuilder provides a fluent API for constructing a valid SignalRecorderConfig.
type SignalRecorderConfigBuilder struct {
	config *SignalRecorderConfig
}

// NewSignalRecorderConfigBuilder creates a new builder instance with defaults applied.
func NewSignalRecorderConfigBuilder() *SignalRecorderConfigBuilder {
	c := &SignalRecorderConfig{}
	c.setDefaults()
	return &SignalRecorderConfigBuilder{config: c}
}

// Build validates and returns the config.
func (b *SignalRecorderConfigBuilder) Build() (*SignalRecorderConfig, error) {
	if err := b.config.validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return b.config, nil
}

func (b *SignalRecorderConfigBuilder) WithTickInterval(d time.Duration) *SignalRecorderConfigBuilder {
	b.config.TickInterval = d
	return b
}

func (b *SignalRecorderConfigBuilder) WithMaxExpectedCompletionsQPS(n int) *SignalRecorderConfigBuilder {
	b.config.MaxExpectedCompletionsQPS = n
	return b
}

// ControllerConfigBuilder provides a fluent API for constructing a valid ControllerConfig.
type ControllerConfigBuilder struct {
	config *ControllerConfig
}

// NewControllerConfigBuilder creates a new builder instance with defaults applied.
func NewControllerConfigBuilder() *ControllerConfigBuilder {
	c := &ControllerConfig{}
	c.setDefaults()
	return &ControllerConfigBuilder{config: c}
}

// Build validates and returns the config.
func (b *ControllerConfigBuilder) Build() (*ControllerConfig, error) {
	if err := b.config.validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return b.config, nil
}

func (b *ControllerConfigBuilder) WithSaturationSetpoint(s float64) *ControllerConfigBuilder {
	b.config.SaturationSetpoint = s
	return b
}

func (b *ControllerConfigBuilder) WithSaturationHeadroom(s float64) *ControllerConfigBuilder {
	b.config.SaturationHeadroom = s
	return b
}

func (b *ControllerConfigBuilder) WithProportionalGain(p float64) *ControllerConfigBuilder {
	b.config.ProportionalGain = p
	return b
}

func (b *ControllerConfigBuilder) WithMinDispatchRate(d float64) *ControllerConfigBuilder {
	b.config.MinDispatchRate = d
	return b
}

func (b *ControllerConfigBuilder) WithMaxQueueLatency(d time.Duration) *ControllerConfigBuilder {
	b.config.MaxQueueLatency = d
	return b
}

func (b *ControllerConfigBuilder) WithEffectiveBatchAlpha(a float64) *ControllerConfigBuilder {
	b.config.EffectiveBatchAlpha = a
	return b
}

func (b *ControllerConfigBuilder) WithQueueDepthAlpha(a float64) *ControllerConfigBuilder {
	b.config.QueueDepthAlpha = a
	return b
}

func (b *ControllerConfigBuilder) WithServiceRateWindow(d time.Duration) *ControllerConfigBuilder {
	b.config.ServiceRateWindow = d
	return b
}

func (b *ControllerConfigBuilder) WithPeakInflightConcurrencyWindow(d time.Duration) *ControllerConfigBuilder {
	b.config.PeakInflightConcurrencyWindow = d
	return b
}

func (b *ControllerConfigBuilder) WithPeakInflightConcurrencySamples(n int) *ControllerConfigBuilder {
	b.config.PeakInflightConcurrencySamples = n
	return b
}

func (b *ControllerConfigBuilder) WithKVCacheWindow(d time.Duration) *ControllerConfigBuilder {
	b.config.KVCacheWindow = d
	return b
}

func (b *ControllerConfigBuilder) WithKVCacheSamples(n int) *ControllerConfigBuilder {
	b.config.KVCacheSamples = n
	return b
}

func (b *ControllerConfigBuilder) WithMaturityQuorumPercentage(p float64) *ControllerConfigBuilder {
	b.config.MaturityQuorumPercentage = p
	return b
}

func (b *ControllerConfigBuilder) WithDormantTimeout(d time.Duration) *ControllerConfigBuilder {
	b.config.DormantTimeout = d
	return b
}

func (b *ControllerConfigBuilder) WithMetricsStalenessThreshold(d time.Duration) *ControllerConfigBuilder {
	b.config.MetricsStalenessThreshold = d
	return b
}

func (b *ControllerConfigBuilder) WithMinSamplesForEffectiveBatchMaturity(n uint64) *ControllerConfigBuilder {
	b.config.MinSamplesForEffectiveBatchMaturity = n
	return b
}

func (b *ControllerConfigBuilder) WithMinEffectiveCountForServiceRateMaturity(c float64) *ControllerConfigBuilder {
	b.config.MinEffectiveCountForServiceRateMaturity = c
	return b
}

func (b *ControllerConfigBuilder) WithMinBatchSampleInterval(d time.Duration) *ControllerConfigBuilder {
	b.config.MinBatchSampleInterval = d
	return b
}

func (b *ControllerConfigBuilder) WithSignalRecorderPluginName(name string) *ControllerConfigBuilder {
	b.config.SignalRecorderPluginName = name
	return b
}

// validate checks the SignalRecorderConfig for logical consistency.
func (c *SignalRecorderConfig) validate() error {
	if c.MaxExpectedCompletionsQPS <= 0 {
		return errors.New("MaxExpectedCompletionsQPS must be a positive integer")
	}
	if c.TickInterval <= 0 {
		return errors.New("TickInterval must be a positive duration")
	}
	return nil
}

// validate checks the ControllerConfig for logical consistency.
func (c *ControllerConfig) validate() error {
	if c.SaturationSetpoint <= 0 || c.SaturationSetpoint > 1.0 {
		return errors.New("SaturationSetpoint must be in the range (0, 1]")
	}
	if c.SaturationHeadroom <= 0 || c.SaturationHeadroom > 1.0 {
		return errors.New("SaturationHeadroom must be in the range (0, 1]")
	}
	if c.MinDispatchRate <= 0 {
		return errors.New("MinDispatchRate must be a positive value")
	}
	if c.ProportionalGain <= 0 {
		return errors.New("ProportionalGain must be a positive value")
	}
	if c.MaxQueueLatency <= 0 {
		return errors.New("MaxQueueLatency must be a positive duration")
	}
	if c.MaturityQuorumPercentage <= 0 || c.MaturityQuorumPercentage > 1.0 {
		return errors.New("MaturityQuorumPercentage must be in the range (0, 1]")
	}
	if c.DormantTimeout <= 0 {
		return errors.New("DormantTimeout must be a positive duration")
	}
	if c.MetricsStalenessThreshold <= 0 {
		return errors.New("MetricsStalenessThreshold must be a positive duration")
	}
	if c.MinSamplesForEffectiveBatchMaturity <= 0 {
		return errors.New("MinSamplesForEffectiveBatchMaturity must be a positive integer")
	}
	if c.MinEffectiveCountForServiceRateMaturity <= 0 {
		return errors.New("MinEffectiveCountForServiceRateMaturity must be a positive value")
	}
	if c.PeakInflightConcurrencyWindow <= 0 {
		return errors.New("PeakInflightConcurrencyWindow must be a positive duration")
	}
	if c.PeakInflightConcurrencySamples <= 0 {
		return errors.New("PeakInflightConcurrencySamples must be a positive integer")
	}
	if c.KVCacheWindow <= 0 {
		return errors.New("KVCacheWindow must be a positive duration")
	}
	if c.KVCacheSamples <= 0 {
		return errors.New("KVCacheSamples must be a positive integer")
	}
	if c.EffectiveBatchAlpha <= 0 || c.EffectiveBatchAlpha > 1.0 {
		return errors.New("EffectiveBatchAlpha must be in the range (0, 1]")
	}
	if c.QueueDepthAlpha <= 0 || c.QueueDepthAlpha > 1.0 {
		return errors.New("QueueDepthAlpha must be in the range (0, 1]")
	}
	if c.ServiceRateWindow <= 0 {
		return errors.New("ServiceRateWindow must be a positive duration")
	}
	if c.MinBatchSampleInterval <= 0 {
		return errors.New("MinBatchSampleInterval must be a positive duration")
	}
	if c.SignalRecorderPluginName == "" {
		return errors.New("SignalRecorderPluginName is required")
	}
	return nil
}
