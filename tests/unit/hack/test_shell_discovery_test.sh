#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `make test-shell` recipe: it finds every *_test.sh under
# tests/unit/ by discovery, symlinks included, and runs them in LC_ALL=C sort
# order of their paths, a failing test does not stop the loop, a .sh file under
# tests/unit/ whose name does not end in _test.sh fails the target before any
# test runs, and an empty tree, a symlink whose target is missing, or a
# directory find cannot read fails the target instead of passing with tests
# unrun. The chaos preflight test went unrun in
# CI for both reasons at once: it sat in a directory the old fixed list of
# globs did not name, and its test_*.sh name matched no *_test.sh glob.
#
# The Makefile is copied into a scratch tree beside stub tests that append
# their own path to $RUN_LOG, so no repository test runs.
#
# Usage: bash tests/unit/hack/test_shell_discovery_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MAKEFILE="$PROJECT_ROOT/Makefile"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_tree
# Creates a scratch tree in $TREE holding the real Makefile and an empty
# tests/unit/newdir/, a directory name no glob of the old fixed list named, so
# a test found there proves discovery rather than the list.
make_tree() {
  # The file runs without -e, so an unchecked failure would continue with an
  # empty TREE and copy the Makefile to /Makefile.
  TREE="$(mktemp -d)" || {
    echo "mktemp -d could not create a scratch tree" >&2
    exit 1
  }
  mkdir -p "$TREE/tests/unit/newdir"
  cp "$MAKEFILE" "$TREE/Makefile"
}

# add_stub <path under the tree> <exit-code>
# Writes a stub test that appends its own path to $RUN_LOG and exits with
# <exit-code>. The recipe runs it with `bash`, so it needs no shebang and no
# execute bit.
add_stub() {
  local path="$1" code="$2"
  mkdir -p "$TREE/$(dirname "$path")"
  cat > "$TREE/$path" <<STUB
echo "\$0" >> "\$RUN_LOG"
exit $code
STUB
}

# run_target
# Runs `make test-shell` in the scratch tree, writes its stdout and stderr to
# $TREE/stdout and $TREE/stderr, and echoes make's exit code.
run_target() {
  local exit_code=0
  : > "$TREE/run.log"
  RUN_LOG="$TREE/run.log" make -C "$TREE" test-shell \
    >"$TREE/stdout" 2>"$TREE/stderr" || exit_code=$?
  echo "$exit_code"
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
  assert_contains "stdout names the test before it runs" \
    "$(cat "$TREE/stdout")" "=== tests/unit/newdir/alpha_test.sh ==="
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
  local code stdout
  make_tree
  add_stub tests/unit/newdir/alpha_test.sh 1
  add_stub tests/unit/newdir/beta_test.sh 0
  code="$(run_target)"
  stdout="$(cat "$TREE/stdout")"

  assert_nonzero_exit "the target exits non-zero" "$code"
  assert_eq "both tests ran" "2" "$(run_count)"
  assert_contains "stdout names the failing test" \
    "$stdout" "=== tests/unit/newdir/alpha_test.sh ==="
  assert_contains "stdout names the test after it" \
    "$stdout" "=== tests/unit/newdir/beta_test.sh ==="
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
  assert_not_contains "stdout names no test" "$(cat "$TREE/stdout")" "=== "
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
  assert_not_contains "stdout names no test" "$(cat "$TREE/stdout")" "=== "
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

test_runs_a_test_in_a_directory_the_old_list_never_named
test_runs_every_test_in_sorted_order
test_a_symlink_counts_like_the_file_it_points_to
test_a_directory_symlink_is_walked_like_the_directory_it_points_to
test_a_dangling_symlink_fails_the_target
test_a_failing_test_does_not_stop_the_loop
test_a_misnamed_script_fails_the_target_before_any_test_runs
test_an_empty_tree_fails_the_target
test_an_unreadable_directory_fails_the_target

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
