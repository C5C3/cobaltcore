#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-vpa-recommendations.sh with a stubbed kubectl:
#   - snapshot applies one recommendation-only VPA per Deployment,
#     StatefulSet and DaemonSet, named sizing-<kind>-<name>, with the
#     managed-by label and the matching targetRef;
#   - snapshot writes a file only for a VPA with a recommendation, and a later
#     pass whose VPA has no recommendation, or whose workload is gone, leaves
#     the earlier file unchanged;
#   - an empty namespace prints `no workloads in <ns>`, applies nothing and
#     exits 0;
#   - a cluster without the VPA kind, a failing workload list and a failing
#     apply exit 2 with their message;
#   - watch writes watch.pid, survives a failing pass with a WARNING and runs
#     the next one;
#   - report writes the TSV with its comment line, converts quantities to
#     millicores and MiB, reads --processes/--threads/--n-threads, writes "-"
#     for what is unset, and prints the Markdown table;
#   - report exits 1 without a snapshot and 2 on a malformed one, and a usage
#     error exits 2 with the usage text;
#   - prepare removes the scale subresource from every version of the MariaDB
#     CRD with one replace, also when only a later version serves it, keeps
#     the other subresources, replaces nothing when no version serves it, and
#     exits 2 on a failing or empty read and on a failing replace;
#   - e2e-controlplane, e2e-controlplane-sso and tempest pass WITH_VPA from the
#     ci:measure-sizing label and start, collect and upload the measurement
#     around their suites.
#
# Usage: bash tests/unit/hack/ci_vpa_recommendations_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
VPA_SH="$PROJECT_ROOT/hack/ci-vpa-recommendations.sh"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The script does its arithmetic in jq. The stub PATH below is tightened to
# the stub dir plus /usr/bin and /bin, so a jq installed elsewhere (Homebrew,
# ~/.local) is linked into the stub dir.
JQ_BIN="$(command -v jq 2>/dev/null || true)"

VPA_KIND="verticalpodautoscalers.autoscaling.k8s.io"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stub <dir>
# kubectl logs every argument in brackets, one invocation per line, to
# $KUBECTL_LOG. It answers:
#   api-resources         $STUB_API_RESOURCES; fails once when
#                         $STUB_API_FAIL_ONCE names a file that does not exist
#                         yet (and creates it)
#   get deployments,...   $STUB_WORKLOADS_JSON, or fails when $STUB_LIST_FAIL
#   apply                 appends stdin to $STUB_APPLY_FILE, or fails when
#                         $STUB_APPLY_FAIL
#   get <vpa kind>        $STUB_VPAS_JSON
#   get deployments       $STUB_RECOMMENDER_IMAGE (the kube-system lookup)
#   get crd               $STUB_CRD_JSON, or fails with NotFound when
#                         $STUB_CRD_FAIL
#   replace -f            appends stdin to $STUB_REPLACE_FILE, or fails with
#                         Conflict when $STUB_REPLACE_FAIL
# sleep counts its calls in $SLEEP_COUNT and, on the second call, kills the
# watch loop that called it.
make_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
printf '[%s]' "$@" >>"$KUBECTL_LOG"
echo >>"$KUBECTL_LOG"
case "$1 $2" in
  "api-resources --api-group=autoscaling.k8s.io")
    if [ -n "${STUB_API_FAIL_ONCE:-}" ] && [ ! -e "$STUB_API_FAIL_ONCE" ]; then
      : >"$STUB_API_FAIL_ONCE"
      echo "error: stub discovery unavailable" >&2
      exit 1
    fi
    printf '%s\n' "$STUB_API_RESOURCES"
    exit 0
    ;;
  "get deployments,statefulsets,daemonsets")
    if [ -n "${STUB_LIST_FAIL:-}" ]; then
      echo "error: stub list forbidden" >&2
      exit 1
    fi
    printf '%s\n' "$STUB_WORKLOADS_JSON"
    exit 0
    ;;
  "apply -n")
    if [ -n "${STUB_APPLY_FAIL:-}" ]; then
      cat >/dev/null
      echo "error: stub apply rejected" >&2
      exit 1
    fi
    cat >>"$STUB_APPLY_FILE"
    exit 0
    ;;
  "get verticalpodautoscalers.autoscaling.k8s.io")
    printf '%s\n' "$STUB_VPAS_JSON"
    exit 0
    ;;
  "get deployments")
    [ -n "${STUB_RECOMMENDER_IMAGE:-}" ] || exit 1
    printf '%s' "$STUB_RECOMMENDER_IMAGE"
    exit 0
    ;;
  "get crd")
    if [ -n "${STUB_CRD_FAIL:-}" ]; then
      echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "mariadbs.k8s.mariadb.com" not found' >&2
      exit 1
    fi
    printf '%s\n' "${STUB_CRD_JSON:-}"
    exit 0
    ;;
  "replace -f")
    if [ -n "${STUB_REPLACE_FAIL:-}" ]; then
      cat >/dev/null
      echo "Error from server (Conflict): the object has been modified" >&2
      exit 1
    fi
    cat >>"$STUB_REPLACE_FILE"
    exit 0
    ;;
