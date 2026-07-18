// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// The Glance DB-credential concern mirrors the Keystone one in
// reconcile_dbcredentials.go, glance-scoped and keystone-independent: Dynamic
// (engine-issued) is the default on a managed SHARED database, and every dynamic
// object — the ServiceAccount, the mTLS client Certificate, the VaultDynamicSecret
// generator, and the generator-backed ExternalSecret — lands in the GLANCE service
// namespace, beside the database it issues against and the child that consumes it.
//
// Only the diverging inputs live here: the names, the OpenBao role and paths, and
// the mode predicate. The object builders and the ensure/delete pair are the
// service-agnostic ones in reconcile_dbcredentials.go, driven by the
// dbCredentialTarget this file constructs.

const (
	// glanceDBDynamicVaultRole is the OpenBao Kubernetes-auth role the Glance
	// generator authenticates against (see deploy/openbao/bootstrap/setup-auth.sh);
	// it is bound to the glance-db-dynamic policy scoping reads to the per-tenant
	// creds path.
	glanceDBDynamicVaultRole = "glance-db"
	// glanceDBCredentialServiceAccountName is the fixed name of the per-ControlPlane
	// ServiceAccount whose token the Glance VaultDynamicSecret generator presents to
	// OpenBao. A fixed name is safe because a namespace belongs to at most one
	// ControlPlane: the one-ControlPlane-per-namespace webhook guarantees it for the
	// ControlPlane's own namespace, and the namespace-claim webhook guarantees it
	// for every service namespace.
	glanceDBCredentialServiceAccountName = "glance-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
)

// glanceDBCredentialSecretName returns the deterministic name of the
// per-ControlPlane Glance DB-credential Secret/ExternalSecret. It tracks the
// projected Glance CR, mirroring dbCredentialSecretName's derivation from the
// Keystone child.
func glanceDBCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return glanceName(cp) + dbCredentialSecretNameSuffix
}

// glanceDBCredentialClientCertName returns the name of the per-ControlPlane
// cert-manager Certificate and the Secret it materialises (client mTLS keypair
// plus the CA under ca.crt) for the Glance generator, mirroring
// dbCredentialClientCertName.
func glanceDBCredentialClientCertName(cp *c5c3v1alpha1.ControlPlane) string {
	return glanceName(cp) + dbCredentialClientCertSuffix
}

// glanceDBCredentialRemoteKeyFor returns the per-ControlPlane, namespace-scoped
// OpenBao KV path the STATIC Glance DB credential is read from (keys username,
// password). It is retained only for the Static opt-out / brownfield-migration
// path; the default managed mode reads engine-issued credentials from
// glanceDBDynamicCredsPathFor instead. The eso-tenant.hcl policy already grants
// this path shape; nothing seeds it, so a ControlPlane on the Static branch must
// have the path seeded out-of-band before ESO can sync the credential.
func glanceDBCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/glance/" + cp.GlanceNamespace() + "/" + cp.Name + "/db"
}

// glanceDBDynamicRoleFor returns the per-tenant OpenBao database-engine role name
// for this ControlPlane's Glance service. It is keyed on the GLANCE SERVICE
// NAMESPACE alone — the namespace the database and the generator that reads from
// it actually live in — so it is collision-free (namespaces are cluster-unique)
// and the glance-db-dynamic policy can scope reads by the caller's
// service_account_namespace with an EXACT match. It MUST stay in sync with the
// role-name derivation in deploy/openbao/bootstrap/setup-database-tenant.sh —
// the operator reads credentials from a role that script provisions as
// glance-<glance-ns>.
func glanceDBDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "glance-" + cp.GlanceNamespace()
}

// glanceDBDynamicCredsPathFor returns the OpenBao path the Glance
// VaultDynamicSecret generator reads short-lived credentials from
// (database/mariadb/creds/<role>).
func glanceDBDynamicCredsPathFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "database/mariadb/creds/" + glanceDBDynamicRoleFor(cp)
}

// glanceDBCredentialTarget describes the Glance DB-credential concern for the
// service-agnostic builders and the ensure/delete pair in
// reconcile_dbcredentials.go.
func glanceDBCredentialTarget(cp *c5c3v1alpha1.ControlPlane) dbCredentialTarget {
	return dbCredentialTarget{
		qualifier:  "Glance ",
		namespace:  cp.GlanceNamespace(),
		secretName: glanceDBCredentialSecretName(cp),
		certName:   glanceDBCredentialClientCertName(cp),
		saName:     glanceDBCredentialServiceAccountName,
		vaultRole:  glanceDBDynamicVaultRole,
		credsPath:  glanceDBDynamicCredsPathFor(cp),
		kvPath:     glanceDBCredentialRemoteKeyFor(cp),
		storeRef:   effectiveControlPlaneStoreRef(cp),
	}
}

// glanceDBCredentialsDynamicEnabled reports the effective credentials mode of the
// database Glance actually connects to: Dynamic (engine-issued) is the default
// for a managed SHARED database; a ControlPlane opts out by setting
// credentialsMode: Static (migration staging / brownfield). A non-empty
// per-service services.glance.databaseCredentialsMode override wins over the
// shared credentialsMode, so a staged migration can run Glance on one mode while
// another service stays on the other.
//
// A DEDICATED glance database is never Dynamic. The OpenBao database engine
// carries one connection and one role per NAMESPACE bootstrapped against the
// SHARED cluster, so no engine role exists that could issue credentials for a
// dedicated instance — it takes the Static branch. Glance is never External-mode
// (services.glance is forbidden in External ControlPlanes), so no External
// short-circuit is needed here; keying the decision on the dedicated declaration
// rather than only on the stored mode keeps a webhook-bypassed CR failing closed
// onto Static rather than projecting a generator that could never sync.
func glanceDBCredentialsDynamicEnabled(cp *c5c3v1alpha1.ControlPlane) bool {
	var override string
	if gl := cp.Spec.Services.Glance; gl != nil {
		override = gl.DatabaseCredentialsMode
	}
	return dbCredentialModeIsDynamic(cp.DedicatedGlanceDatabase(), effectiveGlanceDatabase(cp), override)
}
