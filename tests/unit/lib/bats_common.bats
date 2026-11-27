#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/lib/bats/common.bash, the helpers every native bats file
# loads: each of the ten assertions passes silently and fails with status 1,
# its description and its labelled values, an empty value and a missing file
# compare as the helpers of the old form did except that a missing file now
# fails assert_file_not_matches too, a reported value is cut
# after 40 lines, every passed check appends one line to the ledger
# TEST_SHELL_CHECK_LOG and nothing is written while it is unset, soft records
# a failed check and goes on, common_teardown fails a test with what soft
# recorded, and require_tool skips for a missing tool only.
#
# The checks here use bats-assert, which common.bash loads, so a helper is
# never checked by itself.
#
# Usage: bin/bats tests/unit/lib/bats_common.bats

load '../../lib/bats/common'

# expect_failure <description> <line>...
# Asserts that the last run failed with status 1 and that its output holds
# the description and each line.
expect_failure() {
  local line
  assert_failure 1
  for line in "$@"; do
    assert_output --partial "$line"
  done
}

@test "assert_eq passes on equal strings and fails with expected and actual" {
  run assert_eq "the strings are equal" same same
  assert_success
  assert_output ''

  run assert_eq "the strings are equal" left right
  expect_failure "the strings are equal" "expected : left" "actual   : right"
}

@test "assert_not_empty passes on a value and fails on an empty one" {
  run assert_not_empty "the value is set" x
  assert_success
  assert_output ''

  run assert_not_empty "the value is set" ''
  expect_failure "the value is set" "a value that is not empty"
}

@test "assert_contains passes on a fixed substring and fails without it" {
  run assert_contains "the haystack holds the needle" "a [b] c" "[b]"
  assert_success
  # An empty needle is in every haystack, as with the old helper.
  run assert_contains "an empty needle is found" "abc" ""
  assert_success

  run assert_contains "the haystack holds the needle" "abc" "x*"
  expect_failure "the haystack holds the needle" "expected to contain : x*" "actual              : abc"
}

@test "assert_not_contains passes without the needle and fails with it" {
  run assert_not_contains "the haystack lacks the needle" "abc" "x"
  assert_success

  run assert_not_contains "the haystack lacks the needle" "abc" "b"
  expect_failure "the haystack lacks the needle" "expected not to contain : b" "actual                  : abc"
  # An empty needle is in every haystack, so the old helper failed on it.
  run assert_not_contains "an empty needle is found" "abc" ""
  expect_failure "an empty needle is found"
}

@test "assert_starts_with passes on a prefix and fails without it" {
  run assert_starts_with "the value starts with the prefix" "prefix-rest" "prefix-"
  assert_success
  run assert_starts_with "an empty prefix starts every value" "abc" ""
  assert_success

  run assert_starts_with "the value starts with the prefix" "rest-prefix" "prefix"
  expect_failure "the value starts with the prefix" "expected to start with : prefix" "actual                 : rest-prefix"
}

@test "assert_nonzero_exit passes on a status other than 0 and fails on 0" {
  run assert_nonzero_exit "the command failed" 3
  assert_success

  run assert_nonzero_exit "the command failed" 0
  expect_failure "the command failed" "expected : a status other than 0" "actual   : 0"
}

@test "assert_gte passes at and above the minimum and fails below it" {
  run assert_gte "the count reaches the minimum" 3 2
  assert_success
  run assert_gte "the count reaches the minimum" 2 2
  assert_success

  run assert_gte "the count reaches the minimum" 1 2
  expect_failure "the count reaches the minimum" "expected : >= 2" "actual   : 1"
}

@test "assert_file_contains_fixed matches a fixed string and fails on a missing one or a missing file" {
  printf 'qemu --nic none\na.b\n' >"$BATS_TEST_TMPDIR/file"

  run assert_file_contains_fixed "the file names the option" "$BATS_TEST_TMPDIR/file" "--nic none"
  assert_success
  run assert_file_contains_fixed "a dot is a dot" "$BATS_TEST_TMPDIR/file" "a.b"
  assert_success

  run assert_file_contains_fixed "the file holds axb" "$BATS_TEST_TMPDIR/file" "axb"
  expect_failure "the file holds axb" "fixed string : axb"
  run assert_file_contains_fixed "the file names the option" "$BATS_TEST_TMPDIR/missing" "--nic none"
  expect_failure "the file names the option" "$BATS_TEST_TMPDIR/missing does not exist"
}

