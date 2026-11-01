#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify how the e2e suites stage the OpenBao root token in openbao-0 (#1580).
#
# containerd closes an exec's stdin on the first EOF of its stdout or stderr. A
# `sh -c` program whose last command is `cat > file` replaces the shell, and the
# redirect drops the only holder of stdout. The token can then be lost before
# cat reads it: the exec exits 0, the file stays empty, and every later bao call
# of the suite sends an empty token. The staging form keeps the shell alive
# until cat ends and fails a short file. This test holds three things:
#
#   - no `sh -c` program under tests, hack or deploy ends on a redirected cat;
#   - every suite that stages the token uses the form and names its failure;
#   - the form keeps a full token in a file of mode 600 and exits 3 on a short
#     one, and only an exit 1 on a pod with no running container can SKIP a suite.
#
# Usage: bash tests/unit/ci/e2e_openbao_token_staging_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# `cat >` or `cat >>` as the last command of a `sh -c` program, in either
# quoting, with or without a space after the redirect and with any other shell
# flag ahead of the -c.
SCAN_REGEX="sh -[a-z]*c ['\"].*cat >>? *(\"[^\"]*\"|[^ ;'\"]+)['\"]"
# shellcheck disable=SC2016 # literal shell source, not expanded here
STAGING_PROGRAM='umask 077; cat > "$1"; [ "$(wc -c < "$1")" -eq "$2" ] || exit 3'
STAGING_ERROR='could not stage the OpenBao root token in openbao-0'
SHORT_FILE_ERROR="ERROR: $STAGING_ERROR: the staged file has the wrong size."
KILLED_EXEC_ERROR="ERROR: $STAGING_ERROR: the exec exited 137."
UNPROVEN_EXEC_ERROR="ERROR: $STAGING_ERROR: the exec exited 1 and the pod is not known to be down."
RUNNING_READ="-o jsonpath='{.status.containerStatuses[*].state.running}'"
RUNNING_STATE='{"startedAt":"2026-11-01T18:58:59Z"}'
EXPECTED_SITES=8
TOKEN='token0123'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# staging_suites — every file under tests/, outside tests/unit, that holds a
# token file and feeds a `kubectl exec -i`, one path per line.
staging_suites() {
  local file
  while IFS= read -r file; do
    if grep -qF -- 'exec -i' "$file"; then
      echo "$file"
    fi
  done < <(grep -rlF --exclude-dir=unit -- '.bao-token' "$PROJECT_ROOT/tests")
}

# run_staging <suite> <exec_exit> [<running> [<get_exit>]] — run the suite's
# staging lines under its shell options, with the skip_or_fail of a job that does
# not require the stack. kubectl exec drains stdin and exits <exec_exit>; kubectl
# get prints <running> and exits <get_exit>. Prints what the lines print and
# CONTINUED when the suite would go on.
run_staging() {
  local file="$1" exec_exit="$2" running="${3:-}" get_exit="${4:-0}"
  (
    set -euo pipefail
    kubectl() {
      case "$1" in
        exec)
          cat >/dev/null
          return "$exec_exit"
          ;;
        get)
          printf '%s' "$running"
          return "$get_exit"
          ;;
      esac
    }
    skip_or_fail() {
      echo "SKIP: $*"
      exit 0
    }
    # shellcheck disable=SC2034 # read by the suite's lines in the eval below
    BAO_TOKEN="$TOKEN" BAO_NS=shared-services TOKEN_FILE=/tmp/t BAO_TOKEN_FILE=/tmp/t
    eval "$(sed -n '/^ *stage_rc=0$/,/^ *esac$/p' "$file")"
    echo "CONTINUED"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# The scan has to catch these spellings of the race and spare the staging form,
# or the repo-wide scan below passes vacuously. It reads one line at a time, so
# a program that spans lines stays out of its reach.
test_detector_flags_the_old_forms_and_spares_the_new_one() {
  echo "Test: the scan flags a redirected cat as the last command and spares the form"

  local tmp hits
  tmp="$(mktemp)"
  trap 'rm -f "$tmp"' RETURN
  # shellcheck disable=SC2016 # literal shell source, not expanded here
  printf '%s\n' \
    'sh -c '\''umask 077; cat > "$1"'\'' _ "${TOKEN_FILE}"; then' \
    'sh -c '\''umask 077; cat > /tmp/.bao-token'\''; then' \
    'sh -c '\''umask 077; cat > "${BAO_TOKEN_FILE}"'\''; then' \
    'sh -c "umask 077; cat > $TOKEN_FILE"' \
    'sh -c '\''umask 077; cat >"$1"'\'' _ "${TOKEN_FILE}"' \
    'sh -c '\''umask 077; cat >> "$1"'\'' _ "${TOKEN_FILE}"' \
    'sh -ec '\''umask 077; cat > "$1"'\'' _ "${TOKEN_FILE}"' \
    'sh -c '\''mkdir -p "$(dirname "$1")"; cat > "$1"'\'' _ "${TOKEN_FILE}"' \
    'sh -c '\'"$STAGING_PROGRAM"\'' _ /tmp/.bao-token "${#BAO_TOKEN}" || stage_rc=$?' \
    >"$tmp"

  hits="$(grep -nE -- "$SCAN_REGEX" "$tmp" | cut -d: -f1 | tr '\n' ' ')"

  assert_eq "flags the race spellings alone" "1 2 3 4 5 6 7 8 " "$hits"
}

