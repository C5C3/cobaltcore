#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/e2e/lib/assert-namespace-scoped-operator.sh, the proof every
# namespace-scoped-rbac suite runs against the operator release it installed:
#   - a healthy namespace-scoped operator passes all eight checks
#   - each check fails on its own evidence: no pod or two pods, a pod list, a
#     CR list or a pod kubectl cannot read, a manager that restarted before
#     check 2 or during the wait of check 8 (with its previous log), a second
#     CR, an empty or unreadable log, a missing or foreign startup line, a
#     forbidden watch (echoed), unreachable or empty metrics, and a success
#     counter that stays absent or zero
#   - a watch failure that is not forbidden passes, and so does a success
#     counter that appears on a later read of the metrics
#   - a counter printed in exponent form still counts
#   - anything but four non-empty arguments exits 2 with the usage line
#   - an edit to the script runs the e2e-operator jobs of the seven operators
#     whose namespace-scoped-rbac suites call it, and no other
#   - each of those seven suites calls it for its own release, controller and
#     resource, the glance and cinder suites for their backend controller too,
#     and no other namespace-scoped-rbac suite exists
#   - the startup line and the metrics port it reads match the shared
#     bootstrap and the seven charts
# Usage: bash tests/unit/ci/assert_namespace_scoped_operator_test.sh

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

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-namespace-scoped-operator.sh"
PLURAL="keystones.keystone.openstack.c5c3.io"
# The operators whose namespace-scoped-rbac suite runs the proof. Test 12
# compares the list with the suites on disk.
NS_SCOPED_OPS="barbican cinder glance horizon keystone nova placement"

STUBS="$(mktemp -d)"
trap 'rm -rf "$STUBS"' EXIT

# make_kubectl_stub <dir>
# Writes a kubectl stub that answers the five calls the script makes and reads
# its behaviour from the environment:
#   STUB_PODS           the "<pod> <restartCount>" lines of `get pods`
#   STUB_PODS_RC        the exit code of `get pods` (default 0)
#   STUB_POD_RESTARTS   the restartCount check 8 re-reads with `get pod`
#   STUB_POD_RC         the exit code of `get pod` (default 0)
#   STUB_CRS            the names `get <resource>` lists
#   STUB_CRS_RC         the exit code of `get <resource>` (default 0)
#   STUB_LOGS           the manager log `logs` prints
#   STUB_LOGS_RC        the exit code of `logs` (default 0)
#   STUB_PREVIOUS_LOGS  the log `logs --previous` prints
#   STUB_METRICS        the body of `get --raw`
#   STUB_METRICS_LATER  when set, the body of every `get --raw` after the
#                       first since the last reset_stub
#   STUB_RAW_RC         the exit code of `get --raw` (default 0)
# Every other call exits 0. A sleep stub beside it returns at once, so check
# 8's re-reads cost no time.
make_kubectl_stub() {
  local dir="$1"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
case "$1 $2" in
  "get pods")
    if [ "${STUB_PODS_RC:-0}" -ne 0 ]; then
      echo "Error from server (Forbidden): pods is forbidden" >&2
      exit "$STUB_PODS_RC"
    fi
    if [ -n "$STUB_PODS" ]; then printf '%s\n' "$STUB_PODS"; fi
    ;;
  "get pod")
    if [ "${STUB_POD_RC:-0}" -ne 0 ]; then
      echo "Error from server (NotFound): pods \"$5\" not found" >&2
      exit "$STUB_POD_RC"
    fi
    printf '%s' "$STUB_POD_RESTARTS"
    ;;
  "get --raw")
    if [ "${STUB_RAW_RC:-0}" -ne 0 ]; then
      echo "Error from server (ServiceUnavailable): the server is currently unable to handle the request" >&2
      exit "$STUB_RAW_RC"
    fi
    if [ -n "$STUB_METRICS_LATER" ] && [ -e "$(dirname "$0")/raw-read" ]; then
      printf '%s' "$STUB_METRICS_LATER"
    else
      : >"$(dirname "$0")/raw-read"
      printf '%s' "$STUB_METRICS"
    fi
    ;;
  "get "*)
    if [ "${STUB_CRS_RC:-0}" -ne 0 ]; then
      echo "error: the server doesn't have a resource type \"$2\"" >&2
      exit "$STUB_CRS_RC"
    fi
    if [ -n "$STUB_CRS" ]; then printf '%s\n' "$STUB_CRS"; fi
    ;;
  "logs "*)
    case " $* " in
      *" --previous "*) printf '%s\n' "$STUB_PREVIOUS_LOGS" ;;
      *)
        if [ "${STUB_LOGS_RC:-0}" -ne 0 ]; then
          echo "Error from server (NotFound): pods \"$4\" not found" >&2
          exit "$STUB_LOGS_RC"
        fi
        if [ -n "$STUB_LOGS" ]; then printf '%s\n' "$STUB_LOGS"; fi
        ;;
    esac
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
  printf '#!/bin/bash\nexit 0\n' >"$dir/sleep"
  chmod +x "$dir/sleep"
}

