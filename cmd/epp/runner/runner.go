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
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	configapi "sigs.k8s.io/gateway-api-inference-extension/apix/config/v1alpha1"
	"sigs.k8s.io/gateway-api-inference-extension/internal/runnable"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	backendmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/config"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/config/loader"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer"
	dlmetrics "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datalayer/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol"
	fccontroller "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/controller"
	fcregistry "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/flowcontrol/registry"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics/collectors"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/plugins"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/requestcontrol"
	testresponsereceived "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/requestcontrol/plugins/test/responsereceived"
	satctrl "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationcontroller/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationcontroller/framework/plugins/staticthreshold"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/multi/prefix"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/multi/slo_aware_router"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/picker"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/profile"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/scorer"
	testfilter "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/framework/plugins/test/filter"
	runserver "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/server"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/env"
	"sigs.k8s.io/gateway-api-inference-extension/version"
)

// Runner orchestrates the lifecycle of the Endpoint Picker (EPP).
// It handles configuration loading, dependency injection, and the startup of the main event loop.
type Runner struct {
	// --- Identity ---
	executableName string

	// --- Configuration ---
	options              *Options
	requestControlConfig *requestcontrol.Config
	schedulerConfig      *scheduling.SchedulerConfig

	// --- Internal State ---
	featureGates     map[string]bool
	customCollectors []prometheus.Collector
	log              logr.Logger
}

// NewRunner creates a new Runner with production-ready defaults.
func NewRunner() *Runner {
	return &Runner{
		executableName:       "GIE",
		options:              NewOptions(),
		requestControlConfig: requestcontrol.NewConfig(),
		customCollectors:     []prometheus.Collector{},
	}
}

// WithExecutableName sets the name used in startup logs.
func (r *Runner) WithExecutableName(name string) *Runner {
	r.executableName = name
	return r
}

// WithRequestControlConfig allows injecting a custom request control configuration (mostly for tests).
func (r *Runner) WithRequestControlConfig(cfg *requestcontrol.Config) *Runner {
	r.requestControlConfig = cfg
	return r
}

// WithSchedulerConfig allows injecting a custom scheduler configuration (mostly for tests).
func (r *Runner) WithSchedulerConfig(cfg *scheduling.SchedulerConfig) *Runner {
	r.schedulerConfig = cfg
	return r
}

// WithCustomCollectors registers additional Prometheus collectors during startup.
func (r *Runner) WithCustomCollectors(collectors ...prometheus.Collector) *Runner {
	r.customCollectors = append(r.customCollectors, collectors...)
	return r
}

// Run executes the EPP application lifecycle.
// This is the entry point for the main binary.
func (r *Runner) Run(ctx context.Context) error {
	// 1. Initialize Configuration & Logging
	r.options.AddFlags(flag.CommandLine)
	flag.Parse()

	r.initLogging()
	r.log.Info("Initializing EPP Runner",
		"executable", r.executableName,
		"commit", version.CommitSHA,
		"build", version.BuildRef)

	if err := r.options.Validate(); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	if r.options.Tracing {
		if err := common.InitTracing(ctx, r.log); err != nil {
			return fmt.Errorf("failed to initialize tracing: %w", err)
		}
	}

	// 2. Load Configuration (Phase 1: Raw Parsing)
	rawConfig, err := r.loadRawConfig()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// 3. Setup Dependencies (Datastore & Metrics)
	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get Kubernetes REST config: %w", err)
	}

	ds, err := r.setupDatastore(ctx, rawConfig.FeatureGates)
	if err != nil {
		return fmt.Errorf("failed to initialize datastore: %w", err)
	}

	// 4. Instantiate Plugins (Phase 2: Configuration)
	eppConfig, err := r.instantiatePlugins(ctx, rawConfig, ds)
	if err != nil {
		return fmt.Errorf("failed to instantiate plugins: %w", err)
	}

	// 5. Build Core Components (Director, Scheduler, Admission)
	director, err := r.buildRequestDirector(ctx, ds, eppConfig)
	if err != nil {
		return fmt.Errorf("failed to build request director: %w", err)
	}

	// 6. Setup Controller Manager
	mgr, err := r.setupManager(ctx, kubeConfig, ds, director)
	if err != nil {
		return fmt.Errorf("failed to setup controller manager: %w", err)
	}

	// 7. Start the System
	r.log.Info("Starting Controller Manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}

	return nil
}

