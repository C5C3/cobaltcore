# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# nova-cell-db-dynamic policy: grants read on the per-tenant dynamic MariaDB
# database-engine credentials path for the Nova cell database user. The schemas
# it covers are nova and nova_cell0. Bound to the "nova-cell-db" role on the
# kubernetes/management auth mount (see setup-auth.sh), which the c5c3 operator's
# per-ControlPlane VaultDynamicSecret generator uses to issue short-lived Nova
# cell DB users at database/mariadb/creds/nova-cell-<namespace>. That generator
# arrives with #1019; until a ControlPlane projects a Nova the role is dormant,
# because no nova-cell-db-creds ServiceAccount exists to authenticate with.
#
# TENANT ISOLATION: the read path is scoped by OpenBao ACL identity templating to
# the caller's OWN service-account namespace. {{...service_account_namespace}}
# resolves, at request time, to the namespace of the nova-cell-db-creds SA token
# that authenticated. Per-tenant roles are named nova-cell-<namespace> (one
# ControlPlane per namespace, so the namespace is a unique, collision-free tenant
# key), so this is an EXACT match with no wildcard: a token minted in namespace A
# can read only database/mariadb/creds/nova-cell-A and cannot read another
# tenant's nova-cell-B path. The nova-cell-db role keeps
# bound_service_account_namespaces="*" (any ControlPlane namespace may
# authenticate); it is this templated policy, not the role binding, that enforces
# the cross-tenant boundary (the client cert only gates transport).
#
# SERVICE ISOLATION: this policy and its nova-cell-db role are entirely separate
# from the keystone-db and cinder-db pairs, and from the nova-api-db pair that
# covers the same service's API database. Every one of those roles binds
# bound_service_account_namespaces="*", and they are told apart by the
# ServiceAccount NAME the token carries: nova-cell-db-creds here,
# nova-api-db-creds for nova-api-db-dynamic, keystone-db-creds for
# keystone-db-dynamic. The cell credential generator can therefore never read the
# API creds path, and the API generator can never read this one, even though both
# run in one namespace. The two role names are prefix-free on top of that: the
# per-tenant engine role is <role>-<namespace>, so a role named nova in namespace
# api-x and a role named nova-api in namespace x would both flatten to
# nova-api-x. Naming this role nova-cell keeps either creds path from equalling
# the other's in any namespace.
#
# The KUBERNETES_MANAGEMENT_ACCESSOR placeholder is substituted with the live
# kubernetes/management auth-mount accessor at apply time by setup-policies.sh.
# The accessor is generated when the mount is enabled (setup-auth.sh, run first)
# and is not known until runtime.
#
# READ-ONLY by design: a dynamic secrets engine has no long-lived static
# password to push back, so there is no write/push grant here.
path "database/mariadb/creds/nova-cell-{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}" {
  capabilities = ["read"]
}
