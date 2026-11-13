// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the Placement watch mappers. These plain handler.MapFunc
// closures are exercised directly against a pre-indexed fake client, mirroring
// glance_watches_test.go.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/cache"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// namedSecret returns a bare Secret object for a watch event (no data needed —
// the mappers key off the name and namespace only).
func namedSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

// mapperManagedPlacement builds a Placement in namespace whose
// spec.database.clusterRef targets the named MariaDB cluster.
func mapperManagedPlacement(name, namespace, clusterRefName string) *placementv1alpha1.Placement {
	p := testPlacement()
	p.Name = name
	p.Namespace = namespace
	p.UID = types.UID(name + "-uid")
	p.Spec.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: clusterRefName},
		Database:   "placement",
		SecretRef:  commonv1.SecretRefSpec{Name: name + "-db"},
	}
	return p
}

// --- secretToPlacementMapper ---

func TestSecretToPlacementMapper_ReferencedSecretsEnqueueTheCR(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient(testPlacement())
	mapper := secretToPlacementMapper(c)

	// spec.serviceUser.secretRef.name of test-placement.
	g.Expect(mapper(context.Background(), namedSecret("placement-service-user"))).To(ConsistOf(placementRequest))
	// spec.database.secretRef.name of test-placement.
	g.Expect(mapper(context.Background(), namedSecret("placement-db"))).To(ConsistOf(placementRequest))
}

func TestSecretToPlacementMapper_UnreferencedSecretEnqueuesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newMapperFakeClient(testPlacement())
	g.Expect(secretToPlacementMapper(c)(context.Background(), namedSecret("unrelated"))).To(BeEmpty())
}

// --- mariaDBToPlacementMapper ---

func TestMariaDBToPlacementMapper_EnqueuesMatchingClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)

	match1 := mapperManagedPlacement("pl-a", "openstack", "openstack-db")
	match2 := mapperManagedPlacement("pl-b", "openstack", "openstack-db")
	other := mapperManagedPlacement("pl-c", "openstack", "some-other-db")
	c := newMapperFakeClient(match1, match2, other)

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: "openstack"},
	}
	reqs := mariaDBToPlacementMapper(c)(context.Background(), mariadb)

	g.Expect(reqs).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "pl-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "pl-b"}},
	), "only Placements whose clusterRef matches the MariaDB name must be enqueued")
}

func TestMariaDBToPlacementMapper_IgnoresBrownfieldAndOtherNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)

	// Brownfield: no clusterRef at all (testPlacement uses host/port).
	brownfield := testPlacement()
	brownfield.Name = "pl-brownfield"
	brownfield.Namespace = "openstack"
	// Managed, but in another namespace than the MariaDB event.
	foreign := mapperManagedPlacement("pl-foreign", "other", "openstack-db")
	c := newMapperFakeClient(brownfield, foreign)

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: "openstack"},
	}
	reqs := mariaDBToPlacementMapper(c)(context.Background(), mariadb)

	g.Expect(reqs).To(BeEmpty(),
		"a MariaDB event must not enqueue brownfield CRs or CRs in another namespace")
}

// --- maxScaleToPlacementMapper ---

// TestMaxScaleToPlacementMapper_EnqueuesTheCRsOfItsMariaDB verifies a MaxScale
// event enqueues the Placements whose clusterRef names the MariaDB the
// MaxScale's spec.mariaDbRef names, and nothing for a brownfield Placement, a
// MaxScale of another MariaDB, or one without mariaDbRef.
func TestMaxScaleToPlacementMapper_EnqueuesTheCRsOfItsMariaDB(t *testing.T) {
	g := NewGomegaWithT(t)

	match := mapperManagedPlacement("pl-a", "openstack", "openstack-db")
	other := mapperManagedPlacement("pl-c", "openstack", "some-other-db")
	brownfield := testPlacement()
	brownfield.Name = "pl-brownfield"
	brownfield.Namespace = "openstack"
	mapper := maxScaleToPlacementMapper(newMapperFakeClient(match, other, brownfield))

	g.Expect(mapper(context.Background(), maxScaleOf("openstack", "openstack-db"))).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "openstack", Name: "pl-a"}},
	))
	g.Expect(mapper(context.Background(), maxScaleOf("openstack", "unrelated-db"))).To(BeEmpty())
	g.Expect(mapper(context.Background(), maxScaleOf("openstack", ""))).To(BeEmpty(),
		"a MaxScale without mariaDbRef concerns no Placement")
}

