# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# ESO Management policy — grants read-only access to the genuinely SHARED
# bootstrap and infrastructure secrets needed by the Management cluster.
# Bound to the ESO ServiceAccount in the Management cluster via the
# kubernetes/management auth mount (see setup-auth.sh, the eso-management role).
#
# This policy carries NO Keystone key material grant. The per-ControlPlane
# Keystone paths — DB credentials, fernet-keys, credential-keys, admin app
# credentials and service-account passwords under
# `kv-v2/data/openstack/keystone/{namespace}/{name}/...` — are read and written
# through the per-tenant eso-tenant identity (deploy/openbao/policies/eso-tenant.hcl),
# which OpenBao scopes to the caller's OWN namespace. The former
# `kv-v2/data/openstack/keystone/*` read here matched EVERY ControlPlane's
# Keystone subtree and so let one tenant's shared ESO token read another
# tenant's key material; it has been removed. The three wildcard write policies
# that were bound alongside this one (push-keystone-keys / push-keystone-admin /
# push-app-credentials) have been retired for the same reason (#606).
#
# the per-CR bootstrap admin password lives at
# `bootstrap/{namespace}/{name}/admin`. The trailing `*` already matches that
# depth, so this read grant covers it with no widening needed. This is a shared
# subtree by design (multiple tenants' bootstrap material); the cluster store
# that binds this policy is namespace-restricted (deploy/eso/clustersecretstore.yaml)
# so only the static-manifest namespaces may reference it.
path "kv-v2/data/bootstrap/*" {
  capabilities = ["read"]
}

path "kv-v2/data/infrastructure/*" {
  capabilities = ["read"]
}
