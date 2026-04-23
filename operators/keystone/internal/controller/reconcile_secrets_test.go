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

// backupPushSecret builds a PushSecret object seeded with ESO's cleanup
// finalizer in the default namespace. The finalizer mirrors production where
// ESO has already adopted the PushSecret before the operator's finalizer
// handler runs — every Pass-0 adoption check must therefore pass for these
// seeds (CC-0079, CC-0091, REQ-001, REQ-002, REQ-007).
func backupPushSecret(name string) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Finalizers: []string{esoPushSecretFinalizer},
		},
	}
}

// backupPushSecretUnadopted builds a PushSecret with NO finalizers — the
// pre-adoption state before ESO has picked it up. Pass-0 of
// finalizeOpenBaoSecrets must block on this state so Delete cannot outrun
// ESO's Spec.DeletionPolicy=Delete handler (CC-0091, REQ-001, REQ-007).
func backupPushSecretUnadopted(name string) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
}

// pushSecretWithPendingDelete returns a PushSecret that will transition to
// Terminating (DeletionTimestamp set, object kept in store) when the fake
// client processes a Delete, because the ESO cleanup finalizer seeded by
// backupPushSecret is non-controller and prevents actual removal. This
// mirrors ESO holding its cleanup finalizer while it purges the kv-v2 path
// (CC-0079, REQ-004).
func pushSecretWithPendingDelete(name string) *esov1alpha1.PushSecret {
	return backupPushSecret(name)
}

// TestFinalizeOpenBaoSecrets_DeletesBothPushSecrets verifies that when both
// backup PushSecrets are adopted (ESO cleanup finalizer present), the handler
// fires Delete on BOTH names in Pass 1 and then reports done=false with
// OpenBaoFinalizerBlocked while ESO drives the remote purge — the happy
// "both cleanly removed" path is no longer reachable in a single call now
// that Pass 0 and ESO's cleanup finalizer are in play (CC-0079, CC-0091,
// REQ-002).
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

	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse(),
		"handler must wait for ESO to finish purging before releasing the Keystone finalizer")

	// Pass 1 must have fired Delete on BOTH names — the ESO cleanup
	// finalizer then holds each object Terminating rather than removing it.
	for _, name := range []string{
		"test-keystone-fernet-keys-backup",
		"test-keystone-credential-keys-backup",
	} {
		fresh := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			fresh)).To(Succeed(),
			"PushSecret %s should still be present (Terminating) after Pass 1", name)
		g.Expect(fresh.GetDeletionTimestamp().IsZero()).To(BeFalse(),
			"PushSecret %s should carry a DeletionTimestamp after Pass 1", name)
	}

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("OpenBaoFinalizerBlocked"))
}

