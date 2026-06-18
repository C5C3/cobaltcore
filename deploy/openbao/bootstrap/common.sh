#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# common.sh — Shared shell functions for OpenBao bootstrap scripts.
#
# Source this file at the top of each bootstrap script:
#   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   # shellcheck source=common.sh
#   source "${SCRIPT_DIR}/common.sh"

# ---------------------------------------------------------------------------
# Shared configuration defaults
# ---------------------------------------------------------------------------
NAMESPACE="${NAMESPACE:-openbao-system}"
BAO_ADDR="${BAO_ADDR:-https://127.0.0.1:8200}"
export VAULT_CACERT="${VAULT_CACERT:-/openbao/tls/ca.crt}"
export VAULT_CLIENT_CERT="${VAULT_CLIENT_CERT:-/openbao/client-tls/tls.crt}"
export VAULT_CLIENT_KEY="${VAULT_CLIENT_KEY:-/openbao/client-tls/tls.key}"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# bao_exec — Execute a bao CLI command inside the openbao-0 pod.
# Requires BAO_TOKEN to be set in the calling script's environment.
# TODO BAO_TOKEN is passed via `env` positional arg, making it
#   visible in /proc/<pid>/cmdline on both the operator workstation and the
#   container. A future hardening pass should inject the token via a mounted
#   K8s Secret or tmpfs file inside the pod instead.
# ---------------------------------------------------------------------------
bao_exec() {
  kubectl exec -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" VAULT_CLIENT_CERT="${VAULT_CLIENT_CERT}" VAULT_CLIENT_KEY="${VAULT_CLIENT_KEY}" "$@"
}

# ---------------------------------------------------------------------------
# bao_exec_stdin — Like bao_exec but with stdin forwarding (-i flag).
# Used when piping content to bao commands (e.g., policy write from stdin).
# See bao_exec TODO regarding BAO_TOKEN process-listing exposure.
# ---------------------------------------------------------------------------
bao_exec_stdin() {
  kubectl exec -i -n "$NAMESPACE" openbao-0 -- \
    env BAO_ADDR="${BAO_ADDR}" BAO_TOKEN="${BAO_TOKEN}" VAULT_CACERT="${VAULT_CACERT}" VAULT_CLIENT_CERT="${VAULT_CLIENT_CERT}" VAULT_CLIENT_KEY="${VAULT_CLIENT_KEY}" "$@"
}
