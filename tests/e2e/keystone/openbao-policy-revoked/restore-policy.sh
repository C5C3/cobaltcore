#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/openbao-policy-revoked/restore-policy.sh — Re-apply the
# push-keystone-keys policy from the snapshot taken by capture-policy.sh.
#
# This is the recovery / cleanup step of the openbao-policy-revoked E2E test
# (CC-0102). It restores the policy that revoke-policy.sh deleted so that:
#   - the PushSecret <name>-fernet-keys-backup transitions back to Ready=True
#     (REQ-003 recovery assertion), and
#   - the cluster is left in the same state every other test expects (REQ-008
#     idempotent cleanup; REQ-006 inter-test isolation).
#
# Source of truth precedence:
#   1. ${CAPTURE_PATH:-/tmp/push-keystone-keys.hcl.captured} — the live policy
#      snapshot taken by capture-policy.sh.
#   2. ${FALLBACK_HCL:-/dev/null} — explicit override (kept for debugging /
#      manual recovery; defaults to no fallback).
# If neither is available, the script hard-fails with a clear pointer rather
# than restoring an empty policy (which would leave PushSecret writes broken
# silently). The test's `finally:` block runs this script unconditionally; a
# missing capture file therefore signals that capture-policy.sh never ran
# (e.g. apply step crashed before it could) — in that case the test's pre-test
# state was the same as cluster start-up state, which already had the policy,
# so we instead refuse to act and surface the situation in CI logs.
#
# Idempotency:
#   `bao policy write` is an upsert; re-running on an already-present policy
#   is a no-op in effect (CC-0102, REQ-008). We do not skip the write when
#   the policy already exists because we want the post-condition (cluster
#   policy == captured snapshot) to be enforced regardless of starting state.
#
# REQ-003, REQ-008 (CC-0102)

set -euo pipefail

# Note: use BAO_NAMESPACE (not NAMESPACE) because chainsaw injects $NAMESPACE
# into script env from the test's spec.namespace (openstack), which would
# silently override a NAMESPACE default and make us look for openbao-0 in the
# wrong namespace.
BAO_NAMESPACE="${BAO_NAMESPACE:-openbao-system}"
BAO_POD="${BAO_POD:-openbao-0}"
SECRET_NAME="${SECRET_NAME:-openbao-init-keys}"
BAO_ADDR="${BAO_ADDR:-https://127.0.0.1:8200}"
VAULT_CACERT="${VAULT_CACERT:-/openbao/tls/ca.crt}"
POLICY_NAME="${POLICY_NAME:-push-keystone-keys}"
CAPTURE_PATH="${CAPTURE_PATH:-/tmp/push-keystone-keys.hcl.captured}"
FALLBACK_HCL="${FALLBACK_HCL:-}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] restore-policy: $*"
}

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

# Pick the source of policy text. Returns the path to a non-empty file on
# stdout, or exits non-zero with a clear error if no source is available.
pick_source() {
  if [[ -s "${CAPTURE_PATH}" ]]; then
    printf '%s' "${CAPTURE_PATH}"
    return 0
  fi
  if [[ -n "${FALLBACK_HCL}" && -s "${FALLBACK_HCL}" ]]; then
    log "WARN: capture file ${CAPTURE_PATH} missing/empty; using FALLBACK_HCL=${FALLBACK_HCL}"
    printf '%s' "${FALLBACK_HCL}"
    return 0
  fi
  log "ERROR: no policy source available — capture-policy.sh did not produce ${CAPTURE_PATH}"
  log "  refusing to restore an empty policy (would leave PushSecret writes broken)"
  log "  if the test never ran capture-policy.sh, the cluster's pre-test policy is unchanged and no restore is needed"
  exit 1
}

main() {
  local source_path
  source_path=$(pick_source)

  log "Recovering root token from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local token
  token=$(read_root_token)
  trap 'token=""' EXIT

  log "Writing policy '${POLICY_NAME}' from ${source_path} ($(wc -c < "${source_path}") bytes) to ${BAO_NAMESPACE}/${BAO_POD}..."
  # `bao policy write <name> -` reads the HCL body from stdin (upsert).
  # `kubectl exec -i` forwards stdin into the container.
  kubectl exec -i -n "${BAO_NAMESPACE}" "${BAO_POD}" -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${token}" VAULT_CACERT="${VAULT_CACERT}" \
    bao policy write "${POLICY_NAME}" - < "${source_path}" > /dev/null

  # Verify the policy is present after the write — guards against the
  # symmetrical failure mode of capture-policy.sh's empty-payload check.
  if ! kubectl exec -n "${BAO_NAMESPACE}" "${BAO_POD}" -- \
      env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${token}" VAULT_CACERT="${VAULT_CACERT}" \
      bao policy read "${POLICY_NAME}" >/dev/null 2>&1; then
    log "ERROR: policy '${POLICY_NAME}' is still absent after 'bao policy write' — bao did not honor the request"
    exit 1
  fi

  log "Policy '${POLICY_NAME}' restored; PushSecret writes to OpenBao should re-converge"
}

main "$@"