// --- Initialization Helpers ---

func (r *Runner) initLogging() {
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)

	// Honor the -v flag if the specific zap-log-level flag wasn't set.
	useV := true
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "zap-log-level" {
			useV = false
		}
	})
	if useV {
		opts.Level = uberzap.NewAtomicLevelAt(zapcore.Level(int8(-1 * r.options.LogVerbosity)))
	}

	r.log = zap.New(zap.UseFlagOptions(&opts), zap.RawZapOpts(uberzap.AddCaller())).WithName("setup")
	ctrl.SetLogger(r.log)
}

func (r *Runner) loadRawConfig() (*configapi.EndpointPickerConfig, error) {
	var configBytes []byte
	var err error

	switch {
	case r.options.ConfigText != "":
		configBytes = []byte(r.options.ConfigText)
	case r.options.ConfigFile != "":
		configBytes, err = os.ReadFile(r.options.ConfigFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read config file %q: %w", r.options.ConfigFile, err)
		}
	default:
		return nil, nil // Valid scenario (code-only configuration)
	}

	// Pre-register known gates and plugins.
	loader.RegisterFeatureGate(datalayer.FeatureGate)
	loader.RegisterFeatureGate(flowcontrol.FeatureGate)
	r.registerInTreePlugins()

	rawConfig, featureGates, err := loader.LoadRawConfig(configBytes, r.log)
	if err != nil {
		return nil, err
	}

	r.featureGates = featureGates
	return rawConfig, nil
}

func (r *Runner) instantiatePlugins(
	ctx context.Context,
	rawConfig *configapi.EndpointPickerConfig,
	ds datastore.Datastore,
) (*config.Config, error) {
	// Apply deprecation overrides (Anti-Corruption Layer).
	applyDeprecatedOverrides(r.log, rawConfig)

	// Create handle with a Pod listing closure.
	handle := plugins.NewEppHandle(ctx, makePodListFunc(ds))

	// Instantiate & configure plugins.
	cfg, err := loader.InstantiateAndConfigure(rawConfig, handle, r.log)
	if err != nil {
		return nil, err
	}
	r.schedulerConfig = cfg.SchedulerConfig

	// Initialize RequestControl config.
	r.requestControlConfig.AddPlugins(handle.GetAllPlugins()...)
	if err := r.requestControlConfig.PrepareDataPluginGraph(); err != nil {
		return nil, fmt.Errorf("cyclic dependency detected in prepare data plugins: %w", err)
	}

	return cfg, nil
}

// --- Component Setup ---

func (r *Runner) setupDatastore(ctx context.Context, featureGates []string) (datastore.Datastore, error) {
	// 1. Determine Metrics Strategy
	useDatalayer := false
	for _, g := range featureGates {
		if g == datalayer.FeatureGate {
			useDatalayer = true
			break
		}
	}

	// Fallback to legacy env var (via direct check, as this happens before config override application).
	if _, ok := os.LookupEnv(envEnableDatalayerV2); ok {
		if env.GetEnvBool(envEnableDatalayerV2, false, r.log) {
			useDatalayer = true
		}
	}

	var epFactory datalayer.EndpointFactory
	var err error

	if useDatalayer {
		epFactory, err = r.setupDatalayerV2()
	} else {
		epFactory, err = r.setupMetricsV1()
	}
	if err != nil {
		return nil, err
	}

	// 2. Initialize Datastore
	// If EndpointSelector is set, we run in "Standalone Mode" (no CRD reconciliation).
	disableReconcile := r.options.EndpointSelector != ""

	if !disableReconcile {
		return datastore.NewDatastore(ctx, epFactory, int32(r.options.LegacyMetrics.Port)), nil
	}

	// Standalone Mode: Configure static endpoint pool.
	pool := datalayer.NewEndpointPool(r.options.PoolNamespace, r.options.PoolName)
	pool.Selector, err = labels.ConvertSelectorToLabelsMap(r.options.EndpointSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint-selector: %w", err)
	}
	pool.TargetPorts, err = strToUniqueIntSlice(r.options.EndpointTargetPorts)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint-target-ports: %w", err)
	}

	return datastore.NewDatastore(ctx, epFactory, int32(r.options.LegacyMetrics.Port), datastore.WithEndpointPool(pool)), nil
}

