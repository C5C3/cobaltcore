#!/usr/bin/env bats
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
#   - a non-zero wait re-reads at least once, even when its deadline passes
#     during the first check
#   - a missing sample names each pod whose containers restarted, also
#     during the wait, from a pod list read again at the failure, or from
#     the first one when that second read fails
#   - condition_type="UNKNOWNX" passes, and so does a sample with its labels
#     in another order, in exponent form, with a comma or a brace in a value,
#     a gauge at 0, or a count that appears on a later read, even after a
#     read that failed in between, and a duration count without labels
#   - fewer than three arguments, an empty argument or a malformed sample
#     exits 2 with the usage line
#   - the metrics port it reads matches the ten charts
#   - an edit to the script runs the e2e-operator jobs of every operator
#   - the 17 suites call it with exactly the arguments of their table row,
#     each in a step with a 120s timeout, and no other suite calls it
# Usage: bin/bats tests/unit/ci/assert_operator_metric_families.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SCRIPT="$PROJECT_ROOT/tests/e2e/lib/assert-operator-metric-families.sh"
# The ten operators whose charts the script reads metrics from.
OPERATORS="barbican c5c3 cinder glance horizon keystone neutron nova ovn placement"

# kubectl_raw_body
# Prints the --exec body of the kubectl rule for get --raw, which
# make_kubectl_stub describes. It keeps its read counters and finds its
# bodies under $STUB_DIR/state, which the tests know as STUBS.
kubectl_raw_body() {
  cat <<'STUB'
dir="$STUB_DIR/state"
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
exit 0
STUB
}

