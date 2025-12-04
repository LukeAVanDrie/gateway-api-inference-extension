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
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
)

func TestBuildCacheOptions(t *testing.T) {
	t.Parallel()

	// Define the expected Identity.
	gknn := common.GKNN{
		NamespacedName: types.NamespacedName{Name: "test-pool", Namespace: "env-prod"},
		GroupKind: schema.GroupKind{
			Group: v1alpha2.GroupName,
			Kind:  "InferencePool",
		},
	}

	tests := []struct {
		name        string
		disableCRDs bool
		group       string // Override group if needed.
		check       func(*testing.T, cache.Options)
	}{
		{
			name:        "Standard Mode: Watches CRDs and Pods",
			disableCRDs: false,
			check: func(t *testing.T, opts cache.Options) {
				// 1. Check Default Namespace
				require.NotNil(t, opts.DefaultNamespaces, "DefaultNamespaces map should not be nil")
				assert.Contains(t, opts.DefaultNamespaces, "env-prod")

				// 2. Check Core Resources (Pods)
				podConfig, found := getConfig(t, opts, &corev1.Pod{})
				require.True(t, found, "should watch Pods")
				assert.Contains(t, podConfig.Namespaces, "env-prod")

				// 3. Check CRDs
				_, foundObj := getConfig(t, opts, &v1alpha2.InferenceObjective{})
				assert.True(t, foundObj, "should watch InferenceObjectives")

				_, foundRewrite := getConfig(t, opts, &v1alpha2.InferenceModelRewrite{})
				assert.True(t, foundRewrite, "should watch InferenceModelRewrites")

				// 4. Check Pool Filtering
				poolConfig, foundPool := getConfig(t, opts, &v1alpha2.InferencePool{})
				require.True(t, foundPool, "should watch InferencePool (v1alpha2)")

				nsConfig, ok := poolConfig.Namespaces["env-prod"]
				require.True(t, ok, "InferencePool should be watched in env-prod")
				require.NotNil(t, nsConfig.FieldSelector, "InferencePool must have FieldSelector")
				assert.Equal(t, "metadata.name=test-pool", nsConfig.FieldSelector.String())
			},
		},
		{
			name:        "Selector Mode: No CRDs",
			disableCRDs: true,
			check: func(t *testing.T, opts cache.Options) {
				// 1. Pods still watched
				_, found := getConfig(t, opts, &corev1.Pod{})
				assert.True(t, found, "should watch Pods in selector mode")

				// 2. CRDs MUST NOT be watched
				_, foundObj := getConfig(t, opts, &v1alpha2.InferenceObjective{})
				assert.False(t, foundObj, "must not watch Objectives in selector mode")

				_, foundPool := getConfig(t, opts, &v1alpha2.InferencePool{})
				assert.False(t, foundPool, "must not watch Pools in selector mode")
			},
		},
		{
			name:        "Legacy API Group Support",
			disableCRDs: false,
			group:       v1.GroupName, // "inference.networking.x-k8s.io"
			check: func(t *testing.T, opts cache.Options) {
				// Check that we watch the V1 pool, not V1Alpha2.
				_, foundV1 := getConfig(t, opts, &v1.InferencePool{})
				assert.True(t, foundV1, "should watch v1.InferencePool")

				_, foundV1Alpha2 := getConfig(t, opts, &v1alpha2.InferencePool{})
				assert.False(t, foundV1Alpha2, "should NOT watch v1alpha2.InferencePool")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			localGKNN := gknn
			if tc.group != "" {
				localGKNN.Group = tc.group
			}

			opts, err := buildCacheOptions(tc.disableCRDs, localGKNN)
			require.NoError(t, err)
			tc.check(t, opts)
		})
	}
}

// getConfig is a helper to find a cache configuration by object Type.
// This works around the fact that cache.ByObject keys are pointers, and strict equality fails.
func getConfig(t *testing.T, opts cache.Options, obj client.Object) (cache.ByObject, bool) {
	t.Helper()
	targetType := reflect.TypeOf(obj)
	for k, v := range opts.ByObject {
		if reflect.TypeOf(k) == targetType {
			return v, true
		}
	}
	return cache.ByObject{}, false
}
