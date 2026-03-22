#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Feature: CC-0009
# Idempotent OpenBao initialization and unseal script.
# Initializes OpenBao with 5-share, 3-threshold Shamir secret sharing,
# stores init output as a Kubernetes Secret, and unseals all 3 replicas.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
KEY_SHARES=5
KEY_THRESHOLD=3
PODS=("openbao-0" "openbao-1" "openbao-2")
SECRET_NAME="openbao-init-keys"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
# init-unseal uses its own kube_exec because BAO_TOKEN is not available
# until after initialization completes.
kube_exec() {
  local pod="$1"
  shift
  kubectl exec -n "${NAMESPACE}" "${pod}" -- \
    env BAO_ADDR="${BAO_ADDR}" VAULT_CACERT="${VAULT_CACERT}" "$@"
}

# ---------------------------------------------------------------------------
# check_initialized
# Returns 0 if already initialized, 1 otherwise.
# `bao status` exit codes:
#   0 = unsealed (initialized + unsealed)
#   1 = error / connectivity issue
#   2 = sealed  (could be initialized OR uninitialized)
# We parse the JSON output to reliably distinguish the two cases.
# ---------------------------------------------------------------------------
check_initialized() {
  local status_json
  # bao status returns non-zero when sealed; capture output regardless.
  status_json=$(kube_exec "${PODS[0]}" bao status -format=json 2>/dev/null) || true

  if [[ -z "${status_json}" ]]; then
    log "ERROR: Could not reach ${PODS[0]} — check that the pod is running and kubectl exec is available."
    exit 1
  fi

  if echo "${status_json}" | jq -e '.initialized == true' >/dev/null 2>&1; then
    return 0  # initialized
  fi
  return 1    # not initialized
}

# ---------------------------------------------------------------------------
# initialize
# Initializes OpenBao and persists the init output as a Kubernetes Secret.
# ---------------------------------------------------------------------------
initialize() {
  log "Initializing OpenBao (key-shares=${KEY_SHARES}, key-threshold=${KEY_THRESHOLD}) ..."

  local init_output
  init_output=$(kube_exec "${PODS[0]}" \
    bao operator init \
      -key-shares="${KEY_SHARES}" \
      -key-threshold="${KEY_THRESHOLD}" \
      -format=json)

  log "Initialization successful. Storing init output in Secret ${NAMESPACE}/${SECRET_NAME} ..."

  # Delete the secret first if it already exists (idempotent re-creation).
  kubectl delete secret "${SECRET_NAME}" \
    -n "${NAMESPACE}" \
    --ignore-not-found=true > /dev/null 2>&1

  # Create the secret via `kubectl apply -f -` to avoid exposing the init
  # output (unseal keys + root token) in process argument lists. The
  # --from-literal approach would place the entire JSON in /proc/<pid>/cmdline.
  local encoded
  encoded=$(echo -n "${init_output}" | base64 | tr -d '\n')
  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Secret
metadata:
  name: ${SECRET_NAME}
  namespace: ${NAMESPACE}
type: Opaque
data:
  init-output: ${encoded}
EOF

  log "Secret ${NAMESPACE}/${SECRET_NAME} created."
}

# ---------------------------------------------------------------------------
# unseal_pod
# Unseals a single pod using the first KEY_THRESHOLD unseal keys.
# Skips the pod if it is already unsealed.
# ---------------------------------------------------------------------------
unseal_pod() {
  local pod="$1"

  # Check current seal status.
  local rc=0
  local status_json
  status_json=$(kube_exec "${pod}" bao status -format=json 2>/dev/null) || rc=$?

  if [[ -z "${status_json}" && "${rc}" -ne 0 ]]; then
    log "ERROR: Could not reach ${pod} — check that the pod is running and kubectl exec is available."
    exit 1
  fi

  if [[ "${rc}" -eq 0 ]]; then
    log "Pod ${pod} is already unsealed. Skipping."
    return 0
  fi

  log "Unsealing pod ${pod} ..."

  # Retrieve unseal keys from the Kubernetes Secret.
  local init_output
  init_output=$(kubectl get secret "${SECRET_NAME}" \
    -n "${NAMESPACE}" \
    -o jsonpath='{.data.init-output}' | base64 -d)

  # Apply the first KEY_THRESHOLD keys.
  local i
  for i in $(seq 0 $(( KEY_THRESHOLD - 1 ))); do
    local key
    key=$(echo "${init_output}" | jq -r ".unseal_keys_b64[${i}]")
    kube_exec "${pod}" bao operator unseal "${key}" > /dev/null
    log "  Applied unseal key $((i + 1))/${KEY_THRESHOLD} to ${pod}."
  done

  log "Pod ${pod} unsealed successfully."
}

# ---------------------------------------------------------------------------
# unseal_all
# Iterates over all replica pods and unseals each one.
# ---------------------------------------------------------------------------
unseal_all() {
  log "Unsealing all OpenBao pods ..."
  for pod in "${PODS[@]}"; do
    unseal_pod "${pod}"
  done
  log "All pods unsealed."
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  log "=== OpenBao Init & Unseal ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"

  if check_initialized; then
    log "OpenBao is already initialized. Skipping initialization."
  else
    initialize
  fi

  unseal_all

  log "=== Done ==="
}

main "$@"
