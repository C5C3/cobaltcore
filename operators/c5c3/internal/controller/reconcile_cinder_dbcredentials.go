// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The Cinder DB-credential concern mirrors the Keystone one in
// reconcile_dbcredentials.go, cinder-scoped and keystone-independent: Dynamic
// (engine-issued) is the default on a managed SHARED database, and every dynamic
// object (the ServiceAccount, the mTLS client Certificate, the VaultDynamicSecret
// generator, and the generator-backed ExternalSecret) lands in the CINDER service
// namespace, beside the database it issues against and the child that consumes
// it.
//
// Only the diverging inputs live here: the names, the OpenBao role and paths, and
// the mode predicate. The object builders and the ensure/delete pair are the
// service-agnostic ones in reconcile_dbcredentials.go, driven by the
// dbCredentialTarget this file constructs.

const (
	// cinderDBDynamicVaultRole is the OpenBao Kubernetes-auth role the Cinder
	// generator authenticates against. Its counterpart is the role
	// deploy/openbao/bootstrap/setup-auth.sh writes, bound to the cinder-db-dynamic
	// policy that scopes reads to the per-tenant creds path.
	cinderDBDynamicVaultRole = "cinder-db"
	// cinderDBCredentialServiceAccountName is the fixed name of the
	// per-ControlPlane ServiceAccount whose token the Cinder VaultDynamicSecret
	// generator presents to OpenBao. It is the name the cinder-db role binds
	// (bound_service_account_names, setup-auth.sh), so the two MUST STAY IN SYNC. A
	// fixed name is safe because a namespace belongs to at most one ControlPlane:
	// the one-ControlPlane-per-namespace webhook guarantees it for the
	// ControlPlane's own namespace, and the namespace-claim webhook guarantees it
	// for every service namespace.
	cinderDBCredentialServiceAccountName = "cinder-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
)

// cinderDBCredentialSecretName returns the deterministic name of the
// per-ControlPlane Cinder DB-credential Secret/ExternalSecret. It tracks the
// projected Cinder CR, mirroring dbCredentialSecretName's derivation from the
// Keystone child.
func cinderDBCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return cinderName(cp) + dbCredentialSecretNameSuffix
}

// cinderDBCredentialClientCertName returns the name of the per-ControlPlane
// cert-manager Certificate and the Secret it materialises (client mTLS keypair
// plus the CA under ca.crt) for the Cinder generator, mirroring
// dbCredentialClientCertName.
func cinderDBCredentialClientCertName(cp *c5c3v1alpha1.ControlPlane) string {
	return cinderName(cp) + dbCredentialClientCertSuffix
}

// cinderDBCredentialRemoteKeyFor returns the per-ControlPlane, namespace-scoped
// OpenBao KV path the STATIC Cinder DB credential is read from (keys username,
// password). It is retained only for the Static opt-out / brownfield-migration
// path; the default managed mode reads engine-issued credentials from
// cinderDBDynamicCredsPathFor instead. The eso-tenant.hcl policy already grants
// this path shape; nothing seeds it, so a ControlPlane on the Static branch must
// have the path seeded out-of-band before ESO can sync the credential.
func cinderDBCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/cinder/" + cp.CinderNamespace() + "/" + cp.Name + "/db"
}

// cinderDBDynamicRoleFor returns the per-tenant OpenBao database-engine role name
// for this ControlPlane's Cinder service. It is keyed on the CINDER SERVICE
// NAMESPACE alone (the namespace the database and the generator that reads from
// it actually live in), so it is collision-free (namespaces are cluster-unique)
// and the cinder-db-dynamic policy can scope reads by the caller's
// service_account_namespace with an EXACT match. It MUST stay in sync with the
// role-name derivation in deploy/openbao/bootstrap/setup-database-tenant.sh: the
// operator reads credentials from the engine role that script provisions per
// service, which for Cinder is cinder-<cinder-ns>. The engine pair is written per
// ControlPlane onboarding, the cinder-db auth role and the cinder-db-dynamic
// policy cluster-wide.
func cinderDBDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "cinder-" + cp.CinderNamespace()
}

// cinderDBDynamicCredsPathFor returns the OpenBao path the Cinder
// VaultDynamicSecret generator reads short-lived credentials from
// (database/mariadb/creds/<role>).
func cinderDBDynamicCredsPathFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "database/mariadb/creds/" + cinderDBDynamicRoleFor(cp)
}

// cinderDBCredentialTarget describes the Cinder DB-credential concern for the
// service-agnostic builders and the ensure/delete pair in
// reconcile_dbcredentials.go.
func cinderDBCredentialTarget(cp *c5c3v1alpha1.ControlPlane) dbCredentialTarget {
	return dbCredentialTarget{
		qualifier:  "Cinder",
		namespace:  cp.CinderNamespace(),
		secretName: cinderDBCredentialSecretName(cp),
		certName:   cinderDBCredentialClientCertName(cp),
		saName:     cinderDBCredentialServiceAccountName,
		vaultRole:  cinderDBDynamicVaultRole,
		credsPath:  cinderDBDynamicCredsPathFor(cp),
		kvPath:     cinderDBCredentialRemoteKeyFor(cp),
		storeRef:   effectiveControlPlaneStoreRef(cp),
	}
}

// cinderDBCredentialsDynamicEnabled reports the effective credentials mode of the
// database Cinder actually connects to: Dynamic (engine-issued) is the default
// for a managed SHARED database; a ControlPlane opts out by setting
// credentialsMode: Static (migration staging / brownfield). A non-empty
// per-service services.cinder.databaseCredentialsMode override wins over the
// shared credentialsMode, so a staged migration can run Cinder on one mode while
// another service stays on the other.
//
// A DEDICATED cinder database is never Dynamic. The OpenBao database engine
// carries one connection and one role per NAMESPACE bootstrapped against the
// SHARED cluster, so no engine role exists that could issue credentials for a
// dedicated instance: it takes the Static branch. Cinder is never External-mode
// (services.cinder is forbidden in External ControlPlanes), so no External
// short-circuit is needed here; keying the decision on the dedicated declaration
// rather than only on the stored mode keeps a webhook-bypassed CR failing closed
// onto Static rather than projecting a generator that could never sync.
func cinderDBCredentialsDynamicEnabled(cp *c5c3v1alpha1.ControlPlane) bool {
	var override string
	if cd := cp.Spec.Services.Cinder; cd != nil {
		override = cd.DatabaseCredentialsMode
	}
	return dbCredentialModeIsDynamic(cp.DedicatedCinderDatabase(), effectiveCinderDatabase(cp), override)
}
