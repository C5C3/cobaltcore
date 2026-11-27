# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash

# The helpers every native bats file under tests/unit/ loads first:
#
#   load '../../lib/bats/common'
#
# It loads bats-support, bats-assert and bats-file, sets PROJECT_ROOT, and
# defines the description-first assertions of the old tests/lib/assertions.sh
# as checks that end the test at the first failure, soft for checks that go
# on, the check ledger hack/test-shell-parity.sh reads, a default teardown,
# and the skips for a missing tool. docs/contributing/shell-unit-tests.md
# describes the form.
#
# bats evaluates a test file once to count its tests and once per test, so
# this file defines functions and sets constants only.
#
# Environment:
#   TEST_SHELL_BATS_LIBS  directory that holds bats-support, bats-assert and
#                         bats-file; default bin/ under PROJECT_ROOT, where
#                         make install-bats puts them. The libraries load by
#                         path, not through BATS_LIB_PATH, which bats always
#                         sets, so a default could not be told from a
#                         caller's value.
#   TEST_SHELL_CHECK_LOG  file that every passed check appends a line to;
#                         unset or empty, nothing is written
#
# Runs under Bash 3.2, like every native test file.

# run --separate-stderr and run -<status> need bats 1.5.0.
bats_require_minimum_version 1.5.0

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

