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

// Package scheduling implements request scheduling algorithms.
package scheduling

import (
	"context"
	"fmt"
	"math/rand"

	"sigs.k8s.io/controller-runtime/pkg/log"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
	envutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/env"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

var ErrBackendsSaturated = fmt.Errorf("backend resources exhausted, cannot accommodate request")

// Config holds all the configuration values for the scheduler
type Config struct {
	KVCacheThreshold       float64
	QueueThresholdCritical int
	QueueingThresholdLoRA  int
	LoraAffinityThreshold  float64
}

const (
	// Default values to use if environment variables are not set
	defaultKVCacheThreshold       = 0.8
	defaultQueueThresholdCritical = 5
	defaultQueueingThresholdLoRA  = 128
	defaultLoraAffinityThreshold  = 0.999
)

// LoadConfig loads configuration from environment variables
func LoadConfig() Config {
	// Use a default logger for initial configuration loading
	baseLogger := log.Log.WithName("scheduling-config")

	config := Config{
		KVCacheThreshold:       envutil.GetEnvFloat("KV_CACHE_THRESHOLD", defaultKVCacheThreshold, baseLogger),
		QueueThresholdCritical: envutil.GetEnvInt("QUEUE_THRESHOLD_CRITICAL", defaultQueueThresholdCritical, baseLogger),
		QueueingThresholdLoRA:  envutil.GetEnvInt("QUEUING_THRESHOLD_LORA", defaultQueueingThresholdLoRA, baseLogger),
		LoraAffinityThreshold:  envutil.GetEnvFloat("LORA_AFFINITY_THRESHOLD", defaultLoraAffinityThreshold, baseLogger),
	}

	baseLogger.V(logutil.DEFAULT).Info("Scheduler configuration loaded", "config", config)

	return config
}

var config = LoadConfig()

var (
	lowLatencyFilter = &decisionTreeFilter{
		// Any model server with queue length and KV cache below their respective
		// thresholds is considered to have enough capacity to handle another
		// request.
		current: hasCapacityFilter,
		nextOnSuccess: &decisionTreeFilter{
			current: loRAAffinityFilter,
			nextOnSuccessOrFailure: &decisionTreeFilter{
				current: leastQueueFilter,
				nextOnSuccessOrFailure: &decisionTreeFilter{
					current: leastKVCacheFilter,
				},
			},
		},
		// If all pods are saturated, we cannot serve the request.
		nextOnFailure: backendsSaturatedFilter,
	}

	hasCapacityFilter = &basicFilter{
		name:   "has capacity for requests",
		filter: toFilterFunc(queueThresholdPredicate(config.QueueThresholdCritical).and(kvCacheThresholdPredicate(config.KVCacheThreshold))),
	}

	backendsSaturatedFilter = &basicFilter{
		name: "backends saturated",
		filter: func(ctx *types.Context, pods []*types.PodMetrics) ([]*types.PodMetrics, error) {
			ctx.Logger.V(logutil.DEFAULT).Info("All backends are saturated, cannot accomodate request", "request", ctx.Req)
			return []*types.PodMetrics{}, ErrBackendsSaturated
		},
	}
)

func NewScheduler(datastore Datastore) *Scheduler {
	return &Scheduler{
		datastore: datastore,
		filter:    lowLatencyFilter,
	}
}

type Scheduler struct {
	datastore Datastore
	filter    Filter
}

type Datastore interface {
	PodGetAll() []backendmetrics.PodMetrics
}

// Schedule finds the target pod based on metrics and the requested lora adapter.
func (s *Scheduler) Schedule(ctx context.Context, req *types.LLMRequest) (targetPod types.Pod, err error) {
	logger := log.FromContext(ctx).WithValues("request", req)

	// Snapshot pod metrics from the datastore to:
	// 1. Reduce concurrent access to the datastore.
	// 2. Ensure consistent data during the scheduling operation of a request.
	sCtx := types.NewContext(ctx, req, types.ToSchedulerPodMetrics(s.datastore.PodGetAll()))
	logger.V(logutil.DEBUG).Info(fmt.Sprintf("Scheduling a request. Metrics: %+v", sCtx.PodsSnapshot))

	pods, err := s.filter.Filter(sCtx, sCtx.PodsSnapshot)
	if err != nil || len(pods) == 0 {
		return nil, fmt.Errorf("failed to apply filter, resulted in %v pods, this should never happen: %w", len(pods), err)
	}
	logger.V(logutil.DEBUG).Info(fmt.Sprintf("Selecting a random pod from %d candidates: %+v", len(pods), pods))
	i := rand.Intn(len(pods))
	return pods[i], nil
}
