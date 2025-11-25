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
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/utils/clock"
	configapi "sigs.k8s.io/gateway-api-inference-extension/apix/config/v1alpha1"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	rcplugins "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/requestcontrol/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
)

const SaturationSignalRecorderType = "SaturationSignalRecorder"

// completionChannelSafetyFactor is a multiplier used to calculate the default completions channel buffer size.
//
// Rationale: A safety factor of 4.0 ensures the buffer can handle significant traffic bursts and processing jitter in
// the controller loop without dropping critical completion events. It can absorb the expected load for several tick
// intervals, making the system highly resilient to transient stalls.
const completionChannelSafetyFactor = 4.0

func init() {
	plugins.Register(SaturationSignalRecorderType, SaturationSignalRecorderFactory)
}

// SaturationSignalRecorderFactory defines the factory function for SaturationSignalRecorder.
func SaturationSignalRecorderFactory(name string, params json.RawMessage, handle plugins.Handle) (plugins.Plugin, error) {
	apixConfig := &configapi.SaturationSignalRecorder{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, apixConfig); err != nil {
			return nil, fmt.Errorf("failed to unmarshal parameters for %s: %w", name, err)
		}
	}

	config := LoadSignalRecorderConfigFromAPIX(apixConfig)
	config.setDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}

	return NewSaturationSignalRecorder(config, WithSaturationSignalRecorderName(name)), nil
}

// completionEvent holds the raw data from a single completed request.
type completionEvent struct {
	podID     string
	timestamp time.Time
}

// SaturationSignalRecorder is an in-tree plugin that acts as the primary data collector for the SaturationDetector.
// It operates on the "fast path" of the request lifecycle, recording raw concurrency and completion events into
// thread-safe aggregators.
// The main controller loop then drains these aggregators on its "slow path" tick.
// This architecture cleanly separates high-frequency, per-request data collection from the lower-frequency,
// synchronous, and strategic decision-making loop of the main controller.
type SaturationSignalRecorder struct {
	clock        clock.Clock
	typedName    plugins.TypedName
	tickInterval time.Duration
	concurrency  *concurrencyTracker

	// completions is a buffered channel used to pass completion events from the high-frequency "fast path" (request
	// lifecycle) to the "slow path" (controller reconciliation loop).
	// This decouples the two paths safely and efficiently.
	completions chan completionEvent

	// droppedCountsMu protects the droppedCounts map from concurrent access when a new pod's counter is created for the
	// first time.
	droppedCountsMu sync.Mutex
	// droppedCounts holds a per-pod atomic counter for completion events that were dropped because the primary channel
	// was full, providing critical observability into internal backpressure.
	droppedCounts map[string]*atomic.Uint64
}

type SaturationSignalRecorderOption func(*SaturationSignalRecorder)

// NewSaturationSignalRecorder creates a new, safely initialized recorder.
// It uses the provided config to intelligently calculate the buffer size for the completions channel based on the
// expected max pool QPS and the controller's tick interval.
func NewSaturationSignalRecorder(config *SignalRecorderConfig, opts ...SaturationSignalRecorderOption) *SaturationSignalRecorder {
	// Calculate the expected number of completions that could arrive in a single controller tick interval.
	completionsPerTick := float64(config.MaxExpectedCompletionsQPS) * config.TickInterval.Seconds()

	// Apply a safety factor to create a robust buffer that can absorb traffic bursts and controller processing jitter.
	bufferSize := max(128, int(completionsPerTick*completionChannelSafetyFactor))

	r := &SaturationSignalRecorder{
		typedName:     plugins.TypedName{Type: SaturationSignalRecorderType, Name: SaturationSignalRecorderType},
		clock:         clock.RealClock{},
		concurrency:   newConcurrencyTracker(),
		completions:   make(chan completionEvent, bufferSize),
		droppedCounts: make(map[string]*atomic.Uint64),
		tickInterval:  config.TickInterval,
	}

	for _, opt := range opts {
		opt(r)
	}
	return r
}

func WithSaturationSignalRecorderName(name string) SaturationSignalRecorderOption {
	return func(r *SaturationSignalRecorder) {
		r.typedName.Name = name
	}
}

