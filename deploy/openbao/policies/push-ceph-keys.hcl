# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# PushSecret policy for Ceph keys — allows PushSecret CRs to write
# Ceph client keys back to OpenBao after Ceph generates them.
# Feature: CC-0009

path "kv-v2/data/ceph/*" {
  capabilities = ["create", "update", "read"]
}
