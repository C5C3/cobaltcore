# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# keystone-db-dynamic policy — grants read on the per-tenant dynamic MariaDB
# database-engine credentials path. Bound to the "keystone-db" role on the
# kubernetes/management auth mount (see setup-auth.sh), which the c5c3 operator's
# per-ControlPlane VaultDynamicSecret generator uses to issue short-lived
# Keystone service-DB users at database/mariadb/creds/keystone-<namespace>.
#
# TENANT ISOLATION: the read path is scoped by OpenBao ACL identity templating to
# the caller's OWN service-account namespace — {{...service_account_namespace}}
# resolves, at request time, to the namespace of the keystone-db-creds SA token
# that authenticated. Per-tenant roles are named keystone-<namespace> (one
# ControlPlane per namespace, so the namespace is a unique, collision-free tenant
# key), so this is an EXACT match with no wildcard: a token minted in namespace A
# can read only database/mariadb/creds/keystone-A and cannot read another
# tenant's keystone-B path. The keystone-db role deliberately keeps
# bound_service_account_namespaces="*" (any ControlPlane namespace may
# authenticate); it is this templated policy — not the role binding — that
# enforces the cross-tenant boundary (the client cert only gates transport).
#
# The KUBERNETES_MANAGEMENT_ACCESSOR placeholder is substituted with the live
# kubernetes/management auth-mount accessor at apply time by setup-policies.sh —
# the accessor is generated when the mount is enabled (setup-auth.sh, run first)
# and is not known until runtime.
#
# READ-ONLY by design: a dynamic secrets engine has no long-lived static
# password to push back, so there is no write/push grant here. This supersedes
# the never-created push-keystone-db.hcl PushSecret policy that stage (a) (#435)
# deferred to this follow-up (#439): with engine-issued credentials there is
# nothing to rotate-and-push, so that policy is confirmed unnecessary.
path "database/mariadb/creds/keystone-{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}" {
  capabilities = ["read"]
}
