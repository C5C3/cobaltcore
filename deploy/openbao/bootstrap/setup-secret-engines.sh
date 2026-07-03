#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Idempotent script to enable OpenBao secret engines (KV v2, PKI, and the
# MariaDB database secrets engine). Guards each enable operation by checking
# whether the path already exists.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
BAO_TOKEN="${BAO_TOKEN:?BAO_TOKEN must be set}"

# ---------------------------------------------------------------------------
# secrets_list
# Returns the list of currently enabled secret engine paths (one per line).
# ---------------------------------------------------------------------------
secrets_list() {
  bao_exec bao secrets list -format=json | jq -r 'keys[]'
}

# ---------------------------------------------------------------------------
# enable_kv_v2
# Enables the KV v2 secret engine at path kv-v2/.
# Skips if the path already exists.
# ---------------------------------------------------------------------------
enable_kv_v2() {
  local path="kv-v2/"

  if secrets_list | grep -qx "${path}"; then
    log "Secret engine already enabled at ${path}. Skipping."
    return 0
  fi

  log "Enabling KV v2 secret engine at ${path} ..."
  bao_exec bao secrets enable -path=kv-v2 -version=2 kv
  log "KV v2 secret engine enabled at ${path}."
}

# ---------------------------------------------------------------------------
# enable_pki
# Enables the PKI secret engine at path pki/ and tunes the max lease TTL.
# Skips the enable step if the path already exists.
# ---------------------------------------------------------------------------
enable_pki() {
  local path="pki/"

  if secrets_list | grep -qx "${path}"; then
    log "Secret engine already enabled at ${path}. Skipping enable."
    return 0
  fi

  log "Enabling PKI secret engine at ${path} ..."
  bao_exec bao secrets enable -path=pki pki
  log "PKI secret engine enabled at ${path}."

  log "Tuning max-lease-ttl for ${path} to 87600h ..."
  bao_exec bao secrets tune -max-lease-ttl=87600h pki
  log "PKI max-lease-ttl tuned to 87600h."
}

# ---------------------------------------------------------------------------
# enable_database
# Enables the database secrets engine at path database/mariadb/. Skips if the
# path already exists.
#
# NOTE per-tenant connection + role configuration is NOT written here: the
# managed MariaDB instances do not exist at bootstrap time (deploy-infra
# bootstraps OpenBao before any MariaDB is Ready), so mounting the engine is the
# only bootstrap-safe step. Each tenant's connection and role are provisioned
# later by setup-database-tenant.sh once its MariaDB is Ready. The mount path
# matches the architecture handbook (database/mariadb/creds/<role>).
# ---------------------------------------------------------------------------
enable_database() {
  local path="database/mariadb/"

  if secrets_list | grep -qx "${path}"; then
    log "Secret engine already enabled at ${path}. Skipping."
    return 0
  fi

  log "Enabling database secret engine at ${path} ..."
  bao_exec bao secrets enable -path=database/mariadb database
  log "Database secret engine enabled at ${path}."
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  log "=== OpenBao Secret Engines Setup ==="
  log "Namespace : ${NAMESPACE}"
  log "BAO_ADDR  : ${BAO_ADDR}"

  enable_kv_v2
  enable_pki
  enable_database

  log "=== Done ==="
}

main "$@"
