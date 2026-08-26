#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-generate-cleanup-matrix.sh derives the GHCR package lists from
# the source tree:
#   - images/<name>/ becomes <name> and operators/<name>/ with a go.mod becomes
#     <name>-operator, in both the full and the e2e list;
#   - an operators/<name>/ without a go.mod is skipped, so neither a
#     catalogs-only directory committed ahead of its operator nor
#     operators/shared/ produces a package name the nightly GHCR prune would
#     then fail to find;
#   - a tree without images/ and operators/ exits 1 with an ::error::
#     annotation instead of an empty matrix.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_resolve_ovn_version_test.sh.
#
# Usage: bash tests/unit/hack/ci_generate_cleanup_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MATRIX_SH="$PROJECT_ROOT/hack/ci-generate-cleanup-matrix.sh"

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

# run_matrix <dir>
# Runs the generator with <dir> as its working directory, with GITHUB_OUTPUT
# unset so it only prints to stdout. Stores the combined stdout/stderr in
# OUTPUT and the exit status in RC.
run_matrix() {
  RC=0
  OUTPUT="$(cd "$1" && env -u GITHUB_OUTPUT bash "$MATRIX_SH" 2>&1)" || RC=$?
}

# ---------------------------------------------------------------------------
# Test 1: images/ and go.mod operators become packages, the rest do not
# ---------------------------------------------------------------------------
test_derives_packages_from_images_and_go_mod_operators() {
  echo "Test: images/ and operators with a go.mod become the package lists"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local tree="$TMP_DIR/tree"
  mkdir -p "$tree/images/alpha" \
    "$tree/operators/beta" \
    "$tree/operators/shared/helm" \
    "$tree/operators/gamma/api/v1alpha1/catalogs"
  : >"$tree/operators/beta/go.mod"
  : >"$tree/operators/gamma/api/v1alpha1/catalogs/2025.2.json"

  run_matrix "$tree"

  assert_eq "generator exits 0 on a tree with one image and one operator" "0" "$RC"
  assert_contains "full list holds the image and the go.mod operator" \
    "$OUTPUT" 'cleanup-packages=["alpha","beta-operator"]'
  assert_contains "e2e list holds the image and the go.mod operator" \
    "$OUTPUT" 'cleanup-e2e-packages=["alpha","beta-operator"]'
  assert_not_contains "a catalogs-only operator dir yields no package" \
    "$OUTPUT" "gamma-operator"
  assert_not_contains "operators/shared/ yields no package" \
    "$OUTPUT" "shared-operator"
}

# ---------------------------------------------------------------------------
# Test 2: a tree without images/ and operators/ fails loudly
# ---------------------------------------------------------------------------
test_empty_tree_fails() {
  echo "Test: a tree without any package directory exits 1 with ::error::"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local tree="$TMP_DIR/empty"
  mkdir -p "$tree"

  run_matrix "$tree"

  assert_nonzero_exit "generator exits non-zero on an empty tree" "$RC"
  assert_eq "generator exits 1 on an empty tree" "1" "$RC"
  assert_contains "generator emits an ::error:: for the empty matrix" \
    "$OUTPUT" "::error::No container packages found under images/ or operators/"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_derives_packages_from_images_and_go_mod_operators
test_empty_tree_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
