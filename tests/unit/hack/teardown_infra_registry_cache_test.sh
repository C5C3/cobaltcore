#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/teardown-infra.sh's registry pull-through cache handling (#564):
# a plain teardown LEAVES the caches running (deleting them defeats the
# survives-recreate purpose), and PURGE_REGISTRY_CACHE=true removes the labelled
# containers AND volumes.
#
# Project-native bash + tests/lib/assertions.sh. The BASH_SOURCE guard at the
# bottom of teardown-infra.sh keeps main() from auto-running when sourced, so
# purge_registry_cache is exercised in isolation against a recording docker stub.
#
# Usage: bash tests/unit/hack/teardown_infra_registry_cache_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
TEARDOWN_SH="$PROJECT_ROOT/hack/teardown-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# make_docker_stub <dir>
# docker appends argv to $DOCKER_LOG. `ps -aq` / `volume ls -q` echo canned IDs
# so the purge path has something to remove; everything else exits 0.
make_docker_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >> "$DOCKER_LOG"
if [ "${1:-}" = "ps" ]; then echo "cid-dockerio cid-ghcr"; fi
if [ "${1:-}" = "volume" ] && [ "${2:-}" = "ls" ]; then echo "registry-cache-dockerio-data registry-cache-ghcr-data"; fi
exit 0
STUB
  chmod +x "$dir/docker"
}

# ---------------------------------------------------------------------------
# Test 1: PURGE_REGISTRY_CACHE defaults to false and is preserved
# ---------------------------------------------------------------------------
test_flag_default() {
  echo "Test: PURGE_REGISTRY_CACHE defaults to false"

  local resolved
  resolved="$(
    unset PURGE_REGISTRY_CACHE
    # shellcheck source=/dev/null
    source "$TEARDOWN_SH"
    printf '%s' "${PURGE_REGISTRY_CACHE}"
  )"
  assert_eq "PURGE_REGISTRY_CACHE defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: default teardown leaves the caches running (no docker removal)
# ---------------------------------------------------------------------------
test_default_leaves_cache() {
  echo "Test: purge_registry_cache leaves the caches running by default"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_docker_stub "$tmp/bin"

  local out
  out="$( (
    PATH="$tmp/bin:$PATH"; export PATH
    export DOCKER_LOG="$tmp/docker.log"; : >"$DOCKER_LOG"
    # shellcheck source=/dev/null
    source "$TEARDOWN_SH"
    PURGE_REGISTRY_CACHE=false purge_registry_cache
  ) 2>&1 )"

  assert_contains "log explains the caches are left running" "$out" "Leaving registry pull-through caches running"
  assert_eq "docker is never invoked on the default (non-purge) path" "" "$(cat "$tmp/docker.log")"
}

# ---------------------------------------------------------------------------
# Test 3: PURGE_REGISTRY_CACHE=true removes labelled containers and volumes
# ---------------------------------------------------------------------------
test_purge_removes_by_label() {
  echo "Test: PURGE_REGISTRY_CACHE=true removes labelled containers and volumes"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_docker_stub "$tmp/bin"

  (
    PATH="$tmp/bin:$PATH"; export PATH
    export DOCKER_LOG="$tmp/docker.log"; : >"$DOCKER_LOG"
    # shellcheck source=/dev/null
    source "$TEARDOWN_SH"
    PURGE_REGISTRY_CACHE=true purge_registry_cache
  ) >/dev/null 2>&1

  local log
  log="$(cat "$tmp/docker.log")"

  assert_contains "containers are looked up by the purge label" \
    "$log" "docker ps -aq --filter label=forge.registry-cache=true"
  assert_contains "volumes are looked up by the purge label" \
    "$log" "docker volume ls -q --filter label=forge.registry-cache=true"
  assert_contains "labelled containers are force-removed" \
    "$log" "docker rm -f cid-dockerio cid-ghcr"
  assert_contains "labelled volumes are removed" \
    "$log" "docker volume rm registry-cache-dockerio-data registry-cache-ghcr-data"
}

# ---------------------------------------------------------------------------
# Test 4: the purge is decoupled from the upstream set (label-based only)
# ---------------------------------------------------------------------------
test_purge_is_label_scoped() {
  echo "Test: purge_registry_cache does not hard-code the upstream registry names"

  # The teardown script must not enumerate the upstream table — it purges purely
  # by label so it never drifts from deploy-infra.sh's upstream set.
  assert_file_not_contains "teardown does not hard-code registry-1.docker.io" \
    "$TEARDOWN_SH" "registry-1.docker.io"
  assert_file_contains "teardown filters on the forge.registry-cache label" \
    "$TEARDOWN_SH" "label=forge.registry-cache=true"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_flag_default
test_default_leaves_cache
test_purge_removes_by_label
test_purge_is_label_scoped

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
