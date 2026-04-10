#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# CC-0055: Verify composite actions meet quality standards (REQ-010)
# Validates: SPDX headers, feature-ID comments, shell: bash, SHA-pinned actions
# Usage: bash tests/ci/verify_composite_actions.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# Collect all composite action files
shopt -s nullglob
ACTION_FILES=("$PROJECT_ROOT"/.github/actions/*/action.yaml)
shopt -u nullglob

if [[ ${#ACTION_FILES[@]} -eq 0 ]]; then
  echo "ERROR: No .github/actions/*/action.yaml files found"
  exit 1
fi

echo "Found ${#ACTION_FILES[@]} composite action files to verify"
echo ""

# CC-0055 actions that require the feature-ID comment
CC0055_ACTIONS=(
  "setup-docker-registry"
  "export-digest"
  "checkout-service-source"
  "supply-chain-attest"
  "build-push-image"
  "merge-manifest-and-attest"
)

# --- Test 1: All action.yaml files have SPDX license headers ---
test_spdx_headers() {
  echo "Test: all action.yaml files have SPDX license headers"

  for action_file in "${ACTION_FILES[@]}"; do
    local dir_name
    dir_name="$(basename "$(dirname "$action_file")")"

    local has_copyright=false has_license=false
    if head -5 "$action_file" | grep -q "SPDX-FileCopyrightText"; then
      has_copyright=true
    fi
    if head -5 "$action_file" | grep -q "SPDX-License-Identifier"; then
      has_license=true
    fi

    if $has_copyright && $has_license; then
      echo "  PASS: $dir_name/action.yaml has SPDX header"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $dir_name/action.yaml missing SPDX header (copyright=$has_copyright, license=$has_license)"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 2: CC-0055 actions have feature-ID comment ---
test_feature_id_comment() {
  echo "Test: CC-0055 actions have feature-ID comment"

  for action_file in "${ACTION_FILES[@]}"; do
    local dir_name
    dir_name="$(basename "$(dirname "$action_file")")"

    local is_cc0055=false
    for cc_action in "${CC0055_ACTIONS[@]}"; do
      if [[ "$dir_name" == "$cc_action" ]]; then
        is_cc0055=true
        break
      fi
    done

    if $is_cc0055; then
      assert_file_contains "$dir_name/action.yaml has CC-0055 comment" "$action_file" "# CC-0055"
    else
      echo "  SKIP: $dir_name/action.yaml is not a CC-0055 action"
      SKIP=$((SKIP + 1))
    fi
  done
}

# --- Test 3: All run: steps have explicit shell: bash ---
test_shell_explicit() {
  echo "Test: all run: steps in composite actions have shell: bash"

  for action_file in "${ACTION_FILES[@]}"; do
    local dir_name
    dir_name="$(basename "$(dirname "$action_file")")"

    local run_count shell_count
    run_count=$(grep -cE '^\s+run:\s' "$action_file" || true)
    shell_count=$(grep -cE '^\s+shell:\s+bash' "$action_file" || true)

    if [[ "$run_count" -eq 0 ]]; then
      echo "  SKIP: $dir_name/action.yaml has no run: steps"
      SKIP=$((SKIP + 1))
    else
      assert_eq "$dir_name/action.yaml run: count ($run_count) matches shell: bash count" "$run_count" "$shell_count"
    fi
  done
}

# --- Test 4: All external uses: references are SHA-pinned with version comment ---
test_sha_pinned_actions() {
  echo "Test: all external uses: references are SHA-pinned with version comment"

  for action_file in "${ACTION_FILES[@]}"; do
    local dir_name
    dir_name="$(basename "$(dirname "$action_file")")"

    # Extract external uses: lines (skip local ./ references)
    local external_uses
    external_uses=$(grep -E '^\s+uses:\s' "$action_file" | grep -v '\./') || true

    if [[ -z "$external_uses" ]]; then
      echo "  SKIP: $dir_name/action.yaml has no external uses: references"
      SKIP=$((SKIP + 1))
      continue
    fi

    local all_pinned=true
    local line_count=0
    while IFS= read -r line; do
      line_count=$((line_count + 1))
      if ! echo "$line" | grep -qE '@[a-f0-9]{40} # v'; then
        echo "  FAIL: $dir_name/action.yaml has unpinned uses: reference: $line"
        FAIL=$((FAIL + 1))
        all_pinned=false
      fi
    done <<< "$external_uses"

    if $all_pinned; then
      echo "  PASS: $dir_name/action.yaml has $line_count SHA-pinned external action(s)"
      PASS=$((PASS + 1))
    fi
  done
}

# --- Test 5: supply-chain-attest validates required inputs in sbom mode (CC-0055) ---
test_supply_chain_attest_input_validation() {
  echo "Test: supply-chain-attest validates image-digest and sbom-output-file in sbom mode"

  local action_file="$PROJECT_ROOT/.github/actions/supply-chain-attest/action.yaml"
  assert_file_contains "validates image-digest in sbom mode" "$action_file" "image-digest is required when scan-mode=sbom"
  assert_file_contains "validates sbom-output-file in sbom mode" "$action_file" "sbom-output-file is required when scan-mode=sbom"
}

# --- Run all tests ---
echo "=== Composite action verification tests (CC-0055, REQ-010) ==="
echo ""
test_spdx_headers
echo ""
test_feature_id_comment
echo ""
test_shell_explicit
echo ""
test_sha_pinned_actions
echo ""
test_supply_chain_attest_input_validation
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
