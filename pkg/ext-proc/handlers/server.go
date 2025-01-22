package handlers

import (
	"context"
	"io"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"inference.networking.x-k8s.io/gateway-api-inference-extension/api/v1alpha1"
	"inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/backend"
	"inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/metrics"
	"inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/scheduling"
	logutil "inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/util/logging"
	klog "k8s.io/klog/v2"
)

// ServerOption configures a Server instance during initialization.
type ServerOption func(s *Server)

func NewServer(pp PodProvider, scheduler Scheduler, targetPodHeader string, datastore ModelDataStore, opts ...ServerOption) *Server {
	s := &Server{
		scheduler:       scheduler,
		podProvider:     pp,
		targetPodHeader: targetPodHeader,
		datastore:       datastore,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithQueueController sets the QueueController on the Server.
func WithQueueController(qc QueueController) ServerOption {
	return func(s *Server) {
		s.queueController = qc
	}
}

// Server implements the Envoy external processing server.
// https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto
type Server struct {
	scheduler       Scheduler
	podProvider     PodProvider
	queueController QueueController
	// The key of the header to specify the target pod address. This value needs to match Envoy
	// configuration.
	targetPodHeader string
	datastore       ModelDataStore
}

type QueueController interface {
	TryEnqueue(req scheduling.RequestContext) error
	OnRequestScheduled()
	OnRequestComplete()
}

type Scheduler interface {
	Schedule(r *scheduling.Request) (targetPod backend.Pod, err error)
}

// PodProvider is an interface to provide set of pods in the backend and information such as metrics.
type PodProvider interface {
	GetPodMetrics(pod backend.Pod) (*backend.PodMetrics, bool)
	UpdatePodMetrics(pod backend.Pod, pm *backend.PodMetrics)
}

type ModelDataStore interface {
	FetchModelData(modelName string) (returnModel *v1alpha1.InferenceModel)
}

func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	klog.V(logutil.VERBOSE).Info("Processing")
	ctx, cancel := context.WithCancel(srv.Context())
	// Create request context to share states during life time of an HTTP request.
	// See https://github.com/envoyproxy/envoy/issues/17540.
	reqCtx := newRequestContext(ctx, cancel)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := srv.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// This error occurs very frequently, though it doesn't seem to have any impact.
			// TODO Figure out if we can remove this noise.
			klog.V(logutil.VERBOSE).Infof("cannot receive stream request: %v", err)
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		var resp *extProcPb.ProcessingResponse
		switch v := req.Request.(type) {
		case *extProcPb.ProcessingRequest_RequestHeaders:
			reqCtx.RequestReceivedTimestamp = time.Now()
			resp = HandleRequestHeaders(reqCtx, req)
			klog.V(logutil.VERBOSE).Infof("Request context after HandleRequestHeaders: %+v", reqCtx)
		case *extProcPb.ProcessingRequest_RequestBody:
			resp, err = s.HandleRequestBody(reqCtx, req)
			if err == nil {
				metrics.RecordRequestCounter(reqCtx.Request.Model, reqCtx.ResolvedTargetModel)
				metrics.RecordRequestSizes(reqCtx.Request.Model, reqCtx.ResolvedTargetModel, reqCtx.RequestSize)
			}
			klog.V(logutil.VERBOSE).Infof("Request context after HandleRequestBody: %+v", reqCtx)
			if s.queueController != nil {
				s.queueController.OnRequestScheduled()
				defer s.queueController.OnRequestComplete()
			}
		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp, err = s.HandleResponseHeaders(reqCtx, req)
			klog.V(logutil.VERBOSE).Infof("Request context after HandleResponseHeaders: %+v", reqCtx)
		case *extProcPb.ProcessingRequest_ResponseBody:
			resp, err = s.HandleResponseBody(reqCtx, req)
			if err == nil && reqCtx.ResponseComplete {
				reqCtx.ResponseCompleteTimestamp = time.Now()
				metrics.RecordRequestLatencies(reqCtx.Request.Model, reqCtx.ResolvedTargetModel, reqCtx.RequestReceivedTimestamp, reqCtx.ResponseCompleteTimestamp)
				metrics.RecordResponseSizes(reqCtx.Request.Model, reqCtx.ResolvedTargetModel, reqCtx.ResponseSize)
				metrics.RecordInputTokens(reqCtx.Request.Model, reqCtx.ResolvedTargetModel, reqCtx.Response.Usage.PromptTokens)
				metrics.RecordOutputTokens(reqCtx.Request.Model, reqCtx.ResolvedTargetModel, reqCtx.Response.Usage.CompletionTokens)
			}
			klog.V(logutil.VERBOSE).Infof("Request context after HandleResponseBody: %+v", reqCtx)
		default:
			klog.Errorf("Unknown Request type %+v", v)
			return status.Error(codes.Unknown, "unknown request type")
		}
		if err != nil {
			klog.Errorf("failed to process request: %v", err)
			switch status.Code(err) {
			// This code can be returned by scheduler when there is no capacity for sheddable
			// requests.
			case codes.ResourceExhausted:
				resp = &extProcPb.ProcessingResponse{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &envoyTypePb.HttpStatus{
								Code: envoyTypePb.StatusCode_TooManyRequests,
							},
						},
					},
				}
			default:
				return status.Errorf(status.Code(err), "failed to handle request: %v", err)
			}
		}

		klog.V(logutil.VERBOSE).Infof("response: %v", resp)
		if err := srv.Send(resp); err != nil {
			klog.Errorf("send error %v", err)
			return status.Errorf(codes.Unknown, "failed to send response back to Envoy: %v", err)
		}
	}
}

// RequestContext stores context information during the lifetime of an HTTP
// request.
type RequestContext struct {
	ctx                       context.Context
	cancel                    context.CancelFunc
	ResponseComplete          bool
	RequestReceivedTimestamp  time.Time
	ResponseCompleteTimestamp time.Time
	RequestSize               int
	ResponseSize              int
	// QueueSize stores the request size used by the queuing system.
	QueueSize uint64
	TargetPod                 backend.Pod
	Response                  Response
	scheduling.Request        // Request being processed (model info).
}

// newRequestContext creates a new RequestContext.
func newRequestContext(ctx context.Context, cancel context.CancelFunc) *RequestContext {
	return &RequestContext{
		ctx:    ctx,
		cancel: cancel,
	}
}

// Method below are needed to implement the scheduling.RequestProperties and
// scheduling.RequestContext interfaces.

// Model returns the request's model.
func (r *RequestContext) Model() string {
	return r.Request.Model
}

// IsCritical indicates whether the request is critical.
func (r *RequestContext) IsCritical() bool {
	return r.Request.IsCritical
}

// Size returns the size of the request, used by the queuing system.
func (r *RequestContext) Size() uint64 {
	return r.QueueSize
}

// Context returns the request's context.
func (r *RequestContext) Context() context.Context {
	return r.ctx
}

// CancelFunc returns the request's cancellation function.
func (r *RequestContext) CancelFunc() context.CancelFunc {
	return r.cancel
}