esac
echo "[kubectl-stub] unexpected invocation: $*" >&2
exit 64
STUB
  cat >"$dir/sleep" <<'STUB'
#!/bin/bash
n=$(( $(cat "$SLEEP_COUNT" 2>/dev/null || echo 0) + 1 ))
# A count that cannot be written would loop forever; stop the watch instead.
if ! echo "$n" >"$SLEEP_COUNT" || [ "$n" -ge 2 ]; then
  kill -TERM "$PPID"
  exit 1
fi
exit 0
STUB
  chmod +x "$dir/kubectl" "$dir/sleep"
  ln -sf "$JQ_BIN" "$dir/jq"
}

# reset_stub <tmp>
# Points the stub at <tmp> and restores every answer to its default: the VPA
# kind is served, the namespace has no workload and no VPA, and the MariaDB
# CRD answer is empty. A test that sets a STUB_CRD_* or STUB_REPLACE_FAIL
# variable exports it, since reset_stub unsets them.
reset_stub() {
  local tmp="$1"
  export KUBECTL_LOG="$tmp/kubectl.log"
  export STUB_APPLY_FILE="$tmp/applied.json"
  export STUB_REPLACE_FILE="$tmp/replaced.json"
  export SLEEP_COUNT="$tmp/sleep.count"
  export STUB_API_RESOURCES="verticalpodautoscalercheckpoints.autoscaling.k8s.io
$VPA_KIND"
  export STUB_WORKLOADS_JSON='{"apiVersion":"v1","kind":"List","items":[]}'
  export STUB_VPAS_JSON='{"apiVersion":"v1","kind":"List","items":[]}'
  export STUB_RECOMMENDER_IMAGE="registry.k8s.io/autoscaling/vpa-recommender:1.8.0"
  unset STUB_API_FAIL_ONCE STUB_LIST_FAIL STUB_APPLY_FAIL
  unset STUB_CRD_JSON STUB_CRD_FAIL STUB_REPLACE_FAIL
  unset GITHUB_RUN_ID GITHUB_RUN_ATTEMPT GITHUB_SHA GITHUB_JOB MEASURE_LEG
  : >"$KUBECTL_LOG"
  rm -f "$STUB_APPLY_FILE" "$STUB_REPLACE_FILE" "$SLEEP_COUNT"
}

# new_tmp prints a fresh temp dir with the stubs; the caller runs reset_stub
# on it, since exports made inside $(...) do not reach the caller.
new_tmp() {
  local tmp
  tmp="$(mktemp -d)"
  make_stub "$tmp/bin"
  echo "$tmp"
}

# run_vpa <tmp> <arg>...
# Runs the script with the stub PATH. stdout and stderr go to $tmp/out and
# $tmp/err; the exit code is returned.
run_vpa() {
  local tmp="$1"
  shift
  (
    PATH="$tmp/bin:/usr/bin:/bin"
    export PATH
    bash "$VPA_SH" "$@"
  ) >"$tmp/out" 2>"$tmp/err"
}

# workload <kind> <name> <replicas|null> <owner kind> <component> <container json>
# One workload object in the shape `kubectl get -o json` lists it.
workload() {
  jq -cn --arg kind "$1" --arg name "$2" --argjson replicas "$3" \
    --arg owner "$4" --arg component "$5" --argjson container "$6" '
    {apiVersion: "apps/v1", kind: $kind,
     metadata: {name: $name, namespace: "openstack",
                ownerReferences: [
                  {kind: "ControlPlane", name: "cp", controller: false},
                  {kind: $owner, name: "\($owner | ascii_downcase)-cr", controller: true}]},
     spec: ({template: {metadata: {labels: {
               "app.kubernetes.io/name": ($owner | ascii_downcase),
               "app.kubernetes.io/component": $component}},
             spec: {containers: [$container]}}}
            + (if $replicas == null then {} else {replicas: $replicas} end))}'
}

# items <json>...
# Wraps objects into a v1 List.
items() {
  printf '%s\n' "$@" | jq -cs '{apiVersion: "v1", kind: "List", items: .}'
}

# vpa <kind> <name> <containerRecommendations json>
# One managed VPA with the given status.recommendation.containerRecommendations.
vpa() {
  jq -cn --arg kind "$1" --arg name "$2" --argjson recs "$3" '
    {apiVersion: "autoscaling.k8s.io/v1", kind: "VerticalPodAutoscaler",
     metadata: {name: "sizing-\($kind | ascii_downcase)-\($name)", namespace: "openstack"},
     spec: {targetRef: {apiVersion: "apps/v1", kind: $kind, name: $name}},
     status: {recommendation: {containerRecommendations: $recs}}}'
}

