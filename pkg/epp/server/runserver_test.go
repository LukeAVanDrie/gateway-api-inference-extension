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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func TestExtProcServerRunner_AsRunnable_LeaderElection(t *testing.T) {
	t.Parallel()

	// We instantiate the runner directly.
	// Because we are only inspecting the Runnable wrapper and not calling Start(), we do not need to populate
	// dependencies like Datastore or Director.
	runner := &ExtProcServerRunner{GrpcPort: 9002}

	r := runner.AsRunnable(logr.Discard())

	// The ExtProc server (gRPC) is the data plane component.
	// It must run on ALL pods, regardless of which one is the "Leader" for K8s Controller reconciliation.
	leRunnable, ok := r.(manager.LeaderElectionRunnable)
	require.True(t, ok, "Runnable must implement LeaderElectionRunnable interface")
	assert.False(t, leRunnable.NeedLeaderElection(),
		"ExtProc server must have leader election DISABLED to serve traffic on all replicas")
}
