#!/usr/bin/env bats
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
#     forbidden watch (echoed), unreachable or empty metrics, and metrics
#     that count neither a success nor a HealthCheck run, even beside
#     requeues, another step's runs, or the service controller's runs when
#     its backend controller is read
#   - a watch failure that is not forbidden passes, and so does a HealthCheck
#     run that appears on a later read of the metrics
#   - a success counter passes on its own, also in exponent form
#   - anything but four non-empty arguments exits 2 with the usage line
#   - an edit to the script runs the e2e-operator jobs of the seven operators
#     whose namespace-scoped-rbac suites call it, and no other
#   - each of those seven suites calls it for its own release, controller and
#     resource, the glance and cinder suites for their backend controller too,
#     and no other namespace-scoped-rbac suite exists
#   - the startup line and the metrics port it reads match the shared
#     bootstrap and the seven charts, and each of the seven operators names
#     its instrumenter <op>_operator and its API probe step HealthCheck
# Usage: bin/bats tests/unit/ci/assert_namespace_scoped_operator.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-namespace-scoped-operator.sh"
PLURAL="keystones.keystone.openstack.c5c3.io"
# The operators whose namespace-scoped-rbac suite runs the proof. Test 12
# compares the list with the suites on disk.
NS_SCOPED_OPS="barbican cinder glance horizon keystone nova placement"

# The kubectl rules answer the five calls the script makes. setup declares
# the answers of reset_stub, and a test declares the rules it changes after
# them, so that its own rule answers:
#   get pods           STUB_PODS, the "<pod> <restartCount>" lines; or fails
#                      with Forbidden
#   get pod            STUB_POD_RESTARTS, the restartCount check 8 re-reads;
#                      or fails with NotFound
#   get <resource>     STUB_CRS, the names of the CRs; or fails
#   logs               STUB_LOGS, the manager log; or fails with NotFound
#   logs --previous    STUB_PREVIOUS_LOGS
#   get --raw          STUB_METRICS, the body of the metrics; or fails with
#                      ServiceUnavailable. A test whose later reads answer
#                      STUB_METRICS_LATER declares that as a lasting rule
#                      and STUB_METRICS after it with --times 1.
# Every other call exits 0. A sleep stub beside it returns at once, so check
# 8's re-reads cost no time.

MODE_LINE='{"level":"info","ts":"2026-10-08T10:00:00Z","logger":"setup","msg":"namespace-scoped mode enabled","namespace":"openstack"}'
WATCH_FAILURE='{"level":"error","logger":"controller-runtime.cache.UnhandledError","msg":"Failed to watch","type":"*v1.ClusterSecretStore","error":"failed to list *v1.ClusterSecretStore: clustersecretstores.external-secrets.io is forbidden: User \"system:serviceaccount:openstack:keystone-operator-ns-scoped\" cannot list resource \"clustersecretstores\" in API group \"external-secrets.io\" at the cluster scope"}'
TRANSIENT_WATCH_FAILURE='{"level":"error","logger":"controller-runtime.cache.UnhandledError","msg":"Failed to watch","type":"*v1.Secret","error":"failed to list *v1.Secret: the server is currently unable to handle the request"}'
SUCCESS_LINE='controller_runtime_reconcile_total{controller="keystone",result="success"} 12'
ZERO_SUCCESS_LINE='controller_runtime_reconcile_total{controller="keystone",result="success"} 0'
ERROR_LINE='controller_runtime_reconcile_total{controller="keystone",result="error"} 3'
REQUEUE_AFTER_LINE='controller_runtime_reconcile_total{controller="keystone",result="requeue_after"} 15'
PROBE_LINE='keystone_operator_reconcile_duration_seconds_count{sub_reconciler="HealthCheck"} 7'

