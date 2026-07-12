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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// openBaoClusterStoreName aliases the shared ClusterSecretStore name
// (secrets.OpenBaoClusterStoreName) for the watch-mapper fixtures in this
// package's tests.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

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

// --- storeToKeystoneMapper ---

// TestStoreToKeystoneMapper_ClusterKindEnqueuesDefaultKeystones verifies a status
// change on the OpenBao-backed ClusterSecretStore enqueues every Keystone in
// the cluster that defaults to it (across namespaces), since the cluster-scoped
// store is shared and an ESO/OpenBao outage affects all of them.
func TestStoreToKeystoneMapper_ClusterKindEnqueuesDefaultKeystones(t *testing.T) {
	g := NewGomegaWithT(t)

	ksA := mapperManagedKeystone("ks-a", "ns-a", "db-a")
	ksB := mapperManagedKeystone("ks-b", "ns-b", "db-b")
	c := newMapperFakeClient(ksA, ksB)
	mapper := storeToKeystoneMapper(c, commonv1.SecretStoreKindCluster)

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
	), "a ClusterSecretStore change must enqueue every Keystone defaulting to it")
}

// TestStoreToKeystoneMapper_ClusterKindIgnoresOtherStores verifies the mapper
// only reacts to the store a Keystone's effective ref resolves to, not to
// unrelated ClusterSecretStores.
func TestStoreToKeystoneMapper_ClusterKindIgnoresOtherStores(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := mapperManagedKeystone("ks", "openstack", "openstack-db")
	c := newMapperFakeClient(ks)
	mapper := storeToKeystoneMapper(c, commonv1.SecretStoreKindCluster)

	other := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "some-other-store"},
	}
	reqs := mapper(context.Background(), other)

	g.Expect(reqs).To(BeEmpty(),
		"a change to an unrelated ClusterSecretStore must enqueue nothing")
}

// TestStoreToKeystoneMapper_NamespacedKindScopesToStoreNamespace verifies a
// namespaced SecretStore only enqueues the Keystone in its own namespace that
// pins it via spec.secretStoreRef; a default (cluster-store) Keystone and a
// same-name Keystone in a foreign namespace are not enqueued.
func TestStoreToKeystoneMapper_NamespacedKindScopesToStoreNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	pinned := mapperManagedKeystone("ks-pinned", "tenant-a", "db-a")
	pinned.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	defaulted := mapperManagedKeystone("ks-default", "tenant-a", "db-b")
	foreign := mapperManagedKeystone("ks-foreign", "tenant-b", "db-c")
	foreign.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}
	c := newMapperFakeClient(pinned, defaulted, foreign)
	mapper := storeToKeystoneMapper(c, commonv1.SecretStoreKindNamespaced)

	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
	}
	reqs := mapper(context.Background(), store)

	g.Expect(reqs).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "ks-pinned"}},
	), "a namespaced SecretStore change must enqueue only the pinned Keystone in its own namespace")
}

// --- identity-backend mappers ---

// TestIdentityBackendToKeystoneMapper_EnqueuesKeystoneRef verifies a backend
// event enqueues exactly its spec.keystoneRef in the backend's namespace.
func TestIdentityBackendToKeystoneMapper_EnqueuesKeystoneRef(t *testing.T) {
	g := NewGomegaWithT(t)
	mapper := identityBackendToKeystoneMapper()

	backend := testIdentityBackend("corp-ldap", "corp")
	requests := mapper(context.Background(), backend)
	g.Expect(requests).To(ConsistOf(reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-keystone"},
	}))

	// An empty keystoneRef (bypassed admission) must enqueue nothing rather
	// than a request with an empty name.
	empty := testIdentityBackend("broken", "corp")
	empty.Spec.KeystoneRef.Name = ""
	g.Expect(mapper(context.Background(), empty)).To(BeEmpty())
}

