#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `render_controlplane_replicas` pins only the
# backing-service fields whose knob is set (CONTROLPLANE_DB_REPLICAS /
# CONTROLPLANE_CACHE_REPLICAS / CONTROLPLANE_DB_STORAGE), leaves the manifest
# byte-identical when none is, and rejects an invalid footprint (non-numeric or < 1
# replicas, the quorum-unsafe DB=2, or a malformed storage quantity) before the CR
# is applied. Also guards that the checked-in bundled kind CR names
# spec.sizing.profile: Minimal and pins none of the three fields, so the profile
# gives a laptop-sized single-node kind one non-Galera MariaDB on a 512Mi volume
# and one Memcached pod.
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
# given DB/cache replica values and DB storage size, mutating <manifest> in place.
# An empty or omitted value leaves that knob unset. Echoes combined output and
# returns the function's exit status. The subshell isolates env mutations and the
# BASH_SOURCE guard at the bottom of deploy-infra.sh keeps main from running when
# sourced.
run_render() {
  local manifest="$1"
  local db="${2:-}"
  local cache="${3:-}"
  local storage="${4:-}"
  (
    unset CONTROLPLANE_DB_REPLICAS CONTROLPLANE_CACHE_REPLICAS CONTROLPLANE_DB_STORAGE
    [ -n "${db}" ] && export CONTROLPLANE_DB_REPLICAS="${db}"
    [ -n "${cache}" ] && export CONTROLPLANE_CACHE_REPLICAS="${cache}"
    [ -n "${storage}" ] && export CONTROLPLANE_DB_STORAGE="${storage}"
    export CONTROLPLANE_NAME="${FIXTURE_CONTROLPLANE_NAME}"
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    render_controlplane_replicas "${manifest}"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: the checked-in bundled CR names Minimal and pins nothing.
# ---------------------------------------------------------------------------
test_bundled_cr_is_single_node() {
  echo "Test: bundled kind ControlPlane CR names the Minimal profile and pins no backing service"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  assert_eq "bundled CR spec.sizing.profile is Minimal" \
    "Minimal" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.sizing.profile' "$BUNDLED_CR")"
  assert_eq "bundled CR leaves database.replicas to the profile" \
    "null" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.replicas' "$BUNDLED_CR")"
  assert_eq "bundled CR leaves cache.replicas to the profile" \
    "null" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.cache.replicas' "$BUNDLED_CR")"
  assert_eq "bundled CR leaves database.storageSize to the profile" \
    "null" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.storageSize' "$BUNDLED_CR")"
}

# ---------------------------------------------------------------------------
# Test 2: with no knob set the manifest is left byte-identical.
# ---------------------------------------------------------------------------
test_default_footprint() {
  echo "Test: render_controlplane_replicas with no knob set leaves the manifest unchanged"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  local out="$tmp/cr.yaml"
  cp "$BUNDLED_CR" "$out"

  local exit_code
  run_render "$out" >/dev/null
  exit_code=$?

  assert_eq "render exits 0 with no knob set" "0" "$exit_code"
  if cmp -s "$BUNDLED_CR" "$out"; then
    echo "  PASS: the rendered copy is byte-identical to the bundled CR"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the rendered copy differs from the bundled CR"
    diff "$BUNDLED_CR" "$out" | head -20
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 2b: each knob set alone pins its own field and nothing else.
# ---------------------------------------------------------------------------
test_single_knob_pins_only_its_field() {
  echo "Test: each knob set alone pins its own field and nothing else"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (15 checks skipped)"
    SKIP=$((SKIP + 15))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  local out="$tmp/cr.yaml"

  # knob|db|cache|storage|the field it pins|its value|its node tag
  local row knob db cache storage field value tag other exit_code
  for row in \
    "CONTROLPLANE_DB_REPLICAS|3|||database.replicas|3|!!int" \
    "CONTROLPLANE_CACHE_REPLICAS||2||cache.replicas|2|!!int" \
    "CONTROLPLANE_DB_STORAGE|||100Gi|database.storageSize|100Gi|!!str"; do
    IFS='|' read -r knob db cache storage field value tag <<<"$row"
    cp "$BUNDLED_CR" "$out"

    run_render "$out" "$db" "$cache" "$storage" >/dev/null
    exit_code=$?

    assert_eq "render exits 0 for ${knob}=${value} alone" "0" "$exit_code"
    assert_eq "${knob} alone: ${field} is ${value}" \
      "$value" \
      "$(yq -r "select(.kind == \"ControlPlane\") | .spec.infrastructure.${field}" "$out")"
    # `... | tag` reports the node type; the CRD schema types the replica
    # counts as integers and storageSize as a string.
    assert_eq "${knob} alone: ${field} is a ${tag} node" \
      "$tag" \
      "$(yq -r "select(.kind == \"ControlPlane\") | .spec.infrastructure.${field} | tag" "$out")"
    for other in database.replicas cache.replicas database.storageSize; do
      [ "$other" = "$field" ] && continue
      assert_eq "${knob} alone: ${other} stays with the profile" \
        "null" \
        "$(yq -r "select(.kind == \"ControlPlane\") | .spec.infrastructure.${other}" "$out")"
    done
  done
}

# ---------------------------------------------------------------------------
# Test 3: an HA override (DB=3 Galera, cache=2) is projected as integers.
# ---------------------------------------------------------------------------
test_ha_override() {
  echo "Test: render_controlplane_replicas with 3/2/100Gi projects a Galera-sized footprint"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  local out="$tmp/cr.yaml"
  cp "$BUNDLED_CR" "$out"

  local exit_code
  run_render "$out" "3" "2" "100Gi" >/dev/null
  exit_code=$?

  assert_eq "render exits 0 for 3/2/100Gi" "0" "$exit_code"
  assert_eq "database.replicas is 3 (Galera)" \
    "3" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.replicas' "$out")"
  assert_eq "cache.replicas is 2" \
    "2" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.cache.replicas' "$out")"
  assert_eq "database.storageSize is 100Gi (production-sized override)" \
    "100Gi" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.storageSize' "$out")"
  # storageSize must stay a string node — an unquoted 100Gi is fine, but the CRD
  # types it as string, so guard the node kind explicitly.
  assert_eq "database.storageSize stays a string node" \
    "!!str" \
    "$(yq -r 'select(.kind == "ControlPlane") | .spec.infrastructure.database.storageSize | tag' "$out")"
}

# ---------------------------------------------------------------------------
# Test 4: invalid footprints fail fast (before kubectl apply).
# ---------------------------------------------------------------------------
test_invalid_footprint_rejected() {
  echo "Test: render_controlplane_replicas rejects non-numeric, < 1, DB=2, and malformed storage"

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

  # Malformed storage quantities: an SI unit (MB, not the CRD's IEC Mi) and a
  # bare number with no unit both fail the ^[0-9]+(Mi|Gi|Ti)$ pattern.
  output="$(run_render "$out" "1" "1" "512MB")"
  exit_code=$?
  assert_nonzero_exit "SI-unit CONTROLPLANE_DB_STORAGE=512MB exits non-zero" "$exit_code"
  assert_contains "malformed storage error surfaces the offending value" \
    "$output" "CONTROLPLANE_DB_STORAGE='512MB'"

  output="$(run_render "$out" "1" "1" "512")"
  exit_code=$?
  assert_nonzero_exit "unit-less CONTROLPLANE_DB_STORAGE=512 exits non-zero" "$exit_code"

  # The storage size is validated when it is the only knob set, too.
  output="$(run_render "$out" "" "" "512MB")"
  exit_code=$?
  assert_nonzero_exit "CONTROLPLANE_DB_STORAGE=512MB alone exits non-zero" "$exit_code"
  assert_contains "the storage-only error surfaces the offending value" \
    "$output" "CONTROLPLANE_DB_STORAGE='512MB'"
}

# ---------------------------------------------------------------------------
# Test 5: main() wires the knobs — the function is called and the env vars are
# declared with an empty default, so an unset knob leaves the field to the profile.
# Static text checks keep this independent of stub plumbing.
# ---------------------------------------------------------------------------
test_main_wires_knobs() {
  echo "Test: main() calls render_controlplane_replicas and declares the knobs"

  assert_file_contains "render_controlplane_replicas is called from main()" \
    "$DEPLOY_INFRA_SH" "render_controlplane_replicas "
  assert_file_contains "CONTROLPLANE_DB_REPLICAS defaults to unset" \
    "$DEPLOY_INFRA_SH" 'CONTROLPLANE_DB_REPLICAS="${CONTROLPLANE_DB_REPLICAS:-}"'
  assert_file_contains "CONTROLPLANE_CACHE_REPLICAS defaults to unset" \
    "$DEPLOY_INFRA_SH" 'CONTROLPLANE_CACHE_REPLICAS="${CONTROLPLANE_CACHE_REPLICAS:-}"'
  assert_file_contains "CONTROLPLANE_DB_STORAGE defaults to unset" \
    "$DEPLOY_INFRA_SH" 'CONTROLPLANE_DB_STORAGE="${CONTROLPLANE_DB_STORAGE:-}"'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_bundled_cr_is_single_node
test_default_footprint
test_single_knob_pins_only_its_field
test_ha_override
test_invalid_footprint_rejected
test_main_wires_knobs

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
