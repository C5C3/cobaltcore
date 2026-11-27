#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/e2e/lib/dump-identity-backend-diagnostics.sh, the failure dump
# of the keystone identity-backend suites:
#   - the six section titles appear once each, in order, and the script exits
#     0
#   - sections 1 and 2 select the events by kind and name and print them in
#     the fixed columns, every one of them; section 3 keeps the last 30
#   - section 4 reads 30 minutes of each operator pod's log, keeps the lines
#     of the KeystoneIdentityBackend controller and the lines that name the
#     Keystone, and the last 300 of those; section 5 reads 60 lines of the
#     running and the previous containers
#   - section 6 reads each pod's metrics through the pod proxy and keeps the
#     seven workqueue and reconcile families only
#   - no event, no operator pod and a log without a kept line print their
#     placeholders; a failing kubectl call prints its error, leaves the other
#     sections alone, and the script still exits 0
#   - a wrong argument count or an empty argument exits 2 with the usage line
#     and calls no kubectl
#   - an edit to the script runs the keystone e2e-operator job
#   - every catch block of oidc-federation, saml-federation and
#     ldap-domain-backend ends with the call for its Keystone under a 90s
#     timeout, no operator log or event line of the old dump is left, and no
#     other suite calls the script
# Usage: bin/bats tests/unit/ci/dump_identity_backend_diagnostics.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/dump-identity-backend-diagnostics.sh"
USAGE="usage: dump-identity-backend-diagnostics.sh <cr-namespace> <keystone-name>"
COLUMNS_ARG="custom-columns=LAST:.lastTimestamp,FIRST:.firstTimestamp,COUNT:.count,TYPE:.type,REASON:.reason,OBJECT:.involvedObject.name,MESSAGE:.message"

TITLES="$(cat <<'TITLES'
=== KeystoneIdentityBackend events in openstack ===
=== Keystone keystone-oidc events ===
=== Events in openstack (last 30) ===
=== operator log: KeystoneIdentityBackend controller and keystone-oidc (since 30m) ===
=== operator log (last 60 lines) ===
=== operator workqueue and reconcile metrics ===
TITLES
)"

# The suites that call the script: the directory under tests/e2e/keystone/,
# the Keystone the call names, and the number of catch blocks.
DUMP_CALLERS="$(cat <<'CALLS'
oidc-federation|keystone-oidc|7
saml-federation|keystone-saml|6
ldap-domain-backend|keystone-ldap|5
CALLS
)"

# make_kubectl_stub
# Declares the kubectl rule that lets every call the other rules do not
# answer exit 0. stub_calls kubectl prints the arguments of every call.
# reset_stub declares the answers to the calls the script makes:
#   get events  STUB_BACKEND_EVENTS for the KeystoneIdentityBackend selector,
#               STUB_KEYSTONE_EVENTS for the Keystone selector, STUB_EVENTS
#               otherwise
#   get pods    STUB_PODS
#   get --raw   STUB_METRICS
#   logs        STUB_SINCE_LOG for --since=30m, STUB_PREVIOUS_LOG for
#               --previous, STUB_RECENT_LOG otherwise
# A test declares what it changes: kubectl's "No resources found" line on
# stderr for an empty list, or --rc 1 with kubectl's error for a failing
# call.
make_kubectl_stub() {
  stub_on kubectl '*'
}

# numbered <prefix> <first> <last>: one "<prefix><n>" line per n.
numbered() {
  local i
  for ((i = $2; i <= $3; i++)); do
    printf '%s%03d\n' "$1" "$i"
  done
}

