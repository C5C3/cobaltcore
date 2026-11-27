#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the UID-gated recovery wait of the chaos suites that kill the pods of
# a Deployment (tests/e2e-chaos/lib/pods.sh, #1390), against a stub kubectl:
#   1. pod_uids reads the UIDs of the pods the selector matches.
#   2. wait_replaced passes once no pre-kill pod is left, spec.replicas new
#      pods are Ready and the Deployment is at readyReplicas.
#   3. It refuses a readyReplicas that still holds its pre-kill value while
#      the new pods are not Ready, the window right after a gracePeriod 0 kill.
#   4. It refuses a pre-kill pod that is still there and names it.
#   5. A failed pod read fails the wait.
#
# Usage: bin/bats tests/unit/ci/chaos_pods_lib.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

LIB="$PROJECT_ROOT/tests/e2e-chaos/lib/pods.sh"

# The stub kubectl names the Deployment ctl, with STUB_DESIRED replicas and
# STUB_READY readyReplicas. The pod list is STUB_PODS, one "<uid> <Ready>"
# line per pod, and its read fails on STUB_PODS_FAIL. A test passes the four
# to run_lib, which declares their rules; a value a call leaves out reads empty.
setup() {
  stub_on kubectl '*'
  stub_on kubectl '*{.items\[0\].metadata.name}*' --out-raw 'ctl'
}

# run_lib <VAR=value...> -- <shell code>: run code with the library sourced
# under set -euo pipefail and the stub first on PATH; $output and $status
# hold the result.
run_lib() {
  local pods_fail=''
  # The empty values come first, so the rules of an earlier call give way.
  set -- STUB_DESIRED= STUB_READY= STUB_PODS= "$@"
  while [ "$1" != "--" ]; do
    case "$1" in
      STUB_DESIRED=*) stub_on kubectl '*{.spec.replicas}*' --out-raw "${1#*=}" ;;
      STUB_READY=*) stub_on kubectl '*{.status.readyReplicas}*' --out-raw "${1#*=}" ;;
      STUB_PODS=*)
        stub_on kubectl 'get pods *' --out-raw "$(printf '%s' "${1#*=}" | awk '{print $1}' | xargs)"$'\n'
        stub_on kubectl 'get pods *{range .items\[\*\]}*' --out-raw "${1#*=}"$'\n'
        ;;
      STUB_PODS_FAIL=*) pods_fail="${1#*=}" ;;
    esac
    shift
  done
  shift
  # The failed read comes before every answer of a pod read.
  if [ -n "$pods_fail" ]; then
    stub_on kubectl 'get pods *' --rc 1 --err "Error from server: the stub refuses the read"
  fi
  run with_stubs bash -c "set -euo pipefail; . '$LIB'; $1"
}

TWO_NEW_READY="$(printf 'new-1 True\nnew-2 True')"

# --- Test 1 ---------------------------------------------------------------
test_pod_uids_reads_the_uids() {
  run_lib STUB_PODS="$(printf 'old-1 True\nold-2 True')" -- 'pod_uids ns app=ctl'
  assert_eq "it prints them space-separated" "old-1 old-2" "$output"
}

# --- Test 2 ---------------------------------------------------------------
test_wait_replaced_passes_on_ready_new_pods() {
  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS="$TWO_NEW_READY" -- \
    'wait_replaced ns app=ctl "old-1 old-2" 0'
  assert_eq "it returns 0" "0" "$status"
  assert_contains "it says so" "$output" \
    "OK: ns/ctl replaced every pre-kill pod with 2 Ready pods and is at readyReplicas 2"
}

# --- Test 3 ---------------------------------------------------------------
test_wait_replaced_refuses_a_stale_ready_replicas() {
  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS="$(printf 'new-1 False\nnew-2 ')" -- \
    'wait_replaced ns app=ctl "old-1 old-2" 0'
  assert_eq "it returns 1 at the deadline" "1" "$status"
  assert_contains "it names the new pods that are not Ready" "$output" \
    "FAIL: ns/ctl 0 s after the kill: pre-kill pods left: none, new pods Ready 0 of 2, readyReplicas 2 of 2"

  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS= -- 'wait_replaced ns app=ctl "old-1 old-2" 0'
  assert_eq "no pod at all is no recovery" "1" "$status"
}

# --- Test 4 ---------------------------------------------------------------
test_wait_replaced_refuses_a_pre_kill_pod() {
  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS="$(printf 'old-1 True\nnew-2 True')" -- \
    'wait_replaced ns app=ctl "old-1 old-2" 0'
  assert_eq "it returns 1 at the deadline" "1" "$status"
  assert_contains "it names the pre-kill pod" "$output" "pre-kill pods left: old-1, new pods Ready 1 of 2"
}

# --- Test 5 ---------------------------------------------------------------
test_wait_replaced_fails_on_a_failed_read() {
  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS_FAIL=1 -- 'wait_replaced ns app=ctl "old-1 old-2" 60'
  assert_eq "it returns 1" "1" "$status"
  assert_contains "and passes the error on" "$output" "Error from server: the stub refuses the read"
  assert_not_contains "and reports no success" "$output" "OK:"
}

# --- Run ---
# Functions stay registered: as @test blocks git would lose this file's history.
bats_test_function --description "pod_uids reads the UIDs of the pods the selector matches" -- \
test_pod_uids_reads_the_uids
bats_test_function --description "wait_replaced passes once new pods replaced the old ones and are Ready" -- \
test_wait_replaced_passes_on_ready_new_pods
bats_test_function --description "wait_replaced refuses a pre-kill readyReplicas while the new pods are not Ready" -- \
test_wait_replaced_refuses_a_stale_ready_replicas
bats_test_function --description "wait_replaced refuses a pre-kill pod that is still there" -- \
test_wait_replaced_refuses_a_pre_kill_pod
bats_test_function --description "a failed pod read fails wait_replaced" -- \
test_wait_replaced_fails_on_a_failed_read