@test "assert_file_matches matches an expression and fails without a match or a file" {
  printf 'replicas: 3\n' >"$BATS_TEST_TMPDIR/file"

  run assert_file_matches "the file sets replicas" "$BATS_TEST_TMPDIR/file" '^replicas: [0-9]'
  assert_success

  run assert_file_matches "the file sets two replicas" "$BATS_TEST_TMPDIR/file" '^replicas: 2'
  expect_failure "the file sets two replicas" "expected to match : ^replicas: 2"
  run assert_file_matches "the file sets replicas" "$BATS_TEST_TMPDIR/missing" 'replicas'
  expect_failure "the file sets replicas" "$BATS_TEST_TMPDIR/missing does not exist"
}

@test "assert_file_not_matches passes without a match and fails on a match or a missing file" {
  printf 'replicas: 3\n' >"$BATS_TEST_TMPDIR/file"

  run assert_file_not_matches "the file sets no image" "$BATS_TEST_TMPDIR/file" '^image:'
  assert_success

  run assert_file_not_matches "the file sets no replicas" "$BATS_TEST_TMPDIR/file" '^replicas'
  expect_failure "the file sets no replicas" "expected not to match" "1:replicas: 3"
  # The old assert_file_not_contains passed on a missing file.
  run assert_file_not_matches "the file sets no image" "$BATS_TEST_TMPDIR/missing" '^image:'
  expect_failure "the file sets no image" "$BATS_TEST_TMPDIR/missing does not exist"
}

@test "a reported value is cut after 40 lines with its total" {
  local long
  long="$(seq 1 100)"

  run assert_eq "the long values are equal" "short" "$long"
  expect_failure "the long values are equal" "... (100 lines in total)"
  assert_line --partial "  40"
  refute_line --partial "  41"
}

@test "every passed check appends one ledger line and a failed one none" {
  local ledger="$BATS_TEST_TMPDIR/ledger" file="tests/unit/lib/bats_common.bats"
  printf 'x\n' >"$BATS_TEST_TMPDIR/file"
  export TEST_SHELL_CHECK_LOG="$ledger"

  assert_eq "eq" a a
  assert_not_empty "not empty" a
  assert_contains "contains" abc b
  assert_not_contains "not contains" abc x
  assert_starts_with "starts with" abc a
  assert_nonzero_exit "nonzero" 1
  assert_gte "gte" 2 1
  assert_file_contains_fixed "fixed" "$BATS_TEST_TMPDIR/file" x
  assert_file_matches "matches" "$BATS_TEST_TMPDIR/file" '^x$'
  assert_file_not_matches "not matches" "$BATS_TEST_TMPDIR/file" '^y$'
  check_passed "by hand"
  check_passed "$(printf 'tab\there and\nnewline')"
  run assert_eq "a failed check" a b

  unset TEST_SHELL_CHECK_LOG
  assert_equal "$(cat "$ledger")" "$(printf "$file\t%s\n" eq "not empty" contains \
    "not contains" "starts with" nonzero gte fixed matches "not matches" "by hand" \
    "tab here and newline")"
}

@test "nothing is written while TEST_SHELL_CHECK_LOG is unset or empty" {
  unset TEST_SHELL_CHECK_LOG
  assert_eq "eq" a a
  check_passed "by hand"
  TEST_SHELL_CHECK_LOG='' assert_eq "eq" a a

  run find "$BATS_TEST_TMPDIR" -type f
  assert_output ''
}

@test "a relative TEST_SHELL_BATS_LIBS resolves against the working directory" {
  local file="$BATS_TEST_TMPDIR/relative.bats" libs
  libs="${TEST_SHELL_BATS_LIBS:-$PROJECT_ROOT/bin}"
  mkdir -p "$BATS_TEST_TMPDIR/cwd"
  ln -s "$libs" "$BATS_TEST_TMPDIR/cwd/libs"
  printf '%s\n' "load '$PROJECT_ROOT/tests/lib/bats/common'" '@test "loads" { assert_equal 1 1; }' >"$file"

  cd "$BATS_TEST_TMPDIR/cwd"
  run env -u TEST_SHELL_CHECK_LOG TEST_SHELL_BATS_LIBS=libs "$BATS_ROOT/bin/bats" --formatter tap "$file"
  assert_success
  assert_line 'ok 1 loads'
}

