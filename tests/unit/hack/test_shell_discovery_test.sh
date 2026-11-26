#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `make test-shell` recipe and hack/test-shell.sh behind it: it
# finds every *_test.sh under tests/unit/ by discovery, symlinks included, and
# runs each as one bats test in LC_ALL=C sort order of their paths, a failing
# test does not stop the run, a .sh file under tests/unit/ whose name does not
# end in _test.sh fails the target before any test runs, and an empty tree, a
# symlink whose target is missing, or a directory find cannot read fails the
# target instead of passing with tests unrun. The chaos preflight test went
# unrun in CI for both reasons at once: it sat in a directory the old fixed
# list of globs did not name, and its test_*.sh name matched no *_test.sh
# glob.
#
# It also verifies what the bats runner adds: the output of a red script and
# not of a green one, the list of failed scripts at the end of a red run, the
# JUnit report and the directory it lands in, the same with parallel workers,
# TEST_SHELL_FILTER, the checks of TEST_SHELL_JOBS and of the
# tools parallel workers need, and the messages for a missing bats and for a
# bats that fails without a failed test.
#
# The Makefile, hack/test-shell.sh and tests/unit/legacy_scripts.bats are
# copied into a scratch tree beside stub tests that append their own path to
# $RUN_LOG, so no repository test runs. The scratch tree runs the bats the
# outer run exported as BATS, or bin/bats, through a bin/ of its own whose
# stamp matches the BATS_COMMIT the command line passes, so its install-bats
# never fetches and never touches the repository's bin/.
#
# Usage: bash tests/unit/hack/test_shell_discovery_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MAKEFILE="$PROJECT_ROOT/Makefile"

# The bats of the outer run: make test-shell exports BATS, and a direct run
# takes the one make install-bats links.
BATS_BIN="${BATS:-$PROJECT_ROOT/bin/bats}"
# Any 40-character ID does: the scratch install-bats compares it with the
# stamp make_tree writes.
SCRATCH_COMMIT=0000000000000000000000000000000000000000

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_tree
# Creates a scratch tree in $TREE holding the real Makefile, hack/test-shell.sh,
# tests/unit/legacy_scripts.bats and an empty tests/unit/newdir/, a directory
# name no glob of the old fixed list named, so a test found there proves
# discovery rather than the list. Its bin/ links bats to $BATS_BIN beside the
# stamp of $SCRATCH_COMMIT.
make_tree() {
  # The file runs without -e, so an unchecked failure would continue with an
  # empty TREE and copy the Makefile to /Makefile.
  TREE="$(mktemp -d)" || {
    echo "mktemp -d could not create a scratch tree" >&2
    exit 1
  }
  mkdir -p "$TREE/tests/unit/newdir" "$TREE/hack" "$TREE/bin/bats-core"
  cp "$MAKEFILE" "$TREE/Makefile"
  cp -p "$PROJECT_ROOT/hack/test-shell.sh" "$TREE/hack/test-shell.sh"
  cp "$PROJECT_ROOT/tests/unit/legacy_scripts.bats" "$TREE/tests/unit/legacy_scripts.bats"
  ln -s "$BATS_BIN" "$TREE/bin/bats"
  echo "$SCRATCH_COMMIT" > "$TREE/bin/bats-core/.bats-commit"
}

# add_stub <path under the tree> <exit-code> [<line>]
# Writes a stub test that appends its own path to $RUN_LOG, prints <line>
# when given, and exits with <exit-code>. The runner starts it with `bash`,
# so it needs no shebang and no execute bit.
add_stub() {
  local path="$1" code="$2" line="${3:-}"
  mkdir -p "$TREE/$(dirname "$path")"
  {
    cat <<'STUB'
echo "$0" >> "$RUN_LOG"
STUB
    [ -z "$line" ] || echo "echo '$line'"
    echo "exit $code"
  } > "$TREE/$path"
}