# make_kubectl_stub
# Declares the kubectl rules for the two calls the script makes; every other
# call exits 0:
#   get pods   reset_stub declares the answer, one "<name> <restart count>..."
#              line per pod. A test declares a list that kubectl cannot read
#              with --rc 1 and kubectl's Forbidden error, and a first list
#              that differs from the later ones as a --times 1 rule after
#              the lasting one.
#   get --raw  serves the metrics of the pod in the path, which must be
#              /api/v1/namespaces/keystone-system/pods/<pod>:8080/proxy/metrics:
#              on the pod's n-th read since the last reset_stub,
#              $STUBS/raw-<n>/<pod> when that body or its .rc file exists,
#              else $STUBS/raw-later/<pod> from the second read on when that
#              file exists, else $STUBS/raw/<pod>. A <pod>.rc file beside the
#              body makes the read fail with its content as the exit code.
# A sleep stub beside it returns at once, so the re-reads cost no time.
make_kubectl_stub() {
  stub_on kubectl '*'
  stub_on kubectl 'get --raw *' --exec "$(kubectl_raw_body)"
  stub_on sleep '*'
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
# and do not wait (METRICS_WAIT=0). setup calls it, and a test calls it
# again to start over from this release; its rules are then the newest.
reset_stub() {
  rm -rf "$STUBS"/raw*
  mkdir -p "$STUBS/raw" "$STUBS/raw-later"
  stub_on kubectl 'get pods *' --out "op-0 0"$'\n'"op-1 0"
  METRICS_WAIT=0
  set_metrics op-0 "# TYPE go_goroutines gauge"$'\n''go_goroutines 42'
  set_metrics op-1 "# TYPE keystone_operator_reconcile_duration_seconds histogram"$'\n'"$DURATION_LINE"$'\n'"$ERRORS_LINE"$'\n'"$DB_SYNC_LINE"
}

# run_check [args...]
# Runs the script through run with the stubs first on PATH: its stdout in
# $output, its stderr in $stderr and its exit code in $status. Without
# arguments it runs the release check with the db_sync sample.
run_check() {
  if [ "$#" -eq 0 ]; then
    set -- keystone-system keystone-operator keystone_operator "$DB_SYNC_SAMPLE"
  fi
  run --separate-stderr with_stubs env \
      METRICS_WAIT_SECONDS="$METRICS_WAIT" \
      bash "$SCRIPT" "$@"
}

# run_sample <sample> <line>
# Runs the release check with one sample against op-1 serving the reconcile
# families plus <line>.
run_sample() {
  set_metrics op-1 "$DURATION_LINE"$'\n'"$ERRORS_LINE"$'\n'"$2"
  run_check keystone-system keystone-operator keystone_operator "$1"
}

# metric_calls <file>
# Every call of the script in <file>, one "<timeout>|<call>" line each: the
# last timeout: value since the step opener "  - try:", and a line whose
# first text is the script's relative path, joined with its backslash
# continuations, blanks squeezed.
metric_calls() {
  awk '
    /^  - try:$/ { timeout = "" }
    !cont && /^[[:space:]]+timeout:[[:space:]]/ { timeout = $2 }
    cont { line = line " " $0 }
    !cont && /^[[:space:]]*\.\.\/\.\.\/lib\/assert-operator-metric-families\.sh / { line = $0; cont = 1 }
    cont {
      if (line ~ /\\$/) { sub(/\\$/, "", line); next }
      gsub(/[[:space:]]+/, " ", line); sub(/^ /, "", line); sub(/ $/, "", line)
      print timeout "|" line
      cont = 0
    }' "$1"
}

# The 17 suites that call the script, each with the arguments it passes after
# the script's path. A suite reads a sample after the step that completes its
# Job, rotation or re-mint, and before any step that deletes its CR.
METRIC_CALLERS="$(cat <<'CALLS'
barbican/basic-deployment|barbican-system barbican-operator barbican_operator 'barbican_operator_db_sync_total{barbican="barbican-basic",namespace="openstack",result="succeeded"}' 'barbican_operator_db_sync_duration_seconds_count{barbican="barbican-basic",namespace="openstack"}' 'barbican_operator_db_clean_total{barbican="barbican-basic",namespace="openstack",result="succeeded"}' 'barbican_operator_db_clean_duration_seconds_count{barbican="barbican-basic",namespace="openstack"}'
barbican/secretstore-managed|barbican-system barbican-operator barbican_operator 'barbican_operator_secretstore_remints_total{store="barbican-store-managed-store",namespace="openstack",trigger="proactive"}'
c5c3/sizing-profile|c5c3-system c5c3-operator c5c3_operator 'c5c3_operator_reconcile_duration_seconds_count{sub_reconciler="Sizing"}' 'c5c3_operator_reconcile_errors_total{sub_reconciler="Sizing",condition_type="SizingReady"}'
cinder/basic-deployment|cinder-system cinder-operator cinder_operator 'cinder_operator_db_sync_total{cinder="cinder-basic",namespace="openstack",result="succeeded"}' 'cinder_operator_db_sync_duration_seconds_count{cinder="cinder-basic",namespace="openstack"}'
cinder/pod-security-restricted|cinder-system cinder-operator cinder_operator 'cinder_operator_db_purge_total{cinder="cinder-pss",namespace="cinder-pss",result="succeeded"}' 'cinder_operator_service_remove_total{cinder="cinder-pss",namespace="cinder-pss",result="succeeded"}'
glance/basic-deployment|glance-system glance-operator glance_operator 'glance_operator_db_sync_total{glance="glance-basic",namespace="openstack",result="succeeded"}' 'glance_operator_db_sync_duration_seconds_count{glance="glance-basic",namespace="openstack"}' 'glance_operator_db_purge_total{glance="glance-basic",namespace="openstack",result="succeeded"}' 'glance_operator_db_purge_duration_seconds_count{glance="glance-basic",namespace="openstack"}'
horizon/basic-deployment|horizon-system horizon-operator horizon_operator
keystone/basic-deployment|keystone-system keystone-operator keystone_operator 'keystone_operator_db_sync_total{keystone="keystone-basic",namespace="openstack",result="succeeded"}' 'keystone_operator_db_sync_duration_seconds_count{keystone="keystone-basic",namespace="openstack"}'
keystone/credential-rotation|keystone-system keystone-operator keystone_operator 'keystone_operator_key_rotation_age_seconds{keystone="keystone-credential",namespace="openstack",key_type="credential"}'
keystone/db-expand-failure|keystone-system keystone-operator keystone_operator 'keystone_operator_db_sync_total{keystone="keystone-db-expand-failure",namespace="openstack",result="failed"}'
keystone/fernet-rotation|keystone-system keystone-operator keystone_operator 'keystone_operator_key_rotation_age_seconds{keystone="keystone-fernet",namespace="openstack",key_type="fernet"}'
neutron/basic-deployment|neutron-system neutron-operator neutron_operator 'neutron_operator_db_sync_total{neutron="neutron-basic",namespace="openstack",result="succeeded"}' 'neutron_operator_db_sync_duration_seconds_count{neutron="neutron-basic",namespace="openstack"}'
nova/basic-deployment|nova-system nova-operator nova_operator 'nova_operator_db_sync_total{nova="nova-basic",namespace="openstack",result="succeeded"}' 'nova_operator_db_sync_duration_seconds_count{nova="nova-basic",namespace="openstack"}'
nova/db-archive|nova-system nova-operator nova_operator 'nova_operator_db_archive_total{nova="nova-archive",namespace="openstack",result="succeeded"}'
ovn/central-backup-restore|ovn-system ovn-operator ovn_operator 'ovn_operator_backup_total{ovncentral="ovn-restore",namespace="openstack",result="succeeded"}'
ovn/central-basic-deployment|ovn-system ovn-operator ovn_operator
placement/basic-deployment|placement-system placement-operator placement_operator 'placement_operator_db_sync_total{placement="placement-basic",namespace="openstack",result="succeeded"}' 'placement_operator_db_sync_duration_seconds_count{placement="placement-basic",namespace="openstack"}'
CALLS
)"

