#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# write-bootstrap-secrets.sh — Write initial bootstrap secrets to OpenBao KV-v2.
# Feature: CC-0009
#
# This script is idempotent: each secret is only written when it does not
# already exist in the KV store.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

###############################################################################
# Configuration
###############################################################################
BAO_TOKEN="${BAO_TOKEN:?BAO_TOKEN must be set}"

# Marker value: use as a key's value to generate a cryptographically random
# password (base64, 32 bytes) inside the pod using OpenBao's sys/tools/random
# API.  Generating in-pod avoids cleartext passwords appearing in host process
# argument lists.
readonly GENERATED_PASSWORD="@generate"

# Write a secret only if it does not already exist in the KV-v2 store.
#
# Usage: write_secret_if_missing <kv_path> <key1>=<value1> [<key2>=<value2> ...]
#
# Use GENERATED_PASSWORD ("@generate") as a value to generate a random password
# inside the pod:  write_secret_if_missing "path" "password=@generate"
#
# The function checks whether the secret at <kv_path> is readable. If the
# secret exists (exit code 0), the write is skipped. Otherwise the secret is
# created with the supplied key/value pairs.
write_secret_if_missing() {
  local kv_path="$1"
  shift

  if bao_exec bao kv get -format=json "${kv_path}" >/dev/null 2>&1; then
    log "Secret '${kv_path}' already exists — skipping."
    return 0
  fi

  # Build bao kv put arguments, replacing @generate markers with in-pod
  # password generation so cleartext never travels as a kubectl exec arg.
  local put_args=""
  for arg in "$@"; do
    local key="${arg%%=*}"
    local val="${arg#*=}"
    if [[ "${val}" == "${GENERATED_PASSWORD}" ]]; then
      put_args+=" ${key}=\"\$(bao write -field=random_bytes sys/tools/random/32 format=hex)\""
    else
      # Quote non-generated values to guard against shell metacharacters
      # (spaces, equals signs in values, etc.) in the sh -c command string.
      # Escape embedded single quotes: ' → '\'' (CC-0009)
      local escaped_val="${val//\'/\'\\\'\'}"
      put_args+=" ${key}='${escaped_val}'"
    fi
  done

  log "Writing secret '${kv_path}'..."
  # Pass kv_path as an env var (BAO_KV_PATH) to the inner shell rather than
  # interpolating it into the sh -c command string. This avoids shell injection
  # if a future caller passes a path containing single quotes or metacharacters.
  kubectl exec -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" \
    BAO_KV_PATH="${kv_path}" \
    sh -c "bao kv put \"\${BAO_KV_PATH}\" ${put_args}"
  log "Secret '${kv_path}' written."
}

# Mark a KV-v2 path as managed by the External Secrets Operator so its
# Vault/OpenBao PushSecret provider will adopt and overwrite it.
#
# Usage: mark_eso_managed <kv_path>
#
# ESO refuses to push to a pre-existing secret whose metadata lacks
# custom_metadata.managed-by=external-secrets and fails with the error
# "secret not managed by external-secrets"
# (external-secrets providers/v1/vault/client_push.go). A path seeded above with
# `bao kv put` carries no custom_metadata, so the Model B scheduled
# admin-password rotation backup PushSecret (RemoteKey bootstrap/keystone-admin,
# see operators/keystone/internal/controller/reconcile_passwordrotation.go) could
# never mirror the rotated password back into OpenBao without this marker
# (CC-0109). Stamping the exact marker ESO itself writes lets ESO treat the
# seeded value as its own on the first push.
#
# Idempotent and migration-safe: it runs unconditionally (not gated on
# write_secret_if_missing's existence check), so it also marks a secret that a
# prior deploy already created without the marker. `bao kv metadata put` updates
# only the metadata — the stored password versions are left untouched.
mark_eso_managed() {
  local kv_path="$1"

  log "Marking '${kv_path}' as managed-by=external-secrets (ESO PushSecret adoption)..."
  kubectl exec -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" \
    BAO_KV_PATH="${kv_path}" \
    sh -c "bao kv metadata put -custom-metadata=managed-by=external-secrets \"\${BAO_KV_PATH}\""
  log "Secret '${kv_path}' marked ESO-managed."
}