MODE_LINE='{"level":"info","ts":"2026-10-08T10:00:00Z","logger":"setup","msg":"namespace-scoped mode enabled","namespace":"openstack"}'
WATCH_FAILURE='{"level":"error","logger":"controller-runtime.cache.UnhandledError","msg":"Failed to watch","type":"*v1.ClusterSecretStore","error":"failed to list *v1.ClusterSecretStore: clustersecretstores.external-secrets.io is forbidden: User \"system:serviceaccount:openstack:keystone-operator-ns-scoped\" cannot list resource \"clustersecretstores\" in API group \"external-secrets.io\" at the cluster scope"}'
TRANSIENT_WATCH_FAILURE='{"level":"error","logger":"controller-runtime.cache.UnhandledError","msg":"Failed to watch","type":"*v1.Secret","error":"failed to list *v1.Secret: the server is currently unable to handle the request"}'
SUCCESS_LINE='controller_runtime_reconcile_total{controller="keystone",result="success"} 12'
ERROR_LINE='controller_runtime_reconcile_total{controller="keystone",result="error"} 3'

# reset_stub
# A healthy namespace-scoped keystone-operator: one pod that never restarted,
# one Keystone, a log with the startup line for openstack, and metrics with
# twelve successful reconciles beside three failed ones. Check 8 reads the
# metrics once and does not wait (RECONCILE_WAIT=0).
reset_stub() {
  STUB_PODS="op-0 0"
  STUB_PODS_RC=0
  STUB_POD_RESTARTS=0
  STUB_POD_RC=0
  STUB_CRS="keystone.keystone.openstack.c5c3.io/keystone-ns-scoped"
  STUB_CRS_RC=0
  STUB_LOGS="$MODE_LINE"$'\n''{"level":"info","logger":"setup","msg":"starting manager"}'
  STUB_LOGS_RC=0
  STUB_PREVIOUS_LOGS='{"level":"error","msg":"timed out waiting for cache to be synced"}'
  STUB_METRICS="# TYPE controller_runtime_reconcile_total counter"$'\n'"$ERROR_LINE"$'\n'"$SUCCESS_LINE"
  STUB_METRICS_LATER=""
  STUB_RAW_RC=0
  RECONCILE_WAIT=0
  rm -f "$STUBS/raw-read"
}

# run_check [args...]
# Runs the script with the kubectl stub first on PATH. Leaves its stdout in
# RUN_OUT and its exit code in RUN_RC, and writes its stderr to $STUBS/stderr.
# Without arguments it runs the keystone suite's call.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- openstack keystone-operator-ns-scoped keystone "$PLURAL"
  fi
  RUN_OUT="$(
    PATH="$STUBS:$PATH" \
      STUB_PODS="$STUB_PODS" STUB_PODS_RC="$STUB_PODS_RC" \
      STUB_POD_RESTARTS="$STUB_POD_RESTARTS" STUB_POD_RC="$STUB_POD_RC" \
      STUB_CRS="$STUB_CRS" STUB_CRS_RC="$STUB_CRS_RC" \
      STUB_LOGS="$STUB_LOGS" STUB_LOGS_RC="$STUB_LOGS_RC" \
      STUB_PREVIOUS_LOGS="$STUB_PREVIOUS_LOGS" \
      STUB_METRICS="$STUB_METRICS" STUB_METRICS_LATER="$STUB_METRICS_LATER" \
      STUB_RAW_RC="$STUB_RAW_RC" RECONCILE_WAIT_SECONDS="$RECONCILE_WAIT" \
      bash "$SCRIPT" "$@" 2>"$STUBS/stderr"
  )"
  RUN_RC=$?
}

