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
	"errors"
	"flag"
	"fmt"
	"time"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const (
	// --- Identity Defaults ---

	// defaultPoolGroup is the Kubernetes API Group for Inference Extension resources.
	defaultPoolGroup = "inference.networking.k8s.io"
	// defaultPoolName is empty to mandate explicit identity configuration via flags (either PoolName or
	// EndpointSelector).
	defaultPoolName = ""

	// --- Network Defaults ---

	// DefaultGrpcPort (9002) is the standard convention for Envoy External Processing.
	DefaultGrpcPort = 9002
	// defaultGrpcHealthPort (9003) is the port for gRPC liveness/readiness probes.
	defaultGrpcHealthPort = 9003
	// defaultMetricsPort (9090) is the standard convention for Prometheus exporters.
	defaultMetricsPort = 9090

	// --- Observability Defaults ---

	// defaultRefreshMetricsInterval (50ms) defines the frequency of backend metric scrapes.
	// High frequency is required to track rapid changes in LLM inference state (KV cache, queue depth).
	// Stale data leads to suboptimal scheduling.
	defaultRefreshMetricsInterval = 50 * time.Millisecond

	// defaultRefreshPrometheusInterval (5s) defines how often EPP aggregates and exposes its own metrics.
	// This is decoupled from the scrape interval to reduce cardinality churn and load on the monitoring system.
	defaultRefreshPrometheusInterval = 5 * time.Second

	// defaultMetricsStaleness (2s) is the maximum age of backend metrics before a pod is considered unhealthy or
	// partitioned.
	// Expired pods are removed from the scheduling pool to prevent blackholing requests.
	defaultMetricsStaleness = 2 * time.Second

	// defaultEnablePprof enables /debug/pprof endpoints.
	// It defaults to true to aid debugging during the alpha phase, though production deployments may wish to disable it
	// for security.
	defaultEnablePprof = true

	// --- Security Defaults ---

	// defaultSecureServing enables TLS for the gRPC server.
	defaultSecureServing = true
	// defaultHealthChecking enables the gRPC Health Checking Protocol.
	defaultHealthChecking = false
	// defaultCertPath is empty, implying the use of self-signed certificates if SecureServing is enabled but no path is
	// provided.
	defaultCertPath = ""
	// defaultMetricsAuth enables authentication/authorization on the metrics endpoint.
	defaultMetricsAuth = true

	// --- Configuration Source Defaults ---

	defaultConfigFile = ""
	defaultConfigText = ""

	// --- Legacy Metric Defaults (vLLM Specific) ---
	// These defaults assume the backend is running vLLM.
	// Users of other model servers (e.g., TGI, TRT-LLM) must override these flags or use the Data Layer v2.

	defaultTotalQueuedMetric  = "vllm:num_requests_waiting"
	defaultTotalRunningMetric = "vllm:num_requests_running"
	defaultKVCacheMetric      = "vllm:kv_cache_usage_perc"
	defaultLoraInfoMetric     = "vllm:lora_requests_info"
	defaultCacheInfoMetric    = "vllm:cache_config_info"
)

