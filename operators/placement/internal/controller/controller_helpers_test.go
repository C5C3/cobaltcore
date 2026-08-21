// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// openBaoClusterStoreName is the default effective ClusterSecretStore a
// Placement selects when spec.secretStoreRef is omitted; the secrets
// sub-reconciler gates on its Ready condition.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// placementRequest is the reconcile request for the shared test-placement
// fixture.
var placementRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-placement"},
}

// testScheme registers the types the fake client resolves in this package's
// tests: core/apps (Secret, ConfigMap, Deployment), the placement API, the
// external-secrets v1 group the credential gate reads to attribute a missing
// Secret, and the Gateway API and MariaDB types the watches and the finalizer
// path exercise.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = placementv1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = gatewayv1.Install(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

// placementFakeClientBuilder returns a fake client builder with the placement
// scheme, the Placement field index the mappers resolve against, and the status
// subresource the controller writes.
func placementFakeClientBuilder(objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&placementv1alpha1.Placement{}).
		WithIndex(&placementv1alpha1.Placement{}, PlacementSecretNameIndexKey, placementSecretNameExtractor)
}

// newPlacementTestReconciler builds a PlacementReconciler over a fake client
// pre-loaded with objs (index and status subresource wired via the shared
// placementFakeClientBuilder).
func newPlacementTestReconciler(objs ...client.Object) *PlacementReconciler {
	return &PlacementReconciler{
		Client:   placementFakeClientBuilder(objs...).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

// newMapperFakeClient is the common-case shortcut for the watch-mapper tests:
// a pre-indexed fake client with the given objects seeded.
func newMapperFakeClient(objs ...client.Object) client.Client {
	return placementFakeClientBuilder(objs...).Build()
}

// testPlacement returns a minimal Placement CR carrying the serviceUser and
// database Secret references the index extractor reads.
func testPlacement() *placementv1alpha1.Placement {
	return &placementv1alpha1.Placement{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-placement",
			Namespace:  "default",
			UID:        "placement-uid",
			Generation: 1,
		},
		Spec: placementv1alpha1.PlacementSpec{
			OpenStackRelease: "2026.1",
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/placement", Tag: "2026.1"},
			Database:         commonv1.DatabaseSpec{Host: "db.example.com", Port: 3306, Database: "placement", SecretRef: commonv1.SecretRefSpec{Name: "placement-db"}},
			Cache:            commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			KeystoneEndpoint: "http://keystone.default.svc:5000",
			ServiceUser:      placementv1alpha1.ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "placement-service-user"}},
		},
	}
}

// readyClusterSecretStore returns a ClusterSecretStore with Ready=True so the
// secrets sub-reconciler proceeds past the store gate.
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

// notReadyClusterSecretStore returns a ClusterSecretStore whose Ready condition
// is explicitly False so the secrets sub-reconciler flips SecretsReady=False
// with reason SecretStoreNotReady.
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

// placementDBSecret returns the database credentials Secret referenced by
// testPlacement, carrying the username+password gate keys.
func placementDBSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placement-db", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("placement"), "password": []byte("db-pw")},
	}
}

// placementServiceUserSecret returns the service-user credentials Secret
// referenced by testPlacement, carrying the default "password" key with the
// given value.
func placementServiceUserSecret(password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placement-service-user", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte(password)},
	}
}

// collectEvents drains the FakeRecorder channel non-blocking.
func collectEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// getPlacement re-reads the Placement CR from the given client.
func getPlacement(t *testing.T, c client.Client, name string) *placementv1alpha1.Placement {
	t.Helper()
	var p placementv1alpha1.Placement
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &p); err != nil {
		t.Fatalf("re-reading Placement %s: %v", name, err)
	}
	return &p
}
