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

// Package epp contains integration tests for the ext proc while faking the backend pods.
package epp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/testing/protocmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsutils "k8s.io/component-base/metrics/testutil"

	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metadata"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/requestcontrol"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationcontroller/framework/plugins/staticthreshold"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/multi/prefix"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/picker"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/profile"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/scorer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/server"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
	requtil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/request"
	epptestutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"
	integrationutils "sigs.k8s.io/gateway-api-inference-extension/test/integration"
	"sigs.k8s.io/yaml"
)

const (
	// Test Infrastructure
	testPoolName = "vllm-llama3-8b-instruct-pool"

	// Model Names
	modelMyModel         = "my-model"
	modelMyModelTarget   = "my-model-12345"
	modelToBeWritten     = "model-to-be-rewritten"
	modelAfterRewrite    = "rewritten-model"
	modelSQLLora         = "sql-lora"
	modelSQLLoraTarget   = "sql-lora-1fdg2"
	modelSheddable       = "sql-lora-sheddable"
	modelSheddableTarget = "sql-lora-1fdg3"
	modelDirect          = "direct-model"
)

var (
	k8sClient k8sclient.Client
	testEnv   *envtest.Environment
	logger    = logutil.NewTestLogger().V(logutil.VERBOSE)
)

func TestMain(m *testing.M) {
	cleanup := BeforeSuite()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

type label struct {
	name,
	value string
}

func labelsToString(labels []label) string {
	var sb strings.Builder
	i := 0
	for _, l := range labels {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf("%s=%q", l.name, l.value))
		i++
	}
	return sb.String()
}

func inferenceObjectiveRequestTotal(labels []label) string {
	return fmt.Sprintf(`
		# HELP inference_objective_request_total [ALPHA] Counter of inference objective requests broken out for each model and target model.
		# TYPE inference_objective_request_total counter
		inference_objective_request_total{%s} 1
		`, labelsToString(labels))
}

func inferencePoolReadyPods(v int, labels []label) string {
	return fmt.Sprintf(`
		# HELP inference_pool_ready_pods [ALPHA] The number of ready pods in the inference server pool.
		# TYPE inference_pool_ready_pods gauge
		inference_pool_ready_pods{%s} %d
		`, labelsToString(labels), v)
}

