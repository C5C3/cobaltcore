#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify how hack/tempest/run-tests.sh reads the exit codes of
# keystone-inventory.py with TEMPEST_KEYSTONE_LEAK_CHECK=fail:
#   - a failed snapshot before the run exits 1 and starts no phase;
#   - a failed snapshot after the run fails the run as a failed check;
#   - an empty diff passes;
#   - a diff with lines fails the run, prints their count and the lines, and
#     copies tempest.log to the output directory;
#   - a diff that exits 1 with an empty leftovers file (a Python traceback)
#     or exits 2 fails the run as a failed check, not as a leak;
#   - a leftover fails a run whose failures the retry resolved;
#   - the check's files of an earlier run in a reused output directory are
#     gone;
#   - with off the script never calls the helper.
#
# Each case runs a copy of run-tests.sh whose /etc/tempest, /output and /tmp
# point below the case's directory, with stub tempest, stestr,
# subunit2pyunit, subunit2junitxml and python3 commands first on PATH. The
# python3 stub runs the staged helpers with bash. The keystone-inventory.py
# stub exits $STUB_BEFORE_RC or $STUB_AFTER_RC for snapshot; for diff it
# writes the lines of $STUB_TSV to --out and exits $STUB_DIFF_RC, and, like a
# crash, it writes no file when it exits non-zero with an empty $STUB_TSV.
# subunit2junitxml reports one test with $STUB_FAILURES failures,
# extract-failed.py names one test and merge-retry-junit.py rewrites the
# report to 0 failures.
#
# Usage: bash tests/unit/hack/tempest_leak_check_gate_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RUNNER="$PROJECT_ROOT/hack/tempest/run-tests.sh"
BASE_PATH="$PATH"
BASH_BIN="$(command -v bash)"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

LEFTOVERS="$(printf 'domain\td1\ttempest-domain\nrole\tr1\ttempest-role')"
TEMPEST_LOG="Request (RoleTest:test_identity_delete_role): 201 POST http://localhost:5000/v3/roles"

# --- Stubs on PATH ----------------------------------------------------------
STUBS="$TMP_DIR/stubs"
mkdir -p "$STUBS"
cat >"$STUBS/tempest" <<'STUB'
#!/bin/bash
mkdir -p etc
STUB
cat >"$STUBS/stestr" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$CASE/stestr.log"
exit "${STUB_STESTR_RC:-0}"
STUB
cat >"$STUBS/subunit2pyunit" <<'STUB'
#!/bin/bash
cat
STUB
cat >"$STUBS/subunit2junitxml" <<'STUB'
#!/bin/bash
cat >/dev/null
printf '<testsuite errors="0" failures="%s" name="" tests="1" time="0">\n</testsuite>\n' \
  "${STUB_FAILURES:-0}"
STUB
cat >"$STUBS/python3" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$CASE/python3.log"
exec bash "$@"
STUB
chmod +x "$STUBS/tempest" "$STUBS/stestr" "$STUBS/subunit2pyunit" \
  "$STUBS/subunit2junitxml" "$STUBS/python3"

# --- Helpers staged next to tempest.conf ------------------------------------
HELPERS="$TMP_DIR/helpers"
mkdir -p "$HELPERS"
cat >"$HELPERS/keystone-inventory.py" <<'STUB'
command=$1
out=
while [ $# -gt 0 ]; do
  if [ "$1" = --out ]; then
    out=$2
  fi
  shift
done
case "$command" in
  snapshot)
    case "$out" in
      *before*) rc=${STUB_BEFORE_RC:-0} ;;
      *) rc=${STUB_AFTER_RC:-0} ;;
    esac
    if [ "$rc" -eq 0 ]; then
      echo '{}' >"$out"
    fi
    exit "$rc"
    ;;
  diff)
    rc=${STUB_DIFF_RC:-0}
    if [ -z "${STUB_TSV:-}" ] && [ "$rc" -ne 0 ]; then
      echo 'Traceback (most recent call last):' >&2
      exit "$rc"
    fi
    # Every line the real helper writes ends in a newline.
    if [ -n "${STUB_TSV:-}" ]; then
      printf '%s\n' "$STUB_TSV" | tee "$out"
    else
      : >"$out"
    fi
    exit "$rc"
    ;;