func (r *Runner) buildRequestDirector(
	ctx context.Context,
	ds datastore.Datastore,
	eppConfig *config.Config,
) (*requestcontrol.Director, error) {
	if r.schedulerConfig == nil {
		return nil, errors.New("scheduler configuration is missing")
	}
	scheduler := scheduling.NewSchedulerWithConfig(r.schedulerConfig)

	// Extract Saturation Controller from plugins.
	var satCtrl satctrl.SaturationController
	for _, p := range eppConfig.Handle.GetAllPlugins() {
		if ctrl, ok := p.(satctrl.SaturationController); ok {
			satCtrl = ctrl
			break
		}
	}
	if satCtrl == nil {
		return nil, errors.New("critical: saturation controller plugin not found in configuration")
	}

	podLocator := requestcontrol.NewDatastorePodLocator(ds)
	cachedPodLocator := requestcontrol.NewCachedPodLocator(ctx, podLocator, time.Minute)

	// Initialize Admission Controller.
	var admissionCtrl requestcontrol.AdmissionController
	if r.featureGates[flowcontrol.FeatureGate] {
		r.log.Info("Initializing Flow Control Layer")

		// Hardcoded default configuration for Flow Control.
		// TODO: Expose this via text-based configuration.
		fcCfg := flowcontrol.Config{
			Controller: fccontroller.Config{},
			Registry: fcregistry.Config{
				PriorityBands: []fcregistry.PriorityBandConfig{
					{Priority: 0, PriorityName: "Default"},
				},
			},
		}

		registry, err := fcregistry.NewFlowRegistry(fcCfg.Registry, r.log)
		if err != nil {
			return nil, fmt.Errorf("failed to create flow registry: %w", err)
		}

		fc, err := fccontroller.NewFlowController(ctx, fcCfg.Controller, registry, nil, r.log)
		if err != nil {
			return nil, fmt.Errorf("failed to create flow controller: %w", err)
		}

		go registry.Run(ctx)
		admissionCtrl = requestcontrol.NewFlowControlAdmissionController(fc)
	} else {
		admissionCtrl = requestcontrol.NewLegacyAdmissionController(satCtrl, cachedPodLocator)
	}

	return requestcontrol.NewDirectorWithConfig(
		ds,
		scheduler,
		admissionCtrl,
		cachedPodLocator,
		r.requestControlConfig,
	), nil
}

