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

// Package observer provides components for monitoring the state and performance of the Flow Control layer with respect
// to FairnessMetrics.
package observer

import (
	"context"
	"math"
	"strconv"
	"time"

	"k8s.io/utils/clock"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

const (
	// defaultScrapeInterval is the default frequency for scraping fairness metrics.
	defaultScrapeInterval = time.Second
)

// Start begins the background process that periodically observes all registered FairnessMetric plugins.
// It should be called once during application startup.
func Start(ctx context.Context, handle plugins.Handle, scrapeInterval time.Duration) {
	start(ctx, handle, scrapeInterval, clock.RealClock{})
}

// start is the internal, testable implementation of the observer's startup logic.
// It discovers all registered plugins that implement the framework.FairnessMetric interface and launches a single
// goroutine to scrape them at the specified interval.
func start(ctx context.Context, handle plugins.Handle, scrapeInterval time.Duration, clk clock.WithTicker) {
	// Discover all FairnessMetric instances from the plugin handle.
	metricsToObserve := make([]framework.FairnessMetric, 0)
	for _, plugin := range handle.GetAllPlugins() {
		if metric, ok := plugin.(framework.FairnessMetric); ok {
			metricsToObserve = append(metricsToObserve, metric)
		}
	}

	if len(metricsToObserve) == 0 {
		return // No fairness metrics are configured, so there is nothing to observe.
	}

	if scrapeInterval <= 0 {
		scrapeInterval = defaultScrapeInterval
	}
	go runScraper(ctx, clk, scrapeInterval, metricsToObserve)
}

// runScraper is the main loop for the observer.
// It wakes up periodically, scrapes all discovered metrics, and then sleeps.
func runScraper(ctx context.Context, clk clock.WithTicker, interval time.Duration, metrics []framework.FairnessMetric) {
	ticker := clk.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			for _, metric := range metrics {
				observe(metric)
			}
		}
	}
}

// observe performs a single scrape of a FairnessMetric, calculating and exporting all relevant fairness statistics.
func observe(metric framework.FairnessMetric) {
	metricName := metric.TypedName().Name
	allValues := metric.GetAllValues()

	if len(allValues) == 0 {
		return // Nothing to observe
	}

	valuesByPriority := make(map[int][]float64)
	for key, value := range allValues {
		pLevel := key.Priority
		valuesByPriority[pLevel] = append(valuesByPriority[pLevel], value)
	}

	for p, distribution := range valuesByPriority {
		priority := strconv.Itoa(p)
		if len(distribution) < 2 {
			// With 0 or 1 flows in this priority, fairness is perfect by definition.
			metrics.RecordFairnessGiniCoefficient(metricName, priority, 0.0)
			metrics.RecordFairnessMaxMinRatio(metricName, priority, 1.0)
			continue
		}

		minVal, maxVal := math.MaxFloat64, -1.0
		hasNonZero := false

		for _, v := range distribution {
			if v > 0 {
				hasNonZero = true
				if v < minVal {
					minVal = v
				}
			}
			if v > maxVal {
				maxVal = v
			}
		}

		gini, err := giniCoefficient(distribution)
		if err == nil {
			metrics.RecordFairnessGiniCoefficient(metricName, priority, gini)
		}

		var ratio = 1.0
		if hasNonZero && minVal > 0 && maxVal > 0 {
			ratio = maxVal / minVal
		}
		metrics.RecordFairnessMaxMinRatio(metricName, priority, ratio)
	}
}
