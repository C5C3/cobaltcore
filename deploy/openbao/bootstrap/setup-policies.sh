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
main() {
  log "=== Applying OpenBao policies from ${POLICIES_DIR} ==="

  shopt -s nullglob
  hcl_files=("${POLICIES_DIR}"/*.hcl)
  shopt -u nullglob

  if [[ ${#hcl_files[@]} -eq 0 ]]; then
    log "ERROR: No .hcl files found in ${POLICIES_DIR}."
    exit 1
  fi

  for policy_file in "${hcl_files[@]}"; do
    policy_name="$(basename "${policy_file}" .hcl)"
    log "Writing policy '${policy_name}' from ${policy_file}..."
    cat "${policy_file}" | bao_exec_stdin bao policy write "${policy_name}" -
    log "Policy '${policy_name}' written."
  done

  log "All ${#hcl_files[@]} policies applied."
}

main "$@"
