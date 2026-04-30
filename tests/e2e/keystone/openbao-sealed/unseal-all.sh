#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/openbao-sealed/unseal-all.sh — Re-unseal every OpenBao
# replica after the openbao-sealed E2E test (CC-0102).
#
# Modelled on tests/e2e-chaos/unseal-openbao.sh. Differences:
#   - Iterates openbao-{0,1,2} (all HA replicas) rather than just openbao-0.
#     The CC-0102 openbao-sealed test seals every replica to flip the
#     ClusterSecretStore to NotReady (sealing only one is insufficient — see
#     seal-all.sh for the Raft-leader rationale), so the recovery step must
#     symmetrically unseal every replica.
#   - Tolerates single-replica kind topologies: a pod that does not exist is
#     skipped with a warning rather than failing.
#
# Idempotency:
#   - If a pod is already Ready, the readiness probe (GET /v1/sys/health) has
#     confirmed it is unsealed; the script skips the bao client entirely
#     (mirrors tests/e2e-chaos/unseal-openbao.sh:128 fast-path, REQ-008).
#   - Re-running on an already-unsealed cluster exits 0 without acting.
#
# Failure modes:
#   - Missing init-output Secret is a hard failure with a clear error pointing
#     to the missing Secret name (REQ-002).
#   - A pod that does not become Ready within BAO_REACHABLE_RETRIES *
#     BAO_REACHABLE_INTERVAL seconds after unseal is a hard failure with the
#     last-known pod state dumped for diagnostics.
#
# REQ-001, REQ-002, REQ-008 (CC-0102)

set -euo pipefail

# Note: use BAO_NAMESPACE (not NAMESPACE) because chainsaw injects $NAMESPACE
# into script env from the test's spec.namespace (openstack), which would
# silently override a NAMESPACE default and make us look for openbao-* in the
# wrong namespace.
BAO_NAMESPACE="${BAO_NAMESPACE:-openbao-system}"
PODS=("openbao-0" "openbao-1" "openbao-2")
SECRET_NAME="${SECRET_NAME:-openbao-init-keys}"
BAO_ADDR="${BAO_ADDR:-https://127.0.0.1:8200}"
VAULT_CACERT="${VAULT_CACERT:-/openbao/tls/ca.crt}"
KEY_THRESHOLD="${KEY_THRESHOLD:-3}"
POD_RUNNING_TIMEOUT="${POD_RUNNING_TIMEOUT:-120}"
BAO_REACHABLE_RETRIES="${BAO_REACHABLE_RETRIES:-30}"
BAO_REACHABLE_INTERVAL="${BAO_REACHABLE_INTERVAL:-5}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] unseal-all: $*"
}

bao_exec() {
  local pod="$1"
  shift
  kubectl exec -n "${BAO_NAMESPACE}" "${pod}" -- \
    env BAO_ADDR="${BAO_ADDR}" VAULT_CACERT="${VAULT_CACERT}" "$@"
}

# Returns 0 if the named pod exists in BAO_NAMESPACE, 1 otherwise.
pod_exists() {
  local pod="$1"
  kubectl get pod "${pod}" -n "${BAO_NAMESPACE}" >/dev/null 2>&1
}

# OpenBao's upstream readiness probe is GET /v1/sys/health, which only returns
# 200 when the pod is initialized AND unsealed AND active. The kubelet's Ready
# condition therefore tracks unseal state directly — far more reliable than
# re-running `bao status` post-unseal: the bao client occasionally returns
# empty stdout while the listener re-initialises, while the readiness probe
# uses an in-process HTTP path that doesn't depend on TLS or the bao CLI.
pod_is_ready() {
  local pod="$1"
  local ready
  ready=$(kubectl get pod "${pod}" -n "${BAO_NAMESPACE}" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) \
    || ready=""
  [[ "${ready}" == "True" ]]
}

# Polls .status.phase directly so a missing pod (StatefulSet still recreating
# after a restart) is treated as "not yet Running" instead of a hard failure
# the way `kubectl wait --for=jsonpath` would handle it.
wait_for_pod_running() {
  local pod="$1"
  local deadline=$(( $(date +%s) + POD_RUNNING_TIMEOUT ))
  log "Waiting up to ${POD_RUNNING_TIMEOUT}s for pod ${BAO_NAMESPACE}/${pod} to reach phase=Running..."
  while true; do
    local phase=""
    phase=$(kubectl get pod "${pod}" -n "${BAO_NAMESPACE}" \
      -o jsonpath='{.status.phase}' 2>/dev/null) || phase=""
    if [[ "${phase}" == "Running" ]]; then
      log "  pod ${BAO_NAMESPACE}/${pod} is Running"
      return 0
    fi
    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: pod ${BAO_NAMESPACE}/${pod} did not reach Running within ${POD_RUNNING_TIMEOUT}s (last phase: ${phase:-<not found>})"
      kubectl get pod "${pod}" -n "${BAO_NAMESPACE}" -o wide 2>&1 || true
      exit 1
    fi
    sleep 2
  done
}