// Options holds all configuration parameters required to initialize the EPP Runner.
// It separates configuration ingestion (flags) from execution logic.
type Options struct {
	// --- Identity Configuration ---

	// PoolName identifies the InferencePool this EPP instance manages.
	// It is mutually exclusive with EndpointSelector.
	PoolName string
	// PoolGroup specifies the API group of the InferencePool.
	PoolGroup string
	// PoolNamespace is the namespace of the InferencePool.
	// It is explicitly empty by default to allow for auto-discovery.
	// Resolution Order: Flag -> POD_NAMESPACE Env Var -> "default".
	PoolNamespace string
	// EndpointSelector is a label selector (e.g., "app=vllm,env=prod") used to discover model server pods directly.
	// It is mutually exclusive with PoolName.
	EndpointSelector string
	// EndpointTargetPorts is a comma-separated list of ports to target on the discovered pods.
	// Required when using EndpointSelector.
	EndpointTargetPorts string

	// --- Network Configuration ---

	// GrpcPort is the port on which the ExtProc gRPC server listens.
	GrpcPort int
	// GrpcHealthPort is the port used for gRPC liveness and readiness probes.
	GrpcHealthPort int
	// MetricsPort is the port used to expose Prometheus metrics.
	MetricsPort int

	// --- Security Configuration ---

	// SecureServing controls whether the gRPC server uses TLS.
	SecureServing bool
	// HealthChecking controls whether the health gRPC server is enabled.
	HealthChecking bool
	// CertPath is the directory path containing the server's TLS certificate (tls.crt) and private key (tls.key).
	// Required if SecureServing is true.
	CertPath string
	// MetricsEndpointAuth controls whether the metrics endpoint requires authentication and authorization (via
	// kube-rbac-proxy or similar).
	MetricsEndpointAuth bool

	// --- Observability Configuration ---

	// LogVerbosity controls the granularity of logs. Higher values result in more verbose logging.
	LogVerbosity int
	// EnablePprof enables the standard Go pprof debugging endpoints at /debug/pprof.
	// This should generally be disabled in production unless debugging active issues.
	EnablePprof bool
	// Tracing enables OpenTelemetry distributed tracing.
	Tracing bool
	// RefreshMetricsInterval is the frequency at which the runner scrapes metrics from backend pods.
	RefreshMetricsInterval time.Duration
	// RefreshPrometheusInterval is the frequency at which the runner updates its own exposed Prometheus metrics.
	RefreshPrometheusInterval time.Duration
	// MetricsStalenessThreshold is the duration after which a backend pod's metrics are considered too old to be useful
	// for scheduling decisions.
	MetricsStalenessThreshold time.Duration

	// --- High Availability Configuration ---

	// HaEnableElection enables Kubernetes leader election.
	// When enabled, only the leader will mark itself as ready, though all instances may serve traffic.
	HaEnableElection bool

	// --- ConfigSource Configuration ---

	// ConfigFile is the filesystem path to the EPP configuration file.
	// Mutually exclusive with ConfigText.
	ConfigFile string
	// ConfigText is the raw content of the configuration, typically injected via Downward API.
	// Mutually exclusive with ConfigFile.
	ConfigText string

	// LegacyMetrics holds deprecated flags for model server scraping.
	// These facilitate the transition to the Data Layer v2.
	LegacyMetrics LegacyMetricsOptions
}

// LegacyMetricsOptions defines configuration for the deprecated direct-pod scraping mechanism.
type LegacyMetricsOptions struct {
	// Port is the specific port to scrape on backend pods.
	// Deprecated: Use EndpointTargetPorts or InferencePool configuration instead.
	Port int
	// Path is the HTTP path to scrape for metrics (default: "/metrics").
	Path string
	// Scheme is the URI scheme to use for scraping ("http" or "https").
	Scheme string
	// InsecureSkipVerify disables TLS certificate validation when scraping pods via HTTPS.
	InsecureSkipVerify bool
	// TotalQueuedMetric is the name of the Prometheus metric representing queue depth.
	TotalQueuedMetric string
	// TotalRunningMetric is the name of the Prometheus metric representing active request count.
	TotalRunningMetric string
	// KVCacheMetric is the name of the Prometheus metric representing KV cache utilization (0.0-1.0).
	KVCacheMetric string
	// LoraInfoMetric is the name of the Prometheus metric containing LoRA adapter information.
	LoraInfoMetric string
	// CacheInfoMetric is the name of the Prometheus metric containing cache configuration info.
	CacheInfoMetric string
}