# reset_stub
# A healthy namespace-scoped keystone-operator: one pod that never restarted,
# one Keystone, a log with the startup line for openstack, and the metrics a
# Ready Keystone leaves: no success, since every converged pass requeues for
# the next API probe, fifteen requeues beside three failed passes, and seven
# runs of the HealthCheck step. Check 8 reads the metrics once and does not
# wait (RECONCILE_WAIT=0). Called again inside a test, it declares these
# rules anew, so they answer again.
reset_stub() {
  STUB_PODS="op-0 0"
  STUB_POD_RESTARTS=0
  STUB_CRS="keystone.keystone.openstack.c5c3.io/keystone-ns-scoped"
  STUB_LOGS="$MODE_LINE"$'\n''{"level":"info","logger":"setup","msg":"starting manager"}'
  STUB_PREVIOUS_LOGS='{"level":"error","msg":"timed out waiting for cache to be synced"}'
  STUB_METRICS="# TYPE controller_runtime_reconcile_total counter"$'\n'"$ERROR_LINE"$'\n'"$REQUEUE_AFTER_LINE"$'\n'"$ZERO_SUCCESS_LINE"$'\n'"$PROBE_LINE"
  RECONCILE_WAIT=0
  stub_on kubectl '*'
  stub_on kubectl 'get *' --out "$STUB_CRS"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  stub_on kubectl 'get pod *' --out-raw "$STUB_POD_RESTARTS"
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS"
  stub_on kubectl 'logs *' --out "$STUB_LOGS"
  stub_on kubectl 'logs * --previous *' --out "$STUB_PREVIOUS_LOGS"
}

setup() {
  stub_on sleep '*'
  reset_stub
}

# run_check [args...]
# Runs the script with the stubs first on PATH, through run: its stdout in
# $output, its stderr in $stderr and its exit code in $status.
# Without arguments it runs the keystone suite's call.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- openstack keystone-operator-ns-scoped keystone "$PLURAL"
  fi
  run --separate-stderr with_stubs env RECONCILE_WAIT_SECONDS="$RECONCILE_WAIT" \
    bash "$SCRIPT" "$@"
}

# --- Test 1: the happy path ---
@test "a healthy namespace-scoped operator passes all eight checks" {
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "it prints eight OK: lines" "8" "$(grep -c '^OK:' <<<"$output" || true)"
  assert_not_contains "it prints no FAIL: line" "$output" "FAIL:"
  assert_contains "check 8 passes on the HealthCheck runs" "$output" \
    "OK: pod op-0 reports a HealthCheck run of controller keystone"
}

# --- Test 2: check 1, the pod list ---
@test "a release without a pod fails" {
  STUB_PODS=""
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the missing pod" "$output" \
    "FAIL: no pod of release keystone-operator-ns-scoped in openstack"
}

@test "a release with two pods fails and names both" {
  STUB_PODS="op-0 0"$'\n'"op-1 0"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it counts the pods" "$output" \
    "FAIL: expected one pod of release keystone-operator-ns-scoped, found 2"
  assert_contains "it names the second pod" "$output" "op-1"
}

@test "a pod list kubectl cannot read fails with kubectl's error" {
  stub_on kubectl 'get pods *' --rc 1 --err "Error from server (Forbidden): pods is forbidden"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the pods cannot be listed" "$output" \
    "FAIL: cannot list the pods of release keystone-operator-ns-scoped"
  assert_contains "kubectl's own error reaches stderr" "$stderr" "Forbidden"
}

# --- Test 3: check 2, the restart count ---
@test "a restarted manager fails and prints its previous log" {
  STUB_PODS="op-0 3"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it counts the restarts" "$output" "FAIL: pod op-0 restarted 3 times"
  assert_contains "it prints the previous log" "$output" \
    "timed out waiting for cache to be synced"
}

# --- Test 4: check 3, the CR list ---
@test "a second CR of the kind fails, since its reconciles would count too" {
  STUB_CRS="keystone.keystone.openstack.c5c3.io/keystone-ns-scoped"$'\n'"keystone.keystone.openstack.c5c3.io/keystone-other"
  stub_on kubectl "get $PLURAL *" --out "$STUB_CRS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it counts the CRs" "$output" \
    "FAIL: expected one $PLURAL in openstack, found 2"
  assert_contains "it names the other CR" "$output" "keystone-other"
}

