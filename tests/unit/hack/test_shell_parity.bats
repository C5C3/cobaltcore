#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/test-shell-parity.sh, which compares the checks of a native
# bats file with those of the old script it replaces: equal lists print ok
# and exit 0, a dropped check prints its "- " line and an added one its
# "+ " line and both exit 1, an old script that fails and a native file
# that fails are reported and exit 1, also with every check kept, skips are
# notes, a path relative to the working directory or absolute pairs the same
# file, an unknown ref and a tree without a pair end with their messages,
# and no worktree is left behind.
#
# Each test builds a scratch git repository with the tool, the old and the
# new helpers, and one old script committed as the base; its native file
# sits in the work tree. The native file runs on the bats of this run with
# the helper libraries of TEST_SHELL_BATS_LIBS, or bin/ of this repository.
#
# Usage: bin/bats tests/unit/hack/test_shell_parity.bats

load '../../lib/bats/common'

setup() {
  require_tool git
  REPO="$BATS_TEST_TMPDIR/repo"
  mkdir -p "$REPO/hack" "$REPO/tests/unit/x"
  cp "$PROJECT_ROOT/hack/test-shell-parity.sh" "$REPO/hack/"
  # The old and the new helpers.
  cp -R "$PROJECT_ROOT/tests/lib" "$REPO/tests/"
  git -C "$REPO" init -q
  write_old 0
  git -C "$REPO" add -A
  git -C "$REPO" -c user.name=test -c user.email=test@example.invalid \
    -c commit.gpgsign=false commit -q -m base
  git -C "$REPO" tag base
}

# write_old <failing checks>
# Writes the old script tests/unit/x/sample_test.sh with the checks "first
# check" and "second check", the given number of failing checks, and one
# skipped check. The counters and the assertion file are printed piecewise,
# so tests/unit/lib/native_form.bats finds no old form in this file.
write_old() {
  {
    # shellcheck disable=SC2016
    printf 'PROJECT_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"\n'
    printf '%s=0\n' PASS FAIL
    printf '%s=1\n' SKIP
    # shellcheck disable=SC2016
    printf 'source "$PROJECT_ROOT/tests/lib/%s"\n' assertions.sh
    cat <<EOF
assert_eq "first check" a a
assert_eq "second check" b b
for i in \$(seq 1 $1); do assert_eq "broken check \$i" a b; done
echo "  SKIP: a tool is missing"
echo "Results: \$PASS passed, \$FAIL failed, \$SKIP skipped"
[ "\$FAIL" -eq 0 ]
EOF
  } >"$REPO/tests/unit/x/sample_test.sh"
}

# write_native <line>...
# Writes the native file tests/unit/x/sample.bats with one test whose body
# is the given lines.
write_native() {
  {
    printf '%s\n' "load '../../lib/bats/common'" '@test "sample" {'
    printf '  %s\n' "$@"
    printf '}\n'
  } >"$REPO/tests/unit/x/sample.bats"
}

# parity <argument>...
# Runs the tool of the scratch repository with the bats of this run.
parity() {
  run env BATS="$BATS_ROOT/bin/bats" \
    TEST_SHELL_BATS_LIBS="${TEST_SHELL_BATS_LIBS:-$PROJECT_ROOT/bin}" \
    TEST_SHELL_JOBS=1 "$REPO/hack/test-shell-parity.sh" "$@"
}

# assert_no_worktree_left
assert_no_worktree_left() {
  assert_equal "$(git -C "$REPO" worktree list | wc -l | tr -d ' ')" 1
}

@test "equal checks print ok and exit 0" {
  write_native 'assert_eq "first check" a a' 'assert_eq "second check" b b'

  parity base tests/unit/x
  assert_success
  assert_line 'ok tests/unit/x/sample.bats: 2 checks'
  assert_line 'note: tests/unit/x/sample_test.sh skipped 1 checks at base'
  assert_equal "${lines[${#lines[@]} - 1]}" 'test-shell-parity: 1 files, 2 checks, 0 mismatched'
  assert_no_worktree_left
}

@test "a dropped check prints its - line and exits 1" {
  write_native 'assert_eq "first check" a a'

  parity base tests/unit/x
  assert_failure 1
  assert_line 'MISMATCH tests/unit/x/sample.bats: 2 checks at base, 1 now'
  assert_line '- second check'
  assert_line 'test-shell-parity: 1 files, 2 checks, 1 mismatched'
  assert_no_worktree_left
}

@test "an added check prints its + line and exits 1" {
  write_native 'assert_eq "first check" a a' 'assert_eq "second check" b b' \
    'check_passed "third check"'

  parity base tests/unit/x
  assert_failure 1
  assert_line 'MISMATCH tests/unit/x/sample.bats: 2 checks at base, 3 now'
  assert_line '+ third check'
  refute_line --regexp '^- '
  assert_no_worktree_left
}

@test "an old script that fails is reported with its FAIL lines" {
  write_old 1
  git -C "$REPO" -c user.name=test -c user.email=test@example.invalid \
    -c commit.gpgsign=false commit -q -am "break"
  write_native 'assert_eq "first check" a a' 'assert_eq "second check" b b'

  parity HEAD tests/unit/x
  assert_failure 1
  assert_line 'FAILED tests/unit/x/sample_test.sh at HEAD'
  assert_line '  FAIL: broken check 1'
  assert_line 'ok tests/unit/x/sample.bats: 2 checks'
  assert_no_worktree_left
}

@test "a native file that fails is reported with its not ok lines" {
  write_native 'assert_eq "first check" a a' 'assert_eq "second check" b c'

  parity base tests/unit/x
  assert_failure 1
  assert_line 'FAILED: bats exited 1'
  assert_line 'not ok 1 sample'
  assert_line '- second check'
  assert_no_worktree_left
}

@test "a native file that fails with every check kept still exits 1" {
  write_native 'assert_eq "first check" a a' 'assert_eq "second check" b b' 'false'

  parity base tests/unit/x
  assert_failure 1
  assert_line 'ok tests/unit/x/sample.bats: 2 checks'
  assert_line 'FAILED: bats exited 1'
  assert_line 'test-shell-parity: 1 files, 2 checks, 0 mismatched'
}

@test "a path relative to the working directory or absolute pairs the same file" {
  write_native 'assert_eq "first check" a a' 'assert_eq "second check" b b'

  cd "$REPO/tests/unit"
  parity base ./x
  assert_success
  assert_line 'ok tests/unit/x/sample.bats: 2 checks'

  parity base "$REPO/tests/unit//x/sample.bats"
  assert_success
  assert_line 'ok tests/unit/x/sample.bats: 2 checks'
}

@test "a skipped native test is a note" {
  write_native 'skip "not here"'

  parity base tests/unit/x
  assert_failure 1
  assert_line 'note: 1 tests skipped now'
  refute_line --partial 'FAILED'
}

@test "an unknown ref ends with its message" {
  write_native 'assert_eq "first check" a a'

  parity no-such-ref tests/unit/x
  assert_failure 1
  assert_output 'test-shell-parity: no-such-ref is not a commit'
}

@test "a tree without a pair ends with its message" {
  mkdir -p "$REPO/tests/unit/y"
  printf '@test "y" { true; }\n' >"$REPO/tests/unit/y/other.bats"

  parity base tests/unit/y
  assert_failure 1
  assert_output 'test-shell-parity: no ported test under tests/unit/y has a *_test.sh at base'
  assert_no_worktree_left
}
