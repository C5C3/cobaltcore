#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the checks of patch 0001 in images/kvm-node-agent/patch-checks.sh,
# which the checkPhase of the manager build in manager.nix runs.
#
# The script runs the patch's two tests in one go test run and checks the
# log for each with require_pass, a grep for the test's top-level PASS line.
# A test binary exits 0 when its test is missing, and
# TestUpdateTLSCertificateKeyGroupNotPermitted skips as root, so require_pass
# is what fails a build whose patch lost its test hunk or that ran as root.
# It has to match a top-level PASS line and nothing else: not a subtest's
# PASS line, not 'no tests to run', not a SKIP and not a FAIL. refuse_root
# has to fail a run as root before any test runs. A run that exits non-zero
# after both PASS lines, as when a goroutine a test left behind panics, has
# to fail main as well.
# The test sources the script and runs require_pass against a log written
# for each case, refuse_root under a stub id, and main with a go stub.
#
# Usage: bash tests/unit/images/kna_patch_test_guard_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
KNA_DIR="$PROJECT_ROOT/images/kvm-node-agent"
CHECKS="$KNA_DIR/patch-checks.sh"

MODE_TEST=TestUpdateTLSCertificateKeyMode
GROUP_TEST=TestUpdateTLSCertificateKeyGroupNotPermitted

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=images/kvm-node-agent/patch-checks.sh
source "$CHECKS"
# The script turns errexit on for its own run; the cases here run failing
# commands on purpose.
set +e

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# guard_rc <test> <content>
# Runs require_pass for <test> against a log holding <content> and prints
# its status: 0 lets the build go on, any other fails it.
guard_rc() {
  local fixture
  fixture="$(mktemp "$TMP_DIR/log.XXXXXX")"
  printf '%s\n' "$2" > "$fixture"
  require_pass "$fixture" "$1" > /dev/null 2>&1
  echo "$?"
}

# run_main <logs> <go status>
# Runs main with TMPDIR=<logs>, under the errexit and pipefail the script
# runs with, as a build user, and a go stub in place of go that prints the
# top-level PASS line of both tests and exits with <go status>. Prints what
# main wrote to stderr, then status=<main's status>.
run_main() {
  local logs="$1" go_status="$2"
  {
    (
      set -euo pipefail
      id() { echo 1000; }
      go() {
        echo "--- PASS: $MODE_TEST (0.01s)"
        echo "--- PASS: $GROUP_TEST (0.00s)"
        [ "$go_status" -eq 0 ] || echo "panic: send on closed channel"
        return "$go_status"
      }
      export TMPDIR="$logs"
      main
    ) > /dev/null
  } 2>&1
  echo "status=$?"
}

# --- Test 1: the manager build runs both tests and both guards ---
test_manager_build_runs_the_tests_and_guards() {
  echo "Test: manager.nix runs patch-checks.sh, which runs both tests and checks the log for each"

  local check_phase
  check_phase="$(awk '/checkPhase = /,/^  '"''"';$/' "$KNA_DIR/manager.nix")"
  assert_contains "the checkPhase runs patch-checks.sh" "$check_phase" 'bash ${./patch-checks.sh}'
  assert_file_contains_fixed "the run selects $MODE_TEST" "$CHECKS" "-run '^($MODE_TEST|"
  assert_file_contains_fixed "the run selects $GROUP_TEST" "$CHECKS" "|$GROUP_TEST)\$'"
  assert_file_contains_fixed "require_pass checks the log for $MODE_TEST" "$CHECKS" "require_pass \"\$TMPDIR/patch-0001.log\" $MODE_TEST"
  assert_file_contains_fixed "require_pass checks the log for $GROUP_TEST" "$CHECKS" "require_pass \"\$TMPDIR/patch-0001.log\" $GROUP_TEST"
  assert_starts_with "main refuses root first" \
    "$(declare -f main | sed -n '3p' | sed 's/^ *//')" "refuse_root"
}

