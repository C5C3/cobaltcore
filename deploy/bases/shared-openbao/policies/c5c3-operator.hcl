# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# c5c3-operator policy — the c5c3-operator's own OpenBao identity. Bound to the
# "c5c3-operator" role on the kubernetes/management auth mount (see
# setup-auth.sh), which the operator logs in on with its ServiceAccount token to
# manage the database-engine roles of MariaDBDatabase orders.
#
# The operator writes, reads and deletes the order roles
# (database/mariadb/roles/order.<namespace>.<order>), and on an order's teardown
# revokes every lease such a role issued, which drops the SQL users at once
# instead of at the end of their leases. revoke-prefix is a root-protected
# endpoint and needs sudo.
#
# Nothing under database/mariadb/config is granted: the engine connections, and
# with them the MariaDB root password, stay with setup-database-tenant.sh. An
# order role can only name a connection whose allowed_roles admits it, which is
# the Keystone connection of the order's ControlPlane namespace.
path "database/mariadb/roles/order.*" {
  capabilities = ["create", "read", "update", "delete"]
}

path "sys/leases/revoke-prefix/database/mariadb/creds/order.*" {
  capabilities = ["update", "sudo"]
}
