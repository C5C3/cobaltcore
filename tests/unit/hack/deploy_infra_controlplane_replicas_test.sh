#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `render_controlplane_replicas` pins the projected
# backing-service footprint from CONTROLPLANE_DB_REPLICAS / CONTROLPLANE_CACHE_REPLICAS
# and rejects an invalid footprint (non-numeric, < 1, or the quorum-unsafe DB=2)
# before the CR is applied. Also guards that the checked-in bundled kind CR ships a
# single-node topology (database.replicas=1, cache.replicas=1) so a laptop-sized
# single-node kind does not spin up a 3-node Galera cluster and OOM.
#
# Sources deploy-infra.sh and invokes the function in a subshell so we can assert
# against a rendered tempfile without spinning up an actual cluster. The yq-backed
# checks are skipped when `yq` is not installed.
#
# Usage: bash tests/unit/hack/deploy_infra_controlplane_replicas_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"
BUNDLED_CR="$PROJECT_ROOT/deploy/kind/controlplane/controlplane.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The name of the ControlPlane CR in the bundled fixture. render_controlplane_replicas
# name-scopes its yq selector to ${CONTROLPLANE_NAME}, so the tests pin this to the
# fixture's metadata.name explicitly rather than leaning on deploy-infra.sh's default —
# the tests stay green even if that default is renamed later.
FIXTURE_CONTROLPLANE_NAME="controlplane"

# Source the script and call render_controlplane_replicas in a subshell with the
# given DB/cache replica values, mutating <manifest> in place. Echoes combined
# output and returns the function's exit status. The subshell isolates env
# mutations and the BASH_SOURCE guard at the bottom of deploy-infra.sh keeps main
# from running when sourced.
run_render() {
  local manifest="$1"
  local db="$2"
  local cache="$3"
  (
    export CONTROLPLANE_DB_REPLICAS="${db}"
    export CONTROLPLANE_CACHE_REPLICAS="${cache}"
    export CONTROLPLANE_NAME="${FIXTURE_CONTROLPLANE_NAME}"
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    render_controlplane_replicas "${manifest}"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: the checked-in bundled CR ships a single-node topology.
# ---------------------------------------------------------------------------
test_bundled_cr_is_single_node() {
  echo "Test: bundled kind ControlPlane CR pins database/cache replicas to 1"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  assert_eq "bundled CR database.replicas is 1" \
    "1" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.replicas' "$BUNDLED_CR")"
  assert_eq "bundled CR cache.replicas is 1" \
    "1" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.cache.replicas' "$BUNDLED_CR")"
}

# ---------------------------------------------------------------------------
# Test 2: default footprint (both = 1) rewrites to integer 1/1.
# ---------------------------------------------------------------------------
test_default_footprint() {
  echo "Test: render_controlplane_replicas with 1/1 yields integer replicas 1/1"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  local out="$tmp/cr.yaml"
  cp "$BUNDLED_CR" "$out"

  local exit_code
  run_render "$out" "1" "1" >/dev/null
  exit_code=$?

  assert_eq "render exits 0 for 1/1" "0" "$exit_code"
  assert_eq "database.replicas is 1" \
    "1" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.replicas' "$out")"
  # `... | tag` reports the node type; integer keeps the CRD schema happy.
  assert_eq "database.replicas stays an integer node" \
    "!!int" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.replicas | tag' "$out")"
}

# ---------------------------------------------------------------------------
# Test 3: an HA override (DB=3 Galera, cache=2) is projected as integers.
# ---------------------------------------------------------------------------
test_ha_override() {
  echo "Test: render_controlplane_replicas with 3/2 projects a Galera-sized footprint"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  local out="$tmp/cr.yaml"
  cp "$BUNDLED_CR" "$out"

  local exit_code
  run_render "$out" "3" "2" >/dev/null
  exit_code=$?

  assert_eq "render exits 0 for 3/2" "0" "$exit_code"
  assert_eq "database.replicas is 3 (Galera)" \
    "3" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.replicas' "$out")"
  assert_eq "cache.replicas is 2" \
    "2" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.cache.replicas' "$out")"
}

# ---------------------------------------------------------------------------
# Test 4: invalid footprints fail fast (before kubectl apply).
# ---------------------------------------------------------------------------
test_invalid_footprint_rejected() {
  echo "Test: render_controlplane_replicas rejects non-numeric, < 1, and DB=2"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  local out="$tmp/cr.yaml"
  cp "$BUNDLED_CR" "$out"

  local output exit_code

  output="$(run_render "$out" "not-a-number" "1")"
  exit_code=$?
  assert_nonzero_exit "non-numeric CONTROLPLANE_DB_REPLICAS exits non-zero" "$exit_code"
  assert_contains "non-numeric error surfaces the offending value" \
    "$output" "CONTROLPLANE_DB_REPLICAS='not-a-number'"

  output="$(run_render "$out" "0" "1")"
  exit_code=$?
  assert_nonzero_exit "CONTROLPLANE_DB_REPLICAS=0 exits non-zero" "$exit_code"

  output="$(run_render "$out" "1" "0")"
  exit_code=$?
  assert_nonzero_exit "CONTROLPLANE_CACHE_REPLICAS=0 exits non-zero" "$exit_code"

  output="$(run_render "$out" "2" "1")"
  exit_code=$?
  assert_nonzero_exit "quorum-unsafe CONTROLPLANE_DB_REPLICAS=2 exits non-zero" "$exit_code"
  assert_contains "DB=2 error explains the Galera quorum constraint" \
    "$output" "quorum"
}

# ---------------------------------------------------------------------------
# Test 5: main() wires the knobs — the function is called, the env vars are
# declared with a single-node default, and they are documented in the preamble.
# Static text checks keep this independent of stub plumbing.
# ---------------------------------------------------------------------------
test_main_wires_knobs() {
  echo "Test: main() calls render_controlplane_replicas and declares the knobs"

  assert_file_contains "render_controlplane_replicas is called from main()" \
    "$DEPLOY_INFRA_SH" "render_controlplane_replicas "
  assert_file_contains "CONTROLPLANE_DB_REPLICAS defaults to 1" \
    "$DEPLOY_INFRA_SH" 'CONTROLPLANE_DB_REPLICAS="${CONTROLPLANE_DB_REPLICAS:-1}"'
  assert_file_contains "CONTROLPLANE_CACHE_REPLICAS defaults to 1" \
    "$DEPLOY_INFRA_SH" 'CONTROLPLANE_CACHE_REPLICAS="${CONTROLPLANE_CACHE_REPLICAS:-1}"'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_bundled_cr_is_single_node
test_default_footprint
test_ha_override
test_invalid_footprint_rejected
test_main_wires_knobs

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