# run_target [VAR=value ...]
# Runs `make test-shell` in the scratch tree, writes its stdout and stderr to
# $TREE/stdout and $TREE/stderr, and echoes make's exit code. The variables
# go on the command line, where they win over the environment and over the
# MAKEFLAGS of the outer run, so a value set for that run does not reach
# this one; a later VAR=value argument overrides an earlier one.
run_target() {
  local exit_code=0
  : > "$TREE/run.log"
  RUN_LOG="$TREE/run.log" make -C "$TREE" test-shell \
    LOCALBIN="$TREE/bin" BATS="$TREE/bin/bats" BATS_COMMIT="$SCRATCH_COMMIT" \
    TEST_SHELL_JOBS=1 TEST_SHELL_FILTER= TEST_SHELL_REPORT_DIR="$TREE/report" \
    "$@" >"$TREE/stdout" 2>"$TREE/stderr" || exit_code=$?
  echo "$exit_code"
}

# run_script <bats>
# Runs hack/test-shell.sh of the scratch tree directly with BATS=<bats>,
# without make, whose install-bats would install a bats in place of a bogus
# one. Writes stdout and stderr like run_target and echoes the exit code.
run_script() {
  local exit_code=0
  : > "$TREE/run.log"
  RUN_LOG="$TREE/run.log" BATS="$1" TEST_SHELL_JOBS=1 TEST_SHELL_FILTER='' \
    TEST_SHELL_REPORT_DIR="$TREE/report" "$TREE/hack/test-shell.sh" \
    >"$TREE/stdout" 2>"$TREE/stderr" || exit_code=$?
  echo "$exit_code"
}