setup() {
  make_kubectl_stub
  STUBS="$STUB_DIR/state"
  reset_stub
}

# --- Test 1: the happy path ---
@test "a two-pod release whose samples sit on one pod passes every check" {
  run_check keystone-system keystone-operator keystone_operator "$DB_SYNC_SAMPLE" \
    'keystone_operator_reconcile_errors_total{sub_reconciler="Database",condition_type="DatabaseReady"}'
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "it prints six OK: lines, four checks and two samples" "6" \
    "$(grep -c '^OK:' <<<"$output" || true)"
  assert_contains "it names both pods" "$output" "OK: release keystone-operator runs 2 pods: op-0 op-1"
  assert_contains "it names the sample" "$output" "OK: sample $DB_SYNC_SAMPLE"
  assert_not_contains "it prints no FAIL: line" "$output" "FAIL:"
}

@test "a release check without samples passes on checks 1 to 4" {
  run_check keystone-system keystone-operator keystone_operator
  assert_eq "the script exits 0" "0" "$status"
  assert_eq "it prints four OK: lines" "4" "$(grep -c '^OK:' <<<"$output" || true)"
}

# --- Test 2: check 1, the pod list ---
@test "a release without a pod fails check 1" {
  stub_on kubectl 'get pods *' --out ''
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the missing pod" "$output" \
    "FAIL: no pod of release keystone-operator in keystone-system"
}

@test "a pod list kubectl cannot read fails check 1 with kubectl's error" {
  # The first pod list fails; a later one would answer again.
  stub_on kubectl 'get pods *' --rc 1 --err "Error from server (Forbidden): pods is forbidden" --times 1
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the pods cannot be listed" "$output" \
    "FAIL: cannot list the pods of release keystone-operator"
  assert_contains "kubectl's own error reaches stderr" "$stderr" "Forbidden"
}

# --- Test 3: check 2, the metrics endpoints ---
@test "a pod whose metrics read fails fails check 2 and is named" {
  echo 1 >"$STUBS/raw/op-1.rc"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the pod" "$output" "FAIL: metrics endpoint of pod op-1 unreachable"
  assert_contains "kubectl's own error reaches stderr" "$stderr" "ServiceUnavailable"
}