# rec <container> <cpu> <memory> <upper cpu> <upper memory>
rec() {
  jq -cn --arg c "$1" --arg cpu "$2" --arg mem "$3" --arg ucpu "$4" --arg umem "$5" '
    {containerName: $c, target: {cpu: $cpu, memory: $mem},
     upperBound: {cpu: $ucpu, memory: $umem}}'
}

# The three fixture workloads: a uWSGI API Deployment, a MariaDB StatefulSet
# without a memory request, and a northd-like DaemonSet with --n-threads=.
API_CONTAINER='{"name":"keystone-api","command":["uwsgi"],"args":["--processes","4","--threads","1"],"resources":{"requests":{"cpu":"100m","memory":"800Mi"},"limits":{"memory":"800Mi"}}}'
DB_CONTAINER='{"name":"mariadb","resources":{"requests":{"cpu":"500m"}}}'
DS_CONTAINER='{"name":"northd","command":["ovn-northd","--n-threads=4"],"resources":{"requests":{"cpu":"0.1","memory":"1Gi"},"limits":{"memory":"1Gi"}}}'

three_workloads() {
  items \
    "$(workload Deployment keystone-api 2 Keystone api "$API_CONTAINER")" \
    "$(workload StatefulSet openstack-db 1 MariaDB database "$DB_CONTAINER")" \
    "$(workload DaemonSet ovn-northd null OVNCentral northd "$DS_CONTAINER")"
}

