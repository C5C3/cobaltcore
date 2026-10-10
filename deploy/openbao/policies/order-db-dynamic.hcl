# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# order-db-dynamic policy — grants read on the dynamic MariaDB database-engine
# credentials of the MariaDBDatabase orders of one ControlPlane namespace. Bound
# to the "order-db" role on the kubernetes/management auth mount (see
# setup-auth.sh), which the VaultDynamicSecret generator of each order uses to
# issue short-lived users at database/mariadb/creds/order.<namespace>.<order>.
#
# TENANT ISOLATION: the c5c3-operator names every order role
# order.<ControlPlane namespace>.<child prefix>, with dots as the separators. A
# namespace cannot contain a dot, so the templated segment below matches the
# caller's own namespace exactly and nothing that only starts with it: the
# token of the order-db-creds ServiceAccount in namespace A reads the roles of
# A's orders and cannot read those of namespace A-B. The order-db role keeps
# bound_service_account_namespaces="*"; it is this templated policy that
# enforces the cross-tenant boundary (the client cert only gates transport).
#
# The KUBERNETES_MANAGEMENT_ACCESSOR placeholder is substituted with the live
# kubernetes/management auth-mount accessor at apply time by setup-policies.sh.
#
# READ-ONLY by design: the engine issues the credential, and nothing is pushed
# back. The roles themselves are written by the c5c3-operator under the
# c5c3-operator policy.
path "database/mariadb/creds/order.{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}.*" {
  capabilities = ["read"]
}
