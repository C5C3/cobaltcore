#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-*.sh scripts meet CI quality standards (CC-0050, review #2 comment 5)
# Validates: set -euo pipefail, shellcheck, ::error:: annotations, SPDX headers
# Usage: bash tests/ci/verify_ci_scripts.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# Collect all CI scripts
shopt -s nullglob
CI_SCRIPTS=("$PROJECT_ROOT"/hack/ci-*.sh)
shopt -u nullglob

if [[ ${#CI_SCRIPTS[@]} -eq 0 ]]; then
  echo "ERROR: No hack/ci-*.sh scripts found"
  exit 1
fi

echo "Found ${#CI_SCRIPTS[@]} CI scripts to verify"
echo ""

# --- Test 1: All ci-*.sh scripts have set -euo pipefail ---
test_set_euo_pipefail() {
  echo "Test: all hack/ci-*.sh scripts have 'set -euo pipefail'"

  for script in "${CI_SCRIPTS[@]}"; do
    local name
    name="$(basename "$script")"
    assert_file_contains "$name has set -euo pipefail" "$script" "set -euo pipefail"
  done
}

# --- Test 2: All ci-*.sh scripts pass shellcheck ---
test_shellcheck() {
  echo "Test: all hack/ci-*.sh scripts pass shellcheck"

  if ! command -v shellcheck >/dev/null 2>&1; then
    echo "  SKIP: shellcheck not installed (${#CI_SCRIPTS[@]} checks skipped)"
    SKIP=$((SKIP + ${#CI_SCRIPTS[@]}))
    return
  fi

  for script in "${CI_SCRIPTS[@]}"; do
    local name
    name="$(basename "$script")"
    if shellcheck --severity=warning "$script" >/dev/null 2>&1; then
      echo "  PASS: $name passes shellcheck"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $name fails shellcheck:"
      shellcheck --severity=warning "$script" 2>&1 | head -20
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 3: Scripts with required env vars have ::error:: annotations ---
test_error_annotations() {
  echo "Test: scripts with required env vars have ::error:: annotations"

  for script in "${CI_SCRIPTS[@]}"; do
    local name
    name="$(basename "$script")"

    # A script "requires" env vars if it uses the ${VAR:?msg} pattern
    # or has an explicit validation block that checks for required vars.
    # Scripts with only optional/defaulted vars are not checked.
    if grep -qE '\$\{[A-Z_]+:\?' "$script" \
       || grep -qE '::error::.*must be set' "$script"; then
      if grep -q '::error::' "$script"; then
        echo "  PASS: $name has ::error:: annotations for env var validation"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: $name requires env vars but has no ::error:: annotations"
        FAIL=$((FAIL + 1))
      fi
    else
      echo "  SKIP: $name has no required env vars"
      SKIP=$((SKIP + 1))
    fi
  done
}

# --- Test 4: All ci-*.sh scripts have SPDX license headers ---
test_spdx_headers() {
  echo "Test: all hack/ci-*.sh scripts have SPDX license headers"

  for script in "${CI_SCRIPTS[@]}"; do
    local name
    name="$(basename "$script")"
    # Line 1 is shebang, line 2 should be SPDX copyright, line 4 should be license
    local line2 line4
    line2=$(sed -n '2p' "$script")
    line4=$(sed -n '4p' "$script")

    local has_copyright=false has_license=false
    if [[ "$line2" == *"SPDX-FileCopyrightText"* ]]; then has_copyright=true; fi
    if [[ "$line4" == *"SPDX-License-Identifier"* ]]; then has_license=true; fi

    if $has_copyright && $has_license; then
      echo "  PASS: $name has SPDX header"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $name missing SPDX header (copyright=$has_copyright, license=$has_license)"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Run all tests ---
echo "=== CI script verification tests (CC-0050) ==="
echo ""
test_set_euo_pipefail
echo ""
test_shellcheck
echo ""
test_error_annotations
echo ""
test_spdx_headers
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
