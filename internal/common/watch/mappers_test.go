// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// The mapper tests use corev1.ConfigMap as the stand-in CR type: the mapper
// is generic over the list/object factories, so any registered type works.
const testIndexKey = "spec.secretRefs.name"

func testMapperConfig(withOwnerLeg bool) SecretMapperConfig {
	cfg := SecretMapperConfig{
		IndexKey: testIndexKey,
		NewList:  func() client.ObjectList { return &corev1.ConfigMapList{} },
	}
	if withOwnerLeg {
		cfg.OwnerGroup = "example.c5c3.io"
		cfg.OwnerKind = "ConfigMap"
		cfg.NewObject = func() client.Object { return &corev1.ConfigMap{} }
	}
	return cfg
}

// indexByRefAnnotation indexes ConfigMaps by their "secret-ref" annotation,
// simulating the per-operator secret-name extractor.
func indexByRefAnnotation(obj client.Object) []string {
	if ref, ok := obj.GetAnnotations()["secret-ref"]; ok && ref != "" {
		return []string{ref}
	}
	return nil
}

func testCM(name, namespace, secretRef string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if secretRef != "" {
		cm.Annotations = map[string]string{"secret-ref": secretRef}
	}
	return cm
}

func testSecret(name, namespace string, owners ...metav1.OwnerReference) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, OwnerReferences: owners,
	}}
}

func TestSecretToOwnersMapper_IndexHit(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("cr-a", "ns1", "db-secret"), testCM("cr-b", "ns1", "other"), testCM("cr-c", "ns2", "db-secret")).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	requests := SecretToOwnersMapper(c, testMapperConfig(false))(context.Background(), testSecret("db-secret", "ns1"))

	g.Expect(requests).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
	), "only the same-namespace CR referencing the Secret may be enqueued")
}

func TestSecretToOwnersMapper_NoMatchesReturnsNil(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	requests := SecretToOwnersMapper(c, testMapperConfig(false))(context.Background(), testSecret("unreferenced", "ns1"))

	g.Expect(requests).To(gomega.BeNil())
}

// The owner-ref leg matches on the API group only — not the exact APIVersion
// — so Secrets persisted with an older APIVersion keep resolving after a
// version bump.
func TestSecretToOwnersMapper_OwnerRefGroupOnlyMatch(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("owner-cr", "ns1", "")).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	secret := testSecret(
		"staging", "ns1",
		metav1.OwnerReference{APIVersion: "example.c5c3.io/v1beta7", Kind: "ConfigMap", Name: "owner-cr", UID: "u1"},
		// Wrong group: must be ignored.
		metav1.OwnerReference{APIVersion: "other.io/v1", Kind: "ConfigMap", Name: "wrong-group", UID: "u2"},
		// Wrong kind: must be ignored.
		metav1.OwnerReference{APIVersion: "example.c5c3.io/v1", Kind: "Other", Name: "wrong-kind", UID: "u3"},
	)

	requests := SecretToOwnersMapper(c, testMapperConfig(true))(context.Background(), secret)

	g.Expect(requests).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "owner-cr"}},
	))
}

// A stale owner-ref whose target no longer exists in the cache is dropped
// instead of enqueueing work for a deleted CR.
func TestSecretToOwnersMapper_StaleOwnerRefDropped(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	secret := testSecret("staging", "ns1",
		metav1.OwnerReference{APIVersion: "example.c5c3.io/v1", Kind: "ConfigMap", Name: "gone", UID: "u1"})

	requests := SecretToOwnersMapper(c, testMapperConfig(true))(context.Background(), secret)

	g.Expect(requests).To(gomega.BeNil())
}

// An empty OwnerKind disables the owner-ref leg entirely: even a matching
// owner reference must not enqueue (the c5c3 index-only shape).
func TestSecretToOwnersMapper_EmptyOwnerKindDisablesLeg(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("owner-cr", "ns1", "")).
		WithIndex(&corev1.ConfigMap{}, testIndexKey, indexByRefAnnotation).
		Build()

	secret := testSecret("staging", "ns1",
		metav1.OwnerReference{APIVersion: "example.c5c3.io/v1", Kind: "ConfigMap", Name: "owner-cr", UID: "u1"})

	requests := SecretToOwnersMapper(c, testMapperConfig(false))(context.Background(), secret)

	g.Expect(requests).To(gomega.BeNil())
}

func cmWithRef(name, namespace, ref string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
		Annotations: map[string]string{"clusterRef": ref},
	}}
}

