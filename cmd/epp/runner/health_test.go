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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
)

// mockDatastore implements the subset of Datastore interface needed for health checks.
type mockDatastore struct {
	datastore.Datastore
	synced bool
}

func (m *mockDatastore) PoolHasSynced() bool { return m.synced }

func TestHealthServer_Check(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		service       string
		leaderEnabled bool
		isLeader      bool
		isSynced      bool
		wantStatus    healthPb.HealthCheckResponse_ServingStatus
	}{
		// --- Liveness (Always Alive) ---
		{
			name:       "Liveness: Should be serving even if not synced",
			service:    "liveness",
			isSynced:   false,
			wantStatus: healthPb.HealthCheckResponse_SERVING,
		},
		{
			name:          "Liveness: Should be serving even if standby node",
			service:       "liveness",
			leaderEnabled: true,
			isLeader:      false,
			wantStatus:    healthPb.HealthCheckResponse_SERVING,
		},

		// --- Readiness (HA Disabled) ---
		{
			name:          "Readiness (No HA): Serving when synced",
			service:       "readiness",
			leaderEnabled: false,
			isSynced:      true,
			wantStatus:    healthPb.HealthCheckResponse_SERVING,
		},
		{
			name:          "Readiness (No HA): Not serving when not synced",
			service:       "readiness",
			leaderEnabled: false,
			isSynced:      false,
			wantStatus:    healthPb.HealthCheckResponse_NOT_SERVING,
		},

		// --- Readiness (HA Enabled) ---
		{
			name:          "Readiness (HA): Serving when Leader AND Synced",
			service:       "readiness",
			leaderEnabled: true,
			isLeader:      true,
			isSynced:      true,
			wantStatus:    healthPb.HealthCheckResponse_SERVING,
		},
		{
			name:          "Readiness (HA): Not serving when Standby (even if synced)",
			service:       "readiness",
			leaderEnabled: true,
			isLeader:      false,
			isSynced:      true,
			wantStatus:    healthPb.HealthCheckResponse_NOT_SERVING,
		},
		{
			name:          "Readiness (HA): Not serving when Leader but syncing",
			service:       "readiness",
			leaderEnabled: true,
			isLeader:      true,
			isSynced:      false,
			wantStatus:    healthPb.HealthCheckResponse_NOT_SERVING,
		},

		// --- Default Service (Load Balancer behavior) ---
		{
			name:          "Default Service: Maps to Readiness (Serving)",
			service:       "",
			leaderEnabled: true,
			isLeader:      true,
			isSynced:      true,
			wantStatus:    healthPb.HealthCheckResponse_SERVING,
		},
		{
			name:          "Default Service: Maps to Readiness (Not Serving)",
			service:       "",
			leaderEnabled: true,
			isLeader:      false,
			wantStatus:    healthPb.HealthCheckResponse_NOT_SERVING,
		},

		// --- Edge Cases ---
		{
			name:       "Unknown Service: Should return unknown status",
			service:    "unknown-service",
			wantStatus: healthPb.HealthCheckResponse_SERVICE_UNKNOWN,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			isLeader := &atomic.Bool{}
			isLeader.Store(tc.isLeader)

			s := &healthServer{
				logger:                logr.Discard(),
				datastore:             &mockDatastore{synced: tc.isSynced},
				isLeader:              isLeader,
				leaderElectionEnabled: tc.leaderEnabled,
			}

			resp, err := s.Check(context.Background(), &healthPb.HealthCheckRequest{Service: tc.service})
			require.NoError(t, err, "Check should not return internal error")
			assert.Equal(t, tc.wantStatus, resp.Status, "health status mismatch")
		})
	}
}

func TestHealthServer_List(t *testing.T) {
	t.Parallel()

	isLeader := &atomic.Bool{}
	isLeader.Store(true)

	s := &healthServer{
		logger:                logr.Discard(),
		datastore:             &mockDatastore{synced: true},
		isLeader:              isLeader,
		leaderElectionEnabled: true,
	}

	resp, err := s.List(context.Background(), nil)
	require.NoError(t, err, "List should not fail")
	assert.Contains(t, resp.Statuses, "liveness")
	assert.Contains(t, resp.Statuses, "readiness")
	assert.Contains(t, resp.Statuses, "")
	assert.Equal(t, healthPb.HealthCheckResponse_SERVING, resp.Statuses["liveness"].Status)
}
