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
	"flag"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewOptions_Defaults(t *testing.T) {
	t.Parallel()

	o := NewOptions()

	// Verify critical defaults match expectations.
	// This ensures we don't accidentally drift from agreed-upon standard ports/timeouts.
	assert.Equal(t, 9002, o.GrpcPort, "default gRPC port should match Envoy standard")
	assert.Equal(t, 50*time.Millisecond, o.RefreshMetricsInterval, "default scrape interval should be high-frequency for LLMs")
	assert.Equal(t, "", o.PoolNamespace, "default namespace should be empty to trigger env var detection")
	assert.True(t, o.SecureServing, "secure serving should be enabled by default for security")
}

func TestOptions_Validate(t *testing.T) {
	t.Parallel()

	type modifier func(*Options)

	const poolName = "llama-pool"

	tests := []struct {
		name      string
		setup     modifier
		expectErr string // substring match
	}{
		{
			name: "Valid: Pool Mode",
			setup: func(o *Options) {
				o.PoolName = poolName
			},
			expectErr: "",
		},
		{
			name: "Valid: Selector Mode",
			setup: func(o *Options) {
				o.EndpointSelector = "app=vllm"
			},
			expectErr: "",
		},
		{
			name: "Invalid: Identity Ambiguity (Both)",
			setup: func(o *Options) {
				o.PoolName = poolName
				o.EndpointSelector = "app=vllm"
			},
			expectErr: "exactly one of --pool-name or --endpoint-selector",
		},
		{
			name: "Invalid: Identity Ambiguity (Neither)",
			setup: func(o *Options) {
				o.PoolName = ""
				o.EndpointSelector = ""
			},
			expectErr: "exactly one of --pool-name or --endpoint-selector",
		},
		{
			name: "Invalid: Config Source Conflict",
			setup: func(o *Options) {
				o.PoolName = poolName
				o.ConfigFile = "/etc/config.yaml"
				o.ConfigText = "data: 1"
			},
			expectErr: "--config-file and --config-text cannot be used simultaneously",
		},
		{
			name: "Invalid: Metrics Scheme",
			setup: func(o *Options) {
				o.PoolName = poolName
				o.LegacyMetrics.Scheme = "ftp"
			},
			expectErr: "invalid metrics scheme",
		},
		{
			name: "Invalid: Metrics Port Range",
			setup: func(o *Options) {
				o.PoolName = poolName
				o.MetricsPort = 70000
			},
			expectErr: "invalid metrics port",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o := NewOptions()
			tc.setup(o)

			err := o.Validate()
			if tc.expectErr == "" {
				assert.NoError(t, err, "expected validation to pass")
			} else {
				require.Error(t, err, "expected validation to fail")
				assert.Contains(t, err.Error(), tc.expectErr, "error message should contain expected context")
			}
		})
	}
}

func TestOptions_AddFlags(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	o := NewOptions()
	o.AddFlags(fs)

	// Simulate CLI arguments.
	args := []string{
		"--pool-name=cli-override",
		"--metrics-port=8080",
		"--enable-pprof=false",
		"--v=4",
	}

	err := fs.Parse(args)
	require.NoError(t, err, "failed to parse valid flags")

	assert.Equal(t, "cli-override", o.PoolName, "flag should override default pool name")
	assert.Equal(t, 8080, o.MetricsPort, "flag should override default metrics port")
	assert.False(t, o.EnablePprof, "bool flag should be parsed correctly")
	assert.Equal(t, 4, o.LogVerbosity, "int flag should be parsed correctly")
}
