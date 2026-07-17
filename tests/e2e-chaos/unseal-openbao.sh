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
# OpenBao auto-recovery that the kind topology cannot provide.
#
# Idempotent: if the pod is already unsealed, the script exits 0 without
# touching anything.

set -euo pipefail

# Note: use BAO_NAMESPACE (not NAMESPACE) because chainsaw injects $NAMESPACE
# into script env from the test's spec.namespace (openstack), which would
# silently override a NAMESPACE default and make us look for openbao-0 in the
# wrong namespace.
BAO_NAMESPACE="${BAO_NAMESPACE:-openbao-system}"
POD="${POD:-openbao-0}"
SECRET_NAME="${SECRET_NAME:-openbao-init-keys}"
BAO_ADDR="${BAO_ADDR:-https://127.0.0.1:8200}"
VAULT_CACERT="${VAULT_CACERT:-/openbao/tls/ca.crt}"
KEY_THRESHOLD="${KEY_THRESHOLD:-3}"
POD_RUNNING_TIMEOUT="${POD_RUNNING_TIMEOUT:-120}"
BAO_REACHABLE_RETRIES="${BAO_REACHABLE_RETRIES:-30}"
BAO_REACHABLE_INTERVAL="${BAO_REACHABLE_INTERVAL:-5}"
EXEC_READY_TIMEOUT="${EXEC_READY_TIMEOUT:-60}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] unseal-openbao: $*"
}

bao_exec() {
  kubectl exec -n "${BAO_NAMESPACE}" "${POD}" -- \
    env BAO_ADDR="${BAO_ADDR}" VAULT_CACERT="${VAULT_CACERT}" "$@"
}

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

# phase=Running is not enough to exec: right after a pod-kill the kubelet
# reports the recreated pod Running while the runtime is still starting the
# openbao container, and `kubectl exec` fails with
# `container not found ("openbao")`. Probe the exec path with a no-op until
# it works, so no unseal key is ever submitted over a transport that is not
# up yet.
wait_for_exec_ready() {
  local deadline=$(( $(date +%s) + EXEC_READY_TIMEOUT ))
  log "Waiting up to ${EXEC_READY_TIMEOUT}s for exec into ${BAO_NAMESPACE}/${POD} to become available..."
  while true; do
    if kubectl exec -n "${BAO_NAMESPACE}" "${POD}" -- true 2>/dev/null; then
      log "  exec into ${BAO_NAMESPACE}/${POD} is available"
      return 0
    fi
    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: exec into ${BAO_NAMESPACE}/${POD} did not become available within ${EXEC_READY_TIMEOUT}s"
      kubectl get pod "${POD}" -n "${BAO_NAMESPACE}" -o wide 2>&1 || true
      exit 1
    fi
    sleep 2
  done
}

unseal() {
  log "Reading unseal keys from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local init_output
  if ! init_output=$(kubectl get secret "${SECRET_NAME}" -n "${BAO_NAMESPACE}" \
      -o jsonpath='{.data.init-output}' | base64 -d); then
    log "ERROR: could not read Secret ${BAO_NAMESPACE}/${SECRET_NAME} (was openbao bootstrap run?)"
    exit 1
  fi

  local i key
  for i in $(seq 0 $(( KEY_THRESHOLD - 1 ))); do
    key=$(printf '%s' "${init_output}" | jq -r ".unseal_keys_b64[${i}]")
    if [[ -z "${key}" || "${key}" == "null" ]]; then
      log "ERROR: unseal key index ${i} missing from Secret"
      exit 1
    fi
    bao_exec bao operator unseal "${key}" > /dev/null
    log "  applied unseal key $((i + 1))/${KEY_THRESHOLD}"
  done
}

# OpenBao's upstream readiness probe is GET /v1/sys/health, which only returns
# 200 when the pod is initialized AND unsealed AND active. The kubelet's Ready
# condition therefore tracks unseal state directly, and is far more reliable
# than re-running `bao status` post-unseal: the bao client occasionally returns
# empty stdout while the listener re-initialises, while the readiness probe
# uses an in-process HTTP path that doesn't depend on TLS or the bao CLI.
pod_is_ready() {
  local ready
  ready=$(kubectl get pod "${POD}" -n "${BAO_NAMESPACE}" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) \
    || ready=""
  [[ "${ready}" == "True" ]]
}

wait_for_pod_ready() {
  local deadline=$(( $(date +%s) + BAO_REACHABLE_RETRIES * BAO_REACHABLE_INTERVAL ))
  while true; do
    if pod_is_ready; then
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
  if pod_is_ready; then
    log "${POD} already Ready — nothing to do"
    return 0
  fi

  log "${POD} is not Ready, applying unseal keys..."
  wait_for_exec_ready
  unseal

  wait_for_pod_ready
  log "${POD} unsealed successfully"
}

main "$@"
