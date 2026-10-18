// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Horizon watch mappers. These plain handler.MapFunc
// closures are exercised directly against a pre-indexed fake client, mirroring
// placement_watches_test.go.
package controller

import (
	"context"
	"errors"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
)

// namedSecret returns a bare Secret object for a watch event. It carries no
// data: the mappers key off the name, namespace and owner references only.
func namedSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

// ownedSecret returns a Secret named some-owned-secret that no Horizon
// references by name and that carries an owner reference to h.
func ownedSecret(h *horizonv1alpha1.Horizon) *corev1.Secret {
	s := namedSecret("some-owned-secret")
	s.OwnerReferences = []metav1.OwnerReference{horizonOwnerRef(h)}
	return s
}

// mapperHorizon builds a Horizon in namespace from the shared testHorizon
// fixture, leaving spec.secretStoreRef unset so it defaults to the cluster
// store.
func mapperHorizon(name, namespace string) *horizonv1alpha1.Horizon {
	h := testHorizon()
	h.Name = name
	h.Namespace = namespace
	h.UID = types.UID(name + "-uid")
	return h
}

// failingListClient returns a fake client seeded with objs whose every List
// call fails with err, so the mapper's index-backed lookup cannot answer.
func failingListClient(err error, objs ...client.Object) client.Client {
	return horizonMapperClientBuilder(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return err
			},
		}).
		Build()
}

// --- secretToHorizonMapper ---

func TestSecretToHorizonMapper_ReferencedSecretEnqueuesTheCR(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient(testHorizon())

	// spec.secretKeyRef.name of test-horizon.
	g.Expect(secretToHorizonMapper(c)(context.Background(), namedSecret("horizon-secret-key"))).
		To(ConsistOf(horizonRequest))
}

func TestSecretToHorizonMapper_OwnedSecretEnqueuesTheOwner(t *testing.T) {
	g := NewGomegaWithT(t)

	h := testHorizon()
	c := newMapperFakeClient(h)

	g.Expect(secretToHorizonMapper(c)(context.Background(), ownedSecret(h))).
		To(ConsistOf(horizonRequest), "a Secret owned by a Horizon must enqueue its owner")
}

func TestSecretToHorizonMapper_UnreferencedSecretEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient(testHorizon())
	g.Expect(secretToHorizonMapper(c)(context.Background(), namedSecret("unrelated"))).To(BeEmpty())
}

func TestSecretToHorizonMapper_SameNameInOtherNamespaceEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient(testHorizon())
	foreign := namedSecret("horizon-secret-key")
	foreign.Namespace = "other"
	g.Expect(secretToHorizonMapper(c)(context.Background(), foreign)).
		To(BeEmpty(), "a Secret only wakes Horizons in its own namespace")
}

func TestSecretToHorizonMapper_OwnedSecretOfDeletedHorizonEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	// The owner reference names a Horizon the client does not hold, so the
	// mapper's cached Get answers NotFound and the owner is dropped.
	c := newMapperFakeClient()
	g.Expect(secretToHorizonMapper(c)(context.Background(), ownedSecret(testHorizon()))).
		To(BeEmpty(), "an owner reference to a deleted Horizon must not enqueue it")
}

func TestSecretToHorizonMapper_ListErrorStillEnqueuesOwner(t *testing.T) {
	g := NewGomegaWithT(t)

	h := testHorizon()
	c := failingListClient(errors.New("index unavailable"), h)

	g.Expect(secretToHorizonMapper(c)(context.Background(), ownedSecret(h))).
		To(ConsistOf(horizonRequest), "a failed index lookup must not stop the owner-reference leg")
}

// --- storeToHorizonMapper ---

func TestStoreToHorizonMapper_ClusterKindEnqueuesDefaultHorizons(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient(mapperHorizon("hz-a", "ns-a"), mapperHorizon("hz-b", "ns-b"))
	mapper := storeToHorizonMapper(c, commonv1.SecretStoreKindCluster)

	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(mapper(context.Background(), store)).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns-a", Name: "hz-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns-b", Name: "hz-b"}},
	), "a ClusterSecretStore change must enqueue every Horizon defaulting to it")

	// An unrelated cluster store wakes nothing.
	other := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: "some-other-store"}}
	g.Expect(mapper(context.Background(), other)).To(BeEmpty())
}

func TestStoreToHorizonMapper_NamespacedKindScopesToStoreNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	pinned := mapperHorizon("hz-pinned", "tenant-a")
	pinned.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	defaulted := mapperHorizon("hz-default", "tenant-a")
	foreign := mapperHorizon("hz-foreign", "tenant-b")
	foreign.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	c := newMapperFakeClient(pinned, defaulted, foreign)

	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
	}
	reqs := storeToHorizonMapper(c, commonv1.SecretStoreKindNamespaced)(context.Background(), store)

	g.Expect(reqs).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "hz-pinned"}},
	), "a namespaced SecretStore change must enqueue only the pinned Horizon in its own namespace")
}

func TestStoreToHorizonMapper_NoHorizonsEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient()

	clusterStore := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(storeToHorizonMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), clusterStore)).
		To(BeEmpty())

	namespacedStore := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "default"},
	}
	g.Expect(storeToHorizonMapper(c, commonv1.SecretStoreKindNamespaced)(context.Background(), namespacedStore)).
		To(BeEmpty())
}

func TestStoreToHorizonMapper_ListErrorEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := failingListClient(errors.New("cache unavailable"), mapperHorizon("hz-a", "ns-a"))

	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(storeToHorizonMapper(c, commonv1.SecretStoreKindCluster)(context.Background(), store)).
		To(BeEmpty(), "a List error logs and returns nil")
}
