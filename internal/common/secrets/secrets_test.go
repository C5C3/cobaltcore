// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"errors"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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

// --- IsMissingSecretOrKey ---

// TestIsMissingSecretOrKey locks in the contract of the shared helper
// independently of GetSecretValue's wrapping details so call sites in
// reconcileDBConnectionSecret (operators/keystone) and any future consumers can
// rely on a stable classification of "upstream Secret absent or required key
// missing".
func TestIsMissingSecretOrKey(t *testing.T) {
	notFound := apierrors.NewNotFound(
		schema.GroupResource{Group: "", Resource: "secrets"},
		"missing-secret",
	)

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error is not missing",
			err:  nil,
			want: false,
		},
		{
			name: "wrapped apierrors NotFound is missing",
			err:  fmt.Errorf("getting Secret default/missing-secret: %w", notFound),
			want: true,
		},
		{
			name: "wrapped ErrKeyNotFound is missing",
			err:  fmt.Errorf("%w: key %q in Secret default/test", ErrKeyNotFound, "password"),
			want: true,
		},
		{
			name: "arbitrary error is not missing",
			err:  errors.New("boom"),
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsMissingSecretOrKey(tc.err)).To(Equal(tc.want))
		})
	}
}

// --- AdminPasswordDigest ---

// TestAdminPasswordDigest pins the cross-operator admin-password digest to
// known SHA-256 hex vectors. Both the keystone bootstrap re-run gate and the
// c5c3 application-credential annotation depend on this exact derivation; the
// known-answer vectors (computed independently of Go's crypto) guard against a
// silent change that would break the gate on one side only.
func TestAdminPasswordDigest(t *testing.T) {
	cases := []struct {
		name     string
		password string
		want     string
	}{
		{
			name:     "empty password",
			password: "",
			want:     "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:     "short password",
			password: "hunter2",
			want:     "f52fbd32b2b3b86ff88ef6c490628285f482af15ddcb29541f94bcf526a3f6c7",
		},
		{
			name:     "admin password with symbols",
			password: "S3cr3t-Admin-Pa55w0rd!",
			want:     "98993606415aecfb6a16cb37a183e39e51c5bb95f0cdcd155760db05c52bdaf4",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			got := AdminPasswordDigest(tc.password)
			g.Expect(got).To(Equal(tc.want))
			// Determinism: a second call yields the identical digest.
			g.Expect(AdminPasswordDigest(tc.password)).To(Equal(got))
		})
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

	exists, ready, err := WaitForExternalSecret(context.Background(), c, client.ObjectKeyFromObject(es))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exists).To(BeTrue())
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

	// Present but no Ready=True condition: exists is true, ready is false so
	// callers can distinguish this from the not-found case.
	exists, ready, err := WaitForExternalSecret(context.Background(), c, client.ObjectKeyFromObject(es))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exists).To(BeTrue())
	g.Expect(ready).To(BeFalse())
}

func TestWaitForExternalSecret_notFound(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	c := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	// NotFound is a normal "not ready" state, not an error. The tri-state
	// reports exists=false so callers can surface a clearer status than the
	// generic "not ready".
	exists, ready, err := WaitForExternalSecret(context.Background(), c, client.ObjectKey{Name: "missing", Namespace: "default"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exists).To(BeFalse())
	g.Expect(ready).To(BeFalse())
}

// --- IsClusterSecretStoreReady ---

func TestIsClusterSecretStoreReady_ready(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-cluster-store"},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store).
		WithStatusSubresource(store).
		Build()

	ready, err := IsClusterSecretStoreReady(context.Background(), c, "openbao-cluster-store")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestIsClusterSecretStoreReady_notReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-cluster-store"},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionFalse},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store).
		WithStatusSubresource(store).
		Build()

	ready, err := IsClusterSecretStoreReady(context.Background(), c, "openbao-cluster-store")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestIsClusterSecretStoreReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	c := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	// NotFound is treated as not-ready so the caller can reflect upstream
	// outages without special-casing a missing store.
	ready, err := IsClusterSecretStoreReady(context.Background(), c, "missing")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestIsSecretStoreReady_ready(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store).
		WithStatusSubresource(store).
		Build()

	ready, err := IsSecretStoreReady(context.Background(), c, "openbao-tenant-store", "tenant-a")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())
}

func TestIsSecretStoreReady_notReady(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionFalse},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(store).
		WithStatusSubresource(store).
		Build()

	ready, err := IsSecretStoreReady(context.Background(), c, "openbao-tenant-store", "tenant-a")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestIsSecretStoreReady_notFound(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()

	c := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	// A namespaced store missing in the requested namespace is not-ready, not an
	// error — mirroring the cluster-store behaviour.
	ready, err := IsSecretStoreReady(context.Background(), c, "missing", "tenant-a")
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
	// The missing-data-key case must wrap the ErrKeyNotFound sentinel so
	// callers can use errors.Is instead of fragile substring matching
	g.Expect(errors.Is(err, ErrKeyNotFound)).To(BeTrue(),
		"missing-key error must wrap ErrKeyNotFound")
	g.Expect(err.Error()).To(ContainSubstring(`key "password"`))
	g.Expect(err.Error()).To(ContainSubstring("Secret default/test-secret"))
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
