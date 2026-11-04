#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/e2e/lib/assert-steady-state-events.sh, the zero event budget a
# Ready, unchanged CR is held to:
#   - a Ready CR without events, or with events whose counts do not move,
#     passes, and so does an event present only in the first snapshot
#   - an event whose count rises, a new event, and a new event without a
#     count fail with one line per event naming its type, reason, delta and
#     message
#   - check 1 fails on a CR that is not found, a get that errors, and a Ready
#     condition that is absent or False; check 4 on Ready=False, a changed
#     generation or a replaced CR at the second read; an events list that
#     errors fails either snapshot
#   - the events are read by the CR's UID, and the window defaults to 120 s
#   - a missing or empty argument exits 2 with the usage line
#   - an edit to the script runs the e2e-operator jobs of every operator but
#     c5c3, whose suites do not call it
#   - the nine basic-deployment suites call it in their last step with the
#     arguments of their table row and a 4m step timeout
#   - horizon/events calls it once, in a step before its last one, with the
#     arguments of its table row and a 4m step timeout, and no suite outside
#     the two tables calls it
# Usage: bash tests/unit/ci/assert_steady_state_events_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-steady-state-events.sh"
PLURAL="keystones.keystone.openstack.c5c3.io"

STUBS="$(mktemp -d)"
trap 'rm -rf "$STUBS"' EXIT

# make_kubectl_stub <dir>
# Writes a kubectl stub that answers the two calls the script makes and logs
# its arguments to <dir>/kubectl.log:
#   get events      STUB_EVENTS_BEFORE on the first call since the last
#                   reset_stub, STUB_EVENTS_AFTER on later ones, or fails with
#                   STUB_EVENTS_BEFORE_RC / STUB_EVENTS_AFTER_RC
#   get <resource>  STUB_CR on the first call, STUB_CR_LATER (default STUB_CR)
#                   on later ones, or fails with STUB_CR_RC / STUB_CR_LATER_RC;
#                   an exit code of 1 prints a NotFound error
# A sleep stub beside it logs its argument to <dir>/sleep.log and returns at
# once.
make_kubectl_stub() {
  local dir="$1"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
echo "$*" >>"$dir/kubectl.log"
case "$1 $2" in
  "get events")
    if [ -e "$dir/events-read" ]; then
      rc="${STUB_EVENTS_AFTER_RC:-0}" body="$STUB_EVENTS_AFTER"
    else
      : >"$dir/events-read"
      rc="${STUB_EVENTS_BEFORE_RC:-0}" body="$STUB_EVENTS_BEFORE"
    fi
    if [ "$rc" -ne 0 ]; then
      echo "Error from server (Forbidden): events is forbidden" >&2
      exit "$rc"
    fi
    if [ -n "$body" ]; then printf '%s\n' "$body"; fi
    ;;
  "get "*)
    if [ -e "$dir/cr-read" ]; then
      rc="${STUB_CR_LATER_RC:-0}" body="${STUB_CR_LATER:-$STUB_CR}"
    else
      : >"$dir/cr-read"
      rc="${STUB_CR_RC:-0}" body="$STUB_CR"
    fi
    if [ "$rc" -eq 1 ]; then
      echo "Error from server (NotFound): $2 \"$3\" not found" >&2
      exit 1
    fi
    if [ "$rc" -ne 0 ]; then
      echo "Error from server (InternalError): an error on the server has prevented the request from succeeding" >&2
      exit "$rc"
    fi
    printf '%s' "$body"
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
  cat >"$dir/sleep" <<'STUB'
#!/bin/bash
echo "$1" >>"$(dirname "$0")/sleep.log"
exit 0
STUB
  chmod +x "$dir/sleep"
}

# event <name> <count> <type> <reason> <message>: one snapshot line.
event() {
  printf '%s\t%s\t%s\t%s\t%s' "$@"
}

SYNCED="$(event keystone-basic.18a1 1 Normal DatabaseSynced 'Database schema is up to date')"
BOOTSTRAPPED="$(event keystone-basic.18a2 2 Normal BootstrapComplete 'Keystone bootstrap completed successfully')"