@test "a pod that answers an empty body fails check 2 and is named" {
  : >"$STUBS/raw/op-0"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the pod" "$output" "FAIL: metrics endpoint of pod op-0 returned nothing"
}

@test "a re-read still failing at the deadline fails check 2" {
  # The first read lacks the sample, every later read of op-1 fails.
  set_metrics op-1 "$DURATION_LINE"
  echo 1 >"$STUBS/raw-later/op-1.rc"
  : >"$STUBS/raw-later/op-1"
  METRICS_WAIT=1
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the pod" "$output" "FAIL: metrics endpoint of pod op-1 unreachable"
}

@test "a deadline that passes during the first check still gets one re-read" {
  # SECONDS counts whole seconds, so a 1s deadline can pass while the first
  # check runs. An awk that sleeps 1.1s on the first check's first call
  # forces that: the failing re-read must still be made and judged.
  set_metrics op-1 "$DURATION_LINE"
  echo 1 >"$STUBS/raw-later/op-1.rc"
  : >"$STUBS/raw-later/op-1"
  stub_script awk <<STUB
if [ "\$1" = "-v" ] && [ ! -e "$STUBS/awk.slept" ]; then
  : >"$STUBS/awk.slept"
  /bin/sleep 1.1
fi
exec "$(command -v awk)" "\$@"
STUB
  METRICS_WAIT=1
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the pod" "$output" "FAIL: metrics endpoint of pod op-1 unreachable"
  assert_eq "op-1 is read twice" "2" "$(cat "$STUBS/raw/op-1.reads")"
}

@test "a re-read that fails once and then recovers passes" {
  # The first read lacks the sample, the second read of op-1 fails, and the
  # third carries the sample, as across an operator restart.
  set_metrics op-1 "$DURATION_LINE"
  mkdir -p "$STUBS/raw-2"
  echo 1 >"$STUBS/raw-2/op-1.rc"
  set_later_metrics op-1 "$DURATION_LINE"$'\n'"$DB_SYNC_LINE"
  METRICS_WAIT=10
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "the second read did fail" "$stderr" "ServiceUnavailable"
  assert_contains "the sample passes" "$output" "OK: sample $DB_SYNC_SAMPLE"
  assert_not_contains "it prints no FAIL: line" "$output" "FAIL:"
}

# --- Test 4: check 3, the reconcile duration count ---
@test "an absent reconcile duration count fails check 3" {
  set_metrics op-1 "$ERRORS_LINE"$'\n'"$DB_SYNC_LINE"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the family" "$output" \
    "FAIL: no keystone_operator_reconcile_duration_seconds_count line of release keystone-operator counts a reconcile after 0s"
}

@test "a reconcile duration count whose only value is 0 fails check 3" {
  set_metrics op-1 'keystone_operator_reconcile_duration_seconds_count{sub_reconciler="Database"} 0'$'\n'"$DB_SYNC_LINE"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it fails check 3" "$output" \
    "FAIL: no keystone_operator_reconcile_duration_seconds_count line"
  assert_contains "it prints the zero line" "$output" \
    'keystone_operator_reconcile_duration_seconds_count{sub_reconciler="Database"} 0'
}

@test "a reconcile duration count that appears on a later read passes" {
  set_metrics op-1 "$DB_SYNC_LINE"
  set_later_metrics op-1 "$DURATION_LINE"$'\n'"$DB_SYNC_LINE"
  METRICS_WAIT=10
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "check 3 passes" "$output" \
    "OK: keystone_operator_reconcile_duration_seconds_count counts a reconcile"
}

@test "a reconcile duration count without labels passes check 3" {
  set_metrics op-1 'keystone_operator_reconcile_duration_seconds_count 3'
  run_check keystone-system keystone-operator keystone_operator
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "check 3 passes" "$output" \
    "OK: keystone_operator_reconcile_duration_seconds_count counts a reconcile"
}

