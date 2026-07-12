# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# eso-tenant policy — the per-ControlPlane ESO identity that gives each tenant
# its OWN OpenBao token, so OpenBao itself (not a naming convention) enforces
# that one tenant's ESO cannot read or write another tenant's Keystone key
# material. Bound to the "eso-tenant" role on the kubernetes/management auth
# mount (see setup-auth.sh), which the per-tenant namespaced SecretStore created
# by setup-eso-tenant.sh authenticates against with the tenant namespace's
# eso-tenant-auth ServiceAccount.
#
# TENANT ISOLATION: every path below is scoped by OpenBao ACL identity templating
# to the caller's OWN service-account namespace —
# {{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}
# resolves, at request time, to the namespace of the eso-tenant-auth SA token
# that authenticated. Because the project enforces one ControlPlane per
# namespace, the namespace is a unique, collision-free tenant key (the same
# argument keystone-db-dynamic.hcl makes). The eso-tenant role deliberately keeps
# bound_service_account_namespaces="*" (any tenant namespace may authenticate);
# it is this templated policy — not the role binding — that enforces the
# cross-tenant boundary. A token minted in namespace A can therefore read and
# write only openstack/keystone/A/... and bootstrap/A/..., and CANNOT touch
# another tenant's B paths. This is the per-tenant identity #435/#439 deferred to
# #605 as "per-tenant OpenBao identities via templated policies + namespaced
# SecretStore".
#
# The KUBERNETES_MANAGEMENT_ACCESSOR placeholder is substituted with the live
# kubernetes/management auth-mount accessor at apply time by setup-policies.sh —
# the accessor is generated when the mount is enabled (setup-auth.sh, run first)
# and is not known until runtime.
#
# NO infrastructure/* grant: the static ExternalSecret manifests under
# deploy/kind/infrastructure/ deliberately keep reading through the shared
# cluster store, so the per-tenant identity is scoped strictly to the Keystone
# key/bootstrap material a ControlPlane owns.

# --- read: the tenant's own Keystone and bootstrap subtrees ---
# Covers the keystone-db ExternalSecret (openstack/keystone/{ns}/db), the
# keystone-admin ExternalSecret (bootstrap/{ns}/{name}/admin), and the read-back
# leg of every PushSecret below.
path "kv-v2/data/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/*" {
  capabilities = ["read"]
}

path "kv-v2/data/bootstrap/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/*" {
  capabilities = ["read"]
}

# --- fernet-keys / credential-keys backup (mirrors push-keystone-keys.hcl) ---
# ESO writes both the data and metadata endpoints on every KV-v2 PushSecret
# (custom_metadata: managed-by=external-secrets), and the openbao-finalizer
# drives DeletionPolicy=Delete (soft-delete on data, hard-delete on metadata),
# so create/update/read/delete on both is required. The single {{ns}}/+ glob
# ends at the literal /fernet-keys or /credential-keys leaf, so the read-only
# openstack/keystone/{ns}/db secret (a flat leaf) stays unwritable.
path "kv-v2/data/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/fernet-keys" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/metadata/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/fernet-keys" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/data/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/credential-keys" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/metadata/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/credential-keys" {
  capabilities = ["create", "update", "read", "delete"]
}

# --- admin bootstrap password backup (mirrors push-keystone-admin.hcl) ---
# adminPasswordPushSecret uses DeletionPolicy=None, so delete is never exercised;
# it is retained for policy portability and consistency with push-keystone-keys,
# adding no blast radius beyond this per-CR bootstrap/{ns}/{name}/admin leaf.
path "kv-v2/data/bootstrap/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/admin" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/metadata/bootstrap/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/admin" {
  capabilities = ["create", "update", "read", "delete"]
}

# --- admin application credential + service-account backups ---
# Mirrors push-app-credentials.hcl verbatim, INCLUDING its metadata-path
# asymmetry: the metadata paths grant create/update/read but NOT delete. That
# asymmetry is pre-existing on main (push-app-credentials.hcl) and is reproduced
# here, not fixed — the admin-AC / service-account PushSecrets run
# DeletionPolicy=Delete and ESO's KV-v2 delete only DELETEs the data path.
path "kv-v2/data/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/admin/app-credential" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/metadata/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/admin/app-credential" {
  capabilities = ["create", "update", "read"]
}

path "kv-v2/data/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/service-accounts/+" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/metadata/openstack/keystone/{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}/+/service-accounts/+" {
  capabilities = ["create", "update", "read"]
}