BACKEND_LINE='{"level":"error","msg":"Reconciler error","controller":"keystoneidentitybackend","controllerKind":"KeystoneIdentityBackend","KeystoneIdentityBackend":{"name":"keycloak-cobaltcore","namespace":"openstack"}}'
KEYSTONE_LINE='{"level":"info","msg":"reconciled","controller":"keystone","controllerKind":"Keystone","Keystone":{"name":"keystone-oidc","namespace":"openstack"}}'
OTHER_KEYSTONE_LINE='{"level":"info","msg":"reconciled","controller":"keystone","controllerKind":"Keystone","Keystone":{"name":"keystone-saml","namespace":"openstack"}}'
UNRELATED_LINE='{"level":"info","msg":"Starting workers","controller":"keystoneservice","worker count":2}'

# The seven kept families, one line each, and the lines section 6 drops:
# comments, three controller-runtime families outside the list, and a family
# whose name ends in a kept one.
KEPT_METRICS="$(cat <<'METRICS'
workqueue_longest_running_processor_seconds{controller="keystoneidentitybackend",name="keystoneidentitybackend"} 312.5
workqueue_unfinished_work_seconds{controller="keystoneidentitybackend",name="keystoneidentitybackend"} 312.5
workqueue_depth{controller="keystoneidentitybackend",name="keystoneidentitybackend"} 1
controller_runtime_active_workers{controller="keystoneidentitybackend"} 1
controller_runtime_max_concurrent_reconciles{controller="keystoneidentitybackend"} 2
controller_runtime_reconcile_total{controller="keystoneidentitybackend",result="requeue_after"} 4
controller_runtime_reconcile_errors_total{controller="keystoneidentitybackend"} 0
METRICS
)"
DROPPED_METRICS="$(cat <<'METRICS'
# HELP workqueue_depth Current depth of workqueue
# TYPE workqueue_depth gauge
workqueue_adds_total{controller="keystoneidentitybackend",name="keystoneidentitybackend"} 9
controller_runtime_reconcile_time_seconds_bucket{controller="keystoneidentitybackend",le="0.005"} 3
controller_runtime_terminal_reconcile_errors_total{controller="keystoneidentitybackend"} 0
keystone_operator_workqueue_depth{controller="keystoneidentitybackend"} 0
METRICS
)"

# reset_stub
# Two operator pods, 40 backend events, one Keystone event, 40 namespace
# events, a log with each kind of line, and a metrics body with the kept and
# the dropped families interleaved, declared as the newest kubectl rules; and
# an empty call log. setup calls it, and a test calls it again to start over
# from these answers.
reset_stub() {
  STUB_BACKEND_EVENTS="$(numbered '2026-11-14T18:14:05Z 2026-11-14T18:14:05Z 1 Normal FederationObjectsDeleted keycloak-cobaltcore backend-event-' 1 40)"
  STUB_KEYSTONE_EVENTS='2026-11-14T18:14:39Z 2026-11-14T18:10:02Z 3 Normal DeploymentReady keystone-oidc Keystone API ready'
  STUB_EVENTS="$(numbered 'namespace-event-' 1 40)"
  STUB_PODS=$'pod/keystone-operator-aaa\npod/keystone-operator-bbb'
  STUB_SINCE_LOG="$BACKEND_LINE"$'\n'"$UNRELATED_LINE"$'\n'"$KEYSTONE_LINE"$'\n'"$OTHER_KEYSTONE_LINE"
  STUB_RECENT_LOG="recent-log-line"
  STUB_PREVIOUS_LOG="previous-log-line"
  STUB_METRICS="$(paste -d'\n' <(printf '%s\n' "$KEPT_METRICS") <(printf '%s\n' "$DROPPED_METRICS" ""))"
  stub_on kubectl 'get events *' --out "$STUB_EVENTS"
  stub_on kubectl 'get events *involvedObject.kind=Keystone,*' --out "$STUB_KEYSTONE_EVENTS"
  stub_on kubectl 'get events *involvedObject.kind=KeystoneIdentityBackend*' --out "$STUB_BACKEND_EVENTS"
  stub_on kubectl 'get pods *' --out "$STUB_PODS"
  stub_on kubectl 'get --raw *' --out "$STUB_METRICS"
  stub_on kubectl 'logs *' --out "$STUB_RECENT_LOG"
  stub_on kubectl 'logs *--since=30m*' --out "$STUB_SINCE_LOG"
  stub_on kubectl 'logs *--previous*' --out "$STUB_PREVIOUS_LOG"
  rm -f "$STUB_DIR/calls/kubectl"
}