# Seed a PASSWORD-based bootstrap clouds.yaml for the K-ORC admin Application
# Credential, breaking the K-ORC credential bootstrap cycle (CC-0110, REQ-024).
#
# THE CYCLE: the c5c3-operator gates its admin-Application-Credential PushSecret
# (which WRITES openstack/keystone/admin/app-credential) on the k-orc-clouds-yaml
# ExternalSecret being Ready — but that ExternalSecret materialises FROM this
# exact OpenBao path. On a fresh cluster the path is empty, so the ExternalSecret
# never goes Ready, the PushSecret is never created, and the path stays empty
# forever (permanent deadlock).
#
# THE SEED: we write a password-based clouds.yaml derived from the admin password
# at bootstrap/keystone-admin so K-ORC can authenticate AS admin and mint the
# restricted application credential. Once K-ORC mints it, the c5c3-operator's
# PushSecret OVERWRITES this path with the application-credential-based clouds.yaml
# — hence mark_eso_managed below grants ESO ownership so that overwrite is allowed.
#
# Idempotent: skips the write when the path already exists, so a re-run never
# clobbers a real minted credential. The admin password is read and the clouds.yaml
# assembled ENTIRELY IN-POD (single-quoted sh -c) so the cleartext password never
# appears in a host process argument list (mirrors write_secret_if_missing's
# in-pod generation discipline). The admin password is a hex string
# (bootstrap/keystone-admin is seeded via `bao write ... format=hex`), so it
# carries no YAML/shell metacharacters.
seed_korc_bootstrap_clouds_yaml() {
  local kv_path="kv-v2/openstack/keystone/admin/app-credential"
  local admin_path="kv-v2/bootstrap/keystone-admin"

  log "Seeding K-ORC bootstrap clouds.yaml at '${kv_path}' (if missing)..."
  # DECISION (CC-0110, REQ-024): auth_url uses the in-cluster Keystone identity
  # Service DNS the c5c3-operator also derives (keystoneEndpointURL:
  # http://keystone.<ns>.svc:5000/v3, control-plane namespace "openstack"). It
  # MUST match the Keystone API Service the keystone-operator exposes; this is the
  # same convention as reconcile_korc.go's keystoneEndpointURL. Reviewer: please
  # verify the Service DNS on a live cluster.
  # shellcheck disable=SC2016  # $vars are intentionally expanded IN-POD, not by the host shell.
  kubectl exec -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" \
    BAO_KV_PATH="${kv_path}" BAO_ADMIN_PATH="${admin_path}" \
    sh -c '
      set -eu
      if bao kv get "${BAO_KV_PATH}" >/dev/null 2>&1; then
        echo "Secret \"${BAO_KV_PATH}\" already exists — skipping bootstrap clouds.yaml seed."
        exit 0
      fi
      admin_pw="$(bao kv get -field=password "${BAO_ADMIN_PATH}")"
      clouds="clouds:
  admin:
    auth:
      auth_url: http://keystone.openstack.svc:5000/v3
      username: admin
      password: ${admin_pw}
      project_name: admin
      user_domain_name: Default
      project_domain_name: Default
    region_name: RegionOne
    interface: public
    identity_api_version: 3
"
      bao kv put "${BAO_KV_PATH}" clouds.yaml="${clouds}"
      echo "Secret \"${BAO_KV_PATH}\" seeded with bootstrap clouds.yaml."
    '
  log "K-ORC bootstrap clouds.yaml seed complete."
}

###############################################################################
# Main
###############################################################################
main() {
  log "=== Writing bootstrap secrets ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"

  write_secret_if_missing "kv-v2/bootstrap/keystone-admin" \
    "password=${GENERATED_PASSWORD}"
  # CC-0109: let the Model B admin-password rotation PushSecret overwrite this
  # pre-seeded path (ESO's managed-by guard rejects it otherwise). See
  # mark_eso_managed.
  mark_eso_managed "kv-v2/bootstrap/keystone-admin"

  write_secret_if_missing "kv-v2/infrastructure/mariadb" \
    "root-password=${GENERATED_PASSWORD}"

  write_secret_if_missing "kv-v2/openstack/keystone/db" \
    "username=keystone" \
    "password=${GENERATED_PASSWORD}"

  # CC-0110, REQ-024: seed the K-ORC admin Application Credential clouds.yaml so
  # the k-orc-clouds-yaml ExternalSecret can materialise on a fresh cluster and
  # K-ORC can bootstrap-authenticate, breaking the credential cycle. Must run
  # AFTER bootstrap/keystone-admin is written (it derives the password from it).
  seed_korc_bootstrap_clouds_yaml
  # Let the c5c3-operator's admin-Application-Credential PushSecret overwrite the
  # seeded path with the minted application credential (ESO's managed-by guard
  # rejects an unmarked pre-existing path otherwise — see mark_eso_managed).
  mark_eso_managed "kv-v2/openstack/keystone/admin/app-credential"

  log "=== Done ==="
}

main "$@"
