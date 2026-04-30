#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/openbao-sealed/seal-all.sh — Seal every OpenBao replica.
#
# Why all three?
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
# REQ-001, REQ-008 (CC-0102)

set -euo pipefail

# Use BAO_NAMESPACE (not NAMESPACE) because chainsaw injects $NAMESPACE into
# script env from the test's spec.namespace (openstack), which would silently
# override a NAMESPACE default and make us look for openbao-* in the wrong
# namespace.
BAO_NAMESPACE="${BAO_NAMESPACE:-openbao-system}"
PODS=("openbao-0" "openbao-1" "openbao-2")
SECRET_NAME="${SECRET_NAME:-openbao-init-keys}"
BAO_ADDR="${BAO_ADDR:-https://127.0.0.1:8200}"
VAULT_CACERT="${VAULT_CACERT:-/openbao/tls/ca.crt}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] seal-all: $*"
}

# Recover the root token from the init-output Secret. This Secret is
# populated by hack/deploy-infra.sh::openbao_init_unseal at bootstrap; if it
# is missing, OpenBao has not been initialised and the seal action cannot
# authenticate — fail fast with a clear pointer to the missing Secret.
read_root_token() {
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

  local token
  token=$(printf '%s' "${init_output}" | jq -r '.root_token')
  if [[ -z "${token}" || "${token}" == "null" ]]; then
    log "ERROR: root_token missing from Secret ${BAO_NAMESPACE}/${SECRET_NAME}"
    exit 1
  fi
  printf '%s' "${token}"
}

# Returns 0 if the named pod exists in BAO_NAMESPACE, 1 otherwise. Used to
# tolerate single-replica topologies (kind) where openbao-1 / openbao-2 are
# not part of the cluster.
pod_exists() {
  local pod="$1"
  kubectl get pod "${pod}" -n "${BAO_NAMESPACE}" >/dev/null 2>&1
}

# Returns 0 if `bao status` reports sealed=true. This lets us skip an
# already-sealed pod for idempotency without relying on `bao operator seal`'s
# own no-op behaviour (which we still rely on as a backstop, but the explicit
# check produces clearer logs in CI).
pod_is_sealed() {
  local pod="$1"
  local status_json
  # bao status returns rc=2 when sealed; capture stdout regardless.
  status_json=$(kubectl exec -n "${BAO_NAMESPACE}" "${pod}" -- \
    env BAO_ADDR="${BAO_ADDR}" VAULT_CACERT="${VAULT_CACERT}" \
    bao status -format=json 2>/dev/null) || true
  if [[ -z "${status_json}" ]]; then
    return 1
  fi
  echo "${status_json}" | jq -e '.sealed == true' >/dev/null 2>&1
}

seal_pod() {
  local pod="$1"
  local token="$2"

  if ! pod_exists "${pod}"; then
    log "  ${pod} does not exist in ${BAO_NAMESPACE} — skipping (single-replica topology?)"
    return 0
  fi

  if pod_is_sealed "${pod}"; then
    log "  ${pod} is already sealed — skipping"
    return 0
  fi

  log "  sealing ${pod}..."
  kubectl exec -n "${BAO_NAMESPACE}" "${pod}" -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${token}" VAULT_CACERT="${VAULT_CACERT}" \
    bao operator seal > /dev/null
  log "  ${pod} sealed"
}

main() {
  log "Recovering root token from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local token
  token=$(read_root_token)

  # Scrub the token from this script's environment on any exit path (success,
  # set -e failure, signal) — mirrors hack/deploy-infra.sh:759 (CC-0010).
  trap 'token=""' EXIT

  log "Sealing OpenBao replicas..."
  local sealed_any=0
  for pod in "${PODS[@]}"; do
    if pod_exists "${pod}"; then
      sealed_any=1
    fi
    seal_pod "${pod}" "${token}"
  done

  if [[ "${sealed_any}" -eq 0 ]]; then
    log "ERROR: none of ${PODS[*]} exist in ${BAO_NAMESPACE} — cannot seal"
    exit 1
  fi

  log "All present OpenBao replicas are sealed"
}

main "$@"
