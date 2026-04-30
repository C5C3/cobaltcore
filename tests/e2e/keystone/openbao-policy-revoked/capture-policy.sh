#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/openbao-policy-revoked/capture-policy.sh — Snapshot the
# active push-keystone-keys policy from OpenBao before revocation.
# Feature: CC-0102
#
# The openbao-policy-revoked E2E test deletes the push-keystone-keys policy to
# force the management-cluster ESO PushSecret writes (rotation backups) to fail
# with HTTP 403, then recovers by re-applying the policy. This helper runs in
# the "before" half of that flow and captures the live policy text so the
# matching restore-policy.sh helper can re-write it byte-for-byte during the
# recovery / cleanup steps.
#
# Why dump the live policy rather than re-applying the in-repo HCL file?
#   - Drift insurance. The cluster under test might be running an older or
#     patched policy (e.g. during a release-train rollout); restoring the
#     in-repo HEAD copy would silently bump the cluster's policy and pollute
#     subsequent tests. Capturing the live policy guarantees end-of-test
#     state == start-of-test state (REQ-008 idempotent recovery).
#   - The cluster is the source of truth for the running policy; the HCL file
#     is the source of truth for the *desired* policy. The test asserts a
#     transient failure mode, not a desired-state migration, so the live
#     snapshot is the correct reference.
#
# Output: writes the policy text to ${CAPTURE_PATH:-/tmp/push-keystone-keys.hcl.captured}.
# The path is on the chainsaw runner's filesystem (not the cluster); restore-policy.sh
# reads from the same path. Both helpers share a default that the chainsaw
# step env can override.
#
# Idempotency: re-running this script overwrites the capture file with the
# current cluster state. If the policy was already revoked when this runs,
# `bao policy read` returns rc!=0 and a non-empty stderr — we hard-fail
# (REQ-003 says the test asserts the *transition* from healthy → revoked, so
# starting from a missing-policy state would invalidate the test).
#
# Failure modes:
#   - Missing init-output Secret → hard fail (cannot authenticate to bao).
#   - Empty / missing policy → hard fail (refuse to capture an empty payload
#     and silently restore an empty policy later).
#   - Captured file exists but is empty → hard fail.
#
# REQ-003, REQ-008 (CC-0102)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_DIR="${SCRIPT_DIR}/../../../lib"

BAO_POD="${BAO_POD:-openbao-0}"
POLICY_NAME="${POLICY_NAME:-push-keystone-keys}"
CAPTURE_PATH="${CAPTURE_PATH:-/tmp/push-keystone-keys.hcl.captured}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] capture-policy: $*"
}

# shellcheck source=tests/lib/openbao.sh
source "${LIB_DIR}/openbao.sh"

main() {
  log "Recovering root token from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local token
  token=$(openbao_read_root_token)
  # Scrub the token from this script's environment on any exit path.
  trap 'token=""' EXIT

  log "Reading policy '${POLICY_NAME}' from ${BAO_NAMESPACE}/${BAO_POD}..."
  local policy_text
  if ! policy_text=$(openbao_bao_run_auth "${BAO_POD}" "${token}" \
      bao policy read "${POLICY_NAME}" 2>/dev/null); then
    log "ERROR: 'bao policy read ${POLICY_NAME}' failed — does the policy exist?"
    log "  (the test pre-condition is that ${POLICY_NAME} is present and active before revoke-policy.sh runs)"
    exit 1
  fi

  if [[ -z "${policy_text}" ]]; then
    log "ERROR: 'bao policy read ${POLICY_NAME}' returned an empty payload"
    log "  refusing to capture an empty policy — restore-policy.sh would silently apply an empty policy on cleanup"
    exit 1
  fi

  # Atomic-ish write: stage to a sibling tmp file, then rename. Avoids a
  # half-written CAPTURE_PATH if restore-policy.sh runs after a SIGTERM.
  local stage="${CAPTURE_PATH}.tmp.$$"
  printf '%s' "${policy_text}" > "${stage}"
  mv -f "${stage}" "${CAPTURE_PATH}"

  if [[ ! -s "${CAPTURE_PATH}" ]]; then
    log "ERROR: post-write check: ${CAPTURE_PATH} is empty (filesystem issue?)"
    exit 1
  fi

  log "Captured ${POLICY_NAME} ($(wc -c < "${CAPTURE_PATH}") bytes) to ${CAPTURE_PATH}"
}

main "$@"
