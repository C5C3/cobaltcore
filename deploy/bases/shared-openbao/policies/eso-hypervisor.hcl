# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Hypervisor policy — grants read-only access to Ceph client keys
# and Nova compute configuration needed on hypervisor nodes.
# Bound to the ESO ServiceAccount in the Hypervisor cluster via
# kubernetes/hypervisor auth mount.

path "kv-v2/data/ceph/client-nova" {
  capabilities = ["read"]
}

path "kv-v2/data/openstack/nova/compute-*" {
  capabilities = ["read"]
}