func (r *Runner) setupManager(
	ctx context.Context,
	cfg *rest.Config,
	ds datastore.Datastore,
	director *requestcontrol.Director,
) (manager.Manager, error) {
	// 1. Resolve Identity (GKNN)
	gknn, err := extractGKNN(r.options.PoolName, r.options.PoolGroup, r.options.PoolNamespace, r.options.EndpointSelector)
	if err != nil {
		return nil, err
	}

	// 2. Configure Metrics Server
	metricsOpts := metricsserver.Options{
		BindAddress: fmt.Sprintf(":%d", r.options.MetricsPort),
	}
	if r.options.MetricsEndpointAuth {
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// 3. Register Custom Collectors
	r.customCollectors = append(r.customCollectors, collectors.NewInferencePoolMetricsCollector(ds))
	metrics.Register(r.customCollectors...)
	metrics.RecordInferenceExtensionInfo(version.CommitSHA, version.BuildRef)

	// 4. Create Manager
	mgr, err := runserver.NewDefaultManager(
		r.options.EndpointSelector != "", // disable K8sReconcile
		*gknn,
		cfg,
		metricsOpts,
		r.options.HaEnableElection,
	)
	if err != nil {
		return nil, err
	}

	// 5. Setup Leader Election / Readiness
	isLeader := &atomic.Bool{}
	isLeader.Store(!r.options.HaEnableElection) // If election disabled, we are effectively leader.
	if r.options.HaEnableElection {
		go func() {
			<-mgr.Elected()
			isLeader.Store(true)
			r.log.Info("Instance elected as leader")
		}()
	}

	// 6. Setup Pprof (if enabled)
	if r.options.EnablePprof {
		if err := setupPprofHandlers(mgr); err != nil {
			return nil, err
		}
	}

	// 7. Register Health Server
	if err := r.registerHealthServer(mgr, ds, isLeader); err != nil {
		return nil, err
	}

	// 8. Register ExtProc Server (Main Event Loop)
	extProcRunner := &runserver.ExtProcServerRunner{
		GKNN:                             *gknn,
		Datastore:                        ds,
		Director:                         director,
		GrpcPort:                         r.options.GrpcPort,
		SecureServing:                    r.options.SecureServing,
		HealthChecking:                   r.options.HealthChecking,
		CertPath:                         r.options.CertPath,
		DisableK8sCrdReconcile:           r.options.EndpointSelector != "",
		RefreshPrometheusMetricsInterval: r.options.RefreshPrometheusInterval,
		MetricsStalenessThreshold:        r.options.MetricsStalenessThreshold,
		UseExperimentalDatalayerV2:       r.featureGates[datalayer.FeatureGate],
	}

	if err := extProcRunner.SetupWithManager(ctx, mgr); err != nil {
		return nil, fmt.Errorf("failed to setup ExtProc runner: %w", err)
	}

	return mgr, nil
}

// --- Internal Helpers ---

func (r *Runner) registerInTreePlugins() {
	plugins.Register(prefix.PrefixCachePluginType, prefix.PrefixCachePluginFactory)
	plugins.Register(picker.MaxScorePickerType, picker.MaxScorePickerFactory)
	plugins.Register(picker.RandomPickerType, picker.RandomPickerFactory)
	plugins.Register(picker.WeightedRandomPickerType, picker.WeightedRandomPickerFactory)
	plugins.Register(profile.SingleProfileHandlerType, profile.SingleProfileHandlerFactory)
	plugins.Register(scorer.KvCacheUtilizationScorerType, scorer.KvCacheUtilizationScorerFactory)
	plugins.Register(scorer.QueueScorerType, scorer.QueueScorerFactory)
	plugins.Register(scorer.LoraAffinityScorerType, scorer.LoraAffinityScorerFactory)
	plugins.Register(slo_aware_router.SLOAwareRouterPluginType, slo_aware_router.SLOAwareRouterFactory)
	plugins.Register(profile.SLOAwareProfileHandlerType, profile.SLOAwareProfileHandlerFactory)
	plugins.Register(testfilter.HeaderBasedTestingFilterType, testfilter.HeaderBasedTestingFilterFactory)
	plugins.Register(testresponsereceived.DestinationEndpointServedVerifierType, testresponsereceived.DestinationEndpointServedVerifierFactory)
	plugins.Register(dlmetrics.MetricsDataSourceType, dlmetrics.MetricsDataSourceFactory)
	plugins.Register(dlmetrics.MetricsExtractorType, dlmetrics.ModelServerExtractorFactory)
	plugins.Register(staticthreshold.StaticThresholdSaturationControllerType, staticthreshold.StaticThresholdSaturationControllerFactory)
}

func (r *Runner) setupDatalayerV2() (datalayer.EndpointFactory, error) {
	legacy := r.options.LegacyMetrics
	source := dlmetrics.NewMetricsDataSource(legacy.Scheme, legacy.Path, legacy.InsecureSkipVerify)

	extractor, err := dlmetrics.NewModelServerExtractor(
		legacy.TotalQueuedMetric,
		legacy.TotalRunningMetric,
		legacy.KVCacheMetric,
		legacy.LoraInfoMetric,
		legacy.CacheInfoMetric,
	)
	if err != nil {
		return nil, err
	}

	if err := source.AddExtractor(extractor); err != nil {
		return nil, err
	}
	if err := datalayer.RegisterSource(source); err != nil {
		return nil, err
	}

	// Sources are now registered globally (singleton pattern in datalayer package).
	// TODO: this could be moved to the configuration loading functions once ported over.
	return datalayer.NewEndpointFactory(datalayer.GetSources(), r.options.RefreshMetricsInterval), nil
}

func (r *Runner) setupMetricsV1() (datalayer.EndpointFactory, error) {
	legacy := r.options.LegacyMetrics

	mapping, err := backendmetrics.NewMetricMapping(
		legacy.TotalQueuedMetric,
		legacy.TotalRunningMetric,
		legacy.KVCacheMetric,
		legacy.LoraInfoMetric,
		legacy.CacheInfoMetric,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create metric mapping: %w", err)
	}

	httpClient := http.DefaultClient
	if legacy.Scheme == "https" {
		httpClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: legacy.InsecureSkipVerify},
			},
		}
	}

	return backendmetrics.NewPodMetricsFactory(&backendmetrics.PodMetricsClientImpl{
		MetricMapping:            mapping,
		ModelServerMetricsPath:   legacy.Path,
		ModelServerMetricsScheme: legacy.Scheme,
		Client:                   httpClient,
	}, r.options.RefreshMetricsInterval), nil
}

