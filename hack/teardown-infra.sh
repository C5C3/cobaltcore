#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/teardown-infra.sh — Delete the kind E2E cluster.
# Feature: CC-0010

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-forge-e2e}"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

main() {
  log "=== Teardown Infrastructure ==="
  log "Deleting kind cluster '${CLUSTER_NAME}'..."
  kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
  log "Cluster '${CLUSTER_NAME}' deleted (or did not exist)."
  log "=== Done ==="
}

main "$@"