make_kubectl_stub "$STUBS"

# --- Test 1: the happy path ---
test_a_healthy_operator_passes_all_eight_checks() {
  echo "Test: a healthy namespace-scoped operator passes all eight checks"

  reset_stub
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_eq "it prints eight OK: lines" "8" "$(grep -c '^OK:' <<<"$RUN_OUT" || true)"
  assert_not_contains "it prints no FAIL: line" "$RUN_OUT" "FAIL:"
}

# --- Test 2: check 1, the pod list ---
test_a_release_without_a_pod_fails() {
  echo "Test: a release without a pod fails"

  reset_stub
  STUB_PODS=""
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the missing pod" "$RUN_OUT" \
    "FAIL: no pod of release keystone-operator-ns-scoped in openstack"
}

test_a_release_with_two_pods_fails() {
  echo "Test: a release with two pods fails and names both"

  reset_stub
  STUB_PODS="op-0 0"$'\n'"op-1 0"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it counts the pods" "$RUN_OUT" \
    "FAIL: expected one pod of release keystone-operator-ns-scoped, found 2"
  assert_contains "it names the second pod" "$RUN_OUT" "op-1"
}

test_an_unlistable_release_fails() {
  echo "Test: a pod list kubectl cannot read fails with kubectl's error"

  reset_stub
  STUB_PODS_RC=1
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the pods cannot be listed" "$RUN_OUT" \
    "FAIL: cannot list the pods of release keystone-operator-ns-scoped"
  assert_contains "kubectl's own error reaches stderr" "$(cat "$STUBS/stderr")" "Forbidden"
}

# --- Test 3: check 2, the restart count ---
test_a_restarted_pod_fails_with_its_previous_log() {
  echo "Test: a restarted manager fails and prints its previous log"

  reset_stub
  STUB_PODS="op-0 3"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it counts the restarts" "$RUN_OUT" "FAIL: pod op-0 restarted 3 times"
  assert_contains "it prints the previous log" "$RUN_OUT" \
    "timed out waiting for cache to be synced"
}

# --- Test 4: check 3, the CR list ---
test_a_second_cr_fails() {
  echo "Test: a second CR of the kind fails, since its reconciles would count too"

  reset_stub
  STUB_CRS="keystone.keystone.openstack.c5c3.io/keystone-ns-scoped"$'\n'"keystone.keystone.openstack.c5c3.io/keystone-other"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it counts the CRs" "$RUN_OUT" \
    "FAIL: expected one $PLURAL in openstack, found 2"
  assert_contains "it names the other CR" "$RUN_OUT" "keystone-other"
}

test_an_unlistable_cr_kind_fails() {
  echo "Test: a CR list kubectl cannot read fails"

  reset_stub
  STUB_CRS_RC=1
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the CRs cannot be listed" "$RUN_OUT" \
    "FAIL: cannot list the $PLURAL in openstack"
}

# --- Test 5: check 4, the log ---
test_an_empty_log_fails() {
  echo "Test: an empty manager log fails"

  reset_stub
  STUB_LOGS=""
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the log is empty" "$RUN_OUT" "FAIL: pod op-0 has no log"
}

test_an_unreadable_log_fails() {
  echo "Test: a log kubectl cannot read fails"

  reset_stub
  STUB_LOGS_RC=1
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the log cannot be read" "$RUN_OUT" \
    "FAIL: cannot read the log of pod op-0"
}

