#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the LiveMigrateOpts check of
# images/openstack-hypervisor-operator/patch-checks.sh, which the checkPhase
# of the manager build in manager.nix runs.
#
# Patch 0001 replaces the gophercloud servers.LiveMigrateOpts that upstream's
# Eviction sends, because its BlockMigration is a *bool and cannot carry
# "auto", and the build fails while Go code outside the tests still names the
# type. A pin move can bring another live migration written any way Go
# allows, so the check has to fail the build on a literal, a 'var', new(), a
# pointer and an aliased import, also after a '/' in a string or a division
# on the same line, and pass on the code patch 0001 leaves, on comments, on
# the LiveMigrateOptsBuilder interface, on _test.go files and on the vendored
# modules, where gophercloud defines the type. A grep error has to fail the
# build too.
#
# main runs that check and the tests of the four patches, and require_pass
# fails the build unless the log of each run holds the test's top-level PASS
# line: go test exits 0 with "no tests to run" when a test is missing. It has
# to match a top-level PASS line and nothing else, every test has to be
# selected by a run and checked, and a failed run or a build error has to
# reach require_pass, which names the test, instead of ending main. A run
# that exits non-zero after its tests printed PASS, as when a goroutine a
# test left behind panics, has to fail main as well.
# The test sources the script and runs check_live_migrate_opts against a Go
# file written for each case, require_pass against a log written for each
# case, and main with a go stub.
#
# Usage: bash tests/unit/images/hvo_live_migrate_opts_check_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
HVO_DIR="$PROJECT_ROOT/images/openstack-hypervisor-operator"

# The tests of the four patches, as main checks them.
PATCH_TESTS="TestLiveMigrateAutoBody TestHypervisorCreatedWithDefaultHighAvailability TestTraitsInSyncSetsTraitsUpdated TestServiceClientInterface"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=images/openstack-hypervisor-operator/patch-checks.sh
source "$HVO_DIR/patch-checks.sh"
# The script turns errexit on for its own run; the cases here run failing
# commands on purpose.
set +e

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# check_rc <file> <go source>
# Writes <go source> to <file> in a fresh tree, runs check_live_migrate_opts
# there and prints its status: 1 when a file names the type, which fails the
# build, 0 when none does.
check_rc() {
  local tree
  tree="$(mktemp -d "$TMP_DIR/tree.XXXXXX")"
  mkdir -p "$tree/$(dirname "$1")"
  printf '%s\n' "$2" > "$tree/$1"
  (cd "$tree" && check_live_migrate_opts . > /dev/null 2>&1)
  echo "$?"
}

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

# run_main <tree> <logs> <omit|--fail|--panic=<test>>
# Runs main in <tree> with TMPDIR=<logs>, under the errexit and pipefail the
# script runs with, and a go stub in place of go. The stub prints the
# top-level PASS line of every test its -run expression selects except
# <omit>; with --fail it prints a build error to stderr and exits 1, and with
# --panic=<test> the run that selects <test> prints a panic after the PASS
# lines and exits 1. Prints what main wrote to stderr, then
# status=<main's status>.
run_main() {
  local tree="$1" logs="$2" omit="$3"
  {
    (
      set -euo pipefail
      go() {
        local run="" name
        if [ "$omit" = --fail ]; then
          echo "internal/controller/eviction/controller.go:12:2: undefined: liveMigrateAuto" >&2
          return 1
        fi
        while [ "$#" -gt 0 ]; do
          [ "$1" != -run ] || run="$2"
          shift
        done
        run="${run#^}"
        run="${run%\$}"
        run="${run#(}"
        run="${run%)}"
        local IFS='|' panic=""
        for name in $run; do
          [ "$name" = "$omit" ] || echo "--- PASS: $name (0.01s)"
          [ "$omit" != "--panic=$name" ] || panic=1
        done
        if [ -n "$panic" ]; then
          echo "panic: send on closed channel"
          echo "FAIL"
          return 1
        fi
      }
      export TMPDIR="$logs"
      cd "$tree"
      main
    ) > /dev/null
  } 2>&1
  echo "status=$?"
}

# patch_code prints what patch 0001 leaves in the files outside the tests: the
# context and added lines of their hunks.
patch_code() {
  awk '
    /^diff --git / { code = ($NF !~ /_test\.go$/); hunk = 0; next }
    /^@@ / { hunk = 1; next }
    hunk && code && /^[ +]/ { print substr($0, 2) }
  ' "$HVO_DIR"/patches/0001-*.patch
}

# --- Test 1: the manager build runs the check ---
test_manager_build_runs_the_check() {
  echo "Test: manager.nix runs patch-checks.sh in its checkPhase, which defines the check"

  local check_phase
  check_phase="$(awk '/checkPhase = /,/^  '"''"';$/' "$HVO_DIR/manager.nix")"
  assert_contains "the checkPhase runs patch-checks.sh" "$check_phase" 'bash ${./patch-checks.sh}'
  assert_eq "patch-checks.sh defines check_live_migrate_opts" "function" \
    "$(type -t check_live_migrate_opts)"
  assert_contains "the check skips the tests" "$(declare -f check_live_migrate_opts)" \
    "--exclude='*_test.go'"
}

