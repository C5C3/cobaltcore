#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/test-shell-parity.sh: show that a port of old *_test.sh scripts to
# native bats files keeps every check. For each <dir>/<name>.bats under the
# given paths whose <dir>/<name>_test.sh exists at the base commit, it
# compares the descriptions the old script prints as "PASS:" lines with the
# descriptions the native file records in the check ledger of
# tests/lib/bats/common.bash. docs/contributing/shell-unit-tests.md says how
# a port uses it; the summary line goes into the pull request body.
#
# The old scripts run in a detached git worktree of the base commit in a
# temporary directory, which an EXIT trap removes. The native files run in
# one bats run from the repository root with TEST_SHELL_CHECK_LOG set.
#
# Output, one line per pair: "ok <file>: <n> checks" when both lists hold
# the same descriptions, else "MISMATCH <file>: <o> checks at <base-ref>,
# <n> now" followed by "- <description>" for each one only the old script
# has and "+ <description>" for each one only the native file has. An old
# script that fails prints "FAILED <script> at <base-ref>" with its FAIL:
# lines, and a bats run that fails prints "FAILED: bats exited <status>"
# with its "not ok" lines. Skips are notes, not failures. The last line is
# "test-shell-parity: <files> files, <checks> checks, <m> mismatched", with
# the checks of the base. The exit status is 0 when nothing mismatched and
# nothing failed, else 1.
#
# Environment:
#   BATS             the bats to run; default bin/bats under the repository
#                    root, which make install-bats links
#   TEST_SHELL_JOBS  parallel workers for the old scripts and for bats
#                    (--jobs, which needs GNU parallel); default 1
#
# Runs under Bash 3.2, like hack/test-shell.sh.
#
# A path is a directory or a native file, relative to the working directory
# or absolute.
#
# Usage: hack/test-shell-parity.sh [<base-ref> [<path>...]]
#        defaults: origin/main and tests/unit

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
caller="$PWD"
cd "$ROOT" || exit 1

base="${1:-origin/main}"
[ "$#" -eq 0 ] || shift
[ "$#" -gt 0 ] || set -- tests/unit

jobs="${TEST_SHELL_JOBS:-1}"
case "$jobs" in
  '' | 0* | *[!0-9]*)
    echo "test-shell-parity: TEST_SHELL_JOBS must be a positive integer, got '$jobs'" >&2
    exit 1
    ;;
esac

if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
  echo "test-shell-parity: $base is not a commit" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Pairs: a native file whose old script exists at the base.
# ---------------------------------------------------------------------------

