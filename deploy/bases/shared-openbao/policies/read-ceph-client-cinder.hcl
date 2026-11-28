# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The read half of the Ceph key hand-off on a separate hypervisor cluster.
# setup-auth.sh binds it with the read-ceph-client-cinder role on
# kubernetes/hypervisor to the ServiceAccount openstack/ceph-keys-read. It reads
# the cinder client key alone: libvirt defines its Ceph secret from that key
# (deploy/overlays/metal-stack/hypervisor/libvirt-configmap.yaml), and a
# hypervisor never uses the key of cinder-backup.

path "kv-v2/data/ceph/client-cinder" {
  capabilities = ["read"]
}