test_no_sh_c_program_ends_on_a_redirected_cat() {
  echo "Test: no sh -c program under tests, hack or deploy ends on a redirected cat"

  local found exit_code
  found="$(cd "$PROJECT_ROOT" && grep -rnE --include='*.yaml' --include='*.sh' \
    --exclude-dir=unit -- "$SCAN_REGEX" tests hack deploy)"
  exit_code=$?

  # Exit 1 is grep's "searched and found nothing"; exit 2 is an error, which
  # must not read as a clean tree.
  assert_eq "grep searched the tree and matched nothing" "1" "$exit_code"
  assert_eq "no line ends a sh -c program on a redirected cat" "" "$found"
}

test_every_staging_suite_carries_the_form() {
  echo "Test: every suite that stages the token uses the form and names its failure"

  local suites file name opener
  suites="$(staging_suites)"

  assert_eq "the inventory holds the $EXPECTED_SITES known suites" "$EXPECTED_SITES" \
    "$(grep -c . <<<"$suites")"

  while IFS= read -r file; do
    name="${file#"$PROJECT_ROOT"/}"
    assert_file_contains_fixed "$name carries the staging program" \
      "$file" "$STAGING_PROGRAM"
    assert_file_contains_fixed "$name names a failed staging" \
      "$file" "$STAGING_ERROR"
    opener="$(grep -B1 -F -- "$STAGING_PROGRAM" "$file")"
    assert_contains "$name feeds the program from printf" "$opener" "printf '%s'"
    assert_contains "$name opens the exec on that line" "$opener" "| kubectl exec -i"
  done <<<"$suites"
}

# A refused exec is the only failure that can SKIP, and exit 1 alone does not
# prove one: kubectl also exits 1 for a stream lost mid-exec and for an API error.
# So exit 1 SKIPs only when a read of the pod succeeds and shows no running
# container. Any other code comes from a pod that took the exec: 3 is a short
# file, 137 a container killed with the token in flight. messaging reaches its
# staging on a job without the stack, where a SKIP would drop every assertion
# with the leg green.
test_only_a_refused_exec_can_skip() {
  echo "Test: a refused exec keeps its SKIP, any other exec failure stops a suite"

  local file name output exit_code
  while IFS= read -r file; do
    grep -qF -- "skip_or_fail \"$STAGING_ERROR" "$file" || continue
    name="${file#"$PROJECT_ROOT"/}"

    output="$(run_staging "$file" 3)"
    exit_code=$?
    assert_eq "$name exits 1 on a short file" "1" "$exit_code"
    assert_contains "$name names the short file" "$output" "$SHORT_FILE_ERROR"

    output="$(run_staging "$file" 137)"
    exit_code=$?
    assert_eq "$name exits 1 on a killed exec" "1" "$exit_code"
    assert_contains "$name names the remote exit" "$output" "$KILLED_EXEC_ERROR"

    assert_file_contains_fixed "$name reads whether a container runs" \
      "$file" "$RUNNING_READ"

    output="$(run_staging "$file" 1 "$RUNNING_STATE")"
    exit_code=$?
    assert_eq "$name exits 1 on an exec lost on a running container" "1" "$exit_code"
    assert_contains "$name names the unproven refusal" "$output" "$UNPROVEN_EXEC_ERROR"

    output="$(run_staging "$file" 1 "" 1)"
    exit_code=$?
    assert_eq "$name exits 1 when the read of the pod fails" "1" "$exit_code"
    assert_contains "$name names the failed read" "$output" "$UNPROVEN_EXEC_ERROR"

    output="$(run_staging "$file" 1)"
    exit_code=$?
    assert_eq "$name exits 0 on a refused exec" "0" "$exit_code"
    assert_contains "$name skips a refused exec" "$output" "SKIP: $STAGING_ERROR"

    output="$(run_staging "$file" 0)"
    exit_code=$?
    assert_eq "$name exits 0 on a staged token" "0" "$exit_code"
    assert_contains "$name goes on after a staged token" "$output" "CONTINUED"
  done < <(staging_suites)
}

test_program_keeps_a_full_token() {
  echo "Test: a full token exits 0 and reaches a file of mode 600 unchanged"

  local tmp exit_code
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  printf '%s' "$TOKEN" | sh -c "$STAGING_PROGRAM" _ "$tmp/token" "${#TOKEN}"
  exit_code=$?

  assert_eq "the program exits 0" "0" "$exit_code"
  assert_eq "the file holds the token" "$TOKEN" "$(cat "$tmp/token")"
  assert_eq "the file has mode 600" "600" "$(stat -c '%a' "$tmp/token")"
}

test_program_fails_on_empty_stdin() {
  echo "Test: an empty stdin exits 3"

  local tmp exit_code
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  sh -c "$STAGING_PROGRAM" _ "$tmp/token" "${#TOKEN}" </dev/null
  exit_code=$?

  assert_eq "the program exits 3" "3" "$exit_code"
}

test_program_fails_on_a_short_token() {
  echo "Test: a token one byte short exits 3"

  local tmp exit_code
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  printf '%s' "${TOKEN%?}" | sh -c "$STAGING_PROGRAM" _ "$tmp/token" "${#TOKEN}"
  exit_code=$?

  assert_eq "the program exits 3" "3" "$exit_code"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_detector_flags_the_old_forms_and_spares_the_new_one
test_no_sh_c_program_ends_on_a_redirected_cat
test_every_staging_suite_carries_the_form
test_only_a_refused_exec_can_skip
test_program_keeps_a_full_token
test_program_fails_on_empty_stdin
test_program_fails_on_a_short_token

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
