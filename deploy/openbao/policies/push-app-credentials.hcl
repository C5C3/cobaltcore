# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# PushSecret policy for Application Credentials — allows PushSecret CRs
# to write OpenStack Application Credentials back to OpenBao after
# operators create them via the Keystone API.
# Feature: CC-0009

path "kv-v2/data/openstack/*/app-credential" {
  capabilities = ["create", "update", "read"]
}
