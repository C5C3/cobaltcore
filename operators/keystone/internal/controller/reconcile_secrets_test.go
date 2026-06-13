// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// secretsTestScheme returns a runtime.Scheme with ESO types registered
// alongside core and Keystone types for reconcileSecrets tests.
func secretsTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	// esov1alpha1 provides the PushSecret kind consumed by the openbao
	// finalizer tests (CC-0079, REQ-002).
	_ = esov1alpha1.SchemeBuilder.AddToScheme(s)
	return s
}

// secretsTestKeystone returns a minimal Keystone CR for reconcileSecrets tests.
func secretsTestKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

// readyExternalSecret returns an ExternalSecret with a Ready=True condition.
func readyExternalSecret(name, namespace string) *esov1.ExternalSecret {
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: esov1.ExternalSecretStatus{
			Conditions: []esov1.ExternalSecretStatusCondition{
				{
					Type:   esov1.ExternalSecretReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

// notReadyExternalSecret returns an ExternalSecret without a Ready condition.
func notReadyExternalSecret(name, namespace string) *esov1.ExternalSecret {
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

// readyClusterSecretStore returns a ClusterSecretStore with a Ready=True
// status condition so reconcileSecrets proceeds past the store gate (CC-0047).
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
// False with reason SecretStoreNotReady (CC-0047).
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

func TestReconcileSecrets_BothReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	dbES := readyExternalSecret("keystone-db", "default")
	adminES := readyExternalSecret("keystone-admin", "default")
	// Materialized K8s Secrets are required by IsSecretReady (CC-0013).
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("keystone"), "password": []byte("secret")},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("admin-password")},
	}

	store := readyClusterSecretStore("openbao-cluster-store")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES, dbSecret, adminSecret).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

func TestReconcileSecrets_DBCredentialsNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	dbES := notReadyExternalSecret("keystone-db", "default")
	adminES := readyExternalSecret("keystone-admin", "default")

	store := readyClusterSecretStore("openbao-cluster-store")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentials"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

func TestReconcileSecrets_AdminCredentialsNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	dbES := readyExternalSecret("keystone-db", "default")
	adminES := notReadyExternalSecret("keystone-admin", "default")
	// Materialized DB Secret is needed so IsSecretReady passes for DB credentials (CC-0013).
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("keystone"), "password": []byte("secret")},
	}

	store := readyClusterSecretStore("openbao-cluster-store")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES, dbSecret).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForAdminCredentials"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

func TestReconcileSecrets_ErrorFetchingExternalSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// A ready ClusterSecretStore lets reconcileSecrets past the store gate so
	// the interceptor exercises the ExternalSecret Get path (CC-0047).
	store := readyClusterSecretStore("openbao-cluster-store")
	// Use an interceptor to inject an error on Get for ExternalSecrets.
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store).
		WithStatusSubresource(store).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1.ExternalSecret); ok {
					return fmt.Errorf("simulated API server error")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("simulated API server error"))
}

func TestReconcileSecrets_DBNotReady_ConditionMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	dbES := notReadyExternalSecret("keystone-db", "default")
	adminES := readyExternalSecret("keystone-admin", "default")

	store := readyClusterSecretStore("openbao-cluster-store")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Message).To(Equal("Waiting for ESO to sync database credentials from OpenBao"))
}

func TestReconcileSecrets_AdminNotReady_ConditionMessage(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	dbES := readyExternalSecret("keystone-db", "default")
	adminES := notReadyExternalSecret("keystone-admin", "default")
	// Materialized DB Secret is needed so IsSecretReady passes for DB credentials (CC-0013).
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("keystone"), "password": []byte("secret")},
	}

	store := readyClusterSecretStore("openbao-cluster-store")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES, dbSecret).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Message).To(Equal("Waiting for ESO to sync admin credentials from OpenBao"))
}

func TestReconcileSecrets_DBSecretMissingKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// ExternalSecret is ready but materialized Secret is missing expected keys (CC-0013).
	dbES := readyExternalSecret("keystone-db", "default")
	adminES := readyExternalSecret("keystone-admin", "default")
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
		Data:       map[string][]byte{"wrong-key": []byte("val")},
	}

	store := readyClusterSecretStore("openbao-cluster-store")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES, dbSecret).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentials"))
	g.Expect(cond.Message).To(Equal("Database credentials Secret exists but is missing expected keys"))
}