# reset_stub
# A Ready Keystone at generation 3 with two events whose counts do not move
# during a window of 0 s.
reset_stub() {
  STUB_CR="0d9c-uid 3 True"
  STUB_CR_RC=0
  STUB_CR_LATER=""
  STUB_CR_LATER_RC=0
  STUB_EVENTS_BEFORE="$SYNCED"$'\n'"$BOOTSTRAPPED"
  STUB_EVENTS_BEFORE_RC=0
  STUB_EVENTS_AFTER="$SYNCED"$'\n'"$BOOTSTRAPPED"
  STUB_EVENTS_AFTER_RC=0
  WINDOW=0
  rm -f "$STUBS/events-read" "$STUBS/cr-read" "$STUBS/kubectl.log" "$STUBS/sleep.log"
}

# run_check [args...]
# Runs the script with the stubs first on PATH. Leaves its stdout in RUN_OUT
# and its exit code in RUN_RC, and writes its stderr to $STUBS/stderr.
# Without arguments it runs the keystone basic-deployment call. An unset
# WINDOW leaves EVENT_WINDOW_SECONDS unset.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- openstack "$PLURAL" keystone-basic
  fi
  RUN_OUT="$(
    if [ -n "$WINDOW" ]; then export EVENT_WINDOW_SECONDS="$WINDOW"; fi
    PATH="$STUBS:$PATH" \
      STUB_CR="$STUB_CR" STUB_CR_RC="$STUB_CR_RC" \
      STUB_CR_LATER="$STUB_CR_LATER" STUB_CR_LATER_RC="$STUB_CR_LATER_RC" \
      STUB_EVENTS_BEFORE="$STUB_EVENTS_BEFORE" STUB_EVENTS_BEFORE_RC="$STUB_EVENTS_BEFORE_RC" \
      STUB_EVENTS_AFTER="$STUB_EVENTS_AFTER" STUB_EVENTS_AFTER_RC="$STUB_EVENTS_AFTER_RC" \
      bash "$SCRIPT" "$@" 2>"$STUBS/stderr"
  )"
  RUN_RC=$?
}

# The nine basic-deployment suites that end with the budget, each with the
# arguments it passes after the script's path.
EVENT_CALLERS="$(cat <<'CALLS'
barbican/basic-deployment|openstack barbicans.barbican.openstack.c5c3.io barbican-basic
cinder/basic-deployment|openstack cinders.cinder.openstack.c5c3.io cinder-basic
glance/basic-deployment|openstack glances.glance.openstack.c5c3.io glance-basic
horizon/basic-deployment|openstack horizons.horizon.openstack.c5c3.io horizon-basic
keystone/basic-deployment|openstack keystones.keystone.openstack.c5c3.io keystone-basic
neutron/basic-deployment|openstack neutrons.neutron.openstack.c5c3.io neutron-basic
nova/basic-deployment|openstack novas.nova.openstack.c5c3.io nova-basic
ovn/central-basic-deployment|openstack ovncentrals.ovn.openstack.c5c3.io ovn-basic
placement/basic-deployment|openstack placements.placement.openstack.c5c3.io placement-basic
CALLS
)"

# The suites that run the budget in a step before their last one, with the
# arguments they pass. horizon/events holds its extraConfig overrides in
# place during the window and clears them in its last step.
MID_SUITE_EVENT_CALLERS="$(cat <<'CALLS'
horizon/events|openstack horizons.horizon.openstack.c5c3.io horizon-events
CALLS
)"

# step_calls <file>
# The calls of the script in any step of <file>, one "<timeout>|<call>" line
# each, shaped like the lines of last_step_calls.
step_calls() {
  awk '
    /^  - try:$/ { timeout = ""; next }
    /^[[:space:]]+timeout:[[:space:]]/ { timeout = $2 }
    /^[[:space:]]*\.\.\/\.\.\/lib\/assert-steady-state-events\.sh / {
      line = $0; sub(/^[[:space:]]+/, "", line); print timeout "|" line
    }' "$1"
}

# last_step_calls <file>
# The calls of the script in the last step of <file>, one "<timeout>|<call>"
# line each: the last timeout: value since the last step opener "  - try:",
# and a line after that opener whose first text is the script's relative
# path, with the leading blanks dropped.
last_step_calls() {
  awk '
    /^  - try:$/ { n = 0; timeout = ""; delete calls; next }
    /^[[:space:]]+timeout:[[:space:]]/ { timeout = $2 }
    /^[[:space:]]*\.\.\/\.\.\/lib\/assert-steady-state-events\.sh / {
      line = $0; sub(/^[[:space:]]+/, "", line); calls[++n] = timeout "|" line
    }
    END { for (i = 1; i <= n; i++) print calls[i] }' "$1"
}