@test "a CR list kubectl cannot read fails" {
  stub_on kubectl "get $PLURAL *" --rc 1 --err "error: the server doesn't have a resource type \"$PLURAL\""
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the CRs cannot be listed" "$output" \
    "FAIL: cannot list the $PLURAL in openstack"
}

# --- Test 5: check 4, the log ---
# A rule for the read of the log names its --tail=-1, so that it leaves the
# read of the previous log to its own rule.
@test "an empty manager log fails" {
  STUB_LOGS=""
  stub_on kubectl 'logs * --tail=-1' --out "$STUB_LOGS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the log is empty" "$output" "FAIL: pod op-0 has no log"
}

@test "a log kubectl cannot read fails" {
  stub_on kubectl 'logs * --tail=-1' --rc 1 --err 'Error from server (NotFound): pods "op-0" not found'
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the log cannot be read" "$output" \
    "FAIL: cannot read the log of pod op-0"
}

# --- Test 6: check 5, the startup line ---
@test "a log without the namespace-scoped startup line fails" {
  STUB_LOGS='{"level":"info","logger":"setup","msg":"starting manager"}'
  stub_on kubectl 'logs * --tail=-1' --out "$STUB_LOGS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the mode is missing" "$output" \
    "FAIL: pod op-0 did not start in namespace-scoped mode for openstack"
}

@test "a startup line naming another namespace fails" {
  STUB_LOGS='{"level":"info","logger":"setup","msg":"namespace-scoped mode enabled","namespace":"team-alpha"}'
  stub_on kubectl 'logs * --tail=-1' --out "$STUB_LOGS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the mode is missing for openstack" "$output" \
    "FAIL: pod op-0 did not start in namespace-scoped mode for openstack"
}

# --- Test 7: check 6, the watch failures ---
@test "a forbidden 'Failed to watch' line fails and is echoed" {
  STUB_LOGS="$MODE_LINE"$'\n'"$TRANSIENT_WATCH_FAILURE"$'\n'"$WATCH_FAILURE"
  stub_on kubectl 'logs * --tail=-1' --out "$STUB_LOGS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it counts the forbidden watches alone" "$output" \
    "FAIL: pod op-0 logged 1 forbidden 'Failed to watch' lines"
  assert_contains "it echoes the line, which names the forbidden kind" "$output" \
    '"type":"*v1.ClusterSecretStore"'
  assert_not_contains "it leaves the transient failure out" "$output" \
    '"type":"*v1.Secret"'
}

@test "a 'Failed to watch' line the API server caused passes" {
  # The reflector logs any failed list this way and retries it; only an RBAC
  # denial is the operator's defect.
  STUB_LOGS="$MODE_LINE"$'\n'"$TRANSIENT_WATCH_FAILURE"
  stub_on kubectl 'logs * --tail=-1' --out "$STUB_LOGS"
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "check 6 passes" "$output" \
    "OK: pod op-0 logged no forbidden 'Failed to watch' line"
}

# --- Test 8: check 7, the metrics endpoint ---
@test "a metrics endpoint the API server cannot reach fails" {
  stub_on kubectl 'get --raw *' --rc 1 --err "Error from server (ServiceUnavailable): the server is currently unable to handle the request"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the endpoint is unreachable" "$output" \
    "FAIL: metrics endpoint of pod op-0 unreachable"
  assert_contains "kubectl's own error reaches stderr" "$stderr" "ServiceUnavailable"
}

@test "an empty metrics body fails" {
  STUB_METRICS=""
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the endpoint returned nothing" "$output" \
    "FAIL: metrics endpoint of pod op-0 returned nothing"
}