# root_relative <path>
# Prints <path>, taken relative to the caller's working directory unless
# absolute, as a path relative to ROOT ("." for ROOT itself). A path that
# does not exist is printed as it is.
root_relative() {
  local abs="$1"
  case "$abs" in
    /*) ;;
    *) abs="$caller/$abs" ;;
  esac
  if [ -d "$abs" ]; then
    abs="$(cd "$abs" && pwd)" || abs="$1"
  elif [ -e "$abs" ]; then
    abs="$(cd "$(dirname "$abs")" && pwd)/$(basename "$abs")" || abs="$1"
  else
    abs="$1"
  fi
  case "$abs" in
    "$ROOT") echo . ;;
    "$ROOT"/*) echo "${abs#"$ROOT"/}" ;;
    *) echo "$abs" ;;
  esac
}

pairs=
for path in "$@"; do
  candidates="$(find -L "$(root_relative "$path")" -name '*.bats' 2>/dev/null)"
  while IFS= read -r file; do
    file="${file#./}"
    [ -n "$file" ] || continue
    [ "$file" != tests/unit/legacy_scripts.bats ] || continue
    if git cat-file -e "$base:${file%.bats}_test.sh" 2>/dev/null; then
      pairs="$pairs$file
"
    fi
  done <<EOF
$candidates
EOF
done
pairs="$(printf '%s' "$pairs" | LC_ALL=C sort -u)"
if [ -z "$pairs" ]; then
  echo "test-shell-parity: no ported test under $* has a *_test.sh at $base" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# The base worktree, removed on every exit.
# ---------------------------------------------------------------------------

tmp="$(mktemp -d "${TMPDIR:-/tmp}/test-shell-parity.XXXXXX")" || {
  echo 'test-shell-parity: could not create a temporary directory' >&2
  exit 1
}
# git names a worktree after the last part of its path, so a unique one
# keeps runs at the same time from racing for a name.
base_tree="$tmp/$(basename "$tmp")"
# The old scripts still running when the tool is stopped; each job execs its
# script, so the PID is the script's.
running=
cleanup() {
  [ -z "$running" ] || kill $running 2>/dev/null
  [ ! -d "$base_tree" ] || git worktree remove --force "$base_tree" >/dev/null 2>&1
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if ! git worktree add --detach -q "$base_tree" "$base" >/dev/null 2>&1; then
  echo "test-shell-parity: could not check out $base" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Old side: each script from the root of the worktree, $jobs at a time.
# ---------------------------------------------------------------------------

# collect waits for the jobs of the batch and keeps the status of each.
collect() {
  local job
  for job in $batch; do
    wait "${job#*:}"
    echo "$?" >"$tmp/old/${job%%:*}.status"
  done
  batch=
  running=
}

mkdir -p "$tmp/old"
n=0
batch=
while IFS= read -r file; do
  n=$((n + 1))
  script="${file%.bats}_test.sh"
  (cd "$base_tree" && exec bash "$script" >"$tmp/old/$n.out" 2>&1) &
  batch="$batch $n:$!"
  running="$running $!"
  if [ $((n % jobs)) -eq 0 ]; then
    collect
  fi
done <<EOF
$pairs
EOF
collect

# ---------------------------------------------------------------------------
# New side: one bats run over the native files with the ledger.
# ---------------------------------------------------------------------------

bats="${BATS:-$ROOT/bin/bats}"
ledger="$tmp/ledger"
: >"$ledger"
set -- --formatter tap
[ "$jobs" -le 1 ] || set -- "$@" --jobs "$jobs"
while IFS= read -r file; do
  set -- "$@" "$file"
done <<EOF
$pairs
EOF
TEST_SHELL_CHECK_LOG="$ledger" "$bats" "$@" >"$tmp/bats.out" 2>&1
bats_status=$?

# ---------------------------------------------------------------------------
# Comparison
# ---------------------------------------------------------------------------

tab="$(printf '\t')"
checks=0 mismatched=0 failed=0
n=0
while IFS= read -r file; do
  n=$((n + 1))
  script="${file%.bats}_test.sh"
  sed -n "s/^ *PASS: //p" "$tmp/old/$n.out" | tr '\t' ' ' | LC_ALL=C sort >"$tmp/old.list"
  awk -v prefix="$file$tab" 'index($0, prefix) == 1 { print substr($0, length(prefix) + 1) }' \
    "$ledger" | LC_ALL=C sort >"$tmp/new.list"
  old_count="$(wc -l <"$tmp/old.list" | tr -d ' ')"
  new_count="$(wc -l <"$tmp/new.list" | tr -d ' ')"
  checks=$((checks + old_count))

  if [ "$(cat "$tmp/old/$n.status")" != 0 ]; then
    failed=1
    echo "FAILED $script at $base"
    grep 'FAIL:' "$tmp/old/$n.out"
  fi
  skipped="$(sed -n 's/^Results: .* \([0-9][0-9]*\) skipped$/\1/p' "$tmp/old/$n.out" | tail -n 1)"
  if [ -n "$skipped" ] && [ "$skipped" -gt 0 ]; then
    echo "note: $script skipped $skipped checks at $base"
  fi

  if cmp -s "$tmp/old.list" "$tmp/new.list"; then
    echo "ok $file: $new_count checks"
  else
    mismatched=$((mismatched + 1))
    echo "MISMATCH $file: $old_count checks at $base, $new_count now"
    LC_ALL=C comm -23 "$tmp/old.list" "$tmp/new.list" | sed 's/^/- /'
    LC_ALL=C comm -13 "$tmp/old.list" "$tmp/new.list" | sed 's/^/+ /'
  fi
done <<EOF
$pairs
EOF

if [ "$bats_status" -ne 0 ]; then
  failed=1
  echo "FAILED: bats exited $bats_status"
  grep '^not ok ' "$tmp/bats.out"
fi
skipped_now="$(grep -c '^ok .* # skip' "$tmp/bats.out")"
if [ "$skipped_now" -gt 0 ]; then
  echo "note: $skipped_now tests skipped now"
fi

echo "test-shell-parity: $n files, $checks checks, $mismatched mismatched"
[ "$mismatched" -eq 0 ] && [ "$failed" -eq 0 ]
