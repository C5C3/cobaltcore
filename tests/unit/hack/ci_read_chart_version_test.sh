#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-read-chart-version.sh, the one reader of the chart version a
# Chart.yaml or a baseline.yaml pin names:
#   - a bare, a double-quoted and a single-quoted version print the same value;
#   - the first top-level `version:` line wins, and an indented one is not read;
#   - a missing file and a file without a `version:` line print nothing and
#     exit 0;
#   - a call without exactly one argument fails with an ::error:: line.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh).
#
# Usage: bash tests/unit/hack/ci_read_chart_version_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
READ_SH="$PROJECT_ROOT/hack/ci-read-chart-version.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# run_read <args...> — runs the script. Stores stdout in OUT, stderr in ERR and
# the exit status in RC.
run_read() {
  RC=0
  OUT=$(bash "$READ_SH" "$@" 2>"$TMP_DIR/stderr") || RC=$?
  ERR=$(cat "$TMP_DIR/stderr")
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_every_quoting_reads_the_same_version() {
  echo "Test: a bare, a double-quoted and a single-quoted version read alike"

  local form
  for form in 'version: 0.6.0' 'version: "0.6.0"' "version: '0.6.0'"; do
    printf 'apiVersion: v2\nname: glance-operator\n%s\nappVersion: "0.1.0"\n' "$form" >"$TMP_DIR/Chart.yaml"
    run_read "$TMP_DIR/Chart.yaml"
    assert_eq "$form exits 0" "0" "$RC"
    assert_eq "$form prints 0.6.0" "0.6.0" "$OUT"
  done
}

test_the_first_top_level_line_wins() {
  echo "Test: the first top-level version: line is the one read"

  printf 'dependencies:\n  - name: operator-library\n    version: 0.10.0\nversion: 0.14.0\nversion: 0.15.0\n' \
    >"$TMP_DIR/Chart.yaml"
  run_read "$TMP_DIR/Chart.yaml"
  assert_eq "it exits 0" "0" "$RC"
  assert_eq "it prints the first top-level version" "0.14.0" "$OUT"
}

test_a_file_without_a_version_prints_nothing() {
  echo "Test: a missing file and a file without a version: line print nothing"

  run_read "$TMP_DIR/absent.yaml"
  assert_eq "a missing file exits 0" "0" "$RC"
  assert_eq "it prints nothing" "" "$OUT"
  assert_eq "it reports nothing" "" "$ERR"

  printf '# scratch pin\n' >"$TMP_DIR/baseline.yaml"
  run_read "$TMP_DIR/baseline.yaml"
  assert_eq "a file without a version: line exits 0" "0" "$RC"
  assert_eq "it prints nothing" "" "$OUT"
}

test_a_wrong_argument_count_fails() {
  echo "Test: a call without exactly one argument fails"

  run_read
  assert_eq "no argument exits 1" "1" "$RC"
  assert_eq "it names the usage" "::error::usage: hack/ci-read-chart-version.sh <file>" "$ERR"

  run_read "$TMP_DIR/a.yaml" "$TMP_DIR/b.yaml"
  assert_eq "two arguments exit 1" "1" "$RC"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_every_quoting_reads_the_same_version
test_the_first_top_level_line_wins
test_a_file_without_a_version_prints_nothing
test_a_wrong_argument_count_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
