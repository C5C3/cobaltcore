#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that every chainsaw suite the e2e-controlplane job runs is bounded by a
# wall clock the job's own timeout-minutes accounts for.
#
# Chainsaw applies a test's `exec` budget to EVERY script operation, not just to
# the one in `try`. A suite whose `catch` and `finally` scripts carry no explicit
# timeout therefore has a ceiling of exec x 3 + cleanup, not exec + cleanup — and
# the three suites run in sequence on one node, so an unbounded diagnostic dump
# against a wedged apiserver (where a kubectl call blocks instead of erroring,
# and `2>&1 || true` bounds the exit status rather than the duration) can carry
# the job past its wall. GitHub then SIGKILLs the runner process mid-catch, no
# JUnit report is written, and a stalled suite is reported as a cancelled job
# with nothing to read it out of — the outcome the wall was raised to prevent.
#
# The suite list is read out of ci.yaml rather than hardcoded, so a fourth
# chainsaw step is covered the day it lands.
#
# Usage: bash tests/unit/ci/e2e_controlplane_suite_budget_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
CHAINSAW_CONFIG="$PROJECT_ROOT/tests/e2e/chainsaw-config.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
# to_minutes — a chainsaw duration ("50m", "300s", "1h") as whole minutes,
# rounded up so a sub-minute budget never reads as zero.
to_minutes() {
  local d="$1" n unit
  n="${d%[a-z]}"
  unit="${d##*[0-9]}"
  case "$unit" in
    h) echo $(( n * 60 )) ;;
    m) echo "$n" ;;
    s) echo $(( (n + 59) / 60 )) ;;
    *) echo 0 ;;
  esac
}

# suite_ceiling — exec/catch/finally script budgets plus the cleanup that runs
# after them, in minutes. An unpinned script operation inherits the test's exec
# budget, which is what makes an unpinned catch expensive.
suite_ceiling() {
  local file="$1" exec_default cleanup total t
  exec_default="$(yq -r '.spec.timeouts.exec // ""' "$file")"
  [ -n "$exec_default" ] || exec_default="$(yq -r '.spec.timeouts.exec' "$CHAINSAW_CONFIG")"
  cleanup="$(yq -r '.spec.timeouts.cleanup // ""' "$file")"
  [ -n "$cleanup" ] || cleanup="$(yq -r '.spec.timeouts.cleanup' "$CHAINSAW_CONFIG")"

  total="$(to_minutes "$cleanup")"
  while read -r t; do
    [ -n "$t" ] || continue
    [ "$t" = "null" ] && t="$exec_default"
    total=$(( total + $(to_minutes "$t") ))
  done < <(yq -r '.spec.steps[] | (.try // []) + (.catch // []) + (.finally // [])
                  | .[] | select(has("script")) | .script.timeout' "$file")
  echo "$total"
}

# controlplane_suites — the suite directories the e2e-controlplane job runs, in
# the order its steps run them.
controlplane_suites() {
  yq -r '.jobs.e2e-controlplane.steps[].run // ""' "$CI_YAML" \
    | grep -oE 'tests/e2e/[A-Za-z0-9/_-]+/' \
    | sed 's:/*$::' \
    | awk '!seen[$0]++'
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------
test_catch_and_finally_are_bounded() {
  echo "Test: every catch and finally script of an e2e-controlplane suite pins a timeout"

  local suite file unpinned
  for suite in $SUITES; do
    file="$PROJECT_ROOT/$suite/chainsaw-test.yaml"
    unpinned="$(yq -r '.spec.steps[] | (.catch // []) + (.finally // [])
                       | .[] | select(has("script")) | .script.timeout' "$file" \
                | grep -c '^null$')"
    assert_eq "$suite pins every catch/finally script timeout" "0" "$unpinned"
  done
}

test_no_suite_outlasts_the_job_wall() {
  echo "Test: no single suite ceiling exceeds the job's timeout-minutes"

  local wall suite ceiling
  wall="$(yq -r '.jobs.e2e-controlplane."timeout-minutes"' "$CI_YAML")"
  assert_not_empty "e2e-controlplane declares timeout-minutes" "$wall"

  for suite in $SUITES; do
    ceiling="$(suite_ceiling "$PROJECT_ROOT/$suite/chainsaw-test.yaml")"
    echo "    $suite ceiling: ${ceiling}m"
    assert_gte "timeout-minutes outlasts the $suite ceiling (${ceiling}m)" \
      "$wall" "$ceiling"
  done
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: yq not installed (all checks skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

SUITES="$(controlplane_suites)"
if [ -z "$SUITES" ]; then
  echo "FAIL: found no chainsaw suite directories in the e2e-controlplane job"
  exit 1
fi

test_catch_and_finally_are_bounded
test_no_suite_outlasts_the_job_wall

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
