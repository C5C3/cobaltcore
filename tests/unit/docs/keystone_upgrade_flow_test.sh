#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the abort recipe of docs/reference/keystone/keystone-upgrade-flow.md
# covers a Keystone whose spec.image pins a digest beside its tag:
#   1. The recipe block carries the digest caveat. ImageSpec admits a tag beside
#      a digest (`has(self.tag) || has(self.digest)`,
#      internal/common/types/types.go), and an abort that reverts the tag alone
#      is refused with ImageReleaseMismatch while the digest names the target
#      build (rejectAbortOnTargetBuild, reconcile_database.go).
#   2. The recipe block carries a patch that reverts tag and digest together.
#
# Usage: bash tests/unit/docs/keystone_upgrade_flow_test.sh
#   UPGRADE_DOC=<path> overrides the page under test.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

UPGRADE_DOC="${UPGRADE_DOC:-$PROJECT_ROOT/docs/reference/keystone/keystone-upgrade-flow.md}"

if [[ ! -f "$UPGRADE_DOC" ]]; then
  echo "FAIL: $UPGRADE_DOC does not exist"
  exit 1
fi

# abort_recipe prints the fenced block that starts at the
# '# Abort an in-flight upgrade' comment, up to its closing fence.
abort_recipe() {
  awk '/# Abort an in-flight upgrade/ {inside = 1} inside {print} inside && /^```$/ {exit}' "$UPGRADE_DOC"
}

# assert_recipe_names greps the recipe for each FIXED phrase.
assert_recipe_names() {
  local recipe="$1" why="$2" phrase
  shift 2
  for phrase in "$@"; do
    if grep -qF -- "$phrase" <<<"$recipe"; then
      echo "  PASS: the abort recipe names '$phrase'"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: the abort recipe does not name '$phrase'; $why"
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 1: the abort recipe warns about a digest-pinned image ---
test_abort_recipe_carries_digest_caveat() {
  echo "Test: the abort recipe tells a digest-pinned image to revert its digest"

  local recipe
  recipe="$(abort_recipe)"
  if [[ -z "$recipe" ]]; then
    echo "  FAIL: $UPGRADE_DOC carries no '# Abort an in-flight upgrade' patch recipe"
    FAIL=$((FAIL + 1))
    return
  fi

  # The caveat has to sit inside the block: the prose around it is not copied
  # under pressure.
  assert_recipe_names "$recipe" "a tag reverted alone beside the target digest is refused" \
    'Digest-pinned image' 'revert "digest"'
}

# --- Test 2: the abort recipe reverts tag and digest in one patch ---
test_abort_recipe_reverts_digest() {
  echo "Test: the abort recipe carries a patch that reverts tag and digest"

  local recipe
  recipe="$(abort_recipe)"
  if [[ -z "$recipe" ]]; then
    echo "  FAIL: $UPGRADE_DOC carries no '# Abort an in-flight upgrade' patch recipe"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_recipe_names "$recipe" "a digest-pinned Keystone has no abort patch to copy" \
    '"image":{"tag":"<installed-release>","digest":"<installed-digest>"}'
}

# --- Run ---
test_abort_recipe_carries_digest_caveat
test_abort_recipe_reverts_digest

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