# --- Test 5: check 4, the UNKNOWN condition type ---
@test "an errors line labelled condition_type=\"UNKNOWN\" fails check 4 and is printed" {
  local line='keystone_operator_reconcile_errors_total{condition_type="UNKNOWN",sub_reconciler="Rotation"} 1'
  set_metrics op-1 "$DURATION_LINE"$'\n'"$ERRORS_LINE"$'\n'"$line"$'\n'"$DB_SYNC_LINE"
  run_check
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it fails check 4" "$output" \
    "FAIL: keystone_operator_reconcile_errors_total carries condition_type=\"UNKNOWN\""
  assert_contains "it prints the line" "$output" "$line"
  assert_not_contains "it leaves the mapped line out" "$output" "$ERRORS_LINE"
}

@test "condition_type=\"UNKNOWNX\" passes check 4" {
  set_metrics op-1 "$DURATION_LINE"$'\n''keystone_operator_reconcile_errors_total{condition_type="UNKNOWNX",sub_reconciler="Rotation"} 1'$'\n'"$DB_SYNC_LINE"
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "check 4 passes" "$output" \
    "OK: no keystone_operator_reconcile_errors_total line carries condition_type=\"UNKNOWN\""
}

# --- Test 6: check 5, samples that pass ---
@test "a sample passes with its labels in another order" {
  # client_golang prints labels in name order: key_type before keystone.
  run_sample "$AGE_SAMPLE" \
    'keystone_operator_key_rotation_age_seconds{key_type="fernet",keystone="keystone-fernet",namespace="openstack"} 12.5'
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "the sample passes" "$output" "OK: sample $AGE_SAMPLE"
}

@test "a counter sample in exponent form passes" {
  run_sample "$DB_SYNC_SAMPLE" \
    'keystone_operator_db_sync_total{keystone="keystone-basic",namespace="openstack",result="succeeded"} 1.2e+06'
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "the sample passes" "$output" "OK: sample $DB_SYNC_SAMPLE"
}

@test "a gauge sample at 0 passes" {
  # A rotation that just ran leaves an age of 0.
  run_sample "$AGE_SAMPLE" \
    'keystone_operator_key_rotation_age_seconds{key_type="fernet",keystone="keystone-fernet",namespace="openstack"} 0'
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "the sample passes" "$output" "OK: sample $AGE_SAMPLE"
}

@test "a sample that appears on a later read passes" {
  # The operator records a finished Job on its next pass.
  set_metrics op-1 "$DURATION_LINE"
  set_later_metrics op-1 "$DURATION_LINE"$'\n'"$DB_SYNC_LINE"
  METRICS_WAIT=10
  run_check
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "the sample passes" "$output" "OK: sample $DB_SYNC_SAMPLE"
}

@test "a label value holding a comma matches only as a whole" {
  local sample='keystone_operator_db_sync_total{keystone="a,b",result="succeeded"}'
  run_sample "$sample" 'keystone_operator_db_sync_total{keystone="a,b",result="succeeded"} 1'
  assert_eq "the value a,b passes" "0" "$status"
  assert_contains "the sample passes" "$output" "OK: sample $sample"

  # Split at its comma, the pair would find keystone="a and b" in two values.
  run_sample "$sample" 'keystone_operator_db_sync_total{keystone="a,x",result="succeeded",y="z,b"} 1'
  assert_eq "the values a,x and z,b fail" "1" "$status"
  assert_contains "it names the sample" "$output" "FAIL: sample $sample"
}

@test "a label value holding a closing brace passes" {
  local sample='keystone_operator_db_sync_total{keystone="a}b",result="succeeded"}'
  run_sample "$sample" 'keystone_operator_db_sync_total{keystone="a}b",result="succeeded"} 1'
  assert_eq "the script exits 0" "0" "$status"
  assert_contains "the sample passes" "$output" "OK: sample $sample"
}

# --- Test 7: check 5, samples that fail ---
@test "a _total sample at 0 fails" {
  run_sample "$DB_SYNC_SAMPLE" \
    'keystone_operator_db_sync_total{keystone="keystone-basic",namespace="openstack",result="succeeded"} 0'
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the sample" "$output" \
    "FAIL: sample $DB_SYNC_SAMPLE has no line with a value of 1 or more after 0s"
  assert_contains "it prints the family's line" "$output" "result=\"succeeded\"} 0"
}

