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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// The Barbican-side test fixtures. The scheme and the store fixtures they build
// on live in barbicansecretstore_controller_test.go, so the package has exactly
// one testScheme and one Barbican CR fixture.

// openBaoClusterStoreName is the default effective ClusterSecretStore a Barbican
// selects when spec.secretStoreRef is omitted; the secrets sub-reconciler gates
// on its Ready condition.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// barbicanRequest is the reconcile request for the shared testBarbican fixture.
var barbicanRequest = reconcile.Request{
	NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testBarbicanName},
}

// barbicanFakeClientBuilder returns a fake client builder with the package
// scheme, the field indexes reconcileSecretStores and the watch mappers resolve
// against, and the status subresources both controllers write.
func barbicanFakeClientBuilder(objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&barbicanv1alpha1.Barbican{}, &barbicanv1alpha1.BarbicanSecretStore{}).
		WithIndex(&barbicanv1alpha1.Barbican{}, BarbicanSecretNameIndexKey, barbicanSecretNameExtractor).
		WithIndex(&barbicanv1alpha1.BarbicanSecretStore{}, BarbicanSecretStoreBarbicanRefIndexKey, barbicanSecretStoreBarbicanRefExtractor).
		WithIndex(&barbicanv1alpha1.BarbicanSecretStore{}, BarbicanSecretStoreInstanceRefIndexKey, barbicanSecretStoreInstanceRefExtractor).
		WithIndex(&barbicanv1alpha1.BarbicanSecretStore{}, BarbicanSecretStoreSecretNameIndexKey, barbicanSecretStoreSecretNameExtractor)
}

// newBarbicanTestReconciler builds a BarbicanReconciler over a fake client
// pre-loaded with objs (indexes and status subresources wired via the shared
// barbicanFakeClientBuilder).
func newBarbicanTestReconciler(objs ...client.Object) *BarbicanReconciler {
	return &BarbicanReconciler{
		Client:   barbicanFakeClientBuilder(objs...).Build(),
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

// barbicanDBSecret returns the database credentials Secret referenced by
// testBarbican, carrying the username+password gate keys.
func barbicanDBSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "barbican-db", Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("barbican"), "password": []byte("db-pw")},
	}
}

// barbicanServiceUserSecret returns the service-user credentials Secret
// referenced by testBarbican, carrying the default "password" key with the given
// value.
func barbicanServiceUserSecret(password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "barbican-service-user", Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte(password)},
	}
}

// barbicanCondition returns one of the Barbican CR's conditions, or nil.
func barbicanCondition(barbican *barbicanv1alpha1.Barbican, conditionType string) *metav1.Condition {
	return conditions.GetCondition(barbican.Status.Conditions, conditionType)
}

// getBarbican re-reads the Barbican CR from the given client.
func getBarbican(t *testing.T, c client.Client) *barbicanv1alpha1.Barbican {
	t.Helper()
	var barbican barbicanv1alpha1.Barbican
	if err := c.Get(context.Background(), barbicanRequest.NamespacedName, &barbican); err != nil {
		t.Fatalf("re-reading Barbican %s: %v", barbicanRequest.NamespacedName, err)
	}
	return &barbican
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

// readyStore marks a store's CredentialsReady condition True, the D-gate the
// projection uses.
func readyStore(store *barbicanv1alpha1.BarbicanSecretStore) *barbicanv1alpha1.BarbicanSecretStore {
	conditions.SetCondition(&store.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCredentialsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: store.Generation,
		Reason:             conditionReasonCredentialsAvailable,
	})
	return store
}

// managedApproleSecret returns the AppRole credentials Secret the operator mints
// for a managed store, carrying both contract keys.
func managedApproleSecret(storeName, roleID, secretID string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: storeName + approleSecretNameSuffix, Namespace: testNamespace},
		Data: map[string][]byte{
			barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(roleID),
			barbicanv1alpha1.OpenBaoSecretIDKey: []byte(secretID),
		},
	}
}
