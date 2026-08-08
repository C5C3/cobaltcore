# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# barbican-db-dynamic policy — grants read on the per-tenant dynamic MariaDB
# database-engine credentials path for the Barbican service DB user. Bound to
# the "barbican-db" role on the kubernetes/management auth mount (see
# setup-auth.sh), which the c5c3 operator's per-ControlPlane VaultDynamicSecret
# generator uses to issue short-lived Barbican service-DB users at
# database/mariadb/creds/barbican-<namespace>.
#
# TENANT ISOLATION: the read path is scoped by OpenBao ACL identity templating to
# the caller's OWN service-account namespace — {{...service_account_namespace}}
# resolves, at request time, to the namespace of the barbican-db-creds SA token
# that authenticated. Per-tenant roles are named barbican-<namespace> (one
# ControlPlane per namespace, so the namespace is a unique, collision-free tenant
# key), so this is an EXACT match with no wildcard: a token minted in namespace A
# can read only database/mariadb/creds/barbican-A and cannot read another
# tenant's barbican-B path. The barbican-db role deliberately keeps
# bound_service_account_namespaces="*" (any ControlPlane namespace may
# authenticate); it is this templated policy — not the role binding — that
# enforces the cross-tenant boundary (the client cert only gates transport).
#
# SERVICE ISOLATION: this policy and its barbican-db role are entirely separate
# from the keystone-db pair. Both roles bind bound_service_account_namespaces="*"
# and are told apart by the ServiceAccount NAME the token carries —
# barbican-db-creds here versus keystone-db-creds for keystone-db-dynamic — so
# a Barbican credential generator can never read a Keystone creds path (or vice
# versa), even for two services co-located in one namespace.
#
# The KUBERNETES_MANAGEMENT_ACCESSOR placeholder is substituted with the live
# kubernetes/management auth-mount accessor at apply time by setup-policies.sh —
# the accessor is generated when the mount is enabled (setup-auth.sh, run first)
# and is not known until runtime.
#
# READ-ONLY by design: a dynamic secrets engine has no long-lived static
# password to push back, so there is no write/push grant here.
path "database/mariadb/creds/barbican-{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}" {
  capabilities = ["read"]
}
