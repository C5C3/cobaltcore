#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/e2e/lib/assert-operator-metric-families.sh, the check that an
# operator release exports its own Prometheus families:
#   - a two-pod release whose samples all sit on one pod passes every check,
#     because the script judges the union of the bodies
#   - each check fails on its own evidence: no pod or a pod list kubectl
#     cannot read, an unreachable or empty endpoint (named), a reconcile
#     duration count that is absent or zero, an errors line labelled
#     condition_type="UNKNOWN" (echoed), and a sample that is missing, a
#     counter at 0, or present only under a longer label value, a longer
#     label name, or split at its own comma across two values
#   - a missing sample names each pod whose containers restarted, also
#     during the wait, from a pod list read again at the failure, or from
#     the first one when that second read fails
#   - condition_type="UNKNOWNX" passes, and so does a sample with its labels
#     in another order, in exponent form, with a comma or a brace in a value,
#     a gauge at 0, or a count that appears on a later read, even after a
#     read that failed in between, and a duration count without labels
#   - fewer than three arguments, an empty argument or a malformed sample
#     exits 2 with the usage line
#   - the metrics port it reads matches the nine charts
#   - an edit to the script runs the e2e-operator jobs of every operator but
#     c5c3, whose suites do not call it
# Usage: bash tests/unit/ci/assert_operator_metric_families_test.sh

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

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-operator-metric-families.sh"
# The nine operators whose charts the script reads metrics from.
OPERATORS="barbican cinder glance horizon keystone neutron nova ovn placement"

STUBS="$(mktemp -d)"
trap 'rm -rf "$STUBS"' EXIT

# make_kubectl_stub <dir>
# Writes a kubectl stub that answers the two calls the script makes:
#   get pods   prints STUB_PODS, one "<name> <restart count>..." line per pod,
#              or fails with STUB_PODS_RC; from the second call since the
#              last reset_stub, STUB_PODS_LATER when set, or fails with
#              STUB_PODS_LATER_RC
#   get --raw  serves the metrics of the pod in the path, which must be
#              /api/v1/namespaces/keystone-system/pods/<pod>:8080/proxy/metrics:
#              on the pod's n-th read since the last reset_stub,
#              <dir>/raw-<n>/<pod> when that body or its .rc file exists,
#              else <dir>/raw-later/<pod> from the second read on when that
#              file exists, else <dir>/raw/<pod>. A <pod>.rc file beside the
#              body makes the read fail with its content as the exit code.
# A sleep stub beside it returns at once, so the re-reads cost no time.
make_kubectl_stub() {
  local dir="$1"

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
case "$1 $2" in
  "get pods")
    lists=0
    if [ -e "$dir/pods.reads" ]; then lists="$(cat "$dir/pods.reads")"; fi
    lists=$((lists + 1))
    echo "$lists" >"$dir/pods.reads"
    list="$STUB_PODS"
    rc="${STUB_PODS_RC:-0}"
    if [ "$lists" -gt 1 ]; then
      list="${STUB_PODS_LATER:-$STUB_PODS}"
      rc="${STUB_PODS_LATER_RC:-0}"
    fi
    if [ "$rc" -ne 0 ]; then
      echo "Error from server (Forbidden): pods is forbidden" >&2
      exit "$rc"
    fi
    if [ -n "$list" ]; then printf '%s\n' "$list"; fi
    ;;
  "get --raw")
    pod="$(sed -n 's|^/api/v1/namespaces/keystone-system/pods/\([^:/]*\):8080/proxy/metrics$|\1|p' <<<"$3")"
    if [ -z "$pod" ]; then
      echo "Error from server (NotFound): the server could not find the requested resource" >&2
      exit 1
    fi
    reads=0
    if [ -e "$dir/raw/$pod.reads" ]; then reads="$(cat "$dir/raw/$pod.reads")"; fi
    reads=$((reads + 1))
    echo "$reads" >"$dir/raw/$pod.reads"
    src="$dir/raw"
    if [ -e "$dir/raw-$reads/$pod" ] || [ -e "$dir/raw-$reads/$pod.rc" ]; then
      src="$dir/raw-$reads"
    elif [ "$reads" -gt 1 ] && [ -e "$dir/raw-later/$pod" ]; then
      src="$dir/raw-later"
    fi
    if [ -e "$src/$pod.rc" ]; then
      echo "Error from server (ServiceUnavailable): the server is currently unable to handle the request" >&2
      exit "$(cat "$src/$pod.rc")"
    fi
    if [ -e "$src/$pod" ]; then cat "$src/$pod"; fi
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
  printf '#!/bin/bash\nexit 0\n' >"$dir/sleep"
  chmod +x "$dir/sleep"
}

