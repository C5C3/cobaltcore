#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/teardown-infra.sh — Delete the kind E2E cluster.

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-forge}"

# When true, also remove the opt-in registry pull-through caches (#564) — the
# proxy containers AND their persistent volumes. Defaults to false: deleting the
# cache on every teardown would defeat its whole point (surviving kind delete /
# recreate cycles), so a plain `make teardown-infra` leaves the warm cache
# running for the next `WITH_REGISTRY_CACHE=true make deploy-infra`. Set
# PURGE_REGISTRY_CACHE=true to reclaim the disk / start cold.
PURGE_REGISTRY_CACHE="${PURGE_REGISTRY_CACHE:-false}"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# purge_registry_cache — Remove every registry pull-through cache container and
# volume, identified by the forge.registry-cache=true label that
# start_registry_cache stamps on them. Decoupled from the upstream set (no need
# to know the registry list here) and best-effort throughout. No-op unless
# PURGE_REGISTRY_CACHE=true.
# ---------------------------------------------------------------------------
purge_registry_cache() {
  if [[ "${PURGE_REGISTRY_CACHE}" != "true" ]]; then
    log "Leaving registry pull-through caches running (set PURGE_REGISTRY_CACHE=true to remove them)."
    return 0
  fi

  if ! command -v docker >/dev/null 2>&1; then
    log "WARNING: docker not found — cannot purge registry caches."
    return 0
  fi

  log "Purging registry pull-through caches (PURGE_REGISTRY_CACHE=true)..."

  local containers volumes
  containers=$(docker ps -aq --filter label=forge.registry-cache=true 2>/dev/null) || true
  if [[ -n "${containers}" ]]; then
    # shellcheck disable=SC2086
    docker rm -f ${containers} >/dev/null 2>&1 || true
    log "  Removed registry-cache container(s)."
  fi

  volumes=$(docker volume ls -q --filter label=forge.registry-cache=true 2>/dev/null) || true
  if [[ -n "${volumes}" ]]; then
    # shellcheck disable=SC2086
    docker volume rm ${volumes} >/dev/null 2>&1 || true
    log "  Removed registry-cache volume(s)."
  fi

  if [[ -z "${containers}" && -z "${volumes}" ]]; then
    log "  No registry-cache containers or volumes found."
  fi
}

main() {
  log "=== Teardown Infrastructure ==="
  log "Deleting kind cluster '${CLUSTER_NAME}'..."
  kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
  log "Cluster '${CLUSTER_NAME}' deleted (or did not exist)."
  purge_registry_cache
  log "=== Done ==="
}

# Run main only when executed directly so unit tests (tests/unit/hack/) can
# source this script and exercise purge_registry_cache in isolation.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
