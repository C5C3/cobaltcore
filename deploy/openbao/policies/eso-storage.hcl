# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Storage policy — grants read and write access to Ceph keys.
# The Storage cluster ESO needs to both read existing keys and write
# newly generated Ceph keys back to OpenBao.
# Bound to the ESO ServiceAccount in the Storage cluster via
# kubernetes/storage auth mount.
# Feature: CC-0009

path "kv-v2/data/ceph/*" {
  capabilities = ["read", "create", "update"]
}