# run_check [args...]
# Runs the script through run with the stubs first on PATH: its stdout in
# $output, its stderr in $stderr and its exit code in $status. Without
# arguments it runs the oidc-federation call.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- openstack keystone-oidc
  fi
  run --separate-stderr with_stubs bash "$SCRIPT" "$@"
}

# section <n>: the lines of $output under the <n>th title of TITLES, up to
# the next title line.
section() {
  local title
  title="$(sed -n "${1}p" <<<"$TITLES")"
  awk -v t="$title" '$0 == t { p = 1; next } /^=== .* ===$/ { p = 0 } p' <<<"$output"
}

# pod_part <text> <pod>: the lines of <text> under "--- <pod> ---", up to the
# next pod line.
pod_part() {
  awk -v t="--- $2 ---" '$0 == t { p = 1; next } /^--- .* ---$/ { p = 0 } p' <<<"$1"
}

# titles: the title lines of $output, in order.
titles() {
  grep '^=== .* ===$' <<<"$output" || true
}

# log_count <fixed string>: the number of stub calls that contain it.
log_count() {
  stub_calls kubectl | grep -cF -- "$1" || true
}

# catch_calls <file>
# One "<timeout>|<calls>|<last line>" line per catch block of <file>: the
# timeout: value of the block's script, the lines whose first text is the
# script's relative path, joined by ";", and the block's last non-empty line,
# each without its leading blanks. A block ends at the next line indented by
# two blanks, or by four that is no list item.
catch_calls() {
  awk '
    /^    catch:$/ { inblk = 1; n++; next }
    inblk && /^(  [^ ]|    [^ -])/ { inblk = 0 }
    !inblk { next }
    /^        timeout:[[:space:]]/ { timeout[n] = $2 }
    { line = $0; sub(/^[[:space:]]+/, "", line) }
    line == "" { next }
    { last[n] = line }
    index(line, "../../lib/dump-identity-backend-diagnostics.sh ") == 1 {
      calls[n] = (n in calls) ? calls[n] ";" line : line
    }
    END { for (i = 1; i <= n; i++) print timeout[i] "|" calls[i] "|" last[i] }' "$1"
}

setup() {
  make_kubectl_stub
  reset_stub
}

# --- Test 1: the sections ---
@test "the six section titles appear once each, in order, and the script exits 0" {
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "the titles are the six of the table, in order" "$TITLES" "$(titles)"
  assert_eq "nothing goes to stderr" "" "$stderr"
}

# --- Test 2: the event sections ---
@test "sections 1 and 2 select by kind and name in the fixed columns, section 3 keeps the last 30" {
  run_check
  assert_eq "section 1 reads the backend events in the fixed columns" "1" \
    "$(log_count "get events -n openstack --field-selector involvedObject.kind=KeystoneIdentityBackend --sort-by=.lastTimestamp -o $COLUMNS_ARG")"
  assert_eq "section 2 reads the Keystone's events in the fixed columns" "1" \
    "$(log_count "get events -n openstack --field-selector involvedObject.kind=Keystone,involvedObject.name=keystone-oidc --sort-by=.lastTimestamp -o $COLUMNS_ARG")"
  assert_eq "section 1 prints all 40 backend events" "$STUB_BACKEND_EVENTS" "$(section 1)"
  assert_eq "section 2 prints the Keystone's event" "$STUB_KEYSTONE_EVENTS" "$(section 2)"
  assert_eq "section 3 sorts the namespace events" "1" \
    "$(log_count "get events -n openstack --sort-by=.lastTimestamp")"
  assert_eq "section 3 keeps the last 30 of 40 events" \
    "$(numbered 'namespace-event-' 11 40)" "$(section 3)"
}

