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
      put_args+=" ${key}=\"\$(bao write -field=random_bytes sys/tools/random/32 format=base64)\""
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

###############################################################################
# Main
###############################################################################
main() {
  log "=== Writing bootstrap secrets ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"

  write_secret_if_missing "kv-v2/bootstrap/keystone-admin" \
    "password=${GENERATED_PASSWORD}"

  write_secret_if_missing "kv-v2/infrastructure/mariadb" \
    "root-password=${GENERATED_PASSWORD}"

  write_secret_if_missing "kv-v2/openstack/keystone/db" \
    "username=keystone" \
    "password=${GENERATED_PASSWORD}"

  log "=== Done ==="
}

main "$@"
