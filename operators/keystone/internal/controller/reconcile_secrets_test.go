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
	corev1 "k8s.io/api/core/v1"
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
