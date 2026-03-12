# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Control Plane policy — grants read-only access to all OpenStack,
# infrastructure, bootstrap, and Ceph secrets.
# Bound to the ESO ServiceAccount in the Control Plane cluster via
# kubernetes/control-plane auth mount.
# Feature: CC-0009

path "kv-v2/data/bootstrap/*" {
  capabilities = ["read"]
}

path "kv-v2/data/openstack/*" {
  capabilities = ["read"]
}

path "kv-v2/data/infrastructure/*" {
  capabilities = ["read"]
}

path "kv-v2/data/ceph/*" {
  capabilities = ["read"]
}
