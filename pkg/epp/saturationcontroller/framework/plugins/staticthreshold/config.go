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

package staticthreshold

import (
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// defaultQueueDepthThreshold is the default set point for the backend waiting queue.
	defaultQueueDepthThreshold = 5

	// defaultKVCacheUtilThreshold is the default safety ceiling (80%) for KV cache utilization.
	defaultKVCacheUtilThreshold = 0.8

	// defaultMetricsStalenessThreshold is the watchdog timer for the data ingestion pipeline.
	//
	// Rationale: 200ms acts as a grace period to absorb normal jitter in a distributed system (network + serialization).
	// It is set to 4x the default TickInterval (50ms) to tolerate up to 3 missed scrapes/reports before failing closed.
	defaultMetricsStalenessThreshold = 200 * time.Millisecond
)

// pluginConfig defines the JSON configuration parameters for the Saturation Controller.
// This struct maps directly to the raw JSON provided in the plugin configuration.
type pluginConfig struct {
	// QueueDepthThreshold defines the target backend waiting queue size (set point).
	// A value of 0 implies strict Just-In-Time (JIT) dispatching.
	//
	// Tuning Guidance:
	//  - Default: 5.
	//  - Higher Values (e.g., 1-2x max batch size): Prioritize throughput.
	//    This allows the model server to buffer enough requests to form efficient batches.
	//  - Lower Values (e.g., 0): Prioritize latency and global fairness.
	//    This forces queuing to happen at the Gateway, enabling strict priority enforcement.
	QueueDepthThreshold *int `json:"queueDepthThreshold,omitempty"`

	// KVCacheUtilThreshold defines the maximum allowed KV cache utilization (safety limit).
	// Range: (0.0, 1.0].
	// If a backend reports utilization higher than this threshold, it is considered saturated.
	KVCacheUtilThreshold *float64 `json:"kvCacheUtilThreshold,omitempty"`

	// MetricsStalenessThreshold defines the maximum age of a metric before the backend is considered to have unknown
	// capacity (fail-closed).
	MetricsStalenessThreshold *metav1.Duration `json:"metricsStalenessThreshold,omitempty"`
}

// Config holds the internal operational parameters for the Saturation Controller.
type Config struct {
	queueDepthThreshold       int
	kvCacheUtilThreshold      float64
	metricsStalenessThreshold time.Duration
}

// NewConfig creates the internal configuration from the API specification.
// It applies defaults for unspecified fields and performs strict validation.
func NewConfig(sc *pluginConfig) (*Config, error) {
	c := &Config{
		queueDepthThreshold:       defaultQueueDepthThreshold,
		kvCacheUtilThreshold:      defaultKVCacheUtilThreshold,
		metricsStalenessThreshold: defaultMetricsStalenessThreshold,
	}

	if sc != nil {
		if sc.QueueDepthThreshold != nil {
			c.queueDepthThreshold = *sc.QueueDepthThreshold
		}
		if sc.KVCacheUtilThreshold != nil {
			c.kvCacheUtilThreshold = *sc.KVCacheUtilThreshold
		}
		if sc.MetricsStalenessThreshold != nil {
			c.metricsStalenessThreshold = sc.MetricsStalenessThreshold.Duration
		}
	}

	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid static threshold config: %w", err)
	}

	return c, nil
}

func (c *Config) validate() error {
	// A set point of 0 is valid (implies prioritizing latency/fairness over throughput).
	if c.queueDepthThreshold < 0 {
		return errors.New("queueDepthThreshold must be non-negative")
	}

	// KV Cache is a utilization ratio (0.0 to 1.0].
	// We exclude 0.0 as that implies a backend with no capacity (or a broken metric), which is an invalid configuration
	// for a safety threshold.
	// We include 1.0 as it is technically possible (though operationally saturated).
	if c.kvCacheUtilThreshold <= 0 || c.kvCacheUtilThreshold > 1.0 {
		return errors.New("kvCacheUtilThreshold must be strictly between 0 and 1")
	}

	if c.metricsStalenessThreshold <= 0 {
		return errors.New("metricsStalenessThreshold must be positive")
	}
	return nil
}
