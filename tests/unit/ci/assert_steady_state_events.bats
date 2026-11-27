#!/usr/bin/env bats
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
# Usage: bin/bats tests/unit/ci/assert_steady_state_events.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-steady-state-events.sh"
PLURAL="keystones.keystone.openstack.c5c3.io"

# The kubectl rules answer the two calls the script makes, and stub_calls
# kubectl lists every call. setup declares the answers of reset_stub, and a
# test declares the rules it changes after them, so that its own rule answers:
#   get events      STUB_EVENTS_BEFORE on the first call since the last
#                   reset_stub, STUB_EVENTS_AFTER on later ones: a lasting
#                   rule with STUB_EVENTS_AFTER and a --times 1 rule with
#                   STUB_EVENTS_BEFORE after it; or fails with Forbidden
#   get <resource>  STUB_CR on the first call, STUB_CR_LATER (default STUB_CR)
#                   on later ones, declared the same way; or fails, and an
#                   exit code of 1 prints a NotFound error
# Every other call exits 0. A sleep stub beside it returns at once, and
# stub_calls sleep lists its arguments.

# event <name> <count> <type> <reason> <message>: one snapshot line.
event() {
  printf '%s\t%s\t%s\t%s\t%s' "$@"
}

SYNCED="$(event keystone-basic.18a1 1 Normal DatabaseSynced 'Database schema is up to date')"
BOOTSTRAPPED="$(event keystone-basic.18a2 2 Normal BootstrapComplete 'Keystone bootstrap completed successfully')"

# reset_stub
# A Ready Keystone at generation 3 with two events whose counts do not move
# during a window of 0 s. Called again inside a test, it declares these rules
# anew, so they answer again and the next read is a first one; the call logs
# keep the calls made before.
reset_stub() {
  STUB_CR="0d9c-uid 3 True"
  STUB_EVENTS_BEFORE="$SYNCED"$'\n'"$BOOTSTRAPPED"
  STUB_EVENTS_AFTER="$SYNCED"$'\n'"$BOOTSTRAPPED"
  WINDOW=0
  stub_on kubectl '*'
  stub_on kubectl 'get *' --out-raw "$STUB_CR"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_AFTER"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_BEFORE" --times 1
}

setup() {
  stub_on sleep '*'
  reset_stub
}

# run_check [args...]
# Runs the script with the stubs first on PATH, through run: its stdout in
# $output, its stderr in $stderr and its exit code in $status.
# Without arguments it runs the keystone basic-deployment call. An unset
# WINDOW leaves EVENT_WINDOW_SECONDS unset.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- openstack "$PLURAL" keystone-basic
  fi
  run --separate-stderr run_script "$@"
}

