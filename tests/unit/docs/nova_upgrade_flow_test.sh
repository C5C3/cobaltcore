#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/reference/nova/nova-upgrade-flow.md still carries the facts a
# reader driving a release upgrade depends on:
#   1. The "## Phases" section heading exists.
#   2. Inside that section the four phases appear in flow order.
#   3. The rollout order literal
#      "conductor -> scheduler -> metadata -> novncproxy -> api".
#   4. The termination-log literal "nova-status upgrade check exit" the expand
#      script writes, which is what the two upgrade-check events carry.
#   5. The three phase Job names.
#   6. The cross-link to the reconciler reference.
#   7. The abort recipe reverts spec.image.tag alongside spec.openStackRelease.
#      Reverting the release alone triggers the abort and then parks the CR on
#      ImageReleaseMismatch, because the lockstep guard is skipped only while a
#      phase is set (reconcile_database.go).
#   8. The same recipe carries the digest caveat. ImageSpec enforces
#      `has(self.tag) != has(self.digest)` (internal/common/types/types.go), so
#      on a digest-pinned Nova the copied patch is rejected whole and the
#      release revert does not land either.
#   9. The cell0 pass of the contract phase: online_data_migrations does not
#      fan out to cell0, so the page has to name the "_cell0" rewrite.
#
# Usage: bash tests/unit/docs/nova_upgrade_flow_test.sh
#   UPGRADE_DOC=<path> overrides the page under test.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

UPGRADE_DOC="${UPGRADE_DOC:-$PROJECT_ROOT/docs/reference/nova/nova-upgrade-flow.md}"

if [[ ! -f "$UPGRADE_DOC" ]]; then
  echo "FAIL: $UPGRADE_DOC does not exist"
  exit 1
fi

# --- Test 1: the Phases section exists ---
test_phases_heading_exists() {
  echo "Test: '## Phases' heading exists"

  if grep -qF -- '## Phases' "$UPGRADE_DOC"; then
    echo "  PASS: '## Phases' heading present"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $UPGRADE_DOC is missing the '## Phases' heading"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: the four phases appear in flow order ---
test_phases_in_flow_order() {
  echo "Test: the four phases appear in flow order inside '## Phases'"

  local section previous=0 phase line
  section="$(awk '/^## Phases$/ {inside = 1; next} inside && /^## / {exit} inside' "$UPGRADE_DOC")"

  for phase in Expanding Migrating RollingUpdate Contracting; do
    line="$(printf '%s\n' "$section" | grep -nF -- "$phase" | head -1 | cut -d: -f1)"
    if [[ -z "$line" ]]; then
      echo "  FAIL: phase '$phase' is not named in the '## Phases' section of $UPGRADE_DOC"
      FAIL=$((FAIL + 1))
      return
    fi
    if [[ "$line" -le "$previous" ]]; then
      echo "  FAIL: phase '$phase' is out of order (section line $line, after section line $previous)"
      FAIL=$((FAIL + 1))
      return
    fi
    previous="$line"
  done

  echo "  PASS: Expanding, Migrating, RollingUpdate, Contracting appear in flow order"
  PASS=$((PASS + 1))
}

# --- Test 3: the rollout order literal ---
test_rollout_order_documented() {
  echo "Test: the RollingUpdate roll order is spelled out"

  assert_file_contains_fixed "roll order conductor -> scheduler -> metadata -> novncproxy -> api" \
    "$UPGRADE_DOC" \
    'conductor -> scheduler -> metadata -> novncproxy -> api'
}

# --- Test 4: the upgrade-check termination-log literal ---
test_upgrade_check_literal_documented() {
  echo "Test: the upgrade-check termination-log literal is documented"

  assert_file_contains_fixed "the 'nova-status upgrade check exit' literal is documented" \
    "$UPGRADE_DOC" 'nova-status upgrade check exit'
}

# --- Test 5: the three phase Job names ---
test_phase_job_names_documented() {
  echo "Test: the three phase Job names are documented"

  assert_file_contains_fixed "expand phase Job name"   "$UPGRADE_DOC" '{name}-db-expand'
  assert_file_contains_fixed "migrate phase Job name"  "$UPGRADE_DOC" '{name}-db-migrate'
  assert_file_contains_fixed "contract phase Job name" "$UPGRADE_DOC" '{name}-db-contract'
}

# --- Test 6: the cross-link to the reconciler reference ---
test_links_reconciler_reference() {
  echo "Test: the page links the reconciler reference"

  assert_file_contains_fixed "cross-link to nova-reconciler.md" \
    "$UPGRADE_DOC" './nova-reconciler.md'
}

# --- Test 7: the abort recipe reverts both fields ---
test_abort_recipe_reverts_image_tag() {
  echo "Test: the abort recipe reverts spec.image.tag alongside the release"

  local recipe
  recipe="$(awk '/# Abort an in-flight upgrade/ {inside = 1} inside {print} inside && /^```$/ {exit}' "$UPGRADE_DOC")"

  if [[ -z "$recipe" ]]; then
    echo "  FAIL: $UPGRADE_DOC carries no '# Abort an in-flight upgrade' patch recipe"
    FAIL=$((FAIL + 1))
    return
  fi

  local key
  for key in '"openStackRelease"' '"image":{"tag"'; do
    if grep -qF -- "$key" <<<"$recipe"; then
      echo "  PASS: the abort patch sets $key"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: the abort patch does not set $key; reverting the release alone parks the CR on ImageReleaseMismatch"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 8: the abort recipe warns about a digest-pinned image ---
test_abort_recipe_carries_digest_caveat() {
  echo "Test: the abort recipe warns that tag and digest are mutually exclusive"

  local recipe
  recipe="$(awk '/# Abort an in-flight upgrade/ {inside = 1} inside {print} inside && /^```$/ {exit}' "$UPGRADE_DOC")"

  if [[ -z "$recipe" ]]; then
    echo "  FAIL: $UPGRADE_DOC carries no '# Abort an in-flight upgrade' patch recipe"
    FAIL=$((FAIL + 1))
    return
  fi

  # The caveat has to sit inside the block: the prose below it is not copied
  # under pressure, and a digest-pinned CR rejects the patch as a whole.
  local phrase
  for phrase in 'Digest-pinned image' 'mutually'; do
    if grep -qF -- "$phrase" <<<"$recipe"; then
      echo "  PASS: the abort recipe names '$phrase'"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: the abort recipe does not name '$phrase'; a digest-pinned Nova rejects the whole patch"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 9: the cell0 pass of the contract phase ---
test_cell0_pass_documented() {
  echo "Test: the contract phase's cell0 pass is documented"

  assert_file_contains_fixed "the _cell0 connection rewrite is documented" \
    "$UPGRADE_DOC" '_cell0'
}

# --- Run ---
test_phases_heading_exists
test_phases_in_flow_order
test_rollout_order_documented
test_upgrade_check_literal_documented
test_phase_job_names_documented
test_links_reconciler_reference
test_abort_recipe_reverts_image_tag
test_abort_recipe_carries_digest_caveat
test_cell0_pass_documented

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
