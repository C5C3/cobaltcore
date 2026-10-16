#!/bin/bash
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
# Usage: bash tests/unit/ci/chaos_pods_lib_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LIB="$PROJECT_ROOT/tests/e2e-chaos/lib/pods.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
mkdir -p "$TMP_DIR/bin"

# The stub kubectl names the Deployment ctl, with STUB_DESIRED replicas and
# STUB_READY readyReplicas. The pod list is STUB_PODS, one "<uid> <Ready>"
# line per pod, and its read fails on STUB_PODS_FAIL.
cat >"$TMP_DIR/bin/kubectl" <<'SH'
#!/bin/bash
case "$*" in
  "get pods "*)
    if [ -n "${STUB_PODS_FAIL:-}" ]; then
      echo "Error from server: the stub refuses the read" >&2
      exit 1
    fi
    case "$*" in
      *"{range .items[*]}"*) printf '%s\n' "${STUB_PODS:-}" ;;
      *) printf '%s' "${STUB_PODS:-}" | awk '{print $1}' | xargs ;;
    esac
    ;;
  *"{.items[0].metadata.name}"*) printf 'ctl' ;;
  *"{.spec.replicas}"*) printf '%s' "${STUB_DESIRED:-}" ;;
  *"{.status.readyReplicas}"*) printf '%s' "${STUB_READY:-}" ;;
esac
exit 0
SH
chmod +x "$TMP_DIR/bin/kubectl"

# run_lib <VAR=value...> -- <shell code>: run code with the library sourced
# under set -euo pipefail and the stub first on PATH; OUT and RC hold the
# result.
run_lib() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  RC=0
  OUT="$(env PATH="$TMP_DIR/bin:$PATH" ${envs[@]+"${envs[@]}"} \
    bash -c "set -euo pipefail; . '$LIB'; $1" 2>&1)" || RC=$?
}

TWO_NEW_READY="$(printf 'new-1 True\nnew-2 True')"

# --- Test 1 ---------------------------------------------------------------
test_pod_uids_reads_the_uids() {
  echo "Test: pod_uids reads the UIDs of the pods the selector matches"
  run_lib STUB_PODS="$(printf 'old-1 True\nold-2 True')" -- 'pod_uids ns app=ctl'
  assert_eq "it prints them space-separated" "old-1 old-2" "$OUT"
}

# --- Test 2 ---------------------------------------------------------------
test_wait_replaced_passes_on_ready_new_pods() {
  echo "Test: wait_replaced passes once new pods replaced the old ones and are Ready"
  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS="$TWO_NEW_READY" -- \
    'wait_replaced ns app=ctl "old-1 old-2" 0'
  assert_eq "it returns 0" "0" "$RC"
  assert_contains "it says so" "$OUT" \
    "OK: ns/ctl replaced every pre-kill pod with 2 Ready pods and is at readyReplicas 2"
}

# --- Test 3 ---------------------------------------------------------------
test_wait_replaced_refuses_a_stale_ready_replicas() {
  echo "Test: wait_replaced refuses a pre-kill readyReplicas while the new pods are not Ready"
  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS="$(printf 'new-1 False\nnew-2 ')" -- \
    'wait_replaced ns app=ctl "old-1 old-2" 0'
  assert_eq "it returns 1 at the deadline" "1" "$RC"
  assert_contains "it names the new pods that are not Ready" "$OUT" \
    "FAIL: ns/ctl 0 s after the kill: pre-kill pods left: none, new pods Ready 0 of 2, readyReplicas 2 of 2"

  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS= -- 'wait_replaced ns app=ctl "old-1 old-2" 0'
  assert_eq "no pod at all is no recovery" "1" "$RC"
}

# --- Test 4 ---------------------------------------------------------------
test_wait_replaced_refuses_a_pre_kill_pod() {
  echo "Test: wait_replaced refuses a pre-kill pod that is still there"
  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS="$(printf 'old-1 True\nnew-2 True')" -- \
    'wait_replaced ns app=ctl "old-1 old-2" 0'
  assert_eq "it returns 1 at the deadline" "1" "$RC"
  assert_contains "it names the pre-kill pod" "$OUT" "pre-kill pods left: old-1, new pods Ready 1 of 2"
}

# --- Test 5 ---------------------------------------------------------------
test_wait_replaced_fails_on_a_failed_read() {
  echo "Test: a failed pod read fails wait_replaced"
  run_lib STUB_DESIRED=2 STUB_READY=2 STUB_PODS_FAIL=1 -- 'wait_replaced ns app=ctl "old-1 old-2" 60'
  assert_eq "it returns 1" "1" "$RC"
  assert_contains "and passes the error on" "$OUT" "Error from server: the stub refuses the read"
  assert_not_contains "and reports no success" "$OUT" "OK:"
}

# --- Run ---
test_pod_uids_reads_the_uids
test_wait_replaced_passes_on_ready_new_pods
test_wait_replaced_refuses_a_stale_ready_replicas
test_wait_replaced_refuses_a_pre_kill_pod
test_wait_replaced_fails_on_a_failed_read

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
