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
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// novaLeftoverClientCert builds one chain's mTLS client Certificate at its
// derived name/namespace, as a prior Dynamic deployment left it.
func novaLeftoverClientCert(target dbCredentialTarget) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(target.certName)
	cert.SetNamespace(target.namespace)
	return cert
}

// TestReconcileNova_DynamicDefaultProjectsEngineObjects verifies a managed shared
// nova database (default Dynamic) projects, for BOTH schemas, the
// generator-backed ExternalSecret (no static Data), the VaultDynamicSecret with
// its own role and per-tenant creds path, its own ServiceAccount, and its own
// mTLS client Certificate.
func TestReconcileNova_DynamicDefaultProjectsEngineObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	g.Expect(novaDBCredentialsDynamicEnabled(cp)).To(BeTrue(),
		"a managed shared nova database defaults to Dynamic")

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, target := range novaDBCredentialTargets(cp) {
		key := types.NamespacedName{Namespace: target.namespace, Name: target.secretName}

		// ExternalSecret: generator-backed, no static KV Data, no SecretStoreRef.
		es := &esov1.ExternalSecret{}
		g.Expect(r.Get(ctx, key, es)).To(Succeed(),
			"operator must create the %s DB-credential ExternalSecret", target.qualifier)
		g.Expect(es.Spec.Data).To(BeEmpty(), "the Dynamic ExternalSecret must carry no static Data refs")
		g.Expect(es.Spec.SecretStoreRef.Name).To(BeEmpty(),
			"a generator-backed ExternalSecret must not reference a SecretStore")
		g.Expect(es.Spec.DataFrom).To(HaveLen(1))
		g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("VaultDynamicSecret"))
		g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Name).To(Equal(target.secretName))

		// VaultDynamicSecret: its own role and per-tenant creds path.
		vds := &esgenv1alpha1.VaultDynamicSecret{}
		g.Expect(r.Get(ctx, key, vds)).To(Succeed(),
			"operator must create the %s VaultDynamicSecret generator", target.qualifier)
		g.Expect(vds.Spec.Path).To(Equal(target.credsPath))
		g.Expect(vds.Spec.Method).To(Equal("GET"))
		g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal(target.vaultRole))
		g.Expect(vds.Spec.Provider.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal(target.saName))
		g.Expect(vds.Spec.Provider.CAProvider.Name).To(Equal(target.certName))
		g.Expect(vds.Spec.Provider.ClientTLS.CertSecretRef.Name).To(Equal(target.certName))

		// The ServiceAccount the auth role binds, and the client Certificate.
		g.Expect(r.Get(ctx, types.NamespacedName{
			Namespace: target.namespace, Name: target.saName,
		}, &corev1.ServiceAccount{})).To(Succeed())
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		g.Expect(r.Get(ctx, types.NamespacedName{
			Namespace: target.namespace, Name: target.certName,
		}, cert)).To(Succeed())
		issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
		g.Expect(issuer).To(Equal(openBaoCAIssuerName))
	}

	// The projected child carries Dynamic on both of its database blocks.
	nv := getProjectedNova(t, r.Client, cp)
	g.Expect(nv.Spec.APIDatabase.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
	g.Expect(nv.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
}

// TestReconcileNova_StaticOptOutProjectsKVAndTearsDownDynamic verifies that both
// opt-out routes, the shared credentialsMode: Static and the per-service
// services.nova.databaseCredentialsMode: Static, project the KV-backed
// ExternalSecrets, tear down the leftover generator objects of BOTH chains, and
// stamp the child Static on both database blocks.
func TestReconcileNova_StaticOptOutProjectsKVAndTearsDownDynamic(t *testing.T) {
	for _, tt := range []struct {
		name  string
		apply func(cp *c5c3v1alpha1.ControlPlane)
	}{
		{
			name: "shared credentialsMode Static",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
			},
		},
		{
			name: "per-service databaseCredentialsMode Static",
			apply: func(cp *c5c3v1alpha1.ControlPlane) {
				cp.Spec.Services.Nova.DatabaseCredentialsMode = commonv1.CredentialsModeStatic
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			tt.apply(cp)
			g.Expect(novaDBCredentialsDynamicEnabled(cp)).To(BeFalse())

			// Pre-seed the leftover generators, SAs and mTLS client Certificates from a
			// prior Dynamic deployment, each carrying the ownership a live projection
			// stamps: the teardown is gated on it.
			s := novaTestScheme(t)
			var leftovers []client.Object
			for _, target := range novaDBCredentialTargets(cp) {
				leftovers = append(leftovers,
					dbCredentialVaultDynamicSecret(target, openBaoDefaultServer, openBaoDefaultKubernetesMount),
					dbCredentialServiceAccount(target),
					novaLeftoverClientCert(target))
			}
			for _, obj := range leftovers {
				g.Expect(claimChildOwnership(localWriter(), cp, obj, s)).To(Succeed())
			}
			r := newNovaTestReconciler(t, append([]client.Object{cp}, leftovers...)...)
			ctx := context.Background()

			_, err := r.reconcileNova(ctx, cp)
			g.Expect(err).NotTo(HaveOccurred())

			for _, target := range novaDBCredentialTargets(cp) {
				key := types.NamespacedName{Namespace: target.namespace, Name: target.secretName}

				es := &esov1.ExternalSecret{}
				g.Expect(r.Get(ctx, key, es)).To(Succeed())
				g.Expect(es.Spec.DataFrom).To(BeEmpty(), "the Static opt-out must project the KV ExternalSecret")
				g.Expect(es.Spec.Data).To(HaveLen(2))
				g.Expect(es.Spec.Data[0].RemoteRef.Key).To(Equal(target.kvPath))

				g.Expect(apierrors.IsNotFound(r.Get(ctx, key, &esgenv1alpha1.VaultDynamicSecret{}))).To(BeTrue(),
					"the Static opt-out must delete the leftover %s VaultDynamicSecret", target.qualifier)
				g.Expect(apierrors.IsNotFound(r.Get(ctx, types.NamespacedName{
					Namespace: target.namespace, Name: target.saName,
				}, &corev1.ServiceAccount{}))).To(BeTrue(),
					"the Static opt-out must delete the generator's ServiceAccount")
				sweptCert := &unstructured.Unstructured{}
				sweptCert.SetGroupVersionKind(certificateGVK)
				g.Expect(apierrors.IsNotFound(r.Get(ctx, types.NamespacedName{
					Namespace: target.namespace, Name: target.certName,
				}, sweptCert))).To(BeTrue(),
					"the Static opt-out must delete the leftover mTLS client Certificate")
			}

			nv := getProjectedNova(t, r.Client, cp)
			g.Expect(nv.Spec.APIDatabase.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
			g.Expect(nv.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))
		})
	}
}

