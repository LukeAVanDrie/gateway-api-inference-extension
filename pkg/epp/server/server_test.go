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

package server

import (
	"context"
	"fmt"
	"net"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/handlers"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metadata"
	testutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"
	"sigs.k8s.io/gateway-api-inference-extension/test/utils"
)

const (
	podName    = "pod1"
	podAddress = "1.2.3.4"
	poolPort   = int32(5678)
	namespace  = "ns1"
	bufSize    = 1024 * 1024
)

// TestExtProcServer_E2E validates the full gRPC streaming flow:
// Request Headers -> Request Body -> Response Headers -> Response Body
func TestExtProcServer_E2E(t *testing.T) {
	t.Parallel()

	model := testutil.MakeInferenceObjective("v1").CreationTimestamp(metav1.Unix(1000, 0)).ObjRef()
	pods := []*v1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: podName}}}

	// We use test utils to setup the datastore, but avoid its server setup logic because we want full control over the
	// listener via bufconn.
	_, _, ds, _ := utils.PrepareForTestStreamingServer(
		[]*v1alpha2.InferenceObjective{model},
		pods,
		"test-pool1",
		namespace,
		poolPort,
	)

	director := &testDirector{}
	serverHandler := handlers.NewStreamingServer(ds, director)

	// Start Server and Client.
	client, closer := startBufferedServer(t, serverHandler)
	defer closer()

	stream, err := client.Process(context.Background())
	require.NoError(t, err, "failed to open stream")

	// --- Send Request Headers ---
	reqHeaders := utils.BuildEnvoyGRPCHeaders(map[string]string{
		"x-test":                   "body",
		":method":                  "POST",
		metadata.FlowFairnessIDKey: "fairness-id-123",
		"x-request-id":             "req-id-123",
	}, true)

	err = stream.Send(&pb.ProcessingRequest{
		Request: &pb.ProcessingRequest_RequestHeaders{RequestHeaders: reqHeaders},
	})
	require.NoError(t, err, "failed to send request headers")

	// --- Send Request Body (triggers header rewriting) ---
	requestBody := `{"model":"food-review","prompt":"Is banana tasty?"}`
	err = stream.Send(&pb.ProcessingRequest{
		Request: &pb.ProcessingRequest_RequestBody{
			RequestBody: &pb.HttpBody{Body: []byte(requestBody), EndOfStream: true},
		},
	})
	require.NoError(t, err, "failed to send request body")

	// --- Verify Request Header Response ---
	// The server responds to headers first.
	resp1, err := stream.Recv()
	require.NoError(t, err, "failed to receive header response")

	respHeaders := resp1.GetRequestHeaders().Response.HeaderMutation.SetHeaders
	assertHeader(t, respHeaders, metadata.DestinationEndpointKey, fmt.Sprintf("%s:%d", podAddress, poolPort))
	assertHeader(t, respHeaders, "Content-Length", "42")

	// --- Verify Request Body Response (Rewrite) ---
	resp2, err := stream.Recv()
	require.NoError(t, err, "failed to receive body response")

	rewrittenBody := resp2.GetRequestBody().Response.BodyMutation.GetStreamedResponse().Body
	expectedBody := `{"model":"v1","prompt":"Is banana tasty?"}`
	assert.JSONEq(t, expectedBody, string(rewrittenBody), "request body should be rewritten by director")

	// --- Verify Scheduler Context ---
	// Check what the Director actually saw
	assert.Equal(t, "body", director.requestHeaders["x-test"])
	assert.Equal(t, "req-id-123", director.requestHeaders["x-request-id"])

	// --- Send Response Headers ---
	// Simulate backend response
	backendHeaders := utils.BuildEnvoyGRPCHeaders(map[string]string{
		"x-test":  "body",
		":method": "POST",
	}, false)

	err = stream.Send(&pb.ProcessingRequest{
		Request: &pb.ProcessingRequest_ResponseHeaders{ResponseHeaders: backendHeaders},
	})
	require.NoError(t, err, "failed to send response headers")

	// --- Verify Response Headers Mutation ---
	resp3, err := stream.Recv()
	require.NoError(t, err, "failed to receive response header mutation")

	mutation := resp3.GetResponseHeaders().Response.HeaderMutation.SetHeaders
	assertHeader(t, mutation, "x-test", "body")
	assertHeader(t, mutation, "x-went-into-resp-headers", "true")
}

// --- Helpers ---

// startBufferedServer creates an in-memory gRPC server for testing.
// It returns the client and a cleanup function.
func startBufferedServer(t *testing.T, handler pb.ExternalProcessorServer) (pb.ExternalProcessorClient, func()) {
	listener := bufconn.Listen(bufSize)
	s := grpc.NewServer()
	pb.RegisterExternalProcessorServer(s, handler)

	go func() {
		_ = s.Serve(listener)
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet", // Bypass DNS resolution and go straight to the custom dialer.
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)

	client := pb.NewExternalProcessorClient(conn)

	return client, func() {
		conn.Close()
		s.Stop()
		listener.Close()
	}
}

// assertHeader checks for the existence and value of a header in the Envoy Core response type.
// It handles both Value (string) and RawValue (bytes) fields.
func assertHeader(t *testing.T, headers []*corev3.HeaderValueOption, key, expected string) {
	t.Helper()
	found := false
	for _, h := range headers {
		if h.Header.Key == key {
			val := h.Header.Value
			// Fallback to RawValue if Value is empty.
			if val == "" && len(h.Header.RawValue) > 0 {
				val = string(h.Header.RawValue)
			}
			assert.Equal(t, expected, val, "header %s mismatch", key)
			found = true
			break
		}
	}
	assert.True(t, found, "header %s not found in response", key)
}

// --- Mocks ---

type testDirector struct {
	requestHeaders map[string]string
}

func (ts *testDirector) HandleRequest(
	_ context.Context,
	reqCtx *handlers.RequestContext,
) (*handlers.RequestContext, error) {
	ts.requestHeaders = reqCtx.Request.Headers
	// Simulate model rewriting logic.
	reqCtx.Request.Body["model"] = "v1"
	reqCtx.TargetEndpoint = fmt.Sprintf("%s:%d", podAddress, poolPort)
	return reqCtx, nil
}

func (ts *testDirector) HandleResponseReceived(
	_ context.Context,
	reqCtx *handlers.RequestContext,
) (*handlers.RequestContext, error) {
	return reqCtx, nil
}

func (ts *testDirector) HandleResponseBodyStreaming(
	_ context.Context,
	reqCtx *handlers.RequestContext,
) (*handlers.RequestContext, error) {
	return reqCtx, nil
}

func (ts *testDirector) HandleResponseBodyComplete(
	_ context.Context,
	reqCtx *handlers.RequestContext,
) (*handlers.RequestContext, error) {
	return reqCtx, nil
}

func (ts *testDirector) GetRandomPod() *backend.Pod {
	return nil
}
