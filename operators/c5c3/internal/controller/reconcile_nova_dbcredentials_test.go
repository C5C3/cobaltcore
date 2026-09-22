// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Nova DB-credentials concern
// (reconcile_nova_dbcredentials.go): the two targets the service-agnostic
// builders in reconcile_dbcredentials.go consume, the OpenBao handles each
// carries, the per-service databaseCredentialsMode override plus the Static
// opt-out that decide the one mode both schemas share, and the objects
// reconcileNova projects from them. Every test runs against novaControlPlane /
// newNovaTestReconciler from reconcile_nova_test.go.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// TestNovaDBDynamicKeys_FollowTheNovaNamespace pins the re-keying of both
// credential chains onto the NOVA service namespace: the engine role names, the
// dynamic creds paths, and the static KV paths all track it, so nova's engine
// plumbing is keystone-independent. The role names MUST stay in sync with
// setup-database-tenant.sh. It also pins every field the service-agnostic
// builders read off the two targets, including the fixed ServiceAccount names
// the nova-api-db and nova-cell-db auth roles bind in setup-auth.sh.
func TestNovaDBDynamicKeys_FollowTheNovaNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := novaControlPlane() // no namespace assignment; nova shares "default"
	g.Expect(novaAPIDBDynamicRoleFor(cp)).To(Equal("nova-api-default"),
		"an unassigned Nova keeps the ControlPlane-namespace-derived role names")
	g.Expect(novaCellDBDynamicRoleFor(cp)).To(Equal("nova-cell-default"))
	g.Expect(novaAPIDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/nova-api-default"))
	g.Expect(novaCellDBDynamicCredsPathFor(cp)).To(Equal("database/mariadb/creds/nova-cell-default"))
	g.Expect(novaAPIDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/nova/default/cp/api-db"))
	g.Expect(novaCellDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/nova/default/cp/db"))

	colocated := novaAPIDBCredentialTarget(cp)
	g.Expect(colocated.namespace).To(Equal("default"),
		"a co-located Nova keeps its credential material in the ControlPlane's namespace")
	g.Expect(novaCellDBCredentialTarget(cp).namespace).To(Equal("default"))

	// A services.nova.namespace assignment moves both concerns with the service:
	// every OpenBao handle re-keys onto it and every object is built in the
	// namespace the generators' policies grant.
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	g.Expect(novaAPIDBDynamicRoleFor(cp)).To(Equal("nova-api-compute"))
	g.Expect(novaCellDBDynamicRoleFor(cp)).To(Equal("nova-cell-compute"))
	g.Expect(novaAPIDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/nova/compute/cp/api-db"))
	g.Expect(novaCellDBCredentialRemoteKeyFor(cp)).To(Equal("openstack/nova/compute/cp/db"))

	// The names the two auth roles bind in setup-auth.sh, and the per-ControlPlane
	// object names tracking the projected Nova child. All of them are
	// ControlPlane-derived, so the move leaves them untouched.
	g.Expect(novaAPIDBCredentialServiceAccountName).To(Equal("nova-api-db-creds"))
	g.Expect(novaCellDBCredentialServiceAccountName).To(Equal("nova-cell-db-creds"))
	g.Expect(novaAPIDBDynamicVaultRole).To(Equal("nova-api-db"))
	g.Expect(novaCellDBDynamicVaultRole).To(Equal("nova-cell-db"))
	g.Expect(novaAPIDBCredentialSecretName(cp)).To(Equal("cp-nova-api-db-credentials"))
	g.Expect(novaCellDBCredentialSecretName(cp)).To(Equal("cp-nova-db-credentials"))
	g.Expect(novaAPIDBCredentialClientCertName(cp)).To(Equal("cp-nova-api-db-openbao-client"))
	g.Expect(novaCellDBCredentialClientCertName(cp)).To(Equal("cp-nova-db-openbao-client"))

	apiTarget := novaAPIDBCredentialTarget(cp)
	g.Expect(apiTarget.qualifier).To(Equal("NovaAPI"))
	g.Expect(apiTarget.prefix()).To(Equal("NovaAPI "), "the target must name the schema in the messages it emits")
	g.Expect(apiTarget.namespace).To(Equal("compute"), "every dynamic object lands beside the Nova child")
	g.Expect(apiTarget.secretName).To(Equal("cp-nova-api-db-credentials"))
	g.Expect(apiTarget.certName).To(Equal("cp-nova-api-db-openbao-client"))
	g.Expect(apiTarget.saName).To(Equal("nova-api-db-creds"),
		"the generator's SA keeps the fixed name the nova-api-db role binds in any namespace")
	g.Expect(apiTarget.vaultRole).To(Equal("nova-api-db"))
	g.Expect(apiTarget.credsPath).To(Equal("database/mariadb/creds/nova-api-compute"))
	g.Expect(apiTarget.kvPath).To(Equal("openstack/nova/compute/cp/api-db"))
	g.Expect(apiTarget.storeRef).To(Equal(effectiveControlPlaneStoreRef(cp)))

	cellTarget := novaCellDBCredentialTarget(cp)
	g.Expect(cellTarget.qualifier).To(Equal("NovaCell"))
	g.Expect(cellTarget.prefix()).To(Equal("NovaCell "))
	g.Expect(cellTarget.namespace).To(Equal("compute"))
	g.Expect(cellTarget.secretName).To(Equal("cp-nova-db-credentials"))
	g.Expect(cellTarget.certName).To(Equal("cp-nova-db-openbao-client"))
	g.Expect(cellTarget.saName).To(Equal("nova-cell-db-creds"))
	g.Expect(cellTarget.vaultRole).To(Equal("nova-cell-db"))
	g.Expect(cellTarget.credsPath).To(Equal("database/mariadb/creds/nova-cell-compute"))
	g.Expect(cellTarget.kvPath).To(Equal("openstack/nova/compute/cp/db"))
	g.Expect(cellTarget.storeRef).To(Equal(effectiveControlPlaneStoreRef(cp)))

	// The two chains share no object at all: one rotation must never take the
	// other schema's login down with it.
	g.Expect(apiTarget.secretName).NotTo(Equal(cellTarget.secretName))
	g.Expect(apiTarget.certName).NotTo(Equal(cellTarget.certName))
	g.Expect(apiTarget.saName).NotTo(Equal(cellTarget.saName))
	g.Expect(apiTarget.credsPath).NotTo(Equal(cellTarget.credsPath))
	g.Expect(apiTarget.kvPath).NotTo(Equal(cellTarget.kvPath))
}

// TestNovaDBDynamicRoles_ArePrefixFree is the edge case the "nova-cell" role
// name exists for. A per-tenant engine role is "<role>-<namespace>", so a cell
// role named "nova" would flatten to the same string in namespace "api-x" that
// the API role produces in namespace "x", and the two ControlPlanes would
// overwrite each other's engine connection config.
func TestNovaDBDynamicRoles_ArePrefixFree(t *testing.T) {
	g := NewGomegaWithT(t)

	inNamespace := func(ns string) *c5c3v1alpha1.ControlPlane {
		cp := novaControlPlane()
		cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: ns}
		return cp
	}

	g.Expect(novaCellDBDynamicRoleFor(inNamespace("api-x"))).NotTo(Equal(novaAPIDBDynamicRoleFor(inNamespace("x"))),
		"the cell role must not collide with another tenant's API role")
	g.Expect(novaCellDBDynamicCredsPathFor(inNamespace("api-x"))).
		NotTo(Equal(novaAPIDBDynamicCredsPathFor(inNamespace("x"))))
}

// TestNovaDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed is
// the fail-safe twin: the validating webhook rejects a Dynamic override / mode on
// a dedicated nova database, but a webhook-bypassed CR must still fall closed
// onto Static rather than projecting generators that could never sync (no engine
// role exists for a dedicated instance). The other closed branches are pinned
// beside it: an explicit Static override, the shared block's own Static opt-out,
// a brownfield shared database with no cluster to issue against, and a CR whose
// database does not resolve at all.
func TestNovaDBCredentialsDynamicEnabled_DedicatedIsStaticEvenWhenModeBypassed(t *testing.T) {
	g := NewGomegaWithT(t)

	dedicated := novaControlPlane()
	dedicated.Spec.Services.Nova.DedicatedBackingServices = &c5c3v1alpha1.NovaDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-nova-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "nova",
			SecretRef:       commonv1.SecretRefSpec{Name: "nova-db"},
		},
	}
	g.Expect(novaDBCredentialsDynamicEnabled(dedicated)).To(BeFalse(),
		"a dedicated nova database is never Dynamic, even with the mode written directly into the spec")

	static := novaControlPlane()
	static.Spec.Services.Nova.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
	g.Expect(novaDBCredentialsDynamicEnabled(static)).To(BeFalse(),
		"a per-service Static override opts both nova schemas out of the engine-issued credential")

	sharedStatic := novaControlPlane()
	sharedStatic.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
	g.Expect(novaDBCredentialsDynamicEnabled(sharedStatic)).To(BeFalse(),
		"with no per-service override Nova inherits the shared Static opt-out")

	brownfield := novaControlPlane()
	brownfield.Spec.Infrastructure.Database.ClusterRef = nil
	g.Expect(novaDBCredentialsDynamicEnabled(brownfield)).To(BeFalse(),
		"an unmanaged shared database has no engine role to issue credentials from")

	unresolvable := novaControlPlane()
	unresolvable.Spec.Infrastructure = nil
	unresolvable.Spec.Services.Nova = nil
	g.Expect(novaDBCredentialsDynamicEnabled(unresolvable)).To(BeFalse(),
		"with no nova block and no infrastructure nothing resolves, so the predicate falls closed")

	shared := novaControlPlane()
	g.Expect(novaDBCredentialsDynamicEnabled(shared)).To(BeTrue(),
		"a managed shared database with no per-service override inherits the ControlPlane-wide Dynamic default")
}
