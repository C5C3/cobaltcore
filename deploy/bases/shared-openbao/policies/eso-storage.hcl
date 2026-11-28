# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Storage policy — grants read-only access to the Ceph keys.
# Bound to the ESO ServiceAccount in the Storage cluster via the
# kubernetes/storage auth mount. Every namespace that may use the cluster's
# shared ESO store shares this identity, so it writes no Ceph key: a push from
# the storage cluster authenticates as the push-ceph-keys role on
# kubernetes/storage (see setup-auth.sh and push-ceph-keys.hcl).

path "kv-v2/data/ceph/*" {
  capabilities = ["read"]
}
