#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/test-shell.sh: run the shell unit tests under tests/unit/ through
# bats. A test is either a native bats file, any *.bats there, or an old
# *_test.sh script, which runs as one bats test that
# tests/unit/legacy_scripts.bats registers. `make test-shell` calls it after
# `make install-bats`. docs/contributing/shell-unit-tests.md says how a
# native file is written.
#
# Before any test runs, it fails on a symlink under tests/unit/ whose target
# is missing, on a .sh file there whose name does not end in _test.sh, on a
# tree find cannot walk, on a tree without any test, on a filter that
# selects nothing, and on a missing helper library while a native file is
# selected.
#
# Environment:
#   BATS                   the bats to run; default bin/bats under the
#                          repository root, which make install-bats links
#   TEST_SHELL_JOBS        parallel workers; default the number of online
#                          CPUs. Above 1 bats needs GNU parallel and flock
#                          or shlock. Unset or empty, a missing tool makes
#                          the run serial with a notice; set above 1, it
#                          fails the run before any test runs.
#   TEST_SHELL_FILTER      extended regular expression matched against the
#                          path of each native file and each script, such as
#                          renovate/; unset or empty selects every file
#   TEST_SHELL_NAME        expression bats matches against the test names
#                          (bats --filter); set, it runs the matching tests
#                          of the selected native files and no script, which
#                          has no test names; unset or empty runs every test
#   TEST_SHELL_BATS_LIBS   directory that holds bats-support, bats-assert
#                          and bats-file, exported for
#                          tests/lib/bats/common.bash; default bin/ under the
#                          repository root, where make install-bats puts them
#   TEST_SHELL_REPORT_DIR  directory of the JUnit report report.xml; default
#                          _output/test-shell, a relative value resolved
#                          against the repository root
#
# stdout carries the TAP stream of bats: an "ok" line per green test, and a
# "not ok" line followed by the test's output per red one. A native file
# brings one test per @test, a script is one test. The stderr of bats
# follows once bats is done; it holds the "not ok" line of a native file
# that fails before its tests run. A red run ends with the list of failed
# tests on stderr. The exit status is the one of bats.
#
# The script runs under Bash 3.2: it builds the bats arguments with `set --`,
# because an empty array under `set -u` is an unbound variable there.
#
# Usage: hack/test-shell.sh

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

# ---------------------------------------------------------------------------
# Pre-run checks: each fails the run before any test runs.
# ---------------------------------------------------------------------------

broken="$(find -L tests/unit -type l)" || { echo 'test-shell: find could not walk tests/unit' >&2; exit 1; }
broken="$(printf '%s\n' "$broken" | LC_ALL=C sort)"
if [ -n "$broken" ]; then
  printf '%s\n' "$broken" | sed 's|^|test-shell: |; s|$| is a symlink whose target does not exist, so nothing behind it would run|' >&2
  exit 1
fi
stray="$(find -L tests/unit ! -type d -name '*.sh' ! -name '*_test.sh')" || { echo 'test-shell: find could not walk tests/unit' >&2; exit 1; }
stray="$(printf '%s\n' "$stray" | LC_ALL=C sort)"
if [ -n "$stray" ]; then
  printf '%s\n' "$stray" | sed 's|^|test-shell: |; s|$| does not end in _test.sh and would never run; rename it, or move a helper under tests/lib/|' >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Selection: the scripts and the native files, then the two filters.
# ---------------------------------------------------------------------------

legacy="$(find -L tests/unit ! -type d -name '*_test.sh')" || { echo 'test-shell: find could not walk tests/unit' >&2; exit 1; }
native="$(find -L tests/unit ! -type d -name '*.bats' ! -path tests/unit/legacy_scripts.bats)" || { echo 'test-shell: find could not walk tests/unit' >&2; exit 1; }
native="$(printf '%s\n' "$native" | LC_ALL=C sort)"
if [ -z "$legacy" ] && [ -z "$native" ]; then
  echo 'test-shell: no *_test.sh and no *.bats test found under tests/unit' >&2
  exit 1
fi

# filter_paths <list> prints the lines of <list> that match TEST_SHELL_FILTER
# and returns the status of grep: 1 for no match, 2 for an invalid
# expression, which grep reports before it reads a line.
filter_paths() {
  printf '%s\n' "$1" | grep -E -- "$TEST_SHELL_FILTER"
}

if [ -n "${TEST_SHELL_FILTER:-}" ]; then
  rc=0
  legacy="$(filter_paths "$legacy")" || rc=$?
  [ "$rc" -le 1 ] && { native="$(filter_paths "$native")" || rc=$?; }
  if [ "$rc" -gt 1 ]; then
    echo "test-shell: TEST_SHELL_FILTER '$TEST_SHELL_FILTER' is not a valid extended regular expression" >&2
    exit 1
  fi
  if [ -z "$legacy" ] && [ -z "$native" ]; then
    echo "test-shell: TEST_SHELL_FILTER '$TEST_SHELL_FILTER' matches no test file" >&2
    exit 1
  fi
fi
# tests/unit/legacy_scripts.bats applies the same filter to its own list.
export TEST_SHELL_FILTER

# A script has no test names, so a name filter runs native tests only.
if [ -n "${TEST_SHELL_NAME:-}" ]; then
  legacy=
  if [ -z "$native" ]; then
    echo "test-shell: TEST_SHELL_NAME '$TEST_SHELL_NAME' matches no test" >&2
    exit 1
  fi
