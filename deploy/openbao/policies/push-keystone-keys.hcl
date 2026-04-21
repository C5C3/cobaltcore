# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# PushSecret policy for Keystone fernet and credential keys — allows
# PushSecret CRs to back up the rotated fernet-keys and credential-keys
# Secrets to OpenBao so the key material is recoverable after a cluster
# rebuild, operator replay, or accidental Secret deletion.
#
# Bound additionally (alongside eso-management) to the management cluster's
# eso-management Kubernetes auth role; eso-management itself stays read-only.
# Pattern source: deploy/openbao/policies/push-app-credentials.hcl.
#
# Scope is intentionally restricted to two exact literal paths and does
# NOT use a wildcard over the whole Keystone subtree, because that
# subtree also holds the read-only MariaDB credentials consumed by
# Keystone; granting write over a wildcard would let a compromised ESO
# controller overwrite those credentials and lock Keystone out of its
# own database.
#
# The `read` capability on each path is retained for policy-portability
# and to match the convention in push-app-credentials.hcl. At the current
# binding it is redundant: the management cluster's ESO role also binds
# eso-management.hcl, whose `kv-v2/data/openstack/keystone/*` wildcard
# already grants read on these two paths. If this policy is later bound
# elsewhere (another cluster, a different role) without eso-management,
# the explicit `read` keeps PushSecret pre-flight reads working.
#
# ESO's Vault/OpenBao provider writes to BOTH the data and the metadata
# endpoint on every PushSecret for KV v2: it stamps `custom_metadata:
# managed-by=external-secrets` on the metadata path so DeleteSecret can
# later verify ownership (providers/v1/vault/client_push.go:149-156).
# The matching metadata paths must therefore grant `create`/`update` as
# well — the data-only grant leads to a 403 on the metadata PUT and the
# PushSecret never reaches Ready.
# Feature: CC-0083

path "kv-v2/data/openstack/keystone/fernet-keys" {
  capabilities = ["create", "update", "read"]
}

path "kv-v2/metadata/openstack/keystone/fernet-keys" {
  capabilities = ["create", "update", "read"]
}

path "kv-v2/data/openstack/keystone/credential-keys" {
  capabilities = ["create", "update", "read"]
}

path "kv-v2/metadata/openstack/keystone/credential-keys" {
  capabilities = ["create", "update", "read"]
}
