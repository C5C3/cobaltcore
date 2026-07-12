// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the ControlPlane SetupWithManager wiring: the secret-name field
// indexer extractor and the Secret -> ControlPlane watch mapper.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// newControlPlaneMapperClient returns a fake client pre-registered with the
// ControlPlaneSecretNameIndexKey field indexer so secretToControlPlaneMapper can
// resolve its MatchingFields lookups, mirroring keystone's
// newMapperFakeClientBuilder.
func newControlPlaneMapperClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(controllerTestScheme(t)).
		WithObjects(objs...).
		WithIndex(&c5c3v1alpha1.ControlPlane{}, ControlPlaneSecretNameIndexKey, controlPlaneSecretNameExtractor).
		Build()
}

// mapperControlPlane builds a minimal ControlPlane whose admin passwordSecretRef
// points at the named Secret.
func mapperControlPlane(name, namespace, secretName string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name + "-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: secretName, Key: "password"},
				},
			},
		},
	}
}

// --- controlPlaneSecretNameExtractor ---

func TestControlPlaneSecretNameExtractor_ReturnsPasswordSecretRefName(t *testing.T) {
	g := NewGomegaWithT(t)

	// mapperControlPlane sets no Database.ClusterRef, so this is the BROWNFIELD
	// case: the effective admin-password Secret is the user-supplied passwordSecretRef.
	cp := mapperControlPlane("cp", "default", "keystone-admin")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("keystone-admin"),
		"extractor must return the admin passwordSecretRef name")
}

func TestControlPlaneSecretNameExtractor_ManagedReturnsEffectiveName(t *testing.T) {
	g := NewGomegaWithT(t)

	// Managed mode (Database.ClusterRef != nil): the operator projects the admin
	// password into a per-ControlPlane Secret, so the indexed name must be the
	// operator-owned adminPasswordSecretName(cp), NOT the spec passwordSecretRef.
	cp := mapperControlPlane("cp", "default", "keystone-admin")
	cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{}
	cp.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf(adminPasswordSecretName(cp)),
		"in managed mode the extractor must index the operator-owned per-CP admin-password Secret name")
}

func TestControlPlaneSecretNameExtractor_EmptyWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(BeEmpty(),
		"extractor must return an empty slice when passwordSecretRef.name is unset")
}

// externalMapperControlPlane is the External-mode shape: the user-supplied admin
// password Secret plus an optional private-CA bundle Secret. Both must be indexed
// so a rotation of either wakes the owning ControlPlane.
func externalMapperControlPlane(name, namespace, secretName, caSecretName string) *c5c3v1alpha1.ControlPlane {
	cp := mapperControlPlane(name, namespace, secretName)
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Mode:     c5c3v1alpha1.KeystoneModeExternal,
		External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
	}
	if caSecretName != "" {
		cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{
			Name: caSecretName, Key: "ca.crt",
		}
	}
	return cp
}

func TestControlPlaneSecretNameExtractor_ExternalIncludesCABundleSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalMapperControlPlane("cp", "default", "external-admin", "keystone-ca")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("external-admin", "keystone-ca"),
		"External mode must index both the admin-password and the CA-bundle Secret")
}

// TestControlPlaneSecretNameExtractor_DeduplicatesSharedSecret covers the shape
// where one Secret carries both the admin password and the CA bundle: a duplicate
// index entry would enqueue the same ControlPlane twice per Secret event.
func TestControlPlaneSecretNameExtractor_DeduplicatesSharedSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalMapperControlPlane("cp", "default", "shared", "shared")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("shared"))
}

// TestControlPlaneSecretNameExtractor_ManagedIgnoresCABundleRef proves the mode
// discriminator gates the CA entry: a managed ControlPlane never dials a
// TLS-fronted endpoint, so a leftover external block indexes nothing extra.
func TestControlPlaneSecretNameExtractor_ManagedIgnoresCABundleRef(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalMapperControlPlane("cp", "default", "keystone-admin", "keystone-ca")
	cp.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeManaged
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("keystone-admin"))
}

// TestControlPlaneSecretNameExtractor_ExternalWithoutPasswordStillIndexesCA covers
// the empty-name edge: an unset passwordSecretRef must not leak an empty string
// into the index, but must not suppress the CA entry either.
func TestControlPlaneSecretNameExtractor_ExternalWithoutPasswordStillIndexesCA(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalMapperControlPlane("cp", "default", "", "keystone-ca")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("keystone-ca"))
}

func TestControlPlaneSecretNameExtractor_WrongTypeReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	// A non-ControlPlane object must not panic; a nil return is the contract.
	got := controlPlaneSecretNameExtractor(&corev1.Secret{})

	g.Expect(got).To(BeNil(),
		"extractor must return nil for a non-ControlPlane object")
}

// --- secretToControlPlaneMapper ---

func TestSecretToControlPlaneMapper_EnqueuesMatchingAdminSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "keystone-admin")
	c := newControlPlaneMapperClient(t, cp)
	mapper := secretToControlPlaneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(HaveLen(1),
		"a Secret matching the admin passwordSecretRef must enqueue its ControlPlane")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: "default", Name: "cp"}))
}

func TestSecretToControlPlaneMapper_IgnoresNonMatchingSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "keystone-admin")
	c := newControlPlaneMapperClient(t, cp)
	mapper := secretToControlPlaneMapper(c)

	// A Secret whose name does not match the admin passwordSecretRef must yield
	// no reconcile requests.
	other := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-secret", Namespace: "default"},
	}
	reqs := mapper(context.Background(), other)

	g.Expect(reqs).To(BeEmpty(),
		"a Secret not referenced by any ControlPlane must enqueue nothing")
}

