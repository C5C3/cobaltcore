# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Hypervisor policy — grants read-only access to the Nova compute
# configuration needed on hypervisor nodes.
# Bound to the ESO ServiceAccount in the Hypervisor cluster via the
# kubernetes/hypervisor auth mount. The Ceph cinder client key is read as the
# read-ceph-client-cinder role on kubernetes/hypervisor (see setup-auth.sh and
# read-ceph-client-cinder.hcl).

path "kv-v2/data/openstack/nova/compute-*" {
  capabilities = ["read"]
}