# --- Test 9: check 8, the success counter and the HealthCheck runs ---
@test "metrics with requeues but neither a success nor a HealthCheck run fail" {
  # A pass that waits on a dependency ends in a requeue too, so requeues alone
  # do not show that this pod got anywhere. Neither do the runs of another
  # step, nor the success line of a controller whose name starts with the one
  # asked for.
  STUB_METRICS="$ERROR_LINE"$'\n'"$REQUEUE_AFTER_LINE"$'\n'"$ZERO_SUCCESS_LINE"$'\n''keystone_operator_reconcile_duration_seconds_count{sub_reconciler="Deployment"} 9'$'\n''controller_runtime_reconcile_total{controller="keystoneidentitybackend",result="success"} 5'
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the controller finished no pass" "$output" \
    "FAIL: pod op-0 reports neither a successful reconcile nor a HealthCheck run of controller keystone"
  assert_contains "it prints the controller's counters" "$output" "$REQUEUE_AFTER_LINE"
  assert_not_contains "it leaves the other step's runs out" "$output" \
    'sub_reconciler="Deployment"'
  assert_not_contains "it leaves the other controller's counters out" "$output" \
    "keystoneidentitybackend"
}

@test "a success counter and a HealthCheck count at 0 fail" {
  STUB_METRICS="$ERROR_LINE"$'\n'"$ZERO_SUCCESS_LINE"$'\n''keystone_operator_reconcile_duration_seconds_count{sub_reconciler="HealthCheck"} 0'
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the controller finished no pass" "$output" \
    "FAIL: pod op-0 reports neither a successful reconcile nor a HealthCheck run of controller keystone"
  assert_contains "it prints the HealthCheck count" "$output" \
    'keystone_operator_reconcile_duration_seconds_count{sub_reconciler="HealthCheck"} 0'
}

@test "a success counter passes without a HealthCheck run" {
  # A backend controller has no probe step and converges with a zero result.
  STUB_METRICS="$ERROR_LINE"$'\n'"$SUCCESS_LINE"
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS"
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "check 8 passes on the success counter" "$output" \
    "OK: pod op-0 reports a successful reconcile of controller keystone"
}

@test "the service controller's HealthCheck runs do not count for its backend controller" {
  # The cinder suite reads one pod for both controllers, so the cinder
  # controller's probe runs are on the same endpoint as the backend's counters.
  STUB_CRS="cinderbackend.cinder.openstack.c5c3.io/ns-scoped-nfs1"
  STUB_METRICS='controller_runtime_reconcile_total{controller="cinderbackend",result="success"} 0'$'\n''cinder_operator_reconcile_duration_seconds_count{sub_reconciler="HealthCheck"} 4'
  stub_on kubectl 'get cinderbackends.cinder.openstack.c5c3.io *' --out "$STUB_CRS"
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS"
  run_check openstack cinder-operator-ns-scoped cinderbackend cinderbackends.cinder.openstack.c5c3.io
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the backend controller finished no pass" "$output" \
    "FAIL: pod op-0 reports neither a successful reconcile nor a HealthCheck run of controller cinderbackend"
  assert_not_contains "it leaves the cinder controller's runs out" "$output" \
    "cinder_operator_"
}

@test "a HealthCheck run that appears on a later read passes" {
  # Another operator may have written Ready while this one's last pass ended in
  # a requeue for a dependency, so the first read can still lack the run.
  STUB_METRICS="$ERROR_LINE"$'\n'"$REQUEUE_AFTER_LINE"
  STUB_METRICS_LATER="$ERROR_LINE"$'\n'"$REQUEUE_AFTER_LINE"$'\n'"$PROBE_LINE"
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS_LATER"
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS" --times 1
  RECONCILE_WAIT=10
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "check 8 passes" "$output" \
    "OK: pod op-0 reports a HealthCheck run of controller keystone"
}