func TestReconcileSecrets_AdminSecretMissingKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	dbES := readyExternalSecret("keystone-db", "default")
	adminES := readyExternalSecret("keystone-admin", "default")
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("keystone"), "password": []byte("secret")},
	}
	// Admin Secret exists but is missing the "password" key (CC-0013).
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
		Data:       map[string][]byte{"wrong-key": []byte("val")},
	}

	store := readyClusterSecretStore("openbao-cluster-store")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES, dbSecret, adminSecret).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForAdminCredentials"))
	g.Expect(cond.Message).To(Equal("Admin credentials Secret exists but is missing expected keys"))
}

func TestReconcileSecrets_BothNotReady_DBCheckFirst(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// Both ExternalSecrets are not ready.
	dbES := notReadyExternalSecret("keystone-db", "default")
	adminES := notReadyExternalSecret("keystone-admin", "default")

	store := readyClusterSecretStore("openbao-cluster-store")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	// DB credentials are checked first, so the reason should be WaitingForDBCredentials.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentials"))
	g.Expect(cond.Message).To(Equal("Waiting for ESO to sync database credentials from OpenBao"))
}

func TestReconcileSecrets_StoreNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// ExternalSecrets look healthy — the store condition should still drive
	// SecretsReady to False so chaos in OpenBao surfaces within the ESO store
	// reconcile interval rather than the per-ES refreshInterval (CC-0047).
	dbES := readyExternalSecret("keystone-db", "default")
	adminES := readyExternalSecret("keystone-admin", "default")
	store := notReadyClusterSecretStore("openbao-cluster-store")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(ContainSubstring("openbao-cluster-store"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

func TestReconcileSecrets_StoreMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// No ClusterSecretStore object exists. IsClusterSecretStoreReady treats
	// NotFound as not-ready so the operator still reports the upstream
	// backend as unreachable (CC-0047).
	c := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
}

func TestReconcileSecrets_StoreGetErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// Transient API-server errors on the ClusterSecretStore Get must be
	// returned to the caller so controller-runtime requeues the reconcile —
	// silently setting SecretsReady=False on a flaky API would mask real
	// outages from everything downstream (CC-0047).
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1.ClusterSecretStore); ok {
					return fmt.Errorf("simulated API server error")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("simulated API server error"))
}

func TestReconcileSecrets_StoreCheckedBeforeExternalSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// Store not ready AND DB ExternalSecret not ready. The reason must be
	// SecretStoreNotReady — store outage is the root cause and must win the
	// ordering so operators do not chase the wrong symptom (CC-0047).
	store := notReadyClusterSecretStore("openbao-cluster-store")
	dbES := notReadyExternalSecret("keystone-db", "default")
	adminES := notReadyExternalSecret("keystone-admin", "default")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES).
		WithStatusSubresource(store, dbES, adminES).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
}

