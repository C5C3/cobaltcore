#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the govulncheck target scans every Go module of the workspace: the
# use directives of go.work, the OPERATORS default of the Makefile and the
# operators/*/go.mod files name the same modules.
#
# make govulncheck passes hack/ci-govulncheck.sh internal/common plus one
# operators/<op> per word of OPERATORS, and the script scans exactly the
# directories it is given. A module added to go.work and operators/ without its
# OPERATORS entry is built and tested and never scanned, and the govulncheck job
# stays green on the modules it does reach.
#
# Usage: bash tests/unit/hack/govulncheck_modules_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
GO_WORK="$PROJECT_ROOT/go.work"
MAKEFILE="$PROJECT_ROOT/Makefile"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the modules of the go.work use ( ... ) block, one per line, ./ stripped
# and sorted; nothing when the file has no such block.
gowork_modules() {
  awk '
    /^use[[:space:]]*\($/ { in_use = 1; next }
    in_use && /^\)/ { exit }
    in_use {
      sub(/^[[:space:]]+/, ""); sub(/[[:space:]]+$/, ""); sub(/^\.\//, "")
      if ($0 != "") print
    }
  ' "$GO_WORK" | LC_ALL=C sort
}

# Echo the modules make govulncheck scans, one per line and sorted:
# internal/common plus operators/<op> per word of the OPERATORS default.
# Nothing when the Makefile carries no OPERATORS default.
makefile_modules() {
  local line op
  line=$(grep -m1 '^OPERATORS ?= ' "$MAKEFILE")
  [ -n "$line" ] || return 0
  {
    echo "internal/common"
    for op in ${line#OPERATORS ?= }; do
      echo "operators/$op"
    done
  } | LC_ALL=C sort
}

# Echo the module directories in the tree, one per line and sorted:
# internal/common plus the parent of each operators/*/go.mod.
tree_modules() {
  local f
  {
    echo "internal/common"
    for f in "$PROJECT_ROOT"/operators/*/go.mod; do
      [ -f "$f" ] && echo "operators/$(basename "$(dirname "$f")")"
    done
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_three_module_lists_agree() {
  echo "Test: go.work, the Makefile OPERATORS default and operators/ agree"

  local gowork mk
  gowork=$(gowork_modules)
  if [ -z "$gowork" ]; then
    echo "  FAIL: go.work declares no modules"
    FAIL=$((FAIL + 1))
    return
  fi
  mk=$(makefile_modules)
  if [ -z "$mk" ]; then
    echo "  FAIL: Makefile declares no OPERATORS default"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "the Makefile scans every go.work module and nothing else" \
    "$gowork" "$mk"
  assert_eq "go.work lists every operators/*/go.mod" "$gowork" "$(tree_modules)"
}

test_the_target_scans_the_makefile_list() {
  echo "Test: make govulncheck scans the OPERATORS list and CI runs it"

  assert_file_contains_fixed "the Makefile declares the target" "$MAKEFILE" \
    ".PHONY: govulncheck"
  assert_file_contains_fixed "the recipe scans internal/common and every OPERATORS entry" \
    "$MAKEFILE" \
    '@hack/ci-govulncheck.sh internal/common $(addprefix operators/,$(OPERATORS))'
  assert_file_contains_fixed "the govulncheck job runs the target" "$CI_YAML" \
    "run: make govulncheck"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_the_three_module_lists_agree
test_the_target_scans_the_makefile_list

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
