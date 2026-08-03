#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json gates automerge on a fully green check suite
#
#   - `platformAutomerge` is disabled, so Renovate performs the merge
#     itself instead of handing the PR to GitHub's auto-merge, which only
#     waits for checks marked required by branch protection
#   - no `ignoreTests: true` anywhere in the file weakens Renovate's
#     default of merging only once every check on the PR head succeeded
#
# Schema validation of renovate.json via `renovate-config-validator`
# (which transitively pulls Renovate via npx) is intentionally NOT run
# from this per-feature test to keep local / CI loops fast: the
# validator fetches the Renovate package on every invocation and taking
# that hit once per feature touching renovate.json multiplies the cost
# linearly. The validation is centralised in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh,
# which runs against the same renovate.json file — a schema regression
# in the automerge keys fails that test too, so coverage is preserved
# without duplicating the npx download.
#
# Usage: bash tests/unit/renovate/automerge_gating_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

# NOTE renovate-config-validator schema check is run by the
# sibling tests/unit/renovate/fluxoperator_custommanager_test.sh
# — see the header of this file for the rationale behind not duplicating
# the npx-driven validation here.

# --- Test 1: platformAutomerge is disabled at the top level ---
test_platform_automerge_disabled() {
  echo "Test: platformAutomerge is disabled so Renovate merges instead of GitHub"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  # Compared as a string against the literal "false": an absent key makes
  # jq print "null" and a re-enabled key prints "true", so both the
  # deletion and the flip back to GitHub's auto-merge fail this check.
  assert_eq "renovate.json sets platformAutomerge to false at the top level" \
    "false" \
    "$(jq -r '.platformAutomerge' "$RENOVATE_FILE")"

  # platformAutomerge is settable per packageRule, so the top-level key
  # alone is not the gate: a nested override would hand exactly the
  # auto-merged custom-manager PRs back to GitHub's auto-merge, which
  # waits for required checks alone and main defines none.
  assert_eq "renovate.json re-enables platformAutomerge at no level" \
    "0" \
    "$(jq '[.. | objects | select(.platformAutomerge == true)] | length' "$RENOVATE_FILE")"
}

# --- Test 2: no ignoreTests anywhere in the config ---
test_no_ignore_tests_anywhere() {
  echo "Test: no ignoreTests weakens the all-checks-green automerge default"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # Recursive descent covers the top level and every nested object,
  # including each packageRules entry, where an ignoreTests would let
  # Renovate merge with a red or pending check on the PR head.
  assert_eq "renovate.json carries no ignoreTests: true at any level" \
    "0" \
    "$(jq '[.. | objects | select(.ignoreTests == true)] | length' "$RENOVATE_FILE")"
}

# --- Run ---
test_platform_automerge_disabled
test_no_ignore_tests_anywhere

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
