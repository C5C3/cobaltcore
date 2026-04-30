#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/lib/openbao.sh - Shared helpers for E2E test scripts that talk to OpenBao.
# Feature: CC-0102
#
# This file is sourced by E2E test helpers under tests/e2e/keystone/openbao-*/
# and tests/e2e-chaos/ that need to read the root token, run commands inside the
# openbao StatefulSet replica pods, or check pod presence/seal state.
# Centralising these helpers eliminates the four-way drift identified in the
# CC-0102 review (the read_root_token / pod_exists / bao_exec helpers were
# previously duplicated verbatim across seal-all.sh, capture-policy.sh,
# revoke-policy.sh, and restore-policy.sh).
#
# Contract for callers:
#   - The caller MUST run 'set -euo pipefail' BEFORE sourcing this file.
#   - The caller MUST define a log() function before invoking the helpers.
#   - The caller MAY override BAO_NAMESPACE / SECRET_NAME / BAO_ADDR /
#     VAULT_CACERT / KEY_THRESHOLD via env BEFORE sourcing.
#
# Why a sourced library and not a wrapper script?
#   - The helpers need access to the caller's variables and need to influence
#     the caller's trap chain (token scrubbing). A standalone wrapper would
#     either duplicate those ergonomics or force every caller into a brittle
#     env-export dance.
#   - There is precedent in tests/lib/assertions.sh (CC-0006), which is sourced
#     from shell test scripts that opt in to assert_* matchers.
#
# Why not source deploy/openbao/bootstrap/common.sh?
#   - common.sh is production code with different invariants (it runs inside
#     the bootstrap Job and assumes BAO_TOKEN is already exported). Coupling
#     production scripts to test code would create a long-lived inversion of
#     dependency direction.
#
# REQ-001, REQ-002, REQ-003, REQ-008 (CC-0102)

# Defaults - callers can override any of these by exporting BEFORE sourcing.
#
# Use BAO_NAMESPACE (not NAMESPACE) because Chainsaw injects $NAMESPACE into
# the script env from the test's spec.namespace (openstack), which would
# silently override a NAMESPACE default and make us look for openbao-* in the
# wrong namespace.
: "${BAO_NAMESPACE:=openbao-system}"
: "${SECRET_NAME:=openbao-init-keys}"
: "${BAO_ADDR:=https://127.0.0.1:8200}"
: "${VAULT_CACERT:=/openbao/tls/ca.crt}"
: "${KEY_THRESHOLD:=3}"

# Recover the OpenBao root token from the init-output Secret populated by
# hack/deploy-infra.sh::openbao_init_unseal at bootstrap. Hard-fails with a
# clear pointer to the missing Secret if init has not run, so the test does
# not silently authenticate as anonymous and produce confusing 403s.
#
# Stdout: the root token (no trailing newline). The caller is expected to
# trap-scrub the captured value (see seal-all.sh for the pattern).
openbao_read_root_token() {
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

# Read the first KEY_THRESHOLD unseal keys from the init-output Secret and
# emit them on stdout, one per line. Hard-fails if the Secret is missing or
# any of the first KEY_THRESHOLD keys is empty.
openbao_read_unseal_keys() {
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

# Returns 0 if the named pod exists in BAO_NAMESPACE, 1 otherwise. Callers use
# this to tolerate single-replica kind topologies (where openbao-1 / openbao-2
# are not part of the cluster).
openbao_pod_exists() {
  local pod="$1"
  kubectl get pod "${pod}" -n "${BAO_NAMESPACE}" >/dev/null 2>&1
}

# Returns 0 if 'bao status' reports sealed=true. Provides clearer logging than
# relying on 'bao operator seal''s own no-op behaviour for idempotency.
openbao_pod_is_sealed() {
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

# OpenBao's upstream readiness probe is GET /v1/sys/health, which only returns
# 200 when the pod is initialized AND unsealed AND active. The kubelet's Ready
# condition therefore tracks unseal state directly - far more reliable than
# re-running 'bao status' post-unseal: the bao client occasionally returns
# empty stdout while the listener re-initialises, while the readiness probe
# uses an in-process HTTP path that does not depend on TLS or the bao CLI.
openbao_pod_is_ready() {
  local pod="$1"
  local ready
  ready=$(kubectl get pod "${pod}" -n "${BAO_NAMESPACE}" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) \
    || ready=""
  [[ "${ready}" == "True" ]]
}

# Run a 'bao' (or any) command inside the named OpenBao pod. The first
# argument is the pod name; remaining arguments form the command to run.
# BAO_ADDR and VAULT_CACERT are set via env so the bao CLI uses the in-pod
# TLS configuration. BAO_TOKEN is NOT set here - callers that need an
# authenticated context must pass it explicitly via openbao_bao_run_auth.
openbao_bao_run() {
  local pod="$1"
  shift
  kubectl exec -n "${BAO_NAMESPACE}" "${pod}" -- \
    env BAO_ADDR="${BAO_ADDR}" VAULT_CACERT="${VAULT_CACERT}" "$@"
}

# Authenticated variant of openbao_bao_run. The caller passes the root token
# as the second argument so this helper does not capture it via global state.
openbao_bao_run_auth() {
  local pod="$1"
  local token="$2"
  shift 2
  kubectl exec -n "${BAO_NAMESPACE}" "${pod}" -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${token}" VAULT_CACERT="${VAULT_CACERT}" "$@"
}

# Authenticated 'bao' invocation that reads HCL or other policy text from
# stdin (forwarded to the pod via 'kubectl exec -i'). Used by restore-policy.sh
# for 'bao policy write <name> -' which expects the body on stdin (CC-0102
# review fix: avoids passing secret-adjacent payloads as positional args, which
# would expose them in /proc/<pid>/cmdline on the host).
openbao_bao_run_auth_stdin() {
  local pod="$1"
  local token="$2"
  shift 2
  kubectl exec -i -n "${BAO_NAMESPACE}" "${pod}" -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${token}" VAULT_CACERT="${VAULT_CACERT}" "$@"
}