@test "a sample without a line fails and the other samples still report" {
  run_check keystone-system keystone-operator keystone_operator "$AGE_SAMPLE" "$DB_SYNC_SAMPLE"
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the sample" "$output" \
    "FAIL: sample $AGE_SAMPLE has no line with a numeric value after 0s"
  assert_contains "it says the family is absent" "$output" \
    "no line of family keystone_operator_key_rotation_age_seconds"
  assert_contains "the present sample still passes" "$output" "OK: sample $DB_SYNC_SAMPLE"
}

@test "keystone=\"keystone-basic-2026-2\" does not satisfy keystone=\"keystone-basic\"" {
  run_sample "$DB_SYNC_SAMPLE" \
    'keystone_operator_db_sync_total{keystone="keystone-basic-2026-2",namespace="openstack",result="succeeded"} 1'
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the sample" "$output" "FAIL: sample $DB_SYNC_SAMPLE"
}

@test "subnamespace=\"openstack\" does not satisfy namespace=\"openstack\"" {
  run_sample "$DB_SYNC_SAMPLE" \
    'keystone_operator_db_sync_total{keystone="keystone-basic",result="succeeded",subnamespace="openstack"} 1'
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the sample" "$output" "FAIL: sample $DB_SYNC_SAMPLE"
}

@test "a line of a longer family name does not satisfy the sample" {
  run_sample 'keystone_operator_db_sync{keystone="keystone-basic"}' "$DB_SYNC_LINE"
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it says the family is absent" "$output" \
    "no line of family keystone_operator_db_sync"
}

@test "a missing sample names the pods that restarted, and only those" {
  # op-1 runs two containers that restarted once and twice.
  stub_on kubectl 'get pods *' --out "op-0 0"$'\n'"op-1 1 2"
  run_check keystone-system keystone-operator keystone_operator "$AGE_SAMPLE"
  assert_eq "the script exits 1" "1" "$status"
  assert_contains "it names the restarted pod with its restarts summed" "$output" \
    "FAIL: pod op-1 restarted 3 times; in-memory counters recorded before the restart are gone"
  assert_not_contains "it leaves the other pod out" "$output" "pod op-0 restarted"

  # op-1 restarted after the first pod list, during the wait.
  reset_stub
  stub_on kubectl 'get pods *' --out "op-0 0"$'\n'"op-1 1"
  stub_on kubectl 'get pods *' --out "op-0 0"$'\n'"op-1 0" --times 1
  run_check keystone-system keystone-operator keystone_operator "$AGE_SAMPLE"
  assert_eq "a restart during the wait exits 1" "1" "$status"
  assert_contains "it names the pod that restarted during the wait" "$output" \
    "FAIL: pod op-1 restarted 1 times; in-memory counters recorded before the restart are gone"

  # The second pod list fails, so the restarts of the first one stand.
  reset_stub
  stub_on kubectl 'get pods *' --rc 1 --err "Error from server (Forbidden): pods is forbidden"
  stub_on kubectl 'get pods *' --out "op-0 0"$'\n'"op-1 2" --times 1
  run_check keystone-system keystone-operator keystone_operator "$AGE_SAMPLE"
  assert_eq "a failed second pod list exits 1" "1" "$status"
  assert_contains "it names the restart of the first pod list" "$output" \
    "FAIL: pod op-1 restarted 2 times; in-memory counters recorded before the restart are gone"
  assert_contains "kubectl's own error reaches stderr" "$stderr" "Forbidden"

  reset_stub
  stub_on kubectl 'get pods *' --out "op-0 0"$'\n'"op-1 2"
  run_check
  assert_eq "a present sample exits 0 despite the restart" "0" "$status"
  assert_not_contains "it names no restart" "$output" "restarted"
}

