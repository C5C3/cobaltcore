#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify no tracked shell script pipes a variable into `grep -q`.
#
# `printf '%s\n' "$var" | grep -q …` is racy under `set -o pipefail`:
# grep -q exits on its first match, and when "$var" is larger than one
# write chunk printf is still writing into a pipe nobody reads. It then
# dies of SIGPIPE, or reports "printf: write error: Broken pipe" where
# the signal is ignored (as on the GitHub Actions runner), and pipefail
# turns the successful match into a failed pipeline. The `if` takes the
# else branch and the test reports a missing match that is present: this
# flaked test-shell on a 33 KB docs section whose heading sits in the
# first 4 KB. `grep -q … <<<"$var"` has no writer process to lose the race.
#
# Usage: bin/bats tests/unit/ci/grep_q_here_string_guard.bats

load '../../lib/bats/common'

SELF="tests/unit/ci/$(basename "$BATS_TEST_FILENAME")"

# A printf/echo of a quoted expansion piped into a grep whose flags include
# q. Comment lines are dropped separately, so prose may still quote the shape.
PIPE_INTO_GREP_Q='(printf|echo)[^|]*"\$[^"]*"[[:space:]]*\|[[:space:]]*grep([[:space:]]+-[[:alpha:]]+)*[[:space:]]+-[[:alpha:]]*q[[:alpha:]]*([[:space:]]|$)'

# Print path:line:content for every non-comment line matching the shape.
# Reads the file list (NUL-separated) from stdin.
offending_lines() {
  xargs -0 grep -nHE -- "$PIPE_INTO_GREP_Q" 2>/dev/null \
    | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' || true
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# The detector has to catch the flaky spellings and let the here-string
# through, or the repo-wide scan below passes vacuously.
@test "the detector flags variable pipes into grep -q and spares here-strings" {
  local tmp
  tmp="$(mktemp "$BATS_TEST_TMPDIR/shapes.XXXXXX")"
  # shellcheck disable=SC2016 # literal shell source, not expanded here
  printf '%s\n' \
    'if printf '\''%s\n'\'' "$section" | grep -qE '\''^### x'\''; then' \
    'echo "${list}" | grep -Fxq -- "$1" || return 1' \
    'if printf '\''%s'\'' "$raw" | grep -E -q y; then' \
    'if grep -qE '\''^### x'\'' <<<"$section"; then' \
    '# prose may quote: printf "$x" | grep -q y' \
    'printf '\''%s\n'\'' "$block" | grep -E '\''^ *paths:'\'' | sed '\''s/^/  /'\''' \
    >"$tmp"

  local hits
  hits="$(printf '%s\0' "$tmp" | offending_lines | cut -d: -f2 | tr '\n' ' ')"
  rm -f "$tmp"

  assert_eq "flags exactly the three racy lines" "1 2 3 " "$hits"
}

@test "no tracked shell script pipes a variable into grep -q" {
  if ! git -C "$PROJECT_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    skip "not a git checkout (1 check skipped)"
  fi

  local found
  found="$(cd "$PROJECT_ROOT" \
    && git ls-files -z -- '*.sh' '*.bats' ':!'"$SELF" | offending_lines)"

  if [[ -z "$found" ]]; then
    check_passed "no printf/echo \"\$var\" | grep -q pipelines"
  else
    while IFS= read -r line; do
      echo "    $line"
    done <<<"$found"
    fail "variable piped into grep -q (use: grep -q PATTERN <<<\"\$var\")"
  fi
}
