#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# setup-auth.sh — Enable and configure OpenBao auth methods (Kubernetes + AppRole).
#
# This script is idempotent: auth mounts are only enabled when they do not
# already exist, and role writes are upserts.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

###############################################################################
# Configuration
###############################################################################
BAO_TOKEN="${BAO_TOKEN:?BAO_TOKEN must be set}"

CLUSTERS=(management control-plane hypervisor storage)

# Return the list of currently enabled auth mounts (trailing slashes included).
auth_mounts() {
  bao_exec bao auth list -format=json | jq -r 'keys[]'
}

# Enable an auth method at the given path if it is not already mounted.
enable_auth_if_missing() {
  local path="$1"   # e.g. kubernetes/management
  local type="$2"   # e.g. kubernetes

  # Auth list keys have a trailing slash, so we compare with one appended.
  if auth_mounts | grep -qx "${path}/"; then
    log "Auth mount '${path}/' already enabled — skipping."
  else
    log "Enabling auth method '${type}' at path '${path}'..."
    bao_exec bao auth enable -path="${path}" "${type}"
    log "Auth method '${type}' enabled at '${path}'."
  fi
}

###############################################################################
# Main
###############################################################################
main() {
  log "=== OpenBao Auth Setup ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"

  # Kubernetes auth — four cluster mounts
  for cluster in "${CLUSTERS[@]}"; do
    enable_auth_if_missing "kubernetes/${cluster}" "kubernetes"
  done

  # Configure the management cluster auth mount with the in-cluster Kubernetes
  # API server endpoint and CA certificate. This tells OpenBao how to validate
  # service account tokens via the TokenReview API. Without this config,
  # OpenBao relies on auto-discovery which requires the system:auth-delegator
  # ClusterRoleBinding (created by the Helm chart when
  # server.authDelegator.enabled=true, the default).
  # Explicit configuration is more reliable and self-documenting.
  log "Configuring Kubernetes auth for management cluster..."
  bao_exec bao write auth/kubernetes/management/config \
    kubernetes_host="https://kubernetes.default.svc" \
    kubernetes_ca_cert=@/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  log "Management cluster Kubernetes auth configured."

  # Create ESO roles for each cluster mount (upsert — inherently idempotent).
  #
  # NOTE Only the management cluster has its auth config written above.
  # The control-plane, hypervisor, and storage clusters do NOT have auth config
  # yet — their `auth/kubernetes/<cluster>/config` is deferred until those
  # clusters are provisioned (they don't exist yet). Until configured, any
  # authentication attempt against those mounts will fail because OpenBao cannot
  # validate service account tokens without a Kubernetes API endpoint.
  #
  # Pre-creating the roles here avoids a second bootstrap pass when those
  # clusters come online — only `bao write auth/kubernetes/<cluster>/config`
  # is needed to activate them.
  for cluster in "${CLUSTERS[@]}"; do
    # Every cluster's shared ESO role now binds ONLY its own read-only
    # eso-<cluster> policy. The cross-tenant Keystone write grants that the
    # management role once carried (push-keystone-keys / push-keystone-admin /
    # push-app-credentials) have been RETIRED: they matched every ControlPlane's
    # per-CR paths behind two `+/+` globs, so a leaked management-cluster ESO
    # token could read, overwrite, and delete ANY tenant's fernet/credential
    # keys and bootstrap admin password. Per-tenant secret traffic now
    # authenticates as the templated eso-tenant role (see the eso-tenant role
    # below and deploy/openbao/policies/eso-tenant.hcl), which OpenBao scopes to
    # the caller's OWN namespace. The shared eso-management read is now confined
    # to the genuinely shared bootstrap/* and infrastructure/* subtrees.
    local token_policies="eso-${cluster}"

    log "Writing ESO role for cluster '${cluster}'..."
    bao_exec bao write "auth/kubernetes/${cluster}/role/eso-${cluster}" \
      bound_service_account_names=external-secrets \
      bound_service_account_namespaces=external-secrets \
      "token_policies=${token_policies}" \
      token_ttl=1h \
      token_max_ttl=4h
    log "ESO role 'eso-${cluster}' written."
  done

  # keystone-db role on the management cluster's Kubernetes auth mount. The c5c3
  # operator's per-ControlPlane VaultDynamicSecret generator authenticates with
  # the "keystone-db-creds" ServiceAccount (projected per ControlPlane namespace)
  # to read short-lived DB credentials at database/mariadb/creds/keystone-<namespace>.
  # bound_service_account_namespaces="*" lets any ControlPlane namespace
  # authenticate; the SA name is fixed and the cross-tenant boundary is enforced by
  # the keystone-db-dynamic policy, which templates the readable creds path to the
  # caller's OWN service_account_namespace (an exact match, so a token minted in
  # namespace A cannot read namespace B's path).
  #
  # Token TTLs must cover the DB credential lease, NOT mirror the eso-<cluster>
  # roles: OpenBao revokes a dynamic-secret lease together with the auth token
  # that minted it, so the effective credential lifetime is
  # min(lease TTL, minting token TTL). With the short eso-style 1h token this
  # role once wore, every issued DB credential silently died after ~1h — 23h
  # before the ExternalSecret's 24h refresh re-minted — dropping the ephemeral
  # MySQL user under a running Keystone. Pin both values to DB_CREDS_MAX_TTL
  # (72h, setup-database-tenant.sh) so the lease TTLs there stay the binding
  # constraint; the token is read-only-scoped by keystone-db-dynamic, which
  # bounds the exposure of its longer lifetime.
  log "Writing keystone-db role on kubernetes/management..."
  bao_exec bao write "auth/kubernetes/management/role/keystone-db" \
    bound_service_account_names=keystone-db-creds \
    bound_service_account_namespaces="*" \
    token_policies=keystone-db-dynamic \
    token_ttl=72h \
    token_max_ttl=72h
  log "keystone-db role written."

  # eso-tenant role on the management cluster's Kubernetes auth mount. This is
  # the per-ControlPlane ESO identity a namespaced SecretStore authenticates
  # with (created per tenant by setup-eso-tenant.sh with the "eso-tenant-auth"
  # ServiceAccount). bound_service_account_namespaces="*" lets any tenant
  # namespace authenticate; the SA name is fixed and the cross-tenant boundary is
  # enforced by the eso-tenant policy, which templates every readable/writable
  # path to the caller's OWN service_account_namespace (an exact match, so a token
  # minted in namespace A cannot touch namespace B's Keystone key material). The
  # client cert only gates transport, exactly like the keystone-db role above.
  # token_max_ttl=4h caps renewal (matching the eso-<cluster> roles) so a leaked
  # tenant token cannot be renewed indefinitely.
  log "Writing eso-tenant role on kubernetes/management..."
  bao_exec bao write "auth/kubernetes/management/role/eso-tenant" \
    bound_service_account_names=eso-tenant-auth \
    bound_service_account_namespaces="*" \
    token_policies=eso-tenant \
    token_ttl=1h \
    token_max_ttl=4h
  log "eso-tenant role written."

  # AppRole auth
  enable_auth_if_missing "approle" "approle"

  log "Writing AppRole provisioner role..."
  # secret_id_ttl=8760h (1 year) bounds the blast radius of a leaked secret
  # ID. CI/CD automation should rotate the secret ID before expiry.
  bao_exec bao write auth/approle/role/provisioner \
    token_policies=ci-cd-provisioner \
    token_ttl=1h \
    token_max_ttl=4h \
    secret_id_ttl=8760h
  log "AppRole provisioner role written."

  log "=== Done ==="
}

main "$@"
