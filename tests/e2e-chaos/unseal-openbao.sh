#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e-chaos/unseal-openbao.sh — Re-unseal openbao-0 after a chaos pod-kill.
#
# The kind/E2E environment runs OpenBao single-replica with file/raft storage
# and Shamir-key sealing (init keys live in Secret openbao-system/openbao-init-keys,
# applied at bootstrap by hack/deploy-infra.sh::openbao_init_unseal). When a
# chaos PodChaos action=pod-kill restarts the pod, the new instance starts
# sealed and stays 0/1 Running indefinitely — there is no auto-unseal.
#
# Production runs 3-replica HA Raft, where the surviving quorum keeps the
# cluster unsealed; that recovery path does not exist with one replica. This
# helper bridges the gap so the openbao-pod-kill chaos test can validate
# operator-side recovery (SecretsReady False -> True) without depending on
# OpenBao auto-recovery that the kind topology cannot provide (CC-0047).
#
# Idempotent: if the pod is already unsealed, the script exits 0 without
# touching anything.
#
# Key delivery (CC-0102 review fix): the unseal keys are forwarded over stdin
# via `bao operator unseal -` rather than as kubectl exec positional args.
# Positional args show up in /proc/<pid>/cmdline on the host while the
# kubectl invocation is alive; piping over stdin keeps the secret out of the
# process listing. The same change is applied to tests/e2e/keystone/
# openbao-sealed/unseal-all.sh for consistency.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_DIR="${SCRIPT_DIR}/../lib"

POD="${POD:-openbao-0}"
POD_RUNNING_TIMEOUT="${POD_RUNNING_TIMEOUT:-120}"
BAO_REACHABLE_RETRIES="${BAO_REACHABLE_RETRIES:-30}"
BAO_REACHABLE_INTERVAL="${BAO_REACHABLE_INTERVAL:-5}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] unseal-openbao: $*"
}

# shellcheck source=tests/lib/openbao.sh
source "${LIB_DIR}/openbao.sh"

# Polls .status.phase directly so a missing pod (StatefulSet still recreating
# after pod-kill) is treated as "not yet Running" instead of a hard failure
# the way `kubectl wait --for=jsonpath` would handle it.
wait_for_pod_running() {
  local deadline=$(( $(date +%s) + POD_RUNNING_TIMEOUT ))
  log "Waiting up to ${POD_RUNNING_TIMEOUT}s for pod ${BAO_NAMESPACE}/${POD} to reach phase=Running..."
  while true; do
    local phase=""
    phase=$(kubectl get pod "${POD}" -n "${BAO_NAMESPACE}" \
      -o jsonpath='{.status.phase}' 2>/dev/null) || phase=""
    if [[ "${phase}" == "Running" ]]; then
      log "  pod ${BAO_NAMESPACE}/${POD} is Running"
      return 0
    fi
    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: pod ${BAO_NAMESPACE}/${POD} did not reach Running within ${POD_RUNNING_TIMEOUT}s (last phase: ${phase:-<not found>})"
      kubectl get pod "${POD}" -n "${BAO_NAMESPACE}" -o wide 2>&1 || true
      exit 1
    fi
    sleep 2
  done
}

# Apply each unseal key over stdin. `bao operator unseal -` reads a single
# key from stdin per invocation. This keeps the key value out of the host's
# /proc/<pid>/cmdline, mirroring the change applied to
# tests/e2e/keystone/openbao-sealed/unseal-all.sh.
unseal() {
  log "Reading unseal keys from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local keys=()
  while IFS= read -r line; do
    keys+=("${line}")
  done < <(openbao_read_unseal_keys)

  local idx=0
  local key
  for key in "${keys[@]}"; do
    printf '%s' "${key}" | openbao_bao_run "${POD}" bao operator unseal - > /dev/null
    idx=$(( idx + 1 ))
    log "  applied unseal key ${idx}/${KEY_THRESHOLD}"
  done
}

wait_for_pod_ready() {
  local deadline=$(( $(date +%s) + BAO_REACHABLE_RETRIES * BAO_REACHABLE_INTERVAL ))
  while true; do
    if openbao_pod_is_ready "${POD}"; then
      log "${POD} is Ready (unsealed)"
      return 0
    fi
    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: ${POD} did not become Ready within $(( BAO_REACHABLE_RETRIES * BAO_REACHABLE_INTERVAL ))s after unseal"
      kubectl get pod "${POD}" -n "${BAO_NAMESPACE}" -o wide 2>&1 || true
      exit 1
    fi
    log "  waiting for ${POD} Ready=True after unseal..."
    sleep "${BAO_REACHABLE_INTERVAL}"
  done
}

main() {
  wait_for_pod_running

  # Fast-path: if the pod is already Ready, OpenBao is already unsealed
  # (the readiness probe enforces it). Skip the bao client entirely.
  if openbao_pod_is_ready "${POD}"; then
    log "${POD} already Ready — nothing to do"
    return 0
  fi

  log "${POD} is not Ready, applying unseal keys..."
  unseal

  wait_for_pod_ready
  log "${POD} unsealed successfully"
}

main "$@"