# run_script [args...]
# The script with the stubs first on PATH and the window of WINDOW. run runs
# it in a subshell, so the export ends with it.
run_script() {
  if [ -n "$WINDOW" ]; then export EVENT_WINDOW_SECONDS="$WINDOW"; fi
  with_stubs bash "$SCRIPT" "$@"
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

# --- Test 1: a quiet CR passes ---
@test "a Ready CR without events, or with unchanged events, passes" {
  STUB_EVENTS_BEFORE=""
  STUB_EVENTS_AFTER=""
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_AFTER"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_BEFORE" --times 1
  run_check
  assert_eq "no events exit 0" "0" "$status"
  assert_eq "it prints five OK: lines" "5" "$(grep -c '^OK:' <<<"$output" || true)"
  assert_contains "it reports the budget" "$output" \
    "OK: $PLURAL keystone-basic gained no event in 0s"

  reset_stub
  run_check
  assert_eq "unchanged events exit 0" "0" "$status"
  assert_contains "it counts the first snapshot" "$output" \
    "OK: $PLURAL keystone-basic has 2 events before the window"
  assert_not_contains "it prints no FAIL: line" "$output" "FAIL:"
}

@test "an event present only in the first snapshot passes" {
  # The API server drops events after their TTL.
  STUB_EVENTS_AFTER="$SYNCED"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_AFTER"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_BEFORE" --times 1
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_not_contains "it prints no FAIL: line" "$output" "FAIL:"
}

# --- Test 2: events that rise ---
@test "an event whose count rises from 2 to 3 fails with its delta" {
  STUB_EVENTS_AFTER="$SYNCED"$'\n'"$(event keystone-basic.18a2 3 Normal BootstrapComplete 'Keystone bootstrap completed successfully')"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_AFTER"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_BEFORE" --times 1
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the event, its type, reason, delta and message" "$output" \
    "FAIL: event keystone-basic.18a2 Normal BootstrapComplete rose by 1: Keystone bootstrap completed successfully"
  assert_contains "it sums the budget" "$output" \
    "FAIL: $PLURAL keystone-basic gained 1 events in 0s, the budget is 0"
  assert_not_contains "the unchanged event is not named" "$output" "keystone-basic.18a1"
}

@test "a new event fails with its full count" {
  STUB_EVENTS_AFTER="$SYNCED"$'\n'"$BOOTSTRAPPED"$'\n'"$(event keystone-basic.18b0 4 Warning DBSyncFailed 'db_sync job failed: boom')"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_AFTER"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_BEFORE" --times 1
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it counts the new event in full" "$output" \
    "FAIL: event keystone-basic.18b0 Warning DBSyncFailed rose by 4: db_sync job failed: boom"
}

@test "a new event without a count fails with delta 1" {
  STUB_EVENTS_AFTER="$SYNCED"$'\n'"$BOOTSTRAPPED"$'\n'"$(event keystone-basic.18b1 '' Normal DatabaseSynced 'Database schema is up to date')"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_AFTER"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_BEFORE" --times 1
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it counts the event as 1" "$output" \
    "FAIL: event keystone-basic.18b1 Normal DatabaseSynced rose by 1: Database schema is up to date"
}

@test "a message holding a tab is printed whole, and two rises are summed" {
  STUB_EVENTS_AFTER="$(event keystone-basic.18a1 2 Normal DatabaseSynced $'first\tsecond')"$'\n'"$(event keystone-basic.18a2 4 Normal BootstrapComplete 'done')"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_AFTER"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_BEFORE" --times 1
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it keeps the message whole" "$output" "rose by 1: first"$'\t'"second"
  assert_contains "it sums both rises" "$output" "gained 3 events"
}

# --- Test 3: check 1, the CR ---
@test "a CR that is not found, or a get that errors, fails check 1" {
  stub_on kubectl "get $PLURAL *" --rc 1 --err "Error from server (NotFound): $PLURAL \"keystone-basic\" not found" --times 1
  run_check
  assert_eq "a missing CR exits 1" "1" "$status"
  assert_contains "it says the CR cannot be read" "$output" \
    "FAIL: cannot read $PLURAL keystone-basic in openstack"
  assert_contains "kubectl's own error reaches stderr" "$stderr" "NotFound"
  assert_eq "it takes no snapshot" "0" "$(stub_calls kubectl | grep -c '^get events' || true)"

  reset_stub
  stub_on kubectl "get $PLURAL *" --rc 2 --err "Error from server (InternalError): an error on the server has prevented the request from succeeding" --times 1
  run_check
  assert_eq "a failing get exits 1" "1" "$status"
  assert_contains "it says the CR cannot be read" "$output" \
    "FAIL: cannot read $PLURAL keystone-basic in openstack"
}

@test "a Ready condition that is absent or False fails check 1" {
  STUB_CR="0d9c-uid 3 "
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR"
  run_check
  assert_eq "an absent Ready exits 1" "1" "$status"
  assert_contains "it says Ready is absent" "$output" \
    "FAIL: $PLURAL keystone-basic is not Ready (Ready=absent)"

  reset_stub
  STUB_CR="0d9c-uid 3 False"
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR"
  run_check
  assert_eq "Ready=False exits 1" "1" "$status"
  assert_contains "it says Ready is False" "$output" \
    "FAIL: $PLURAL keystone-basic is not Ready (Ready=False)"
}

# --- Test 4: check 4, the CR after the window ---
@test "Ready=False, a new generation or a new UID after the window fails check 4" {
  STUB_CR_LATER="0d9c-uid 3 False"
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR_LATER"
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR" --times 1
  run_check
  assert_eq "Ready=False exits 1" "1" "$status"
  assert_contains "it says the CR changed" "$output" \
    "FAIL: $PLURAL keystone-basic changed during the window: generation 3 -> 3, Ready=False"

  reset_stub
  STUB_CR_LATER="0d9c-uid 4 True"
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR_LATER"
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR" --times 1
  run_check
  assert_eq "a new generation exits 1" "1" "$status"
  assert_contains "it names both generations" "$output" "generation 3 -> 4"

  reset_stub
  STUB_CR_LATER="77aa-uid 1 True"
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR_LATER"
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR" --times 1
  run_check
  assert_eq "a replaced CR exits 1" "1" "$status"
  assert_contains "it names both UIDs" "$output" "uid 0d9c-uid -> 77aa-uid"

  reset_stub
  stub_on kubectl "get $PLURAL *" --rc 1 --err "Error from server (NotFound): $PLURAL \"keystone-basic\" not found"
  stub_on kubectl "get $PLURAL *" --out-raw "$STUB_CR" --times 1
  run_check
  assert_eq "a CR deleted during the window exits 1" "1" "$status"
  assert_contains "it says the CR cannot be read" "$output" \
    "FAIL: cannot read $PLURAL keystone-basic in openstack after the window"
}

# --- Test 5: the snapshots ---
@test "an events list that errors fails either snapshot" {
  stub_on kubectl 'get events *' --rc 1 --err "Error from server (Forbidden): events is forbidden" --times 1
  run_check
  assert_eq "the first snapshot exits 1" "1" "$status"
  assert_contains "it names the first snapshot" "$output" \
    "FAIL: cannot list the events of $PLURAL keystone-basic before the window"

  reset_stub
  stub_on kubectl 'get events *' --rc 1 --err "Error from server (Forbidden): events is forbidden"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS_BEFORE" --times 1
  run_check
  assert_eq "the second snapshot exits 1" "1" "$status"
  assert_contains "it names the second snapshot" "$output" \
    "FAIL: cannot list the events of $PLURAL keystone-basic after the window"
}

@test "the snapshots select the CR's UID, and the window defaults to 120 s" {
  WINDOW=""
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "both snapshots select the UID" "2" \
    "$(stub_calls kubectl | grep -c -- '--field-selector involvedObject.uid=0d9c-uid ' || true)"
  assert_eq "the window is 120 s" "120" "$(stub_calls sleep)"
  assert_contains "it reports the window" "$output" "OK: waited 120s"
}

# --- Test 6: the arguments ---
@test "a missing or empty argument exits 2 with the usage line" {
  local usage="usage: assert-steady-state-events.sh <namespace> <resource> <name>"
  run_check openstack "$PLURAL"
  assert_eq "two arguments exit 2" "2" "$status"
  assert_contains "the usage line goes to stderr" "$stderr" "$usage"
  assert_eq "nothing goes to stdout" "" "$output"

  run_check openstack "" keystone-basic
  assert_eq "an empty argument exits 2" "2" "$status"
  assert_contains "the usage line goes to stderr" "$stderr" "$usage"
}

@test "the script parses" {
  bash -n "$SCRIPT"
  assert_eq "bash -n passes" "0" "$?"
}

# --- Test 7: the path filters ---
@test "an edit to the script runs every leg but c5c3" {
  local op block script=assert-steady-state-events.sh
  # These seven list the whole directory.
  for op in barbican cinder glance horizon keystone nova placement; do
    block="$(filter_block "tests_e2e_$op")"
    soft assert_contains "tests_e2e_$op lists the shared directory" "$block" "'tests/e2e/lib/**'"
  done
  # ovn and neutron name the script; c5c3 does not call it.
  for op in ovn neutron; do
    block="$(filter_block "tests_e2e_$op")"
    soft assert_contains "tests_e2e_$op names $script" "$block" "'tests/e2e/lib/$script'"
  done
  block="$(filter_block tests_e2e_c5c3)"
  assert_not_empty "tests_e2e_c5c3 exists" "$block"
  assert_not_contains "tests_e2e_c5c3 leaves $script out" "$block" "$script"
}

# --- Test 8: the callers ---
@test "the nine basic-deployment suites end with the budget, and no suite outside the tables calls it" {
  local suite args expected found calls
  while IFS='|' read -r suite args; do
    calls="$(last_step_calls "$PROJECT_ROOT/tests/e2e/$suite/chainsaw-test.yaml")"
    soft assert_eq "the $suite suite calls the script in its last step, with its tabled arguments" \
      "../../lib/assert-steady-state-events.sh $args" "${calls#*|}"
    # The script sleeps 120 s between its snapshots; chainsaw's exec default
    # is 30s.
    soft assert_eq "the $suite suite gives that step a 4m timeout" "4m" "${calls%%|*}"
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
@test "horizon/events runs the budget once before its last step" {
  local suite args
  while IFS='|' read -r suite args; do
    soft assert_eq "the $suite suite calls the script once, with its tabled arguments and a 4m timeout" \
      "4m|../../lib/assert-steady-state-events.sh $args" \
      "$(step_calls "$PROJECT_ROOT/tests/e2e/$suite/chainsaw-test.yaml")"
    soft assert_eq "the $suite suite does not call the script in its last step" \
      "" "$(last_step_calls "$PROJECT_ROOT/tests/e2e/$suite/chainsaw-test.yaml")"
  done <<<"$MID_SUITE_EVENT_CALLERS"
}
