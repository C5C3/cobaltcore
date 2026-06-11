# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Control Plane policy — grants read-only access to all OpenStack,
# infrastructure, bootstrap, and Ceph secrets.
# Bound to the ESO ServiceAccount in the Control Plane cluster via
# kubernetes/control-plane auth mount.
# Feature: CC-0009

# CC-0117 verification: the operator-projected admin-password ExternalSecret
# (c5c3 reconcileAdminPassword) reads `bootstrap/{namespace}/{keystone}/admin`
# under this existing bootstrap read grant — read-only, no widening required.
path "kv-v2/data/bootstrap/*" {
  capabilities = ["read"]
}

# CC-0116 (REQ-006): this grant already covers the per-ControlPlane Keystone DB
# path 'openstack/keystone/{namespace}/{name}/db', so no widening is required.
path "kv-v2/data/openstack/*" {
  capabilities = ["read"]
}

path "kv-v2/data/infrastructure/*" {
  capabilities = ["read"]
}

path "kv-v2/data/ceph/*" {
  capabilities = ["read"]
}