make_kubectl_stub "$STUBS"

# --- Test 1: a quiet CR passes ---
test_a_quiet_cr_passes() {
  echo "Test: a Ready CR without events, or with unchanged events, passes"

  reset_stub
  STUB_EVENTS_BEFORE=""
  STUB_EVENTS_AFTER=""
  run_check
  assert_eq "no events exit 0" "0" "$RUN_RC"
  assert_eq "it prints five OK: lines" "5" "$(grep -c '^OK:' <<<"$RUN_OUT" || true)"
  assert_contains "it reports the budget" "$RUN_OUT" \
    "OK: $PLURAL keystone-basic gained no event in 0s"

  reset_stub
  run_check
  assert_eq "unchanged events exit 0" "0" "$RUN_RC"
  assert_contains "it counts the first snapshot" "$RUN_OUT" \
    "OK: $PLURAL keystone-basic has 2 events before the window"
  assert_not_contains "it prints no FAIL: line" "$RUN_OUT" "FAIL:"
}

test_a_vanished_event_passes() {
  echo "Test: an event present only in the first snapshot passes"

  # The API server drops events after their TTL.
  reset_stub
  STUB_EVENTS_AFTER="$SYNCED"
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_not_contains "it prints no FAIL: line" "$RUN_OUT" "FAIL:"
}

# --- Test 2: events that rise ---
test_a_rising_count_fails() {
  echo "Test: an event whose count rises from 2 to 3 fails with its delta"

  reset_stub
  STUB_EVENTS_AFTER="$SYNCED"$'\n'"$(event keystone-basic.18a2 3 Normal BootstrapComplete 'Keystone bootstrap completed successfully')"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the event, its type, reason, delta and message" "$RUN_OUT" \
    "FAIL: event keystone-basic.18a2 Normal BootstrapComplete rose by 1: Keystone bootstrap completed successfully"
  assert_contains "it sums the budget" "$RUN_OUT" \
    "FAIL: $PLURAL keystone-basic gained 1 events in 0s, the budget is 0"
  assert_not_contains "the unchanged event is not named" "$RUN_OUT" "keystone-basic.18a1"
}

test_a_new_event_fails() {
  echo "Test: a new event fails with its full count"

  reset_stub
  STUB_EVENTS_AFTER="$SYNCED"$'\n'"$BOOTSTRAPPED"$'\n'"$(event keystone-basic.18b0 4 Warning DBSyncFailed 'db_sync job failed: boom')"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it counts the new event in full" "$RUN_OUT" \
    "FAIL: event keystone-basic.18b0 Warning DBSyncFailed rose by 4: db_sync job failed: boom"
}

test_a_new_event_without_count_fails() {
  echo "Test: a new event without a count fails with delta 1"

  reset_stub
  STUB_EVENTS_AFTER="$SYNCED"$'\n'"$BOOTSTRAPPED"$'\n'"$(event keystone-basic.18b1 '' Normal DatabaseSynced 'Database schema is up to date')"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it counts the event as 1" "$RUN_OUT" \
    "FAIL: event keystone-basic.18b1 Normal DatabaseSynced rose by 1: Database schema is up to date"
}

test_a_message_with_tabs_is_kept_whole() {
  echo "Test: a message holding a tab is printed whole, and two rises are summed"

  reset_stub
  STUB_EVENTS_AFTER="$(event keystone-basic.18a1 2 Normal DatabaseSynced $'first\tsecond')"$'\n'"$(event keystone-basic.18a2 4 Normal BootstrapComplete 'done')"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it keeps the message whole" "$RUN_OUT" "rose by 1: first"$'\t'"second"
  assert_contains "it sums both rises" "$RUN_OUT" "gained 3 events"
}

# --- Test 3: check 1, the CR ---
test_an_unreadable_cr_fails_check_1() {
  echo "Test: a CR that is not found, or a get that errors, fails check 1"

  reset_stub
  STUB_CR_RC=1
  run_check
  assert_eq "a missing CR exits 1" "1" "$RUN_RC"
  assert_contains "it says the CR cannot be read" "$RUN_OUT" \
    "FAIL: cannot read $PLURAL keystone-basic in openstack"
  assert_contains "kubectl's own error reaches stderr" "$(cat "$STUBS/stderr")" "NotFound"
  assert_eq "it takes no snapshot" "0" "$(grep -c '^get events' "$STUBS/kubectl.log" || true)"

  reset_stub
  STUB_CR_RC=2
  run_check
  assert_eq "a failing get exits 1" "1" "$RUN_RC"
  assert_contains "it says the CR cannot be read" "$RUN_OUT" \
    "FAIL: cannot read $PLURAL keystone-basic in openstack"
}

