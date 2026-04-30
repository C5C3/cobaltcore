#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/openbao-policy-revoked/restore-policy.sh — Re-apply the
# push-keystone-keys policy from the snapshot taken by capture-policy.sh.
# Feature: CC-0102
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
# silently). The test's per-step `cleanup:` blocks run this script
# unconditionally; a missing capture file therefore signals that
# capture-policy.sh never ran (e.g. apply step crashed before it could) — in
# that case the test's pre-test state was the same as cluster start-up state,
# which already had the policy, so we instead refuse to act and surface the
# situation in CI logs.
#
# Idempotency:
#   `bao policy write` is an upsert; re-running on an already-present policy
#   is a no-op in effect (CC-0102, REQ-008). We do not skip the write when
#   the policy already exists because we want the post-condition (cluster
#   policy == captured snapshot) to be enforced regardless of starting state.
#
# REQ-003, REQ-008 (CC-0102)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_DIR="${SCRIPT_DIR}/../../../lib"

BAO_POD="${BAO_POD:-openbao-0}"
POLICY_NAME="${POLICY_NAME:-push-keystone-keys}"
CAPTURE_PATH="${CAPTURE_PATH:-/tmp/push-keystone-keys.hcl.captured}"
FALLBACK_HCL="${FALLBACK_HCL:-}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] restore-policy: $*"
}

# shellcheck source=tests/lib/openbao.sh
source "${LIB_DIR}/openbao.sh"

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
  token=$(openbao_read_root_token)
  trap 'token=""' EXIT

  log "Writing policy '${POLICY_NAME}' from ${source_path} ($(wc -c < "${source_path}") bytes) to ${BAO_NAMESPACE}/${BAO_POD}..."
  # `bao policy write <name> -` reads the HCL body from stdin (upsert).
  # `kubectl exec -i` forwards stdin into the container (handled by the
  # openbao_bao_run_auth_stdin helper). The HCL body is application data, but
  # routing it via stdin keeps the helper symmetric with unseal-all.sh's
  # stdin-piped key delivery and avoids any future temptation to pass the
  # body as a positional arg, which would be visible on the host's
  # /proc/<pid>/cmdline.
  openbao_bao_run_auth_stdin "${BAO_POD}" "${token}" \
    bao policy write "${POLICY_NAME}" - < "${source_path}" > /dev/null

  # Verify the policy is present after the write — guards against the
  # symmetrical failure mode of capture-policy.sh's empty-payload check.
  if ! openbao_bao_run_auth "${BAO_POD}" "${token}" \
      bao policy read "${POLICY_NAME}" >/dev/null 2>&1; then
    log "ERROR: policy '${POLICY_NAME}' is still absent after 'bao policy write' — bao did not honor the request"
    exit 1
  fi

  log "Policy '${POLICY_NAME}' restored; PushSecret writes to OpenBao should re-converge"
}

main "$@"
