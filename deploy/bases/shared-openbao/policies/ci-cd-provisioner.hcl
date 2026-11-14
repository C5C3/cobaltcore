# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# CI/CD Provisioner policy — grants full read/write access for initial
# secret provisioning by CI/CD pipelines. No delete capability to prevent
# accidental secret removal.
# Bound to the provisioner AppRole via approle/ auth mount.

path "kv-v2/data/*" {
  capabilities = ["create", "update", "read"]
}

path "kv-v2/metadata/*" {
  capabilities = ["read", "list"]
}