// TestReconcileSecrets_ConditionObservedGeneration verifies that
// ObservedGeneration is set on the SecretsReady condition for
// False (SecretStoreNotReady, WaitingForDBCredentials,
// WaitingForAdminCredentials) and True (SecretsAvailable) paths
// with distinct generation values (CC-0072, REQ-002, REQ-003).
func TestReconcileSecrets_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()

	// Test ObservedGeneration for the SecretStoreNotReady path.
	ks := secretsTestKeystone()
	ks.Generation = 7

	store := notReadyClusterSecretStore("openbao-cluster-store")
	dbES := readyExternalSecret("keystone-db", "default")
	adminES := readyExternalSecret("keystone-admin", "default")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store, dbES, adminES).
		WithStatusSubresource(dbES, adminES, store).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.reconcileSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))

	// Test ObservedGeneration for the WaitingForDBCredentials path (ES not synced).
	ks3 := secretsTestKeystone()
	ks3.Generation = 5

	c3 := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			readyClusterSecretStore("openbao-cluster-store"),
			notReadyExternalSecret("keystone-db", "default"),
			readyExternalSecret("keystone-admin", "default"),
		).
		WithStatusSubresource(
			&esov1.ExternalSecret{},
			&esov1.ClusterSecretStore{},
		).
		Build()

	r3 := &KeystoneReconciler{
		Client:   c3,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err = r3.reconcileSecrets(context.Background(), ks3)
	g.Expect(err).NotTo(HaveOccurred())

	cond3 := meta.FindStatusCondition(ks3.Status.Conditions, "SecretsReady")
	g.Expect(cond3).NotTo(BeNil())
	g.Expect(cond3.ObservedGeneration).To(Equal(int64(5)))

	// Test ObservedGeneration for the WaitingForAdminCredentials path (ES not synced).
	ks4 := secretsTestKeystone()
	ks4.Generation = 9

	c4 := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			readyClusterSecretStore("openbao-cluster-store"),
			readyExternalSecret("keystone-db", "default"),
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
				Data:       map[string][]byte{"username": []byte("keystone"), "password": []byte("secret")},
			},
			notReadyExternalSecret("keystone-admin", "default"),
		).
		WithStatusSubresource(
			&esov1.ExternalSecret{},
			&esov1.ClusterSecretStore{},
		).
		Build()

	r4 := &KeystoneReconciler{
		Client:   c4,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err = r4.reconcileSecrets(context.Background(), ks4)
	g.Expect(err).NotTo(HaveOccurred())

	cond4 := meta.FindStatusCondition(ks4.Status.Conditions, "SecretsReady")
	g.Expect(cond4).NotTo(BeNil())
	g.Expect(cond4.ObservedGeneration).To(Equal(int64(9)))

	// Test ObservedGeneration for the SecretsAvailable path.
	ks2 := secretsTestKeystone()
	ks2.Generation = 12

	dbES2 := readyExternalSecret("keystone-db", "default")
	adminES2 := readyExternalSecret("keystone-admin", "default")
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("keystone"), "password": []byte("secret")},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("admin-password")},
	}
	store2 := readyClusterSecretStore("openbao-cluster-store")

	c2 := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store2, dbES2, adminES2, dbSecret, adminSecret).
		WithStatusSubresource(dbES2, adminES2, store2).
		Build()

	r2 := &KeystoneReconciler{
		Client:   c2,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err = r2.reconcileSecrets(context.Background(), ks2)
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, "SecretsReady")
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(12)))
}

// TestFernetKeysPushSecret_HasDeletionPolicyDelete verifies that the PushSecret
// builder for the Fernet backup sets Spec.DeletionPolicy=Delete so that
// deleting the PushSecret triggers ESO to purge the kv-v2 path in OpenBao —
// the cleanup path that the openbao finalizer depends on (CC-0079, REQ-008).
func TestFernetKeysPushSecret_HasDeletionPolicyDelete(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone", Namespace: "default"},
	}

	ps := fernetKeysPushSecret(ks)

	g.Expect(ps.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyDelete),
		"fernet-keys-backup PushSecret must use DeletionPolicy=Delete so ESO purges the remote kv-v2 path on deletion")
}

// TestCredentialKeysPushSecret_HasDeletionPolicyDelete verifies that the
// PushSecret builder for the credential backup sets Spec.DeletionPolicy=Delete
// so that deleting the PushSecret triggers ESO to purge the kv-v2 path in
// OpenBao — the cleanup path that the openbao finalizer depends on (CC-0079,
// REQ-008).
func TestCredentialKeysPushSecret_HasDeletionPolicyDelete(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone", Namespace: "default"},
	}

	ps := credentialKeysPushSecret(ks)

	g.Expect(ps.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyDelete),
		"credential-keys-backup PushSecret must use DeletionPolicy=Delete so ESO purges the remote kv-v2 path on deletion")
}

// backupPushSecret builds a minimal PushSecret already adopted by ESO (carries
// the cleanup finalizer) so existing finalize-handler tests exercise the
// post-adoption Pass-1/Pass-2 paths. Tests covering the Pass-0 adoption wait
// should use backupPushSecretUnadopted instead (CC-0079, CC-0091, REQ-001).
func backupPushSecret(name string) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Finalizers: []string{esoPushSecretFinalizer},
		},
	}
}

// backupPushSecretUnadopted builds a minimal PushSecret with NO finalizers,
// modelling the brief window before ESO has installed its cleanup finalizer
// on a freshly-created PushSecret. Used by the Pass-0 adoption-wait tests
// (CC-0091, REQ-001, REQ-007).
func backupPushSecretUnadopted(name string) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
}

