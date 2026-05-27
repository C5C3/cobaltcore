#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/quick-start-extended.md's "Access Keystone from your local
# machine" section keeps the nip.io Gateway endpoint as the primary flow
# and `kubectl port-forward svc/keystone` confined to the Fallback
# subsection. Sub-resource Service name is bare `keystone` (no `-api`
# suffix).
#
# Assertions:
#   1. `## Access Keystone from your local machine` exists
#   2. Between that heading and the `## Fallback — kubectl port-forward`
#      subheading, there is NO `kubectl port-forward svc/keystone`
#      command (the primary flow must not rely on port-forward).
#   3. A nip.io explainer paragraph sits inside the primary section
#      (searches for `nip.io` between the two headings).
#   4. An `### Accept the self-signed certificate` subsection exists.
#   5. The `## Fallback — kubectl port-forward` subsection exists and
#      contains the port-forward command.
#
# Usage: bash tests/unit/docs/quick_start_access_section_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START="$PROJECT_ROOT/docs/quick-start-extended.md"

# --- Locate the line numbers of the three anchor headings ---
if [[ ! -f "$QUICK_START" ]]; then
  echo "FAIL: $QUICK_START does not exist"
  exit 1
fi

access_line="$( { grep -n '^## Access Keystone from your local machine$' "$QUICK_START" || true; } | head -n1 | cut -d: -f1)"
fallback_line="$( { grep -n '^### Fallback — ' "$QUICK_START" || true; } | head -n1 | cut -d: -f1)"
accept_line="$( { grep -n '^### Accept the self-signed certificate$' "$QUICK_START" || true; } | head -n1 | cut -d: -f1)"

# --- Test 1: primary section heading exists (CC-0088, REQ-007) ---
test_primary_section_heading() {
  echo "Test: '## Access Keystone from your local machine' heading exists (CC-0088, REQ-007)"
  if [[ -n "$access_line" ]]; then
    echo "  PASS: found at line $access_line"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: '## Access Keystone from your local machine' heading not found"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: no `kubectl port-forward svc/keystone` before Fallback (CC-0088, REQ-007) ---
test_no_portforward_before_fallback() {
  echo "Test: no 'kubectl port-forward svc/keystone' appears in the primary section (CC-0088, REQ-007)"

  if [[ -z "$access_line" || -z "$fallback_line" ]]; then
    echo "  FAIL: missing anchor heading(s); cannot perform range check"
    echo "    access heading line: '${access_line:-<none>}' fallback heading line: '${fallback_line:-<none>}'"
    FAIL=$((FAIL + 1))
    return
  fi

  if (( access_line >= fallback_line )); then
    echo "  FAIL: Fallback heading (line $fallback_line) must appear AFTER the primary heading (line $access_line)"
    FAIL=$((FAIL + 1))
    return
  fi

  local window
  window="$(sed -n "${access_line},$((fallback_line - 1))p" "$QUICK_START")"

  if echo "$window" | grep -q 'kubectl port-forward svc/keystone'; then
    echo "  FAIL: 'kubectl port-forward svc/keystone' found in the primary section (should be in Fallback only)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: primary section does not use 'kubectl port-forward svc/keystone'"
    PASS=$((PASS + 1))
  fi
}

# --- Test 3: nip.io explainer sits inside the primary section (CC-0088, REQ-007) ---
test_nip_io_explainer_present() {
  echo "Test: nip.io explainer paragraph appears inside the primary section (CC-0088, REQ-007)"

  if [[ -z "$access_line" || -z "$fallback_line" ]]; then
    echo "  FAIL: missing anchor heading(s); cannot perform range check"
    FAIL=$((FAIL + 1))
    return
  fi

  local window
  window="$(sed -n "${access_line},$((fallback_line - 1))p" "$QUICK_START")"

  if echo "$window" | grep -q 'nip.io'; then
    echo "  PASS: primary section mentions nip.io"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: primary section does not mention nip.io"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 4: `### Accept the self-signed certificate` subsection exists (CC-0088, REQ-007) ---
test_accept_self_signed_subsection() {
  echo "Test: '### Accept the self-signed certificate' subsection exists (CC-0088, REQ-007)"
  if [[ -n "$accept_line" ]]; then
    echo "  PASS: found at line $accept_line"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: '### Accept the self-signed certificate' heading not found"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 5: Fallback subsection exists and contains the port-forward command (CC-0088, REQ-007) ---
test_fallback_contains_portforward() {
  echo "Test: Fallback subsection contains kubectl port-forward svc/keystone (CC-0088, REQ-007)"

  if [[ -z "$fallback_line" ]]; then
    echo "  FAIL: Fallback heading not found"
    FAIL=$((FAIL + 1))
    return
  fi

  local fallback_tail
  fallback_tail="$(tail -n +"$fallback_line" "$QUICK_START")"

  if echo "$fallback_tail" | grep -q 'kubectl port-forward svc/keystone'; then
    echo "  PASS: Fallback section contains the port-forward command"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: Fallback section does not contain 'kubectl port-forward svc/keystone'"
    FAIL=$((FAIL + 1))
  fi
}

# --- Run ---
test_primary_section_heading
test_no_portforward_before_fallback
test_nip_io_explainer_present
test_accept_self_signed_subsection
test_fallback_contains_portforward

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