// NewOptions returns an Options struct initialized with project-standard defaults.
func NewOptions() *Options {
	return &Options{
		// --- Identity Defaults ---
		PoolName:  defaultPoolName,
		PoolGroup: defaultPoolGroup,
		// PoolNamespace must be empty ("") to allow extractGKNN to fall back to the POD_NAMESPACE environment variable.
		PoolNamespace: "",

		// --- Network Defaults ---
		GrpcPort:       DefaultGrpcPort,
		GrpcHealthPort: defaultGrpcHealthPort,
		MetricsPort:    defaultMetricsPort,

		// --- Security Defaults ---
		SecureServing:       defaultSecureServing,
		HealthChecking:      defaultHealthChecking,
		CertPath:            defaultCertPath,
		MetricsEndpointAuth: defaultMetricsAuth,

		// --- Observability Defaults ---
		LogVerbosity:              logging.DEFAULT,
		EnablePprof:               defaultEnablePprof,
		Tracing:                   true,
		RefreshMetricsInterval:    defaultRefreshMetricsInterval,
		RefreshPrometheusInterval: defaultRefreshPrometheusInterval,
		MetricsStalenessThreshold: defaultMetricsStaleness,

		// --- Config Defaults ---
		ConfigFile: defaultConfigFile,
		ConfigText: defaultConfigText,

		// --- Legacy Defaults ---
		LegacyMetrics: LegacyMetricsOptions{
			Path:               "/metrics",
			Scheme:             "http",
			InsecureSkipVerify: true,
			TotalQueuedMetric:  defaultTotalQueuedMetric,
			TotalRunningMetric: defaultTotalRunningMetric,
			KVCacheMetric:      defaultKVCacheMetric,
			LoraInfoMetric:     defaultLoraInfoMetric,
			CacheInfoMetric:    defaultCacheInfoMetric,
		},
	}
}