DURATION_LINE='keystone_operator_reconcile_duration_seconds_count{sub_reconciler="Database"} 7'
ERRORS_LINE='keystone_operator_reconcile_errors_total{condition_type="DatabaseReady",sub_reconciler="Database"} 2'
DB_SYNC_LINE='keystone_operator_db_sync_total{keystone="keystone-basic",namespace="openstack",result="succeeded"} 1'
DB_SYNC_SAMPLE='keystone_operator_db_sync_total{keystone="keystone-basic",namespace="openstack",result="succeeded"}'
AGE_SAMPLE='keystone_operator_key_rotation_age_seconds{keystone="keystone-fernet",namespace="openstack",key_type="fernet"}'

# set_metrics <pod> <body> / set_later_metrics <pod> <body>
# The body a pod serves on its first read, and on every later one.
set_metrics() {
  printf '%s\n' "$2" >"$STUBS/raw/$1"
}
set_later_metrics() {
  printf '%s\n' "$2" >"$STUBS/raw-later/$1"
}

# reset_stub
# A healthy keystone-operator release of two pods: op-0 is the standby and
# serves only the Go runtime families, op-1 leads and serves the reconcile
# duration and error families and the db_sync counter. The checks read once
# and do not wait (METRICS_WAIT=0).
reset_stub() {
  rm -rf "$STUBS"/raw* "$STUBS/pods.reads"
  mkdir -p "$STUBS/raw" "$STUBS/raw-later"
  STUB_PODS="op-0 0"$'\n'"op-1 0"
  STUB_PODS_RC=0
  STUB_PODS_LATER=""
  STUB_PODS_LATER_RC=0
  METRICS_WAIT=0
  set_metrics op-0 "# TYPE go_goroutines gauge"$'\n''go_goroutines 42'
  set_metrics op-1 "# TYPE keystone_operator_reconcile_duration_seconds histogram"$'\n'"$DURATION_LINE"$'\n'"$ERRORS_LINE"$'\n'"$DB_SYNC_LINE"
}

# run_check [args...]
# Runs the script with the kubectl stub first on PATH. Leaves its stdout in
# RUN_OUT and its exit code in RUN_RC, and writes its stderr to $STUBS/stderr.
# Without arguments it runs the release check with the db_sync sample.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- keystone-system keystone-operator keystone_operator "$DB_SYNC_SAMPLE"
  fi
  RUN_OUT="$(
    PATH="$STUBS:$PATH" \
      STUB_PODS="$STUB_PODS" STUB_PODS_RC="$STUB_PODS_RC" \
      STUB_PODS_LATER="$STUB_PODS_LATER" STUB_PODS_LATER_RC="$STUB_PODS_LATER_RC" \
      METRICS_WAIT_SECONDS="$METRICS_WAIT" \
      bash "$SCRIPT" "$@" 2>"$STUBS/stderr"
  )"
  RUN_RC=$?
}

# run_sample <sample> <line>
# Runs the release check with one sample against op-1 serving the reconcile
# families plus <line>.
run_sample() {
  set_metrics op-1 "$DURATION_LINE"$'\n'"$ERRORS_LINE"$'\n'"$2"
  run_check keystone-system keystone-operator keystone_operator "$1"
}

make_kubectl_stub "$STUBS"

# --- Test 1: the happy path ---
test_a_two_pod_release_passes_on_the_union() {
  echo "Test: a two-pod release whose samples sit on one pod passes every check"

  reset_stub
  run_check keystone-system keystone-operator keystone_operator "$DB_SYNC_SAMPLE" \
    'keystone_operator_reconcile_errors_total{sub_reconciler="Database",condition_type="DatabaseReady"}'
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_eq "it prints six OK: lines, four checks and two samples" "6" \
    "$(grep -c '^OK:' <<<"$RUN_OUT" || true)"
  assert_contains "it names both pods" "$RUN_OUT" "OK: release keystone-operator runs 2 pods: op-0 op-1"
  assert_contains "it names the sample" "$RUN_OUT" "OK: sample $DB_SYNC_SAMPLE"
  assert_not_contains "it prints no FAIL: line" "$RUN_OUT" "FAIL:"
}

