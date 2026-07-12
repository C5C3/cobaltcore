// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Shared fixtures for the Horizon controller unit tests.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

// openBaoClusterStoreName aliases the shared ClusterSecretStore name
// (secrets.OpenBaoClusterStoreName) for the ClusterSecretStore fixtures in
// this package's tests.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = autoscalingv2.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = gatewayv1.Install(s)
	_ = horizonv1alpha1.AddToScheme(s)
	return s
}

// testHorizon returns a minimal valid Horizon CR mirroring what the
// defaulting webhook would admit.
func testHorizon() *horizonv1alpha1.Horizon {
	return &horizonv1alpha1.Horizon{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-horizon",
			Namespace:  "default",
			UID:        "hz-uid",
			Generation: 1,
		},
		Spec: horizonv1alpha1.HorizonSpec{
			Deployment: horizonv1alpha1.DeploymentSpec{Replicas: 3},
			Image:      commonv1.ImageSpec{Repository: "ghcr.io/c5c3/horizon", Tag: "2025.2"},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "memcached"},
				Backend:    horizonv1alpha1.DefaultCacheBackend,
			},
			KeystoneEndpoint: "http://keystone.default.svc.cluster.local:5000/v3",
			SecretKeyRef:     commonv1.SecretRefSpec{Name: "horizon-secret-key", Key: "secret-key"},
		},
	}
}

// newTestReconciler creates a HorizonReconciler backed by a fake client
// pre-populated with the given objects.
func newTestReconciler(s *runtime.Scheme, objs ...client.Object) *HorizonReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	cb = cb.WithStatusSubresource(&horizonv1alpha1.Horizon{})
	return &HorizonReconciler{
		Client: cb.Build(),
		Scheme: s,
	}
}

// newUpdateStatusReconciler builds a HorizonReconciler backed by a fake client
// seeded with the given Horizon. When statusUpdateErr is non-nil the status
// subresource update is intercepted to always fail, so a skipped write is
// observable as a nil error return.
func newUpdateStatusReconciler(t *testing.T, h *horizonv1alpha1.Horizon, statusUpdateErr error) (*HorizonReconciler, *horizonv1alpha1.Horizon) {
	t.Helper()
	s := testScheme()
	cb := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(h.DeepCopy()).
		WithStatusSubresource(&horizonv1alpha1.Horizon{})
	if statusUpdateErr != nil {
		cb = cb.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return statusUpdateErr
			},
		})
	}
	c := cb.Build()

	// Re-fetch so the object carries the ResourceVersion assigned by the fake client.
	fetched := &horizonv1alpha1.Horizon{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(h), fetched); err != nil {
		t.Fatalf("fetching Horizon from fake client: %v", err)
	}
	return &HorizonReconciler{Client: c, Scheme: s}, fetched
}

// readyClusterSecretStore returns a ClusterSecretStore with a Ready=True
// status condition so reconcileSecrets proceeds past the store gate.
func readyClusterSecretStore(name string) *esov1.ClusterSecretStore {
	return &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// notReadyClusterSecretStore returns a ClusterSecretStore whose Ready
// condition is explicitly False so reconcileSecrets flips SecretsReady to
// False with reason SecretStoreNotReady.
func notReadyClusterSecretStore(name string) *esov1.ClusterSecretStore {
	return &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionFalse},
			},
		},
	}
}

// readySecretStore returns a namespaced SecretStore with a Ready=True status
// condition so reconcileSecrets proceeds past the store gate for a Horizon that
// selects a per-tenant store.
func readySecretStore(name, namespace string) *esov1.SecretStore {
	return &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// secretKeySecret returns the materialized SECRET_KEY Secret the ESO
// ExternalSecret would sync.
func secretKeySecret(name, namespace, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

// autoscalingSpecWithCPU returns an AutoscalingSpec with a CPU target and the
// given replica bounds.
func autoscalingSpecWithCPU(minReplicas, maxReplicas int32) *horizonv1alpha1.AutoscalingSpec {
	return &horizonv1alpha1.AutoscalingSpec{
		MinReplicas:          &minReplicas,
		MaxReplicas:          maxReplicas,
		TargetCPUUtilization: ptr.To(int32(80)),
	}
}
