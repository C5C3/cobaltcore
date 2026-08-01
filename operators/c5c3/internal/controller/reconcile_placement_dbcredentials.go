// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// The Placement DB-credential concern mirrors the Keystone one in
// reconcile_dbcredentials.go, placement-scoped and keystone-independent: Dynamic
// (engine-issued) is the default on a managed SHARED database, and every dynamic
// object — the ServiceAccount, the mTLS client Certificate, the VaultDynamicSecret
// generator, and the generator-backed ExternalSecret — lands in the PLACEMENT
// service namespace, beside the database it issues against and the child that
// consumes it.
//
// Only the diverging inputs live here: the names, the OpenBao role and paths, and
// the mode predicate. The object builders and the ensure/delete pair are the
// service-agnostic ones in reconcile_dbcredentials.go, driven by the
// dbCredentialTarget this file constructs.

const (
	// placementDBDynamicVaultRole is the OpenBao Kubernetes-auth role the Placement
	// generator authenticates against (see deploy/openbao/bootstrap/setup-auth.sh);
	// it is bound to the placement-db-dynamic policy scoping reads to the per-tenant
	// creds path.
	placementDBDynamicVaultRole = "placement-db"
	// placementDBCredentialServiceAccountName is the fixed name of the
	// per-ControlPlane ServiceAccount whose token the Placement VaultDynamicSecret
	// generator presents to OpenBao. It is the name the placement-db role binds
	// (bound_service_account_names, setup-auth.sh), so the two MUST STAY IN SYNC. A
	// fixed name is safe because a namespace belongs to at most one ControlPlane:
	// the one-ControlPlane-per-namespace webhook guarantees it for the
	// ControlPlane's own namespace, and the namespace-claim webhook guarantees it
	// for every service namespace.
	placementDBCredentialServiceAccountName = "placement-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
)

// placementDBCredentialSecretName returns the deterministic name of the
// per-ControlPlane Placement DB-credential Secret/ExternalSecret. It tracks the
// projected Placement CR, mirroring dbCredentialSecretName's derivation from the
// Keystone child.
func placementDBCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return placementName(cp) + dbCredentialSecretNameSuffix
}

// placementDBCredentialClientCertName returns the name of the per-ControlPlane
// cert-manager Certificate and the Secret it materialises (client mTLS keypair
// plus the CA under ca.crt) for the Placement generator, mirroring
// dbCredentialClientCertName.
func placementDBCredentialClientCertName(cp *c5c3v1alpha1.ControlPlane) string {
	return placementName(cp) + dbCredentialClientCertSuffix
}

// placementDBCredentialRemoteKeyFor returns the per-ControlPlane,
// namespace-scoped OpenBao KV path the STATIC Placement DB credential is read from
// (keys username, password). It is retained only for the Static opt-out /
// brownfield-migration path; the default managed mode reads engine-issued
// credentials from placementDBDynamicCredsPathFor instead. The eso-tenant.hcl
// policy already grants this path shape; nothing seeds it, so a ControlPlane on
// the Static branch must have the path seeded out-of-band before ESO can sync the
// credential.
func placementDBCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/placement/" + cp.PlacementNamespace() + "/" + cp.Name + "/db"
}

// placementDBDynamicRoleFor returns the per-tenant OpenBao database-engine role
// name for this ControlPlane's Placement service. It is keyed on the PLACEMENT
// SERVICE NAMESPACE alone — the namespace the database and the generator that
// reads from it actually live in — so it is collision-free (namespaces are
// cluster-unique) and the placement-db-dynamic policy can scope reads by the
// caller's service_account_namespace with an EXACT match. It MUST stay in sync
// with the role-name derivation in
// deploy/openbao/bootstrap/setup-database-tenant.sh: the operator reads
// credentials from the engine role that script provisions per service, which for
// Placement is placement-<placement-ns>. The auth half (the placement-db role and
// the placement-db-dynamic policy) is already bootstrapped; the engine branch of
// setup-database-tenant.sh is what makes the path resolve.
func placementDBDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "placement-" + cp.PlacementNamespace()
}

// placementDBDynamicCredsPathFor returns the OpenBao path the Placement
// VaultDynamicSecret generator reads short-lived credentials from
// (database/mariadb/creds/<role>).
func placementDBDynamicCredsPathFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "database/mariadb/creds/" + placementDBDynamicRoleFor(cp)
}

// placementDBCredentialTarget describes the Placement DB-credential concern for
// the service-agnostic builders and the ensure/delete pair in
// reconcile_dbcredentials.go.
func placementDBCredentialTarget(cp *c5c3v1alpha1.ControlPlane) dbCredentialTarget {
	return dbCredentialTarget{
		qualifier:  "Placement ",
		namespace:  cp.PlacementNamespace(),
		secretName: placementDBCredentialSecretName(cp),
		certName:   placementDBCredentialClientCertName(cp),
		saName:     placementDBCredentialServiceAccountName,
		vaultRole:  placementDBDynamicVaultRole,
		credsPath:  placementDBDynamicCredsPathFor(cp),
		kvPath:     placementDBCredentialRemoteKeyFor(cp),
		storeRef:   effectiveControlPlaneStoreRef(cp),
	}
}

// placementDBCredentialsDynamicEnabled reports the effective credentials mode of
// the database Placement actually connects to: Dynamic (engine-issued) is the
// default for a managed SHARED database; a ControlPlane opts out by setting
// credentialsMode: Static (migration staging / brownfield). A non-empty
// per-service services.placement.databaseCredentialsMode override wins over the
// shared credentialsMode, so a staged migration can run Placement on one mode
// while another service stays on the other.
//
// A DEDICATED placement database is never Dynamic. The OpenBao database engine
// carries one connection and one role per NAMESPACE bootstrapped against the
// SHARED cluster, so no engine role exists that could issue credentials for a
// dedicated instance — it takes the Static branch. Placement is never
// External-mode (services.placement is forbidden in External ControlPlanes), so
// no External short-circuit is needed here; keying the decision on the dedicated
// declaration rather than only on the stored mode keeps a webhook-bypassed CR
// failing closed onto Static rather than projecting a generator that could never
// sync.
func placementDBCredentialsDynamicEnabled(cp *c5c3v1alpha1.ControlPlane) bool {
	var override string
	if pl := cp.Spec.Services.Placement; pl != nil {
		override = pl.DatabaseCredentialsMode
	}
	return dbCredentialModeIsDynamic(cp.DedicatedPlacementDatabase(), effectivePlacementDatabase(cp), override)
}
