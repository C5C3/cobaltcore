# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# PushSecret policy for Application Credentials — allows PushSecret CRs
# to write OpenStack Application Credentials back to OpenBao after
# operators create them via the Keystone API.
# Feature: CC-0009

path "kv-v2/data/openstack/*/app-credential" {
  capabilities = ["create", "update", "read"]
}

# CC-0110, REQ-024 — narrow, single-leaf grant for the c5c3-operator's admin
# Application Credential PushSecret. The operator mints ONE restricted admin AC
# per cluster and mirrors it to the flat path below via the openbao-cluster-store
# (KV-v2). This is a LITERAL leaf, deliberately NOT a trailing "*" or "+" glob:
# the existing kv-v2/data/openstack/*/app-credential grant above matches only a
# SINGLE mid-segment (openstack/<svc>/app-credential) and so does NOT cover the
# two-segment openstack/keystone/admin/app-credential — hence this explicit path
# rather than widening the glob above.
#
# READ is already granted cluster-wide for this subtree by the eso-management
# policy's trailing glob (kv-v2/data/openstack/keystone/* — see eso-management.hcl);
# it is repeated here for policy portability / self-containment.
#
# DECISION (metadata path): the matching kv-v2/metadata/... path is included
# because ESO's Vault provider writes custom_metadata on every KV-v2 PushSecret
# (managed-by=external-secrets, used by DeleteSecret ownership checks). A data-only
# grant 403s on the metadata PUT and the PushSecret never reaches Ready — same
# rationale documented in push-keystone-admin.hcl. The grant stays scoped to the
# single literal admin AC leaf, adding no blast radius beyond this one credential.
# Reviewer: please verify.
path "kv-v2/data/openstack/keystone/admin/app-credential" {
  capabilities = ["create", "update", "read"]
}

path "kv-v2/metadata/openstack/keystone/admin/app-credential" {
  capabilities = ["create", "update", "read"]
}