// pushSecretWithPendingDelete returns a PushSecret that will transition to
// Terminating (DeletionTimestamp set, object kept in store) when the fake
// client processes a Delete, because the non-controller finalizer prevents
// actual removal. This mirrors ESO holding its cleanup finalizer while it
// purges the kv-v2 path (CC-0079, REQ-004).
func pushSecretWithPendingDelete(name string) *esov1alpha1.PushSecret {
	ps := backupPushSecret(name)
	// backupPushSecret already seeds esoPushSecretFinalizer; append the ESO
	// cleanup finalizer so the fake client holds the object in Terminating
	// when the handler's Pass-1 Delete fires (CC-0079, CC-0091, REQ-004).
	ps.Finalizers = append(ps.Finalizers, "external-secrets.io/cleanup")
	return ps
}

// TestFinalizeOpenBaoSecrets_DeletesBothPushSecrets verifies that the handler
// deletes both the fernet-keys-backup and credential-keys-backup PushSecrets
// and reports done=true once both have been through ESO's cleanup path
// (CC-0079, CC-0091, REQ-001, REQ-002). After CC-0091, adopted PushSecrets
// carry esoPushSecretFinalizer, so the fake client holds them Terminating on
// Delete until ESO clears the finalizer — this test models that hand-off.
func TestFinalizeOpenBaoSecrets_DeletesBothPushSecrets(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := backupPushSecret("test-keystone-fernet-keys-backup")
	credential := backupPushSecret("test-keystone-credential-keys-backup")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet, credential).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	names := []string{
		"test-keystone-fernet-keys-backup",
		"test-keystone-credential-keys-backup",
	}

	// First pass: Pass-0 accepts (finalizer present), Pass-1 Deletes, Pass-2
	// sees Terminating and reports done=false.
	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())

	// Simulate ESO completing its cleanup by clearing the finalizer on both
	// PushSecrets.
	for _, name := range names {
		fresh := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			fresh)).To(Succeed())
		fresh.Finalizers = nil
		g.Expect(c.Update(context.Background(), fresh)).To(Succeed())
	}

	// Second pass: everything should now be gone.
	done, err = r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	for _, name := range names {
		err := c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			&esov1alpha1.PushSecret{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"PushSecret %s should be NotFound after ESO clears its finalizer", name)
	}
}

// TestFinalizeOpenBaoSecrets_NotFoundIsTolerated verifies that partial or
// fully-absent PushSecret state eventually produces done=true with no error —
// idempotency for operator restarts and prior cleanups (CC-0079, CC-0091,
// REQ-001, REQ-003). When a PushSecret is present it carries the ESO cleanup
// finalizer (adopted), so the handler's first call returns done=false
// (Terminating behind finalizer); once the finalizer is cleared a second call
// returns done=true.
func TestFinalizeOpenBaoSecrets_NotFoundIsTolerated(t *testing.T) {
	testCases := []struct {
		name    string
		present []client.Object
	}{
		{
			name:    "no PushSecrets",
			present: nil,
		},
		{
			name:    "only fernet present",
			present: []client.Object{backupPushSecret("test-keystone-fernet-keys-backup")},
		},
		{
			name:    "only credential present",
			present: []client.Object{backupPushSecret("test-keystone-credential-keys-backup")},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := secretsTestScheme()
			ks := secretsTestKeystone()

			c := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tc.present...).
				Build()

			r := &KeystoneReconciler{
				Client:   c,
				Scheme:   s,
				Recorder: record.NewFakeRecorder(10),
			}

			done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
			g.Expect(err).NotTo(HaveOccurred())

			if len(tc.present) == 0 {
				// Empty client — nothing to wait on, done immediately.
				g.Expect(done).To(BeTrue())
				return
			}

			// Adopted PushSecret(s) held in Terminating by the ESO finalizer.
			g.Expect(done).To(BeFalse())

			// Simulate ESO finishing its DeleteSecret work by clearing the
			// finalizer on every still-present object.
			for _, obj := range tc.present {
				fresh := &esov1alpha1.PushSecret{}
				g.Expect(c.Get(context.Background(),
					client.ObjectKeyFromObject(obj),
					fresh)).To(Succeed())
				fresh.Finalizers = nil
				g.Expect(c.Update(context.Background(), fresh)).To(Succeed())
			}

			done, err = r.finalizeOpenBaoSecrets(context.Background(), ks)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(done).To(BeTrue())

			for _, obj := range tc.present {
				err := c.Get(context.Background(),
					client.ObjectKeyFromObject(obj),
					&esov1alpha1.PushSecret{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
					"PushSecret %s should be NotFound after ESO finalizer cleared", obj.GetName())
			}
		})
	}
}

