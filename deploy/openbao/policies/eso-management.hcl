# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Management policy — grants read-only access to bootstrap and
# infrastructure secrets needed by the Management cluster.
# Bound to the ESO ServiceAccount in the Management cluster via
# kubernetes/management auth mount.
# Feature: CC-0009

# CC-0112: the per-CR bootstrap admin password now lives at
# `bootstrap/{namespace}/{name}/admin`. The trailing `*` already matches that
# extra depth, so this read grant covers the new shape with no widening needed.
path "kv-v2/data/bootstrap/*" {
  capabilities = ["read"]
}

path "kv-v2/data/infrastructure/*" {
  capabilities = ["read"]
}

# DEVIATION from architecture/docs/09-implementation/09-openbao-deployment.md:
# The architecture doc specifies only bootstrap/* and infrastructure/* paths
# for the eso-management policy. The openstack/keystone/* path is added because
# the keystone-db ExternalSecret (deploy/eso/externalsecrets/keystone-db.yaml)
# reads from kv-v2/openstack/keystone/db, which requires this capability
# on the management cluster's ESO role (CC-0009).
# Scoped to keystone/* rather than openstack/* to maintain least-privilege —
# other OpenStack service credentials (nova, neutron, etc.) are excluded.
#
# This policy stays READ-ONLY by design. Write access for the fernet-keys
# and credential-keys backup PushSecrets is granted by a separate, narrowly-
# scoped policy — see deploy/openbao/policies/push-keystone-keys.hcl — which
# is bound alongside eso-management on the management cluster's auth role.
# The separation preserves the audit invariant that a leaked management-cluster
# ESO token on eso-management alone cannot write to OpenBao (CC-0083).
#
# CC-0112 verification: the per-CR paths are now namespace+name-scoped —
# `openstack/keystone/{namespace}/{name}/{admin/app-credential,fernet-keys,
# credential-keys}`. The trailing `*` wildcard already matches any remaining
# depth, so it covers these new shapes with no widening required; this policy
# stays read-only.
path "kv-v2/data/openstack/keystone/*" {
  capabilities = ["read"]
}