// TestFinalizeOpenBaoSecrets_NotFoundIsTolerated verifies that partial or
// fully-absent PushSecret state does not produce errors — NotFound on Get or
// Delete is tolerated for idempotency across operator restarts. When every
// backup PushSecret is already gone the handler returns done=true; when one
// is still present (adopted) it returns done=false while ESO drives the
// remote purge (CC-0079, CC-0091, REQ-003).
func TestFinalizeOpenBaoSecrets_NotFoundIsTolerated(t *testing.T) {
	testCases := []struct {
		name      string
		present   []client.Object
		wantDone  bool
		stuckName string
	}{
		{
			name:     "no PushSecrets",
			present:  nil,
			wantDone: true,
		},
		{
			name:      "only fernet present",
			present:   []client.Object{backupPushSecret("test-keystone-fernet-keys-backup")},
			wantDone:  false,
			stuckName: "test-keystone-fernet-keys-backup",
		},
		{
			name:      "only credential present",
			present:   []client.Object{backupPushSecret("test-keystone-credential-keys-backup")},
			wantDone:  false,
			stuckName: "test-keystone-credential-keys-backup",
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
			g.Expect(done).To(Equal(tc.wantDone))

			if tc.stuckName != "" {
				// Present (adopted) PushSecret must have had Delete fired
				// and still exist Terminating behind ESO's finalizer.
				fresh := &esov1alpha1.PushSecret{}
				g.Expect(c.Get(context.Background(),
					client.ObjectKey{Name: tc.stuckName, Namespace: "default"},
					fresh)).To(Succeed())
				g.Expect(fresh.GetDeletionTimestamp().IsZero()).To(BeFalse(),
					"PushSecret %s should carry a DeletionTimestamp after Pass 1", tc.stuckName)
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

// TestHasESOFinalizer covers the four finalizer-list shapes the openbao
// adoption check must classify correctly: empty, wrong value only, exact
// match, and exact match alongside unrelated finalizers (CC-0091, REQ-001,
// REQ-007).
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
			name:       "only unrelated finalizer",
			finalizers: []string{"external-secrets.io/cleanup"},
			want:       false,
		},
		{
			name:       "only ESO finalizer",
			finalizers: []string{esoPushSecretFinalizer},
			want:       true,
		},
		{
			name: "ESO finalizer alongside others",
			finalizers: []string{
				"external-secrets.io/cleanup",
				esoPushSecretFinalizer,
				"other.example.com/finalizer",
			},
			want: true,
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

// TestESOPushSecretFinalizerConstant pins the exact string value of the ESO
// cleanup finalizer so a rename typo cannot silently slip through — the
// adoption check compares against this value and a drift would re-open the
// CC-0091 race (CC-0091, REQ-007).
func TestESOPushSecretFinalizerConstant(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(esoPushSecretFinalizer).To(Equal(
		"pushsecret.externalsecrets.external-secrets.io/finalizer"))
}

// TestSetOpenBaoWaitingForESOAdoptionCondition verifies that the pre-Delete
// blocked-condition helper records SecretsReady=False with the
// WaitingForESOAdoption reason, names the unadopted PushSecret in the
// message, and mirrors the ObservedGeneration convention used by
// setOpenBaoFinalizerBlockedCondition — the two reasons must co-exist under
// one Condition Type so downstream consumers reading SecretsReady continue to
// work (CC-0091, REQ-002).
func TestSetOpenBaoWaitingForESOAdoptionCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := secretsTestKeystone()

	setOpenBaoWaitingForESOAdoptionCondition(ks, "test-keystone-credential-keys-backup")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForESOAdoption"))
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-credential-keys-backup"))
	g.Expect(cond.Message).To(ContainSubstring("cleanup finalizer not yet installed"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))
}

// TestFinalizeOpenBaoSecrets_BlocksOnMissingESOAdoption verifies that Pass 0
// holds Pass 1 at bay while any backup PushSecret is still unadopted. Delete
// must not fire on EITHER object because that is the race that would remove
// the PushSecret before ESO's DeletionPolicy=Delete runs and leak the KV-v2
// path in OpenBao (CC-0091, REQ-001).
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
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					deleteCount++
				}
				return cl.Delete(ctx, obj, opts...)
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
	g.Expect(done).To(BeFalse(),
		"handler must block until ESO adopts every backup PushSecret")
	g.Expect(deleteCount).To(Equal(0),
		"Delete must NOT fire while any PushSecret is unadopted — that is the race CC-0091 closes")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForESOAdoption"))
	// openBaoBackupPushSecretNames iterates fernet first, so the first
	// unadopted PushSecret named in the condition is fernet.
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-fernet-keys-backup"))
}

// TestFinalizeOpenBaoSecrets_ProceedsOnceAdopted verifies the full adoption
// → Terminating → gone progression. Once ESO has installed its cleanup
// finalizer Pass 0 passes, Pass 1 fires Delete, ESO holds the objects
// Terminating behind its finalizer until it finishes the remote purge, at
// which point a subsequent invocation observes them absent and reports
// done=true (CC-0091, REQ-001).
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

	// First call: Pass 0 passes (adopted), Pass 1 flips both Terminating,
	// Pass 2 sees them still present → blocked.
	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())

	for _, name := range []string{
		"test-keystone-fernet-keys-backup",
		"test-keystone-credential-keys-backup",
	} {
		fresh := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			fresh)).To(Succeed())
		g.Expect(fresh.GetDeletionTimestamp().IsZero()).To(BeFalse(),
			"PushSecret %s should be Terminating after Pass 1", name)
	}

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("OpenBaoFinalizerBlocked"))

	// Simulate ESO finishing the remote purge by clearing its finalizer on
	// each object — the fake client then removes the object fully.
	for _, name := range []string{
		"test-keystone-fernet-keys-backup",
		"test-keystone-credential-keys-backup",
	} {
		fresh := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			fresh)).To(Succeed())
		fresh.Finalizers = nil
		g.Expect(c.Update(context.Background(), fresh)).To(Succeed())
	}

	// Second call: Pass 0 skips (NotFound), Pass 1 no-op, Pass 2 all gone.
	done, err = r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeTrue())

	for _, name := range []string{
		"test-keystone-fernet-keys-backup",
		"test-keystone-credential-keys-backup",
	} {
		err := c.Get(context.Background(),
			client.ObjectKey{Name: name, Namespace: "default"},
			&esov1alpha1.PushSecret{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"PushSecret %s should be NotFound after ESO clears its finalizer", name)
	}
}

