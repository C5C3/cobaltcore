#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify every native bats file under tests/unit/ (every *.bats but the
# script wrapper tests/unit/legacy_scripts.bats) keeps the native form of
# docs/contributing/shell-unit-tests.md: the bats shebang and the SPDX pair
# in its first five lines, a load of tests/lib/bats/common.bash, no code at
# file level that creates a temporary directory or sets a trap, none of the
# counters and the assertion file of the old form, common_teardown in a
# teardown of its own, and no construct newer than Bash 3.2, which holds for
# the helpers under tests/lib/bats/ too. Each test checks every file with
# soft, so a red run names each offender.
#
# Usage: bin/bats tests/unit/lib/native_form.bats

load '../../lib/bats/common'

# native_files
# Prints the native files under tests/unit/, relative to PROJECT_ROOT, found
# the way hack/test-shell.sh finds them.
native_files() {
  (cd "$PROJECT_ROOT" && find -L tests/unit ! -type d -name '*.bats' ! -path tests/unit/legacy_scripts.bats) |
    LC_ALL=C sort
}

check_header() {
  local head
  head="$(head -n 5 "$PROJECT_ROOT/$1")"
  assert_contains "$1 has the bats shebang in its first five lines" "$head" '#!/usr/bin/env bats'
  assert_contains "$1 has SPDX-FileCopyrightText in its first five lines" "$head" '# SPDX-FileCopyrightText: '
  assert_contains "$1 has SPDX-License-Identifier in its first five lines" "$head" '# SPDX-License-Identifier: '
}

check_load() {
  assert_file_matches "$1 loads lib/bats/common" "$PROJECT_ROOT/$1" \
    "^load ['\"]*\(\.\./\)*lib/bats/common['\"]*$"
}

# grep_absent <description> <file> <extended regular expression>
# Passes when grep -E finds no line; otherwise fails with the lines found.
grep_absent() {
  local hits
  if hits="$(grep -nE -- "$3" "$PROJECT_ROOT/$2")"; then
    fail "$(printf '%s\n%s' "$1" "$hits")"
  else
    check_passed "$1"
  fi
}

# A line at column 0 that is no comment runs once per test and once to
# count, so it must not create a temporary directory or set a trap.
check_file_level() {
  grep_absent "$1 creates no temporary directory at file level" "$1" \
    '^([^[:space:]#].*)?mk[t]emp'
  assert_file_not_matches "$1 sets no trap at file level" "$PROJECT_ROOT/$1" '^tra[p] '
}

check_old_form() {
  grep_absent "$1 assigns none of the counters PASS, FAIL and SKIP" "$1" \
    '(^|[^A-Za-z0-9_])(PASS|FAIL|SKIP)='
  assert_file_not_matches "$1 does not source the old assertion file" "$PROJECT_ROOT/$1" \
    'tests/lib/assertions[.]sh'
}

# Any way to define the function counts: teardown(), teardown () and
# function teardown.
check_teardown() {
  if grep -qE '(^|[[:space:]])teardown[[:space:]]*\(\)|function[[:space:]]+teardown([[:space:]]|$)' \
    "$PROJECT_ROOT/$1"; then
    assert_file_contains_fixed "$1 calls common_teardown in its own teardown" \
      "$PROJECT_ROOT/$1" 'common_teardown'
  fi
}

# The pattern is bracketed, and no alternative after a | starts with &, so
# that this file does not match it itself.
check_bash32() {
  grep_absent "$1 uses no construct newer than Bash 3.2" "$1" \
    '&[>]>|map[f]ile|read[a]rray|declare -[A]|\$\{[a-zA-Z_]+(,,|\^\^)|[|]&|copro[c]'
}

@test "every native file starts with the bats shebang and the SPDX pair" {
  local file
  # An empty list would leave every test below green with nothing checked.
  assert_contains "the list of native files holds this file" "$(native_files)" \
    tests/unit/lib/native_form.bats
  for file in $(native_files); do
    soft check_header "$file"
  done
}

@test "every native file loads tests/lib/bats/common.bash" {
  local file
  for file in $(native_files); do
    soft check_load "$file"
  done
}

@test "no native file creates a temporary directory or sets a trap at file level" {
  local file
  for file in $(native_files); do
    soft check_file_level "$file"
  done
}

@test "no native file keeps the counters or the assertion file of the old form" {
  local file
  for file in $(native_files); do
    soft check_old_form "$file"
  done
}

@test "a native file with a teardown of its own calls common_teardown in it" {
  local file
  for file in $(native_files); do
    soft check_teardown "$file"
  done
}

@test "no native file and no helper uses a construct newer than Bash 3.2" {
  local file
  for file in $(native_files) $(cd "$PROJECT_ROOT" && ls tests/lib/bats/*.bash); do
    soft check_bash32 "$file"
  done
}