# --- Test 3: the log sections ---
@test "section 4 keeps the backend controller's lines and the Keystone's, per pod" {
  local pod part
  run_check
  for pod in keystone-operator-aaa keystone-operator-bbb; do
    soft assert_eq "section 4 reads 30 minutes of $pod's whole log" "1" \
      "$(log_count "logs -n keystone-system $pod --all-containers --since=30m --tail=-1")"
    part="$(pod_part "$(section 4)" "$pod")"
    soft assert_eq "section 4 keeps the two lines for $pod, in log order" \
      "$BACKEND_LINE"$'\n'"$KEYSTONE_LINE" "$part"
  done
  assert_not_contains "a line of another Keystone is dropped" "$(section 4)" "keystone-saml"
  assert_not_contains "an unrelated line is dropped" "$(section 4)" "Starting workers"
}

@test "section 4 keeps the last 300 of 400 kept lines per pod" {
  STUB_SINCE_LOG="$(numbered '{"controllerKind":"KeystoneIdentityBackend","n":' 1 400)"$'\n'"$UNRELATED_LINE"
  stub_on kubectl 'logs *--since=30m*' --out "$STUB_SINCE_LOG"
  run_check
  assert_eq "the pod's part holds lines 101 to 400" \
    "$(numbered '{"controllerKind":"KeystoneIdentityBackend","n":' 101 400)" \
    "$(pod_part "$(section 4)" keystone-operator-aaa)"
}

@test "section 5 reads 60 lines of the running and the previous containers per pod" {
  local pod
  run_check
  for pod in keystone-operator-aaa keystone-operator-bbb; do
    soft assert_eq "section 5 reads the running containers of $pod" "1" \
      "$(stub_calls kubectl | grep -cxF -- "logs -n keystone-system $pod --all-containers --tail=60" || true)"
    soft assert_eq "section 5 reads the previous containers of $pod" "1" \
      "$(log_count "logs -n keystone-system $pod --all-containers --tail=60 --previous")"
    soft assert_eq "section 5 prints both logs of $pod" $'recent-log-line\nprevious-log-line' \
      "$(pod_part "$(section 5)" "$pod")"
  done
}

# --- Test 4: the metrics section ---
@test "section 6 keeps the seven families of each pod's metrics" {
  local pod
  run_check
  for pod in keystone-operator-aaa keystone-operator-bbb; do
    soft assert_eq "section 6 reads $pod's metrics once, through the pod proxy" "1" \
      "$(log_count "get --raw /api/v1/namespaces/keystone-system/pods/$pod:8080/proxy/metrics")"
    soft assert_eq "section 6 keeps a line of each of the seven families for $pod" "$KEPT_METRICS" \
      "$(pod_part "$(section 6)" "$pod")"
  done
  assert_eq "no proxy path carries the pod/ prefix" "0" "$(log_count "pods/pod/")"
  assert_not_contains "workqueue_adds_total is dropped" "$(section 6)" "workqueue_adds_total"
  assert_not_contains "the reconcile time histogram is dropped" "$(section 6)" \
    "controller_runtime_reconcile_time_seconds_bucket"
  assert_not_contains "a reconcile family outside the list is dropped" "$(section 6)" \
    "controller_runtime_terminal_reconcile_errors_total"
  assert_not_contains "a family whose name ends in a kept one is dropped" "$(section 6)" \
    "keystone_operator_workqueue_depth"
}

# --- Test 5: the empty cases ---
@test "without events the event sections hold only what kubectl printed" {
  local i
  # No events at all: kubectl prints its "No resources found" line on stderr
  # for each of the three reads.
  stub_on kubectl 'get events *' --err "No resources found in openstack namespace."
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "the six titles are printed" "$TITLES" "$(titles)"
  for i in 1 2 3; do
    soft assert_eq "section $i holds kubectl's own line only" \
      "No resources found in openstack namespace." "$(section "$i")"
  done
}

