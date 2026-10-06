#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-hvo-commit.sh parses the
# images/openstack-hypervisor-operator/Dockerfile pin:
#   - a well-formed 'ARG HVO_COMMIT=<40 hex>' line prints the commit, and bare
#     'ARG HVO_COMMIT' re-declarations beside it are ignored;
#   - a missing line, an empty file, a non-commit value (a tag, a short SHA),
#     a second 'ARG HVO_COMMIT=' line and a missing Dockerfile each exit
#     non-zero with an ::error:: annotation and print nothing on stdout;
#   - the checked-in Dockerfile resolves to a 40-character commit.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_resolve_ovn_version_test.sh.
#
# Usage: bash tests/unit/hack/ci_resolve_hvo_commit_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RESOLVE_SH="$PROJECT_ROOT/hack/ci-resolve-hvo-commit.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMIT="a2baf3f4002e39ff93f105fae9594b357ba55b21"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# run_resolve <dockerfile>
# Runs the resolver against <dockerfile>, or against its own default when
# <dockerfile> is "-" (HVO_DOCKERFILE left unset). Stores the combined
# stdout/stderr in OUTPUT, stdout alone in STDOUT and the exit status in RC.
run_resolve() {
  RC=0
  if [ "$1" = "-" ]; then
    OUTPUT="$(unset HVO_DOCKERFILE; bash "$RESOLVE_SH" 2>&1)" || RC=$?
    STDOUT="$(unset HVO_DOCKERFILE; bash "$RESOLVE_SH" 2>/dev/null)" || true
  else
    OUTPUT="$(HVO_DOCKERFILE="$1" bash "$RESOLVE_SH" 2>&1)" || RC=$?
    STDOUT="$(HVO_DOCKERFILE="$1" bash "$RESOLVE_SH" 2>/dev/null)" || true
  fi
}

# ---------------------------------------------------------------------------
# Test 1: a pinned commit prints the commit, bare re-declarations are ignored
# ---------------------------------------------------------------------------
test_valid_pin() {
  echo "Test: a valid 'ARG HVO_COMMIT=<commit>' line prints the commit"

  local fixture="$TMP_DIR/Dockerfile.valid"
  cat >"$fixture" <<FIXTURE
# A commit of upstream main.
ARG HVO_COMMIT=${COMMIT}
FROM golang:1.27 AS build
ARG HVO_COMMIT
RUN echo build
FROM gcr.io/distroless/static:nonroot
ARG HVO_COMMIT
FIXTURE

  run_resolve "$fixture"

  assert_eq "resolver exits 0 on a valid pin" "0" "$RC"
  assert_eq "resolver prints the commit and nothing else" "$COMMIT" "$OUTPUT"
}

# ---------------------------------------------------------------------------
# Test 2: a Dockerfile without the valued ARG line fails
# ---------------------------------------------------------------------------
test_missing_arg_line() {
  echo "Test: a Dockerfile with only a bare 'ARG HVO_COMMIT' exits non-zero"

  local fixture="$TMP_DIR/Dockerfile.noarg"
  cat >"$fixture" <<'FIXTURE'
FROM golang:1.27 AS build
ARG HVO_COMMIT
RUN echo build
FIXTURE

  run_resolve "$fixture"

  assert_nonzero_exit "resolver exits non-zero without the ARG line" "$RC"
  assert_contains "resolver emits an ::error:: for the missing line" "$OUTPUT" "::error::no 'ARG HVO_COMMIT=' line in $fixture"
  assert_eq "resolver prints nothing on stdout" "" "$STDOUT"
}

# ---------------------------------------------------------------------------
# Test 3: a non-commit value fails and names the offending value
# ---------------------------------------------------------------------------
test_non_commit_value() {
  echo "Test: a tag or a short SHA instead of a 40-character commit exits non-zero"

  local fixture value
  for value in v1.2.3 a2baf3f; do
    fixture="$TMP_DIR/Dockerfile.$value"
    printf 'ARG HVO_COMMIT=%s\nFROM golang:1.27\n' "$value" >"$fixture"

    run_resolve "$fixture"

    assert_nonzero_exit "resolver exits non-zero on '$value'" "$RC"
    assert_contains "resolver names '$value' as not a commit" "$OUTPUT" \
      "::error::ARG HVO_COMMIT in $fixture is not a 40-character commit: '$value'"
    assert_eq "resolver prints nothing on stdout for '$value'" "" "$STDOUT"
  done
}

# ---------------------------------------------------------------------------
# Test 4: two valued 'ARG HVO_COMMIT=' lines fail rather than resolving one
# ---------------------------------------------------------------------------
test_duplicate_arg_lines() {
  echo "Test: a second 'ARG HVO_COMMIT=' line exits non-zero"

  # sed emits two lines for two valued pins, and the anchored 40-hex match in
  # the resolver is what rejects the multi-line value. Without this test a
  # `| head -1` appended to that pipeline would pass CI and start returning
  # the first of two conflicting pins.
  local fixture="$TMP_DIR/Dockerfile.dup"
  printf 'ARG HVO_COMMIT=%s\nFROM golang:1.27 AS build\nARG HVO_COMMIT=%s\n' \
    "$COMMIT" "0123456789abcdef0123456789abcdef01234567" >"$fixture"

  run_resolve "$fixture"

  assert_nonzero_exit "resolver exits non-zero on two pins" "$RC"
  assert_contains "resolver emits an ::error:: for the ambiguous pin" "$OUTPUT" \
    "::error::ARG HVO_COMMIT in $fixture is not a 40-character commit:"
  assert_eq "resolver prints nothing on stdout on two pins" "" "$STDOUT"
}

# ---------------------------------------------------------------------------
# Test 5: a missing Dockerfile fails and names the path
# ---------------------------------------------------------------------------
test_missing_dockerfile() {
  echo "Test: a missing HVO_DOCKERFILE exits non-zero and names the path"

  run_resolve "/nonexistent"

  assert_nonzero_exit "resolver exits non-zero on a missing Dockerfile" "$RC"
  assert_eq "resolver emits the not-found ::error::" \
    "::error::hvo Dockerfile not found: /nonexistent" "$OUTPUT"
  assert_eq "resolver prints nothing on stdout" "" "$STDOUT"
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
  assert_contains "resolver emits an ::error:: for the empty file" "$OUTPUT" "::error::no 'ARG HVO_COMMIT=' line in $fixture"
}

# ---------------------------------------------------------------------------
# Test 7: the checked-in Dockerfile resolves to a 40-character commit
# ---------------------------------------------------------------------------
test_checked_in_dockerfile() {
  echo "Test: the checked-in images/openstack-hypervisor-operator/Dockerfile resolves to a commit"

  run_resolve "-"

  local matches="no"
  if [[ "$OUTPUT" =~ ^[0-9a-f]{40}$ ]]; then
    matches="yes"
  fi

  assert_eq "resolver exits 0 on the checked-in Dockerfile" "0" "$RC"
  assert_eq "resolved pin is a 40-character commit (got '$OUTPUT')" "yes" "$matches"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_valid_pin
test_missing_arg_line
test_non_commit_value
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