# --- Test 8: the arguments ---
@test "fewer than three arguments, an empty one or a malformed sample exits 2" {
  local usage="usage: assert-operator-metric-families.sh <operator-namespace> <release> <prefix> [<sample>...]"
  run_check keystone-system keystone-operator
  assert_eq "two arguments exit 2" "2" "$status"
  assert_contains "the usage line goes to stderr" "$stderr" "$usage"
  assert_eq "nothing goes to stdout" "" "$output"

  run_check keystone-system "" keystone_operator
  assert_eq "an empty argument exits 2" "2" "$status"
  assert_contains "the usage line goes to stderr" "$stderr" "$usage"

  run_check keystone-system keystone-operator keystone_operator "$DB_SYNC_SAMPLE" ""
  assert_eq "an empty sample exits 2" "2" "$status"

  run_check keystone-system keystone-operator keystone_operator 'db_sync_total{keystone}'
  assert_eq "a pair without a value exits 2" "2" "$status"
  assert_contains "the usage line goes to stderr" "$stderr" "$usage"
  assert_eq "nothing goes to stdout" "" "$output"

  run_check keystone-system keystone-operator keystone_operator 'db_sync_total'
  assert_eq "a sample without labels exits 2" "2" "$status"
}

@test "the script parses" {
  run bash -n "$SCRIPT"
  assert_eq "bash -n passes" "0" "$status"
}

# --- Test 9: the metrics port ---
@test "the metrics port the script reads matches the ten charts" {
  local op port
  port="$(sed -n 's/^metrics_port=//p' "$SCRIPT")"
  assert_not_empty "the script names its metrics port" "$port"
  for op in $OPERATORS; do
    soft assert_eq "the $op chart serves metrics on the script's port" "$port" \
      "$(yq '.metrics.port' "$PROJECT_ROOT/operators/$op/helm/$op-operator/values.yaml")"
  done
}

# --- Test 10: the path filters ---
@test "an edit to the script runs every leg" {
  local op block script=assert-operator-metric-families.sh
  # These seven list the whole directory.
  for op in barbican cinder glance horizon keystone nova placement; do
    block="$(filter_block "tests_e2e_$op")"
    soft assert_contains "tests_e2e_$op lists the shared directory" "$block" "'tests/e2e/lib/**'"
  done
  # ovn, neutron and c5c3 name the script.
  for op in ovn neutron c5c3; do
    block="$(filter_block "tests_e2e_$op")"
    soft assert_contains "tests_e2e_$op names $script" "$block" "'tests/e2e/lib/$script'"
  done
}

# --- Test 11: the callers ---
@test "the 17 suites call the script with the tabled arguments in a 120s step, and no other suite calls it" {
  local suite args file expected found calls
  while IFS='|' read -r suite args; do
    file="$PROJECT_ROOT/tests/e2e/$suite/chainsaw-test.yaml"
    calls="$(metric_calls "$file")"
    soft assert_eq "the $suite suite calls the script once, with its tabled arguments" \
      "../../lib/assert-operator-metric-families.sh $args" "${calls#*|}"
    # The script re-reads for up to 60 s; chainsaw's exec default is 30s.
    soft assert_eq "the $suite suite gives that step a 120s timeout" "120s" "${calls%%|*}"
  done <<<"$METRIC_CALLERS"

  assert_eq "the table names 17 suites" "17" "$(grep -c . <<<"$METRIC_CALLERS")"
  expected="$(cut -d'|' -f1 <<<"$METRIC_CALLERS" | LC_ALL=C sort)"
  # Any line that is not a comment and names the script counts as a call.
  found="$(grep -rlE --include=chainsaw-test.yaml \
    '^[[:space:]]*[^#[:space:]].*assert-operator-metric-families\.sh' "$PROJECT_ROOT/tests" |
    sed -e "s|^$PROJECT_ROOT/tests/e2e/||" -e 's|/chainsaw-test.yaml$||' | LC_ALL=C sort)"
  assert_eq "the suites that call the script are the tabled ones" "$expected" "$found"
}
