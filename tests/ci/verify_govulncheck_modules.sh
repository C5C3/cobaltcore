#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# CC-0061: Verify govulncheck Makefile target covers all go.work modules
# Parses go.work use directives and compares against the modules scanned by
# the govulncheck Makefile target, failing if they diverge.
# Usage: bash tests/ci/verify_govulncheck_modules.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

GO_WORK="$PROJECT_ROOT/go.work"
MAKEFILE="$PROJECT_ROOT/Makefile"

# --- Extract go.work use directives ---
# Parses lines between "use (" and ")" removing leading "./" and whitespace.
gowork_modules=()
in_use_block=false
while IFS= read -r line; do
  if [[ "$line" =~ ^use[[:space:]]*\( ]]; then
    in_use_block=true
    continue
  fi
  if $in_use_block; then
    if [[ "$line" =~ ^\) ]]; then
      break
    fi
    # Trim whitespace and leading ./
    module=$(echo "$line" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//;s|^\./||')
    if [[ -n "$module" ]]; then
      gowork_modules+=("$module")
    fi
  fi
done < "$GO_WORK"

# --- Extract OPERATORS from Makefile ---
# Parses the default OPERATORS ?= line.
makefile_operators=()
operators_line=$(grep -E '^OPERATORS \?=' "$MAKEFILE" | head -1)
# Strip "OPERATORS ?= " prefix to get the operator names
operators_value="${operators_line#*= }"
read -ra makefile_operators <<< "$operators_value"

# Build the set of modules the Makefile govulncheck target covers:
# internal/common (hardcoded) + operators/<op> for each OPERATORS entry.
makefile_modules=("internal/common")
for op in "${makefile_operators[@]}"; do
  makefile_modules+=("operators/$op")
done

# Sort both arrays for comparison
IFS=$'\n' gowork_sorted=($(sort <<< "${gowork_modules[*]}")); unset IFS
IFS=$'\n' makefile_sorted=($(sort <<< "${makefile_modules[*]}")); unset IFS

echo "=== govulncheck module coverage verification (CC-0061) ==="
echo ""
echo "go.work modules:  ${gowork_sorted[*]}"
echo "Makefile modules: ${makefile_sorted[*]}"
echo ""

# --- Test 1: go.work and Makefile have the same module count ---
test_module_count() {
  echo "Test: go.work and Makefile govulncheck target cover the same number of modules"
  assert_eq "module count matches" "${#gowork_sorted[@]}" "${#makefile_sorted[@]}"
}

# --- Test 2: Every go.work module is covered by the Makefile target ---
test_gowork_covered() {
  echo "Test: every go.work module is covered by the govulncheck Makefile target"

  for module in "${gowork_sorted[@]}"; do
    local found=false
    for mk_module in "${makefile_sorted[@]}"; do
      if [[ "$module" == "$mk_module" ]]; then
        found=true
        break
      fi
    done
    if $found; then
      echo "  PASS: $module is covered"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $module is in go.work but NOT scanned by make govulncheck"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 3: Every Makefile module exists in go.work ---
test_makefile_covered() {
  echo "Test: every module in the govulncheck Makefile target exists in go.work"

  for mk_module in "${makefile_sorted[@]}"; do
    local found=false
    for module in "${gowork_sorted[@]}"; do
      if [[ "$mk_module" == "$module" ]]; then
        found=true
        break
      fi
    done
    if $found; then
      echo "  PASS: $mk_module is in go.work"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $mk_module is scanned by make govulncheck but NOT in go.work"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 4: Makefile has a govulncheck .PHONY target ---
test_makefile_target_exists() {
  echo "Test: Makefile declares a govulncheck target"
  assert_file_contains "Makefile has .PHONY: govulncheck" "$MAKEFILE" ".PHONY: govulncheck"
}

# --- Run all tests ---
test_module_count
echo ""
test_gowork_covered
echo ""
test_makefile_covered
echo ""
test_makefile_target_exists
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
