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
	"crypto/tls"
	"fmt"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/gateway-api-inference-extension/internal/runnable"
	tlsutil "sigs.k8s.io/gateway-api-inference-extension/internal/tls"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/controller"
	dlmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/handlers"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/requestcontrol"
)

// ExtProcServerRunner provides methods to manage an external process server.
type ExtProcServerRunner struct {
	// --- Identity ---
	GKNN common.GKNN

	// --- Network & Security ---
	GrpcPort       int
	SecureServing  bool
	HealthChecking bool
	CertPath       string

	//  --- Operational Config ---
	DisableK8sCrdReconcile           bool
	RefreshPrometheusMetricsInterval time.Duration
	MetricsStalenessThreshold        time.Duration
	UseExperimentalDatalayerV2       bool

	// --- Dependencies ---
	Datastore datastore.Datastore
	Director  *requestcontrol.Director

	// --- Test Hooks ---
	// TODO: Cleanup once metrics injection is solved properly (Issue #432)
	TestPodMetricsClient *backendmetrics.FakePodMetricsClient
}

// SetupWithManager initializes the necessary controllers and registers them with the manager.
func (r *ExtProcServerRunner) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	// 1. InferencePool Reconciliation (if enabled)
	if !r.DisableK8sCrdReconcile {
		if err := (&controller.InferencePoolReconciler{
			Datastore: r.Datastore,
			Reader:    mgr.GetClient(),
			PoolGKNN:  r.GKNN,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("failed setting up InferencePoolReconciler: %w", err)
		}

		if err := (&controller.InferenceObjectiveReconciler{
			Datastore: r.Datastore,
			Reader:    mgr.GetClient(),
			PoolGKNN:  r.GKNN,
		}).SetupWithManager(ctx, mgr); err != nil {
			return fmt.Errorf("failed setting up InferenceObjectiveReconciler: %w", err)
		}
	}

	// 2. InferenceModelRewrite Reconciliation
	if err := (&controller.InferenceModelRewriteReconciler{
		Datastore: r.Datastore,
		Reader:    mgr.GetClient(),
		PoolGKNN:  r.GKNN,
	}).SetupWithManager(ctx, mgr); err != nil {
		return fmt.Errorf("failed setting up InferenceModelRewriteReconciler: %w", err)
	}

	// 3. Pod Reconciliation
	if err := (&controller.PodReconciler{
		Datastore: r.Datastore,
		Reader:    mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed setting up PodReconciler: %w", err)
	}

	return nil
}

// AsRunnable returns a Runnable that starts the ext-proc gRPC server.
func (r *ExtProcServerRunner) AsRunnable(logger logr.Logger) manager.Runnable {
	return runnable.NoLeaderElection(manager.RunnableFunc(func(ctx context.Context) error {
		r.startMetricsLogging(ctx)

		srv, err := r.createGRPCServer(logger)
		if err != nil {
			return err
		}

		extProcPb.RegisterExternalProcessorServer(srv, handlers.NewStreamingServer(r.Datastore, r.Director))

		if r.HealthChecking {
			r.registerHealthCheck(logger, srv)
		}

		return runnable.GRPCServer("ext-proc", srv, r.GrpcPort).Start(ctx)
	}))
}

func (r *ExtProcServerRunner) startMetricsLogging(ctx context.Context) {
	if r.UseExperimentalDatalayerV2 {
		dlmetrics.StartMetricsLogger(ctx, r.Datastore, r.RefreshPrometheusMetricsInterval, r.MetricsStalenessThreshold)
	} else {
		backendmetrics.StartMetricsLogger(ctx, r.Datastore, r.RefreshPrometheusMetricsInterval, r.MetricsStalenessThreshold)
	}
}

func (r *ExtProcServerRunner) createGRPCServer(logger logr.Logger) (*grpc.Server, error) {
	if !r.SecureServing {
		return grpc.NewServer(), nil
	}

	var cert tls.Certificate
	var err error

	if r.CertPath != "" {
		cert, err = tls.LoadX509KeyPair(r.CertPath+"/tls.crt", r.CertPath+"/tls.key")
	} else {
		logger.Info("CertPath not provided; generating self-signed certificate")
		cert, err = tlsutil.CreateSelfSignedTLSCertificate(logger)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to setup TLS: %w", err)
	}

	return grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}))), nil
}

func (r *ExtProcServerRunner) registerHealthCheck(logger logr.Logger, srv *grpc.Server) {
	healthcheck := health.NewServer()
	healthgrpc.RegisterHealthServer(srv, healthcheck)
	svcName := extProcPb.ExternalProcessor_ServiceDesc.ServiceName
	logger.Info("Enabling gRPC Health Check", "serviceName", svcName)
	healthcheck.SetServingStatus(svcName, healthgrpc.HealthCheckResponse_SERVING)
}
