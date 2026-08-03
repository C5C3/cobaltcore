#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the test-shell job in .github/workflows/ci.yaml stays reachable
# from the push runs for main and v* tags.
#
# test-shell is the one gate job that deliberately carries no
# `github.event_name == 'pull_request'` guard: a lockstep breakage that
# lands on main has to fail main's own run at the commit that caused it
# instead of surfacing on the next, unrelated PR branch. Nothing else
# enforces that — re-adding the `if:` during a workflow refactor restores
# the old behaviour silently and leaves the two claims in
# docs/reference/ci-cd/ci-workflow.md false. The same is true of the push
# trigger itself: dropping `main` or `v*` from `on.push` takes the job off
# those runs without touching the job block.
#
# Usage: bash tests/unit/ci/test_shell_event_guard_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the body of the ci.yaml block whose key line matches $1, where the
# key sits at $2 spaces of indent: every following line indented deeper,
# up to the first line at the same or lower indent. Blank lines belong to
# the block so a step separated by one does not truncate it.
extract_block() {
  local key_re="$1" indent="$2"
  awk -v key_re="$key_re" -v indent="$indent" '
    !found && $0 ~ key_re { found = 1; next }
    found {
      if ($0 ~ /^[[:space:]]*$/) { print; next }
      match($0, /^ */)
      if (RLENGTH <= indent) { exit }
      print
    }
  ' "$CI_YAML"
}

# PASS when the YAML block $2 lists $3 (an ERE) as a whole entry, in flow
# (`[main]`) or block (`- main`) style, alone or alongside others. A plain
# substring test would accept a `branches: [main-experiment]` that no
# longer covers main; pinning one exact spelling would instead turn red
# when a second branch is added or a formatter switches the style, and
# name the wrong cause while doing it.
assert_lists_entry() {
  local description="$1" block="$2" entry="$3"
  local boundary='[^A-Za-z0-9_./-]'

  if printf '%s\n' "$block" | grep -qE "(^|${boundary})${entry}(${boundary}|\$)"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected a list entry matching: $entry"
    printf '%s\n' "$block" | sed 's/^/    /'
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_shell_job_carries_no_event_guard() {
  echo "Test: the test-shell job block declares no if: key"

  local block
  block="$(extract_block '^  test-shell:$' 2)"

  # Without this the next check passes vacuously on a renamed job.
  assert_contains "the test-shell job block is extractable and runs make test-shell" \
    "$block" "make test-shell"

  # Job-level keys sit at four spaces; step-level keys are deeper, so this
  # matches the event guard without matching a step's own `if:`.
  if printf '%s\n' "$block" | grep -qE '^    if:'; then
    echo "  FAIL: test-shell carries a job-level if: — it must run on push to main and v* tags too"
    printf '%s\n' "$block" | grep -E '^    if:' | sed 's/^/    /'
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: test-shell carries no job-level if:"
    PASS=$((PASS + 1))
  fi
}

test_push_trigger_covers_main_and_tags() {
  echo "Test: on.push still covers the main branch and v* tags"

  local block
  block="$(extract_block '^  push:$' 2)"

  # The push block carries only branches / tags filters (the paths check
  # below keeps it that way), so matching the entry inside the block is
  # as precise as matching the filter line — and survives a second branch
  # or a block-style rewrite.
  assert_lists_entry "on.push covers the main branch" "$block" 'main'
  assert_lists_entry "on.push covers v-prefixed tags" "$block" 'v\*'

  # A paths / paths-ignore filter takes the job off the very runs this guard
  # exists for — a push to main that only touches tests/ is exactly the
  # lockstep breakage that has to fail main's own run.
  if printf '%s\n' "$block" | grep -qE '^ *paths(-ignore)?:'; then
    echo "  FAIL: on.push carries a paths filter — it can skip test-shell on a push to main"
    printf '%s\n' "$block" | grep -E '^ *paths(-ignore)?:' | sed 's/^/    /'
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: on.push carries no paths filter"
    PASS=$((PASS + 1))
  fi
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_shell_job_carries_no_event_guard
test_push_trigger_covers_main_and_tags

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