@test "a HealthCheck run that appears only after the wait fails" {
  STUB_METRICS="$ERROR_LINE"$'\n'"$REQUEUE_AFTER_LINE"
  STUB_METRICS_LATER="$ERROR_LINE"$'\n'"$REQUEUE_AFTER_LINE"$'\n'"$PROBE_LINE"
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS_LATER"
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS" --times 1
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the wait" "$output" \
    "FAIL: pod op-0 reports neither a successful reconcile nor a HealthCheck run of controller keystone after 0s"
}

@test "a manager that restarts while check 8 waits fails" {
  # The restarted container keeps the pod name, so its fresh counters answer
  # the later reads and record a HealthCheck run.
  STUB_METRICS="$ERROR_LINE"
  STUB_METRICS_LATER="$ERROR_LINE"$'\n'"$PROBE_LINE"
  STUB_POD_RESTARTS=1
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS_LATER"
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS" --times 1
  stub_on kubectl 'get pod *' --out-raw "$STUB_POD_RESTARTS"
  RECONCILE_WAIT=10
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it counts the restarts" "$output" \
    "FAIL: pod op-0 restarted 1 times while check 8 waited"
  assert_contains "it prints the previous log" "$output" \
    "timed out waiting for cache to be synced"
  assert_not_contains "check 8 does not pass" "$output" \
    "OK: pod op-0 reports a HealthCheck run"

  # A pod the Deployment replaced during the wait is gone by name.
  reset_stub
  stub_on kubectl 'get pod *' --rc 1 --err 'Error from server (NotFound): pods "op-0" not found'
  run_check
  assert_eq "a pod gone by name exits 1" "1" "$status"
  assert_contains "it says the count cannot be re-read" "$output" \
    "FAIL: cannot re-read the restart count of pod op-0"
}

@test "a success counter in exponent form counts" {
  STUB_METRICS='controller_runtime_reconcile_total{controller="keystone",result="success"} 1.2e+06'
  stub_on kubectl 'get --raw *' --out-raw "$STUB_METRICS"
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "check 8 passes" "$output" \
    "OK: pod op-0 reports a successful reconcile of controller keystone"
}

# --- Test 10: the arguments ---
@test "anything but four non-empty arguments exits 2 with the usage line" {
  local usage="usage: assert-namespace-scoped-operator.sh <namespace> <release> <controller> <resource>"
  run_check openstack keystone-operator-ns-scoped keystone
  assert_eq "three arguments exit 2" "2" "$status"
  assert_contains "the usage line goes to stderr" "$stderr" "$usage"
  assert_eq "nothing goes to stdout" "" "$output"

  run_check openstack "" keystone "$PLURAL"
  assert_eq "an empty argument exits 2" "2" "$status"
  assert_contains "the usage line goes to stderr" "$stderr" "$usage"
}

@test "the script parses" {
  bash -n "$SCRIPT"
  assert_eq "bash -n passes" "0" "$?"
}

# --- Test 11: the path filters ---
@test "the seven operators with a namespace-scoped-rbac suite list tests/e2e/lib/ in their filter" {
  local op block
  for op in $NS_SCOPED_OPS; do
    block="$(filter_block "tests_e2e_$op")"
    soft assert_contains "tests_e2e_$op lists the shared scripts" "$block" "'tests/e2e/lib/**'"
  done
  # The neutron, ovn and c5c3 charts refuse rbac.namespaceScoped=true, so
  # these operators have no such suite, and an edit to the proof must not run
  # them. Their env-key-material suites run the environment scan beside it,
  # which their filters name on its own.
  for op in c5c3 ovn neutron; do
    block="$(filter_block "tests_e2e_$op")"
    soft assert_not_empty "tests_e2e_$op exists" "$block"
    soft assert_not_contains "tests_e2e_$op leaves the shared directory out" "$block" "tests/e2e/lib/**"
    soft assert_not_contains "tests_e2e_$op leaves the proof out" "$block" "assert-namespace-scoped-operator.sh"
  done
}