func TestSecretToControlPlaneMapper_ScopedToNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	// Two ControlPlanes in different namespaces referencing the same Secret name.
	// Only the one in the event's namespace must be enqueued.
	cpA := mapperControlPlane("cp-a", "ns-a", "shared-secret")
	cpB := mapperControlPlane("cp-b", "ns-b", "shared-secret")
	c := newControlPlaneMapperClient(t, cpA, cpB)
	mapper := secretToControlPlaneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "ns-a"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(HaveLen(1),
		"only the ControlPlane in the Secret's namespace must be enqueued")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: "ns-a", Name: "cp-a"}))
}

// --- storeToControlPlaneMapper (#476, #605) ---

// TestStoreToControlPlaneMapper_ClusterKindEnqueuesExplicitControlPlanes verifies
// that a status change on the OpenBao-backed ClusterSecretStore enqueues every
// ControlPlane that EXPLICITLY selects it (across namespaces), but NOT a
// ControlPlane that omits spec.secretStoreRef — the default now resolves to the
// operator-provisioned per-tenant namespaced store, so a cluster-store change no
// longer concerns it.
func TestStoreToControlPlaneMapper_ClusterKindEnqueuesExplicitControlPlanes(t *testing.T) {
	g := NewGomegaWithT(t)

	cpA := mapperControlPlane("cp-a", "ns-a", "secret-a")
	cpA.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindCluster, Name: openBaoClusterStoreName,
	}
	cpB := mapperControlPlane("cp-b", "ns-b", "secret-b")
	cpB.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindCluster, Name: openBaoClusterStoreName,
	}
	// A nil-ref ControlPlane defaults to the per-tenant store, so a cluster-store
	// change must NOT enqueue it.
	cpDefault := mapperControlPlane("cp-default", "ns-c", "secret-c")
	c := newControlPlaneMapperClient(t, cpA, cpB, cpDefault)
	mapper := storeToControlPlaneMapper(c, commonv1.SecretStoreKindCluster)

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
	}
	reqs := mapper(context.Background(), store)

	names := make([]types.NamespacedName, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.NamespacedName)
	}
	g.Expect(names).To(ConsistOf(
		types.NamespacedName{Namespace: "ns-a", Name: "cp-a"},
		types.NamespacedName{Namespace: "ns-b", Name: "cp-b"},
	), "a ClusterSecretStore change must enqueue only ControlPlanes that explicitly select it, not the nil-ref default")
}

// TestStoreToControlPlaneMapper_ClusterKindIgnoresOtherStores verifies the
// mapper only reacts to the store a ControlPlane's effective ref resolves to,
// not to unrelated ClusterSecretStores.
func TestStoreToControlPlaneMapper_ClusterKindIgnoresOtherStores(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "secret")
	c := newControlPlaneMapperClient(t, cp)
	mapper := storeToControlPlaneMapper(c, commonv1.SecretStoreKindCluster)

	other := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "some-other-store"},
	}
	reqs := mapper(context.Background(), other)

	g.Expect(reqs).To(BeEmpty(),
		"a change to an unrelated ClusterSecretStore must enqueue nothing")
}

// TestStoreToControlPlaneMapper_NamespacedKindScopesToStoreNamespace verifies a
// namespaced SecretStore named openbao-tenant-store enqueues every ControlPlane
// in its OWN namespace that resolves to it — both a ControlPlane that pins it
// explicitly and a nil-ref ControlPlane that defaults to it — but NOT a same-name
// store in a foreign namespace.
func TestStoreToControlPlaneMapper_NamespacedKindScopesToStoreNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	pinned := mapperControlPlane("cp-pinned", "tenant-a", "secret-a")
	pinned.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	// A nil-ref ControlPlane in the same namespace defaults to the per-tenant
	// store named openbao-tenant-store, so it too must be enqueued.
	defaulted := mapperControlPlane("cp-default", "tenant-a", "secret-b")
	foreign := mapperControlPlane("cp-foreign", "tenant-b", "secret-c")
	foreign.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	c := newControlPlaneMapperClient(t, pinned, defaulted, foreign)
	mapper := storeToControlPlaneMapper(c, commonv1.SecretStoreKindNamespaced)

	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
	}
	reqs := mapper(context.Background(), store)

	names := make([]types.NamespacedName, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.NamespacedName)
	}
	g.Expect(names).To(ConsistOf(
		types.NamespacedName{Namespace: "tenant-a", Name: "cp-pinned"},
		types.NamespacedName{Namespace: "tenant-a", Name: "cp-default"},
	), "a namespaced SecretStore change must enqueue every ControlPlane in its own namespace that resolves to it")
}

// TestControlPlaneSecretNameExtractor_ExternalModeIndexesUserSecret asserts the
// field indexer follows effectiveAdminPasswordSecretRef into External mode: the
// indexed name is the USER-supplied Secret, so an edit to it wakes the
// ControlPlane and feeds the hash-driven application-credential re-mint. Indexing
// the operator-owned name instead would leave an out-of-band password rotation
// invisible until the next periodic resync.
func TestControlPlaneSecretNameExtractor_ExternalModeIndexesUserSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "external-admin")
	cp.Spec.Services.Keystone = &c5c3v1alpha1.ServiceKeystoneSpec{
		Mode:     c5c3v1alpha1.KeystoneModeExternal,
		External: &c5c3v1alpha1.ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
	}

	g.Expect(controlPlaneSecretNameExtractor(cp)).To(ConsistOf("external-admin"))

	// Even with a (webhook-impossible) managed database block, the mode
	// discriminator keeps the user-supplied Secret indexed.
	cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{}
	cp.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}
	g.Expect(controlPlaneSecretNameExtractor(cp)).To(ConsistOf("external-admin"))
}
