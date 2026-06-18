#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify build-images workflow refactoring meets requirements through
# Validates: line count, deduplication, composite action usage, script references
# Usage: bash tests/ci/verify_build_images_refactor.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

WORKFLOW="$PROJECT_ROOT/.github/workflows/build-images.yaml"

if [[ ! -f "$WORKFLOW" ]]; then
  echo "ERROR: Workflow file not found: $WORKFLOW"
  exit 1
fi

echo "=== Build-images workflow refactor verification ==="
echo "Workflow: $WORKFLOW"
echo ""

# --- Test 1: Workflow line count is under 600 lines ---
test_workflow_line_count() {
  echo "Test: workflow file is under 600 lines"

  local line_count
  line_count=$(wc -l < "$WORKFLOW")
  if [[ "$line_count" -lt 600 ]]; then
    echo "  PASS: workflow is $line_count lines (< 600)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: workflow is $line_count lines (expected < 600)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: No directive 'MUST stay in sync' comments ---
# A quoted reference to the phrase (e.g. 'Eliminates the "MUST stay in sync"')
# is acceptable; only unquoted directives are flagged.
test_no_must_stay_in_sync_comments() {
  echo "Test: no directive 'MUST stay in sync' comments in workflow"

  local count
  # Exclude lines that reference the phrase inside quotes (meta-comments about removal)
  count=$(grep -i 'MUST stay in sync' "$WORKFLOW" | grep -cv '"MUST stay in sync"' || true)
  assert_eq "no unquoted 'MUST stay in sync' directives" "0" "$count"
}

# --- Test 3: GITHUB_REPOSITORY_OWNER appears at most 1 time ---
test_normalize_owner_count() {
  echo "Test: GITHUB_REPOSITORY_OWNER consolidated (at most 1 occurrence)"

  local count
  count=$(grep -c 'GITHUB_REPOSITORY_OWNER' "$WORKFLOW" || true)
  if [[ "$count" -le 1 ]]; then
    echo "  PASS: GITHUB_REPOSITORY_OWNER appears $count time(s) (<= 1)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: GITHUB_REPOSITORY_OWNER appears $count times (expected <= 1)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 4: supply-chain-attest composite action usage ---
# supply-chain-attest is now used via build-push-image and
# merge-manifest-and-attest composites, so search all action files.
test_supply_chain_attest_usage() {
  echo "Test: supply-chain-attest composite action used >= 2 times (across actions)"

  local count
  count=$(grep -r '\./.github/actions/supply-chain-attest' "$PROJECT_ROOT/.github/" | wc -l)
  if [[ "$count" -ge 2 ]]; then
    echo "  PASS: supply-chain-attest used $count times (>= 2)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: supply-chain-attest used $count times (expected >= 2)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 5: setup-docker-registry composite action usage ---
test_setup_docker_registry_usage() {
  echo "Test: setup-docker-registry composite action used >= 7 times"

  local count
  count=$(grep -c '\./.github/actions/setup-docker-registry' "$WORKFLOW" || true)
  if [[ "$count" -ge 7 ]]; then
    echo "  PASS: setup-docker-registry used $count times (>= 7)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: setup-docker-registry used $count times (expected >= 7)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 6: checkout-service-source composite action usage ---
test_checkout_service_source_usage() {
  echo "Test: checkout-service-source composite action used >= 2 times"

  local count
  count=$(grep -c '\./.github/actions/checkout-service-source' "$WORKFLOW" || true)
  if [[ "$count" -ge 2 ]]; then
    echo "  PASS: checkout-service-source used $count times (>= 2)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: checkout-service-source used $count times (expected >= 2)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 7: export-digest composite action usage ---
# export-digest is now used via build-push-image composite.
test_export_digest_usage() {
  echo "Test: export-digest composite action used >= 1 time (across actions)"

  local count
  count=$(grep -r '\./.github/actions/export-digest' "$PROJECT_ROOT/.github/" | wc -l)
  if [[ "$count" -ge 1 ]]; then
    echo "  PASS: export-digest used $count times (>= 1)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: export-digest used $count times (expected >= 1)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 8: ci-merge-manifest.sh script usage ---
# ci-merge-manifest.sh is now used via merge-manifest-and-attest composite.
test_ci_merge_manifest_usage() {
  echo "Test: ci-merge-manifest.sh referenced >= 1 time (across actions)"

  local count
  count=$(grep -r 'ci-merge-manifest\.sh' "$PROJECT_ROOT/.github/" | wc -l)
  if [[ "$count" -ge 1 ]]; then
    echo "  PASS: ci-merge-manifest.sh used $count times (>= 1)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: ci-merge-manifest.sh used $count times (expected >= 1)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 9: ci-run-unit-tests.sh script usage ---
test_ci_run_unit_tests_usage() {
  echo "Test: ci-run-unit-tests.sh referenced >= 1 time"

  local count
  count=$(grep -c 'ci-run-unit-tests\.sh' "$WORKFLOW" || true)
  if [[ "$count" -ge 1 ]]; then
    echo "  PASS: ci-run-unit-tests.sh used $count times (>= 1)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: ci-run-unit-tests.sh used $count times (expected >= 1)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 10: No inline scripts over 5 lines ---
# Detects multi-line run: | blocks that exceed 5 continuation lines.
# Some inline blocks (matrix generation, tag derivation, version resolution,
# extra-packages parsing) are inherently workflow-specific and acceptable.
# This test ensures the count does not grow beyond the current baseline.
test_no_inline_scripts_over_5_lines() {
  echo "Test: inline run: | blocks over 5 lines do not exceed baseline count"

  local violation_count
  violation_count=$(awk '
    /^[[:space:]]*run: \|/ {
      in_block = 1
      block_start = NR
      match($0, /^[[:space:]]*/)
      run_indent = RLENGTH
      count = 0
      next
    }
    in_block {
      if (/^[[:space:]]*$/) { count++; next }
      match($0, /^[[:space:]]*/)
      if (RLENGTH > run_indent) { count++ }
      else {
        if (count > 5) { violations++ }
        in_block = 0
        count = 0
      }
    }
    END {
      if (in_block && count > 5) { violations++ }
      print violations + 0
    }
  ' "$WORKFLOW")

  # All inline blocks over 5 lines have been extracted to scripts or compacted.
  local max_allowed=0
  if [[ "$violation_count" -le "$max_allowed" ]]; then
    echo "  PASS: $violation_count inline blocks exceed 5 lines (<= $max_allowed baseline)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $violation_count inline blocks exceed 5 lines (expected <= $max_allowed)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 11: build-push-image composite action usage ---
test_build_push_image_usage() {
  echo "Test: build-push-image composite action used >= 4 times"

  local count
  count=$(grep -c '\./.github/actions/build-push-image' "$WORKFLOW" || true)
  if [[ "$count" -ge 4 ]]; then
    echo "  PASS: build-push-image used $count times (>= 4)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: build-push-image used $count times (expected >= 4)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 12: merge-manifest-and-attest composite action usage ---
test_merge_manifest_and_attest_usage() {
  echo "Test: merge-manifest-and-attest composite action used >= 3 times"

  local count
  count=$(grep -c '\./.github/actions/merge-manifest-and-attest' "$WORKFLOW" || true)
  if [[ "$count" -ge 3 ]]; then
    echo "  PASS: merge-manifest-and-attest used $count times (>= 3)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: merge-manifest-and-attest used $count times (expected >= 3)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 13: Path triggers include composite actions and hack scripts (B-001) ---
test_path_triggers_include_extracted_components() {
  echo "Test: path triggers include .github/actions/** and hack/ci-*"

  local missing=0
  # Both push and pull_request triggers must include the extracted component paths.
  for pattern in '.github/actions/\*\*' 'hack/ci-\*'; do
    local count
    count=$(grep -c "$pattern" "$WORKFLOW" || true)
    if [[ "$count" -lt 2 ]]; then
      echo "  FAIL: $pattern appears $count time(s) in path triggers (expected >= 2: push + pull_request)"
      missing=$((missing + 1))
    fi
  done

  if [[ "$missing" -eq 0 ]]; then
    echo "  PASS: .github/actions/** and hack/ci-* present in both push and pull_request triggers"
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
  fi
}

# --- Run all tests ---
test_workflow_line_count
echo ""
test_no_must_stay_in_sync_comments
echo ""
test_normalize_owner_count
echo ""
test_supply_chain_attest_usage
echo ""
test_setup_docker_registry_usage
echo ""
test_checkout_service_source_usage
echo ""
test_export_digest_usage
echo ""
test_ci_merge_manifest_usage
echo ""
test_ci_run_unit_tests_usage
echo ""
test_no_inline_scripts_over_5_lines
echo ""
test_build_push_image_usage
echo ""
test_merge_manifest_and_attest_usage
echo ""
test_path_triggers_include_extracted_components
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
