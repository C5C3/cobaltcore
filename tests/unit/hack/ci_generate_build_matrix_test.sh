#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-generate-build-matrix.sh derives the build matrices from
# releases/*/source-refs.yaml and honours the optional SERVICES filter:
#   - SERVICES unset or "all" keeps every {service, release} pair, which is what
#     the push path publishes;
#   - a list keeps that service's pairs across every release, because the
#     matrices are per service and per release, not per release alone;
#   - the empty string keeps none, leaving {"include":[]} for the jobs that
#     gate on has-services, while the Tempest matrices stay full;
#   - an unknown name exits 1 before a single matrix line is written, so a typo
#     in the resolver's service list cannot show up as a silently missing leg;
#   - a tree without releases/ keeps its ::error:: and its four empty matrices.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_generate_cleanup_matrix_test.sh.
#
# Usage: bash tests/unit/hack/ci_generate_build_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MATRIX_SH="$PROJECT_ROOT/hack/ci-generate-build-matrix.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# have_tools — true when yq and jq are both installed.
have_tools() {
  command -v yq >/dev/null 2>&1 && command -v jq >/dev/null 2>&1
}

# run_matrix <dir> [ENV=value ...]
# Runs the generator with <dir> as its working directory and GITHUB_OUTPUT
# unset, so it only prints to stdout. Stores the combined stdout/stderr in
# OUTPUT and the exit status in RC.
run_matrix() {
  local dir="$1"
  shift
  RC=0
  OUTPUT="$(cd "$dir" && env -u GITHUB_OUTPUT -u SERVICES \
    GITHUB_EVENT_NAME=pull_request "$@" bash "$MATRIX_SH" 2>&1)" || RC=$?
}

# assert_output <description> <key> <expected-value>
# Compares one whole output line, so `matrix=` never matches `build-matrix=`.
assert_output() {
  local description="$1" key="$2" expected="$3" actual
  actual="$(grep -E "^${key}=" <<<"$OUTPUT")"
  assert_eq "$description" "${key}=${expected}" "$actual"
}

# A tree with two releases: glance and keystone in both, neutron in one only.
# The asymmetry catches a filter that drops a pair by release instead of by
# service.
make_tree() {
  local tree="$TMP_DIR/tree"
  mkdir -p "$tree/releases/2025.2" "$tree/releases/2026.1"
  printf 'keystone:\n  ref: a\nglance:\n  ref: b\n' \
    >"$tree/releases/2025.2/source-refs.yaml"
  printf 'keystone:\n  ref: a\nglance:\n  ref: b\nneutron:\n  ref: c\n' \
    >"$tree/releases/2026.1/source-refs.yaml"
  echo "$tree"
}

FULL_MATRIX='{"include":[{"service":"keystone","release":"2025.2"},{"service":"glance","release":"2025.2"},{"service":"keystone","release":"2026.1"},{"service":"glance","release":"2026.1"},{"service":"neutron","release":"2026.1"}]}'
GLANCE_MATRIX='{"include":[{"service":"glance","release":"2025.2"},{"service":"glance","release":"2026.1"}]}'
GLANCE_BUILD_MATRIX='{"include":[{"service":"glance","release":"2025.2","platform":"linux/amd64","runner":"ubuntu-latest"},{"service":"glance","release":"2026.1","platform":"linux/amd64","runner":"ubuntu-latest"}]}'
FULL_TEMPEST_RELEASES='{"include":[{"release":"2025.2"},{"release":"2026.1"}]}'
FULL_TEMPEST_MATRIX='{"include":[{"release":"2025.2","platform":"linux/amd64","runner":"ubuntu-latest"},{"release":"2026.1","platform":"linux/amd64","runner":"ubuntu-latest"}]}'

# ---------------------------------------------------------------------------
# Test 1: SERVICES unset and SERVICES=all both keep every pair
# ---------------------------------------------------------------------------
test_unset_and_all_keep_every_pair() {
  echo "Test: SERVICES unset and SERVICES=all keep every {service, release} pair"

  if ! have_tools; then
    echo "  SKIP: yq or jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local tree
  tree="$(make_tree)"

  run_matrix "$tree"
  assert_eq "generator exits 0 with SERVICES unset" "0" "$RC"
  assert_output "SERVICES unset keeps all five pairs" matrix "$FULL_MATRIX"
  assert_output "SERVICES unset keeps the full Tempest matrix" \
    tempest-matrix "$FULL_TEMPEST_MATRIX"

  local unset_output="$OUTPUT"

  run_matrix "$tree" SERVICES=all
  assert_eq "generator exits 0 with SERVICES=all" "0" "$RC"
  assert_eq "SERVICES=all prints what SERVICES unset prints" \
    "$unset_output" "$OUTPUT"
  assert_output "SERVICES=all keeps all five pairs" matrix "$FULL_MATRIX"
}