func TestFullDuplexStreamed_KubeInferenceObjectiveRequest(t *testing.T) {
	tests := []struct {
		name              string
		requests          []*extProcPb.ProcessingRequest
		pods              map[*backend.Pod]*backendmetrics.MetricsState
		wantResponses     []*extProcPb.ProcessingResponse
		wantMetrics       map[string]string
		wantErr           bool
		immediateResponse *extProcPb.ImmediateResponse
	}{
		// Request flow tests
		{
			name:     "select lower queue and kv cache, no active lora",
			requests: integrationutils.GenerateStreamedRequestSet(logger, "test1", modelMyModel, modelMyModelTarget, nil),
			// Pod 1 will be picked because it has relatively low queue size and low KV cache.
			pods: newPodStates(
				podState{index: 0, queueSize: 3, kvCacheUsage: 0.2},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.1},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.2},
			),
			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelMyModel},
					{"target_model_name", modelMyModelTarget},
				}),
				"inference_pool_ready_pods": inferencePoolReadyPods(3, []label{
					{"name", testPoolName},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.2:8000",
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test1","temperature":0}`, modelMyModelTarget),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
		{
			name: "invalid json; return body",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:   "hi",
										Value: "mom",
									},
								},
							},
						},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{Body: []byte("no healthy upstream"), EndOfStream: true},
					},
				},
			},
			// Pod 1 will be picked because it has relatively low queue size, the requested model active, and low KV cache.
			pods: newPodStates(
				podState{index: 0, queueSize: 0, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.1, activeModels: []string{"foo", modelSQLLoraTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
			),
			wantErr: false,
			wantResponses: integrationutils.NewImmediateErrorResponse(
				envoyTypePb.StatusCode_BadRequest,
				"inference gateway: BadRequest - Error unmarshaling request body",
			),
		},
		{
			name:     "select active lora, low queue",
			requests: integrationutils.GenerateStreamedRequestSet(logger, "test2", modelSQLLora, modelSQLLoraTarget, nil),
			// Pod 1 will be picked because it has relatively low queue size, the requested model active, and low KV cache.
			pods: newPodStates(
				podState{index: 0, queueSize: 0, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.1, activeModels: []string{"foo", modelSQLLoraTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
			),

			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelSQLLora},
					{"target_model_name", modelSQLLoraTarget},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.2:8000",
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test2","temperature":0}`, modelSQLLoraTarget),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
		{
			name:     "select lora despite higher kv cache usage",
			requests: integrationutils.GenerateStreamedRequestSet(logger, "test3", modelSQLLora, modelSQLLoraTarget, nil),
			// Pod 2 will be picked despite NOT having the requested model active as it is above the affinity for queue size.
			// Also it is critical, so we should still admit the request despite all queue sizes being greater than the queue
			// size threshold.
			pods: newPodStates(
				podState{index: 0, queueSize: 10, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
				podState{index: 1, queueSize: 10, kvCacheUsage: 0.4, activeModels: []string{"foo", modelSQLLoraTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.3, activeModels: []string{"foo"}},
			),
			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelSQLLora},
					{"target_model_name", modelSQLLoraTarget},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.2:8000",
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test3","temperature":0}`, modelSQLLoraTarget),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
		{
			name:     "don't shed requests by default",
			requests: integrationutils.GenerateStreamedRequestSet(logger, "test4", modelSQLLora, modelSQLLoraTarget, nil),
			// pod 0: excluded; above queue size threshold
			// pod 1: excluded; above KV cache threshold
			// pod 2: excluded; above queue size threshold
			pods: newPodStates(
				podState{index: 0, queueSize: 6, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar", modelSQLLoraTarget}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.85, activeModels: []string{"foo"}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.9, activeModels: []string{"foo"}},
			),
			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelSQLLora},
					{"target_model_name", modelSQLLoraTarget},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.1:8000",
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test4","temperature":0}`, modelSQLLoraTarget),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
		{
			name: "body sent over multiple requests, noncritical, but one server has capacity, do not shed",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:   "hi",
										Value: "mom",
									},
									{
										Key:   metadata.ObjectiveKey,
										Value: modelSheddable,
									},
									{
										Key:   metadata.ModelNameRewriteKey,
										Value: modelSheddableTarget,
									},
									{
										Key:   requtil.RequestIdHeaderKey,
										Value: "test-request-id",
									},
								},
							},
						},
					},
				}, {
					Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{Body: []byte("{\"max_tokens\":100,\"model\":\"sql-lo"), EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{Body: []byte("ra-sheddable\",\"prompt\":\"test6\",\"temperature\":0}"), EndOfStream: true},
					},
				},
			},
			// Pod 1 will be picked because it has relatively low queue size and low KV cache.
			pods: newPodStates(
				podState{index: 0, queueSize: 4, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar", modelSheddableTarget}},
				podState{index: 1, queueSize: 4, kvCacheUsage: 0.85, activeModels: []string{"foo", modelSheddableTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.9, activeModels: []string{"foo", modelSheddableTarget}},
			),
			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelSheddable},
					{"target_model_name", modelSheddableTarget},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.1:8000",
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test6","temperature":0}`, modelSheddableTarget),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
		{
			name: "inferenceobjective's modelName is not translated, passthrough",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:   "hi",
										Value: "mom",
									},
									{
										Key:   metadata.ObjectiveKey,
										Value: modelDirect,
									},
									{
										Key:   metadata.ModelNameRewriteKey,
										Value: modelDirect,
									},
									{
										Key:   metadata.ModelNameRewriteKey,
										Value: modelDirect,
									},
									{
										Key:   requtil.RequestIdHeaderKey,
										Value: "test-request-id",
									},
								},
							},
						},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{Body: []byte("{\"max_tokens\":100,\"model\":\"direct-"), EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{Body: []byte("model\",\"prompt\":\"test6\",\"temperature\":0}"), EndOfStream: true},
					},
				},
			},
			// pod 0: selected due to low queue size and kv cache usage
			pods: newPodStates(
				podState{index: 0, queueSize: 4, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar", modelSheddableTarget}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.85, activeModels: []string{"foo", modelSheddableTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.9, activeModels: []string{"foo", modelSheddableTarget}},
			),
			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelDirect},
					{"target_model_name", modelDirect},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.1:8000",
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test6","temperature":0}`, modelDirect),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
		// Response flow tests
		{
			name: "responsebody sent over multiple requests, content-type is json, buffer",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_ResponseHeaders{
						ResponseHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:   "content-type",
										Value: "application/json",
									},
								},
							},
						},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{Body: []byte("{\"max_tokens\":100,\"model\":\"sql-lo"), EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{Body: []byte("ra-sheddable\",\"prompt\":\"test6\",\"temperature\":0}"), EndOfStream: true},
					},
				},
			},
			// pod 0: selected
			// pod 1: excluded; above KV cache threshold
			// pod 2: excluded; above queue size threshold
			pods: newPodStates(
				podState{index: 0, queueSize: 4, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar", modelSheddableTarget}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.85, activeModels: []string{"foo", modelSheddableTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.9, activeModels: []string{"foo", modelSheddableTarget}},
			),
			wantErr: false,
			wantResponses: integrationutils.NewResponseBufferedResponse(
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test6","temperature":0}`, modelSheddable),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "x-went-into-resp-headers",
						RawValue: []byte("true"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "content-type",
						RawValue: []uint8("application/json"),
					},
				},
			),
		},
		{
			name: "Response is invalid json; return body",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_ResponseHeaders{
						ResponseHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:   "content-type",
										Value: "application/json",
									},
								},
							},
						},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{Body: []byte("no healthy upstream"), EndOfStream: true},
					},
				},
			},
			// pod 0: selected
			// pod 1: excluded; above KV cache threshold
			// pod 2: excluded; above queue size threshold
			pods: newPodStates(
				podState{index: 0, queueSize: 4, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar", modelSheddableTarget}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.85, activeModels: []string{"foo", modelSheddableTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.9, activeModels: []string{"foo", modelSheddableTarget}},
			),
			wantErr: false,
			wantResponses: integrationutils.NewResponseBufferedResponse(
				"no healthy upstream",
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "x-went-into-resp-headers",
						RawValue: []byte("true"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "content-type",
						RawValue: []uint8("application/json"),
					},
				},
			),
		},
		{
			name: "responsebody sent over a single request, but empty body with EndOfStream in the second request(this is how envoy operates); content-type is json, buffer",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_ResponseHeaders{
						ResponseHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:   "content-type",
										Value: "application/json",
									},
								},
							},
						},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{Body: []byte("{\"max_tokens\":100,\"model\":\"sql-lora-sheddable\",\"prompt\":\"test6\",\"temperature\":0}"), EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{Body: []byte(""), EndOfStream: true},
					},
				},
			},
			// pod 0: selected
			// pod 1: excluded; above KV cache threshold
			// pod 2: excluded; above queue size threshold
			pods: newPodStates(
				podState{index: 0, queueSize: 4, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar", modelSheddableTarget}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.85, activeModels: []string{"foo", modelSheddableTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.9, activeModels: []string{"foo", modelSheddableTarget}},
			),
			wantErr: false,
			wantResponses: integrationutils.NewResponseBufferedResponse(
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test6","temperature":0}`, modelSheddable),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "x-went-into-resp-headers",
						RawValue: []byte("true"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "content-type",
						RawValue: []uint8("application/json"),
					},
				},
			),
		},
		{
			name: "responsebody sent over a single request, but empty body with EndOfStream in the second request(this is how envoy operates); content-type is json, buffer",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_ResponseHeaders{
						ResponseHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:      "content-type",
										RawValue: []byte("text/event-stream"),
									},
									{
										Key:      "status",
										RawValue: []byte("200"),
									},
								},
							},
						},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{
							Body:        []byte(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"NEVER","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`),
							EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{
							Body:        []byte(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"GONNA","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`),
							EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{
							Body:        []byte(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"GIVE","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`),
							EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{
							Body:        []byte(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"YOU","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`),
							EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{
							Body:        []byte(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"UP","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`),
							EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{
							Body:        []byte("data: {\"id\":\"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9\",\"object\":\"text_completion\",\"created\":1741379018,\"model\":\"food-review-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"total_tokens\":17,\"completion_tokens\":10}}\ndata: [DONE]"),
							EndOfStream: false},
					},
				},
				{
					Request: &extProcPb.ProcessingRequest_ResponseBody{
						ResponseBody: &extProcPb.HttpBody{
							Body:        []byte(""),
							EndOfStream: true},
					},
				},
			},
			wantMetrics: map[string]string{`inference_objective_input_tokens`: `
					# HELP inference_objective_input_tokens [ALPHA] Inference objective input token count distribution for requests in each model.
					# TYPE inference_objective_input_tokens histogram
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="1"} 0
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="8"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="16"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="32"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="64"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="128"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="256"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="512"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="1024"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="2048"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="4096"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="8192"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="16384"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="32778"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="65536"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="131072"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="262144"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="524288"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="1.048576e+06"} 1
		            inference_objective_input_tokens_bucket{model_name="",target_model_name="",le="+Inf"} 1
		            inference_objective_input_tokens_sum{model_name="",target_model_name=""} 7
		            inference_objective_input_tokens_count{model_name="",target_model_name=""} 1
					`,
				`inference_objective_normalized_time_per_output_token_seconds`: `
					# HELP inference_objective_normalized_time_per_output_token_seconds [ALPHA] Inference objective latency divided by number of output tokens in seconds for each model and target model.
					# TYPE inference_objective_normalized_time_per_output_token_seconds histogram
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.001"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.002"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.005"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.01"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.02"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.05"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.1"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.2"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="0.5"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="1"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="2"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="5"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="10"} 0
					inference_objective_normalized_time_per_output_token_seconds_bucket{model_name="",target_model_name="",le="+Inf"} 1
					inference_objective_normalized_time_per_output_token_seconds_sum{model_name="",target_model_name=""} 9.223372036854776e+08
					inference_objective_normalized_time_per_output_token_seconds_count{model_name="",target_model_name=""} 1
			`},
			wantResponses: []*extProcPb.ProcessingResponse{
				integrationutils.NewResponseHeaders(
					&configPb.HeaderValueOption{
						Header: &configPb.HeaderValue{
							Key:      "x-went-into-resp-headers",
							RawValue: []byte("true"),
						},
					},
					&configPb.HeaderValueOption{
						Header: &configPb.HeaderValue{
							Key:      "content-type",
							RawValue: []byte("text/event-stream"),
						},
					},
					&configPb.HeaderValueOption{
						Header: &configPb.HeaderValue{
							Key:      "status",
							RawValue: []byte("200"),
						},
					},
				),
				integrationutils.NewResponseStreamChunk(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"NEVER","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`, false),
				integrationutils.NewResponseStreamChunk(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"GONNA","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`, false),
				integrationutils.NewResponseStreamChunk(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"GIVE","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`, false),
				integrationutils.NewResponseStreamChunk(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"YOU","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`, false),
				integrationutils.NewResponseStreamChunk(`data: {"id":"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9","object":"text_completion","created":1741379018,"model":"food-review-1","choices":[{"index":0,"text":"UP","logprobs":null,"finish_reason":null,"stop_reason":null}],"usage":null}`, false),
				integrationutils.NewResponseStreamChunk("data: {\"id\":\"cmpl-0fee233f-7d56-404a-acd3-4dad775d03d9\",\"object\":\"text_completion\",\"created\":1741379018,\"model\":\"food-review-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"total_tokens\":17,\"completion_tokens\":10}}\ndata: [DONE]", false),
				integrationutils.NewResponseStreamChunk("", true),
			},
		},
		// Bodyless Request test
		{
			name: "simple GET Request",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:      "content-type",
										RawValue: []byte("text/event-stream"),
									},
									{
										Key:      "status",
										RawValue: []byte("200"),
									},
								},
							},
							EndOfStream: true,
						},
					},
				},
			},
			wantResponses: []*extProcPb.ProcessingResponse{},
			pods: newPodStates(
				podState{index: 0, queueSize: 4, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar", modelSheddableTarget}},
			),
			wantMetrics: map[string]string{},
		},
		{
			name: "select active lora with subsetting tag, all pods available",
			requests: integrationutils.GenerateStreamedRequestSet(
				logger,
				"test2",
				modelSQLLora,
				modelSQLLoraTarget,
				[]string{"192.168.1.1:8000", "192.168.1.2:8000", "192.168.1.3:8000"}),
			// Pod 1 will be picked because it has relatively low queue size, the requested model active, low KV cache, and within subset.
			pods: newPodStates(
				podState{index: 0, queueSize: 0, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.1, activeModels: []string{"foo", modelSQLLoraTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
			),

			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelSQLLora},
					{"target_model_name", modelSQLLoraTarget},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.2:8000",
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test2","temperature":0}`, modelSQLLoraTarget),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
		{
			name: "select active lora with subsetting tag, some pods match",
			requests: integrationutils.GenerateStreamedRequestSet(
				logger,
				"test2",
				modelSQLLora,
				modelSQLLoraTarget,
				[]string{"192.168.1.3:8000"}),
			// Pod 3 has high queue and kv cache utilization, but it will still be picked because it is the only one matching subsetting target.
			pods: newPodStates(
				podState{index: 0, queueSize: 0, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.1, activeModels: []string{"foo", modelSQLLoraTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
			),

			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelSQLLora},
					{"target_model_name", modelSQLLoraTarget},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.3:8000",
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test2","temperature":0}`, modelSQLLoraTarget),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
		{
			name: "select active lora with subsetting tag, no pods available",
			requests: integrationutils.GenerateStreamedRequestSet(
				logger,
				"test2",
				modelSQLLora,
				modelSQLLoraTarget,
				[]string{"192.168.1.4:8000", "192.168.1.5:8000", "192.168.1.6:8000"}),
			// No pods will be picked as none are within the subset.
			pods: newPodStates(
				podState{index: 0, queueSize: 0, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
				podState{index: 1, queueSize: 0, kvCacheUsage: 0.1, activeModels: []string{"foo", modelSQLLoraTarget}},
				podState{index: 2, queueSize: 10, kvCacheUsage: 0.2, activeModels: []string{"foo", "bar"}},
			),

			wantMetrics: map[string]string{},
			wantErr:     true,
			wantResponses: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &envoyTypePb.HttpStatus{
								Code: envoyTypePb.StatusCode_ServiceUnavailable,
							},
							Body: []byte("inference gateway: ServiceUnavailable - failed to find candidate pods for serving the request"),
						},
					},
				},
			},
		},
		{
			name: "no backend pods are available",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &configPb.HeaderMap{
								Headers: []*configPb.HeaderValue{
									{
										Key:      "content-type",
										RawValue: []byte("text/event-stream"),
									},
									{
										Key:      "status",
										RawValue: []byte("200"),
									},
								},
							},
							EndOfStream: true,
						},
					},
				},
			},
			pods:        nil,
			wantMetrics: map[string]string{},
			wantErr:     true,
			wantResponses: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &envoyTypePb.HttpStatus{
								Code: envoyTypePb.StatusCode_InternalServerError,
							},
							Body: []byte("inference gateway: Internal - no pods available in datastore"),
						},
					},
				},
			},
		},
		{
			name: "request don't contains invalid payload, model not exist",
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{
							Body:        []byte(`{"hello":"world"}`),
							EndOfStream: true},
					},
				},
			},
			wantErr:     true,
			wantMetrics: map[string]string{},
			wantResponses: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &envoyTypePb.HttpStatus{
								Code: envoyTypePb.StatusCode_BadRequest,
							},
							Body: []byte("inference gateway: BadRequest - model not found in request body"),
						},
					},
				},
			},
		},
		{
			name:     "rewrite request model",
			requests: integrationutils.GenerateStreamedRequestSet(logger, "test-rewrite", modelToBeWritten, modelToBeWritten, nil),
			// Pod 0 will be picked.
			// Expected flow:
			// 1. Request asks for "model-to-be-rewritten"
			// 2. Rewrite rule transforms "model-to-be-rewritten" -> "rewritten-model"
			// 3. EPP sends request to backend with model "rewritten-model"
			pods: newPodStates(
				podState{index: 0, queueSize: 0, kvCacheUsage: 0.1, activeModels: []string{"foo", "rewritten-model"}},
			),
			wantMetrics: map[string]string{
				"inference_objective_request_total": inferenceObjectiveRequestTotal([]label{
					{"model_name", modelToBeWritten},
					{"target_model_name", modelAfterRewrite},
				}),
			},
			wantErr: false,
			wantResponses: integrationutils.NewRequestBufferedResponse(
				"192.168.1.1:8000",
				// Note: The prompt remains "test-rewrite", but the model in the JSON body is updated to the *rewritten target* model.
				fmt.Sprintf(`{"max_tokens":100,"model":%q,"prompt":"test-rewrite","temperature":0}`, modelAfterRewrite),
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "hi",
						RawValue: []byte("mom"),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      requtil.RequestIdHeaderKey,
						RawValue: []byte("test-request-id"),
					},
				},
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, cleanup := setUpHermeticServer(t, test.pods)
			t.Cleanup(cleanup)
			responses, err := integrationutils.StreamedRequest(t, client, test.requests, len(test.wantResponses))

			if err != nil && !test.wantErr {
				t.Errorf("In test %s, unexpected error, got: %v, want error: %v", test.name, err, test.wantErr)
			}
			if diff := cmp.Diff(test.wantResponses, responses,
				protocmp.Transform(),
				protocmp.SortRepeated(func(a, b *configPb.HeaderValueOption) bool {
					return a.GetHeader().GetKey() < b.GetHeader().GetKey()
				}),
			); diff != "" {
				t.Errorf("In test %s, unexpected response, (-want +got): %v", test.name, diff)
			}

			if len(test.wantMetrics) != 0 {
				for metricName, value := range test.wantMetrics {
					if err := metricsutils.GatherAndCompare(crmetrics.Registry, strings.NewReader(value), metricName); err != nil {
						t.Error(fmt.Errorf("In test %s, %v", test.name, err))
					}
				}
			}
			metrics.Reset()
		})
	}
}