test_a_release_without_samples_passes() {
  echo "Test: a release check without samples passes on checks 1 to 4"

  reset_stub
  run_check keystone-system keystone-operator keystone_operator
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_eq "it prints four OK: lines" "4" "$(grep -c '^OK:' <<<"$RUN_OUT" || true)"
}

# --- Test 2: check 1, the pod list ---
test_a_release_without_a_pod_fails() {
  echo "Test: a release without a pod fails check 1"

  reset_stub
  STUB_PODS=""
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the missing pod" "$RUN_OUT" \
    "FAIL: no pod of release keystone-operator in keystone-system"
}

test_an_unlistable_release_fails() {
  echo "Test: a pod list kubectl cannot read fails check 1 with kubectl's error"

  reset_stub
  STUB_PODS_RC=1
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the pods cannot be listed" "$RUN_OUT" \
    "FAIL: cannot list the pods of release keystone-operator"
  assert_contains "kubectl's own error reaches stderr" "$(cat "$STUBS/stderr")" "Forbidden"
}

# --- Test 3: check 2, the metrics endpoints ---
test_an_unreachable_pod_fails() {
  echo "Test: a pod whose metrics read fails fails check 2 and is named"

  reset_stub
  echo 1 >"$STUBS/raw/op-1.rc"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the pod" "$RUN_OUT" "FAIL: metrics endpoint of pod op-1 unreachable"
  assert_contains "kubectl's own error reaches stderr" "$(cat "$STUBS/stderr")" "ServiceUnavailable"
}

test_an_empty_body_fails() {
  echo "Test: a pod that answers an empty body fails check 2 and is named"

  reset_stub
  : >"$STUBS/raw/op-0"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the pod" "$RUN_OUT" "FAIL: metrics endpoint of pod op-0 returned nothing"
}

test_a_read_failing_at_the_deadline_fails() {
  echo "Test: a re-read still failing at the deadline fails check 2"

  # The first read lacks the sample, every later read of op-1 fails.
  reset_stub
  set_metrics op-1 "$DURATION_LINE"
  echo 1 >"$STUBS/raw-later/op-1.rc"
  : >"$STUBS/raw-later/op-1"
  METRICS_WAIT=1
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the pod" "$RUN_OUT" "FAIL: metrics endpoint of pod op-1 unreachable"
}

test_a_transient_read_failure_recovers() {
  echo "Test: a re-read that fails once and then recovers passes"

  # The first read lacks the sample, the second read of op-1 fails, and the
  # third carries the sample, as across an operator restart.
  reset_stub
  set_metrics op-1 "$DURATION_LINE"
  mkdir -p "$STUBS/raw-2"
  echo 1 >"$STUBS/raw-2/op-1.rc"
  set_later_metrics op-1 "$DURATION_LINE"$'\n'"$DB_SYNC_LINE"
  METRICS_WAIT=10
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "the second read did fail" "$(cat "$STUBS/stderr")" "ServiceUnavailable"
  assert_contains "the sample passes" "$RUN_OUT" "OK: sample $DB_SYNC_SAMPLE"
  assert_not_contains "it prints no FAIL: line" "$RUN_OUT" "FAIL:"
}

# --- Test 4: check 3, the reconcile duration count ---
test_an_absent_duration_count_fails() {
  echo "Test: an absent reconcile duration count fails check 3"

  reset_stub
  set_metrics op-1 "$ERRORS_LINE"$'\n'"$DB_SYNC_LINE"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the family" "$RUN_OUT" \
    "FAIL: no keystone_operator_reconcile_duration_seconds_count line of release keystone-operator counts a reconcile after 0s"
}

test_a_zero_duration_count_fails() {
  echo "Test: a reconcile duration count whose only value is 0 fails check 3"

  reset_stub
  set_metrics op-1 'keystone_operator_reconcile_duration_seconds_count{sub_reconciler="Database"} 0'$'\n'"$DB_SYNC_LINE"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it fails check 3" "$RUN_OUT" \
    "FAIL: no keystone_operator_reconcile_duration_seconds_count line"
  assert_contains "it prints the zero line" "$RUN_OUT" \
    'keystone_operator_reconcile_duration_seconds_count{sub_reconciler="Database"} 0'
}

test_a_later_duration_count_passes() {
  echo "Test: a reconcile duration count that appears on a later read passes"

  reset_stub
  set_metrics op-1 "$DB_SYNC_LINE"
  set_later_metrics op-1 "$DURATION_LINE"$'\n'"$DB_SYNC_LINE"
  METRICS_WAIT=10
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "check 3 passes" "$RUN_OUT" \
    "OK: keystone_operator_reconcile_duration_seconds_count counts a reconcile"
}