func TestClusterRefMapper_MatchesRefInSameNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	// ConfigMaps stand in for CRs; the clusterRef name lives in an annotation.
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(
			cmWithRef("cr-a", "ns1", "mariadb"), // matches
			cmWithRef("cr-b", "ns1", "other"),   // wrong ref
			cmWithRef("cr-c", "ns1", ""),        // no ref
			cmWithRef("cr-d", "ns2", "mariadb"), // wrong namespace
		).Build()

	mapper := ClusterRefMapper(c,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		func(o client.Object) string { return o.GetAnnotations()["clusterRef"] })

	changed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: "ns1"}}
	reqs := mapper(context.Background(), changed)
	g.Expect(reqs).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
	))
}

func TestClusterRefMapper_NoMatchReturnsNil(t *testing.T) {
	g := gomega.NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(cmWithRef("cr-a", "ns1", "mariadb")).Build()

	mapper := ClusterRefMapper(c,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		func(o client.Object) string { return o.GetAnnotations()["clusterRef"] })

	changed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: "ns1"}}
	g.Expect(mapper(context.Background(), changed)).To(gomega.BeEmpty())
}

// storeRefEffective extracts a store reference from a ConfigMap standing in for
// a CR: annotation "store-kind"/"store-name" model the CR's effective ref.
func storeRefEffective(o client.Object) commonv1.SecretStoreRefSpec {
	a := o.GetAnnotations()
	return commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreRefKind(a["store-kind"]),
		Name: a["store-name"],
	}
}

func cmWithStore(name, namespace, kind, storeName string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
		Annotations: map[string]string{"store-kind": kind, "store-name": storeName},
	}}
}

func TestStoreRefFanOut_ClusterKindEnqueuesMatchingRefs(t *testing.T) {
	g := gomega.NewWithT(t)
	// A cluster-scoped store fans out to every CR pinned to it by name, across
	// namespaces. cr-c pins a different store and must be skipped.
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		cmWithStore("cr-a", "ns1", "ClusterSecretStore", "openbao-cluster-store"),
		cmWithStore("cr-b", "ns2", "ClusterSecretStore", "openbao-cluster-store"),
		cmWithStore("cr-c", "ns2", "ClusterSecretStore", "other-store"),
	).Build()

	mapper := StoreRefFanOut(c, commonv1.SecretStoreKindCluster,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		storeRefEffective)

	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openbao-cluster-store"}}
	reqs := mapper(context.Background(), changed)
	g.Expect(reqs).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns2", Name: "cr-b"}},
	))
}

func TestStoreRefFanOut_ClusterKindIgnoresOtherStoreName(t *testing.T) {
	g := gomega.NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		cmWithStore("cr-a", "ns1", "ClusterSecretStore", "openbao-cluster-store"),
	).Build()

	mapper := StoreRefFanOut(c, commonv1.SecretStoreKindCluster,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		storeRefEffective)

	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-store"}}
	g.Expect(mapper(context.Background(), changed)).To(gomega.BeEmpty())
}

func TestStoreRefFanOut_NamespacedKindScopesToStoreNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	// A namespaced store only enqueues the CR in its own namespace that pins it
	// as a namespaced ref. cr-b (ns2) and cr-c (cluster ref) must be skipped.
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		cmWithStore("cr-a", "ns1", "SecretStore", "openbao-tenant-store"),
		cmWithStore("cr-b", "ns2", "SecretStore", "openbao-tenant-store"),
		cmWithStore("cr-c", "ns1", "ClusterSecretStore", "openbao-tenant-store"),
	).Build()

	mapper := StoreRefFanOut(c, commonv1.SecretStoreKindNamespaced,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		storeRefEffective)

	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "ns1"}}
	reqs := mapper(context.Background(), changed)
	g.Expect(reqs).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
	))
}

func TestStoreRefFanOut_NamespacedKindIgnoresForeignNamespace(t *testing.T) {
	g := gomega.NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		cmWithStore("cr-a", "ns1", "SecretStore", "openbao-tenant-store"),
	).Build()

	mapper := StoreRefFanOut(c, commonv1.SecretStoreKindNamespaced,
		func() client.ObjectList { return &corev1.ConfigMapList{} },
		storeRefEffective)

	// The store event is in ns2, where no CR pins it — the ns1 CR must not fire.
	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "ns2"}}
	g.Expect(mapper(context.Background(), changed)).To(gomega.BeEmpty())
}