func WithSaturationSignalRecorderClock(clock clock.Clock) SaturationSignalRecorderOption {
	return func(r *SaturationSignalRecorder) {
		r.clock = clock
	}
}

func (r *SaturationSignalRecorder) TickInterval() time.Duration { return r.tickInterval }

// --- Plugin Interface Implementations ---

// TypedName returns the type and name of the plugin instance.
func (r *SaturationSignalRecorder) TypedName() plugins.TypedName {
	return r.typedName
}

func (r *SaturationSignalRecorder) PreRequest(
	_ context.Context,
	_ *types.LLMRequest,
	schedulingResult *types.SchedulingResult,
) {
	// The scheduling result must contain a primary profile with at least one target pod.
	// This contract is guaranteed by the upstream Scheduling layer.
	pod := schedulingResult.ProfileResults[schedulingResult.PrimaryProfileName].TargetPods[0].GetPod()
	r.concurrency.getOrCreateCounter(pod.NamespacedName.String()).Add(1)
}

// ResponseComplete is called after a request has fully completed.
// It decrements the inflight counter and attempts to buffer the completion event for the main controller.
//
// This method is architected for absolute stability. It uses a non-blocking send to the completions channel,
// deliberately sacrificing short-term telemetry accuracy to guarantee the stability of the user-facing data plane.
// An observability component should never be allowed to block or degrade the system it is observing.
func (r *SaturationSignalRecorder) ResponseComplete(
	_ context.Context,
	_ *types.LLMRequest,
	_ *rcplugins.Response,
	targetPod *backend.Pod,
) {
	podID := targetPod.NamespacedName.String()
	r.concurrency.getCounter(podID).Add(^-uint64(0)) // Atomically subtraction using the two's complement of 1.
	event := completionEvent{
		podID:     podID,
		timestamp: r.clock.Now(),
	}

	select {
	case r.completions <- event:
		// Event was successfully sent and buffered.
	default:
		// The channel is full, indicating the main controller loop is stalled.
		// We drop the event, which causes the pod's service rate estimate (^μ_t) to become artificially low, triggering a
		// safe, self-stabilizing negative feedback loop.
		// The dropped count itself becomes a critical health signal.
		r.getOrCreateDropCounter(podID).Add(1)
		// TODO: Increment a Prometheus counter:
		// saturation_controller_completion_events_dropped_total{pod="...", reason="buffer_full"}
	}
}

// --- Methods for the SaturationController ---

// DrainCompletions is called by the controller's reconciliation loop.
// It efficiently drains all pending events from the channel buffer in a non-blocking manner.
func (r *SaturationSignalRecorder) DrainCompletions() []completionEvent {
	drained := make([]completionEvent, 0, cap(r.completions))
	for {
		select {
		case event := <-r.completions:
			drained = append(drained, event)
		default:
			return drained // Channel is now empty.
		}
	}
}

// DrainDroppedCounts is called by the controller's reconciliation loop.
// It returns the current dropped counts for each pod and resets them. A non-zero value in this map is a critical health
// signal, indicating the controller's slow path is not keeping up with the fast path.
func (r *SaturationSignalRecorder) DrainDroppedCounts() map[string]uint64 {
	r.droppedCountsMu.Lock()
	defer r.droppedCountsMu.Unlock()

	drained := make(map[string]uint64, len(r.droppedCounts))
	for podID, counter := range r.droppedCounts {
		// Atomically load the count and swap it with 0.
		if count := counter.Swap(0); count > 0 {
			drained[podID] = count
		}
	}
	// TODO: A production implementation might periodically clean this map of terminated pods.
	return drained
}

// ConcurrencyTracker returns the underlying tracker so the controller can query it.
func (r *SaturationSignalRecorder) ConcurrencyTracker() *concurrencyTracker {
	return r.concurrency
}

// --- Internal Helpers ---

func (r *SaturationSignalRecorder) getOrCreateDropCounter(podID string) *atomic.Uint64 {
	r.droppedCountsMu.Lock()
	defer r.droppedCountsMu.Unlock()
	if counter, exists := r.droppedCounts[podID]; exists {
		return counter
	}
	newCounter := &atomic.Uint64{}
	r.droppedCounts[podID] = newCounter
	return newCounter
}
