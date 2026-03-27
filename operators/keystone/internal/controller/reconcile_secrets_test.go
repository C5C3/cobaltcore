// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(dbES, adminES, dbSecret, adminSecret).
		WithStatusSubresource(dbES, adminES).
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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(dbES, adminES).
		WithStatusSubresource(dbES, adminES).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(15 * time.Second))

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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(dbES, adminES, dbSecret).
		WithStatusSubresource(dbES, adminES).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(15 * time.Second))

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

	// Use an interceptor to inject an error on Get for ExternalSecrets.
	c := fake.NewClientBuilder().
		WithScheme(s).
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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(dbES, adminES).
		WithStatusSubresource(dbES, adminES).
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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(dbES, adminES, dbSecret).
		WithStatusSubresource(dbES, adminES).
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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(dbES, adminES, dbSecret).
		WithStatusSubresource(dbES, adminES).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(15 * time.Second))

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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(dbES, adminES, dbSecret, adminSecret).
		WithStatusSubresource(dbES, adminES).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(15 * time.Second))

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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(dbES, adminES).
		WithStatusSubresource(dbES, adminES).
		Build()

	r := &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result, err := r.reconcileSecrets(context.Background(), ks)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(15 * time.Second))

	// DB credentials are checked first, so the reason should be WaitingForDBCredentials.
	cond := meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDBCredentials"))
	g.Expect(cond.Message).To(Equal("Waiting for ESO to sync database credentials from OpenBao"))
}
