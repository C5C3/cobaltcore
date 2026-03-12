# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Management policy — grants read-only access to bootstrap and
# infrastructure secrets needed by the Management cluster.
# Bound to the ESO ServiceAccount in the Management cluster via
# kubernetes/management auth mount.
# Feature: CC-0009

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
path "kv-v2/data/openstack/keystone/*" {
  capabilities = ["read"]
}
