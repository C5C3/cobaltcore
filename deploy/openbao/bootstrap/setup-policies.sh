#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# setup-policies.sh — Apply all HCL policies to OpenBao.
#
# This script is idempotent: `bao policy write` performs an upsert, so
# re-running it with the same policy content is a no-op in effect.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

###############################################################################
# Configuration
###############################################################################
BAO_TOKEN="${BAO_TOKEN:?BAO_TOKEN must be set}"
POLICIES_DIR="${POLICIES_DIR:-$(cd "$(dirname "$0")/../policies" && pwd)}"

###############################################################################
# Main
###############################################################################

# ACCESSOR_PLACEHOLDER is the token that policies scoping reads by the caller's
# service-account namespace (OpenBao ACL identity templating) carry in place of
# the kubernetes/management auth-mount accessor. The accessor is generated when
# the mount is enabled (setup-auth.sh, run before this script) and is not known
# until runtime, so it is substituted at write time here.
ACCESSOR_PLACEHOLDER="KUBERNETES_MANAGEMENT_ACCESSOR"

main() {
  log "=== Applying OpenBao policies from ${POLICIES_DIR} ==="

  shopt -s nullglob
  hcl_files=("${POLICIES_DIR}"/*.hcl)
  shopt -u nullglob

  if [[ ${#hcl_files[@]} -eq 0 ]]; then
    log "ERROR: No .hcl files found in ${POLICIES_DIR}."
    exit 1
  fi

  # Resolve the kubernetes/management mount accessor only when a policy actually
  # references it, so re-applying policies that do not need templating stays a
  # no-op even before that mount exists.
  local mgmt_accessor=""
  if grep -q "${ACCESSOR_PLACEHOLDER}" "${hcl_files[@]}"; then
    mgmt_accessor="$(bao_exec bao auth list -format=json \
      | jq -r '."kubernetes/management/".accessor // empty')"
    if [[ -z "${mgmt_accessor}" ]]; then
      log "ERROR: a policy references ${ACCESSOR_PLACEHOLDER} but the kubernetes/management auth-mount accessor could not be resolved. Run setup-auth.sh first."
      exit 1
    fi
    log "Resolved kubernetes/management mount accessor for policy templating."
  fi

  for policy_file in "${hcl_files[@]}"; do
    policy_name="$(basename "${policy_file}" .hcl)"
    log "Writing policy '${policy_name}' from ${policy_file}..."
    sed "s|${ACCESSOR_PLACEHOLDER}|${mgmt_accessor}|g" "${policy_file}" \
      | bao_exec_stdin bao policy write "${policy_name}" -
    log "Policy '${policy_name}' written."
  done

  log "All ${#hcl_files[@]} policies applied."
}

main "$@"
