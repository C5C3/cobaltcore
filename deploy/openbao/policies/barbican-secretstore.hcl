# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Barbican secret-store policy — grants full read/write access to the KV v2
# mount barbican/, where Barbican stores its tenant secrets.
# Bound to the barbican AppRole via the approle/ auth mount (see setup-auth.sh,
# the barbican role). The consumer is Barbican's vault_plugin via castellan,
# which talks the plain Vault-compatible HTTP API that OpenBao keeps compatible.
#
# This is the brownfield twin of the barbican-secretstore policy the proving
# instance self-inits (deploy/kind/infrastructure/openbao-instance.yaml): same
# mount, policy, and role names, so a deployment attaching Barbican to this
# shared instance differs from the dedicated one only in which instance it
# points at.
#
# No wiring is needed to apply it: setup-policies.sh writes every *.hcl file in
# this directory.

path "barbican/data/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}

# No delete: in KV v2 a DELETE on metadata/ permanently destroys every version
# of a secret and its metadata, which removes the only in-store recovery path
# tenant key material has. The soft delete castellan's Vault key manager issues
# goes through data/ above and is undeletable-safe.
path "barbican/metadata/*" {
  capabilities = ["create", "read", "update", "list"]
}
