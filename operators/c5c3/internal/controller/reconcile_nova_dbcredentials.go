// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The Nova DB-credential concern mirrors the Cinder one in
// reconcile_cinder_dbcredentials.go, nova-scoped and keystone-independent:
// Dynamic (engine-issued) is the default on a managed SHARED database, and every
// dynamic object (the ServiceAccount, the mTLS client Certificate, the
// VaultDynamicSecret generator, and the generator-backed ExternalSecret) lands in
// the NOVA service namespace, beside the database it issues against and the child
// that consumes it.
//
// Nova is the first service that takes TWO of them. It splits its state across
// the nova_api schema (the cell map, the flavors, the instance mappings) and the
// nova cell schema (the instances and their migrations), each with its own
// db-sync and its own login, so each gets a credential chain of its own: two
// targets, two OpenBao engine roles, two ServiceAccounts, two Secrets. The mode
// is decided ONCE for both, because the Nova CRD rejects a child whose two
// database blocks carry different credentialsModes.
//
// Only the diverging inputs live here: the names, the OpenBao roles and paths,
// and the mode predicate. The object builders and the ensure/delete pair are the
// service-agnostic ones in reconcile_dbcredentials.go, driven by the
// dbCredentialTargets this file constructs.

const (
	// novaAPIDBDynamicVaultRole is the OpenBao Kubernetes-auth role the Nova API
	// credential generator authenticates against. Its counterpart is the role
	// deploy/openbao/bootstrap/setup-auth.sh writes, bound to the
	// nova-api-db-dynamic policy that scopes reads to the per-tenant creds path.
	novaAPIDBDynamicVaultRole = "nova-api-db"
	// novaCellDBDynamicVaultRole is the same for the cell schema, bound to the
	// nova-cell-db-dynamic policy. The two roles are told apart by the
	// ServiceAccount NAME the token carries, which is what keeps the API
	// generator from reading the cell creds path and the other way round.
	novaCellDBDynamicVaultRole = "nova-cell-db"
	// novaAPIDBCredentialServiceAccountName is the fixed name of the
	// per-ControlPlane ServiceAccount whose token the Nova API VaultDynamicSecret
	// generator presents to OpenBao. It is the name the nova-api-db role binds
	// (bound_service_account_names, setup-auth.sh), so the two MUST STAY IN SYNC.
	// A fixed name is safe because a namespace belongs to at most one
	// ControlPlane: the one-ControlPlane-per-namespace webhook guarantees it for
	// the ControlPlane's own namespace, and the namespace-claim webhook
	// guarantees it for every service namespace.
	novaAPIDBCredentialServiceAccountName = "nova-api-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
	// novaCellDBCredentialServiceAccountName is the same for the cell schema, the
	// name the nova-cell-db role binds. The two accounts are separate objects in
	// one namespace: a token minted for one can never read the other's creds path.
	novaCellDBCredentialServiceAccountName = "nova-cell-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
)

// novaAPIDBCredentialSecretName returns the deterministic name of the
// per-ControlPlane Nova API DB-credential Secret/ExternalSecret
// ("<cp>-nova-api-db-credentials"). It tracks the projected Nova CR with the
// "-api" qualifier the two schemas are told apart by, mirroring the nova
// operator's own naming of its managed API database instance.
func novaAPIDBCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + "-api" + dbCredentialSecretNameSuffix
}

// novaCellDBCredentialSecretName returns the deterministic name of the
// per-ControlPlane Nova CELL DB-credential Secret/ExternalSecret
// ("<cp>-nova-db-credentials"), the unqualified half of the pair.
func novaCellDBCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + dbCredentialSecretNameSuffix
}

// novaAPIDBCredentialClientCertName returns the name of the per-ControlPlane
// cert-manager Certificate and the Secret it materialises (client mTLS keypair
// plus the CA under ca.crt) for the Nova API generator, mirroring
// cinderDBCredentialClientCertName.
func novaAPIDBCredentialClientCertName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + "-api" + dbCredentialClientCertSuffix
}

// novaCellDBCredentialClientCertName returns the same for the cell generator.
// The two generators take a certificate each rather than sharing one, so
// rotating either leaves the other's connection untouched.
func novaCellDBCredentialClientCertName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + dbCredentialClientCertSuffix
}

// novaAPIDBCredentialRemoteKeyFor returns the per-ControlPlane,
// namespace-scoped OpenBao KV path the STATIC Nova API DB credential is read
// from (keys username, password). It is retained only for the Static opt-out /
// brownfield-migration path; the default managed mode reads engine-issued
// credentials from novaAPIDBDynamicCredsPathFor instead. The eso-tenant.hcl
// policy already grants this path shape; nothing seeds it, so a ControlPlane on
// the Static branch must have the path seeded out-of-band before ESO can sync
// the credential.
func novaAPIDBCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/nova/" + cp.NovaNamespace() + "/" + cp.Name + "/api-db"
}

// novaCellDBCredentialRemoteKeyFor returns the same path for the cell schema's
// static credential. The two paths differ in their last element alone, so one
// seeding run covers the pair under one prefix.
func novaCellDBCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/nova/" + cp.NovaNamespace() + "/" + cp.Name + "/db"
}

