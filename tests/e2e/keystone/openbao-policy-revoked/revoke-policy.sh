#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/keystone/openbao-policy-revoked/revoke-policy.sh — Delete the
# push-keystone-keys policy from OpenBao to inject the failure.
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

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] revoke-policy: $*"
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

# Returns 0 if the named policy currently exists, 1 otherwise.
policy_exists() {
  local token="$1"
  kubectl exec -n "${BAO_NAMESPACE}" "${BAO_POD}" -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${token}" VAULT_CACERT="${VAULT_CACERT}" \
    bao policy read "${POLICY_NAME}" >/dev/null 2>&1
}

main() {
  log "Recovering root token from Secret ${BAO_NAMESPACE}/${SECRET_NAME}..."
  local token
  token=$(read_root_token)
  trap 'token=""' EXIT

  if ! policy_exists "${token}"; then
    log "Policy '${POLICY_NAME}' is already absent — no-op (idempotent re-run)"
    return 0
  fi

  log "Deleting policy '${POLICY_NAME}' in ${BAO_NAMESPACE}/${BAO_POD}..."
  kubectl exec -n "${BAO_NAMESPACE}" "${BAO_POD}" -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${token}" VAULT_CACERT="${VAULT_CACERT}" \
    bao policy delete "${POLICY_NAME}" > /dev/null

  if policy_exists "${token}"; then
    log "ERROR: policy '${POLICY_NAME}' still present after delete — bao did not honor the request"
    exit 1
  fi

  log "Policy '${POLICY_NAME}' deleted; PushSecret writes to OpenBao will now 403"
}

main "$@"