// TestFinalizeOpenBaoSecrets_MixedAdoptionState verifies that ANY unadopted
// backup PushSecret blocks Pass 1 for BOTH — Pass 0 protects the adopted
// fernet from being deleted while credential is still unadopted. Once ESO
// adopts the credential, the next call proceeds to Pass 1 on both (CC-0091,
// REQ-003).
func TestFinalizeOpenBaoSecrets_MixedAdoptionState(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := backupPushSecret("test-keystone-fernet-keys-backup")
	credential := backupPushSecretUnadopted("test-keystone-credential-keys-backup")

	var deletedNames []string
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet, credential).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					deletedNames = append(deletedNames, obj.GetName())
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	// First call: credential is unadopted → Pass 0 blocks → NO Delete on
	// either object, even though fernet IS adopted.
	done, err := r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(deletedNames).To(BeEmpty(),
		"Pass 1 must not fire Delete on any backup PushSecret while ANY is unadopted")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("WaitingForESOAdoption"))
	g.Expect(cond.Message).To(ContainSubstring("test-keystone-credential-keys-backup"),
		"condition must name the unadopted credential — not the already-adopted fernet")
	g.Expect(cond.Message).NotTo(ContainSubstring("fernet"),
		"fernet is adopted and must not be named as the blocker")

	// Now adopt the credential — the second call clears Pass 0 and fires
	// Delete on both names in Pass 1.
	fresh := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(),
		client.ObjectKey{Name: "test-keystone-credential-keys-backup", Namespace: "default"},
		fresh)).To(Succeed())
	fresh.Finalizers = []string{esoPushSecretFinalizer}
	g.Expect(c.Update(context.Background(), fresh)).To(Succeed())

	deletedNames = nil
	done, err = r.finalizeOpenBaoSecrets(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(done).To(BeFalse(),
		"Pass 2 still blocks on ESO holding the cleanup finalizer")
	g.Expect(deletedNames).To(ConsistOf(
		"test-keystone-fernet-keys-backup",
		"test-keystone-credential-keys-backup",
	), "Pass 1 must Delete BOTH backup PushSecrets once every one is adopted")
}

// TestFinalizeOpenBaoSecrets_TerminatingCountsAsAdopted verifies that a
// PushSecret already carrying a DeletionTimestamp counts as adopted: ESO
// must have picked it up before any actor flipped it Terminating. The
// handler must therefore emit OpenBaoFinalizerBlocked (Pass 2 wait), NOT
// WaitingForESOAdoption (Pass 0 block) — the two reasons have different
// remediations (CC-0091, REQ-001).
func TestFinalizeOpenBaoSecrets_TerminatingCountsAsAdopted(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := backupPushSecret("test-keystone-fernet-keys-backup")
	credential := backupPushSecret("test-keystone-credential-keys-backup")

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet, credential).
		Build()

	// Pre-flip both to Terminating via Delete — the ESO finalizer (seeded
	// by backupPushSecret) keeps them alive in the store.
	g.Expect(c.Delete(context.Background(), fernet)).To(Succeed())
	g.Expect(c.Delete(context.Background(), credential)).To(Succeed())

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
		"Terminating PushSecrets must not be flagged as unadopted — they are already in ESO's grasp")
}

// TestFinalizeOpenBaoSecrets_AbsentPushSecretIsStillIdempotent pins that an
// empty client still returns (true, nil) — operator restarts after a prior
// successful cleanup must never block CR removal (CC-0091, REQ-001).
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

// TestFinalizeOpenBaoSecrets_RBACShape asserts that the handler touches
// PushSecrets through only {Get, Delete} verbs — pinning the RBAC surface
// the operator's ClusterRole has to grant for PushSecrets. A new verb
// appearing here is a signal to update the ClusterRole manifest in lockstep
// (CC-0091, REQ-006, REQ-007).
func TestFinalizeOpenBaoSecrets_RBACShape(t *testing.T) {
	g := NewGomegaWithT(t)
	s := secretsTestScheme()
	ks := secretsTestKeystone()

	fernet := backupPushSecret("test-keystone-fernet-keys-backup")
	credential := backupPushSecret("test-keystone-credential-keys-backup")

	seenVerbs := map[string]bool{}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(fernet, credential).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					seenVerbs["get"] = true
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*esov1alpha1.PushSecretList); ok {
					seenVerbs["list"] = true
				}
				return cl.List(ctx, list, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					seenVerbs["create"] = true
				}
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					seenVerbs["update"] = true
				}
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					seenVerbs["patch"] = true
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					seenVerbs["delete"] = true
				}
				return cl.Delete(ctx, obj, opts...)
			},
			DeleteAllOf: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
				if _, ok := obj.(*esov1alpha1.PushSecret); ok {
					seenVerbs["deletecollection"] = true
				}
				return cl.DeleteAllOf(ctx, obj, opts...)
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

	g.Expect(seenVerbs).To(HaveKey("get"))
	g.Expect(seenVerbs).To(HaveKey("delete"))
	for _, forbidden := range []string{"list", "create", "update", "patch", "deletecollection"} {
		g.Expect(seenVerbs).NotTo(HaveKey(forbidden),
			"finalizeOpenBaoSecrets must not use verb %q on PushSecrets — RBAC drift risk", forbidden)
	}
}
