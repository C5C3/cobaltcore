// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the MariaDB and ClusterSecretStore watch mappers extracted
// into keystone_watches.go. These plain handler.MapFunc closures are never
// executed by the integration tests (which build the controller inline and
// only wire the Secret watch), so they are covered here directly with a fake
// client — mirroring the sibling coverage of secretToKeystoneMapper and the
// c5c3 setupwithmanager_test.go mapper tests.
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// mapperManagedKeystone builds a minimal managed-mode Keystone whose
// spec.database.clusterRef targets the named MariaDB cluster.
func mapperManagedKeystone(name, namespace, clusterRefName string) *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name + "-uid"),
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: clusterRefName},
				Database:   "keystone",
			},
		},
	}
}

// --- mariaDBToKeystoneMapper ---

// TestMariaDBToKeystoneMapper_EnqueuesMatchingClusterRef verifies a MariaDB
// event enqueues exactly the managed-mode Keystones whose
// spec.database.clusterRef.name matches the event object's name in the same
// namespace.
func TestMariaDBToKeystoneMapper_EnqueuesMatchingClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)

	// Two Keystones reference "openstack-db"; a third references a different
	// cluster and must not be enqueued.
	match1 := mapperManagedKeystone("ks-a", "openstack", "openstack-db")
	match2 := mapperManagedKeystone("ks-b", "openstack", "openstack-db")
	other := mapperManagedKeystone("ks-c", "openstack", "some-other-db")
	c := newMapperFakeClient(match1, match2, other)
	mapper := mariaDBToKeystoneMapper(c)

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: "openstack"},
	}
	reqs := mapper(context.Background(), mariadb)

	names := make([]types.NamespacedName, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.NamespacedName)
	}
	g.Expect(names).To(ConsistOf(
		types.NamespacedName{Namespace: "openstack", Name: "ks-a"},
		types.NamespacedName{Namespace: "openstack", Name: "ks-b"},
	), "only Keystones whose clusterRef matches the MariaDB name must be enqueued")
}

// TestMariaDBToKeystoneMapper_IgnoresBrownfieldAndOtherClusters verifies that a
// brownfield Keystone (ClusterRef == nil) and a managed Keystone targeting a
// different MariaDB are both left alone.
func TestMariaDBToKeystoneMapper_IgnoresBrownfieldAndOtherClusters(t *testing.T) {
	g := NewGomegaWithT(t)

	// Brownfield: no clusterRef at all.
	brownfield := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: "ks-brownfield", Namespace: "openstack"},
		Spec: keystonev1alpha1.KeystoneSpec{
			Database: commonv1.DatabaseSpec{Host: "db.example.com", Database: "keystone"},
		},
	}
	// Managed, but references a different MariaDB cluster.
	otherCluster := mapperManagedKeystone("ks-other", "openstack", "different-db")
	c := newMapperFakeClient(brownfield, otherCluster)
	mapper := mariaDBToKeystoneMapper(c)

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: "openstack"},
	}
	reqs := mapper(context.Background(), mariadb)

	g.Expect(reqs).To(BeEmpty(),
		"a MariaDB event must not enqueue brownfield CRs or CRs targeting a different cluster")
}

// TestMariaDBToKeystoneMapper_ScopedToNamespace verifies the mapper only
// enqueues Keystones in the MariaDB event's own namespace, even when a
// same-named cluster is referenced from another namespace.
func TestMariaDBToKeystoneMapper_ScopedToNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	inNs := mapperManagedKeystone("ks-in", "ns-a", "shared-db")
	otherNs := mapperManagedKeystone("ks-out", "ns-b", "shared-db")
	c := newMapperFakeClient(inNs, otherNs)
	mapper := mariaDBToKeystoneMapper(c)

	mariadb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-db", Namespace: "ns-a"},
	}
	reqs := mapper(context.Background(), mariadb)

	g.Expect(reqs).To(HaveLen(1),
		"only the Keystone in the MariaDB event's namespace must be enqueued")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: "ns-a", Name: "ks-in"}))
}

// --- clusterSecretStoreToKeystoneMapper ---

// TestClusterSecretStoreToKeystoneMapper_EnqueuesAllKeystones verifies a status
// change on the OpenBao-backed ClusterSecretStore enqueues every Keystone in
// the cluster (across namespaces), since the cluster-scoped store is shared and
// an ESO/OpenBao outage affects all of them.
func TestClusterSecretStoreToKeystoneMapper_EnqueuesAllKeystones(t *testing.T) {
	g := NewGomegaWithT(t)

	ksA := mapperManagedKeystone("ks-a", "ns-a", "db-a")
	ksB := mapperManagedKeystone("ks-b", "ns-b", "db-b")
	c := newMapperFakeClient(ksA, ksB)
	mapper := clusterSecretStoreToKeystoneMapper(c)

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
	}
	reqs := mapper(context.Background(), store)

	names := make([]types.NamespacedName, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.NamespacedName)
	}
	g.Expect(names).To(ConsistOf(
		types.NamespacedName{Namespace: "ns-a", Name: "ks-a"},
		types.NamespacedName{Namespace: "ns-b", Name: "ks-b"},
	), "a ClusterSecretStore change must enqueue every Keystone in the cluster")
}

// TestClusterSecretStoreToKeystoneMapper_IgnoresOtherStores verifies the mapper
// only reacts to the OpenBao-backed store the operator routes secrets through,
// not to unrelated ClusterSecretStores.
func TestClusterSecretStoreToKeystoneMapper_IgnoresOtherStores(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := mapperManagedKeystone("ks", "openstack", "openstack-db")
	c := newMapperFakeClient(ks)
	mapper := clusterSecretStoreToKeystoneMapper(c)

	other := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "some-other-store"},
	}
	reqs := mapper(context.Background(), other)

	g.Expect(reqs).To(BeEmpty(),
		"a change to an unrelated ClusterSecretStore must enqueue nothing")
}
