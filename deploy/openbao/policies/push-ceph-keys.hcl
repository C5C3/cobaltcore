# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# PushSecret policy for Ceph keys — allows PushSecret CRs to write
# Ceph client keys back to OpenBao after Ceph generates them.
# setup-auth.sh binds it with the push-ceph-keys role on kubernetes/management
# to the ServiceAccount rook-ceph/ceph-keys-push, the identity of the lab
# Ceph's PushSecrets (deploy/lab/metal-stack/ceph/cluster/keys-push.yaml).

path "kv-v2/data/ceph/*" {
  capabilities = ["create", "update", "read"]
}

# ESO writes the custom_metadata of every KV v2 secret it pushes
# (managed-by=external-secrets, see eso-tenant.hcl) and reads it back to check
# that it owns the secret. Without this path the push fails with 403 on the
# metadata write.
path "kv-v2/metadata/ceph/*" {
  capabilities = ["create", "update", "read"]
}
