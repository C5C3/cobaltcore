#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/quick-start.md Step 3 coverage:
#   - The `What happens` table gained a `Step 2b` row for
#     `Install Envoy Gateway + Gateway/openstack-gw`
#   - The `What gets deployed` architecture snapshot lists
#     `envoy-gateway-system   envoy-gateway-*   Ready`
#
#
#
# Usage: bash tests/unit/docs/quick_start_coverage_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START="$PROJECT_ROOT/docs/quick-start-extended.md"

if [[ ! -f "$QUICK_START" ]]; then
  echo "FAIL: $QUICK_START does not exist"
  exit 1
fi

# --- Test 1: Step 2b row in the "What happens" table ---
# Matches a table row beginning with `| 2b |` describing Envoy Gateway +
# openstack-gw. We check the opening cell and a couple of descriptive
# substrings rather than the full line so we don't break on reword.
test_step_2b_table_row_present() {
  echo "Test: Step 3 'What happens' table contains a 2b row for Envoy Gateway + openstack-gw"

  if ! grep -Eq '^\| 2b \|' "$QUICK_START"; then
    echo "  FAIL: no table row starting with '| 2b |' found"
    FAIL=$((FAIL + 1))
    return
  fi
  PASS=$((PASS + 1))
  echo "  PASS: found a row starting with '| 2b |'"

  # Extract the single Step 2b row and confirm content.
  local row
  row="$(grep -E '^\| 2b \|' "$QUICK_START" | head -n1)"

  assert_contains "Step 2b row names Envoy Gateway" "$row" "Envoy Gateway"
  assert_contains "Step 2b row names openstack-gw" "$row" "openstack-gw"
}

# --- Test 2: 2b row sits between 2a and 3 ---
test_step_2b_row_position() {
  echo "Test: Step 2b row is positioned between Step 2a and Step 3 in the table"

  local line_2a line_2b line_3
  line_2a="$( { grep -nE '^\| 2a \|' "$QUICK_START" || true; } | head -n1 | cut -d: -f1)"
  line_2b="$( { grep -nE '^\| 2b \|' "$QUICK_START" || true; } | head -n1 | cut -d: -f1)"
  line_3="$( { grep -nE '^\| 3 \|'  "$QUICK_START" || true; } | head -n1 | cut -d: -f1)"

  if [[ -z "$line_2a" || -z "$line_2b" || -z "$line_3" ]]; then
    echo "  FAIL: could not locate all of Step 2a / 2b / 3 table rows"
    echo "    2a: '${line_2a:-<none>}'  2b: '${line_2b:-<none>}'  3: '${line_3:-<none>}'"
    FAIL=$((FAIL + 1))
    return
  fi

  if (( line_2a < line_2b && line_2b < line_3 )); then
    echo "  PASS: 2a (line $line_2a) < 2b (line $line_2b) < 3 (line $line_3)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: expected 2a < 2b < 3 (got 2a=$line_2a 2b=$line_2b 3=$line_3)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 3: Architecture snapshot lists envoy-gateway-system pod ---
test_snapshot_lists_envoy_gateway() {
  echo "Test: 'What gets deployed' snapshot lists envoy-gateway-system  envoy-gateway-*  Ready"

  # Match the row across whitespace variations. Uses extended regex.
  if grep -Eq 'envoy-gateway-system[[:space:]]+envoy-gateway-\*[[:space:]]+Ready' "$QUICK_START"; then
    echo "  PASS: snapshot contains the envoy-gateway-system entry"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: snapshot missing 'envoy-gateway-system   envoy-gateway-*   Ready'"
    FAIL=$((FAIL + 1))
  fi
}

# --- Run ---
test_step_2b_table_row_present
test_step_2b_row_position
test_snapshot_lists_envoy_gateway

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