# --- Test 12: the seven suites ---
@test "each namespace-scoped-rbac suite runs the proof against its own release" {
  # A new suite must join NS_SCOPED_OPS, so that tests 11 and 12 check its
  # path filter and its call as well.
  local f found op
  found="$(for f in "$PROJECT_ROOT"/tests/e2e/*/namespace-scoped-rbac/chainsaw-test.yaml; do
    f="${f%/namespace-scoped-rbac/chainsaw-test.yaml}"
    echo "${f##*/}"
  done | LC_ALL=C sort | paste -s -d ' ' -)"
  assert_eq "the namespace-scoped-rbac suites on disk are NS_SCOPED_OPS" "$NS_SCOPED_OPS" "$found"

  for op in $NS_SCOPED_OPS; do
    soft assert_file_contains_fixed "the $op suite runs the proof" \
      "$PROJECT_ROOT/tests/e2e/$op/namespace-scoped-rbac/chainsaw-test.yaml" \
      "../../lib/assert-namespace-scoped-operator.sh openstack $op-operator-ns-scoped $op ${op}s.$op.openstack.c5c3.io"
  done
  # The cluster-wide operator drives these suites' backend CRs to Ready too.
  for op in glance cinder; do
    soft assert_file_contains_fixed "the $op suite runs the proof for its backend" \
      "$PROJECT_ROOT/tests/e2e/$op/namespace-scoped-rbac/chainsaw-test.yaml" \
      "../../lib/assert-namespace-scoped-operator.sh openstack $op-operator-ns-scoped ${op}backend ${op}backends.$op.openstack.c5c3.io"
  done
}

# --- Test 13: the operator side of the proof ---
@test "the startup line and the metrics port the proof reads match the operator" {
  # Check 5 greps the message and the key of this call.
  assert_file_contains_fixed "the shared bootstrap logs the startup line of check 5" \
    "$PROJECT_ROOT/internal/common/bootstrap/manager.go" \
    'setupLog.Info("namespace-scoped mode enabled", "namespace", opts.namespace)'

  # Check 7 reads the port each chart passes as --metrics-bind-address.
  local op port
  port="$(sed -n 's/^metrics_port=//p' "$SCRIPT")"
  assert_not_empty "the script names its metrics port" "$port"
  for op in $NS_SCOPED_OPS; do
    soft assert_eq "the $op chart serves metrics on the script's port" "$port" \
      "$(yq '.metrics.port' "$PROJECT_ROOT/operators/$op/helm/$op-operator/values.yaml")"
  done

  # Check 8 counts the runs of the HealthCheck step through the histogram the
  # shared instrumenter names <prefix>_reconcile_duration_seconds, labelled by
  # sub_reconciler. Each operator passes <op>_operator as the prefix, and its
  # service controller, whose converged pass requeues for the next probe, runs
  # the probe step under that name.
  local instrumentation="$PROJECT_ROOT/internal/common/instrumentation/instrumentation.go"
  assert_file_contains_fixed "the instrumenter names its histogram <prefix>_reconcile_duration_seconds" \
    "$instrumentation" 'Name:    prefix + "_reconcile_duration_seconds",'
  assert_file_contains_fixed "the histogram is labelled by sub_reconciler" \
    "$instrumentation" '}, []string{"sub_reconciler"}),'
  for op in $NS_SCOPED_OPS; do
    soft assert_file_contains_fixed "the $op operator names its instrumenter ${op}_operator" \
      "$PROJECT_ROOT/operators/$op/internal/controller/instrumentation.go" \
      "instrumentation.NewSubReconcilerInstrumenter(\"${op}_operator\","
    soft assert_file_matches "the $op controller runs a step named HealthCheck" \
      "$PROJECT_ROOT/operators/$op/internal/controller/${op}_controller.go" \
      'Name: *"HealthCheck",'
    soft assert_file_contains_fixed "the $op controller requeues a converged pass for the next probe" \
      "$PROJECT_ROOT/operators/$op/internal/controller/${op}_controller.go" \
      "healthcheck.ScheduleReprobe("
  done
}