// novaAPIDBDynamicRoleFor returns the per-tenant OpenBao database-engine role
// name for this ControlPlane's Nova API schema. It is keyed on the NOVA SERVICE
// NAMESPACE alone (the namespace the database and the generator that reads from
// it actually live in), so it is collision-free (namespaces are cluster-unique)
// and the nova-api-db-dynamic policy can scope reads by the caller's
// service_account_namespace with an EXACT match. It MUST stay in sync with the
// role-name derivation in deploy/openbao/bootstrap/setup-database-tenant.sh,
// whose "nova-api nova nova_api" row provisions the engine pair this reads from.
func novaAPIDBDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "nova-api-" + cp.NovaNamespace()
}

// novaCellDBDynamicRoleFor returns the per-tenant engine role name for the cell
// schema, provisioned by the "nova-cell nova nova,nova_cell0" row of the same
// script (the role grants on the cell schema and on its nova_cell0 twin).
//
// The prefix is "nova-cell-" and not "nova-": no role name may be a
// hyphen-prefix of another, because the per-tenant name is <role>-<namespace>. A
// role "nova" in namespace "api-x" and a role "nova-api" in namespace "x" both
// flatten to "nova-api-x" and would overwrite each other's connection config.
func novaCellDBDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "nova-cell-" + cp.NovaNamespace()
}

// novaAPIDBDynamicCredsPathFor returns the OpenBao path the Nova API
// VaultDynamicSecret generator reads short-lived credentials from
// (database/mariadb/creds/<role>).
func novaAPIDBDynamicCredsPathFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "database/mariadb/creds/" + novaAPIDBDynamicRoleFor(cp)
}

// novaCellDBDynamicCredsPathFor returns the same path for the cell generator.
func novaCellDBDynamicCredsPathFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "database/mariadb/creds/" + novaCellDBDynamicRoleFor(cp)
}

// novaAPIDBCredentialTarget describes the Nova API DB-credential concern for the
// service-agnostic builders and the ensure/delete pair in
// reconcile_dbcredentials.go. The qualifier is "NovaAPI" so its condition
// reasons and messages ("WaitingForNovaAPIDBCredential") name which of the two
// chains is waiting.
func novaAPIDBCredentialTarget(cp *c5c3v1alpha1.ControlPlane) dbCredentialTarget {
	return dbCredentialTarget{
		qualifier:  "NovaAPI",
		namespace:  cp.NovaNamespace(),
		secretName: novaAPIDBCredentialSecretName(cp),
		certName:   novaAPIDBCredentialClientCertName(cp),
		saName:     novaAPIDBCredentialServiceAccountName,
		vaultRole:  novaAPIDBDynamicVaultRole,
		credsPath:  novaAPIDBDynamicCredsPathFor(cp),
		kvPath:     novaAPIDBCredentialRemoteKeyFor(cp),
		storeRef:   effectiveControlPlaneStoreRef(cp),
	}
}

// novaCellDBCredentialTarget describes the Nova CELL DB-credential concern, the
// twin of novaAPIDBCredentialTarget under the qualifier "NovaCell". Every field
// differs from the API target's, the store ref apart: two chains sharing an
// object would have one schema's rotation take the other's login down with it.
func novaCellDBCredentialTarget(cp *c5c3v1alpha1.ControlPlane) dbCredentialTarget {
	return dbCredentialTarget{
		qualifier:  "NovaCell",
		namespace:  cp.NovaNamespace(),
		secretName: novaCellDBCredentialSecretName(cp),
		certName:   novaCellDBCredentialClientCertName(cp),
		saName:     novaCellDBCredentialServiceAccountName,
		vaultRole:  novaCellDBDynamicVaultRole,
		credsPath:  novaCellDBDynamicCredsPathFor(cp),
		kvPath:     novaCellDBCredentialRemoteKeyFor(cp),
		storeRef:   effectiveControlPlaneStoreRef(cp),
	}
}

// novaDBCredentialsDynamicEnabled reports the effective credentials mode of the
// databases Nova actually connects to: Dynamic (engine-issued) is the default
// for a managed SHARED database; a ControlPlane opts out by setting
// credentialsMode: Static (migration staging / brownfield). A non-empty
// services.nova.databaseCredentialsMode override wins over the shared
// credentialsMode, so a staged migration can run Nova on one mode while another
// service stays on the other.
//
// ONE verdict covers BOTH schemas. The Nova CRD rejects a child whose apiDatabase
// and database carry different credentialsModes, and both resolve from the same
// effective database, so there is nothing a per-schema predicate could express.
//
// A DEDICATED nova database is never Dynamic. The OpenBao database engine carries
// one connection and one role per NAMESPACE bootstrapped against the SHARED
// cluster, so no engine role exists that could issue credentials for a dedicated
// instance: it takes the Static branch. Keying the decision on the dedicated
// declaration rather than only on the stored mode keeps a webhook-bypassed CR
// failing closed onto Static rather than projecting generators that could never
// sync.
func novaDBCredentialsDynamicEnabled(cp *c5c3v1alpha1.ControlPlane) bool {
	var override string
	if nv := cp.Spec.Services.Nova; nv != nil {
		override = nv.DatabaseCredentialsMode
	}
	return dbCredentialModeIsDynamic(cp.DedicatedNovaDatabase(), effectiveNovaDatabase(cp), override)
}
