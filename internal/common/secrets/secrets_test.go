// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Feature: CC-0005

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = esov1alpha1.SchemeBuilder.AddToScheme(s)
	return s
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

// --- WaitForExternalSecret ---

func TestWaitForExternalSecret_ready(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-es",
			Namespace: "default",
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

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(es).
		WithStatusSubresource(es).
		Build()

	ready, err := WaitForExternalSecret(context.Background(), c, client.ObjectKeyFromObject(es))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestWaitForExternalSecret_notReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-es",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(es).
		Build()

	ready, err := WaitForExternalSecret(context.Background(), c, client.ObjectKeyFromObject(es))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestWaitForExternalSecret_notFound(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	c := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	// NotFound is a normal "not ready" state, not an error (CC-0005).
	ready, err := WaitForExternalSecret(context.Background(), c, client.ObjectKey{Name: "missing", Namespace: "default"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

// --- IsSecretReady ---

func TestIsSecretReady_exists(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	ready, err := IsSecretReady(context.Background(), c, client.ObjectKeyFromObject(secret))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestIsSecretReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	c := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	ready, err := IsSecretReady(context.Background(), c, client.ObjectKey{Name: "missing", Namespace: "default"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestIsSecretReady_allExpectedKeysPresent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("s3cret"),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	ready, err := IsSecretReady(context.Background(), c, client.ObjectKeyFromObject(secret), "username", "password")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestIsSecretReady_missingExpectedKey(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	ready, err := IsSecretReady(context.Background(), c, client.ObjectKeyFromObject(secret), "username", "password")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestIsSecretReady_noExpectedKeysChecksExistenceOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	// No expectedKeys — should return true as long as Secret exists.
	ready, err := IsSecretReady(context.Background(), c, client.ObjectKeyFromObject(secret))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

// --- GetSecretValue ---

func TestGetSecretValue_found(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"password": []byte("s3cret"),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	val, err := GetSecretValue(context.Background(), c, client.ObjectKeyFromObject(secret), "password")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal("s3cret"))
}

func TestGetSecretValue_secretNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	c := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	_, err := GetSecretValue(context.Background(), c, client.ObjectKey{Name: "missing", Namespace: "default"}, "password")
	g.Expect(err).To(HaveOccurred())
}

func TestGetSecretValue_keyNotPresent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	_, err := GetSecretValue(context.Background(), c, client.ObjectKeyFromObject(secret), "password")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("not found"))
}

// --- EnsurePushSecret ---

func TestEnsurePushSecret_creates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	ps := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ps",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	err := EnsurePushSecret(context.Background(), c, s, owner, ps)
	g.Expect(err).NotTo(HaveOccurred())

	created := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(ps), created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
}

func TestEnsurePushSecret_updates(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	existing := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ps",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, existing).
		Build()

	updated := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ps",
			Namespace: "default",
		},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyDelete,
		},
	}

	err := EnsurePushSecret(context.Background(), c, s, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(existing), fetched)).To(Succeed())
	g.Expect(fetched.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyDelete))
}

func TestEnsurePushSecret_idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	ps := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ps",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	ctx := context.Background()
	g.Expect(EnsurePushSecret(ctx, c, s, owner, ps)).To(Succeed())
	g.Expect(EnsurePushSecret(ctx, c, s, owner, ps)).To(Succeed())

	list := &esov1alpha1.PushSecretList{}
	g.Expect(c.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}