# --- Test 6: check 5, the startup line ---
test_a_log_without_the_mode_line_fails() {
  echo "Test: a log without the namespace-scoped startup line fails"

  reset_stub
  STUB_LOGS='{"level":"info","logger":"setup","msg":"starting manager"}'
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the mode is missing" "$RUN_OUT" \
    "FAIL: pod op-0 did not start in namespace-scoped mode for openstack"
}

test_a_mode_line_for_another_namespace_fails() {
  echo "Test: a startup line naming another namespace fails"

  reset_stub
  STUB_LOGS='{"level":"info","logger":"setup","msg":"namespace-scoped mode enabled","namespace":"team-alpha"}'
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the mode is missing for openstack" "$RUN_OUT" \
    "FAIL: pod op-0 did not start in namespace-scoped mode for openstack"
}

# --- Test 7: check 6, the watch failures ---
test_a_watch_failure_fails_and_is_echoed() {
  echo "Test: a forbidden 'Failed to watch' line fails and is echoed"

  reset_stub
  STUB_LOGS="$MODE_LINE"$'\n'"$TRANSIENT_WATCH_FAILURE"$'\n'"$WATCH_FAILURE"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it counts the forbidden watches alone" "$RUN_OUT" \
    "FAIL: pod op-0 logged 1 forbidden 'Failed to watch' lines"
  assert_contains "it echoes the line, which names the forbidden kind" "$RUN_OUT" \
    '"type":"*v1.ClusterSecretStore"'
  assert_not_contains "it leaves the transient failure out" "$RUN_OUT" \
    '"type":"*v1.Secret"'
}

test_a_transient_watch_failure_passes() {
  echo "Test: a 'Failed to watch' line the API server caused passes"

  # The reflector logs any failed list this way and retries it; only an RBAC
  # denial is the operator's defect.
  reset_stub
  STUB_LOGS="$MODE_LINE"$'\n'"$TRANSIENT_WATCH_FAILURE"
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "check 6 passes" "$RUN_OUT" \
    "OK: pod op-0 logged no forbidden 'Failed to watch' line"
}

# --- Test 8: check 7, the metrics endpoint ---
test_unreachable_metrics_fail() {
  echo "Test: a metrics endpoint the API server cannot reach fails"

  reset_stub
  STUB_RAW_RC=1
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the endpoint is unreachable" "$RUN_OUT" \
    "FAIL: metrics endpoint of pod op-0 unreachable"
  assert_contains "kubectl's own error reaches stderr" "$(cat "$STUBS/stderr")" "ServiceUnavailable"
}

test_empty_metrics_fail() {
  echo "Test: an empty metrics body fails"

  reset_stub
  STUB_METRICS=""
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the endpoint returned nothing" "$RUN_OUT" \
    "FAIL: metrics endpoint of pod op-0 returned nothing"
}

# --- Test 9: check 8, the success counter ---
test_metrics_without_the_success_line_fail() {
  echo "Test: metrics without the controller's success counter fail"

  # The success line of a controller whose name starts with the one asked for
  # must not count for it.
  reset_stub
  STUB_METRICS="$ERROR_LINE"$'\n''controller_runtime_reconcile_total{controller="keystoneidentitybackend",result="success"} 5'
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says no reconcile succeeded" "$RUN_OUT" \
    "FAIL: pod op-0 reports no successful reconcile of controller keystone"
  assert_contains "it prints the controller's counters" "$RUN_OUT" "$ERROR_LINE"
  assert_not_contains "it leaves the other controller's counters out" "$RUN_OUT" \
    "keystoneidentitybackend"
}

test_a_zero_success_count_fails() {
  echo "Test: a success counter at 0 fails"

  reset_stub
  STUB_METRICS="$ERROR_LINE"$'\n''controller_runtime_reconcile_total{controller="keystone",result="success"} 0'
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says no reconcile succeeded" "$RUN_OUT" \
    "FAIL: pod op-0 reports no successful reconcile of controller keystone"
}

