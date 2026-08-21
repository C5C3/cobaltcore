// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The Barbican DB-credential concern mirrors the Keystone one in
// reconcile_dbcredentials.go, barbican-scoped and keystone-independent: Dynamic
// (engine-issued) is the default on a managed SHARED database, and every dynamic
// object — the ServiceAccount, the mTLS client Certificate, the VaultDynamicSecret
// generator, and the generator-backed ExternalSecret — lands in the BARBICAN
// service namespace, beside the database it issues against and the child that
// consumes it.
//
// Only the diverging inputs live here: the names, the OpenBao role and paths, and
// the mode predicate. The object builders and the ensure/delete pair are the
// service-agnostic ones in reconcile_dbcredentials.go, driven by the
// dbCredentialTarget this file constructs.

const (
	// barbicanDBDynamicVaultRole is the OpenBao Kubernetes-auth role the Barbican
	// generator authenticates against (see deploy/openbao/bootstrap/setup-auth.sh);
	// it is bound to the barbican-db-dynamic policy scoping reads to the per-tenant
	// creds path.
	barbicanDBDynamicVaultRole = "barbican-db"
	// barbicanDBCredentialServiceAccountName is the fixed name of the
	// per-ControlPlane ServiceAccount whose token the Barbican VaultDynamicSecret
	// generator presents to OpenBao. It is the name the barbican-db role binds
	// (bound_service_account_names, setup-auth.sh), so the two MUST STAY IN SYNC. A
	// fixed name is safe because a namespace belongs to at most one ControlPlane:
	// the one-ControlPlane-per-namespace webhook guarantees it for the
	// ControlPlane's own namespace, and the namespace-claim webhook guarantees it
	// for every service namespace.
	barbicanDBCredentialServiceAccountName = "barbican-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
)

// barbicanDBCredentialSecretName returns the deterministic name of the
// per-ControlPlane Barbican DB-credential Secret/ExternalSecret. It tracks the
// projected Barbican CR, mirroring dbCredentialSecretName's derivation from the
// Keystone child.
func barbicanDBCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return barbicanName(cp) + dbCredentialSecretNameSuffix
}

// barbicanDBCredentialClientCertName returns the name of the per-ControlPlane
// cert-manager Certificate and the Secret it materialises (client mTLS keypair
// plus the CA under ca.crt) for the Barbican generator, mirroring
// dbCredentialClientCertName.
func barbicanDBCredentialClientCertName(cp *c5c3v1alpha1.ControlPlane) string {
	return barbicanName(cp) + dbCredentialClientCertSuffix
}

// barbicanDBCredentialRemoteKeyFor returns the per-ControlPlane, namespace-scoped
// OpenBao KV path the STATIC Barbican DB credential is read from (keys username,
// password). It is retained only for the Static opt-out / brownfield-migration
// path; the default managed mode reads engine-issued credentials from
// barbicanDBDynamicCredsPathFor instead. The eso-tenant.hcl policy already grants
// this path shape; nothing seeds it, so a ControlPlane on the Static branch must
// have the path seeded out-of-band before ESO can sync the credential.
func barbicanDBCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/barbican/" + cp.BarbicanNamespace() + "/" + cp.Name + "/db"
}

// barbicanDBDynamicRoleFor returns the per-tenant OpenBao database-engine role
// name for this ControlPlane's Barbican service. It is keyed on the BARBICAN
// SERVICE NAMESPACE alone — the namespace the database and the generator that
// reads from it actually live in — so it is collision-free (namespaces are
// cluster-unique) and the barbican-db-dynamic policy can scope reads by the
// caller's service_account_namespace with an EXACT match. It MUST stay in sync
// with the role-name derivation in
// deploy/openbao/bootstrap/setup-database-tenant.sh: the operator reads
// credentials from the engine role that script provisions per service, which for
// Barbican is barbican-<barbican-ns>. Both OpenBao halves are pre-wired — the
// barbican-db auth role and the barbican-db-dynamic policy cluster-wide, the
// engine pair per ControlPlane onboarding.
func barbicanDBDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "barbican-" + cp.BarbicanNamespace()
}

// barbicanDBDynamicCredsPathFor returns the OpenBao path the Barbican
// VaultDynamicSecret generator reads short-lived credentials from
// (database/mariadb/creds/<role>).
func barbicanDBDynamicCredsPathFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "database/mariadb/creds/" + barbicanDBDynamicRoleFor(cp)
}

// barbicanDBCredentialTarget describes the Barbican DB-credential concern for the
// service-agnostic builders and the ensure/delete pair in
// reconcile_dbcredentials.go.
func barbicanDBCredentialTarget(cp *c5c3v1alpha1.ControlPlane) dbCredentialTarget {
	return dbCredentialTarget{
		qualifier:  "Barbican",
		namespace:  cp.BarbicanNamespace(),
		secretName: barbicanDBCredentialSecretName(cp),
		certName:   barbicanDBCredentialClientCertName(cp),
		saName:     barbicanDBCredentialServiceAccountName,
		vaultRole:  barbicanDBDynamicVaultRole,
		credsPath:  barbicanDBDynamicCredsPathFor(cp),
		kvPath:     barbicanDBCredentialRemoteKeyFor(cp),
		storeRef:   effectiveControlPlaneStoreRef(cp),
	}
}

// barbicanDBCredentialsDynamicEnabled reports the effective credentials mode of
// the database Barbican actually connects to: Dynamic (engine-issued) is the
// default for a managed SHARED database; a ControlPlane opts out by setting
// credentialsMode: Static (migration staging / brownfield). A non-empty
// per-service services.barbican.databaseCredentialsMode override wins over the
// shared credentialsMode, so a staged migration can run Barbican on one mode while
// another service stays on the other.
//
// A DEDICATED barbican database is never Dynamic. The OpenBao database engine
// carries one connection and one role per NAMESPACE bootstrapped against the
// SHARED cluster, so no engine role exists that could issue credentials for a
// dedicated instance — it takes the Static branch. Barbican is never External-mode
// (services.barbican is forbidden in External ControlPlanes), so no External
// short-circuit is needed here; keying the decision on the dedicated declaration
// rather than only on the stored mode keeps a webhook-bypassed CR failing closed
// onto Static rather than projecting a generator that could never sync.
func barbicanDBCredentialsDynamicEnabled(cp *c5c3v1alpha1.ControlPlane) bool {
	var override string
	if bn := cp.Spec.Services.Barbican; bn != nil {
		override = bn.DatabaseCredentialsMode
	}
	return dbCredentialModeIsDynamic(cp.DedicatedBarbicanDatabase(), effectiveBarbicanDatabase(cp), override)
}
