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

package plugins

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const (
	PodQueueMetricsPluginName = "PodQueueMetrics"
)

// PodQueueMetricsPlugin is responsible for updating per-pod queuing metrics
// like arrival rate and sojourn time EWMAs.
type PodQueueMetricsPlugin struct{}

// NewPodQueueMetricsPlugin creates a new PodQueueMetricsPlugin.
func NewPodQueueMetricsPlugin() *PodQueueMetricsPlugin {
	return &PodQueueMetricsPlugin{}
}

var _ PreRequest = &PodQueueMetricsPlugin{}
var _ ResponseComplete = &PodQueueMetricsPlugin{}

// TypedName returns the type and name of the plugin.
func (p *PodQueueMetricsPlugin) TypedName() plugins.TypedName {
	return plugins.TypedName{Type: "Metrics", Name: PodQueueMetricsPluginName}
}

// PreRequest is called before the request is sent to the backend.
// It updates the arrival rate EWMA for the selected pod.
func (p *PodQueueMetricsPlugin) PreRequest(
	ctx context.Context,
	_ *types.LLMRequest,
	schedulingResult *types.SchedulingResult,
) {
	targetPod := schedulingResult.ProfileResults[schedulingResult.PrimaryProfileName].TargetPods[0]
	metrics := targetPod.GetEWMAMetrics()
	arrivalRate := metrics.UpdateArrivalRateEWMA(time.Now())
	log.FromContext(ctx).V(logutil.TRACE).Info("PodQueueMetricsPlugin.PreRequest: Updated arrival rate EWMA",
		"pod", targetPod.GetPod().NamespacedName, "newRate", arrivalRate)
}

// ResponseComplete is called after the response is received from the backend.
// It updates the sojourn time mean and variance EWMAs for the pod.
func (p *PodQueueMetricsPlugin) ResponseComplete(
	ctx context.Context,
	_ *types.LLMRequest,
	response *Response,
	targetPod types.Pod,
) {
	metrics := targetPod.GetEWMAMetrics()
	mean, variance := metrics.UpdateSojournTimeEWMA(response.SojournTime)
	log.FromContext(ctx).V(logutil.TRACE).Info("PodQueueMetricsPlugin.PostResponse: Updated sojourn time EWMA",
		"pod", targetPod.GetPod().NamespacedName,
		"sojournTime", response.SojournTime,
		"newMean", mean,
		"newVariance", variance)
}