test_a_later_success_passes() {
  echo "Test: a success counter that appears on a later read passes"

  # Another operator may have written Ready while this one's last pass ended in
  # a requeue, so the first read can still lack the success line.
  reset_stub
  STUB_METRICS="$ERROR_LINE"
  STUB_METRICS_LATER="$ERROR_LINE"$'\n'"$SUCCESS_LINE"
  RECONCILE_WAIT=10
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "check 8 passes" "$RUN_OUT" \
    "OK: pod op-0 reports a successful reconcile of controller keystone"
}

test_a_success_after_the_wait_fails() {
  echo "Test: a success counter that appears only after the wait fails"

  reset_stub
  STUB_METRICS="$ERROR_LINE"
  STUB_METRICS_LATER="$ERROR_LINE"$'\n'"$SUCCESS_LINE"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the wait" "$RUN_OUT" \
    "FAIL: pod op-0 reports no successful reconcile of controller keystone after 0s"
}

test_a_restart_during_the_wait_fails() {
  echo "Test: a manager that restarts while check 8 waits fails"

  # The restarted container keeps the pod name, so its fresh counters answer
  # the later reads and record a success.
  reset_stub
  STUB_METRICS="$ERROR_LINE"
  STUB_METRICS_LATER="$ERROR_LINE"$'\n'"$SUCCESS_LINE"
  STUB_POD_RESTARTS=1
  RECONCILE_WAIT=10
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it counts the restarts" "$RUN_OUT" \
    "FAIL: pod op-0 restarted 1 times while check 8 waited"
  assert_contains "it prints the previous log" "$RUN_OUT" \
    "timed out waiting for cache to be synced"
  assert_not_contains "check 8 does not pass" "$RUN_OUT" \
    "OK: pod op-0 reports a successful reconcile"

  # A pod the Deployment replaced during the wait is gone by name.
  reset_stub
  STUB_POD_RC=1
  run_check
  assert_eq "a pod gone by name exits 1" "1" "$RUN_RC"
  assert_contains "it says the count cannot be re-read" "$RUN_OUT" \
    "FAIL: cannot re-read the restart count of pod op-0"
}

test_an_exponent_success_count_passes() {
  echo "Test: a success counter in exponent form counts"

  reset_stub
  STUB_METRICS='controller_runtime_reconcile_total{controller="keystone",result="success"} 1.2e+06'
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "check 8 passes" "$RUN_OUT" \
    "OK: pod op-0 reports a successful reconcile of controller keystone"
}

# --- Test 10: the arguments ---
test_wrong_arguments_exit_2() {
  echo "Test: anything but four non-empty arguments exits 2 with the usage line"

  local usage="usage: assert-namespace-scoped-operator.sh <namespace> <release> <controller> <resource>"
  reset_stub
  run_check openstack keystone-operator-ns-scoped keystone
  assert_eq "three arguments exit 2" "2" "$RUN_RC"
  assert_contains "the usage line goes to stderr" "$(cat "$STUBS/stderr")" "$usage"
  assert_eq "nothing goes to stdout" "" "$RUN_OUT"

  run_check openstack "" keystone "$PLURAL"
  assert_eq "an empty argument exits 2" "2" "$RUN_RC"
  assert_contains "the usage line goes to stderr" "$(cat "$STUBS/stderr")" "$usage"
}

test_the_script_parses() {
  echo "Test: the script parses"

  bash -n "$SCRIPT"
  assert_eq "bash -n passes" "0" "$?"
}

# --- Test 11: the path filters ---
test_the_seven_filters_list_the_shared_scripts() {
  echo "Test: the seven operators with a namespace-scoped-rbac suite list tests/e2e/lib/ in their filter"

  local op block
  for op in $NS_SCOPED_OPS; do
    block="$(filter_block "tests_e2e_$op")"
    assert_contains "tests_e2e_$op lists the shared scripts" "$block" "'tests/e2e/lib/**'"
  done
  # The neutron, ovn and c5c3 charts refuse rbac.namespaceScoped=true, so
  # these operators have no such suite, and a script edit must not run them.
  for op in c5c3 ovn neutron; do
    block="$(filter_block "tests_e2e_$op")"
    assert_not_empty "tests_e2e_$op exists" "$block"
    assert_not_contains "tests_e2e_$op leaves the shared scripts out" "$block" "tests/e2e/lib/"
  done
}