// AddFlags binds the Options fields to the provided FlagSet.
func (o *Options) AddFlags(fs *flag.FlagSet) {
	// --- Identity ---
	fs.StringVar(&o.PoolName, "pool-name", o.PoolName,
		"Name of the InferencePool this Endpoint Picker is associated with.")
	fs.StringVar(&o.PoolGroup, "pool-group", o.PoolGroup,
		"Group of the InferencePool this Endpoint Picker is associated with.")
	fs.StringVar(&o.PoolNamespace, "pool-namespace", o.PoolNamespace,
		"Namespace of the InferencePool. If unset, it attempts to resolve from the POD_NAMESPACE environment variable, finally defaulting to 'default'.")
	fs.StringVar(&o.EndpointSelector, "endpoint-selector", o.EndpointSelector,
		"Label selector to filter model server pods (e.g., 'app=vllm,env=prod'). Mutually exclusive with pool-name.")
	fs.StringVar(&o.EndpointTargetPorts, "endpoint-target-ports", o.EndpointTargetPorts,
		"Comma-separated list of target ports for model server pods.")

	// --- Network ---
	fs.IntVar(&o.GrpcPort, "grpc-port", o.GrpcPort,
		"The gRPC port used for communicating with Envoy proxy.")
	fs.IntVar(&o.GrpcHealthPort, "grpc-health-port", o.GrpcHealthPort,
		"The port used for gRPC liveness and readiness probes.")
	fs.IntVar(&o.MetricsPort, "metrics-port", o.MetricsPort,
		"The port used to expose Prometheus metrics.")

	// --- Security ---
	fs.BoolVar(&o.SecureServing, "secure-serving", o.SecureServing,
		"Enables secure serving via TLS.")
	fs.BoolVar(&o.HealthChecking, "health-checking", o.HealthChecking,
		"Enables gRPC health checking.")
	fs.StringVar(&o.CertPath, "cert-path", o.CertPath,
		"Path to the directory containing tls.crt and tls.key.")
	fs.BoolVar(&o.MetricsEndpointAuth, "metrics-endpoint-auth", o.MetricsEndpointAuth,
		"Enables authentication and authorization for the metrics endpoint.")

	// --- Observability ---
	fs.IntVar(&o.LogVerbosity, "v", o.LogVerbosity,
		"Log verbosity level.")
	fs.BoolVar(&o.EnablePprof, "enable-pprof", o.EnablePprof,
		"Enables pprof debugging handlers.")
	fs.BoolVar(&o.Tracing, "tracing", o.Tracing,
		"Enables distributed tracing.")
	fs.DurationVar(&o.RefreshMetricsInterval, "refresh-metrics-interval", o.RefreshMetricsInterval,
		"Interval to refresh backend metrics.")
	fs.DurationVar(&o.RefreshPrometheusInterval, "refresh-prometheus-metrics-interval", o.RefreshPrometheusInterval,
		"Interval to flush Prometheus metrics.")
	fs.DurationVar(&o.MetricsStalenessThreshold, "metrics-staleness-threshold", o.MetricsStalenessThreshold,
		"Duration after which backend metrics are considered stale.")

	// --- HA ---
	fs.BoolVar(&o.HaEnableElection, "ha-enable-leader-election", o.HaEnableElection,
		"Enables leader election for high availability.")

	// --- Config Source ---
	fs.StringVar(&o.ConfigFile, "config-file", o.ConfigFile,
		"Path to the configuration file.")
	fs.StringVar(&o.ConfigText, "config-text", o.ConfigText,
		"Inline configuration text (mutually exclusive with config-file).")

	// --- Legacy / Deprecated ---
	fs.IntVar(&o.LegacyMetrics.Port, "model-server-metrics-port", 0,
		"[DEPRECATED] Port to scrape metrics from pods.")
	fs.StringVar(&o.LegacyMetrics.Path, "model-server-metrics-path", o.LegacyMetrics.Path,
		"Path to scrape metrics from pods.")
	fs.StringVar(&o.LegacyMetrics.Scheme, "model-server-metrics-scheme", o.LegacyMetrics.Scheme,
		"Scheme to scrape metrics from pods (http or https).")
	fs.BoolVar(&o.LegacyMetrics.InsecureSkipVerify, "model-server-metrics-https-insecure-skip-verify", o.LegacyMetrics.InsecureSkipVerify,
		"Skip TLS verification when scraping model servers.")
	fs.StringVar(&o.LegacyMetrics.TotalQueuedMetric, "total-queued-requests-metric", o.LegacyMetrics.TotalQueuedMetric,
		"Prometheus metric for queued requests.")
	fs.StringVar(&o.LegacyMetrics.TotalRunningMetric, "total-running-requests-metric", o.LegacyMetrics.TotalRunningMetric,
		"Prometheus metric for running requests.")
	fs.StringVar(&o.LegacyMetrics.KVCacheMetric, "kv-cache-usage-percentage-metric", o.LegacyMetrics.KVCacheMetric,
		"Prometheus metric for KV-cache usage (0.0 to 1.0).")
	fs.StringVar(&o.LegacyMetrics.LoraInfoMetric, "lora-info-metric", o.LegacyMetrics.LoraInfoMetric,
		"Prometheus metric for LoRA info.")
	fs.StringVar(&o.LegacyMetrics.CacheInfoMetric, "cache-info-metric", o.LegacyMetrics.CacheInfoMetric,
		"Prometheus metric for cache info.")
}

// Validate checks constraints on the parsed flags.
// It returns an aggregated error if any configuration is invalid.
func (o *Options) Validate() error {
	hasPool := o.PoolName != ""
	hasSelector := o.EndpointSelector != ""
	if hasPool == hasSelector {
		return errors.New("configuration error: exactly one of --pool-name or --endpoint-selector must be provided")
	}

	if o.ConfigFile != "" && o.ConfigText != "" {
		return errors.New("configuration error: --config-file and --config-text cannot be used simultaneously")
	}

	if o.LegacyMetrics.Scheme != "http" && o.LegacyMetrics.Scheme != "https" {
		return fmt.Errorf(
			"configuration error: invalid metrics scheme %q (must be 'http' or 'https')",
			o.LegacyMetrics.Scheme)
	}

	if o.MetricsPort <= 0 || o.MetricsPort > 65535 {
		return fmt.Errorf("configuration error: invalid metrics port %d", o.MetricsPort)
	}

	return nil
}
