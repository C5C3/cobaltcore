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

func TestClusterSecretStoreFanOut_NameGuard(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("cr-a", "ns1", "")).
		Build()

	mapper := ClusterSecretStoreFanOut(c, "openbao-cluster-store",
		func() client.ObjectList { return &corev1.ConfigMapList{} })

	// A different store must not fan out.
	g.Expect(mapper(context.Background(), testSecret("some-other-store", ""))).To(gomega.BeNil())
}

func TestClusterSecretStoreFanOut_EnqueuesAllCRs(t *testing.T) {
	g := gomega.NewWithT(t)

	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithObjects(testCM("cr-a", "ns1", ""), testCM("cr-b", "ns2", "")).
		Build()

	mapper := ClusterSecretStoreFanOut(c, "openbao-cluster-store",
		func() client.ObjectList { return &corev1.ConfigMapList{} })

	requests := mapper(context.Background(), testSecret("openbao-cluster-store", ""))

	g.Expect(requests).To(gomega.ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "cr-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns2", Name: "cr-b"}},
	), "the store transition must fan out to every CR cluster-wide")
}