test_an_unlabelled_duration_count_passes() {
  echo "Test: a reconcile duration count without labels passes check 3"

  reset_stub
  set_metrics op-1 'keystone_operator_reconcile_duration_seconds_count 3'
  run_check keystone-system keystone-operator keystone_operator
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "check 3 passes" "$RUN_OUT" \
    "OK: keystone_operator_reconcile_duration_seconds_count counts a reconcile"
}

# --- Test 5: check 4, the UNKNOWN condition type ---
test_an_unknown_condition_type_fails() {
  echo "Test: an errors line labelled condition_type=\"UNKNOWN\" fails check 4 and is printed"

  local line='keystone_operator_reconcile_errors_total{condition_type="UNKNOWN",sub_reconciler="Rotation"} 1'
  reset_stub
  set_metrics op-1 "$DURATION_LINE"$'\n'"$ERRORS_LINE"$'\n'"$line"$'\n'"$DB_SYNC_LINE"
  run_check
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it fails check 4" "$RUN_OUT" \
    "FAIL: keystone_operator_reconcile_errors_total carries condition_type=\"UNKNOWN\""
  assert_contains "it prints the line" "$RUN_OUT" "$line"
  assert_not_contains "it leaves the mapped line out" "$RUN_OUT" "$ERRORS_LINE"
}

test_a_longer_condition_type_passes() {
  echo "Test: condition_type=\"UNKNOWNX\" passes check 4"

  reset_stub
  set_metrics op-1 "$DURATION_LINE"$'\n''keystone_operator_reconcile_errors_total{condition_type="UNKNOWNX",sub_reconciler="Rotation"} 1'$'\n'"$DB_SYNC_LINE"
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "check 4 passes" "$RUN_OUT" \
    "OK: no keystone_operator_reconcile_errors_total line carries condition_type=\"UNKNOWN\""
}

# --- Test 6: check 5, samples that pass ---
test_a_sample_in_another_label_order_passes() {
  echo "Test: a sample passes with its labels in another order"

  # client_golang prints labels in name order: key_type before keystone.
  reset_stub
  run_sample "$AGE_SAMPLE" \
    'keystone_operator_key_rotation_age_seconds{key_type="fernet",keystone="keystone-fernet",namespace="openstack"} 12.5'
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "the sample passes" "$RUN_OUT" "OK: sample $AGE_SAMPLE"
}

test_an_exponent_value_passes() {
  echo "Test: a counter sample in exponent form passes"

  reset_stub
  run_sample "$DB_SYNC_SAMPLE" \
    'keystone_operator_db_sync_total{keystone="keystone-basic",namespace="openstack",result="succeeded"} 1.2e+06'
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "the sample passes" "$RUN_OUT" "OK: sample $DB_SYNC_SAMPLE"
}

test_a_gauge_at_zero_passes() {
  echo "Test: a gauge sample at 0 passes"

  # A rotation that just ran leaves an age of 0.
  reset_stub
  run_sample "$AGE_SAMPLE" \
    'keystone_operator_key_rotation_age_seconds{key_type="fernet",keystone="keystone-fernet",namespace="openstack"} 0'
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "the sample passes" "$RUN_OUT" "OK: sample $AGE_SAMPLE"
}

test_a_later_sample_passes() {
  echo "Test: a sample that appears on a later read passes"

  # The operator records a finished Job on its next pass.
  reset_stub
  set_metrics op-1 "$DURATION_LINE"
  set_later_metrics op-1 "$DURATION_LINE"$'\n'"$DB_SYNC_LINE"
  METRICS_WAIT=10
  run_check
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "the sample passes" "$RUN_OUT" "OK: sample $DB_SYNC_SAMPLE"
}

test_a_label_value_with_a_comma_matches_whole() {
  echo "Test: a label value holding a comma matches only as a whole"

  local sample='keystone_operator_db_sync_total{keystone="a,b",result="succeeded"}'
  reset_stub
  run_sample "$sample" 'keystone_operator_db_sync_total{keystone="a,b",result="succeeded"} 1'
  assert_eq "the value a,b passes" "0" "$RUN_RC"
  assert_contains "the sample passes" "$RUN_OUT" "OK: sample $sample"

  # Split at its comma, the pair would find keystone="a and b" in two values.
  run_sample "$sample" 'keystone_operator_db_sync_total{keystone="a,x",result="succeeded",y="z,b"} 1'
  assert_eq "the values a,x and z,b fail" "1" "$RUN_RC"
  assert_contains "it names the sample" "$RUN_OUT" "FAIL: sample $sample"
}

