#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# teardown-infra.sh — Tears down the local kind infrastructure cluster.
# Feature: CC-0010

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-c5c3}"

log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

log "Tearing down kind cluster '${CLUSTER_NAME}'..."

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  kind delete cluster --name "${CLUSTER_NAME}"
  log "Cluster '${CLUSTER_NAME}' deleted successfully."
else
  log "Cluster '${CLUSTER_NAME}' does not exist. Nothing to do."
  exit 0
fi