// TestKeystoneToIdentityBackendsMapper_FansOutToAttachedBackends verifies a
// Keystone event enqueues every backend attached via the keystoneRef index —
// and none attached to other Keystones.
func TestKeystoneToIdentityBackendsMapper_FansOutToAttachedBackends(t *testing.T) {
	g := NewGomegaWithT(t)

	attached1 := testIdentityBackend("a-ldap", "corp-a")
	attached2 := testIdentityBackend("b-ldap", "corp-b")
	foreign := testIdentityBackend("c-ldap", "corp-c")
	foreign.Spec.KeystoneRef.Name = "another-keystone"
	c := newMapperFakeClient(attached1, attached2, foreign)

	ks := testKeystone()
	requests := keystoneToIdentityBackendsMapper(c)(context.Background(), ks)
	g.Expect(requests).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "a-ldap"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "b-ldap"}},
	))

	// A Keystone with no attached backends fans out to nothing.
	other := testKeystone()
	other.Name = "unattached"
	g.Expect(keystoneToIdentityBackendsMapper(c)(context.Background(), other)).To(BeEmpty())
}

// TestSecretToKeystoneWithBackendsMapper_BindSecretWakesOwningKeystone
// verifies the identity-backend leg: a bind-credentials (or TLS CA) Secret
// event enqueues the Keystone the referencing backend attaches to, unioned
// with the base Keystone legs without duplicates.
func TestSecretToKeystoneWithBackendsMapper_BindSecretWakesOwningKeystone(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := testKeystone()
	backend := testIdentityBackend("corp-ldap", "corp")
	c := newMapperFakeClient(ks, backend)
	mapper := secretToKeystoneWithBackendsMapper(c)

	// The bind Secret is not referenced by any Keystone spec field, so only
	// the backend leg produces the request.
	bindSecret := testBindSecret("corp-ldap")
	requests := mapper(context.Background(), bindSecret)
	g.Expect(requests).To(ConsistOf(reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-keystone"},
	}))

	// A Secret referenced by BOTH the Keystone spec (admin password) and a
	// backend must yield exactly one request per Keystone (dedup contract).
	dualBackend := testIdentityBackend("dual-ldap", "corp-dual")
	dualBackend.Spec.LDAP.BindCredentialsSecretRef.Name = "keystone-admin"
	c2 := newMapperFakeClient(ks, dualBackend)
	adminSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"}}
	requests = secretToKeystoneWithBackendsMapper(c2)(context.Background(), adminSecret)
	g.Expect(requests).To(ConsistOf(reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-keystone"},
	}))

	// An unreferenced Secret enqueues nothing.
	unref := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"}}
	g.Expect(mapper(context.Background(), unref)).To(BeEmpty())
}

// TestIdentityBackendSecretNameExtractor_UnionsBindAndCA pins the extractor's
// dedup contract: bind + CA names, empty names skipped, duplicates collapsed.
func TestIdentityBackendSecretNameExtractor_UnionsBindAndCA(t *testing.T) {
	g := NewGomegaWithT(t)

	b := testIdentityBackend("corp-ldap", "corp")
	g.Expect(identityBackendSecretNameExtractor(b)).To(ConsistOf("corp-ldap-bind"))

	b.Spec.LDAP.TLS = &keystonev1alpha1.LDAPTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "corp-ca"},
	}
	g.Expect(identityBackendSecretNameExtractor(b)).To(ConsistOf("corp-ldap-bind", "corp-ca"))

	// Same Secret for bind and CA: deduplicated.
	b.Spec.LDAP.TLS.CABundleSecretRef.Name = "corp-ldap-bind"
	g.Expect(identityBackendSecretNameExtractor(b)).To(ConsistOf("corp-ldap-bind"))

	// No LDAP block (bypassed admission): nothing indexed.
	b.Spec.LDAP = nil
	g.Expect(identityBackendSecretNameExtractor(b)).To(BeEmpty())
}