# path_without <tool>
# Echoes $PATH with every directory that holds <tool> replaced by
# $TREE/path, which links every other entry of those directories, so <tool>
# is not found and every other command still is.
path_without() {
  local tool="$1" dir="$TREE/path" d path=""
  local IFS=:
  mkdir -p "$dir"
  for d in $PATH; do
    if [ -n "$d" ] && [ -e "$d/$tool" ]; then
      # A name in two such directories keeps the link of the first.
      ln -s "$d"/* "$dir"/ 2>/dev/null
      d="$dir"
    fi
    path="${path:+$path:}$d"
  done
  rm -f "$dir/$tool"
  echo "$path"
}

# assert_tap_line <description> <file> <line>
# PASS when <file> holds the TAP line <line>, alone or followed by the
# " # in <n> ms" bats appends. A substring match would take "not ok 1 x"
# for "ok 1 x".
assert_tap_line() {
  local description="$1" file="$2" line="$3"
  if awk -v l="$line" '$0 == l || index($0, l " # ") == 1 { found = 1 } END { exit !found }' "$file"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected the line: $line"
    sed 's/^/    /' "$file"
    FAIL=$((FAIL + 1))
  fi
}

# assert_no_plan_line <description> <file>
# PASS when <file> holds no TAP plan line, so bats never started counting.
assert_no_plan_line() {
  local description="$1" file="$2"
  if grep -q '^1\.\.' "$file"; then
    echo "  FAIL: $description"
    sed 's/^/    /' "$file"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  fi
}

# run_count
# Echoes how many stub tests ran in the last run_target.
run_count() {
  wc -l < "$TREE/run.log" | tr -d ' '
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_runs_a_test_in_a_directory_the_old_list_never_named() {
  echo "Test: test-shell runs a test in a directory the old list never named"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  code="$(run_target)"

  assert_eq "the target exits 0" "0" "$code"
  assert_tap_line "stdout reports the test as ok" \
    "$TREE/stdout" "ok 1 tests/unit/newdir/alpha_test.sh"
  assert_eq "the test ran once" "1" "$(run_count)"
  rm -rf "$TREE"
}

test_runs_every_test_in_sorted_order() {
  echo "Test: test-shell runs every test in LC_ALL=C sort order of its path"
  local code lc_all=C
  make_tree
  # The six newdir/ tests are created neither in sorted order nor in its
  # reverse, so a find listing in creation order or its reverse cannot pass
  # unsorted, and one in directory-hash order (ext4) rarely does.
  # Zeta_test.sh sorts first only under C collation.
  add_stub tests/unit/newdir/delta_test.sh 0
  add_stub tests/unit/newdir/Zeta_test.sh 0
  add_stub tests/unit/newdir/gamma_test.sh 0
  add_stub tests/unit/newdir/alpha_test.sh 0
  add_stub tests/unit/newdir/epsilon_test.sh 0
  add_stub tests/unit/newdir/beta_test.sh 0
  add_stub tests/unit/another/alpha_test.sh 0
  # make runs under a locale that sorts Zeta_test.sh last, so a recipe that
  # sorts without LC_ALL=C fails here.
  if [ "$(printf 'a\nZ\n' | LC_ALL=en_US.UTF-8 sort | head -n 1)" = a ]; then
    lc_all=en_US.UTF-8
  else
    echo "  SKIP: no en_US.UTF-8 collation, so the LC_ALL=C pin goes unchecked (1 check skipped)"
    SKIP=$((SKIP + 1))
  fi
  code="$(LC_ALL="$lc_all" run_target)"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "the tests ran in LC_ALL=C sorted path order" \
    "$(printf 'tests/unit/%s\n' another/alpha_test.sh newdir/Zeta_test.sh \
      newdir/alpha_test.sh newdir/beta_test.sh newdir/delta_test.sh \
      newdir/epsilon_test.sh newdir/gamma_test.sh)" \
    "$(cat "$TREE/run.log")"
  rm -rf "$TREE"
}

test_a_symlink_counts_like_the_file_it_points_to() {
  echo "Test: test-shell treats a symlink under tests/unit like the file it points to"
  local code
  make_tree
  # The targets sit outside tests/unit, so only the symlinks can be found.
  add_stub hack/shared_test.sh 0
  add_stub hack/helper.sh 0
  ln -s ../../../hack/shared_test.sh "$TREE/tests/unit/newdir/shared_test.sh"
  code="$(run_target)"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "the symlinked test ran under its tests/unit path" \
    "tests/unit/newdir/shared_test.sh" "$(cat "$TREE/run.log")"

  ln -s ../../../hack/helper.sh "$TREE/tests/unit/newdir/helper.sh"
  code="$(run_target)"

  assert_nonzero_exit "a symlinked misnamed script fails the target" "$code"
  assert_contains "stderr names the symlinked misnamed script" \
    "$(cat "$TREE/stderr")" \
    "test-shell: tests/unit/newdir/helper.sh does not end in _test.sh"
  rm -rf "$TREE"
}

test_a_directory_symlink_is_walked_like_the_directory_it_points_to() {
  echo "Test: test-shell walks a directory symlink under tests/unit like the directory it points to"
  local code
  make_tree
  # The target directory sits outside tests/unit, so only the walk through
  # the symlink can find its tests. A second test keeps the empty-discovery
  # guard from catching a find that does not follow the link.
  add_stub tests/unit/newdir/alpha_test.sh 0
  add_stub hack/linked/inner_test.sh 0
  ln -s ../../hack/linked "$TREE/tests/unit/linked"
  code="$(run_target)"

  assert_eq "the target exits 0" "0" "$code"
  assert_contains "the test behind the directory symlink ran under its tests/unit path" \
    "$(cat "$TREE/run.log")" "tests/unit/linked/inner_test.sh"

  add_stub hack/linked/helper.sh 0
  code="$(run_target)"

  assert_nonzero_exit "a misnamed script behind the directory symlink fails the target" "$code"
  assert_contains "stderr names the misnamed script behind the directory symlink" \
    "$(cat "$TREE/stderr")" \
    "test-shell: tests/unit/linked/helper.sh does not end in _test.sh"
  rm -rf "$TREE"
}

test_a_dangling_symlink_fails_the_target() {
  echo "Test: a symlink under tests/unit whose target is missing fails test-shell instead of dropping what it pointed to"
  local code
  make_tree
  # find -L lists a dangling link as a plain entry and exits 0, and the
  # link's name matches neither *.sh nor *_test.sh, so only a check for
  # the dangling link itself can catch it.
  add_stub tests/unit/newdir/alpha_test.sh 0
  ln -s ../../hack/gone "$TREE/tests/unit/gone"
  code="$(run_target)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr names the dangling symlink" \
    "$(cat "$TREE/stderr")" \
    "test-shell: tests/unit/gone is a symlink whose target does not exist"
  assert_eq "no test ran" "0" "$(run_count)"
  rm -rf "$TREE"
}

test_a_failing_test_does_not_stop_the_loop() {
  echo "Test: a failing test does not stop test-shell from running the rest"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 1
  add_stub tests/unit/newdir/beta_test.sh 0
  code="$(run_target)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_eq "both tests ran" "2" "$(run_count)"
  assert_tap_line "stdout reports the failing test as not ok" \
    "$TREE/stdout" "not ok 1 tests/unit/newdir/alpha_test.sh"
  assert_tap_line "stdout reports the test after it as ok" \
    "$TREE/stdout" "ok 2 tests/unit/newdir/beta_test.sh"
  rm -rf "$TREE"
}

test_a_misnamed_script_fails_the_target_before_any_test_runs() {
  echo "Test: a .sh under tests/unit that does not end in _test.sh fails test-shell before any test runs"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  add_stub tests/unit/newdir/test_stray.sh 0
  code="$(run_target)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr names the misnamed script" \
    "$(cat "$TREE/stderr")" \
    "test-shell: tests/unit/newdir/test_stray.sh does not end in _test.sh"
  assert_eq "no test ran" "0" "$(run_count)"
  assert_no_plan_line "stdout names no test" "$TREE/stdout"
  rm -rf "$TREE"
}

test_an_empty_tree_fails_the_target() {
  echo "Test: an empty tests/unit tree fails test-shell instead of passing with nothing run"
  local code
  make_tree
  code="$(run_target)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr says no test was found" \
    "$(cat "$TREE/stderr")" "test-shell: no *_test.sh found under tests/unit"
  assert_no_plan_line "stdout names no test" "$TREE/stdout"
  rm -rf "$TREE"
}

test_an_unreadable_directory_fails_the_target() {
  echo "Test: a directory under tests/unit that find cannot read fails test-shell instead of skipping its tests"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  add_stub tests/unit/locked/beta_test.sh 0
  chmod 000 "$TREE/tests/unit/locked"
  if [ -r "$TREE/tests/unit/locked" ]; then
    echo "  SKIP: a mode-000 directory stays readable to this user (3 checks skipped)"
    SKIP=$((SKIP + 3))
    chmod 755 "$TREE/tests/unit/locked"
    rm -rf "$TREE"
    return
  fi
  code="$(run_target)"
  chmod 755 "$TREE/tests/unit/locked"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr says find could not walk the tree" \
    "$(cat "$TREE/stderr")" "test-shell: find could not walk tests/unit"
  assert_eq "no test ran" "0" "$(run_count)"
  rm -rf "$TREE"
}

test_a_red_test_shows_its_output_and_a_green_one_does_not() {
  echo "Test: test-shell prints the output of a failing test under its not ok line and hides a passing test's"
  local code block
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 1 "alpha-output"
  add_stub tests/unit/newdir/beta_test.sh 0 "beta-output"
  code="$(run_target)"
  block="$(sed -n '/^not ok 1 /,/^ok 2 /p' "$TREE/stdout")"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_eq "both tests ran" "2" "$(run_count)"
  assert_contains "the failing test's output follows its not ok line" \
    "$block" "# alpha-output"
  assert_not_contains "stdout holds no output of the passing test" \
    "$(cat "$TREE/stdout")" "beta-output"
  rm -rf "$TREE"
}

test_a_red_run_ends_with_the_failed_tests() {
  echo "Test: a red test-shell run ends with the list of failed tests on stderr"
  local code stderr
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 1
  add_stub tests/unit/newdir/beta_test.sh 0
  code="$(run_target)"
  stderr="$(cat "$TREE/stderr")"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr counts the failed tests" \
    "$stderr" "test-shell: 1 of 2 scripts failed:"
  assert_contains "stderr names the failed test without its timing" \
    "$stderr" "test-shell:   tests/unit/newdir/alpha_test.sh"$'\n'
  assert_not_contains "stderr does not name the passing test" \
    "$stderr" "beta_test.sh"
  rm -rf "$TREE"
}

test_the_filter_selects_by_path() {
  echo "Test: TEST_SHELL_FILTER runs the tests whose path matches it"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  add_stub tests/unit/newdir/beta_test.sh 0
  code="$(run_target TEST_SHELL_FILTER=beta)"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "only the matching test ran" \
    "tests/unit/newdir/beta_test.sh" "$(cat "$TREE/run.log")"
  assert_tap_line "stdout plans one test" "$TREE/stdout" "1..1"
  assert_tap_line "stdout reports the matching test as ok" \
    "$TREE/stdout" "ok 1 tests/unit/newdir/beta_test.sh"
  rm -rf "$TREE"
}

test_a_filter_that_matches_nothing_fails() {
  echo "Test: a TEST_SHELL_FILTER that matches no test fails test-shell"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  code="$(run_target TEST_SHELL_FILTER=matches-nothing)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr says the filter matches nothing" \
    "$(cat "$TREE/stderr")" \
    "test-shell: TEST_SHELL_FILTER 'matches-nothing' matches no script"
  assert_eq "no test ran" "0" "$(run_count)"
  rm -rf "$TREE"
}

test_the_junit_report_names_every_test() {
  echo "Test: test-shell writes a JUnit report with a testcase per test and a failure per failed one"
  local code report
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 1
  add_stub tests/unit/newdir/beta_test.sh 0
  code="$(run_target)"
  report="$TREE/report/report.xml"

  assert_nonzero_exit "the target exits non-zero" "$code"
  if [ ! -f "$report" ]; then
    echo "  FAIL: $report was not written (4 checks failed)"
    FAIL=$((FAIL + 4))
    rm -rf "$TREE"
    return
  fi
  assert_eq "the report holds a testcase per test" \
    "2" "$(grep -c '<testcase' "$report")"
  assert_file_contains_fixed "the failed test's testcase is named by its path" \
    "$report" 'name="tests/unit/newdir/alpha_test.sh"'
  assert_file_contains_fixed "the passing test's testcase is named by its path" \
    "$report" 'name="tests/unit/newdir/beta_test.sh"'
  assert_eq "the report holds one failure" \
    "1" "$(grep -c '<failure' "$report")"
  rm -rf "$TREE"
}

test_jobs_must_be_a_positive_integer() {
  echo "Test: a TEST_SHELL_JOBS that is not a positive integer fails test-shell before any test runs"
  local code value
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  for value in abc 0; do
    code="$(run_target TEST_SHELL_JOBS="$value")"
    assert_nonzero_exit "TEST_SHELL_JOBS=$value fails the target" "$code"
    assert_contains "stderr says TEST_SHELL_JOBS=$value is no positive integer" \
      "$(cat "$TREE/stderr")" \
      "test-shell: TEST_SHELL_JOBS must be a positive integer, got '$value'"
    assert_eq "no test ran with TEST_SHELL_JOBS=$value" "0" "$(run_count)"
  done
  rm -rf "$TREE"
}

test_workers_without_parallel_fail() {
  echo "Test: TEST_SHELL_JOBS above 1 without GNU parallel fails test-shell before any test runs"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  code="$(PATH="$(path_without parallel)" run_target TEST_SHELL_JOBS=2)"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr names the missing tool" \
    "$(cat "$TREE/stderr")" \
    "test-shell: TEST_SHELL_JOBS=2 needs parallel, which is not installed"
  assert_eq "no test ran" "0" "$(run_count)"
  rm -rf "$TREE"
}

test_unset_jobs_without_parallel_run_serially() {
  echo "Test: an unset TEST_SHELL_JOBS without GNU parallel runs every test serially with a notice"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  add_stub tests/unit/newdir/beta_test.sh 0
  code="$(PATH="$(path_without parallel)" run_target TEST_SHELL_JOBS=)"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "every test ran" "2" "$(run_count)"
  assert_contains "stderr says the run is serial and why" \
    "$(cat "$TREE/stderr")" "test-shell: running serially, parallel is not installed"
  rm -rf "$TREE"
}

test_parallel_workers_run_every_test_once() {
  echo "Test: TEST_SHELL_JOBS=2 with GNU parallel runs every test exactly once"
  local code
  if ! command -v parallel >/dev/null 2>&1 ||
    { ! command -v flock >/dev/null 2>&1 && ! command -v shlock >/dev/null 2>&1; }; then
    echo "  SKIP: GNU parallel or flock/shlock is not installed, so two workers cannot run (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  add_stub tests/unit/newdir/beta_test.sh 0
  add_stub tests/unit/newdir/gamma_test.sh 0
  add_stub tests/unit/newdir/delta_test.sh 0
  code="$(run_target TEST_SHELL_JOBS=2)"

  assert_eq "the target exits 0" "0" "$code"
  assert_eq "four tests ran" "4" "$(run_count)"
  assert_eq "no test ran twice" "4" "$(sort -u "$TREE/run.log" | wc -l | tr -d ' ')"
  rm -rf "$TREE"
}

test_parallel_workers_report_a_red_test() {
  echo "Test: TEST_SHELL_JOBS=2 shows a red test's output, lists it at the end and records its failure in the report"
  local code stdout stderr report
  if ! command -v parallel >/dev/null 2>&1 ||
    { ! command -v flock >/dev/null 2>&1 && ! command -v shlock >/dev/null 2>&1; }; then
    echo "  SKIP: GNU parallel or flock/shlock is not installed, so two workers cannot run (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 1 "alpha-output"
  add_stub tests/unit/newdir/beta_test.sh 0 "beta-output"
  add_stub tests/unit/newdir/gamma_test.sh 0
  add_stub tests/unit/newdir/delta_test.sh 0
  code="$(run_target TEST_SHELL_JOBS=2)"
  stdout="$(cat "$TREE/stdout")"
  stderr="$(cat "$TREE/stderr")"
  report="$TREE/report/report.xml"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_tap_line "stdout reports the red test as not ok" \
    "$TREE/stdout" "not ok 1 tests/unit/newdir/alpha_test.sh"
  assert_contains "the red test's output follows its not ok line" \
    "$(sed -n '/^not ok 1 /,/^ok 2 /p' "$TREE/stdout")" "# alpha-output"
  assert_not_contains "stdout holds no output of a passing test" \
    "$stdout" "beta-output"
  assert_contains "stderr counts the failed tests" \
    "$stderr" "test-shell: 1 of 4 scripts failed:"
  assert_contains "stderr names the failed test without its timing" \
    "$stderr" "test-shell:   tests/unit/newdir/alpha_test.sh"$'\n'
  assert_eq "the report holds a testcase per test" \
    "4" "$(grep -c '<testcase' "$report" 2>/dev/null)"
  assert_eq "the report holds one failure" \
    "1" "$(grep -c '<failure' "$report" 2>/dev/null)"
  rm -rf "$TREE"
}

test_a_missing_bats_fails() {
  echo "Test: a BATS that names no file fails test-shell with a pointer to make install-bats"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  code="$(run_script "$TREE/nonexistent")"

  assert_eq "the runner exits 1" "1" "$code"
  assert_contains "stderr names the bats and the target that installs it" \
    "$(cat "$TREE/stderr")" \
    "test-shell: $TREE/nonexistent is not an executable bats; run make install-bats"
  assert_eq "no test ran" "0" "$(run_count)"
  rm -rf "$TREE"
}

test_a_bats_failing_without_a_failed_test() {
  echo "Test: a bats that fails without a failed test ends test-shell with its exit code"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  printf '#!/bin/sh\nexit 3\n' > "$TREE/broken-bats"
  chmod +x "$TREE/broken-bats"
  code="$(run_script "$TREE/broken-bats")"

  assert_eq "the runner exits with the code of bats" "3" "$code"
  assert_contains "stderr says bats failed without a failed test" \
    "$(cat "$TREE/stderr")" "test-shell: bats exited 3 without a failed script"
  rm -rf "$TREE"
}

test_a_report_dir_that_cannot_be_created_fails() {
  echo "Test: a TEST_SHELL_REPORT_DIR that cannot be created fails test-shell before any test runs"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  # A regular file as parent makes mkdir -p fail for any user, root included.
  : > "$TREE/blocker"
  code="$(run_target TEST_SHELL_REPORT_DIR="$TREE/blocker/report")"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_contains "stderr names the directory" \
    "$(cat "$TREE/stderr")" "test-shell: could not create $TREE/blocker/report"
  assert_eq "no test ran" "0" "$(run_count)"
  rm -rf "$TREE"
}

test_the_report_dir_resolves_against_the_root() {
  echo "Test: test-shell writes report.xml to _output/test-shell under the root by default and resolves a relative TEST_SHELL_REPORT_DIR against the root"
  local code
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 0
  code="$(run_target TEST_SHELL_REPORT_DIR=)"

  assert_eq "the target exits 0 without TEST_SHELL_REPORT_DIR" "0" "$code"
  assert_eq "the report lands in _output/test-shell under the root" "yes" \
    "$([ -f "$TREE/_output/test-shell/report.xml" ] && echo yes || echo no)"
  # upload-artifact only warns on a missing file, so a drift between the two
  # paths would leave the job green without its report.
  assert_file_contains_fixed "the test-shell job uploads that path" \
    "$PROJECT_ROOT/.github/workflows/ci.yaml" "path: _output/test-shell/report.xml"

  # make starts the script from the root, so a run from a subdirectory is
  # what shows the relative value is not resolved against the caller's.
  mkdir -p "$TREE/sub"
  code=0
  (cd "$TREE/sub" && RUN_LOG="$TREE/run.log" BATS="$TREE/bin/bats" \
    TEST_SHELL_JOBS=1 TEST_SHELL_FILTER='' TEST_SHELL_REPORT_DIR=rel/report \
    "$TREE/hack/test-shell.sh") >"$TREE/stdout" 2>"$TREE/stderr" || code=$?

  assert_eq "the runner exits 0 with a relative TEST_SHELL_REPORT_DIR" "0" "$code"
  assert_eq "the relative report dir resolves against the root" "yes" \
    "$([ -f "$TREE/rel/report/report.xml" ] && echo yes || echo no)"
  rm -rf "$TREE"
}

if [ ! -f "$BATS_BIN" ] || [ ! -x "$BATS_BIN" ]; then
  echo "  SKIP: $BATS_BIN is not an executable bats; run make install-bats (1 check skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_runs_a_test_in_a_directory_the_old_list_never_named
test_runs_every_test_in_sorted_order
test_a_symlink_counts_like_the_file_it_points_to
test_a_directory_symlink_is_walked_like_the_directory_it_points_to
test_a_dangling_symlink_fails_the_target
test_a_failing_test_does_not_stop_the_loop
test_a_misnamed_script_fails_the_target_before_any_test_runs
test_an_empty_tree_fails_the_target
test_an_unreadable_directory_fails_the_target
test_a_red_test_shows_its_output_and_a_green_one_does_not
test_a_red_run_ends_with_the_failed_tests
test_the_filter_selects_by_path
test_a_filter_that_matches_nothing_fails
test_the_junit_report_names_every_test
test_jobs_must_be_a_positive_integer
test_workers_without_parallel_fail
test_unset_jobs_without_parallel_run_serially
test_parallel_workers_run_every_test_once
test_parallel_workers_report_a_red_test
test_a_missing_bats_fails
test_a_bats_failing_without_a_failed_test
test_a_report_dir_that_cannot_be_created_fails
test_the_report_dir_resolves_against_the_root

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