@test "a ledger that cannot be appended to fails the check" {
  TEST_SHELL_CHECK_LOG="$BATS_TEST_TMPDIR/missing/ledger" run assert_eq "eq" a a
  expect_failure "common.bash: could not append to $BATS_TEST_TMPDIR/missing/ledger"
}

@test "soft records a failed assertion and goes on" {
  soft assert_eq "the first value" one two
  assert_file_contains "$BATS_TEST_TMPDIR/soft-failures" 'the first value'
  assert_file_contains "$BATS_TEST_TMPDIR/soft-failures" 'actual   : two'
  rm "$BATS_TEST_TMPDIR/soft-failures"
}

@test "soft records a function whose first assertion fails and whose last passes" {
  two_checks() {
    assert_eq "the first check" one two
    assert_eq "the last check" same same
  }
  soft two_checks
  assert_file_contains "$BATS_TEST_TMPDIR/soft-failures" 'the first check'
  rm "$BATS_TEST_TMPDIR/soft-failures"
}

@test "soft records a failed bats-assert call" {
  soft assert_equal one two
  assert_file_contains "$BATS_TEST_TMPDIR/soft-failures" 'values do not equal'
  rm "$BATS_TEST_TMPDIR/soft-failures"
}

@test "soft records a command that fails without calling fail" {
  soft sh -c 'echo plain failure; exit 3'
  assert_file_contains "$BATS_TEST_TMPDIR/soft-failures" 'plain failure'
  rm "$BATS_TEST_TMPDIR/soft-failures"
}

@test "a check under soft sees status and output of the last run" {
  run printf hello
  soft assert_output hello
  soft assert_eq "the run exits 0" 0 "$status"
  assert_file_not_exists "$BATS_TEST_TMPDIR/soft-failures"

  exits_zero() { assert_eq "$1 exits 0" 0 "$status"; }
  run bash -c 'exit 4'
  soft exits_zero "the second run"
  assert_file_contains "$BATS_TEST_TMPDIR/soft-failures" 'the second run exits 0'
  rm "$BATS_TEST_TMPDIR/soft-failures"
}

@test "soft records nothing for a passing check" {
  soft assert_eq "the values are equal" same same
  soft true
  assert_file_not_exists "$BATS_TEST_TMPDIR/soft-failures"
}

@test "soft without a command fails with its usage" {
  run soft
  assert_failure 1
  assert_output 'soft: usage: soft <command> [<argument>...]'
}

@test "common_teardown fails with the recorded output and passes without" {
  run common_teardown
  assert_success
  assert_output ''

  printf 'first failure\nsecond failure\n' >"$BATS_TEST_TMPDIR/soft-failures"
  run common_teardown
  assert_failure 1
  assert_output "$(printf 'first failure\nsecond failure')"
  rm "$BATS_TEST_TMPDIR/soft-failures"
}

@test "require_tool skips for a missing tool and not for bash" {
  # bats' skip ends a subshell of run with status 0; this one says why.
  skip() { echo "skipped: $1"; exit 0; }

  run require_tool bash
  assert_success
  assert_output ''

  run require_tool bash no-such-tool-for-common-bash bash
  assert_output 'skipped: no-such-tool-for-common-bash is not installed'
}

@test "a loop with two failing soft checks fails its test once with both descriptions" {
  local file="$BATS_TEST_TMPDIR/loop.bats"
  cat >"$file" <<EOF
load '$PROJECT_ROOT/tests/lib/bats/common'
@test "every item is even" {
  local item
  for item in 1 2 3; do
    soft assert_eq "item \$item is even" 0 "\$((item % 2))"
  done
}
@test "a later test still runs" {
  require_tool no-such-tool-for-common-bash
}
EOF
  run env -u TEST_SHELL_CHECK_LOG "$BATS_ROOT/bin/bats" --formatter tap "$file"
  assert_failure 1
  assert_line 'not ok 1 every item is even'
  assert_line 'ok 2 a later test still runs # skip no-such-tool-for-common-bash is not installed'
  assert_output --partial 'item 1 is even'
  assert_output --partial 'item 3 is even'
  refute_output --partial 'item 2 is even'
}