// setUpHermeticServer creates a fully isolated test environment for a single test case.
func setUpHermeticServer(t *testing.T, podAndMetrics map[*backend.Pod]*backendmetrics.MetricsState) (client extProcPb.ExternalProcessor_ProcessClient, cleanup func()) {
	// 1. Generate Identity for Isolation
	testID := uuid.New().String()
	testNamespace := fmt.Sprintf("test-ns-%s", testID[:8])

	// 2. Create the Namespace
	ns := &corev1.Namespace{}
	ns.Name = testNamespace
	require.NoError(t, k8sClient.Create(context.Background(), ns), "failed to create test namespace")

	// 3. Define Identity
	gknn := common.GKNN{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testPoolName},
		GroupKind:      schema.GroupKind{Group: v1.GroupVersion.Group, Kind: "InferencePool"},
	}

	// 4. Find Free Ports (Force IPv4 to avoid ::1 vs 127.0.0.1 races)
	grpcPort, err := getFreePort()
	require.NoError(t, err)
	testGRPCAddress := fmt.Sprintf("127.0.0.1:%d", grpcPort)

	metricsPort, err := getFreePort()
	require.NoError(t, err)

	// 5. Configure Manager
	metricsServerOptions := metricsserver.Options{
		BindAddress:    fmt.Sprintf("127.0.0.1:%d", metricsPort),
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}

	mgr, err := server.NewDefaultManager(
		false, // disableK8sCrdReconcile
		gknn,
		testEnv.Config,
		metricsServerOptions,
		false, // leaderElectionEnabled
		func(o *ctrl.Options) {
			t := true
			o.Controller.SkipNameValidation = &t
		},
	)
	require.NoError(t, err, "failed to create manager")

	// 6. Configure Runner
	runner := &server.ExtProcServerRunner{
		GKNN:                             gknn,
		GrpcPort:                         grpcPort,
		SecureServing:                    false,
		HealthChecking:                   false,
		DisableK8sCrdReconcile:           false,
		TestPodMetricsClient:             &backendmetrics.FakePodMetricsClient{},
		RefreshPrometheusMetricsInterval: 50 * time.Millisecond,
		MetricsStalenessThreshold:        2 * time.Second,
	}

	// 7. Wire Dependencies
	res := map[types.NamespacedName]*backendmetrics.MetricsState{}
	for pod, metrics := range podAndMetrics {
		// Fix: Use the generated testNamespace, not the global placeholder
		namespacedName := types.NamespacedName{Name: pod.PodName, Namespace: testNamespace}
		res[namespacedName] = metrics
	}
	runner.TestPodMetricsClient.SetRes(res)

	pmf := backendmetrics.NewPodMetricsFactory(runner.TestPodMetricsClient, 10*time.Millisecond)
	runner.Datastore = datastore.NewDatastore(context.Background(), pmf, 0)

	// Scheduler Setup
	kvCacheUtilizationScorer := scorer.NewKVCacheUtilizationScorer()
	queueingScorer := scorer.NewQueueScorer()
	prefixCacheScorer := prefix.New(context.Background(), prefix.DefaultConfig)
	loraAffinityScorer := scorer.NewLoraAffinityScorer()

	defaultProfile := framework.NewSchedulerProfile().
		WithScorers(framework.NewWeightedScorer(kvCacheUtilizationScorer, 1),
			framework.NewWeightedScorer(queueingScorer, 1),
			framework.NewWeightedScorer(prefixCacheScorer, 1),
			framework.NewWeightedScorer(loraAffinityScorer, 1),
		).
		WithPicker(picker.NewMaxScorePicker(picker.DefaultMaxNumOfEndpoints))

	profileHandler := profile.NewSingleProfileHandler()
	schedulerConfig := scheduling.NewSchedulerConfig(profileHandler, map[string]*framework.SchedulerProfile{"default": defaultProfile})
	scheduler := scheduling.NewSchedulerWithConfig(schedulerConfig)

	satCtrlCfg, _ := staticthreshold.NewConfig(nil)
	satCtrl := staticthreshold.NewController("saturation-controller", satCtrlCfg)
	podLocator := requestcontrol.NewDatastorePodLocator(runner.Datastore)
	cachedPodLocator := requestcontrol.NewCachedPodLocator(context.Background(), podLocator, time.Minute)
	admissionController := requestcontrol.NewLegacyAdmissionController(satCtrl, cachedPodLocator)

	runner.Director = requestcontrol.NewDirectorWithConfig(
		runner.Datastore,
		scheduler, admissionController,
		cachedPodLocator,
		requestcontrol.NewConfig(),
	)

	// 8. Start Manager
	require.NoError(t, runner.SetupWithManager(context.Background(), mgr))

	mgrCtx, cancelMgr := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			// Expected error on cancellation
		}
	}()

	// 9. Create Resources
	podLabels := map[string]string{"app": testPoolName}
	for pod := range podAndMetrics {
		p := epptestutil.MakePod(pod.PodName).
			Namespace(testNamespace).
			ReadyCondition().
			Labels(podLabels).
			IP(pod.GetIPAddress()).
			Complete().
			ObjRef()

		// Snapshot status before creation (API server wipes it)
		desiredStatus := p.Status
		require.NoError(t, k8sClient.Create(context.Background(), p))

		p.Status = desiredStatus
		require.NoError(t, k8sClient.Status().Update(context.Background(), p))
	}

	manifestsPath := filepath.Join("..", "..", "testdata", "inferencepool-with-model-hermetic.yaml")
	docs, err := readDocuments(manifestsPath)
	require.NoError(t, err)

	for _, doc := range docs {
		obj := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(doc, obj))
		obj.SetNamespace(testNamespace)
		require.NoError(t, k8sClient.Create(context.Background(), obj))
	}

	// 10. Wait for Sync (Datastore)
	assert.EventuallyWithT(t, func(t *assert.CollectT) {
		assert.True(t, runner.Datastore.PoolHasSynced(), "Pool not synced")
		assert.NotNil(t, runner.Datastore.ObjectiveGet(modelMyModel), "Objective not found")
		assert.Len(t, runner.Datastore.PodList(backendmetrics.AllPodsPredicate), len(podAndMetrics), "Pod count mismatch")
	}, 10*time.Second, 100*time.Millisecond)

	// 11. Wait for Port (Network Liveness)
	// CRITICAL FIX: Ensure the port is actually open before dialing gRPC.
	assert.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", testGRPCAddress, 100*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 5*time.Second, 100*time.Millisecond, "gRPC server port %s not reachable", testGRPCAddress)

	// 12. Connect Client
	// Use NewClient (DialContext is deprecated) and insecure creds for loopback test
	conn, err := grpc.NewClient(testGRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	ctx, cancelClient := context.WithTimeout(context.Background(), 10*time.Second)
	client, err = extProcPb.NewExternalProcessorClient(conn).Process(ctx)
	require.NoError(t, err)

	return client, func() {
		cancelClient()
		conn.Close()
		cancelMgr()
		// cleanup namespace resources
		_ = k8sClient.Delete(context.Background(), ns)
	}
}