_common_libs="${TEST_SHELL_BATS_LIBS:-$PROJECT_ROOT/bin}"
# load resolves a relative path against the test file, the check below
# against the working directory; an absolute path means the same to both.
case "$_common_libs" in
  /*) ;;
  *) _common_libs="$PWD/$_common_libs" ;;
esac
for _common_lib in bats-support bats-assert bats-file; do
  if [ ! -f "$_common_libs/$_common_lib/load.bash" ]; then
    echo "common.bash: $_common_libs/$_common_lib/load.bash is missing; run make install-bats" >&2
    exit 1
  fi
  load "$_common_libs/$_common_lib/load"
done
unset _common_lib _common_libs

# ---------------------------------------------------------------------------
# Reporting
# ---------------------------------------------------------------------------

# Lines of a value an assertion prints at most. bats-support formats a value
# line by line in Bash, which took 21 s for the 6,318 lines of ci.yaml.
COMMON_MAX_VALUE_LINES=40

# _common_cut <value>
# Prints the value, cut after COMMON_MAX_VALUE_LINES lines with a last line
# that gives the total.
_common_cut() {
  local total
  total="$(printf '%s\n' "$1" | wc -l | tr -d ' ')"
  if [ "$total" -gt "$COMMON_MAX_VALUE_LINES" ]; then
    printf '%s\n' "$1" | sed -n "1,${COMMON_MAX_VALUE_LINES}p"
    printf '... (%s lines in total)\n' "$total"
  else
    printf '%s\n' "$1"
  fi
}

# _common_fail <description> <label> <value> [<label> <value>]...
# Fails the check through fail of bats-support: the description, then each
# labelled value, cut by _common_cut. Returns 1.
_common_fail() {
  local description="$1"
  shift
  local -a pairs
  pairs=()
  while [ "$#" -gt 1 ]; do
    pairs[${#pairs[@]}]="$1"
    pairs[${#pairs[@]}]="$(_common_cut "$2")"
    shift 2
  done
  batslib_print_kv_single_or_multi \
    "$(batslib_get_max_single_line_key_width "${pairs[@]}")" "${pairs[@]}" |
    batslib_decorate "$description" | fail
  return 1
}

# _common_ledger <description>
# Appends "<test file relative to PROJECT_ROOT><tab><description>" to
# TEST_SHELL_CHECK_LOG when it is set, with every tab and newline of the
# description replaced by a space. A file that cannot be appended to fails
# the check.
_common_ledger() {
  [ -n "${TEST_SHELL_CHECK_LOG:-}" ] || return 0
  local description="$1" tab nl
  tab="$(printf '\t')"
  nl='
'
  description="${description//$tab/ }"
  description="${description//$nl/ }"
  if ! printf '%s\t%s\n' "${BATS_TEST_FILENAME#"$PROJECT_ROOT"/}" "$description" \
    2>/dev/null >>"$TEST_SHELL_CHECK_LOG"; then
    fail "common.bash: could not append to $TEST_SHELL_CHECK_LOG"
    return 1
  fi
}

# check_passed <description>
# Records a passed check that a test made without one of the assertions, the
# passing branch of a hand-written if.
check_passed() {
  _common_ledger "$1"
}

# ---------------------------------------------------------------------------
# Assertions: the description first, the comparison of tests/lib/assertions.sh
# unchanged. A passed one prints nothing and returns 0; a failed one prints
# the description and its values and returns 1, which ends the test.
# ---------------------------------------------------------------------------

# assert_eq <description> <expected> <actual>
assert_eq() {
  if [ "$2" = "$3" ]; then
    _common_ledger "$1"
  else
    _common_fail "$1" expected "$2" actual "$3"
  fi
}

# assert_not_empty <description> <value>
assert_not_empty() {
  if [ -n "$2" ]; then
    _common_ledger "$1"
  else
    _common_fail "$1" expected 'a value that is not empty' actual "$2"
  fi
}

# assert_contains <description> <haystack> <needle>
# The haystack holds the needle as a fixed string.
assert_contains() {
  if [[ "$2" == *"$3"* ]]; then
    _common_ledger "$1"
  else
    _common_fail "$1" 'expected to contain' "$3" actual "$2"
  fi
}

# assert_not_contains <description> <haystack> <needle>
assert_not_contains() {
  if [[ "$2" != *"$3"* ]]; then
    _common_ledger "$1"
  else
    _common_fail "$1" 'expected not to contain' "$3" actual "$2"
  fi
}

# assert_starts_with <description> <actual> <prefix>
assert_starts_with() {
  if [[ "$2" == "$3"* ]]; then
    _common_ledger "$1"
  else
    _common_fail "$1" 'expected to start with' "$3" actual "$2"
  fi
}

# assert_nonzero_exit <description> <status>
assert_nonzero_exit() {
  if [ "$2" -ne 0 ]; then
    _common_ledger "$1"
  else
    _common_fail "$1" expected 'a status other than 0' actual "$2"
  fi
}

# assert_gte <description> <actual> <minimum>
assert_gte() {
  if [[ "$2" -ge "$3" ]]; then
    _common_ledger "$1"
  else
    _common_fail "$1" expected ">= $3" actual "$2"
  fi
}

# _common_file_exists <description> <file>
# Fails the check when the file does not exist.
_common_file_exists() {
  [ -e "$2" ] || _common_fail "$1" file "$2 does not exist"
}

# assert_file_contains_fixed <description> <file> <string>
# grep -qF finds the string, so a string that starts with a dash or holds
# regular expression characters is matched as it is.
assert_file_contains_fixed() {
  _common_file_exists "$1" "$2" || return 1
  if grep -qF -- "$3" "$2"; then
    _common_ledger "$1"
  else
    _common_fail "$1" file "$2" 'fixed string' "$3"
  fi
}

# assert_file_matches <description> <file> <expression>
# grep -q finds the basic regular expression. Was assert_file_contains,
# which bats-file defines with the arguments file and expression.
assert_file_matches() {
  _common_file_exists "$1" "$2" || return 1
  if grep -q "$3" "$2"; then
    _common_ledger "$1"
  else
    _common_fail "$1" file "$2" 'expected to match' "$3"
  fi
}

# assert_file_not_matches <description> <file> <expression>
# grep -q does not find the expression. Was assert_file_not_contains,
# which bats-file defines with the arguments file and expression. A missing
# file fails.
assert_file_not_matches() {
  _common_file_exists "$1" "$2" || return 1
  if ! grep -q "$3" "$2"; then
    _common_ledger "$1"
  else
    _common_fail "$1" file "$2" 'expected not to match' "$3" \
      'matching lines' "$(grep -n "$3" "$2")"
  fi
}

# ---------------------------------------------------------------------------
# Checks that go on
# ---------------------------------------------------------------------------

# soft <command> [<argument>...]
# Runs the command in a subshell with its output captured. The run fails
# when its status is not 0 or when fail was called in it: errexit is off in
# the subshell, so a function whose first check fails and whose last passes
# returns 0, and fail is wrapped there to leave a marker file. A failed run
# appends its output to $BATS_TEST_TMPDIR/soft-failures, which
# common_teardown reports. soft returns 0, also for a failed run.
#
# The command sees the variables of the test, $status and $output of the
# last run included, so the locals of soft carry names a test does not use.
soft() {
  if [ "$#" -eq 0 ]; then
    echo 'soft: usage: soft <command> [<argument>...]' >&2
    return 1
  fi
  local _soft_marker _soft_output _soft_status=0
  _soft_marker="$BATS_TEST_TMPDIR/soft-marker.$$.$RANDOM"
  _soft_output="$(
    # shellcheck disable=SC2120 # bats-assert and bats-file pass arguments.
    fail() {
      : >"$_soft_marker"
      if [ "$#" -eq 0 ]; then
        batslib_err
      else
        batslib_err "$@"
      fi
      return 1
    }
    "$@" 2>&1
  )" || _soft_status=$?
  if [ "$_soft_status" -ne 0 ] || [ -e "$_soft_marker" ]; then
    rm -f "$_soft_marker"
    printf '%s\n' "$_soft_output" >>"$BATS_TEST_TMPDIR/soft-failures"
  fi
  return 0
}

# common_teardown
# Prints the output of every failed soft run to stderr and returns 1 when
# there is one, so a test with a failed soft check fails. The default
# teardown below calls it; a file with a teardown of its own calls it there.
common_teardown() {
  if [ -s "$BATS_TEST_TMPDIR/soft-failures" ]; then
    cat "$BATS_TEST_TMPDIR/soft-failures" >&2
    return 1
  fi
}

teardown() {
  common_teardown
}

# ---------------------------------------------------------------------------
# Skips
# ---------------------------------------------------------------------------

# require_tool <tool>...
# Skips the test for the first tool that command -v does not find.
require_tool() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || skip "$tool is not installed"
  done
}

# require_mikefarah_yq
# Skips the test unless yq is the mikefarah one, whose expression language
# the tests use.
require_mikefarah_yq() {
  command -v yq >/dev/null 2>&1 && yq --version 2>&1 | grep -q 'mikefarah' ||
    skip "mikefarah yq is not installed"
}
