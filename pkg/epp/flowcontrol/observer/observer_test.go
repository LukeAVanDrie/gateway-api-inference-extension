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

package observer

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"k8s.io/component-base/metrics/testutil"
	testingclock "k8s.io/utils/clock/testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
)

// --- Test Doubles (Fakes) ---

type fakeHandle struct {
	plugins.Handle
	plugins []plugins.Plugin
}

func (h *fakeHandle) GetAllPlugins() []plugins.Plugin { return h.plugins }
func (h *fakeHandle) Context() context.Context        { return context.Background() }

type fakeFairnessMetric struct {
	plugins.Plugin
	name   string
	values map[types.FlowKey]float64
}

func (m *fakeFairnessMetric) GetAllValues() map[types.FlowKey]float64             { return m.values }
func (m *fakeFairnessMetric) GetValue(types.FlowKey) float64                      { return 0 }
func (m *fakeFairnessMetric) GetValues([]types.FlowKey) map[types.FlowKey]float64 { return nil }
func (m *fakeFairnessMetric) TypedName() plugins.TypedName {
	return plugins.TypedName{Type: framework.FairnessMetricType, Name: m.name}
}

type nonFairnessMetricPlugin struct{ plugins.Plugin }

func (p *nonFairnessMetricPlugin) TypedName() plugins.TypedName {
	return plugins.TypedName{Type: "NotAFairnessMetric", Name: "other-plugin"}
}

// --- Test Helper ---

// getHistogramVecObserverCount returns the sample count for a given histogram and labels.
// This is useful for asserting that an observation was made.
func getHistogramVecObserverCount(t *testing.T, h *prometheus.HistogramVec, labelValues ...string) uint64 {
	t.Helper()
	m, err := h.GetMetricWithLabelValues(labelValues...)
	if err != nil {
		return 0 // Metric does not exist, so count is 0.
	}

	metricDto := &dto.Metric{}
	err = m.(prometheus.Histogram).Write(metricDto)
	require.NoError(t, err, "failed to write histogram DTO for metric %v with labels %v", h, labelValues)

	return metricDto.GetHistogram().GetSampleCount()
}

// setupTestObserver is a helper to DRY the test setup.
func setupTestObserver(t *testing.T) (*testingclock.FakeClock, *fakeFairnessMetric) {
	t.Helper()
	metrics.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	const scrapeInterval = 1 * time.Second
	fakeClock := testingclock.NewFakeClock(time.Now())

	metric := &fakeFairnessMetric{name: "turns", values: make(map[types.FlowKey]float64)}
	metricsToObserve := []framework.FairnessMetric{metric}

	go runScraper(ctx, fakeClock, scrapeInterval, metricsToObserve)
	require.Eventually(t, fakeClock.HasWaiters, 1*time.Second, 10*time.Millisecond,
		"timed out waiting for the observer's ticker to be created and waited on")
	return fakeClock, metric
}

// --- Tests ---

func TestStart(t *testing.T) {
	// This test cannot run in parallel because it relies on the global Prometheus registry.
	metrics.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	t.Run("does nothing if no fairness metrics are registered", func(t *testing.T) {
		handle := &fakeHandle{plugins: []plugins.Plugin{&nonFairnessMetricPlugin{}}}
		fakeClock := testingclock.NewFakeClock(time.Now())

		start(ctx, handle, 1*time.Second, fakeClock)

		// The most reliable way to prove nothing happened is to show that no tickers were created on the fake clock.
		require.False(t, fakeClock.HasWaiters(), "Start should not create a ticker if no FairnessMetrics are found")
	})

	t.Run("discovers and starts scraper for registered fairness metrics", func(t *testing.T) {
		metric := &fakeFairnessMetric{name: "turns", values: map[types.FlowKey]float64{{ID: "A"}: 100}}
		handle := &fakeHandle{
			plugins: []plugins.Plugin{
				metric,
				&nonFairnessMetricPlugin{}, // Should be ignored.
			},
		}
		fakeClock := testingclock.NewFakeClock(time.Now())

		start(ctx, handle, 1*time.Second, fakeClock)

		// Verify that a ticker was created, proving the scraper was launched.
		require.Eventually(t, fakeClock.HasWaiters, 1*time.Second, 10*time.Millisecond, "scraper ticker was not created")

		// Advance the clock and verify that the metric was scraped.
		fakeClock.Step(1 * time.Second)
		require.Eventually(t, func() bool {
			// Check for any observation; the logic is tested more deeply elsewhere.
			return getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "0") == 1
		}, 100*time.Millisecond, 10*time.Millisecond, "metric was not scraped after ticker fired")
	})

	t.Run("uses default scrape interval when provided interval is zero or negative", func(t *testing.T) {
		metrics.Reset()
		metric := &fakeFairnessMetric{name: "turns", values: map[types.FlowKey]float64{{ID: "A"}: 100}}
		handle := &fakeHandle{plugins: []plugins.Plugin{metric}}
		fakeClock := testingclock.NewFakeClock(time.Now())
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		start(ctx, handle, 0, fakeClock) // Call with an invalid interval.

		require.Eventually(t, fakeClock.HasWaiters, 1*time.Second, 10*time.Millisecond, "scraper ticker was not created")

		// Step by less than the default interval.
		fakeClock.Step(defaultScrapeInterval - 1*time.Nanosecond)
		time.Sleep(20 * time.Millisecond) // Give goroutine a chance.
		require.Equal(t, uint64(0), getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "0"),
			"scraper should not have run yet")

		// Step just a little more to cross the interval boundary.
		fakeClock.Step(1 * time.Nanosecond)
		require.Eventually(t, func() bool {
			return getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "0") == 1
		}, 100*time.Millisecond, 10*time.Millisecond, "metric was not scraped after default interval fired")
	})
}

