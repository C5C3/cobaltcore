# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# PushSecret policy for the Keystone admin bootstrap credential — allows the
# management cluster's ESO PushSecret to write the operator-rotated admin
# password back to OpenBao at the shared bootstrap path so the keystone-admin
# ExternalSecret can sync it and Part-1 re-bootstrap can adopt it (Model B,
# scheduled admin-password rotation).
#
# Bound additionally (alongside eso-management and push-keystone-keys) to the
# management cluster's eso-management Kubernetes auth role; eso-management
# itself stays read-only. The separation preserves the audit invariant that a
# leaked management-cluster ESO token on eso-management alone cannot write to
# OpenBao — write capability lives only in this narrowly-scoped policy
# (CC-0083). Pattern source: deploy/openbao/policies/push-keystone-keys.hcl.
#
# Scope — boundary 8, option (a): a single FLAT, literal path. The production
# sink for this credential is OpenBao itself (not an in-cluster Secret): a
# value written here round-trips through ESO into the keystone-admin Secret
# and drives a cluster-wide admin re-bootstrap. The grant is therefore the two
# exact leaves below and deliberately does NOT use a trailing `*` wildcard or
# a `+` single-segment glob over the bootstrap subtree. eso-management already
# grants read on `kv-v2/data/bootstrap/*` (every bootstrap secret, including
# this one); a write wildcard here would let a compromised ESO controller
# overwrite ANY other bootstrap secret (e.g. database root credentials),
# escalating a single-credential rotation grant into write access over the
# whole bootstrap subtree. Because the path is cluster-global, at most one
# Model-B-enabled Keystone CR may exist per cluster (documented precondition).
#
# ESO's Vault/OpenBao provider writes to BOTH the data and the metadata
# endpoint on every PushSecret for KV v2: it stamps `custom_metadata:
# managed-by=external-secrets` on the metadata path so DeleteSecret can later
# verify ownership (providers/v1/vault/client_push.go:149-156). The matching
# metadata path must therefore grant `create`/`update` as well — a data-only
# grant leads to a 403 on the metadata PUT and the PushSecret never reaches
# Ready.
#
# `delete` is retained on both the data and metadata paths for policy
# portability and consistency with push-keystone-keys.hcl, NOT because this
# PushSecret exercises it: adminPasswordPushSecret sets DeletionPolicy=None
# (operators/keystone/internal/controller/reconcile_passwordrotation.go), so ESO
# never issues a DELETE against OpenBao when the PushSecret is torn down — the
# last-pushed admin password is deliberately left intact at
# bootstrap/keystone-admin so disabling rotation can never lock the admin out.
# The capability is kept (rather than dropped) so re-binding this policy under a
# future DeletionPolicy=Delete would not silently 403 on teardown; because the
# grant stays scoped to the two exact admin leaves, `delete` adds no blast
# radius beyond the very credential this policy already lets the holder write.
#
# `read` is retained for PushSecret pre-flight reads and policy-portability,
# matching the convention in push-keystone-keys.hcl.
# Feature: CC-0109
path "kv-v2/data/bootstrap/keystone-admin" {
  capabilities = ["create", "update", "read", "delete"]
}

path "kv-v2/metadata/bootstrap/keystone-admin" {
  capabilities = ["create", "update", "read", "delete"]
}