// TestReconcileNova_DynamicObjectsLandInTheNovaNamespace verifies every dynamic
// object of both chains lands beside the Nova child in a namespace of its own,
// carrying the ownership labels rather than an owner reference, and that nothing
// is left in the ControlPlane's namespace.
func TestReconcileNova_DynamicObjectsLandInTheNovaNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	_, err := r.reconcileNova(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, target := range novaDBCredentialTargets(cp) {
		g.Expect(target.namespace).To(Equal("compute"))
		key := types.NamespacedName{Namespace: "compute", Name: target.secretName}

		es := &esov1.ExternalSecret{}
		g.Expect(r.Get(ctx, key, es)).To(Succeed())
		g.Expect(es.OwnerReferences).To(BeEmpty(), "a cross-namespace object cannot carry an owner reference")
		g.Expect(es.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))

		vds := &esgenv1alpha1.VaultDynamicSecret{}
		g.Expect(r.Get(ctx, key, vds)).To(Succeed())
		g.Expect(vds.Spec.Path).To(Equal(target.credsPath),
			"the generator's per-tenant path follows the nova namespace")

		g.Expect(r.Get(ctx, types.NamespacedName{
			Namespace: "compute", Name: target.saName,
		}, &corev1.ServiceAccount{})).To(Succeed(),
			"the generator's SA must authenticate from the namespace the policy grants")

		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		g.Expect(r.Get(ctx, types.NamespacedName{
			Namespace: "compute", Name: target.certName,
		}, cert)).To(Succeed())

		// Nothing may be left in the ControlPlane's own namespace.
		home := types.NamespacedName{Namespace: "default", Name: target.secretName}
		g.Expect(r.Get(ctx, home, &esov1.ExternalSecret{})).NotTo(Succeed())
		g.Expect(r.Get(ctx, home, &esgenv1alpha1.VaultDynamicSecret{})).NotTo(Succeed())
	}
}