# ---------------------------------------------------------------------------
# Test 1: one VPA per workload kind
# ---------------------------------------------------------------------------
test_snapshot_applies_one_vpa_per_workload() {
  echo "Test: snapshot applies sizing-<kind>-<name> VPAs in Off mode for three workload kinds"
  local tmp rc
  tmp="$(new_tmp)"
  reset_stub "$tmp"
  STUB_WORKLOADS_JSON="$(three_workloads)"

  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  rc=$?
  assert_eq "exit code is 0" "0" "$rc"

  local names
  names="$(jq -r '.items[].metadata.name' "$STUB_APPLY_FILE" | sort | tr '\n' ' ')"
  assert_eq "one VPA per workload, named by kind and name" \
    "sizing-daemonset-ovn-northd sizing-deployment-keystone-api sizing-statefulset-openstack-db " "$names"
  assert_eq "every VPA is recommendation-only" "Off Off Off " \
    "$(jq -r '.items[].spec.updatePolicy.updateMode' "$STUB_APPLY_FILE" | tr '\n' ' ')"
  assert_eq "every VPA carries the managed-by label" \
    "ci-vpa-recommendations ci-vpa-recommendations ci-vpa-recommendations " \
    "$(jq -r '.items[].metadata.labels["app.kubernetes.io/managed-by"]' "$STUB_APPLY_FILE" | tr '\n' ' ')"
  assert_eq "the StatefulSet VPA targets its StatefulSet" \
    '{"apiVersion":"apps/v1","kind":"StatefulSet","name":"openstack-db"}' \
    "$(jq -c '.items[] | select(.metadata.name == "sizing-statefulset-openstack-db") | .spec.targetRef' "$STUB_APPLY_FILE")"
  assert_eq "the VPAs are applied as one List per namespace" "1" \
    "$(grep -c '^\[apply\]\[-n\]\[openstack\]\[-f\]\[-\]$' "$KUBECTL_LOG")"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 2: files only for recommendations, and earlier files survive
# ---------------------------------------------------------------------------
test_snapshot_keeps_earlier_files() {
  echo "Test: snapshot writes only recommended VPAs and never erases an earlier snapshot"
  local tmp rc file
  tmp="$(new_tmp)"
  reset_stub "$tmp"
  STUB_WORKLOADS_JSON="$(three_workloads)"
  STUB_VPAS_JSON="$(items \
    "$(vpa Deployment keystone-api "[$(rec keystone-api 250m 314572800 1 300Mi)]")" \
    "$(vpa StatefulSet openstack-db '[]')")"
  file="$tmp/sizing/snapshots/openstack/sizing-deployment-keystone-api.json"

  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  rc=$?
  assert_eq "first pass exits 0" "0" "$rc"
  assert_eq "only the recommended VPA has a snapshot" "sizing-deployment-keystone-api.json" \
    "$(ls "$tmp/sizing/snapshots/openstack" | tr '\n' ' ' | sed 's/ $//')"
  assert_eq "the snapshot records the workload's owner" "Keystone/keystone-cr" \
    "$(jq -r '.workload.owner.kind + "/" + .workload.owner.name' "$file")"
  assert_eq "the snapshot records the container args" "--processes 4 --threads 1" \
    "$(jq -r '.containers[0].args | join(" ")' "$file")"
  cp "$file" "$tmp/first.json"

  # The recommendation is gone (a VPA whose target was deleted gets none).
  STUB_VPAS_JSON="$(items "$(vpa Deployment keystone-api '[]')")"
  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  rc=$?
  assert_eq "a pass without recommendation exits 0" "0" "$rc"
  if cmp -s "$file" "$tmp/first.json"; then
    echo "  PASS: a VPA without recommendation leaves the earlier snapshot unchanged"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: a VPA without recommendation rewrote the earlier snapshot"
    FAIL=$((FAIL + 1))
  fi

  # The workload is gone while its VPA still shows the old recommendation.
  STUB_WORKLOADS_JSON="$(items "$(workload StatefulSet openstack-db 1 MariaDB database "$DB_CONTAINER")")"
  STUB_VPAS_JSON="$(items "$(vpa Deployment keystone-api "[$(rec keystone-api 999m 1Gi 2 2Gi)]")")"
  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  if cmp -s "$file" "$tmp/first.json"; then
    echo "  PASS: a VPA whose workload is gone leaves the earlier snapshot unchanged"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: a VPA whose workload is gone rewrote the earlier snapshot"
    FAIL=$((FAIL + 1))
  fi
  assert_eq "no temporary file is left behind" "" \
    "$(find "$tmp/sizing" -name '*.tmp')"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 3: an empty namespace
# ---------------------------------------------------------------------------
test_snapshot_empty_namespace() {
  echo "Test: snapshot of a namespace without workloads prints a line and applies nothing"
  local tmp rc
  tmp="$(new_tmp)"
  reset_stub "$tmp"

  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  rc=$?
  assert_eq "exit code is 0" "0" "$rc"
  assert_eq "stdout names the empty namespace" "no workloads in openstack" "$(cat "$tmp/out")"
  assert_eq "nothing is applied" "0" "$(grep -c '^\[apply\]' "$KUBECTL_LOG")"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 4: cluster errors exit 2
# ---------------------------------------------------------------------------
test_snapshot_errors() {
  echo "Test: snapshot exits 2 without the VPA kind, on a failing list and on a failing apply"
  local tmp rc
  tmp="$(new_tmp)"
  reset_stub "$tmp"

  STUB_API_RESOURCES="verticalpodautoscalercheckpoints.autoscaling.k8s.io"
  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  rc=$?
  assert_eq "a cluster without the VPA kind exits 2" "2" "$rc"
  assert_contains "the message names the missing kind and the flag" "$(cat "$tmp/err")" \
    "ci-vpa-recommendations: the cluster does not serve autoscaling.k8s.io VerticalPodAutoscaler; deploy with WITH_VPA=true"

  reset_stub "$tmp"
  export STUB_LIST_FAIL=1
  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  rc=$?
  assert_eq "a failing workload list exits 2" "2" "$rc"
  assert_contains "the message carries kubectl's stderr" "$(cat "$tmp/err")" \
    "ci-vpa-recommendations: listing workloads in openstack failed: error: stub list forbidden"

  reset_stub "$tmp"
  STUB_WORKLOADS_JSON="$(three_workloads)"
  export STUB_APPLY_FAIL=1
  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  rc=$?
  assert_eq "a failing apply exits 2" "2" "$rc"
  assert_contains "the message carries kubectl's stderr" "$(cat "$tmp/err")" \
    "ci-vpa-recommendations: applying the VPAs in openstack failed: error: stub apply rejected"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 5: watch survives a failing pass
# ---------------------------------------------------------------------------
test_watch_survives_a_failing_pass() {
  echo "Test: watch writes watch.pid, warns on a failing pass and runs the next"
  local tmp pid rc
  tmp="$(new_tmp)"
  reset_stub "$tmp"
  STUB_WORKLOADS_JSON="$(three_workloads)"
  STUB_VPAS_JSON="$(items "$(vpa Deployment keystone-api "[$(rec keystone-api 250m 300Mi 1 1Gi)]")")"
  export STUB_API_FAIL_ONCE="$tmp/api-failed"

  (
    PATH="$tmp/bin:/usr/bin:/bin"
    export PATH
    exec bash "$VPA_SH" watch "$tmp/sizing" openstack
  ) >"$tmp/out" 2>"$tmp/err" &
  pid=$!
  # The stub's kill ends the watch; keep bash 3.2's "Terminated" notice quiet.
  wait "$pid" 2>/dev/null
  rc=$?

  assert_eq "the watch ran until the second sleep killed it" "2" "$(cat "$SLEEP_COUNT")"
  assert_nonzero_exit "the watch ends only when killed" "$rc"
  assert_eq "watch.pid holds the watch's PID" "$pid" "$(cat "$tmp/sizing/watch.pid" 2>/dev/null)"
  assert_contains "the failing pass prints a prefixed warning" "$(cat "$tmp/err")" \
    "WARNING: ci-vpa-recommendations: listing the autoscaling.k8s.io API resources failed: error: stub discovery unavailable"
  assert_eq "the second pass ran" "2" "$(grep -c '^\[api-resources\]' "$KUBECTL_LOG")"
  assert_eq "the second pass wrote the snapshot" "sizing-deployment-keystone-api.json" \
    "$(ls "$tmp/sizing/snapshots/openstack" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 6: report writes the TSV and the Markdown table
# ---------------------------------------------------------------------------
test_report_rows() {
  echo "Test: report converts the recommendations into TSV and Markdown rows"
  local tmp rc tsv
  tmp="$(new_tmp)"
  reset_stub "$tmp"
  STUB_WORKLOADS_JSON="$(three_workloads)"
  STUB_VPAS_JSON="$(items \
    "$(vpa Deployment keystone-api "[$(rec keystone-api 250m 314572800 1 300Mi)]")" \
    "$(vpa StatefulSet openstack-db "[$(rec mariadb 12m 700Mi 40m 1100Mi)]")" \
    "$(vpa DaemonSet ovn-northd "[$(rec northd 3m 20971520 5m 30Mi)]")")"
  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  # A leftover of a killed pass is not a snapshot.
  echo '{"truncated":' >"$tmp/sizing/snapshots/openstack/sizing-x.json.tmp"

  export GITHUB_RUN_ID=4242 GITHUB_RUN_ATTEMPT=2 GITHUB_SHA=abc123 GITHUB_JOB=tempest MEASURE_LEG=nova-2025.2
  run_vpa "$tmp" report "$tmp/sizing"
  rc=$?
  assert_eq "exit code is 0" "0" "$rc"
  tsv="$tmp/sizing/recommendations.tsv"

  assert_eq "the comment line names the run, the job, the leg and the recommender" \
    "# run=4242 attempt=2 sha=abc123 job=tempest leg=nova-2025.2 recommender=registry.k8s.io/autoscaling/vpa-recommender:1.8.0" \
    "$(sed -n 1p "$tsv")"
  assert_eq "the header row lists the columns" \
    "namespace kind workload replicas owner_kind owner_name app component container processes threads cpu_target_m memory_target_mi cpu_upper_m memory_upper_mi cpu_request_m memory_request_mi memory_limit_mi snapshot" \
    "$(sed -n 2p "$tsv" | tr '\t' ' ')"
  assert_eq "three rows, sorted by namespace, workload and container" \
    "keystone-api openstack-db ovn-northd " "$(awk -F'\t' 'NR > 2 {print $3}' "$tsv" | tr '\n' ' ')"

  # Columns 1-18; the 19th is the pass time.
  assert_eq "the API row: 250m, 314572800, 1 and 300Mi convert; --processes 4 --threads 1" \
    "openstack Deployment keystone-api 2 Keystone keystone-cr keystone api keystone-api 4 1 250 300 1000 300 100 800 800" \
    "$(awk -F'\t' '$3 == "keystone-api"' "$tsv" | cut -f1-18 | tr '\t' ' ')"
  assert_eq "the database row: an unset memory request and limit read -" \
    "openstack StatefulSet openstack-db 1 MariaDB mariadb-cr mariadb database mariadb - - 12 700 40 1100 500 - -" \
    "$(awk -F'\t' '$3 == "openstack-db"' "$tsv" | cut -f1-18 | tr '\t' ' ')"
  assert_eq "the DaemonSet row: replicas -, threads from --n-threads=4, 0.1 CPU is 100m" \
    "openstack DaemonSet ovn-northd - OVNCentral ovncentral-cr ovncentral northd northd - 4 3 20 5 30 100 1024 1024" \
    "$(awk -F'\t' '$3 == "ovn-northd"' "$tsv" | cut -f1-18 | tr '\t' ' ')"
  assert_contains "every row carries its pass time" "$(awk -F'\t' 'NR == 3 {print $19}' "$tsv")" "T"

  assert_contains "the Markdown file opens with the run" "$(sed -n 1p "$tmp/sizing/recommendations.md")" \
    'run=4242 attempt=2 sha=abc123 job=tempest leg=nova-2025.2'
  assert_contains "the Markdown table has the API row" "$(cat "$tmp/sizing/recommendations.md")" \
    "| openstack | Deployment | keystone-api | 2 | Keystone |"
  assert_eq "stdout is the Markdown file" "$(cat "$tmp/sizing/recommendations.md")" "$(cat "$tmp/out")"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 7: report without cluster access and without CI variables
# ---------------------------------------------------------------------------
test_report_offline_defaults() {
  echo "Test: report fills unset CI variables and an unreachable recommender with -"
  local tmp
  tmp="$(new_tmp)"
  reset_stub "$tmp"
  STUB_WORKLOADS_JSON="$(three_workloads)"
  STUB_VPAS_JSON="$(items "$(vpa Deployment keystone-api "[$(rec keystone-api 250m 300Mi 1 1Gi)]")")"
  run_vpa "$tmp" snapshot "$tmp/sizing" openstack
  unset STUB_RECOMMENDER_IMAGE

  run_vpa "$tmp" report "$tmp/sizing"
  assert_eq "the comment line reads - for everything unknown" \
    "# run=- attempt=- sha=- job=- leg=- recommender=-" \
    "$(sed -n 1p "$tmp/sizing/recommendations.tsv")"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 8: report errors
# ---------------------------------------------------------------------------
test_report_errors() {
  echo "Test: report exits 1 without a snapshot and 2 on a malformed one"
  local tmp rc
  tmp="$(new_tmp)"
  reset_stub "$tmp"

  mkdir -p "$tmp/sizing/snapshots"
  run_vpa "$tmp" report "$tmp/sizing"
  rc=$?
  assert_eq "an empty snapshot directory exits 1" "1" "$rc"
  assert_contains "the message names the directory" "$(cat "$tmp/err")" \
    "ci-vpa-recommendations: no recommendation was recorded under $tmp/sizing/snapshots"

  run_vpa "$tmp" report "$tmp/nowhere"
  rc=$?
  assert_eq "a missing output directory exits 1" "1" "$rc"

  mkdir -p "$tmp/sizing/snapshots/openstack"
  echo '{"namespace": "openstack",' >"$tmp/sizing/snapshots/openstack/sizing-deployment-bad.json"
  run_vpa "$tmp" report "$tmp/sizing"
  rc=$?
  assert_eq "a malformed snapshot exits 2" "2" "$rc"
  assert_contains "the message names the file" "$(cat "$tmp/err")" \
    "ci-vpa-recommendations: cannot parse snapshot $tmp/sizing/snapshots/openstack/sizing-deployment-bad.json:"

  jq -cn '{namespace: "openstack", time: "t", workload: {kind: "Deployment", name: "bad"},
           containers: [], recommendation: {containerRecommendations: [
             {containerName: "c", target: {cpu: "lots", memory: "1Mi"}}]}}' \
    >"$tmp/sizing/snapshots/openstack/sizing-deployment-bad.json"
  run_vpa "$tmp" report "$tmp/sizing"
  rc=$?
  assert_eq "an unparsable quantity exits 2" "2" "$rc"
  assert_contains "the message names the quantity" "$(cat "$tmp/err")" 'quantity "lots"'
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 9: usage errors
# ---------------------------------------------------------------------------
test_usage_errors() {
  echo "Test: usage errors exit 2 with the usage text"
  local tmp rc args
  tmp="$(new_tmp)"
  reset_stub "$tmp"

  for args in "measure $tmp/sizing openstack" "snapshot" "snapshot $tmp/sizing" "watch $tmp/sizing" "report" "prepare extra" ""; do
    # shellcheck disable=SC2086 # split the argument list on purpose
    run_vpa "$tmp" $args
    rc=$?
    assert_eq "'${args:-<none>}' exits 2" "2" "$rc"
    assert_contains "'${args:-<none>}' prints the usage" "$(cat "$tmp/err")" \
      "usage: hack/ci-vpa-recommendations.sh snapshot <out-dir> <namespace>..."
  done
  assert_eq "no usage error reaches kubectl" "" "$(cat "$KUBECTL_LOG")"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test 10: the three measurement jobs wire the script
# ---------------------------------------------------------------------------

# step_index <step names, one per line> <name> — the 1-based position of the
# first step with that name, or nothing.
step_index() {
  printf '%s\n' "$1" | awk -v n="$2" '$0 == n {print NR; exit}'
}

test_wiring() {
  echo "Test: e2e-controlplane, e2e-controlplane-sso and tempest wire the measurement"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (28 checks skipped)"
    SKIP=$((SKIP + 28))
    return
  fi

  local job last artifact steps idx_setup idx_start idx_last idx_collect idx_upload idx_dump
  local gate="needs.changes.outputs.measure-sizing == 'true'"
  for job in e2e-controlplane e2e-controlplane-sso tempest; do
    case "$job" in
      e2e-controlplane)
        last="Run own-namespace registration E2E test"
        artifact="sizing-e2e-controlplane" ;;
      e2e-controlplane-sso)
        last="Run federated ControlPlane E2E test"
        artifact="sizing-e2e-controlplane-sso" ;;
      tempest)
        last="Upload Tempest results"
        artifact='sizing-tempest-${{ matrix.service }}-${{ matrix.release }}' ;;
    esac
    steps="$(yq -r ".jobs[\"$job\"].steps[].name // \"-\"" "$CI_YAML")"
    idx_setup="$(step_index "$steps" "Setup E2E infrastructure")"
    idx_start="$(step_index "$steps" "Start the sizing measurement")"
    idx_last="$(step_index "$steps" "$last")"
    idx_collect="$(step_index "$steps" "Collect the sizing measurement")"
    idx_upload="$(step_index "$steps" "Upload the sizing measurement")"
    idx_dump="$(step_index "$steps" "Dump diagnostic info")"

    assert_eq "$job: setup passes WITH_VPA from the label" \
      "\${{ $gate && 'true' || '' }}" \
      "$(yq -r ".jobs[\"$job\"].steps[] | select(.name == \"Setup E2E infrastructure\") | .env.WITH_VPA" "$CI_YAML")"
    assert_eq "$job: the start step follows the setup step" "$((idx_setup + 1))" "${idx_start:-none}"
    assert_eq "$job: the collect step follows the last suite step" "$((idx_last + 1))" "${idx_collect:-none}"
    assert_eq "$job: the upload step follows the collect step" "$((idx_collect + 1))" "${idx_upload:-none}"
    assert_eq "$job: the diagnostics dump follows the upload step" "$((idx_upload + 1))" "${idx_dump:-none}"
    assert_eq "$job: the start step runs only under the label" "$gate" \
      "$(yq -r ".jobs[\"$job\"].steps[] | select(.name == \"Start the sizing measurement\") | .if" "$CI_YAML")"
    assert_eq "$job: collect and upload run always under the label" \
      "always() && $gate always() && $gate " \
      "$(yq -r ".jobs[\"$job\"].steps[] | select(.name == \"Collect the sizing measurement\" or .name == \"Upload the sizing measurement\") | .if" "$CI_YAML" | tr '\n' ' ')"
    assert_eq "$job: the artifact name" "$artifact" \
      "$(yq -r ".jobs[\"$job\"].steps[] | select(.name == \"Upload the sizing measurement\") | .with.name" "$CI_YAML")"
    assert_eq "$job: the artifact is kept 14 days" "14" \
      "$(yq -r ".jobs[\"$job\"].steps[] | select(.name == \"Upload the sizing measurement\") | .with[\"retention-days\"]" "$CI_YAML")"
  done

  assert_eq "only the tempest leg names MEASURE_LEG" \
    'null null ${{ matrix.service }}-${{ matrix.release }} ' \
    "$(for job in e2e-controlplane e2e-controlplane-sso tempest; do
         yq -r ".jobs[\"$job\"].steps[] | select(.name == \"Collect the sizing measurement\") | .env.MEASURE_LEG" "$CI_YAML"
       done | tr '\n' ' ')"
}

# ---------------------------------------------------------------------------
# Tests 11 to 13: prepare
# ---------------------------------------------------------------------------

# crd <versions json>
# The MariaDB CRD in the shape `kubectl get crd -o json` prints it, with the
# given spec.versions.
crd() {
  jq -cn --argjson versions "$1" '
    {apiVersion: "apiextensions.k8s.io/v1", kind: "CustomResourceDefinition",
     metadata: {name: "mariadbs.k8s.mariadb.com", resourceVersion: "4711"},
     spec: {group: "k8s.mariadb.com", scope: "Namespaced",
            names: {kind: "MariaDB", plural: "mariadbs"}, versions: $versions}}'
}

# The first version serves status and scale, the second scale alone, the
# third no subresource.
SCALED_VERSIONS='[{"name":"v1alpha1","served":true,"storage":true,"subresources":{"status":{},"scale":{"specReplicasPath":".spec.replicas"}}},{"name":"v1alpha2","served":true,"storage":false,"subresources":{"scale":{"specReplicasPath":".spec.replicas"}}},{"name":"v1beta1","served":true,"storage":false}]'

test_prepare_removes_the_scale_subresource() {
  echo "Test: prepare removes the scale subresource with one replace and keeps the rest"
  local tmp rc
  tmp="$(new_tmp)"
  reset_stub "$tmp"
  STUB_CRD_JSON="$(crd "$SCALED_VERSIONS")"
  export STUB_CRD_JSON

  run_vpa "$tmp" prepare
  rc=$?
  assert_eq "exit code is 0" "0" "$rc"
  assert_eq "stdout says the subresource was removed" "MariaDB CRD scale subresource removed" "$(cat "$tmp/out")"
  assert_eq "the CRD is read by name" "1" \
    "$(grep -c '^\[get\]\[crd\]\[mariadbs\.k8s\.mariadb\.com\]\[-o\]\[json\]$' "$KUBECTL_LOG")"
  assert_eq "one kubectl replace -f - is sent" "1" "$(grep -c '^\[replace\]\[-f\]\[-\]$' "$KUBECTL_LOG")"
  assert_eq "the replace carries one document, the MariaDB CRD" "mariadbs.k8s.mariadb.com" \
    "$(jq -rs 'if length == 1 then .[0].metadata.name else "\(length) documents" end' "$STUB_REPLACE_FILE" 2>&1)"
  assert_eq "no version of the replaced CRD serves scale" "0" \
    "$(jq '[.spec.versions[] | select(.subresources.scale != null)] | length' "$STUB_REPLACE_FILE" 2>&1)"
  assert_eq "the first version keeps its status subresource" '{"status":{}}' \
    "$(jq -c '.spec.versions[0].subresources' "$STUB_REPLACE_FILE" 2>&1)"
  assert_eq "the second version loses its scale subresource" '{}' \
    "$(jq -c '.spec.versions[1].subresources' "$STUB_REPLACE_FILE" 2>&1)"
  assert_eq "the third version gains no subresources key" "false" \
    "$(jq '.spec.versions[2] | has("subresources")' "$STUB_REPLACE_FILE" 2>&1)"

  # Only a later version serves scale.
  reset_stub "$tmp"
  STUB_CRD_JSON="$(crd '[{"name":"v1alpha1"},{"name":"v1alpha2","subresources":{"scale":{}}}]')"
  export STUB_CRD_JSON
  run_vpa "$tmp" prepare
  rc=$?
  assert_eq "scale on a later version only: exit code is 0" "0" "$rc"
  assert_eq "scale on a later version only: stdout says the subresource was removed" \
    "MariaDB CRD scale subresource removed" "$(cat "$tmp/out")"
  assert_eq "scale on a later version only: one kubectl replace -f - is sent" "1" \
    "$(grep -c '^\[replace\]\[-f\]\[-\]$' "$KUBECTL_LOG")"
  assert_eq "scale on a later version only: the replaced CRD serves no scale" "0" \
    "$(jq '[.spec.versions[] | select(.subresources.scale != null)] | length' "$STUB_REPLACE_FILE" 2>&1)"
  rm -rf "$tmp"
}

test_prepare_without_a_scale_subresource() {
  echo "Test: prepare replaces nothing when no version serves the scale subresource"
  local tmp rc entry label versions
  tmp="$(new_tmp)"
  for entry in \
    'versions with a status subresource only|[{"name":"v1alpha1","served":true,"storage":true,"subresources":{"status":{}}}]' \
    'a version without a subresources key|[{"name":"v1alpha1","served":true,"storage":true}]' \
    'versions: []|[]'; do
    label="${entry%%|*}"
    versions="${entry#*|}"
    reset_stub "$tmp"
    STUB_CRD_JSON="$(crd "$versions")"
    export STUB_CRD_JSON

    run_vpa "$tmp" prepare
    rc=$?
    assert_eq "$label: exit code is 0" "0" "$rc"
    assert_eq "$label: stdout says no version serves it" "MariaDB CRD serves no scale subresource" "$(cat "$tmp/out")"
    assert_eq "$label: no kubectl replace" "0" "$(grep -c '^\[replace\]' "$KUBECTL_LOG")"
  done
  rm -rf "$tmp"
}

test_prepare_errors() {
  echo "Test: prepare exits 2 on a failing read, a failing replace and an empty answer"
  local tmp rc
  tmp="$(new_tmp)"

  reset_stub "$tmp"
  export STUB_CRD_FAIL=1
  run_vpa "$tmp" prepare
  rc=$?
  assert_eq "a failing read exits 2" "2" "$rc"
  assert_eq "the message carries kubectl's stderr" \
    'ci-vpa-recommendations: reading the MariaDB CRD mariadbs.k8s.mariadb.com failed: Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "mariadbs.k8s.mariadb.com" not found' \
    "$(cat "$tmp/err")"
  assert_eq "a failing read sends no replace" "0" "$(grep -c '^\[replace\]' "$KUBECTL_LOG")"

  reset_stub "$tmp"
  STUB_CRD_JSON="$(crd "$SCALED_VERSIONS")"
  export STUB_CRD_JSON
  export STUB_REPLACE_FAIL=1
  run_vpa "$tmp" prepare
  rc=$?
  assert_eq "a failing replace exits 2" "2" "$rc"
  assert_starts_with "the message names the replace" "$(cat "$tmp/err")" \
    'ci-vpa-recommendations: replacing the MariaDB CRD mariadbs.k8s.mariadb.com failed:'
  assert_contains "the message carries kubectl's stderr" "$(cat "$tmp/err")" \
    'Error from server (Conflict): the object has been modified'
  assert_eq "a failing replace prints no result line" "" "$(cat "$tmp/out")"

  reset_stub "$tmp"
  export STUB_CRD_JSON=''
  run_vpa "$tmp" prepare
  rc=$?
  assert_eq "an empty answer exits 2" "2" "$rc"
  assert_starts_with "the message says the CRD cannot be parsed" "$(cat "$tmp/err")" \
    'ci-vpa-recommendations: cannot parse the MariaDB CRD mariadbs.k8s.mariadb.com'
  assert_eq "an empty answer sends no replace" "0" "$(grep -c '^\[replace\]' "$KUBECTL_LOG")"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if [ -z "$JQ_BIN" ]; then
  echo "SKIP: jq not installed (every case needs it)"
  SKIP=$((SKIP + 1))
else
  test_snapshot_applies_one_vpa_per_workload
  test_snapshot_keeps_earlier_files
  test_snapshot_empty_namespace
  test_snapshot_errors
  test_watch_survives_a_failing_pass
  test_report_rows
  test_report_offline_defaults
  test_report_errors
  test_usage_errors
  test_prepare_removes_the_scale_subresource
  test_prepare_without_a_scale_subresource
  test_prepare_errors
fi
test_wiring

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
