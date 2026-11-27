#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the lock check of the chaos suite cinder-nfs-outage
# (tests/e2e-chaos/cinder-nfs-outage, #1245):
#   1. Step 2 starts the lock holder of 02-lock-holder-pod.yaml and prints
#      LOCK-HELD-OK; its cleanup deletes the holder and its file. The holder
#      is a bare Pod whose name the catch block dumps, and it mounts the share
#      hard, as Nova does.
#   2. Step 5 holds exactly one script that prints LOCK-KEPT-OK, right after
#      the script that brings the server back.
#   3. That script, run with bash against a stub kubectl and a stub sleep,
#      passes when the holder's log grows by three ok lines and the pod is
#      Running, and deletes the holder. It fails without a delete, with its
#      own message, when the log cannot be read, holds no ok line, does not
#      grow within its 90 rounds, gets an error line, or the pod is not
#      Running.
#
# The script reads the cluster through kubectl alone, so the stub answers
# `logs` from a file that grows by one line per call, `get` with a phase, and
# records `delete`. A test that needs yq skips without it.
#
# Usage: bin/bats tests/unit/ci/cinder_nfs_outage_lock_check.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

SUITE_DIR="$PROJECT_ROOT/tests/e2e-chaos/cinder-nfs-outage"
SUITE="$SUITE_DIR/chainsaw-test.yaml"
HOLDER="$SUITE_DIR/02-lock-holder-pod.yaml"

# script_index <step> <text> — the positions in the try list of step <step>
# (0-based) of the scripts whose content holds <text>, one per line.
script_index() {
  yq -r ".spec.steps[$1].try | to_entries | .[] | select(.value | has(\"script\"))
    | select(.value.script.content | contains(\"$2\")) | .key" "$SUITE"
}

# lock_check — the content of the one script of the suite that prints
# LOCK-KEPT-OK; empty when there is none or more than one.
lock_check() {
  local count
  count="$(yq -r '[.spec.steps[].try[] | select(has("script")) | .script.content | select(contains("LOCK-KEPT-OK"))] | length' "$SUITE")"
  [ "$count" = 1 ] || return 0
  yq -r '.spec.steps[].try[] | select(has("script")) | .script.content | select(contains("LOCK-KEPT-OK"))' "$SUITE"
}

# write_stubs — a kubectl and a sleep for run_check, with their files in
# $STUB_DIR/state.
#   kubectl logs: fails with $STUB_LOGS_ERROR on stderr when it is set;
#     otherwise prints log.base and, on its n-th call, the first n-1 lines of
#     log.more.
#   kubectl get: prints $STUB_PHASE.
#   kubectl delete: appends its arguments to deletes.
#   sleep: returns at once.
# run_check declares the logs and get answers, those of STUB_LOGS_ERROR and
# STUB_PHASE included, for each run.
write_stubs() {
  stub_on kubectl '*' --exec 'echo "stub kubectl: unexpected call: $*" >&2; exit 1'
  stub_on kubectl 'delete *' --exec 'echo "$*" >>"$STUB_DIR/state/deletes"'
  stub_on sleep '*'
}

# run_check <dir> <base log> <lines added per logs call> [VAR=value...]
# Runs the lock check with the stubs first on PATH. Prints its output and
# returns its exit status.
run_check() {
  local dir="$1" base="$2" more="$3" arg
  shift 3
  printf '%s' "$base" >"$dir/log.base"
  printf '%s' "$more" >"$dir/log.more"
  echo 0 >"$dir/logs.calls"
  : >"$dir/deletes"
  stub_on kubectl 'logs *' --exec '
n=$(( $(cat "$STUB_DIR/state/logs.calls") + 1 ))
echo "$n" >"$STUB_DIR/state/logs.calls"
cat "$STUB_DIR/state/log.base"
head -n $(( n - 1 )) "$STUB_DIR/state/log.more"'
  stub_on kubectl 'get *' --out Running
  for arg in "$@"; do
    case "$arg" in
      STUB_LOGS_ERROR=*) stub_on kubectl 'logs *' --rc 1 --err "${arg#*=}" ;;
      STUB_PHASE=*) stub_on kubectl 'get *' --out "${arg#*=}" ;;
    esac
  done
  with_stubs env NAMESPACE=openstack bash -c "$CHECK" 2>&1
}

# probe_lines <result> <first> <last> — holder log lines, as the holder prints
# them.
probe_lines() {
  local i
  for ((i = $2; i <= $3; i++)); do
    printf 'probe 12:00:%02d %s %d\n' "$i" "$1" "$i"
  done
}