test_a_cr_that_is_not_ready_fails_check_1() {
  echo "Test: a Ready condition that is absent or False fails check 1"

  reset_stub
  STUB_CR="0d9c-uid 3 "
  run_check
  assert_eq "an absent Ready exits 1" "1" "$RUN_RC"
  assert_contains "it says Ready is absent" "$RUN_OUT" \
    "FAIL: $PLURAL keystone-basic is not Ready (Ready=absent)"

  reset_stub
  STUB_CR="0d9c-uid 3 False"
  run_check
  assert_eq "Ready=False exits 1" "1" "$RUN_RC"
  assert_contains "it says Ready is False" "$RUN_OUT" \
    "FAIL: $PLURAL keystone-basic is not Ready (Ready=False)"
}

# --- Test 4: check 4, the CR after the window ---
test_a_changed_cr_fails_check_4() {
  echo "Test: Ready=False, a new generation or a new UID after the window fails check 4"

  reset_stub
  STUB_CR_LATER="0d9c-uid 3 False"
  run_check
  assert_eq "Ready=False exits 1" "1" "$RUN_RC"
  assert_contains "it says the CR changed" "$RUN_OUT" \
    "FAIL: $PLURAL keystone-basic changed during the window: generation 3 -> 3, Ready=False"

  reset_stub
  STUB_CR_LATER="0d9c-uid 4 True"
  run_check
  assert_eq "a new generation exits 1" "1" "$RUN_RC"
  assert_contains "it names both generations" "$RUN_OUT" "generation 3 -> 4"

  reset_stub
  STUB_CR_LATER="77aa-uid 1 True"
  run_check
  assert_eq "a replaced CR exits 1" "1" "$RUN_RC"
  assert_contains "it names both UIDs" "$RUN_OUT" "uid 0d9c-uid -> 77aa-uid"

  reset_stub
  STUB_CR_LATER_RC=1
  run_check
  assert_eq "a CR deleted during the window exits 1" "1" "$RUN_RC"
  assert_contains "it says the CR cannot be read" "$RUN_OUT" \
    "FAIL: cannot read $PLURAL keystone-basic in openstack after the window"
}

# --- Test 5: the snapshots ---
test_a_failing_events_list_fails() {
  echo "Test: an events list that errors fails either snapshot"

  reset_stub
  STUB_EVENTS_BEFORE_RC=1
  run_check
  assert_eq "the first snapshot exits 1" "1" "$RUN_RC"
  assert_contains "it names the first snapshot" "$RUN_OUT" \
    "FAIL: cannot list the events of $PLURAL keystone-basic before the window"

  reset_stub
  STUB_EVENTS_AFTER_RC=1
  run_check
  assert_eq "the second snapshot exits 1" "1" "$RUN_RC"
  assert_contains "it names the second snapshot" "$RUN_OUT" \
    "FAIL: cannot list the events of $PLURAL keystone-basic after the window"
}

test_the_events_are_read_by_uid_over_the_window() {
  echo "Test: the snapshots select the CR's UID, and the window defaults to 120 s"

  reset_stub
  WINDOW=""
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_eq "both snapshots select the UID" "2" \
    "$(grep -c -- '--field-selector involvedObject.uid=0d9c-uid ' "$STUBS/kubectl.log" || true)"
  assert_eq "the window is 120 s" "120" "$(cat "$STUBS/sleep.log")"
  assert_contains "it reports the window" "$RUN_OUT" "OK: waited 120s"
}

# --- Test 6: the arguments ---
test_wrong_arguments_exit_2() {
  echo "Test: a missing or empty argument exits 2 with the usage line"

  local usage="usage: assert-steady-state-events.sh <namespace> <resource> <name>"
  reset_stub
  run_check openstack "$PLURAL"
  assert_eq "two arguments exit 2" "2" "$RUN_RC"
  assert_contains "the usage line goes to stderr" "$(cat "$STUBS/stderr")" "$usage"
  assert_eq "nothing goes to stdout" "" "$RUN_OUT"

  run_check openstack "" keystone-basic
  assert_eq "an empty argument exits 2" "2" "$RUN_RC"
  assert_contains "the usage line goes to stderr" "$(cat "$STUBS/stderr")" "$usage"
}

