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
# Per-CR path layout: the operator writes each Keystone CR's fernet-keys and
# credential-keys to a CR-scoped KV-v2 path. The original CC-0093 layout used a
# single CR-name segment — `openstack/keystone/{keystone.Name}/fernet-keys`
# (and …/credential-keys) — behind one `+` glob. As of CC-0112 the path is
# additionally scoped by namespace, shaped
# `openstack/keystone/{namespace}/{name}/fernet-keys` (namespace then name); the
# policy therefore uses a two-segment `+/+` glob, the added segment (CC-0112)
# being the CR's namespace ahead of the existing name segment. OpenBao ACL
# syntax supports `+` as "any number of characters bounded within a single path
# segment" (verbatim, upstream docs at https://openbao.org/docs/concepts/policies
# — "OpenBao > Architecture > Policies > Path Syntax"), and allows `+` between
# literal segments: the canonical upstream example `path "secret/+/teamb"`
# matches `secret/foo/teamb` but not `secret/foo/bar/teamb`. The patterns below
# apply the same shape: `kv-v2/data/openstack/keystone/+/+/fernet-keys` matches
# exactly the namespace and name segments followed by the literal leaf.
#
# Scope is still intentionally restricted to the two leaf kinds
# (fernet-keys, credential-keys) and does NOT use a trailing `*` wildcard
# over the whole Keystone subtree, because that subtree also holds the
# read-only MariaDB credentials consumed by Keystone at
# `kv-v2/data/openstack/keystone/{namespace}/db`; granting write over a
# trailing-star wildcard would let a compromised ESO controller overwrite
# those credentials and lock Keystone out of its own database.
#
# The read-only MariaDB credentials at `kv-v2/data/openstack/keystone/{namespace}/db`
# remain unwritable: `db` is a flat leaf and the patterns above require
# the literal `/fernet-keys` or `/credential-keys` suffix after the two
# `+/+` segments, so the `db` secret itself is not in the match set. A
# holder of this policy could write to the two sibling paths
# `kv-v2/…/keystone/<ns>/db/fernet-keys` and
# `kv-v2/…/keystone/<ns>/db/credential-keys` (via name `= "db"`), but those
# are independent KV-v2 keys and writes to them do not affect the `db`
# secret; the "MariaDB credentials locked out of their own database"
# failure mode a trailing-star wildcard would enable is therefore still
# prevented.
#
# The `read` capability on each path is retained for policy-portability
# and to match the convention in push-app-credentials.hcl. At the current
# binding it is redundant: the management cluster's ESO role also binds
# eso-management.hcl, whose `kv-v2/data/openstack/keystone/*` wildcard
# already grants read on every descendant path — including the new
# per-CR leaves `kv-v2/data/openstack/keystone/{namespace}/{name}/fernet-keys`
# and `kv-v2/data/openstack/keystone/{namespace}/{name}/credential-keys`
# (CC-0112). If this policy is later bound elsewhere (another cluster, a
# different role) without eso-management, the explicit `read` keeps PushSecret
# pre-flight reads working.
#
# ESO's Vault/OpenBao provider writes to BOTH the data and the metadata
# endpoint on every PushSecret for KV v2: it stamps `custom_metadata:
# managed-by=external-secrets` on the metadata path so DeleteSecret can
# later verify ownership (providers/v1/vault/client_push.go:149-156).
# The matching metadata paths must therefore grant `create`/`update` as
# well — the data-only grant leads to a 403 on the metadata PUT and the
# PushSecret never reaches Ready.
# Feature: CC-0083
#
# `delete` is required on both the data and metadata paths because the
# openbao-finalizer drives PushSecret deletion with DeletionPolicy=Delete
# (CC-0079). ESO's DeleteSecret for KV v2 issues DELETE on the data path
# (soft-delete) followed by DELETE on the metadata path (hard-delete);
# without both grants ESO loops on 403 and never clears its cleanup
# finalizer, which would stall the Keystone CR in Terminating forever.
path "kv-v2/data/openstack/keystone/+/+/fernet-keys" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/metadata/openstack/keystone/+/+/fernet-keys" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/data/openstack/keystone/+/+/credential-keys" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/metadata/openstack/keystone/+/+/credential-keys" {
  capabilities = ["create", "update", "read", "delete"]
}