@test "without an operator pod sections 4 to 6 print the empty-list line" {
  local i
  stub_on kubectl 'get pods *' --err "No resources found in keystone-system namespace."
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "the six titles are printed" "$TITLES" "$(titles)"
  for i in 4 5 6; do
    soft assert_eq "section $i prints the empty-list line" \
      "(no operator pod in keystone-system)" "$(section "$i")"
  done
  assert_eq "no log is read" "0" "$(log_count "logs ")"
  assert_eq "no metrics are read" "0" "$(log_count "get --raw")"
}

@test "a log without a kept line prints the no-match line" {
  STUB_SINCE_LOG="$UNRELATED_LINE"$'\n'"$OTHER_KEYSTONE_LINE"
  stub_on kubectl 'logs *--since=30m*' --out "$STUB_SINCE_LOG"
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "the pod's part says no line matched" "(no matching line)" \
    "$(pod_part "$(section 4)" keystone-operator-aaa)"
}

@test "a metrics body without a kept family prints the no-match line" {
  STUB_METRICS="$DROPPED_METRICS"
  stub_on kubectl 'get --raw *' --out "$STUB_METRICS"
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "the pod's part says no line matched" "(no matching line)" \
    "$(pod_part "$(section 6)" keystone-operator-aaa)"
}

# --- Test 6: the failing calls ---
@test "a failing kubectl call prints its error and the script exits 0" {
  stub_on kubectl 'get events *' --rc 1 --err "Error from server (Forbidden): events is forbidden"
  run_check
  assert_eq "failing events exit 0" "0" "$status"
  assert_eq "failing events print the six titles" "$TITLES" "$(titles)"
  assert_eq "section 1 prints kubectl's error" \
    "Error from server (Forbidden): events is forbidden" "$(section 1)"
  assert_eq "section 3 prints kubectl's error" \
    "Error from server (Forbidden): events is forbidden" "$(section 3)"
  assert_contains "section 6 is still read" "$(section 6)" "controller_runtime_active_workers{"

  reset_stub
  stub_on kubectl 'get pods *' --rc 1 --err "Error from server (Forbidden): pods is forbidden"
  run_check
  assert_eq "a failing pod list exits 0" "0" "$status"
  assert_eq "a failing pod list prints the six titles" "$TITLES" "$(titles)"
  assert_eq "section 4 prints kubectl's error" \
    "Error from server (Forbidden): pods is forbidden" "$(section 4)"
  assert_eq "section 6 prints kubectl's error" \
    "Error from server (Forbidden): pods is forbidden" "$(section 6)"

  reset_stub
  # Every log read fails but the one of the previous containers.
  stub_on kubectl 'logs *' --rc 1 --err 'Error from server (BadRequest): previous terminated container "manager" not found'
  stub_on kubectl 'logs *--previous*' --out "$STUB_PREVIOUS_LOG"
  run_check
  assert_eq "failing logs exit 0" "0" "$status"
  assert_eq "failing logs print the six titles" "$TITLES" "$(titles)"
  assert_eq "section 4 prints kubectl's error, not the no-match line" \
    'Error from server (BadRequest): previous terminated container "manager" not found' \
    "$(pod_part "$(section 4)" keystone-operator-aaa)"
  assert_contains "section 5 prints the previous log" \
    "$(pod_part "$(section 5)" keystone-operator-aaa)" "previous-log-line"

  reset_stub
  stub_on kubectl 'logs *--previous*' --rc 1 --err 'Error from server (BadRequest): previous terminated container "manager" not found'
  run_check
  assert_eq "a failing --previous exits 0" "0" "$status"
  assert_eq "a failing --previous prints the six titles" "$TITLES" "$(titles)"
  assert_eq "section 5 prints the running log and kubectl's error" \
    $'recent-log-line\nError from server (BadRequest): previous terminated container "manager" not found' \
    "$(pod_part "$(section 5)" keystone-operator-bbb)"

  reset_stub
  stub_on kubectl 'get --raw *' --rc 1 --err "Error from server (ServiceUnavailable): the server is currently unable to handle the request"
  run_check
  assert_eq "an unreachable metrics endpoint exits 0" "0" "$status"
  assert_eq "an unreachable metrics endpoint prints the six titles" "$TITLES" "$(titles)"
  assert_eq "section 6 prints kubectl's error and names the pod" \
    $'Error from server (ServiceUnavailable): the server is currently unable to handle the request\n(metrics endpoint of keystone-operator-aaa unreachable)' \
    "$(pod_part "$(section 6)" keystone-operator-aaa)"
}

