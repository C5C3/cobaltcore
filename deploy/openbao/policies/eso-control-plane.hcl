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

# kv-v2/data/openstack/* already covers the per-ControlPlane Keystone DB
# credential at kv-v2/data/openstack/keystone/{namespace}/{name}/db (CC-0116);
# the wildcard needs no widening for the per-CP scoping.
path "kv-v2/data/openstack/*" {
  capabilities = ["read"]
}

path "kv-v2/data/infrastructure/*" {
  capabilities = ["read"]
}

path "kv-v2/data/ceph/*" {
  capabilities = ["read"]
}
