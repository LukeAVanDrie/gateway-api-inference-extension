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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
)

// Scheme is the global runtime scheme used by the manager.
// It is exported so unit tests can register their own types or use the same scheme.
var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(v1alpha2.Install(Scheme))
	utilruntime.Must(v1.Install(Scheme))
}

// ManagerOption allows tweaking the controller-runtime Options before the Manager is created.
// This is primarily used for testing (e.g., skipping name validation).
type ManagerOption func(*ctrl.Options)

// NewDefaultManager creates a new controller-runtime Manager.
// It configures strict cache filtering to ensure the EPP only watches resources related to its specific InferencePool,
// preventing OOMs in large clusters.
func NewDefaultManager(
	disableK8sCrdReconcile bool,
	gknn common.GKNN,
	restConfig *rest.Config,
	metricsServerOptions metricsserver.Options,
	leaderElectionEnabled bool,
	opts ...ManagerOption,
) (ctrl.Manager, error) {
	cacheOpts, err := buildCacheOptions(disableK8sCrdReconcile, gknn)
	if err != nil {
		return nil, fmt.Errorf("failed to configure cache options: %w", err)
	}

	ctrlOptions := ctrl.Options{
		Scheme:  Scheme,
		Cache:   cacheOpts,
		Metrics: metricsServerOptions,
	}

	if leaderElectionEnabled {
		ctrlOptions.LeaderElection = true
		ctrlOptions.LeaderElectionResourceLock = "leases"
		ctrlOptions.LeaderElectionID = generateLeaderElectionID(gknn)
		ctrlOptions.LeaderElectionNamespace = gknn.Namespace
		ctrlOptions.LeaderElectionReleaseOnCancel = true
	}

	for _, opt := range opts {
		opt(&ctrlOptions)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrlOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to create controller manager: %w", err)
	}

	return mgr, nil
}

// buildCacheOptions constructs the cache filtering logic.
// This is critical for performance; we must restrict the Informer's scope to the specific Namespace and, where
// possible, the specific Name of the resources.
func buildCacheOptions(disableCRDs bool, gknn common.GKNN) (cache.Options, error) {
	// Base configuration: Always filter by Namespace.
	defaultNamespaces := map[string]cache.Config{
		gknn.Namespace: {},
	}

	// 1. Core Resources (Always Watched)
	// We watch Pods to gather metrics and update scheduling decisions.
	byObject := map[client.Object]cache.ByObject{
		&corev1.Pod{}: {Namespaces: map[string]cache.Config{
			gknn.Namespace: {},
		}},
	}

	// 2. CRD Resources (Conditional)
	// If running in "Selector Mode" (no CRDs), we must NOT try to watch these or the manager will crash if the CRDs are
	// missing from the cluster.
	if !disableCRDs {
		// Objectives and Rewrites are scoped to the namespace.
		byObject[&v1alpha2.InferenceObjective{}] = cache.ByObject{Namespaces: map[string]cache.Config{
			gknn.Namespace: {},
		}}
		byObject[&v1alpha2.InferenceModelRewrite{}] = cache.ByObject{Namespaces: map[string]cache.Config{
			gknn.Namespace: {},
		}}

		// InferencePool is scoped to the specific NAME.
		// We only care about the pool we are assigned to manage.
		poolFilter := cache.Config{
			FieldSelector: fields.SelectorFromSet(fields.Set{"metadata.name": gknn.Name}),
		}

		// Handle API Group versions.
		switch gknn.Group {
		case v1alpha2.GroupName:
			byObject[&v1alpha2.InferencePool{}] = cache.ByObject{
				Namespaces: map[string]cache.Config{gknn.Namespace: poolFilter},
			}
		case v1.GroupName:
			byObject[&v1.InferencePool{}] = cache.ByObject{
				Namespaces: map[string]cache.Config{gknn.Namespace: poolFilter},
			}
		default:
			return cache.Options{}, fmt.Errorf("unsupported InferencePool group: %s", gknn.Group)
		}
	}

	return cache.Options{
		ByObject:          byObject,
		DefaultNamespaces: defaultNamespaces,
	}, nil
}

func generateLeaderElectionID(gknn common.GKNN) string {
	// The ID must be unique per EPP deployment to prevent conflict between different pools.
	return fmt.Sprintf("epp-%s-%s.gateway-api-inference-extension.sigs.k8s.io", gknn.Namespace, gknn.Name)
}