// TestFinalizeOpenBaoSecrets_IsIdempotent verifies that a second invocation
// against a clean client produces the same outcome so operator restarts or
// retries never block CR removal (CC-0079, REQ-003).
func TestFinalizeOpenBaoSecrets_IsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	c := fake.NewClientBuilder().WithScheme(s).Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	done, err = r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())
}

// TestFinalizeOpenBaoSecrets_RequeuesWhilePushSecretTerminating verifies that
// a PushSecret held in Terminating state by ESO's cleanup finalizer keeps the
// handler returning done=false so the Keystone CR finalizer is not released
// before ESO has purged the kv-v2 path. The blocked-condition message (asserted
// separately by TestFinalizeOpenBaoSecrets_SetsBlockedConditionOnStall) names
// the stuck PushSecret (CC-0079, REQ-004).
func TestFinalizeOpenBaoSecrets_RequeuesWhilePushSecretTerminating(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := pushSecretWithPendingDelete("test-keystone-fernet-keys-backup")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())

	// The held PushSecret must now carry a DeletionTimestamp — the fake
	// client flips it because the cleanup finalizer blocks actual removal.
	fresh := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(),
		client.ObjectKey{Name: "test-keystone-fernet-keys-backup", Namespace: "default"},
		fresh)).To(Succeed())
	g.Expect(fresh.GetDeletionTimestamp().IsZero()).To(BeFalse(),
		"held PushSecret must be marked for deletion before the finalizer returns")
}

// TestFinalizeOpenBaoSecrets_SetsBlockedConditionOnStall verifies that when a
// PushSecret is stuck Terminating, the handler records
// SecretsReady=False with reason OpenBaoFinalizerBlocked so operators can see
// why the Keystone CR has not finished deleting (CC-0079, REQ-004).
func TestFinalizeOpenBaoSecrets_SetsBlockedConditionOnStall(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := pushSecretWithPendingDelete("test-keystone-fernet-keys-backup")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("OpenBaoFinalizerBlocked"))
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-fernet-keys-backup"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestHasESOFinalizer covers each shape the finalizer list can take on a
// backup PushSecret so the adoption check in finalizeOpenBaoSecrets recognises
// ESO adoption regardless of ordering or co-resident finalizers (CC-0091,
// REQ-001, REQ-007).
func TestHasESOFinalizer(t *testing.T) {
	testCases := []struct {
		name       string
		finalizers []string
		want       bool
	}{
		{
			name:       "empty finalizers",
			finalizers: nil,
			want:       false,
		},
		{
			name:       "only an unrelated finalizer",
			finalizers: []string{"example.com/unrelated"},
			want:       false,
		},
		{
			name:       "only the ESO finalizer",
			finalizers: []string{esoPushSecretFinalizer},
			want:       true,
		},
		{
			name:       "ESO finalizer alongside others",
			finalizers: []string{"example.com/unrelated", esoPushSecretFinalizer, "other.example/finalizer"},
			want:       true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ps := &esov1alpha1.PushSecret{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-keystone-fernet-keys-backup",
					Namespace:  "default",
					Finalizers: tc.finalizers,
				},
			}
			g.Expect(hasESOFinalizer(ps)).To(Equal(tc.want))
		})
	}
}

// TestSetOpenBaoWaitingForESOAdoptionCondition verifies the helper records
// SecretsReady=False with the WaitingForESOAdoption reason, names the
// unadopted PushSecret, and pins ObservedGeneration — the signal contract
// SREs rely on to distinguish pre-Delete from post-Delete blocked states
// (CC-0091, REQ-002).
func TestSetOpenBaoWaitingForESOAdoptionCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := secretsTestKeystone()

	setOpenBaoWaitingForESOAdoptionCondition(ks, "test-keystone-credential-keys-backup")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForESOAdoption"))
	g.Expect(cond.Message).To(Equal(
		`Waiting for ESO to adopt PushSecret "test-keystone-credential-keys-backup" (cleanup finalizer not yet installed)`,
	))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestFinalizeOpenBaoSecrets_BlocksOnMissingESOAdoption pins the Pass-0