# ---------------------------------------------------------------------------
# Test 2: a service list filters both service matrices, and nothing else
# ---------------------------------------------------------------------------
test_service_list_filters_service_matrices() {
  echo "Test: SERVICES=glance keeps the Glance pairs across every release"

  if ! have_tools; then
    echo "  SKIP: yq or jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local tree
  tree="$(make_tree)"

  run_matrix "$tree" SERVICES=glance

  assert_eq "generator exits 0" "0" "$RC"
  assert_output "matrix holds both Glance pairs" matrix "$GLANCE_MATRIX"
  assert_output "build-matrix holds both Glance pairs" \
    build-matrix "$GLANCE_BUILD_MATRIX"
  assert_output "the Tempest matrix is not filtered" \
    tempest-matrix "$FULL_TEMPEST_MATRIX"
  assert_output "the Tempest release matrix is not filtered" \
    tempest-release-matrix "$FULL_TEMPEST_RELEASES"
}

# ---------------------------------------------------------------------------
# Test 3: the empty string empties the service matrices, successfully
# ---------------------------------------------------------------------------
test_empty_services_empties_service_matrices() {
  echo "Test: SERVICES='' yields empty service matrices and exits 0"

  if ! have_tools; then
    echo "  SKIP: yq or jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local tree
  tree="$(make_tree)"

  run_matrix "$tree" SERVICES=

  assert_eq "generator exits 0" "0" "$RC"
  assert_output "matrix is empty" matrix '{"include":[]}'
  assert_output "build-matrix is empty" build-matrix '{"include":[]}'
  assert_output "the Tempest matrix stays full" \
    tempest-matrix "$FULL_TEMPEST_MATRIX"
  assert_output "the Tempest release matrix stays full" \
    tempest-release-matrix "$FULL_TEMPEST_RELEASES"
}

# ---------------------------------------------------------------------------
# Test 4: an unknown service fails before any matrix is written
# ---------------------------------------------------------------------------
test_unknown_service_fails_before_any_output() {
  echo "Test: an unknown name in SERVICES exits 1 with no matrix line"

  if ! have_tools; then
    echo "  SKIP: yq or jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local tree
  tree="$(make_tree)"

  run_matrix "$tree" SERVICES="glance nova"

  assert_eq "generator exits 1" "1" "$RC"
  assert_contains "the annotation names the unknown service" \
    "$OUTPUT" "::error::Unknown service 'nova' in SERVICES"
  assert_not_contains "no matrix line is written" "$OUTPUT" "matrix="
  assert_not_contains "the known service does not leak into the output" \
    "$OUTPUT" "glance\",\"release"
}

# ---------------------------------------------------------------------------
# Test 5: a tree without releases/ keeps its error and its empty matrices
# ---------------------------------------------------------------------------
test_no_releases_directory() {
  echo "Test: a tree without releases/ emits four empty matrices and exits 0"

  if ! have_tools; then
    echo "  SKIP: yq or jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local tree="$TMP_DIR/empty"
  mkdir -p "$tree"

  run_matrix "$tree"

  assert_eq "generator exits 0" "0" "$RC"
  assert_contains "the annotation names the missing directory" \
    "$OUTPUT" "::error::No release directories found under releases/"
  assert_output "matrix is empty" matrix '{"include":[]}'
  assert_output "build-matrix is empty" build-matrix '{"include":[]}'
  assert_output "tempest-matrix is empty" tempest-matrix '{"include":[]}'
  assert_output "tempest-release-matrix is empty" \
    tempest-release-matrix '{"include":[]}'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_unset_and_all_keep_every_pair
test_service_list_filters_service_matrices
test_empty_services_empties_service_matrices
test_unknown_service_fails_before_any_output
test_no_releases_directory

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
