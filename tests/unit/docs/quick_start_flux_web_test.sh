#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the Quick Start guide documents the Flux Web UI demo addon
# introduced by deploy/kind/base/flux-web.yaml:
#   - Step 4a heading and anchor exist
#   - Step 4a contains the port-forward command and URL
#   - The "Quick Start does not enable it" disclaimer has been removed
#     in favour of an affirmative Step 4a link
#   - The "What gets deployed" block lists the flux-web workload as
#     kind-only and points at Step 4a
#
# The Step 4a naming (rather than 4c) reflects the deliberate grouping
# decision raised in review #2: Headlamp (Step 4) and the Flux Web UI
# are adjacent because both render Flux state, while the OpenBao UI
# (Step 4b) is a separate concern. Keeping the Flux Web UI as 4a —
# alphabetically first — lets the reader progress Step 4 → 4a → 4b in
# document order without backtracking. The ordering is codified below
# by test_step_4a_positioned_before_step_4b so a well-meaning future
# re-alphabetisation does not silently reshuffle the two sibling UIs
# apart.
#
# Usage: bash tests/unit/docs/quick_start_flux_web_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START="$PROJECT_ROOT/docs/quick-start-extended.md"

# --- Test 1: Step 4a heading + anchor exist ---
test_step_4a_heading_and_anchor() {
  echo "Test: docs/quick-start.md has '## Step 4a — Open the Flux Web UI' heading with {#step-4a-flux-web-ui} anchor"

  if [[ ! -f "$QUICK_START" ]]; then
    echo "  FAIL: $QUICK_START does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "Step 4a heading with Flux Web UI title and anchor" \
    "$QUICK_START" \
    '^## Step 4a — Open the Flux Web UI {#step-4a-flux-web-ui}$'
}

# --- Test 2: Step 4a contains the port-forward command + URL ---
test_step_4a_portforward_and_url() {
  echo "Test: Step 4a documents 'kubectl port-forward svc/flux-web -n flux-system 9080:9080' and http://localhost:9080"

  assert_file_contains "port-forward command present" \
    "$QUICK_START" \
    'kubectl port-forward svc/flux-web -n flux-system 9080:9080'
  assert_file_contains "URL http://localhost:9080 present" \
    "$QUICK_START" \
    'http://localhost:9080'
}

# --- Test 3: Step 4a names the three flux-operator-specific kinds ---
test_step_4a_names_flux_operator_kinds() {
  echo "Test: Step 4a names ResourceSet, ResourceSetInputProvider, and FluxReport"

  assert_file_contains "Step 4a names ResourceSet" \
    "$QUICK_START" \
    'ResourceSet'
  assert_file_contains "Step 4a names ResourceSetInputProvider" \
    "$QUICK_START" \
    'ResourceSetInputProvider'
  assert_file_contains "Step 4a names FluxReport" \
    "$QUICK_START" \
    'FluxReport'
}

# --- Test 4: old disclaimer sentence has been removed ---
test_old_disclaimer_removed() {
  echo "Test: legacy 'the Quick Start does not enable it' sentence no longer appears"

  assert_file_not_contains "no 'the Quick Start does not enable it' clause" \
    "$QUICK_START" \
    'the Quick Start does not enable it'
}

# --- Test 5: the rewritten paragraph links the Step 4a anchor ---
test_flux_ui_paragraph_links_step_4a() {
  echo "Test: 'Accessing the Flux UI' paragraph links the #step-4a-flux-web-ui anchor"

  assert_file_contains "paragraph links #step-4a-flux-web-ui anchor" \
    "$QUICK_START" \
    '#step-4a-flux-web-ui'
}

# --- Test 6: 'What gets deployed' block lists flux-web-* ---
test_what_gets_deployed_lists_flux_web() {
  echo "Test: 'What gets deployed' block lists 'flux-web-*' as kind-only with Step 4a pointer"

  assert_file_contains "flux-web-* entry present" \
    "$QUICK_START" \
    'flux-web-\*'
  assert_file_contains "flux-web-* entry cites kind-only Step 4a" \
    "$QUICK_START" \
    'kind-only; see Step 4a'
}

# --- Test 7: Step 4a appears BEFORE Step 4b in the document ---
# Rationale: Step 4 (Headlamp) and Step 4a (Flux Web UI) both render Flux
# state, so they belong adjacent; Step 4b (OpenBao UI) is a separate
# concern. Pinning the order keeps Step 4 → 4a → 4b readable top-to-bottom.
test_step_4a_positioned_before_step_4b() {
  echo "Test: Step 4a appears before Step 4b in docs/quick-start.md"

  local step_4a_line step_4b_line
  step_4a_line="$( { grep -n '^## Step 4a — Open the Flux Web UI' "$QUICK_START" || true; } | head -n1 | cut -d: -f1)"
  step_4b_line="$( { grep -n '^## Step 4b — Open the OpenBao UI' "$QUICK_START" || true; } | head -n1 | cut -d: -f1)"

  if [[ -z "$step_4a_line" || -z "$step_4b_line" ]]; then
    echo "  FAIL: could not locate both Step 4a and Step 4b headings"
    echo "    Step 4a line: '${step_4a_line:-<none>}' Step 4b line: '${step_4b_line:-<none>}'"
    FAIL=$((FAIL + 1))
    return
  fi

  if (( step_4a_line < step_4b_line )); then
    echo "  PASS: Step 4a (line $step_4a_line) appears before Step 4b (line $step_4b_line)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: Step 4a (line $step_4a_line) must appear before Step 4b (line $step_4b_line)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Run ---
test_step_4a_heading_and_anchor
test_step_4a_portforward_and_url
test_step_4a_names_flux_operator_kinds
test_old_disclaimer_removed
test_flux_ui_paragraph_links_step_4a
test_what_gets_deployed_lists_flux_web
test_step_4a_positioned_before_step_4b

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