// behaviour: when a backup PushSecret exists but has not yet been adopted by
// ESO (no cleanup finalizer installed), the handler must report
// WaitingForESOAdoption and return WITHOUT firing a Delete. A premature
// Delete would remove the PushSecret object outright — ESO would never see a
// DeletionTimestamp, never run its DeletionPolicy=Delete branch, and the
// referenced kv-v2 path would be orphaned in OpenBao (CC-0091, REQ-001,
// REQ-003, REQ-007).
func TestFinalizeOpenBaoSecrets_BlocksOnMissingESOAdoption(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := backupPushSecretUnadopted("test-keystone-fernet-keys-backup")
	credential := backupPushSecretUnadopted("test-keystone-credential-keys-backup")

	var deleteCount int
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet, credential).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCount++
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(deleteCount).To(Equal(0),
		"Pass-0 must return before firing any Delete when adoption has not completed")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForESOAdoption"))
	// openBaoBackupPushSecretNames returns fernet first, so the first
	// unadopted name encountered is fernet.
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-fernet-keys-backup"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestFinalizeOpenBaoSecrets_ProceedsOnceAdopted verifies that once ESO has
// adopted both backup PushSecrets (cleanup finalizer installed), the handler
// advances past Pass-0 to Pass-1 (Delete) and Pass-2 (wait-on-gone). On the
// first call the PushSecrets transition to Terminating held by the ESO
// finalizer; after the test simulates ESO finishing its remote cleanup by
// clearing the finalizer, a second call returns done=true (CC-0091, REQ-001,
// REQ-003).
func TestFinalizeOpenBaoSecrets_ProceedsOnceAdopted(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := backupPushSecret("test-keystone-fernet-keys-backup")
	credential := backupPushSecret("test-keystone-credential-keys-backup")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet, credential).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	names := []string{
		"test-keystone-fernet-keys-backup",
		"test-keystone-credential-keys-backup",
	}

	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())

	// Both PushSecrets should now be Terminating — the ESO finalizer holds
	// them in-store after Pass-1's Delete.
	for _, name := range names {
		fresh := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			fresh)).To(Succeed())
		g.Expect(fresh.GetDeletionTimestamp().IsZero()).To(BeFalse(),
			"PushSecret %s must carry DeletionTimestamp after Pass-1 Delete", name)
	}

	// The reported reason at this point should be OpenBaoFinalizerBlocked
	// (Pass-2), NOT WaitingForESOAdoption (Pass-0) — adoption already
	// completed so the Pass-0 gate let us through.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("OpenBaoFinalizerBlocked"))

	// Simulate ESO finishing: clear the cleanup finalizer on both.
	for _, name := range names {
		fresh := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			fresh)).To(Succeed())
		fresh.Finalizers = nil
		g.Expect(c.Update(context.Background(), fresh)).To(Succeed())
	}

	done, err = r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	for _, name := range names {
		err := c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			&esov1alpha1.PushSecret{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"PushSecret %s should be NotFound after ESO clears its finalizer", name)
	}
}

// TestFinalizeOpenBaoSecrets_MixedAdoptionState verifies that Pass-0 is a
// per-PushSecret gate: if ANY backup PushSecret lacks the ESO cleanup
// finalizer the handler aborts BEFORE any Delete fires, even when sibling
// PushSecrets are fully adopted. Once the laggard PushSecret picks up the
// finalizer a subsequent call proceeds to Pass-1 for all of them (CC-0091,
// REQ-001, REQ-003, REQ-007).
func TestFinalizeOpenBaoSecrets_MixedAdoptionState(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// fernet is adopted, credential is not. openBaoBackupPushSecretNames
	// iterates fernet first, so the handler clears fernet's Pass-0 check
	// then trips on credential — confirming the gate is per-name.
	fernet := backupPushSecret("test-keystone-fernet-keys-backup")
	credential := backupPushSecretUnadopted("test-keystone-credential-keys-backup")

	var deleteCount int
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet, credential).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCount++
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	// Phase 1: mixed adoption state.
	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(deleteCount).To(Equal(0),
		"Pass-0 must return before firing Delete on ANY PushSecret when at least one is unadopted")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForESOAdoption"))
	// The unadopted name is credential; fernet is adopted and must not be
	// named in the blocked message.
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-credential-keys-backup"))

	// Phase 2: ESO adopts credential by installing its cleanup finalizer.
	fresh := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(),
		client.ObjectKey{Name: "test-keystone-credential-keys-backup", Namespace: "default"},
		fresh)).To(Succeed())
	fresh.Finalizers = []string{esoPushSecretFinalizer}
	g.Expect(c.Update(context.Background(), fresh)).To(Succeed())

	done, err = r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	// Both objects still held by ESO finalizer, so done=false — but the fact
	// that Deletes fired at all proves Pass-1 ran for both.
	g.Expect(done).To(BeFalse())
	g.Expect(deleteCount).To(Equal(2),
		"Pass-1 must fire Delete exactly once on every adopted PushSecret once the Pass-0 gate opens")
}