func TestObserver_EqualityAndInequality(t *testing.T) {
	fakeClock, metric := setupTestObserver(t)
	const scrapeInterval = 1 * time.Second

	// Scenario 1: Perfect Equality
	metric.values = map[types.FlowKey]float64{
		{ID: "A", Priority: 0}: 100,
		{ID: "B", Priority: 0}: 100,
	}
	fakeClock.Step(scrapeInterval)

	require.Eventually(t, func() bool {
		ratio, err := testutil.GetGaugeMetricValue(metrics.FairnessMaxMinRatioCurrent.WithLabelValues("turns", "0"))
		require.NoError(t, err, "fetching MaxMinRatioCurrent for turns/0 should not error")
		return ratio == 1.0 && getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "0") == 1
	}, 100*time.Millisecond, 10*time.Millisecond, "metrics for perfect equality were not recorded as expected")

	// Scenario 2: High Inequality
	metric.values = map[types.FlowKey]float64{
		{ID: "A", Priority: 0}: 10,
		{ID: "B", Priority: 0}: 100, // Ratio = 10.0
	}
	fakeClock.Step(scrapeInterval)

	require.Eventually(t, func() bool {
		ratio, err := testutil.GetGaugeMetricValue(metrics.FairnessMaxMinRatioCurrent.WithLabelValues("turns", "0"))
		require.NoError(t, err, "fetching MaxMinRatioCurrent for turns/0 should not error")
		return ratio == 10.0 && getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "0") == 2
	}, 100*time.Millisecond, 10*time.Millisecond, "metrics for high inequality were not recorded as expected")
}

func TestObserver_EdgeCases(t *testing.T) {
	fakeClock, metric := setupTestObserver(t)
	const scrapeInterval = 1 * time.Second

	// Scenario 1: Single flow (perfectly fair)
	metric.values = map[types.FlowKey]float64{{ID: "A", Priority: 0}: 1000}
	fakeClock.Step(scrapeInterval)

	require.Eventually(t, func() bool {
		ratio, err := testutil.GetGaugeMetricValue(metrics.FairnessMaxMinRatioCurrent.WithLabelValues("turns", "0"))
		require.NoError(t, err, "fetching MaxMinRatioCurrent for turns/0 should not error")
		return ratio == 1.0 && getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "0") == 1
	}, 100*time.Millisecond, 10*time.Millisecond, "metrics for single-flow were not recorded as expected")

	// Scenario 2: Empty distribution (should do nothing)
	metric.values = map[types.FlowKey]float64{}
	fakeClock.Step(scrapeInterval)

	// We sleep briefly to ensure the scraper has run and done nothing.
	time.Sleep(20 * time.Millisecond)
	// The count should still be 1 from the previous step, as no new metric was recorded.
	require.Equal(t, uint64(1), getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "0"), "Gini count should not increment for an empty distribution")
}

func TestObserver_MultiplePriorities(t *testing.T) {
	fakeClock, metric := setupTestObserver(t)
	const scrapeInterval = 1 * time.Second
	metric.values = map[types.FlowKey]float64{
		// Priority 0: High inequality
		{ID: "A", Priority: 0}: 10,
		{ID: "B", Priority: 0}: 100, // Ratio = 10
		// Priority 1: Perfect equality
		{ID: "C", Priority: 1}: 50,
		{ID: "D", Priority: 1}: 50, // Ratio = 1
		// Priority 2: Single flow (perfectly fair)
		{ID: "E", Priority: 2}: 200, // Ratio = 1
	}

	fakeClock.Step(scrapeInterval)

	require.Eventually(t, func() bool {
		ratioP0, err := testutil.GetGaugeMetricValue(metrics.FairnessMaxMinRatioCurrent.WithLabelValues("turns", "0"))
		require.NoError(t, err, "fetching MaxMinRatioCurrent for turns/0 should not error")

		ratioP1, err := testutil.GetGaugeMetricValue(metrics.FairnessMaxMinRatioCurrent.WithLabelValues("turns", "1"))
		require.NoError(t, err, "fetching MaxMinRatioCurrent for turns/1 should not error")

		ratioP2, err := testutil.GetGaugeMetricValue(metrics.FairnessMaxMinRatioCurrent.WithLabelValues("turns", "2"))
		require.NoError(t, err, "fetching MaxMinRatioCurrent for turns/2 should not error")

		return ratioP0 == 10.0 && ratioP1 == 1.0 && ratioP2 == 1.0
	}, 100*time.Millisecond, 10*time.Millisecond, "metrics for multiple priorities were not recorded correctly")

	// Check Gini counts for each priority
	require.Equal(t, uint64(1), getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "0"), "Gini count for turns/0 should be 1")
	require.Equal(t, uint64(1), getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "1"), "Gini count for turns/1 should be 1")
	require.Equal(t, uint64(1), getHistogramVecObserverCount(t, metrics.FairnessGiniCoefficient, "turns", "2"), "Gini count for turns/2 should be 1")
}