# Wait for a single pod to report Ready=True after unseal. Uses kubelet's
# probe rather than re-issuing `bao status` (see pod_is_ready rationale).
wait_for_pod_ready() {
  local pod="$1"
  local deadline=$(( $(date +%s) + BAO_REACHABLE_RETRIES * BAO_REACHABLE_INTERVAL ))
  while true; do
    if pod_is_ready "${pod}"; then
      log "  ${pod} is Ready (unsealed)"
      return 0
    fi
    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: ${pod} did not become Ready within $(( BAO_REACHABLE_RETRIES * BAO_REACHABLE_INTERVAL ))s after unseal"
      kubectl get pod "${pod}" -n "${BAO_NAMESPACE}" -o wide 2>&1 || true
      exit 1
    fi
    log "  waiting for ${pod} Ready=True after unseal..."
    sleep "${BAO_REACHABLE_INTERVAL}"
  done
}

# Read the unseal keys from the init-output Secret once and emit them on
# stdout, one per line. Hard-fails if the Secret is missing or any of the
# first KEY_THRESHOLD keys is empty.
read_unseal_keys() {
  local init_output
  if ! init_output=$(kubectl get secret "${SECRET_NAME}" -n "${BAO_NAMESPACE}" \
      -o jsonpath='{.data.init-output}' 2>/dev/null | base64 -d); then
    log "ERROR: could not read Secret ${BAO_NAMESPACE}/${SECRET_NAME} (was openbao bootstrap run?)"
    exit 1
  fi
  if [[ -z "${init_output}" ]]; then
    log "ERROR: Secret ${BAO_NAMESPACE}/${SECRET_NAME} exists but init-output key is empty"
    exit 1
  fi

  local i key
  for i in $(seq 0 $(( KEY_THRESHOLD - 1 ))); do
    key=$(printf '%s' "${init_output}" | jq -r ".unseal_keys_b64[${i}]")
    if [[ -z "${key}" || "${key}" == "null" ]]; then
      log "ERROR: unseal key index ${i} missing from Secret ${BAO_NAMESPACE}/${SECRET_NAME}"
      exit 1
    fi
    printf '%s\n' "${key}"
  done
}

unseal_pod() {
  local pod="$1"
  shift
  local keys=("$@")

  log "Unsealing ${pod}..."
  local idx=0
  local key
  for key in "${keys[@]}"; do
    bao_exec "${pod}" bao operator unseal "${key}" > /dev/null
    idx=$(( idx + 1 ))
    log "  applied unseal key ${idx}/${KEY_THRESHOLD} to ${pod}"
  done
}

main() {
  # Discover which of the expected pods actually exist (kind topology may run
  # single-replica). Fail fast if NONE exist — that means OpenBao is not
  # deployed in this cluster.
  local present=()
  local pod
  for pod in "${PODS[@]}"; do
    if pod_exists "${pod}"; then
      present+=("${pod}")
    else
      log "  ${pod} does not exist in ${BAO_NAMESPACE} — skipping (single-replica topology?)"
    fi
  done
  if [[ "${#present[@]}" -eq 0 ]]; then
    log "ERROR: none of ${PODS[*]} exist in ${BAO_NAMESPACE} — cannot unseal"
    exit 1
  fi

  # Wait for every present pod to reach phase=Running before any unseal
  # attempt. A pod still in ContainerCreating would reject `bao operator
  # unseal` with a connection-refused error.
  for pod in "${present[@]}"; do
    wait_for_pod_running "${pod}"
  done

  # Fast-path: if every present pod is already Ready, OpenBao is already
  # unsealed (the readiness probe enforces it). Skip the bao client entirely
  # — this is the idempotent re-run case (REQ-008).
  local needs_unseal=0
  for pod in "${present[@]}"; do
    if ! pod_is_ready "${pod}"; then
      needs_unseal=1
      break
    fi
  done
  if [[ "${needs_unseal}" -eq 0 ]]; then
    log "All present pods (${present[*]}) already Ready — nothing to do"
    return 0
  fi

  # Read unseal keys once, reuse for every sealed pod.
  log "Reading unseal keys from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local keys=()
  while IFS= read -r line; do
    keys+=("${line}")
  done < <(read_unseal_keys)

  for pod in "${present[@]}"; do
    if pod_is_ready "${pod}"; then
      log "${pod} already Ready — skipping"
      continue
    fi
    unseal_pod "${pod}" "${keys[@]}"
    wait_for_pod_ready "${pod}"
  done

  log "All present OpenBao replicas unsealed successfully"
}

main "$@"
