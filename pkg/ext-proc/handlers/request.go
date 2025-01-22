package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/backend"
	"inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/scheduling"
	logutil "inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/util/logging"
	klog "k8s.io/klog/v2"
)

// HandleRequestBody handles body of the request to the backend server, such as parsing the "model"
// parameter.
// Envoy sends the request body to ext proc before sending the request to the backend server.
func (s *Server) HandleRequestBody(reqCtx *RequestContext, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, error) {
	klog.V(logutil.VERBOSE).Infof("Handling request body")

	// Unmarshal request body (must be JSON).
	v := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
	var rb map[string]interface{}
	if err := json.Unmarshal(v.RequestBody.Body, &rb); err != nil {
		klog.Errorf("Error unmarshaling request body: %v", err)
		return nil, fmt.Errorf("error unmarshaling request body: %v", err)
	}
	klog.V(logutil.VERBOSE).Infof("Request body: %v", rb)

	// Resolve target models.
	model, ok := rb["model"].(string)
	if !ok {
		return nil, errors.New("model not found in request")
	}
	klog.V(logutil.VERBOSE).Infof("Model requested: %v", model)
	modelName := model

	// NOTE: The nil checking for the modelObject means that we DO allow passthrough currently.
	// This might be a security risk in the future where adapters not registered in the InferenceModel
	// are able to be requested by using their distinct name.
	modelObj := s.datastore.FetchModelData(model)
	if modelObj == nil {
		return nil, fmt.Errorf("error finding a model object in InferenceModel for input %v", model)
	}
	if len(modelObj.Spec.TargetModels) > 0 {
		modelName = backend.RandomWeightedDraw(modelObj, 0)
		if modelName == "" {
			return nil, fmt.Errorf("error getting target model name for model %v", modelObj.Name)
		}
	}
	llmReq := &scheduling.Request{
		IsCritical:          backend.IsCritical(modelObj),
		Model:               model,
		ResolvedTargetModel: modelName,
	}
	klog.V(logutil.VERBOSE).Infof("LLM Request: %+v", llmReq)
	requestBody := v.RequestBody.Body

	// Update target models in the body.
	if llmReq.Model != llmReq.ResolvedTargetModel {
		var err error
		rb["model"] = llmReq.ResolvedTargetModel
		requestBody, err = json.Marshal(rb)
		if err != nil {
			klog.Errorf("Error marshaling request body: %v", err)
			return nil, fmt.Errorf("error marshaling request body: %v", err)
		}
		klog.V(logutil.VERBOSE).Infof("Updated body: %v", string(requestBody))
	}

	reqCtx.Request = *llmReq
	reqCtx.RequestSize = len(requestBody)

	if s.queueController != nil {
		sizer := scheduling.WordCountSizer{}
		if prompt, ok := rb["prompt"].(string); ok {
				reqCtx.QueueSize = sizer.Size(prompt)
		} else {
				return nil, status.Errorf(codes.InvalidArgument, "prompt field not found or not a string in request body")
		}

		// Blocks if successfully enqueued until dequeued or evicted.  A request
		// may bypass the queue in which case, this line does not block.
		// Returns an error if request fails to enqueue or is evicted before
		// successful dequeue.
		if err := s.queueController.TryEnqueue(reqCtx); err != nil {
			klog.V(logutil.VERBOSE).InfoS("Error during TryEnqueue", "model", reqCtx.Request.Model, "error", err)
			if errors.Is(err, scheduling.ErrTooManyFlows) || errors.Is(err, scheduling.ErrInsufficientFlowCapacity) {
				return nil, status.Error(codes.Unavailable, "queue service unavailable")
			}
			if errors.Is(err, scheduling.ErrExpiredTTL) {
				return nil, status.Error(codes.Unavailable, "request timed out in queue: ttl")
			}
			if stat := status.FromContextError(err); stat != nil {
				return nil, stat.Err()
			}
			return nil, err // May or may not be a status error.
		}
	}

	targetPod, err := s.scheduler.Schedule(llmReq)
	if err != nil {
		return nil, fmt.Errorf("failed to find target pod: %w", err)
	}
	klog.V(logutil.VERBOSE).Infof("Selected target model %v in target pod: %v\n", llmReq.ResolvedTargetModel, targetPod)

	// Insert "target-pod" to instruct Envoy to route requests to the specified target pod.
	headers := []*configPb.HeaderValueOption{
		{
			Header: &configPb.HeaderValue{
				Key:      s.targetPodHeader,
				RawValue: []byte(targetPod.Address),
			},
		},
		// We need to update the content length header if the body is mutated, see Envoy doc:
		// https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ext_proc/v3/processing_mode.proto
		{
			Header: &configPb.HeaderValue{
				Key:      "Content-Length",
				RawValue: []byte(strconv.Itoa(len(requestBody))),
			},
		},
	}
	// Print headers for debugging
	for _, header := range headers {
		klog.V(logutil.VERBOSE).Infof("[request_body] Header Key: %s, Header Value: %s\n", header.Header.Key, header.Header.RawValue)
	}

	resp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					BodyMutation: &extProcPb.BodyMutation{
						Mutation: &extProcPb.BodyMutation_Body{
							Body: requestBody,
						},
					},
				},
			},
		},
	}
	return resp, nil
}

func HandleRequestHeaders(reqCtx *RequestContext, req *extProcPb.ProcessingRequest) *extProcPb.ProcessingResponse {
	klog.V(logutil.VERBOSE).Info("Handling request headers ...")
	r := req.Request
	h := r.(*extProcPb.ProcessingRequest_RequestHeaders)
	klog.V(logutil.VERBOSE).Infof("Headers: %+v\n", h)

	resp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					// Set `clear_route_cache = true` to force Envoy to recompute the target cluster
					// based on the new "target-pod" header.
					// See https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto#service-ext-proc-v3-commonresponse.
					ClearRouteCache: true,
				},
			},
		},
	}

	return resp
}
