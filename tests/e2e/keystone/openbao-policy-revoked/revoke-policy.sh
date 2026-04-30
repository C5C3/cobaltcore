#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/openbao-policy-revoked/revoke-policy.sh — Delete the
# push-keystone-keys policy from OpenBao to inject the failure.
# Feature: CC-0102
#
# This is the failure-injection step of the openbao-policy-revoked E2E test
# (CC-0102). With push-keystone-keys deleted, the management-cluster ESO role
# loses the only grant on `kv-v2/data/openstack/keystone/+/fernet-keys` (and
# the matching credential-keys / metadata paths — see
# deploy/openbao/policies/push-keystone-keys.hcl). The PushSecret created by
# reconcileFernetKeys (`<name>-fernet-keys-backup`) — and any rotation Job
# that re-pushes the staged secret — will then be rejected by OpenBao with a
# 403 forbidden response, surfacing the `Ready=False`/error condition the
# test asserts (REQ-003).
#
# Why an explicit revocation rather than removing the role binding?
#   The role binding controls *every* policy attached to the management-cluster
#   ESO auth role (eso-management + push-keystone-keys + …). Removing the
#   binding would also strip read access on bootstrap/* and infrastructure/*,
#   pushing far more of the operator into a failure mode than the test wants
#   to assert. Deleting the single push-keystone-keys policy cleanly isolates
#   the PushSecret-write path while leaving every read path untouched
#   (the test asserts SecretsReady stays True — that requires the read-side
#   eso-management policy to remain active).
#
# Idempotency:
#   `bao policy delete` is a no-op against an already-deleted policy and exits
#   0 (CC-0102, REQ-008). We additionally fast-path the no-op by checking
#   `bao policy read` first so the success log line is honest about whether
#   any state changed.
#
# REQ-003, REQ-008 (CC-0102)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_DIR="${SCRIPT_DIR}/../../../lib"

BAO_POD="${BAO_POD:-openbao-0}"
POLICY_NAME="${POLICY_NAME:-push-keystone-keys}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] revoke-policy: $*"
}

# shellcheck source=tests/lib/openbao.sh
source "${LIB_DIR}/openbao.sh"

# Returns 0 if the named policy currently exists, 1 otherwise.
policy_exists() {
  local token="$1"
  openbao_bao_run_auth "${BAO_POD}" "${token}" \
    bao policy read "${POLICY_NAME}" >/dev/null 2>&1
}

main() {
  log "Recovering root token from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local token
  token=$(openbao_read_root_token)
  trap 'token=""' EXIT

  if ! policy_exists "${token}"; then
    log "Policy '${POLICY_NAME}' is already absent — no-op (idempotent re-run)"
    return 0
  fi

  log "Deleting policy '${POLICY_NAME}' in ${BAO_NAMESPACE}/${BAO_POD}..."
  openbao_bao_run_auth "${BAO_POD}" "${token}" \
    bao policy delete "${POLICY_NAME}" > /dev/null

  if policy_exists "${token}"; then
    log "ERROR: policy '${POLICY_NAME}' still present after delete — bao did not honor the request"
    exit 1
  fi

  log "Policy '${POLICY_NAME}' deleted; PushSecret writes to OpenBao will now 403"
}

main "$@"