// fakePod uses a placeholder namespace. The actual namespace is overwritten in setUpHermeticServer.
func fakePod(index int) *backend.Pod {
	return &backend.Pod{
		NamespacedName: types.NamespacedName{Name: fmt.Sprintf("pod-%v-rank-0", index), Namespace: "placeholder"},
		Address:        fmt.Sprintf("192.168.1.%d", index+1),
		PodName:        fmt.Sprintf("pod-%v", index),
		Labels:         make(map[string]string, 0),
	}
}

// podState is a descriptor for a pod's simulated metrics.
type podState struct {
	index        int
	queueSize    int
	kvCacheUsage float64
	activeModels []string
}

// newPodStates generates the backend metrics map required by the test setup.
func newPodStates(states ...podState) map[*backend.Pod]*backendmetrics.MetricsState {
	res := make(map[*backend.Pod]*backendmetrics.MetricsState)
	for _, s := range states {
		pod := fakePod(s.index)
		activeModelsMap := make(map[string]int)
		for _, model := range s.activeModels {
			activeModelsMap[model] = 1
		}
		res[pod] = &backendmetrics.MetricsState{
			WaitingQueueSize:    s.queueSize,
			KVCacheUsagePercent: s.kvCacheUsage,
			ActiveModels:        activeModelsMap,
			WaitingModels:       make(map[string]int),
		}
	}
	return res
}

// Sets up a global test environment.
func BeforeSuite() func() {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		logutil.Fatal(logger, err, "Failed to start test environment", "config", cfg)
	}

	utilruntime.Must(clientgoscheme.AddToScheme(server.Scheme))
	utilruntime.Must(v1alpha2.Install(server.Scheme))
	utilruntime.Must(v1.Install(server.Scheme))

	k8sClient, err = k8sclient.New(cfg, k8sclient.Options{Scheme: server.Scheme})
	if err != nil {
		logutil.Fatal(logger, err, "Failed to start k8s Client")
	} else if k8sClient == nil {
		logutil.Fatal(logger, nil, "No error, but returned kubernetes client is nil", "config", cfg)
	}

	ctrl.SetLogger(logger)
	metrics.Register() // Register once globally.

	return func() {
		_ = testEnv.Stop()
	}
}

// readDocuments reads documents from file.
func readDocuments(fp string) ([][]byte, error) {
	b, err := os.ReadFile(fp)
	if err != nil {
		return nil, err
	}

	docs := [][]byte{}
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(b)))
	for {
		// Read document
		doc, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// getFreePort binds to a random port on 127.0.0.1 to ensure we get an IPv4-compatible port.
func getFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