test_a_label_value_with_a_brace_passes() {
  echo "Test: a label value holding a closing brace passes"

  local sample='keystone_operator_db_sync_total{keystone="a}b",result="succeeded"}'
  reset_stub
  run_sample "$sample" 'keystone_operator_db_sync_total{keystone="a}b",result="succeeded"} 1'
  assert_eq "the script exits 0" "0" "$RUN_RC"
  assert_contains "the sample passes" "$RUN_OUT" "OK: sample $sample"
}

# --- Test 7: check 5, samples that fail ---
test_a_counter_at_zero_fails() {
  echo "Test: a _total sample at 0 fails"

  reset_stub
  run_sample "$DB_SYNC_SAMPLE" \
    'keystone_operator_db_sync_total{keystone="keystone-basic",namespace="openstack",result="succeeded"} 0'
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the sample" "$RUN_OUT" \
    "FAIL: sample $DB_SYNC_SAMPLE has no line with a value of 1 or more after 0s"
  assert_contains "it prints the family's line" "$RUN_OUT" "result=\"succeeded\"} 0"
}

test_a_missing_sample_fails() {
  echo "Test: a sample without a line fails and the other samples still report"

  reset_stub
  run_check keystone-system keystone-operator keystone_operator "$AGE_SAMPLE" "$DB_SYNC_SAMPLE"
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the sample" "$RUN_OUT" \
    "FAIL: sample $AGE_SAMPLE has no line with a numeric value after 0s"
  assert_contains "it says the family is absent" "$RUN_OUT" \
    "no line of family keystone_operator_key_rotation_age_seconds"
  assert_contains "the present sample still passes" "$RUN_OUT" "OK: sample $DB_SYNC_SAMPLE"
}

test_a_longer_label_value_fails() {
  echo "Test: keystone=\"keystone-basic-2026-2\" does not satisfy keystone=\"keystone-basic\""

  reset_stub
  run_sample "$DB_SYNC_SAMPLE" \
    'keystone_operator_db_sync_total{keystone="keystone-basic-2026-2",namespace="openstack",result="succeeded"} 1'
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the sample" "$RUN_OUT" "FAIL: sample $DB_SYNC_SAMPLE"
}

test_a_longer_label_name_fails() {
  echo "Test: subnamespace=\"openstack\" does not satisfy namespace=\"openstack\""

  reset_stub
  run_sample "$DB_SYNC_SAMPLE" \
    'keystone_operator_db_sync_total{keystone="keystone-basic",result="succeeded",subnamespace="openstack"} 1'
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the sample" "$RUN_OUT" "FAIL: sample $DB_SYNC_SAMPLE"
}

test_a_longer_family_name_fails() {
  echo "Test: a line of a longer family name does not satisfy the sample"

  reset_stub
  run_sample 'keystone_operator_db_sync{keystone="keystone-basic"}' "$DB_SYNC_LINE"
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it says the family is absent" "$RUN_OUT" \
    "no line of family keystone_operator_db_sync"
}

test_a_restarted_pod_is_named_when_a_sample_is_missing() {
  echo "Test: a missing sample names the pods that restarted, and only those"

  # op-1 runs two containers that restarted once and twice.
  reset_stub
  STUB_PODS="op-0 0"$'\n'"op-1 1 2"
  run_check keystone-system keystone-operator keystone_operator "$AGE_SAMPLE"
  assert_eq "the script exits 1" "1" "$RUN_RC"
  assert_contains "it names the restarted pod with its restarts summed" "$RUN_OUT" \
    "FAIL: pod op-1 restarted 3 times; in-memory counters recorded before the restart are gone"
  assert_not_contains "it leaves the other pod out" "$RUN_OUT" "pod op-0 restarted"

  # op-1 restarted after the first pod list, during the wait.
  reset_stub
  STUB_PODS_LATER="op-0 0"$'\n'"op-1 1"
  run_check keystone-system keystone-operator keystone_operator "$AGE_SAMPLE"
  assert_eq "a restart during the wait exits 1" "1" "$RUN_RC"
  assert_contains "it names the pod that restarted during the wait" "$RUN_OUT" \
    "FAIL: pod op-1 restarted 1 times; in-memory counters recorded before the restart are gone"

  # The second pod list fails, so the restarts of the first one stand.
  reset_stub
  STUB_PODS="op-0 0"$'\n'"op-1 2"
  STUB_PODS_LATER_RC=1
  run_check keystone-system keystone-operator keystone_operator "$AGE_SAMPLE"
  assert_eq "a failed second pod list exits 1" "1" "$RUN_RC"
  assert_contains "it names the restart of the first pod list" "$RUN_OUT" \
    "FAIL: pod op-1 restarted 2 times; in-memory counters recorded before the restart are gone"
  assert_contains "kubectl's own error reaches stderr" "$(cat "$STUBS/stderr")" "Forbidden"

  reset_stub
  STUB_PODS="op-0 0"$'\n'"op-1 2"
  run_check
  assert_eq "a present sample exits 0 despite the restart" "0" "$RUN_RC"
  assert_not_contains "it names no restart" "$RUN_OUT" "restarted"
}

