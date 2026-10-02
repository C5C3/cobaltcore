#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the checks of patch 0001 in the build step of
# images/kvm-node-agent/Dockerfile.
#
# The build runs the patch's two tests from a test binary and greps each log
# for the test's top-level PASS line. A test binary exits 0 when its test is
# missing, and TestUpdateTLSCertificateKeyGroupNotPermitted skips as root, so
# the greps are what fail a build whose patch lost its test hunk or whose
# second run was root. They have to match a top-level PASS line and nothing
# else: not a subtest's PASS line, not 'no tests to run', not a SKIP and not
# a FAIL. The second run has to drop root.
# The test reads the build step from the Dockerfile and runs each grep
# against a log written for each case.
#
# Usage: bash tests/unit/images/kna_patch_test_guard_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DOCKERFILE="$PROJECT_ROOT/images/kvm-node-agent/Dockerfile"

ROOT_LOG=/tmp/patch-test.log
NOBODY_LOG=/tmp/patch-test-nobody.log

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# segments prints the commands of the build step that writes $ROOT_LOG, one
# per line: the RUN instruction with its continuation lines joined, split at
# each ' && '.
segments() {
  awk -v file="$ROOT_LOG" '
    /^RUN / { step = ""; in_run = 1 }
    in_run {
      line = $0
      more = sub(/\\$/, "", line)
      step = step " " line
      if (!more) {
        in_run = 0
        if (index(step, "tee " file)) { gsub(/[[:space:]]+/, " ", step); gsub(/ && /, "\n", step); print step }
      }
    }
  ' "$DOCKERFILE"
}

# run_of <log>: the command that writes <log>.
run_of() {
  segments | awk -v file="$1" '$NF == file && index($0, "| tee ")'
}

# guard_of <log>: the command that checks <log>, a grep whose last word is
# <log>.
guard_of() {
  segments | awk -v file="$1" '$1 == "grep" && $NF == file'
}

# guard_rc <log> <content>
# Runs the guard of <log> against a file holding <content> instead of <log>
# and prints its exit status: 0 lets the build go on, any other fails it.
guard_rc() {
  local guard fixture
  guard="$(guard_of "$1")"
  fixture="$(mktemp "$TMP_DIR/log.XXXXXX")"
  printf '%s\n' "$2" > "$fixture"
  eval "${guard% *} \"\$fixture\"" > /dev/null 2>&1
  echo "$?"
}

# --- Test 1: the build step carries both runs and both guards ---
test_build_step_carries_the_runs_and_guards() {
  echo "Test: the build step runs both tests, the second without root, and greps each log"

  assert_contains "the first run selects TestUpdateTLSCertificateKeyMode" \
    "$(run_of "$ROOT_LOG")" "-test.run '^TestUpdateTLSCertificateKeyMode\$'"
  assert_contains "the second run selects TestUpdateTLSCertificateKeyGroupNotPermitted" \
    "$(run_of "$NOBODY_LOG")" "-test.run '^TestUpdateTLSCertificateKeyGroupNotPermitted\$'"
  assert_starts_with "the second run drops root" \
    "$(run_of "$NOBODY_LOG")" "setpriv --reuid=65534 --regid=65534 --clear-groups "
  assert_starts_with "a grep checks the first log" "$(guard_of "$ROOT_LOG")" "grep "
  assert_starts_with "a grep checks the second log" "$(guard_of "$NOBODY_LOG")" "grep "
}

# --- Test 2: a top-level PASS line lets the build go on ---
test_top_level_pass_goes_on() {
  echo "Test: a log with the test's top-level PASS line lets the build go on"

  assert_eq "the first test's log goes on" "0" "$(guard_rc "$ROOT_LOG" '=== RUN   TestUpdateTLSCertificateKeyMode
=== RUN   TestUpdateTLSCertificateKeyMode/new_files
--- PASS: TestUpdateTLSCertificateKeyMode (0.01s)
    --- PASS: TestUpdateTLSCertificateKeyMode/new_files (0.00s)
PASS')"
  assert_eq "the second test's log goes on" "0" "$(guard_rc "$NOBODY_LOG" '=== RUN   TestUpdateTLSCertificateKeyGroupNotPermitted
--- PASS: TestUpdateTLSCertificateKeyGroupNotPermitted (0.00s)
PASS')"
}

# --- Test 3: every other log fails the build ---
test_other_logs_fail() {
  echo "Test: a subtest's PASS, no tests, a SKIP and a FAIL fail the build"

  assert_eq "a subtest's PASS line alone fails the build" "1" "$(guard_rc "$ROOT_LOG" '=== RUN   TestUpdateTLSCertificateKeyMode
=== RUN   TestUpdateTLSCertificateKeyMode/new_files
    --- PASS: TestUpdateTLSCertificateKeyMode/new_files (0.00s)')"
  assert_eq "a missing test fails the build" "1" "$(guard_rc "$ROOT_LOG" 'testing: warning: no tests to run
PASS')"
  assert_eq "a FAIL with a passing subtest fails the build" "1" "$(guard_rc "$ROOT_LOG" '=== RUN   TestUpdateTLSCertificateKeyMode
--- FAIL: TestUpdateTLSCertificateKeyMode (0.01s)
    --- PASS: TestUpdateTLSCertificateKeyMode/new_files (0.00s)
    --- FAIL: TestUpdateTLSCertificateKeyMode/key_group (0.00s)
FAIL')"
  assert_eq "a skip, as in a run that was root, fails the build" "1" "$(guard_rc "$NOBODY_LOG" '=== RUN   TestUpdateTLSCertificateKeyGroupNotPermitted
    manage_libvirt_test.go:265: root may give a file any group
--- SKIP: TestUpdateTLSCertificateKeyGroupNotPermitted (0.00s)
PASS')"
  assert_eq "the other test's PASS line fails the build" "1" "$(guard_rc "$NOBODY_LOG" '--- PASS: TestUpdateTLSCertificateKeyMode (0.01s)
PASS')"
}

# --- Run ---
test_build_step_carries_the_runs_and_guards
test_top_level_pass_goes_on
test_other_logs_fail

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