fi

# tests/lib/bats/common.bash loads the three libraries from this directory,
# which a relative value names below the root.
TEST_SHELL_BATS_LIBS="${TEST_SHELL_BATS_LIBS:-$ROOT/bin}"
case "$TEST_SHELL_BATS_LIBS" in
  /*) ;;
  *) TEST_SHELL_BATS_LIBS="$ROOT/$TEST_SHELL_BATS_LIBS" ;;
esac
export TEST_SHELL_BATS_LIBS
if [ -n "$native" ]; then
  for lib in bats-support bats-assert bats-file; do
    if [ ! -f "$TEST_SHELL_BATS_LIBS/$lib/load.bash" ]; then
      echo "test-shell: $TEST_SHELL_BATS_LIBS/$lib/load.bash is missing; run make install-bats" >&2
      exit 1
    fi
  done
fi

# ---------------------------------------------------------------------------
# bats and its options
# ---------------------------------------------------------------------------

bats="${BATS:-$ROOT/bin/bats}"
if [ ! -f "$bats" ] || [ ! -x "$bats" ]; then
  echo "test-shell: $bats is not an executable bats; run make install-bats" >&2
  exit 1
fi

# bats runs more than one worker only with GNU parallel and a lock tool.
missing=
if ! command -v parallel >/dev/null 2>&1; then
  missing=parallel
elif ! command -v flock >/dev/null 2>&1 && ! command -v shlock >/dev/null 2>&1; then
  missing=flock
fi

if [ -z "${TEST_SHELL_JOBS:-}" ]; then
  jobs="$(getconf _NPROCESSORS_ONLN 2>/dev/null)" || jobs=1
  case "$jobs" in
    '' | 0* | *[!0-9]*) jobs=1 ;;
  esac
  if [ -n "$missing" ]; then
    echo "test-shell: running serially, $missing is not installed" >&2
    jobs=1
  fi
else
  jobs="$TEST_SHELL_JOBS"
  case "$jobs" in
    0* | *[!0-9]*)
      echo "test-shell: TEST_SHELL_JOBS must be a positive integer, got '$jobs'" >&2
      exit 1
      ;;
  esac
  if [ "$jobs" -gt 1 ] && [ -n "$missing" ]; then
    echo "test-shell: TEST_SHELL_JOBS=$jobs needs $missing, which is not installed" >&2
    exit 1
  fi
fi

# bats takes the worker count and the report file name from these when its
# flags do not set them. TEST_SHELL_JOBS alone sets the workers here, and
# the report is always report.xml.
unset BATS_NUMBER_OF_PARALLEL_JOBS BATS_REPORT_FILENAME

report_dir="${TEST_SHELL_REPORT_DIR:-_output/test-shell}"
if ! mkdir -p "$report_dir"; then
  echo "test-shell: could not create $report_dir" >&2
  exit 1
fi

set -- --formatter tap --report-formatter junit --output "$report_dir"
if [ "$jobs" -gt 1 ]; then
  set -- "$@" --jobs "$jobs"
fi
if [ -n "${TEST_SHELL_NAME:-}" ]; then
  set -- "$@" --filter "$TEST_SHELL_NAME"
fi
if [ -n "$legacy" ]; then
  set -- "$@" tests/unit/legacy_scripts.bats
fi
# A here-document, not a process substitution, keeps the loop in this shell.
while IFS= read -r file; do
  [ -z "$file" ] || set -- "$@" "$file"
done <<EOF
$native
EOF

# ---------------------------------------------------------------------------
# The run
# ---------------------------------------------------------------------------

log="$(mktemp "${TMPDIR:-/tmp}/test-shell.XXXXXX")" || {
  echo "test-shell: could not create a temporary log" >&2
  exit 1
}
trap 'rm -f "$log" "$log.err"' EXIT

# bats reports a file that fails before its tests run on stderr only, so
# the list of failed tests reads both streams.
"$bats" "$@" 2>"$log.err" | tee "$log"
status=${PIPESTATUS[0]}
cat "$log.err" >&2

if [ "$status" -ne 0 ]; then
  # The JUnit report makes bats time every test and append " # in <n> ms"
  # to its TAP line, which the list leaves out.
  failed="$(sed -n '/^not ok [0-9][0-9]* /{s///;s/ # in [0-9][0-9]* ms$//;p;}' "$log" "$log.err")"
  if [ -n "$failed" ]; then
    total="$(sed -n 's/^1\.\.\([0-9][0-9]*\)$/\1/p' "$log" "$log.err" | head -n 1)"
    count="$(printf '%s\n' "$failed" | wc -l | tr -d ' ')"
    {
      echo "test-shell: $count of $total tests failed:"
      printf '%s\n' "$failed" | sed 's/^/test-shell:   /'
    } >&2
  elif ! grep -Eq '^(not )?ok ' "$log" "$log.err" && [ -n "${TEST_SHELL_NAME:-}" ]; then
    # bats ends its own "Found no tests" message without a newline.
    printf '\ntest-shell: TEST_SHELL_NAME %s matches no test\n' "'$TEST_SHELL_NAME'" >&2
  else
    echo "test-shell: bats exited $status without a failed test" >&2
  fi
fi

exit "$status"