// maxScaleOf returns a MaxScale in namespace whose spec.mariaDbRef names
// mariaDB; an empty mariaDB leaves the reference unset.
func maxScaleOf(namespace, mariaDB string) *mariadbv1alpha1.MaxScale {
	mxs := &mariadbv1alpha1.MaxScale{ObjectMeta: metav1.ObjectMeta{Name: "openstack-db-maxscale", Namespace: namespace}}
	if mariaDB != "" {
		mxs.Spec.MariaDBRef = &mariadbv1alpha1.MariaDBRef{ObjectReference: mariadbv1alpha1.ObjectReference{Name: mariaDB}}
	}
	return mxs
}

// --- storeToPlacementMapper ---

func TestStoreToPlacementMapper_ClusterKindEnqueuesDefaultPlacements(t *testing.T) {
	g := NewGomegaWithT(t)

	plA := mapperManagedPlacement("pl-a", "ns-a", "db-a")
	plB := mapperManagedPlacement("pl-b", "ns-b", "db-b")
	c := newMapperFakeClient(plA, plB)
	mapper := storeToPlacementMapper(c, commonv1.SecretStoreKindCluster)

	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(mapper(context.Background(), store)).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns-a", Name: "pl-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns-b", Name: "pl-b"}},
	), "a ClusterSecretStore change must enqueue every Placement defaulting to it")

	// An unrelated cluster store wakes nothing.
	other := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: "some-other-store"}}
	g.Expect(mapper(context.Background(), other)).To(BeEmpty())
}

func TestStoreToPlacementMapper_NamespacedKindScopesToStoreNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	pinned := mapperManagedPlacement("pl-pinned", "tenant-a", "db-a")
	pinned.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	defaulted := mapperManagedPlacement("pl-default", "tenant-a", "db-b")
	foreign := mapperManagedPlacement("pl-foreign", "tenant-b", "db-c")
	foreign.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	c := newMapperFakeClient(pinned, defaulted, foreign)

	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
	}
	reqs := storeToPlacementMapper(c, commonv1.SecretStoreKindNamespaced)(context.Background(), store)

	g.Expect(reqs).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "pl-pinned"}},
	), "a namespaced SecretStore change must enqueue only the pinned Placement in its own namespace")
}

// TestMemcachedToPlacementMapper verifies a Memcached event enqueues exactly the
// Placement CRs in its namespace whose spec.cache.clusterRef names it, and leaves
// brownfield caches, other namespaces and other names alone.
func TestMemcachedToPlacementMapper(t *testing.T) {
	withCache := func(name, namespace string, spec commonv1.CacheSpec) *placementv1alpha1.Placement {
		cr := testPlacement()
		cr.Name, cr.Namespace = name, namespace
		cr.Spec.Cache = spec
		return cr
	}
	managed := func(name string) commonv1.CacheSpec {
		return commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{Name: name}}
	}
	c := newMapperFakeClient(
		withCache("match", "default", managed("mc")),
		withCache("brownfield", "default", commonv1.CacheSpec{Servers: []string{"mc:11211"}}),
		withCache("other-ns", "elsewhere", managed("mc")),
		withCache("other-name", "default", managed("other-mc")),
	)
	mc := cache.NewMemcached()
	mc.SetNamespace("default")
	mc.SetName("mc")

	reqs := memcachedToPlacementMapper(c)(context.Background(), mc)

	g := NewGomegaWithT(t)
	g.Expect(reqs).To(ConsistOf(reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "match"},
	}))
}