esac
exit 2
STUB
cat >"$HELPERS/extract-failed.py" <<'STUB'
echo '^tempest\.api\.identity\.v3\.test_roles\.RolesV3TestJSON\.test_role_create$'
STUB
cat >"$HELPERS/merge-retry-junit.py" <<'STUB'
printf '<testsuite errors="0" failures="0" name="" tests="1" time="0">\n</testsuite>\n' >"$1"
STUB

# new_case <name>
# Lays out a case directory with the copied runner, tempest.conf, both phase
# files, the three helpers and a tempest.log under its tmp. Sets CASE_DIR.
new_case() {
  CASE_DIR="$TMP_DIR/case-$1"
  mkdir -p "$CASE_DIR/etc/tempest/phases" "$CASE_DIR/output" \
    "$CASE_DIR/tmp/tempest-logs"
  # One pass, so a root that itself lies below /tmp is not moved again.
  sed -E "s#(/etc/tempest|/output|/tmp)#${CASE_DIR}\\1#g" "$RUNNER" \
    >"$CASE_DIR/run-tests.sh"
  : >"$CASE_DIR/etc/tempest/tempest.conf"
  echo 'tempest.api.identity' >"$CASE_DIR/etc/tempest/phases/phase-1-core.txt"
  echo 'keystone_tempest_plugin.tests' \
    >"$CASE_DIR/etc/tempest/phases/phase-2-plugin.txt"
  cp "$HELPERS"/*.py "$CASE_DIR/etc/tempest/"
  printf '%s\n' "$TEMPEST_LOG" >"$CASE_DIR/tmp/tempest-logs/tempest.log"
  : >"$CASE_DIR/stestr.log"
  : >"$CASE_DIR/python3.log"
}

# run_case <env assignment>...
# Runs the case's runner with the leak check on, the stubs first on PATH and
# the given variables. Sets OUTPUT and RC.
run_case() {
  OUTPUT=$(cd "$CASE_DIR" && env -i PATH="$STUBS:$BASE_PATH" CASE="$CASE_DIR" \
    TEMPEST_CONCURRENCY=1 TEMPEST_KEYSTONE_LEAK_CHECK=fail "$@" \
    "$BASH_BIN" "$CASE_DIR/run-tests.sh" 2>&1)
  RC=$?
}

# inventory_calls
# Prints the subcommands the case's runner called keystone-inventory.py with.
inventory_calls() {
  grep -F keystone-inventory.py "$CASE_DIR/python3.log" | awk '{print $2}' |
    tr '\n' ' '
}

# exists <path>
exists() {
  [ -e "$1" ] && echo yes || echo no
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_failed_before_snapshot_starts_no_test() {
  echo "Test: a failed snapshot before the run exits 1 and starts no phase"

  new_case before
  run_case STUB_BEFORE_RC=2

  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it says no test was started" "$OUTPUT" \
    "ERROR: Keystone inventory before the run failed (exit 2); no test was started."
  assert_eq "stestr never ran" "" "$(cat "$CASE_DIR/stestr.log")"
  assert_eq "the helper ran once" "snapshot " "$(inventory_calls)"
}

test_failed_after_snapshot_is_a_failed_check() {
  echo "Test: a failed snapshot after the run fails the run as a failed check"

  new_case after
  local name
  for name in keystone-inventory-before.json keystone-inventory-after.json \
    keystone-leftovers.tsv tempest.log; do
    echo stale >"$CASE_DIR/output/$name"
  done
  run_case STUB_AFTER_RC=2

  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it reports the failed check" "$OUTPUT" \
    "ERROR: Keystone leak check failed (exit 2); the run proves nothing about leftovers."
  assert_not_contains "it reports no leak" "$OUTPUT" "Tempest left"
  assert_eq "the helper ran two snapshots and no diff" "snapshot snapshot " \
    "$(inventory_calls)"
  assert_eq "the stale keystone-leftovers.tsv is gone" "no" \
    "$(exists "$CASE_DIR/output/keystone-leftovers.tsv")"
  assert_eq "the stale keystone-inventory-after.json is gone" "no" \
    "$(exists "$CASE_DIR/output/keystone-inventory-after.json")"
  assert_eq "the stale tempest.log is gone" "no" \
    "$(exists "$CASE_DIR/output/tempest.log")"
  assert_eq "the before snapshot is this run's" "{}" \
    "$(cat "$CASE_DIR/output/keystone-inventory-before.json")"
}

test_empty_diff_passes() {
  echo "Test: an empty diff passes, and an earlier run's tempest.log is gone"

  new_case clean
  echo stale >"$CASE_DIR/output/tempest.log"
  run_case

  assert_eq "it exits 0" "0" "$RC"
  assert_contains "it reports a clean run" "$OUTPUT" \
    "Keystone leak check: the run left no Keystone object behind."
  assert_not_contains "it prints no error" "$OUTPUT" "ERROR: "
  assert_eq "the helper ran two snapshots and a diff" "snapshot snapshot diff " \
    "$(inventory_calls)"
  assert_eq "the stale tempest.log is gone" "no" \
    "$(exists "$CASE_DIR/output/tempest.log")"
}

test_leftovers_fail_the_run() {
  echo "Test: a diff with lines fails the run and copies tempest.log"

  new_case leak
  run_case STUB_DIFF_RC=1 "STUB_TSV=$LEFTOVERS"

  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it counts the leftovers" "$OUTPUT" \
    "ERROR: Tempest left 2 Keystone object(s) behind; see keystone-leftovers.tsv:"
  assert_contains "it prints the leftovers" "$OUTPUT" "$LEFTOVERS"
  assert_not_contains "it reports no failed check" "$OUTPUT" "leak check failed"
  assert_eq "tempest.log is copied to the output directory" "$TEMPEST_LOG" \
    "$(cat "$CASE_DIR/output/tempest.log" 2>/dev/null)"
}

test_helper_failure_is_not_a_leak() {
  echo "Test: a diff that crashes or exits 2 fails the run as a failed check"

  local diff_rc
  for diff_rc in 1 2; do
    new_case "diff-$diff_rc"
    run_case "STUB_DIFF_RC=$diff_rc"

    assert_eq "diff exit $diff_rc: it exits 1" "1" "$RC"
    assert_contains "diff exit $diff_rc: it reports the failed check" "$OUTPUT" \
      "ERROR: Keystone leak check failed (exit $diff_rc); the run proves nothing about leftovers."
    assert_not_contains "diff exit $diff_rc: it reports no leak" "$OUTPUT" \
      "Tempest left"
  done
}

test_leftover_fails_a_retry_resolved_run() {
  echo "Test: a leftover fails a run whose failures the retry resolved"

  new_case retry-clean
  run_case STUB_FAILURES=1 STUB_STESTR_RC=1

  assert_contains "the retry runs" "$OUTPUT" \
    "Tempest retry: 1 failed test(s), rerunning serially"
  assert_eq "without a leftover the resolved run exits 0" "0" "$RC"

  new_case retry-leak
  run_case STUB_FAILURES=1 STUB_STESTR_RC=1 STUB_DIFF_RC=1 "STUB_TSV=$LEFTOVERS"

  assert_contains "the retry runs" "$OUTPUT" \
    "Tempest retry: 1 failed test(s), rerunning serially"
  assert_not_contains "the retry resolved every failure" "$OUTPUT" \
    "Tempest reported test failures"
  assert_eq "with a leftover the resolved run exits 1" "1" "$RC"
}

test_off_never_calls_the_helper() {
  echo "Test: with off the script runs without a staged helper"

  new_case off
  rm "$CASE_DIR/etc/tempest/keystone-inventory.py"
  run_case TEMPEST_KEYSTONE_LEAK_CHECK=off

  assert_eq "it exits 0" "0" "$RC"
  assert_eq "the helper is never called" "" "$(inventory_calls)"
  assert_not_contains "it prints no leak check group" "$OUTPUT" \
    "Keystone leak check"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_failed_before_snapshot_starts_no_test
test_failed_after_snapshot_is_a_failed_check
test_empty_diff_passes
test_leftovers_fail_the_run
test_helper_failure_is_not_a_leak
test_leftover_fails_a_retry_resolved_run
test_off_never_calls_the_helper

echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
