#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/test-shell.sh: run the shell unit tests under tests/unit/ through
# bats, one bats test per *_test.sh (tests/unit/legacy_scripts.bats
# registers them). `make test-shell` calls it after `make install-bats`.
#
# Before any test runs, it fails on a symlink under tests/unit/ whose target
# is missing, on a .sh file there whose name does not end in _test.sh, on a
# tree find cannot walk, and on a tree without any test.
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
#                          script path, such as renovate/; unset or empty
#                          selects every script
#   TEST_SHELL_REPORT_DIR  directory of the JUnit report report.xml; default
#                          _output/test-shell, a relative value resolved
#                          against the repository root
#
# stdout carries the TAP stream of bats: an "ok" line per green script, and
# a "not ok" line followed by the script's output per red one. A red run
# ends with the list of failed scripts on stderr. The exit status is the one
# of bats.
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
tests="$(find -L tests/unit ! -type d -name '*_test.sh')" || { echo 'test-shell: find could not walk tests/unit' >&2; exit 1; }
[ -n "$tests" ] || { echo 'test-shell: no *_test.sh found under tests/unit' >&2; exit 1; }

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
if [ -n "${TEST_SHELL_FILTER:-}" ]; then
  set -- "$@" --filter "$TEST_SHELL_FILTER"
fi
set -- "$@" tests/unit/legacy_scripts.bats

# ---------------------------------------------------------------------------
# The run
# ---------------------------------------------------------------------------

log="$(mktemp "${TMPDIR:-/tmp}/test-shell.XXXXXX")" || {
  echo "test-shell: could not create a temporary log" >&2
  exit 1
}
trap 'rm -f "$log"' EXIT

"$bats" "$@" | tee "$log"
status=${PIPESTATUS[0]}

if [ "$status" -ne 0 ]; then
  # The JUnit report makes bats time every test and append " # in <n> ms"
  # to its TAP line, which the list leaves out.
  failed="$(sed -n '/^not ok [0-9][0-9]* /{s///;s/ # in [0-9][0-9]* ms$//;p;}' "$log")"
  if [ -n "$failed" ]; then
    total="$(sed -n 's/^1\.\.\([0-9][0-9]*\)$/\1/p' "$log" | head -n 1)"
    count="$(printf '%s\n' "$failed" | wc -l | tr -d ' ')"
    {
      echo "test-shell: $count of $total scripts failed:"
      printf '%s\n' "$failed" | sed 's/^/test-shell:   /'
    } >&2
  elif ! grep -Eq '^(not )?ok ' "$log" && [ -n "${TEST_SHELL_FILTER:-}" ]; then
    # bats ends its own "Found no tests" message without a newline.
    printf '\ntest-shell: TEST_SHELL_FILTER %s matches no script\n' "'$TEST_SHELL_FILTER'" >&2
  else
    echo "test-shell: bats exited $status without a failed script" >&2
  fi
fi

exit "$status"
