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

package runner

import (
	"context"
	"sync/atomic"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const (
	// livenessService indicates the check is for basic process existence.
	livenessService = "liveness"
	// readinessService indicates the check is for traffic-serving capability.
	readinessService = "readiness"
)

// healthServer implements the standard gRPC Health Checking Protocol.
// It adapts the health status based on leader election and datastore synchronization.
type healthServer struct {
	logger                logr.Logger
	datastore             datastore.Datastore
	isLeader              *atomic.Bool
	leaderElectionEnabled bool
}

// Check implements the health checking logic.
//
// Policy:
//  1. Liveness ("liveness"): Always SERVING. If this handler is reachable, the process is alive.
//  2. Readiness ("readiness"): SERVING only if the instance is the Leader (if HA enabled) AND the Datastore is synced.
//  3. ExtProc Service: Mirrors Readiness.
//  4. Overall Health (""): Mirrors Readiness. Load Balancers using the default check expect to know if they can send
//     traffic.
func (s *healthServer) Check(ctx context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	var requiredChecksPassed bool

	// Cached state to avoid racing/locking overhead during high-frequency checks.
	isSynced := s.datastore.PoolHasSynced()
	isLeader := s.isLeader.Load()

	// Logic Matrix:
	// - Leader Election Enabled:  Readiness requires (Leader && Synced). Liveness requires (True).
	// - Leader Election Disabled: Readiness requires (Synced).           Liveness requires (True).
	isReady := isSynced
	if s.leaderElectionEnabled {
		isReady = isSynced && isLeader
	}

	switch in.Service {
	case livenessService:
		// Explicit Liveness Probe:
		// We are reachable, therefore we are alive.
		// We do NOT check sync/leader status here. Doing so would cause Kubelet to restart the pod during long initial
		// syncs or kill healthy standby replicas
		requiredChecksPassed = true
	case "", readinessService, extProcPb.ExternalProcessor_ServiceDesc.ServiceName:
		// Load Balancer / Readiness Probe:
		// We can only accept traffic if we are the active leader and have data.
		//
		// The empty string ("") represents "Overall Health".
		// For a traffic-handling component, this implies "Ready to Serve".
		// If we are not ready (syncing or standby), we must return NOT_SERVING so LBs stop routing to us.
		//
		// NOTE: Kubernetes Liveness Probes MUST explicitly use "-service liveness".
		// If they use the default (empty string), they will fall into this block and kill the pod during startup.
		requiredChecksPassed = isReady

	default:
		// Unknown service names should result in a specific gRPC error code, per spec.
		return &healthPb.HealthCheckResponse{
			Status: healthPb.HealthCheckResponse_SERVICE_UNKNOWN,
		}, nil
	}

	if !requiredChecksPassed {
		s.logger.V(logutil.DEFAULT).Info("Health check failing",
			"service", in.Service,
			"isLeader", isLeader,
			"isSynced", isSynced,
			"electionEnabled", s.leaderElectionEnabled)

		return &healthPb.HealthCheckResponse{
			Status: healthPb.HealthCheckResponse_NOT_SERVING,
		}, nil
	}

	s.logger.V(logutil.TRACE).Info("Health check passing", "service", in.Service)

	return &healthPb.HealthCheckResponse{
		Status: healthPb.HealthCheckResponse_SERVING,
	}, nil
}

// List implements the optional Health V1 extension to list all services and their status.
// This is primarily useful for manual debugging via grpcurl.
func (s *healthServer) List(ctx context.Context, _ *healthPb.HealthListRequest) (*healthPb.HealthListResponse, error) {
	// Define the list of services we know about.
	knownServices := []string{
		"", // Overall health
		livenessService,
		readinessService,
		extProcPb.ExternalProcessor_ServiceDesc.ServiceName,
	}

	statuses := make(map[string]*healthPb.HealthCheckResponse)

	for _, service := range knownServices {
		resp, err := s.Check(ctx, &healthPb.HealthCheckRequest{Service: service})
		if err != nil {
			// Check logic doesn't return errors for internal failures, only for unknown services.
			// Since we are iterating known services, this shouldn't happen.
			s.logger.Error(err, "Internal error checking health during List", "service", service)
			continue
		}
		statuses[service] = resp
	}

	return &healthPb.HealthListResponse{
		Statuses: statuses,
	}, nil
}

// Watch is required by the interface but not implemented.
// Kubelet generally uses unary Check, not streaming Watch.
func (s *healthServer) Watch(_ *healthPb.HealthCheckRequest, _ healthPb.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}
