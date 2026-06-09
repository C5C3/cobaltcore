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

# KORC_CONTROLPLANES: whitespace-separated list of "<namespace>/<controlplane>"
# identities to seed per-ControlPlane bootstrap secrets for (CC-0112, REQ-009).
# For each identity the script seeds:
#   - the Model B admin password at  kv-v2/bootstrap/<namespace>/<controlplane>-keystone/admin
# The K-ORC bootstrap clouds.yaml is no longer seeded here: the operator now seeds
# it from reconcileKORC (seedBootstrapCloudsYAML) — CC-0114.
# The default is the single canonical ControlPlane the Quick Start brings up
# (deploy/kind/controlplane/controlplane.yaml: ControlPlane "controlplane" in
# namespace "openstack"), so the single-CR `make deploy-infra` path is unchanged
# (CC-0112, REQ-009; see hack/deploy-infra.sh). Override it to bring up several
# ControlPlanes (e.g. KORC_CONTROLPLANES="tenant-a/cp tenant-b/cp"); each entry
# MUST be "<namespace>/<controlplane>".
KORC_CONTROLPLANES="${KORC_CONTROLPLANES:-openstack/controlplane}"

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
# admin-password rotation backup PushSecret (per-CR RemoteKey
# bootstrap/{namespace}/{keystone}/admin, see
# operators/keystone/internal/controller/reconcile_passwordrotation.go; CC-0112)
# could never mirror the rotated password back into OpenBao without this marker
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

###############################################################################
# Main
###############################################################################
main() {
  log "=== Writing bootstrap secrets ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"

  write_secret_if_missing "kv-v2/infrastructure/mariadb" \
    "root-password=${GENERATED_PASSWORD}"

  write_secret_if_missing "kv-v2/openstack/keystone/db" \
    "username=keystone" \
    "password=${GENERATED_PASSWORD}"

  # CC-0112 (REQ-009): per-ControlPlane bootstrap seeding. For each
  # "<namespace>/<controlplane>" identity in KORC_CONTROLPLANES (default
  # "openstack/controlplane"), seed ONLY the per-CR Model B admin password on the
  # per-CR OpenBao path so two ControlPlanes never collide on the cluster-global
  # OpenBao backend. The K-ORC bootstrap clouds.yaml is now seeded by the
  # operator's reconcileKORC (seedBootstrapCloudsYAML) rather than here (CC-0114).
  # The legacy flat writes (bootstrap/keystone-admin,
  # openstack/keystone/admin/app-credential) are gone: the keystone-operator Model
  # B rotation PushSecret and the c5c3-operator admin AC PushSecret now target
  # bootstrap/{ns}/{keystone}/admin and
  # openstack/keystone/{ns}/{cp}/admin/app-credential respectively (CC-0112,
  # REQ-001/REQ-002).
  # shellcheck disable=SC2086  # KORC_CONTROLPLANES is intentionally word-split on whitespace into identities.
  for identity in ${KORC_CONTROLPLANES}; do
    if [[ "${identity}" != */* ]]; then
      log "ERROR: KORC_CONTROLPLANES entry '${identity}' is not in '<namespace>/<controlplane>' form."
      exit 1
    fi
    local cp_ns="${identity%%/*}"
    local cp_name="${identity#*/}"
    local keystone_name="${cp_name}-keystone"
    local admin_path="kv-v2/bootstrap/${cp_ns}/${keystone_name}/admin"

    log "--- Seeding ControlPlane '${identity}' (keystone='${keystone_name}') ---"

    # Model B admin password (the bootstrap source the keystone-admin
    # ExternalSecret reads and the rotation PushSecret later overwrites).
    write_secret_if_missing "${admin_path}" \
      "password=${GENERATED_PASSWORD}"
    # CC-0109: let the Model B admin-password rotation PushSecret overwrite this
    # pre-seeded path (ESO's managed-by guard rejects it otherwise). See
    # mark_eso_managed.
    mark_eso_managed "${admin_path}"
  done

  log "=== Done ==="
}

main "$@"
