// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esov1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Feature: CC-0005

func newFakeClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = esov1beta1.AddToScheme(s)
	_ = esov1alpha1.AddToScheme(s)
	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&esov1beta1.ExternalSecret{}, &esov1alpha1.PushSecret{}).
		Build()
}

func owner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       types.UID("owner-uid-1234"),
		},
	}
}

// --- WaitForExternalSecret ---

func TestWaitForExternalSecret_NotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	ready, err := WaitForExternalSecret(ctx, c, "default", "missing-es")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestWaitForExternalSecret_ExistsButNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	es := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-es",
			Namespace: "default",
		},
	}
	c := newFakeClient(es)

	// Set status condition to not ready.
	es.Status.Conditions = []esov1beta1.ExternalSecretStatusCondition{
		{
			Type:   esov1beta1.ExternalSecretReady,
			Status: corev1.ConditionFalse,
			Reason: "NotSynced",
		},
	}
	g.Expect(c.Status().Update(ctx, es)).To(Succeed())

	ready, err := WaitForExternalSecret(ctx, c, "default", "my-es")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestWaitForExternalSecret_ExistsAndReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	es := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-es",
			Namespace: "default",
		},
	}
	c := newFakeClient(es)

	// Set status condition to ready.
	es.Status.Conditions = []esov1beta1.ExternalSecretStatusCondition{
		{
			Type:   esov1beta1.ExternalSecretReady,
			Status: corev1.ConditionTrue,
			Reason: "SecretSynced",
		},
	}
	g.Expect(c.Status().Update(ctx, es)).To(Succeed())

	ready, err := WaitForExternalSecret(ctx, c, "default", "my-es")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestWaitForExternalSecret_NoConditions(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	es := &esov1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-es",
			Namespace: "default",
		},
	}
	c := newFakeClient(es)

	ready, err := WaitForExternalSecret(ctx, c, "default", "my-es")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

// --- IsSecretReady ---

func TestIsSecretReady_Exists(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
	}
	c := newFakeClient(secret)

	ready, err := IsSecretReady(ctx, c, "default", "my-secret")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestIsSecretReady_NotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	ready, err := IsSecretReady(ctx, c, "default", "missing-secret")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

// --- GetSecretValue ---

func TestGetSecretValue_ReturnsValue(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"password": []byte("s3cret"),
		},
	}
	c := newFakeClient(secret)

	val, err := GetSecretValue(ctx, c, "default", "my-secret", "password")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(val).To(Equal("s3cret"))
}

func TestGetSecretValue_SecretNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	_, err := GetSecretValue(ctx, c, "default", "missing-secret", "password")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("getting Secret"))
}

func TestGetSecretValue_KeyNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
		},
	}
	c := newFakeClient(secret)

	_, err := GetSecretValue(ctx, c, "default", "my-secret", "password")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("key \"password\" not found"))
}

// --- EnsurePushSecret ---

func TestEnsurePushSecret_Creates(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	desired := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ps",
			Namespace: "default",
		},
		Spec: esov1alpha1.PushSecretSpec{
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{
				{Name: "my-store"},
			},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: "source-secret"},
			},
		},
	}

	err := EnsurePushSecret(ctx, c, owner(), desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the PushSecret was created.
	var created esov1alpha1.PushSecret
	err = c.Get(ctx, types.NamespacedName{Name: "test-ps", Namespace: "default"}, &created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.SecretStoreRefs).To(HaveLen(1))
	g.Expect(created.Spec.SecretStoreRefs[0].Name).To(Equal("my-store"))
}

func TestEnsurePushSecret_SetsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	o := owner()
	desired := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned-ps",
			Namespace: "default",
		},
		Spec: esov1alpha1.PushSecretSpec{
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{
				{Name: "my-store"},
			},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: "source-secret"},
			},
		},
	}

	err := EnsurePushSecret(ctx, c, o, desired)
	g.Expect(err).NotTo(HaveOccurred())

	var created esov1alpha1.PushSecret
	err = c.Get(ctx, types.NamespacedName{Name: "owned-ps", Namespace: "default"}, &created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(types.UID("owner-uid-1234")))
}

func TestEnsurePushSecret_UpdatesExisting(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	existing := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "update-ps",
			Namespace: "default",
		},
		Spec: esov1alpha1.PushSecretSpec{
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{
				{Name: "old-store"},
			},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: "old-secret"},
			},
		},
	}
	c := newFakeClient(existing)

	desired := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "update-ps",
			Namespace: "default",
		},
		Spec: esov1alpha1.PushSecretSpec{
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{
				{Name: "new-store"},
			},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: "new-secret"},
			},
		},
	}

	err := EnsurePushSecret(ctx, c, owner(), desired)
	g.Expect(err).NotTo(HaveOccurred())

	var updated esov1alpha1.PushSecret
	err = c.Get(ctx, types.NamespacedName{Name: "update-ps", Namespace: "default"}, &updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updated.Spec.SecretStoreRefs[0].Name).To(Equal("new-store"))
	g.Expect(updated.Spec.Selector.Secret.Name).To(Equal("new-secret"))
}
