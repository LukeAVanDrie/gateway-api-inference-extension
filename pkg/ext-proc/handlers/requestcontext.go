package handlers

import (
	"context"
	"time"

	"inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/backend"
	"inference.networking.x-k8s.io/gateway-api-inference-extension/pkg/ext-proc/scheduling"
)

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

// queuedRequestContext represents a request queued in the QueueController.  It
// contains the minimal subset of RequestContext information needed for queue
// management and implements the scheduling.QueuedRequestContext interface.
type queuedRequestContext struct {
	ctx    context.Context    // Parent context for cancellation.
	cancel context.CancelFunc // CancelFunc for parent context.
	signal chan struct{}      // Channel for queue signaling.
	scheduling.Request
}

// newQueuedRequestContext creates a new queuedRequestContext from a
// RequestContext.
func newQueuedRequestContext(reqCtx *RequestContext) *queuedRequestContext {
	return &queuedRequestContext{
		Request: reqCtx.Request,
		ctx:     reqCtx.ctx,
		// Buffered channel to prevent blocking on Signal().
		signal: make(chan struct{}, 1),
	}
}

// Wait blocks request execution until the request is dequeued (via Signal())
// or the context expires.
func (qrc *queuedRequestContext) Wait() {
	select {
	case <-qrc.ctx.Done():
		return
	case <-qrc.signal:
		return
	}
}

// Signal resumes the waiting request (if any).  Sends a non-blocking signal
// on the channel.
func (qrc *queuedRequestContext) Signal() {
	select {
	case qrc.signal <- struct{}{}: // Non-blocking send.
		return
	case <-qrc.ctx.Done():
		<-qrc.signal // Avoid goroutine leak.
	}
}

// Cancel manually cancels the request. This is necessary for queue eviction.
func (qrc *queuedRequestContext) Cancel() {
	qrc.cancel()
}

// Model gets the request's model.
func (qrc *queuedRequestContext) Model() string {
	return qrc.Request.Model
}

// IsCritical indicates whether the request is critical.
func (qrc *queuedRequestContext) IsCritical() bool {
	return qrc.Request.IsCritical
}
