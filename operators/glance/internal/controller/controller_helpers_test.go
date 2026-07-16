// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/secrets"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// openBaoClusterStoreName is the default effective ClusterSecretStore a Glance
// selects when spec.secretStoreRef is omitted; the secrets sub-reconciler gates
// on its Ready condition.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// newGlanceTestReconciler builds a GlanceReconciler over a fake client
// pre-loaded with objs (indexes and status subresources wired via the shared
// glanceFakeClientBuilder).
func newGlanceTestReconciler(objs ...client.Object) *GlanceReconciler {
	return &GlanceReconciler{
		Client:   glanceFakeClientBuilder(objs...).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
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

// glanceDBSecret returns the database credentials Secret referenced by
// testGlance, carrying the username+password gate keys.
func glanceDBSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "glance-db", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("glance"), "password": []byte("db-pw")},
	}
}

// glanceServiceUserSecret returns the service-user credentials Secret referenced
// by testGlance, carrying the default "password" key.
func glanceServiceUserSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "glance-service-user", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("svc-pw")},
	}
}

// credentialReadyBackend builds an S3 GlanceBackend attached to glanceRef with
// its CredentialsReady condition already True (the D-gate the projection reads)
// and the given isDefault flag.
func credentialReadyBackend(name, glanceRef string, isDefault bool) *glancev1alpha1.GlanceBackend {
	b := testGlanceBackend(name, glanceRef)
	b.Spec.IsDefault = isDefault
	b.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeCredentialsReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonCredentialsAvailable,
		LastTransitionTime: metav1.Now(),
	}}
	return b
}

// getGlance re-reads the Glance CR from the given client.
func getGlance(t *testing.T, c client.Client, name string) *glancev1alpha1.Glance {
	t.Helper()
	var g glancev1alpha1.Glance
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &g); err != nil {
		t.Fatalf("re-reading Glance %s: %v", name, err)
	}
	return &g
}