# --- Test 12: the seven suites ---
test_the_seven_suites_run_the_proof() {
  echo "Test: each namespace-scoped-rbac suite runs the proof against its own release"

  # A new suite must join NS_SCOPED_OPS, so that tests 11 and 12 check its
  # path filter and its call as well.
  local f found op
  found="$(for f in "$PROJECT_ROOT"/tests/e2e/*/namespace-scoped-rbac/chainsaw-test.yaml; do
    f="${f%/namespace-scoped-rbac/chainsaw-test.yaml}"
    echo "${f##*/}"
  done | LC_ALL=C sort | paste -s -d ' ' -)"
  assert_eq "the namespace-scoped-rbac suites on disk are NS_SCOPED_OPS" "$NS_SCOPED_OPS" "$found"

  for op in $NS_SCOPED_OPS; do
    assert_file_contains_fixed "the $op suite runs the proof" \
      "$PROJECT_ROOT/tests/e2e/$op/namespace-scoped-rbac/chainsaw-test.yaml" \
      "../../lib/assert-namespace-scoped-operator.sh openstack $op-operator-ns-scoped $op ${op}s.$op.openstack.c5c3.io"
  done
  # The cluster-wide operator drives these suites' backend CRs to Ready too.
  for op in glance cinder; do
    assert_file_contains_fixed "the $op suite runs the proof for its backend" \
      "$PROJECT_ROOT/tests/e2e/$op/namespace-scoped-rbac/chainsaw-test.yaml" \
      "../../lib/assert-namespace-scoped-operator.sh openstack $op-operator-ns-scoped ${op}backend ${op}backends.$op.openstack.c5c3.io"
  done
}

# --- Test 13: the operator side of the proof ---
test_the_proof_matches_the_operator() {
  echo "Test: the startup line and the metrics port the proof reads match the operator"

  # Check 5 greps the message and the key of this call.
  assert_file_contains_fixed "the shared bootstrap logs the startup line of check 5" \
    "$PROJECT_ROOT/internal/common/bootstrap/manager.go" \
    'setupLog.Info("namespace-scoped mode enabled", "namespace", opts.namespace)'

  # Check 7 reads the port each chart passes as --metrics-bind-address.
  local op port
  port="$(sed -n 's/^metrics_port=//p' "$SCRIPT")"
  assert_not_empty "the script names its metrics port" "$port"
  for op in $NS_SCOPED_OPS; do
    assert_eq "the $op chart serves metrics on the script's port" "$port" \
      "$(yq '.metrics.port' "$PROJECT_ROOT/operators/$op/helm/$op-operator/values.yaml")"
  done
}

# --- Run ---
test_a_healthy_operator_passes_all_eight_checks
test_a_release_without_a_pod_fails
test_a_release_with_two_pods_fails
test_an_unlistable_release_fails
test_a_restarted_pod_fails_with_its_previous_log
test_a_second_cr_fails
test_an_unlistable_cr_kind_fails
test_an_empty_log_fails
test_an_unreadable_log_fails
test_a_log_without_the_mode_line_fails
test_a_mode_line_for_another_namespace_fails
test_a_watch_failure_fails_and_is_echoed
test_a_transient_watch_failure_passes
test_unreachable_metrics_fail
test_empty_metrics_fail
test_metrics_without_the_success_line_fail
test_a_zero_success_count_fails
test_a_later_success_passes
test_a_success_after_the_wait_fails
test_a_restart_during_the_wait_fails
test_an_exponent_success_count_passes
test_wrong_arguments_exit_2
test_the_script_parses
test_the_seven_filters_list_the_shared_scripts
test_the_seven_suites_run_the_proof
test_the_proof_matches_the_operator

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
