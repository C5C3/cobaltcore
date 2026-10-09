# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The read half of the Ceph key hand-off: ExternalSecrets read the Ceph client
# keys that the push-ceph-keys policy lets the storage side write. setup-auth.sh
# binds it with the read-ceph-keys role on kubernetes/management to the
# ServiceAccount openstack/ceph-keys-read, the identity of the lab's
# ExternalSecrets ceph-client-cinder and ceph-client-cinder-backup
# (deploy/lab/metal-stack/ceph/cluster/keys-pull.yaml).

path "kv-v2/data/ceph/*" {
  capabilities = ["read"]
}