# --- Test 8: the arguments ---
test_wrong_arguments_exit_2() {
  echo "Test: fewer than three arguments, an empty one or a malformed sample exits 2"

  local usage="usage: assert-operator-metric-families.sh <operator-namespace> <release> <prefix> [<sample>...]"
  reset_stub
  run_check keystone-system keystone-operator
  assert_eq "two arguments exit 2" "2" "$RUN_RC"
  assert_contains "the usage line goes to stderr" "$(cat "$STUBS/stderr")" "$usage"
  assert_eq "nothing goes to stdout" "" "$RUN_OUT"

  run_check keystone-system "" keystone_operator
  assert_eq "an empty argument exits 2" "2" "$RUN_RC"
  assert_contains "the usage line goes to stderr" "$(cat "$STUBS/stderr")" "$usage"

  run_check keystone-system keystone-operator keystone_operator "$DB_SYNC_SAMPLE" ""
  assert_eq "an empty sample exits 2" "2" "$RUN_RC"

  run_check keystone-system keystone-operator keystone_operator 'db_sync_total{keystone}'
  assert_eq "a pair without a value exits 2" "2" "$RUN_RC"
  assert_contains "the usage line goes to stderr" "$(cat "$STUBS/stderr")" "$usage"
  assert_eq "nothing goes to stdout" "" "$RUN_OUT"

  run_check keystone-system keystone-operator keystone_operator 'db_sync_total'
  assert_eq "a sample without labels exits 2" "2" "$RUN_RC"
}

test_the_script_parses() {
  echo "Test: the script parses"

  bash -n "$SCRIPT"
  assert_eq "bash -n passes" "0" "$?"
}

# --- Test 9: the metrics port ---
test_the_port_matches_the_charts() {
  echo "Test: the metrics port the script reads matches the nine charts"

  local op port
  port="$(sed -n 's/^metrics_port=//p' "$SCRIPT")"
  assert_not_empty "the script names its metrics port" "$port"
  for op in $OPERATORS; do
    assert_eq "the $op chart serves metrics on the script's port" "$port" \
      "$(yq '.metrics.port' "$PROJECT_ROOT/operators/$op/helm/$op-operator/values.yaml")"
  done
}

# --- Test 10: the path filters ---
test_the_filters_name_the_shared_scripts() {
  echo "Test: an edit to the script runs every leg but c5c3"

  local op block script=assert-operator-metric-families.sh
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

# --- Run ---
test_a_two_pod_release_passes_on_the_union
test_a_release_without_samples_passes
test_a_release_without_a_pod_fails
test_an_unlistable_release_fails
test_an_unreachable_pod_fails
test_an_empty_body_fails
test_a_read_failing_at_the_deadline_fails
test_a_transient_read_failure_recovers
test_an_absent_duration_count_fails
test_a_zero_duration_count_fails
test_a_later_duration_count_passes
test_an_unlabelled_duration_count_passes
test_an_unknown_condition_type_fails
test_a_longer_condition_type_passes
test_a_sample_in_another_label_order_passes
test_an_exponent_value_passes
test_a_gauge_at_zero_passes
test_a_later_sample_passes
test_a_label_value_with_a_comma_matches_whole
test_a_label_value_with_a_brace_passes
test_a_counter_at_zero_fails
test_a_missing_sample_fails
test_a_longer_label_value_fails
test_a_longer_label_name_fails
test_a_longer_family_name_fails
test_a_restarted_pod_is_named_when_a_sample_is_missing
test_wrong_arguments_exit_2
test_the_script_parses
test_the_port_matches_the_charts

test_the_filters_name_the_shared_scripts

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
