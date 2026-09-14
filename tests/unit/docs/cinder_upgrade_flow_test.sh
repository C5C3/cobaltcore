#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/reference/cinder/cinder-upgrade-flow.md still carries the facts a
# reader driving a release upgrade depends on:
#   1. The "## Phases" section heading exists.
#   2. Inside that section the four phases appear in flow order.
#   3. The rollout order literal "scheduler -> volume -> backup -> api".
#   4. The two upgrade-check traps: the "cinder.conf" file name and the
#      "service_uuid" column that makes the check exit 2.
#   5. The three phase Job names.
#   6. The cross-link to the reconciler reference.
#   7. The abort recipe reverts spec.image.tag alongside spec.openStackRelease.
#      Reverting the release alone triggers the abort and then parks the CR on
#      ImageReleaseMismatch, because the lockstep guard is skipped only while a
#      phase is set (reconcile_database.go).
#   8. The same recipe carries the digest caveat. ImageSpec enforces
#      `has(self.tag) != has(self.digest)` (internal/common/types/types.go), so
#      on a digest-pinned Cinder the copied patch is rejected whole and the
#      release revert does not land either.
#
# Usage: bash tests/unit/docs/cinder_upgrade_flow_test.sh
#   UPGRADE_DOC=<path> overrides the page under test.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

UPGRADE_DOC="${UPGRADE_DOC:-$PROJECT_ROOT/docs/reference/cinder/cinder-upgrade-flow.md}"

if [[ ! -f "$UPGRADE_DOC" ]]; then
  echo "FAIL: $UPGRADE_DOC does not exist"
  exit 1
fi

# assert_file_contains_fixed greps a FIXED (non-regex) pattern with `--`, so a
# dotted literal such as "cinder.conf" cannot be satisfied by "cinderXconf" and
# a pattern beginning with a dash is not parsed as a grep option. The shared
# assert_file_contains uses `grep -q "$pattern"`, which expresses neither.
assert_file_contains_fixed() {
  local description="$1" file="$2" pattern="$3"
  if grep -qF -- "$pattern" "$file"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (fixed pattern '$pattern' not found in $file)"
    FAIL=$((FAIL + 1))
  fi
}

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

  assert_file_contains_fixed "roll order scheduler -> volume -> backup -> api" \
    "$UPGRADE_DOC" \
    'scheduler → volume → backup → api'
}

# --- Test 4: the two upgrade-check traps ---
test_upgrade_check_traps_documented() {
  echo "Test: the upgrade-check traps name the config file and the NULL column"

  assert_file_contains_fixed "the cinder.conf file name is documented" \
    "$UPGRADE_DOC" 'cinder.conf'
  assert_file_contains_fixed "the service_uuid column is documented" \
    "$UPGRADE_DOC" 'service_uuid'
}

# --- Test 5: the three phase Job names ---
test_phase_job_names_documented() {
  echo "Test: the three phase Job names are documented"

  assert_file_contains_fixed "expand phase Job name"   "$UPGRADE_DOC" '<cinder>-db-expand'
  assert_file_contains_fixed "migrate phase Job name"  "$UPGRADE_DOC" '<cinder>-db-migrate'
  assert_file_contains_fixed "contract phase Job name" "$UPGRADE_DOC" '<cinder>-db-contract'
}

# --- Test 6: the cross-link to the reconciler reference ---
test_links_reconciler_reference() {
  echo "Test: the page links the reconciler reference"

  assert_file_contains_fixed "cross-link to cinder-reconciler.md" \
    "$UPGRADE_DOC" './cinder-reconciler.md'
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
      echo "  FAIL: the abort recipe does not name '$phrase'; a digest-pinned Cinder rejects the whole patch"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Run ---
test_phases_heading_exists
test_phases_in_flow_order
test_rollout_order_documented
test_upgrade_check_traps_documented
test_phase_job_names_documented
test_links_reconciler_reference
test_abort_recipe_reverts_image_tag
test_abort_recipe_carries_digest_caveat

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
