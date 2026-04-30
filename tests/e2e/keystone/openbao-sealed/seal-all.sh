#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/openbao-sealed/seal-all.sh — Seal every OpenBao replica
# and verify the ClusterSecretStore observes the cluster as NotReady.
# Feature: CC-0102
#
# Why all three replicas?
# OpenBao runs as a 3-replica HA Raft cluster. Sealing only one replica is NOT
# sufficient to flip the ClusterSecretStore Ready=False: Raft elects a new
# leader from the surviving quorum and the CSS continues serving secrets
# through the unsealed peers. The ClusterSecretStore — and consequently the
# operator's reconcileSecrets ClusterSecretStore probing branch
# (operators/keystone/internal/controller/reconcile_secrets.go:66) — only
# observes a SecretStoreNotReady condition once *every* replica is sealed.
# This helper therefore iterates openbao-{0,1,2} and seals each one.
#
# Authentication: `bao operator seal` is a privileged command that requires
# the root token. The token is recovered from the openbao-init-keys Secret
# (the same Secret that init-unseal.sh populates at bootstrap), parsed with
# jq, and passed via the BAO_TOKEN env var (matching deploy/openbao/bootstrap/
# common.sh:bao_exec).
#
# Idempotency: `bao operator seal` is a no-op against an already-sealed
# instance and exits 0 (CC-0102, REQ-008). A pod that does not exist (e.g.
# kind topology with single-replica OpenBao) is skipped with a warning rather
# than failing — the helper therefore tolerates 1- and 3-replica topologies.
#
# Single-replica topology guard (CC-0102 review fix): the helper requires at
# least one expected pod to be present AND records how many were actually
# sealed. This avoids the silent "single-replica topology that does not match
# production HA Raft" failure mode flagged in the review — when fewer than
# all production replicas exist, the post-condition CSS Ready=False assertion
# below still runs and gates correctness.
#
# CSS post-condition (CC-0102 review fix): after sealing every present
# replica, the helper asserts the ClusterSecretStore openbao-cluster-store
# transitions to Ready=False within CSS_READY_FALSE_TIMEOUT (default 240 s).
# This is the contract the upstream Keystone reconcileSecrets branch under
# test relies on; if a single-replica kind topology cannot reach that state,
# the helper fails fast rather than silently passing through the chainsaw
# step and timing out 5 minutes later in a cryptic Step 4 assertion.
#
# REQ-001, REQ-002, REQ-008 (CC-0102)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_DIR="${SCRIPT_DIR}/../../../lib"

PODS=("openbao-0" "openbao-1" "openbao-2")
CSS_NAME="${CSS_NAME:-openbao-cluster-store}"
CSS_READY_FALSE_TIMEOUT="${CSS_READY_FALSE_TIMEOUT:-240}"
CSS_POLL_INTERVAL="${CSS_POLL_INTERVAL:-5}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] seal-all: $*"
}

# shellcheck source=tests/lib/openbao.sh
source "${LIB_DIR}/openbao.sh"

seal_pod() {
  local pod="$1"
  local token="$2"

  if ! openbao_pod_exists "${pod}"; then
    log "  ${pod} does not exist in ${BAO_NAMESPACE} — skipping (single-replica topology?)"
    return 0
  fi

  if openbao_pod_is_sealed "${pod}"; then
    log "  ${pod} is already sealed — skipping"
    return 0
  fi

  log "  sealing ${pod}..."
  openbao_bao_run_auth "${pod}" "${token}" bao operator seal > /dev/null
  log "  ${pod} sealed"
}

# Wait until ClusterSecretStore openbao-cluster-store reports Ready=False.
# This is the cluster-side contract the operator's reconcileSecrets branch
# under test depends on (reconcile_secrets.go:66). A single-replica kind
# topology CAN reach this state (sealing the only replica trivially flips
# the store), but the timing differs from a 3-replica cluster — the wait
# bound here (240 s default) covers ESO's default 5-minute reconcile window
# minus a small headroom for kubectl latency.
wait_for_css_not_ready() {
  local deadline=$(( $(date +%s) + CSS_READY_FALSE_TIMEOUT ))
  log "Waiting up to ${CSS_READY_FALSE_TIMEOUT}s for ClusterSecretStore/${CSS_NAME} to report Ready=False..."
  while true; do
    local status
    status=$(kubectl get clustersecretstore "${CSS_NAME}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) \
      || status=""
    if [[ "${status}" == "False" ]]; then
      log "  ClusterSecretStore/${CSS_NAME} is Ready=False (sealed cluster confirmed)"
      return 0
    fi
    if [[ $(date +%s) -ge ${deadline} ]]; then
      log "ERROR: ClusterSecretStore/${CSS_NAME} did not transition to Ready=False within ${CSS_READY_FALSE_TIMEOUT}s (last observed status: ${status:-<absent>})"
      kubectl get clustersecretstore "${CSS_NAME}" -o yaml 2>&1 || true
      kubectl get pods -n "${BAO_NAMESPACE}" -l app.kubernetes.io/name=openbao -o wide 2>&1 || true
      exit 1
    fi
    sleep "${CSS_POLL_INTERVAL}"
  done
}

main() {
  log "Recovering root token from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local token
  token=$(openbao_read_root_token)

  # Scrub the token from this script's environment on any exit path (success,
  # set -e failure, signal) — mirrors hack/deploy-infra.sh:759 (CC-0010).
  trap 'token=""' EXIT

  log "Sealing OpenBao replicas..."
  local sealed_present=0
  local pod
  for pod in "${PODS[@]}"; do
    if openbao_pod_exists "${pod}"; then
      sealed_present=$(( sealed_present + 1 ))
    fi
    seal_pod "${pod}" "${token}"
  done

  if [[ "${sealed_present}" -eq 0 ]]; then
    log "ERROR: none of ${PODS[*]} exist in ${BAO_NAMESPACE} — cannot seal"
    exit 1
  fi
  log "Sealed ${sealed_present} OpenBao replica(s); validating ClusterSecretStore observes the cluster as NotReady..."

  # Belt-and-braces post-condition: ESO MUST observe the sealed cluster and
  # transition the ClusterSecretStore to Ready=False before the chainsaw test
  # proceeds to its Step 4 SecretsReady=False assertion. Without this gate,
  # a topology mismatch (single-replica kind cluster where some helpers'
  # behaviour differs from production HA Raft) would silently fall through
  # and surface as a generic Step 4 timeout.
  wait_for_css_not_ready

  log "All present OpenBao replicas are sealed and ClusterSecretStore is Ready=False"
}

main "$@"