func (r *Runner) registerHealthServer(mgr manager.Manager, ds datastore.Datastore, isLeader *atomic.Bool) error {
	srv := grpc.NewServer()
	healthPb.RegisterHealthServer(srv, &healthServer{
		logger:                r.log.WithName("health"),
		datastore:             ds,
		isLeader:              isLeader,
		leaderElectionEnabled: r.options.HaEnableElection,
	})

	if err := mgr.Add(runnable.NoLeaderElection(runnable.GRPCServer("health", srv, r.options.GrpcHealthPort))); err != nil {
		return fmt.Errorf("failed to register health server: %w", err)
	}
	return nil
}

// --- Utilities ---

func makePodListFunc(ds datastore.Datastore) func() []types.NamespacedName {
	return func() []types.NamespacedName {
		pods := ds.PodList(backendmetrics.AllPodsPredicate)
		out := make([]types.NamespacedName, len(pods))
		for i, p := range pods {
			out[i] = p.GetPod().NamespacedName
		}
		return out
	}
}

func setupPprofHandlers(mgr manager.Manager) error {
	profiles := []string{"heap", "goroutine", "allocs", "threadcreate", "block", "mutex"}
	for _, p := range profiles {
		if err := mgr.AddMetricsServerExtraHandler("/debug/pprof/"+p, pprof.Handler(p)); err != nil {
			return err
		}
	}
	return nil
}

func extractGKNN(poolName, poolGroup, poolNamespace, endpointSelector string) (*common.GKNN, error) {
	// Mode 1: InferencePool Reconciliation
	if poolName != "" {
		ns := poolNamespace
		if ns == "" {
			if env := os.Getenv("NAMESPACE"); env != "" {
				ns = env
			} else {
				// Fallback only if ENV is missing, though NewOptions leaves this empty by default.
				ns = "default"
			}
		}
		return &common.GKNN{
			NamespacedName: types.NamespacedName{Name: poolName, Namespace: ns},
			GroupKind:      schema.GroupKind{Group: poolGroup, Kind: "InferencePool"},
		}, nil
	}

	// Mode 2: Standalone Deployment (Selector Mode)
	if endpointSelector != "" {
		podName := os.Getenv("POD_NAME")
		if podName == "" {
			return nil, errors.New("environment variable POD_NAME is required when using --endpoint-selector")
		}

		eppName, err := extractDeploymentName(podName)
		if err != nil {
			return nil, err
		}

		ns := poolNamespace
		if ns == "" {
			ns = os.Getenv("NAMESPACE")
		}
		if ns == "" {
			ns = "default"
		}

		return &common.GKNN{
			NamespacedName: types.NamespacedName{Namespace: ns, Name: eppName},
			GroupKind:      schema.GroupKind{Kind: "Deployment", Group: "apps"},
		}, nil
	}

	return nil, errors.New("invalid configuration: must specify either --pool-name or --endpoint-selector")
}

func extractDeploymentName(podName string) (string, error) {
	regex := regexp.MustCompile(`^(.+)-[a-z0-9]+-[a-z0-9]+$`)
	matches := regex.FindStringSubmatch(podName)
	if len(matches) == 2 {
		return matches[1], nil
	}
	return "", fmt.Errorf("failed to parse deployment name from pod name %q", podName)
}

func strToUniqueIntSlice(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	seen := sets.NewInt()
	var out []int
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			v, err := strconv.Atoi(t)
			if err != nil {
				return nil, fmt.Errorf("invalid port value %q: %w", t, err)
			}
			if !seen.Has(v) {
				seen.Insert(v)
				out = append(out, v)
			}
		}
	}
	return out, nil
}