// TestFinalizeOpenBaoSecrets_TerminatingCountsAsAdopted verifies that a
// PushSecret already in Terminating state is skipped by the Pass-0 adoption
// gate — the DeletionTimestamp!=0 branch lets Pass-2 wait on the object
// instead. A Terminating PushSecret has necessarily been through a prior
// Delete, so the adoption question was already resolved; re-requiring the
// finalizer at this point would trap the CR forever if the user strips the
// finalizer to unblock a stuck cleanup (CC-0091, REQ-001).
func TestFinalizeOpenBaoSecrets_TerminatingCountsAsAdopted(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	// Seed both PushSecrets with the ESO cleanup finalizer so the fake
	// client retains them after Delete, then call Delete BEFORE running the
	// handler — the fake client will set DeletionTimestamp because a
	// finalizer is present, simulating "already Terminating".
	fernet := backupPushSecret("test-keystone-fernet-keys-backup")
	credential := backupPushSecret("test-keystone-credential-keys-backup")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet, credential).
		Build()

	for _, name := range []string{
		"test-keystone-fernet-keys-backup",
		"test-keystone-credential-keys-backup",
	} {
		ps := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			ps)).To(Succeed())
		g.Expect(c.Delete(context.Background(), ps)).To(Succeed())
		// Confirm seeding produced a Terminating object.
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			ps)).To(Succeed())
		g.Expect(ps.GetDeletionTimestamp().IsZero()).To(BeFalse())
	}

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("OpenBaoFinalizerBlocked"),
		"Terminating PushSecrets must bypass Pass-0 so Pass-2 reports blocked, not WaitingForESOAdoption")
}

// TestFinalizeOpenBaoSecrets_AbsentPushSecretIsStillIdempotent is a
// regression pin: when no backup PushSecret exists (operator restart, prior
// cleanup, or the CR never reached the point of creating them) the handler
// must return done=true with no error. Separate from
// TestFinalizeOpenBaoSecrets_IsIdempotent so a future refactor cannot
// silently remove the empty-client fast path (CC-0091, REQ-003).
func TestFinalizeOpenBaoSecrets_AbsentPushSecretIsStillIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	c := fake.NewClientBuilder().WithScheme(s).Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())
}

// TestFinalizeOpenBaoSecrets_RBACShape verifies that the finalize handler
// only issues get and delete verbs against PushSecret resources. If a future
// refactor introduces create/update/patch/list/watch calls, the operator's
// RBAC must grow to match — this test pins the minimal verb set so
// unexpected broadening is caught in CI rather than at runtime with
// permission denied errors (CC-0091, REQ-006).
func TestFinalizeOpenBaoSecrets_RBACShape(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := backupPushSecret("test-keystone-fernet-keys-backup")

	verbs := map[string]bool{}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					verbs["get"] = true
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					verbs["delete"] = true
				}
				return c.Delete(ctx, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					verbs["create"] = true
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					verbs["update"] = true
				}
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					verbs["patch"] = true
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*esov1alpha1.PushSecretList); ok {
					verbs["list"] = true
				}
				return c.List(ctx, list, opts...)
			},
			Watch: func(ctx context.Context, c client.WithWatch, obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
				if _, ok := obj.(*esov1alpha1.PushSecretList); ok {
					verbs["watch"] = true
				}
				return c.Watch(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(verbs["get"]).To(BeTrue(), "handler must call Get on PushSecret")
	g.Expect(verbs["delete"]).To(BeTrue(), "handler must call Delete on PushSecret")
	g.Expect(verbs["create"]).To(BeFalse(), "handler must NOT Create PushSecrets")
	g.Expect(verbs["update"]).To(BeFalse(), "handler must NOT Update PushSecrets")
	g.Expect(verbs["patch"]).To(BeFalse(), "handler must NOT Patch PushSecrets")
	g.Expect(verbs["list"]).To(BeFalse(), "handler must NOT List PushSecrets")
	g.Expect(verbs["watch"]).To(BeFalse(), "handler must NOT Watch PushSecrets")
}