# --- Test 1: step 2 starts the holder, and its cleanup removes it ---
@test "step 2 starts the lock holder and its cleanup deletes it and its file" {
  require_tool yq

  local start cleanup
  start="$(yq -r '.spec.steps[1].try[] | select(has("script")) | .script.content | select(contains("LOCK-HELD-OK"))' "$SUITE")"
  assert_contains "a step 2 script applies the holder" "$start" "kubectl apply -f 02-lock-holder-pod.yaml"
  assert_contains "it waits for the third ok line" "$start" "' ok 3\$'"
  assert_contains "it prints LOCK-HELD-OK" "$start" 'echo "LOCK-HELD-OK"'
  assert_contains "it fails when no third ok line comes" "$start" \
    "FAIL: the lock holder wrote no third ok line within 120s"

  cleanup="$(yq -r '.spec.steps[1].cleanup[].script.content' "$SUITE")"
  assert_contains "the step 2 cleanup deletes the holder" "$cleanup" \
    'kubectl delete pod cinder-nfs-chaos-probe-lock -n "$NAMESPACE" --ignore-not-found --wait=false'
  assert_contains "the step 2 cleanup removes the holder's file from the export" "$cleanup" \
    'rm -f /exports/volumes/chaos-lock-holder.img'

  # The catch block dumps the logs of every pod with this prefix.
  assert_starts_with "the holder's name carries the prefix the catch block dumps" \
    "$(yq -r '.metadata.name' "$HOLDER")" "cinder-nfs-chaos-probe-"
  assert_eq "the holder is a bare Pod that is never restarted" "Pod Never" \
    "$(yq -r '.kind + " " + .spec.restartPolicy' "$HOLDER")"
  assert_contains "the holder mounts the share hard, as Nova does" \
    "$(yq -r '.spec.volumes[0].csi.volumeAttributes.mountOptions' "$HOLDER")" "hard"
}

# --- Test 2: step 5 checks the lock right after the server is back ---
@test "step 5 holds one lock check, right after the server is back" {
  require_tool yq

  assert_not_empty "the suite holds exactly one script that prints LOCK-KEPT-OK" "$(lock_check)"
  local back lock
  back="$(script_index 4 'OK: the NFS server is back')"
  lock="$(script_index 4 'LOCK-KEPT-OK')"
  if [[ "$back" =~ ^[0-9]+$ && "$lock" =~ ^[0-9]+$ && "$lock" -eq $((back + 1)) ]]; then
    check_passed "the lock check is the step 5 script right after the one that brings the server back"
  else
    fail "the lock check sits at '$lock' in step 5, the server's return at '$back'"
  fi
}

# --- Test 3: the lock check, against a stub kubectl ---
@test "the lock check passes on three new ok lines and fails with its own message otherwise" {
  require_tool yq
  CHECK="$(lock_check)"
  if [ -z "$CHECK" ]; then
    fail "no single LOCK-KEPT-OK script to run (19 checks)"
  fi

  local tmp out rc base
  write_stubs
  tmp="$STUB_DIR/state"
  base="holder: lock held"$'\n'"$(probe_lines ok 1 40)"$'\n'

  rc=0
  out="$(run_check "$tmp" "$base" "$(probe_lines ok 41 60)"$'\n')" || rc=$?
  assert_eq "three new ok lines pass" "0" "$rc"
  assert_contains "the pass prints LOCK-KEPT-OK" "$out" "LOCK-KEPT-OK"
  assert_eq "the pass deletes the holder once and waits for it" \
    "delete pod cinder-nfs-chaos-probe-lock -n openstack --wait=true --timeout=60s" "$(cat "$tmp/deletes")"

  rc=0
  out="$(run_check "$tmp" "$base" "" \
    STUB_LOGS_ERROR='Error from server (NotFound): pods "cinder-nfs-chaos-probe-lock" not found')" || rc=$?
  assert_eq "a failed log read fails" "1" "$rc"
  assert_contains "it names kubectl's error" "$out" \
    "FAIL: cannot read the lock holder's log: Error from server (NotFound): pods \"cinder-nfs-chaos-probe-lock\" not found"
  assert_eq "it deletes nothing" "" "$(cat "$tmp/deletes")"

  rc=0
  out="$(run_check "$tmp" "holder: lock held"$'\n' "$(probe_lines ok 1 20)"$'\n')" || rc=$?
  assert_eq "a log without an ok line fails" "1" "$rc"
  assert_contains "it says so" "$out" "FAIL: the lock holder wrote no ok line before the outage"
  assert_eq "it deletes nothing" "" "$(cat "$tmp/deletes")"

  rc=0
  out="$(run_check "$tmp" "$base" "")" || rc=$?
  assert_eq "a log that never grows fails" "1" "$rc"
  assert_contains "it names the bound" "$out" \
    "FAIL: the lock holder wrote no new ok line within 180s of the server's return"
  assert_eq "it read the log once per round, 90 rounds after the first read" "91" "$(cat "$tmp/logs.calls")"
  assert_eq "it deletes nothing" "" "$(cat "$tmp/deletes")"

  rc=0
  out="$(run_check "$tmp" "$base" \
    "$(probe_lines ok 41 41)"$'\n'"$(probe_lines 'error errno=5 Input/output error' 42 60)"$'\n')" || rc=$?
  assert_eq "an error line fails" "1" "$rc"
  assert_contains "it quotes the first error line" "$out" \
    "FAIL: the lock holder lost its lock: probe 12:00:42 error errno=5 Input/output error 42"
  assert_eq "it deletes nothing" "" "$(cat "$tmp/deletes")"

  rc=0
  out="$(run_check "$tmp" "$base" "$(probe_lines ok 41 60)"$'\n' STUB_PHASE=Failed)" || rc=$?
  assert_eq "a holder that is not Running fails" "1" "$rc"
  assert_contains "it names the phase" "$out" "FAIL: the lock holder is Failed, not Running"
  assert_eq "it deletes nothing" "" "$(cat "$tmp/deletes")"
}

# --- Run ---