# --- Test 2: every way of naming the type fails the build ---
test_every_way_of_naming_the_type_fails_the_build() {
  echo "Test: a literal, a var, new(), a pointer, an aliased import and a '/' before the type fail the build"

  assert_eq "upstream's literal fails the build" "1" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	liveMigrateOpts := servers.LiveMigrateOpts{
		BlockMigration: &[]bool{false}[0],
	}')"
  assert_eq "a var declaration fails the build" "1" "$(check_rc internal/controller/maintenance/controller.go '
package maintenance

	var opts servers.LiveMigrateOpts
	opts.BlockMigration = ptr.To(false)')"
  assert_eq "new() fails the build" "1" "$(check_rc internal/controller/offboarding/controller.go '
package offboarding

	opts := new(servers.LiveMigrateOpts)')"
  assert_eq "a pointer type fails the build" "1" "$(check_rc internal/openstack/migrate.go '
package openstack

func migrateOpts() *servers.LiveMigrateOpts {')"
  assert_eq "an aliased import fails the build" "1" "$(check_rc internal/openstack/evacuate.go '
package openstack

import compute "github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	opts := compute.LiveMigrateOpts{Host: &host}')"
  assert_eq "a '/' in a string before the type fails the build" "1" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	r.log.Info("live-migrate", "path", "a/b", "opts", servers.LiveMigrateOpts{Host: &host})')"
  assert_eq "a division before the type fails the build" "1" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	opts := migrate(d/2, servers.LiveMigrateOpts{})')"
  assert_eq "an escaped quote does not end the string" "1" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	msg := fmt.Sprintf("a\" // %v", servers.LiveMigrateOpts{})')"
}

# --- Test 3: the patched code, comments, the builder and tests pass ---
test_patched_code_comments_builder_and_tests_pass() {
  echo "Test: patch 0001's code, comments, the builder interface and _test.go files pass"

  local code
  code="$(patch_code)"
  # An awk that finds no hunk would pass the check below vacuously.
  assert_contains "patch 0001 leaves its liveMigrateAuto body" "$code" "type liveMigrateAuto struct{}"
  assert_contains "patch 0001 leaves the comment that names the type" "$code" "servers.LiveMigrateOpts cannot express it"
  assert_eq "the code patch 0001 leaves passes" "0" \
    "$(check_rc internal/controller/eviction/patched.go "$code")"

  assert_eq "a trailing comment passes" "0" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	res := servers.LiveMigrate(ctx, r.computeClient, uuid, opts) // not servers.LiveMigrateOpts')"
  assert_eq "a trailing comment after a '//' in a string passes" "0" "$(check_rc internal/controller/eviction/controller.go '
package eviction

	u, err := url.Parse("http://nova") // servers.LiveMigrateOpts')"
  assert_eq "the LiveMigrateOptsBuilder interface passes" "0" "$(check_rc internal/controller/eviction/builder.go '
package eviction

var _ servers.LiveMigrateOptsBuilder = liveMigrateAuto{}')"
  assert_eq "a _test.go file passes" "0" "$(check_rc internal/controller/eviction/controller_test.go '
package eviction

	opts := servers.LiveMigrateOpts{BlockMigration: &[]bool{false}[0]}')"
  assert_eq "the vendored gophercloud passes" "0" \
    "$(check_rc vendor/github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers/requests.go '
package servers

func (opts LiveMigrateOpts) ToLiveMigrateMap() (map[string]any, error) {')"
}

# --- Test 4: a grep error fails the build ---
test_a_grep_error_fails_the_build() {
  echo "Test: a grep error fails the build with status 2"

  local output status=0
  output="$(grep() { return 3; }; check_live_migrate_opts . 2>&1)" || status=$?
  assert_eq "a failing grep returns 2" "2" "$status"
  assert_contains "the message names grep's status" "$output" "patch-checks: grep exited 3"

  # GNU grep, which the Nix build runs, exits 2 on a missing directory; BSD
  # grep exits 1 when --include is given.
  status=0
  grep -r --include='*.go' x "$TMP_DIR/missing" >/dev/null 2>&1 || status=$?
  if [ "$status" -ne 2 ]; then
    echo "  SKIP: this grep exits $status on a missing directory"
    SKIP=$((SKIP + 1))
    return
  fi
  status=0
  output="$(check_live_migrate_opts "$TMP_DIR/missing" 2>&1)" || status=$?
  assert_eq "a missing directory returns 2" "2" "$status"
}

# --- Test 5: a top-level PASS line lets the build go on, nothing else ---
test_require_pass_wants_the_top_level_pass_line() {
  echo "Test: require_pass lets a top-level PASS line through and fails every other log"

  assert_eq "a top-level PASS line goes on" "0" "$(guard_rc TestLiveMigrateAutoBody '=== RUN   TestLiveMigrateAutoBody
--- PASS: TestLiveMigrateAutoBody (0.00s)
PASS')"
  assert_eq "a subtest's PASS line alone fails the build" "1" "$(guard_rc TestTraitsInSyncSetsTraitsUpdated '=== RUN   TestTraitsInSyncSetsTraitsUpdated
=== RUN   TestTraitsInSyncSetsTraitsUpdated/no_change
    --- PASS: TestTraitsInSyncSetsTraitsUpdated/no_change (0.00s)')"
  assert_eq "a missing test fails the build" "1" "$(guard_rc TestServiceClientInterface 'testing: warning: no tests to run
PASS
ok  	github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack	0.01s [no tests to run]')"
  assert_eq "a skip fails the build" "1" "$(guard_rc TestLiveMigrateAutoBody '=== RUN   TestLiveMigrateAutoBody
--- SKIP: TestLiveMigrateAutoBody (0.00s)
PASS')"
  assert_eq "a FAIL with a passing subtest fails the build" "1" "$(guard_rc TestTraitsInSyncSetsTraitsUpdated '=== RUN   TestTraitsInSyncSetsTraitsUpdated
--- FAIL: TestTraitsInSyncSetsTraitsUpdated (0.01s)
    --- PASS: TestTraitsInSyncSetsTraitsUpdated/no_change (0.00s)
FAIL')"
  assert_eq "the other test's PASS line fails the build" "1" "$(guard_rc TestHypervisorCreatedWithDefaultHighAvailability '--- PASS: TestTraitsInSyncSetsTraitsUpdated (0.01s)
PASS')"
}

# --- Test 6: main selects and checks every patch test ---
test_main_selects_and_checks_every_patch_test() {
  echo "Test: main passes when every patch test passes and names each one that printed no PASS line"

  local tree logs output test
  tree="$(mktemp -d "$TMP_DIR/tree.XXXXXX")"

  logs="$(mktemp -d "$TMP_DIR/logs.XXXXXX")"
  output="$(run_main "$tree" "$logs" none)"
  assert_eq "main exits 0 when every selected test passes" "status=0" "${output##*$'\n'}"

  for test in $PATCH_TESTS; do
    logs="$(mktemp -d "$TMP_DIR/logs.XXXXXX")"
    output="$(run_main "$tree" "$logs" "$test")"
    assert_eq "main exits 1 without $test's PASS line" "status=1" "${output##*$'\n'}"
    assert_contains "main names $test" "$output" \
      "patch-checks: $test printed no top-level PASS line"
  done
}

# --- Test 7: main runs the LiveMigrateOpts check ---
test_main_runs_the_live_migrate_opts_check() {
  echo "Test: main fails on a tree that still names servers.LiveMigrateOpts"

  local tree logs output
  tree="$(mktemp -d "$TMP_DIR/tree.XXXXXX")"
  logs="$(mktemp -d "$TMP_DIR/logs.XXXXXX")"
  mkdir -p "$tree/internal/controller/eviction"
  printf '%s\n' 'package eviction' '' '	opts := servers.LiveMigrateOpts{}' \
    > "$tree/internal/controller/eviction/controller.go"

  output="$(run_main "$tree" "$logs" none)"
  assert_eq "main exits 1" "status=1" "${output##*$'\n'}"
  assert_contains "main names the check" "$output" \
    "a live migration still sends servers.LiveMigrateOpts; extend patch 0001"
}

# --- Test 8: a failed run reaches require_pass ---
test_a_failed_run_reaches_require_pass() {
  echo "Test: a failing go test leaves the verdict to require_pass, with the error in the log"

  local tree logs output
  tree="$(mktemp -d "$TMP_DIR/tree.XXXXXX")"
  logs="$(mktemp -d "$TMP_DIR/logs.XXXXXX")"

  output="$(run_main "$tree" "$logs" --fail)"
  assert_eq "main exits 1" "status=1" "${output##*$'\n'}"
  assert_contains "require_pass names the first test" "$output" \
    "patch-checks: TestLiveMigrateAutoBody printed no top-level PASS line"
  assert_file_contains_fixed "the build error reaches the log" "$logs/patch-0001.log" \
    "undefined: liveMigrateAuto"
}

# --- Test 9: a run that fails after its PASS lines fails main ---
test_a_run_failing_after_pass_fails_main() {
  echo "Test: a go test that exits non-zero after its tests printed PASS fails main"

  local tree logs output test
  tree="$(mktemp -d "$TMP_DIR/tree.XXXXXX")"

  for test in $PATCH_TESTS; do
    logs="$(mktemp -d "$TMP_DIR/logs.XXXXXX")"
    output="$(run_main "$tree" "$logs" "--panic=$test")"
    assert_eq "main exits 1 when the run of $test fails after PASS" "status=1" "${output##*$'\n'}"
    assert_contains "main says a run failed after PASS for $test" "$output" \
      "patch-checks: a go test run exited non-zero after its tests passed"
  done
}

# --- Run ---
test_manager_build_runs_the_check
test_every_way_of_naming_the_type_fails_the_build
test_patched_code_comments_builder_and_tests_pass
test_a_grep_error_fails_the_build
test_require_pass_wants_the_top_level_pass_line
test_main_selects_and_checks_every_patch_test
test_main_runs_the_live_migrate_opts_check
test_a_failed_run_reaches_require_pass
test_a_run_failing_after_pass_fails_main

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
