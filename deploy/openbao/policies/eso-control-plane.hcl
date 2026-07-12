# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Control Plane policy — grants read-only access to the shared bootstrap,
# infrastructure, and Ceph secrets.
# Bound to the ESO ServiceAccount in the Control Plane cluster via the
# kubernetes/control-plane auth mount. NOTE: that mount is enabled but not yet
# configured (setup-auth.sh), so this policy grants nothing today; it is narrowed
# here so the confused-deputy defect does not survive onto the day this cluster
# is onboarded.

# verification: the operator-projected admin-password ExternalSecret
# (c5c3 reconcileAdminPassword) reads `bootstrap/{namespace}/{keystone}/admin`
# under this existing bootstrap read grant — read-only, no widening required.
path "kv-v2/data/bootstrap/*" {
  capabilities = ["read"]
}

# The former `kv-v2/data/openstack/*` read wildcard matched every ControlPlane's
# Keystone key material (and every other OpenStack service's secrets) and has
# been removed (#606). When this cluster is onboarded, its per-ControlPlane
# Keystone paths must be read through a per-tenant templated identity of the
# same shape as deploy/openbao/policies/eso-tenant.hcl (which scopes every
# readable/writable path to the caller's own namespace), NOT through a shared
# `openstack/*` wildcard.

path "kv-v2/data/infrastructure/*" {
  capabilities = ["read"]
}

path "kv-v2/data/ceph/*" {
  capabilities = ["read"]
}