# --- Test 7: the arguments ---
@test "a wrong argument count or an empty argument exits 2 with the usage line" {
  local args words
  for args in "openstack" "openstack keystone-oidc extra" "|keystone-oidc" "openstack|"; do
    reset_stub
    case "$args" in
      *"|"*) run_check "${args%%|*}" "${args#*|}" ;;
      *) read -ra words <<<"$args" && run_check "${words[@]}" ;;
    esac
    soft assert_eq "'$args' exits 2" "2" "$status"
    soft assert_eq "'$args' prints the usage line on stderr" "$USAGE" "$stderr"
    soft assert_eq "'$args' prints nothing on stdout" "" "$output"
    soft assert_eq "'$args' calls no kubectl" "0" "$(log_count "")"
  done
}

@test "the script parses" {
  run bash -n "$SCRIPT"
  assert_eq "bash -n passes" "0" "$status"
}

# --- Test 8: the path filter ---
@test "an edit to the script runs the keystone e2e-operator job" {
  assert_contains "tests_e2e_keystone lists the shared directory" \
    "$(filter_block tests_e2e_keystone)" "'tests/e2e/lib/**'"
}

# --- Test 9: the callers ---
@test "every catch block of the three suites ends with the call under a 90s timeout" {
  local suite keystone count file call expected i old
  while IFS='|' read -r suite keystone count; do
    file="$PROJECT_ROOT/tests/e2e/keystone/$suite/chainsaw-test.yaml"
    call="../../lib/dump-identity-backend-diagnostics.sh \$NAMESPACE $keystone || true"
    soft assert_eq "$suite has $count catch blocks" "$count" "$(grep -c '^    catch:$' "$file" || true)"
    expected=""
    for ((i = 0; i < count; i++)); do
      expected+="90s|$call|$call"$'\n'
    done
    soft assert_eq "each catch block of $suite ends with the call for $keystone, under a 90s timeout" \
      "${expected%$'\n'}" "$(catch_calls "$file")"
    for old in 'keystone-operator --all-containers --tail=' "--sort-by='.lastTimestamp' 2>&1 | tail -30" \
      'echo "=== operator logs ==="' 'echo "=== Events ==="'; do
      soft assert_eq "$suite no longer holds: $old" "0" "$(grep -cF -- "$old" "$file" || true)"
    done
  done <<<"$DUMP_CALLERS"
}

@test "no suite outside the three calls the script" {
  local expected found
  expected="$(cut -d'|' -f1 <<<"$DUMP_CALLERS" | sed 's|^|keystone/|' | LC_ALL=C sort)"
  # Any line that is not a comment and names the script counts as a call.
  found="$(grep -rlE --include=chainsaw-test.yaml \
    '^[[:space:]]*[^#[:space:]].*dump-identity-backend-diagnostics\.sh' "$PROJECT_ROOT/tests" |
    sed -e "s|^$PROJECT_ROOT/tests/e2e/||" -e 's|/chainsaw-test.yaml$||' | LC_ALL=C sort)"
  assert_eq "the suites that call the script are the three" "$expected" "$found"
}
