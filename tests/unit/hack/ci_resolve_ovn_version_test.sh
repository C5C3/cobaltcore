#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-ovn-version.sh parses the images/ovn/Dockerfile pin:
#   - a well-formed 'ARG OVN_VERSION=vX.Y.Z' line prints X.Y.Z;
#   - a missing line, an empty file, a non-tag value, a second 'ARG
#     OVN_VERSION=' line and a missing Dockerfile each exit non-zero with an
#     ::error:: annotation;
#   - the checked-in Dockerfile resolves to an X.Y.Z version.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_fetch_released_operator_test.sh.
#
# Usage: bash tests/unit/hack/ci_resolve_ovn_version_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RESOLVE_SH="$PROJECT_ROOT/hack/ci-resolve-ovn-version.sh"

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

# run_resolve <dockerfile>
# Runs the resolver against <dockerfile>, or against its own default when
# <dockerfile> is "-" (OVN_DOCKERFILE left unset). Stores the combined
# stdout/stderr in OUTPUT and the exit status in RC.
run_resolve() {
  RC=0
  if [ "$1" = "-" ]; then
    OUTPUT="$(unset OVN_DOCKERFILE; bash "$RESOLVE_SH" 2>&1)" || RC=$?
  else
    OUTPUT="$(OVN_DOCKERFILE="$1" bash "$RESOLVE_SH" 2>&1)" || RC=$?
  fi
}

# ---------------------------------------------------------------------------
# Test 1: a pinned vX.Y.Z tag prints X.Y.Z
# ---------------------------------------------------------------------------
test_valid_pin() {
  echo "Test: a valid 'ARG OVN_VERSION=vX.Y.Z' line prints the version"

  local fixture="$TMP_DIR/Dockerfile.valid"
  cat >"$fixture" <<'FIXTURE'
FROM ubuntu:noble AS builder
ARG APT_MIRROR=""
# Git tag of ovn-org/ovn.
ARG OVN_VERSION=v26.03.2
RUN echo build
FIXTURE

  run_resolve "$fixture"

  assert_eq "resolver exits 0 on a valid pin" "0" "$RC"
  assert_eq "resolver strips the leading v" "26.03.2" "$OUTPUT"
}

# ---------------------------------------------------------------------------
# Test 2: a Dockerfile without the ARG line fails
# ---------------------------------------------------------------------------
test_missing_arg_line() {
  echo "Test: a Dockerfile without the ARG line exits non-zero"

  local fixture="$TMP_DIR/Dockerfile.noarg"
  cat >"$fixture" <<'FIXTURE'
FROM ubuntu:noble AS builder
RUN echo build
FIXTURE

  run_resolve "$fixture"

  assert_nonzero_exit "resolver exits non-zero without the ARG line" "$RC"
  assert_contains "resolver emits an ::error:: for the missing line" "$OUTPUT" "::error::"
}

# ---------------------------------------------------------------------------
# Test 3: a non-tag value fails and names the offending value
# ---------------------------------------------------------------------------
test_non_tag_value() {
  echo "Test: a branch name instead of a vX.Y.Z tag exits non-zero"

  local fixture="$TMP_DIR/Dockerfile.branch"
  printf 'FROM ubuntu:noble\nARG OVN_VERSION=main\n' >"$fixture"

  run_resolve "$fixture"

  assert_nonzero_exit "resolver exits non-zero on a non-tag value" "$RC"
  assert_contains "resolver emits an ::error:: for the non-tag value" "$OUTPUT" "::error::"
  assert_contains "resolver names the offending value" "$OUTPUT" "main"
}

# ---------------------------------------------------------------------------
# Test 4: two 'ARG OVN_VERSION=' lines fail rather than resolving the first
# ---------------------------------------------------------------------------
test_duplicate_arg_lines() {
  echo "Test: a second 'ARG OVN_VERSION=' line exits non-zero"

  # images/ovn/Dockerfile is a two-stage build, and redeclaring a build arg in
  # the later stage is the standard Docker idiom (APT_MIRROR is declared twice
  # there already). sed then emits two lines, and the anchored vX.Y.Z match in
  # the resolver is what rejects the multi-line value. Without this test a
  # `| head -1` appended to that pipeline would pass CI and start returning the
  # first of two conflicting pins.
  local fixture="$TMP_DIR/Dockerfile.dup"
  printf 'FROM ubuntu:noble AS build\nARG OVN_VERSION=v26.03.2\nFROM ubuntu:noble\nARG OVN_VERSION=v26.03.3\n' >"$fixture"

  run_resolve "$fixture"

  assert_nonzero_exit "resolver exits non-zero on two pins" "$RC"
  assert_contains "resolver emits an ::error:: for the ambiguous pin" "$OUTPUT" "::error::"
}

# ---------------------------------------------------------------------------
# Test 5: a missing Dockerfile fails and names the path
# ---------------------------------------------------------------------------
test_missing_dockerfile() {
  echo "Test: a missing OVN_DOCKERFILE exits non-zero and names the path"

  run_resolve "/nonexistent"

  assert_nonzero_exit "resolver exits non-zero on a missing Dockerfile" "$RC"
  assert_contains "resolver emits an ::error:: for the missing file" "$OUTPUT" "::error::"
  assert_contains "resolver names the missing path" "$OUTPUT" "/nonexistent"
}

# ---------------------------------------------------------------------------
# Test 6: an empty Dockerfile fails
# ---------------------------------------------------------------------------
test_empty_dockerfile() {
  echo "Test: an empty Dockerfile exits non-zero"

  local fixture="$TMP_DIR/Dockerfile.empty"
  : >"$fixture"

  run_resolve "$fixture"

  assert_nonzero_exit "resolver exits non-zero on an empty Dockerfile" "$RC"
  assert_contains "resolver emits an ::error:: for the empty file" "$OUTPUT" "::error::"
}

# ---------------------------------------------------------------------------
# Test 7: the checked-in Dockerfile resolves to an X.Y.Z version
# ---------------------------------------------------------------------------
test_checked_in_dockerfile() {
  echo "Test: the checked-in images/ovn/Dockerfile resolves to X.Y.Z"

  run_resolve "-"

  local matches="no"
  if [[ "$OUTPUT" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    matches="yes"
  fi

  assert_eq "resolver exits 0 on the checked-in Dockerfile" "0" "$RC"
  assert_eq "resolved pin is an X.Y.Z version (got '$OUTPUT')" "yes" "$matches"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_valid_pin
test_missing_arg_line
test_non_tag_value
test_duplicate_arg_lines
test_missing_dockerfile
test_empty_dockerfile
test_checked_in_dockerfile

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