test_the_script_parses() {
  echo "Test: the script parses"

  bash -n "$SCRIPT"
  assert_eq "bash -n passes" "0" "$?"
}

# --- Test 7: the path filters ---
test_the_filters_name_the_shared_scripts() {
  echo "Test: an edit to the script runs every leg but c5c3"

  local op block script=assert-steady-state-events.sh
  # These seven list the whole directory.
  for op in barbican cinder glance horizon keystone nova placement; do
    block="$(filter_block "tests_e2e_$op")"
    assert_contains "tests_e2e_$op lists the shared directory" "$block" "'tests/e2e/lib/**'"
  done
  # ovn and neutron name the script; c5c3 does not call it.
  for op in ovn neutron; do
    block="$(filter_block "tests_e2e_$op")"
    assert_contains "tests_e2e_$op names $script" "$block" "'tests/e2e/lib/$script'"
  done
  block="$(filter_block tests_e2e_c5c3)"
  assert_not_empty "tests_e2e_c5c3 exists" "$block"
  assert_not_contains "tests_e2e_c5c3 leaves $script out" "$block" "$script"
}

# --- Test 8: the callers ---
test_the_nine_suites_end_with_the_budget() {
  echo "Test: the nine basic-deployment suites end with the budget, and no suite outside the tables calls it"

  local suite args expected found calls
  while IFS='|' read -r suite args; do
    calls="$(last_step_calls "$PROJECT_ROOT/tests/e2e/$suite/chainsaw-test.yaml")"
    assert_eq "the $suite suite calls the script in its last step, with its tabled arguments" \
      "../../lib/assert-steady-state-events.sh $args" "${calls#*|}"
    # The script sleeps 120 s between its snapshots; chainsaw's exec default
    # is 30s.
    assert_eq "the $suite suite gives that step a 4m timeout" "4m" "${calls%%|*}"
  done <<<"$EVENT_CALLERS"

  assert_eq "the table names nine suites" "9" "$(grep -c . <<<"$EVENT_CALLERS")"
  expected="$(printf '%s\n%s\n' "$EVENT_CALLERS" "$MID_SUITE_EVENT_CALLERS" |
    cut -d'|' -f1 | LC_ALL=C sort)"
  # Any line that is not a comment and names the script counts as a call.
  found="$(grep -rlE --include=chainsaw-test.yaml \
    '^[[:space:]]*[^#[:space:]].*assert-steady-state-events\.sh' "$PROJECT_ROOT/tests" |
    sed -e "s|^$PROJECT_ROOT/tests/e2e/||" -e 's|/chainsaw-test.yaml$||' | LC_ALL=C sort)"
  assert_eq "the suites that call the script are the tabled ones" "$expected" "$found"
}

# --- Test 9: the mid-suite callers ---
test_the_mid_suite_callers_hold_the_budget() {
  echo "Test: horizon/events runs the budget once before its last step"

  local suite args
  while IFS='|' read -r suite args; do
    assert_eq "the $suite suite calls the script once, with its tabled arguments and a 4m timeout" \
      "4m|../../lib/assert-steady-state-events.sh $args" \
      "$(step_calls "$PROJECT_ROOT/tests/e2e/$suite/chainsaw-test.yaml")"
    assert_eq "the $suite suite does not call the script in its last step" \
      "" "$(last_step_calls "$PROJECT_ROOT/tests/e2e/$suite/chainsaw-test.yaml")"
  done <<<"$MID_SUITE_EVENT_CALLERS"
}

# --- Run ---
test_a_quiet_cr_passes
test_a_vanished_event_passes
test_a_rising_count_fails
test_a_new_event_fails
test_a_new_event_without_count_fails
test_a_message_with_tabs_is_kept_whole
test_an_unreadable_cr_fails_check_1
test_a_cr_that_is_not_ready_fails_check_1
test_a_changed_cr_fails_check_4
test_a_failing_events_list_fails
test_the_events_are_read_by_uid_over_the_window
test_wrong_arguments_exit_2
test_the_script_parses

test_the_filters_name_the_shared_scripts
test_the_nine_suites_end_with_the_budget
test_the_mid_suite_callers_hold_the_budget

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