# --- Test 2: a top-level PASS line lets the build go on ---
test_top_level_pass_goes_on() {
  echo "Test: a log with the test's top-level PASS line lets the build go on"

  assert_eq "the first test's log goes on" "0" "$(guard_rc "$MODE_TEST" '=== RUN   TestUpdateTLSCertificateKeyMode
=== RUN   TestUpdateTLSCertificateKeyMode/new_files
--- PASS: TestUpdateTLSCertificateKeyMode (0.01s)
    --- PASS: TestUpdateTLSCertificateKeyMode/new_files (0.00s)
PASS')"
  assert_eq "the second test's log goes on" "0" "$(guard_rc "$GROUP_TEST" '=== RUN   TestUpdateTLSCertificateKeyGroupNotPermitted
--- PASS: TestUpdateTLSCertificateKeyGroupNotPermitted (0.00s)
PASS')"
}

# --- Test 3: every other log fails the build ---
test_other_logs_fail() {
  echo "Test: a subtest's PASS, no tests, a SKIP and a FAIL fail the build"

  assert_eq "a subtest's PASS line alone fails the build" "1" "$(guard_rc "$MODE_TEST" '=== RUN   TestUpdateTLSCertificateKeyMode
=== RUN   TestUpdateTLSCertificateKeyMode/new_files
    --- PASS: TestUpdateTLSCertificateKeyMode/new_files (0.00s)')"
  assert_eq "a missing test fails the build" "1" "$(guard_rc "$MODE_TEST" 'testing: warning: no tests to run
PASS')"
  assert_eq "a FAIL with a passing subtest fails the build" "1" "$(guard_rc "$MODE_TEST" '=== RUN   TestUpdateTLSCertificateKeyMode
--- FAIL: TestUpdateTLSCertificateKeyMode (0.01s)
    --- PASS: TestUpdateTLSCertificateKeyMode/new_files (0.00s)
    --- FAIL: TestUpdateTLSCertificateKeyMode/key_group (0.00s)
FAIL')"
  assert_eq "a skip, as in a run that was root, fails the build" "1" "$(guard_rc "$GROUP_TEST" '=== RUN   TestUpdateTLSCertificateKeyGroupNotPermitted
    manage_libvirt_test.go:265: root may give a file any group
--- SKIP: TestUpdateTLSCertificateKeyGroupNotPermitted (0.00s)
PASS')"
  assert_eq "the other test's PASS line fails the build" "1" "$(guard_rc "$GROUP_TEST" '--- PASS: TestUpdateTLSCertificateKeyMode (0.01s)
PASS')"
}

# --- Test 4: refuse_root fails a run as root ---
test_refuse_root() {
  echo "Test: refuse_root fails a run as root and lets a build user through"

  local stubs output status=0
  stubs="$(mktemp -d "$TMP_DIR/stubs.XXXXXX")"
  printf '#!/bin/sh\necho "$ID_STUB_UID"\n' > "$stubs/id"
  chmod +x "$stubs/id"

  output="$(PATH="$stubs:$PATH" ID_STUB_UID=0 refuse_root 2>&1)" || status=$?
  assert_eq "uid 0 returns 1" "1" "$status"
  assert_eq "the message says why" \
    "patch-checks: $GROUP_TEST skips as root; build as a Nix build user" "$output"

  status=0
  output="$(PATH="$stubs:$PATH" ID_STUB_UID=1000 refuse_root 2>&1)" || status=$?
  assert_eq "uid 1000 returns 0" "0" "$status"
  assert_eq "a build user gets no message" "" "$output"
}

# --- Test 5: a run that fails after its PASS lines fails main ---
test_a_run_failing_after_pass_fails_main() {
  echo "Test: main passes a passing run and fails one that exits non-zero after both PASS lines"

  local logs output
  logs="$(mktemp -d "$TMP_DIR/logs.XXXXXX")"
  output="$(run_main "$logs" 0)"
  assert_eq "main exits 0 when go test passes" "status=0" "${output##*$'\n'}"

  logs="$(mktemp -d "$TMP_DIR/logs.XXXXXX")"
  output="$(run_main "$logs" 1)"
  assert_eq "main exits 1 when go test fails after both PASS lines" "status=1" "${output##*$'\n'}"
  assert_contains "main says go test failed after PASS" "$output" \
    "patch-checks: go test exited non-zero after its tests passed"
}

# --- Run ---
test_manager_build_runs_the_tests_and_guards
test_top_level_pass_goes_on
test_other_logs_fail
test_refuse_root
test_a_run_failing_after_pass_fails_main

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
