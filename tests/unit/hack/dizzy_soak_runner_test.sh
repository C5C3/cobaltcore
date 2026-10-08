#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the in-pod runner of the dizzy soak, hack/dizzy-soak-runner.sh
# (#1274):
#   1. sample_usage converts the CPU quantities 1500000n, 250u, 12m, 2 and 0
#      and the memory quantities 1024Ki, 3Mi, 1Gi and 4096, and refuses a
#      quantity of another form.
#   2. derive_restarts counts a restartCount that rises from 1 to 3 as 2, a
#      pod created during the run from 0, and an OOMKilled termination inside
#      the run once, not one before it. derive_usage_summary averages the
#      first and the last tenth of a container's samples and the growth
#      between them. read_conditions skips a kind whose CRD is absent without
#      an error and keeps a list larger than one command-line argument, and
#      derive_conditions lists a transition inside the run and shows - for an
#      object without a Ready condition.
#   3. A run whose dizzy exits 0 with one run record and the clean leak line,
#      with a Ready ControlPlane and no restart, writes the full report
#      directory and verdict.json PASS with four passing checks, and exits 0.
#      dizzy gets the arguments and environment of the soak, and an empty
#      DIZZY_ARGS adds no argument.
#   4. A TERM sent twice while dizzy runs reaches dizzy once; the runner
#      waits for dizzy, writes the report and exits by verdict. A TERM before
#      dizzy starts skips dizzy, and the report still names the early stop.
#   5. Every way a check fails: dizzy exits 3, dizzy writes no record or
#      two, the leak check finds resources or prints nothing, the
#      ControlPlane is not Ready, gone at the end, absent or forbidden, a
#      container restarts, every pod read fails; and the ways it passes: the
#      clean leak line of glance and keystone, no container observed, no
#      metrics API.
#   6. The error-rate and p95 checks run only when their setting is set, and
#      decide against it. A json report that fails to render leaves no
#      dizzy-report.json, and error-rate fails for want of it.
#   7. With less than 512 MiB free on the claim the runner exits 1 before
#      dizzy starts.
#   8. The OTLP endpoint is the VictoriaMetrics Service the Grafana
#      datasource reads.
#   9. When the pinned dizzy's source is cached in _output/dizzy/<pin>/,
#      leak-check passes on every clean leak line it prints.
#
# The runner is sourced in a subshell (its BASH_SOURCE guard keeps main()
# from running) with stub kubectl and df and a fake dizzy first on PATH;
# REPORTS_DIR, DIZZY_BIN, SCENARIO_FILE and CLOUDS_FILE are pointed at a
# temporary directory after sourcing.
#
# Usage: bash tests/unit/hack/dizzy_soak_runner_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RUNNER_SH="$PROJECT_ROOT/hack/dizzy-soak-runner.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CLEAN_LEAK_LINE="leak check: no run-tagged resources remain"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# A kubectl stub, a df stub and a fake dizzy. kubectl appends its argv to
# $CALL_LOG and answers from the environment:
#   KUBECTL_PODS_RC       non-empty: `get pods` fails with Forbidden
#   KUBECTL_PODS_FIRST    file answering the first `get pods` (default
#                         pods.json beside the stub)
#   KUBECTL_PODS_LATER    file answering every later one (default: the first)
#   KUBECTL_METRICS_RC    non-empty: the metrics API answers NotFound
#   KUBECTL_CP            the ControlPlane read: ready (default), notready,
#                         none or forbidden; KUBECTL_CP_END applies to the
#                         second read alone
#   KUBECTL_ABSENT_KINDS  space-separated kinds whose CRD is absent
#   KUBECTL_BLOCK_NAMESPACES  non-empty: `get namespaces` waits until the
#                         file KUBECTL_RELEASE exists
# A kind whose CRD the stub knows answers <kind>.json beside the stub when
# that file exists, otherwise an empty list, keystones one Keystone with a
# transition inside the run.
# df prints DF_FREE_KIB (default 10 GiB) as the available KiB.
# dizzy appends `<argv>` and the three settings of its environment to
# $DIZZY_CALLS and, for chaos, runs FAKE_DIZZY_SECONDS (default 0) unless a
# TERM comes, which it records in $DIZZY_SIGNALS and answers after a second
# of teardown. It then writes run-x.json (unless FAKE_DIZZY_RECORD is 0;
# with 2 run-y.json too), prints FAKE_DIZZY_LEAK (default the clean line;
# "none" prints none) and exits FAKE_DIZZY_RC (default 0). report prints
# FAKE_REPORT_JSON for json, and fails for the format FAKE_REPORT_FAIL.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/pods.json" <<'JSON'
{"items":[
 {"metadata":{"namespace":"openstack","name":"keystone-0","uid":"uid-keystone","creationTimestamp":"2026-01-01T00:00:00Z",
   "ownerReferences":[{"kind":"StatefulSet","name":"keystone"}]},
  "status":{"containerStatuses":[{"name":"keystone","restartCount":1,
   "lastState":{"terminated":{"reason":"Error","finishedAt":"2026-01-02T00:00:00Z"}}}]}},
 {"metadata":{"namespace":"kube-system","name":"coredns-1","uid":"uid-coredns","creationTimestamp":"2026-01-01T00:00:00Z"},
  "status":{"containerStatuses":[{"name":"coredns","restartCount":7}]}},
 {"metadata":{"namespace":"openstack","name":"keystone-db-sync","uid":"uid-sync","creationTimestamp":"2026-01-01T00:00:00Z",
   "ownerReferences":[{"kind":"Job","name":"keystone-db-sync"}]},
  "status":{"initContainerStatuses":[{"name":"wait","restartCount":0}],"containerStatuses":[{"name":"sync","restartCount":0}]}}
]}
JSON
  cat >"$dir/metrics.json" <<'JSON'
{"items":[
 {"metadata":{"namespace":"openstack","name":"keystone-0"},"containers":[{"name":"keystone","usage":{"cpu":"1500000n","memory":"1024Ki"}}]},
 {"metadata":{"namespace":"kube-system","name":"coredns-1"},"containers":[{"name":"coredns","usage":{"cpu":"1m","memory":"1Mi"}}]}
]}
JSON
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
echo "kubectl $*" >>"$CALL_LOG"
count() {
  local n
  n=$(( $(cat "$dir/$1.count" 2>/dev/null || echo 0) + 1 ))
  echo "$n" >"$dir/$1.count"
  echo "$n"
}
case "$*" in
  "get pods -A -o json")
    if [ -n "${KUBECTL_PODS_RC:-}" ]; then
      echo 'Error from server (Forbidden): pods is forbidden: User "system:serviceaccount:dizzy:dizzy-soak" cannot list resource "pods" in API group "" at the cluster scope' >&2
      exit 1
    fi
    first="${KUBECTL_PODS_FIRST:-$dir/pods.json}"
    if [ "$(count pods)" -gt 1 ]; then cat "${KUBECTL_PODS_LATER:-$first}"; else cat "$first"; fi
    ;;
  "get --raw /apis/metrics.k8s.io/v1beta1/pods")
    if [ -n "${KUBECTL_METRICS_RC:-}" ]; then
      echo 'Error from server (NotFound): the server could not find the requested resource' >&2
      exit 1
    fi
    cat "$dir/metrics.json"
    ;;
  "get namespaces -o json")
    if [ -n "${KUBECTL_BLOCK_NAMESPACES:-}" ]; then
      while [ ! -e "$KUBECTL_RELEASE" ]; do sleep 0.1; done
    fi
    echo '{"items":[{"metadata":{"name":"default"}},{"metadata":{"name":"dizzy"}},{"metadata":{"name":"openstack"}},{"metadata":{"name":"kube-system"}}]}'
    ;;
  "get controlplanes.c5c3.io -A -o json")
    state="${KUBECTL_CP:-ready}"
    if [ "$(count controlplanes)" -gt 1 ] && [ -n "${KUBECTL_CP_END:-}" ]; then state="$KUBECTL_CP_END"; fi
    case "$state" in
      forbidden)
        echo 'Error from server (Forbidden): controlplanes.c5c3.io is forbidden: User "system:serviceaccount:dizzy:dizzy-soak" cannot list resource "controlplanes" in API group "c5c3.io" at the cluster scope' >&2
        exit 1
        ;;
      none) echo '{"items":[]}' ;;
      notready) status=False ;;
      *) status=True ;;
    esac
    [ "$state" = none ] || printf '{"items":[{"kind":"ControlPlane","metadata":{"namespace":"openstack","name":"controlplane"},"status":{"conditions":[{"type":"Ready","status":"%s","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}]}\n' "$status"
    ;;
  "get "*" -A -o json")
    kind="${2}"
    case " ${KUBECTL_ABSENT_KINDS:-} " in
      *" ${kind} "*)
        if [ "$kind" = "barbicans.barbican.openstack.c5c3.io" ]; then
          echo "error: resource mapping not found: no matches for kind \"Barbican\" in version \"barbican.openstack.c5c3.io/v1alpha1\"" >&2
        else
          echo "error: the server doesn't have a resource type \"${kind%%.*}\"" >&2
        fi
        exit 1
        ;;
    esac
    if [ -f "$dir/$kind.json" ]; then
      cat "$dir/$kind.json"
    elif [ "$kind" = "keystones.keystone.openstack.c5c3.io" ]; then
      echo '{"items":[{"kind":"Keystone","metadata":{"namespace":"openstack","name":"controlplane-keystone"},"status":{"conditions":[
        {"type":"Ready","status":"True","lastTransitionTime":"2099-01-01T00:00:00Z"},
        {"type":"DatabaseReady","status":"True","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}]}'
    else
      echo '{"items":[]}'
    fi
    ;;
esac
exit 0
STUB
  cat >"$dir/df" <<'STUB'
#!/bin/bash
echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "/dev/claim 5242880 0 ${DF_FREE_KIB:-10485760} 0% $2"
STUB
  cat >"$dir/dizzy" <<'STUB'
#!/bin/bash
echo "dizzy $* | OS_CLIENT_CONFIG_FILE=${OS_CLIENT_CONFIG_FILE:-} OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=${OTEL_EXPORTER_OTLP_METRICS_ENDPOINT:-} OTEL_METRIC_EXPORT_INTERVAL=${OTEL_METRIC_EXPORT_INTERVAL:-}" >>"$DIZZY_CALLS"
if [ "$2" = report ]; then
  report='{"metrics":{"overall":{"attempted":100,"failed":0,"latency":{"p95":1500000000}}}}'
  case "$*" in
    *"--format ${FAKE_REPORT_FAIL:-none}"*) echo partial; echo boom >&2; exit 1 ;;
    *"--format json"*) printf '%s\n' "${FAKE_REPORT_JSON:-$report}" ;;
    *"--format html"*) echo '<html>dizzy</html>' ;;
    *) echo 'TABLE OF THE FAKE DIZZY' ;;
  esac
  exit 0
fi
stopped=""
trap 'echo TERM >>"$DIZZY_SIGNALS"; stopped=1' TERM
end=$((SECONDS + ${FAKE_DIZZY_SECONDS:-0}))
while [ -z "$stopped" ] && [ "$SECONDS" -lt "$end" ]; do sleep 0.1; done
if [ -n "$stopped" ]; then sleep 1; fi
echo "churn finished"
if [ "${FAKE_DIZZY_RECORD:-1}" != 0 ]; then echo '{}' >run-x.json; echo "run record written to run-x.json"; fi
if [ "${FAKE_DIZZY_RECORD:-1}" = 2 ]; then echo '{}' >run-y.json; fi
if [ "${FAKE_DIZZY_LEAK:-}" != none ]; then echo "${FAKE_DIZZY_LEAK:-leak check: no run-tagged resources remain}"; fi
exit "${FAKE_DIZZY_RC:-0}"
STUB
  chmod +x "$dir/kubectl" "$dir/df" "$dir/dizzy"
}

# new_case <tmp> — A fresh stub directory, report claim and logs under <tmp>.
new_case() {
  local tmp="$1"
  rm -rf "$tmp"
  mkdir -p "$tmp/reports"
  make_stubs "$tmp/bin"
  printf 'name: lab-soak\n' >"$tmp/scenario.yaml"
  export CALL_LOG="$tmp/calls.log" DIZZY_CALLS="$tmp/dizzy-calls.log" DIZZY_SIGNALS="$tmp/dizzy-signals.log"
  : >"$CALL_LOG"
  : >"$DIZZY_CALLS"
  : >"$DIZZY_SIGNALS"
}

# in_runner <tmp> [env_var=value...] — Source the runner in the current
# (sub)shell with the stubs of <tmp>/bin first on PATH and the given
# settings, and point its paths into <tmp>. Call it in a subshell.
# shellcheck disable=SC2034 # the sourced runner reads the four paths
in_runner() {
  local tmp="$1"
  shift
  unset DIZZY_SOAK_SERVICE DIZZY_SOAK_DURATION DIZZY_ARGS DIZZY_VERSION DIZZY_SOAK_NAMESPACES \
    DIZZY_SOAK_MAX_ERROR_RATE DIZZY_SOAK_MAX_P95_SECONDS
  export DIZZY_SOAK_SAMPLE_INTERVAL=1 DIZZY_VERSION=v0.3.0
  local assignment
  for assignment in "$@"; do
    export "${assignment?}"
  done
  PATH="$tmp/bin:$PATH"
  export PATH
  # shellcheck source=/dev/null
  source "$RUNNER_SH"
  REPORTS_DIR="$tmp/reports"
  DIZZY_BIN="$tmp/bin/dizzy"
  SCENARIO_FILE="$tmp/scenario.yaml"
  CLOUDS_FILE="$tmp/clouds.yaml"
}

# run_runner <tmp> [env_var=value...] — Run main() in a subshell; its output
# goes to <tmp>/runner.log. Returns its exit status and sets RUN_DIR to the
# run directory it created.
run_runner() {
  local tmp="$1" rc
  shift
  (
    in_runner "$tmp" "$@"
    main
  ) >"$tmp/runner.log" 2>&1
  rc=$?
  RUN_DIR="$(find "$tmp/reports" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  return "$rc"
}

# check_of <name> — The "pass detail" of a check in $RUN_DIR/verdict.json,
# "absent" when it did not run.
check_of() {
  jq -r --arg n "$1" '([.checks[] | select(.name == $n)] | first) as $c
    | if $c then "\($c.pass) \($c.detail)" else "absent" end' "$RUN_DIR/verdict.json" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Test 1: quantities
# ---------------------------------------------------------------------------
test_quantities() {
  echo "Test: sample_usage converts the quantities of the metrics API"

  local tmp usage memory
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  mkdir -p "$tmp/run/platform"
  : >"$tmp/run/platform/errors.log"
  # Each container is named <cpu>-<memory> after the quantities it reports.
  cat >"$tmp/bin/metrics.json" <<'JSON'
{"items":[{"metadata":{"namespace":"openstack","name":"quantities-0"},"containers":[
 {"name":"1500000n-1024Ki","usage":{"cpu":"1500000n","memory":"1024Ki"}},
 {"name":"250u-3Mi","usage":{"cpu":"250u","memory":"3Mi"}},
 {"name":"12m-1Gi","usage":{"cpu":"12m","memory":"1Gi"}},
 {"name":"2-4096","usage":{"cpu":"2","memory":"4096"}},
 {"name":"0-4096","usage":{"cpu":"0","memory":"4096"}},
 {"name":"2k-1Mi","usage":{"cpu":"2k","memory":"1Mi"}},
 {"name":"1m-1G","usage":{"cpu":"1m","memory":"1G"}},
 {"name":"1m-","usage":{"cpu":"1m","memory":""}}]}]}
JSON
  (
    in_runner "$tmp"
    cd "$tmp/run" || exit 1
    sample_usage
  )
  usage="$(cat "$tmp/run/platform/usage.tsv")"
  assert_eq "CPU in millicores" "1500000n=1.5 250u=0.25 12m=12 2=2000 0=0" \
    "$(awk -F '\t' '{ split($4, q, "-"); printf "%s=%s ", q[1], $5 }' <<<"$usage" | sed 's/ $//')"
  memory="$(awk -F '\t' '{ split($4, q, "-"); printf "%s=%s ", q[2], $6 }' <<<"$usage")"
  assert_contains "1024Ki is 1048576 bytes" "$memory" "1024Ki=1048576 "
  assert_contains "3Mi is 3145728 bytes" "$memory" "3Mi=3145728 "
  assert_contains "1Gi is 1073741824 bytes" "$memory" "1Gi=1073741824 "
  assert_contains "4096 is 4096 bytes" "$memory" "4096=4096 "
  assert_not_contains "a CPU quantity with the suffix k is refused" "$usage" $'\t2k-1Mi\t'
  assert_not_contains "a memory quantity with the suffix G is refused" "$usage" $'\t1m-1G\t'
  assert_not_contains "an empty quantity is refused" "$usage" $'\t1m-\t'
  assert_eq "each refused line is logged" "3" \
    "$(grep -c ' usage: cannot convert the quantities ' "$tmp/run/platform/errors.log")"
}

# ---------------------------------------------------------------------------
# Test 2: the derived tables
# ---------------------------------------------------------------------------
test_derive_restarts() {
  echo "Test: derive_restarts counts restarts since the first sample and OOM kills inside the run"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  mkdir -p "$tmp/run/platform"
  # A container whose restartCount rises from 1 to 3; one of a pod created
  # during the run at 1; one OOM-killed inside the run, seen twice, and once
  # before it; and a container without a change.
  printf '%s\n' \
    $'2026-10-12T08:15:00Z\topenstack\tkeystone-0\tuid-a\t2026-01-01T00:00:00Z\tStatefulSet\tkeystone\t1\t-\t-' \
    $'2026-10-12T08:15:00Z\topenstack\tnova-api-0\tuid-c\t2026-01-01T00:00:00Z\tReplicaSet\tapi\t4\tOOMKilled\t2026-10-01T00:00:00Z' \
    $'2026-10-12T08:15:00Z\topenstack\tglance-0\tuid-d\t2026-01-01T00:00:00Z\tStatefulSet\tglance\t0\t-\t-' \
    $'2026-10-12T08:16:00Z\topenstack\tkeystone-0\tuid-a\t2026-01-01T00:00:00Z\tStatefulSet\tkeystone\t2\t-\t-' \
    $'2026-10-12T08:16:00Z\topenstack\tneutron-1\tuid-b\t2026-10-12T08:15:30Z\tReplicaSet\tserver\t1\tError\t2026-10-12T08:15:50Z' \
    $'2026-10-12T08:16:00Z\topenstack\tnova-api-0\tuid-c\t2026-01-01T00:00:00Z\tReplicaSet\tapi\t5\tOOMKilled\t2026-10-12T08:15:40Z' \
    $'2026-10-12T08:17:00Z\topenstack\tkeystone-0\tuid-a\t2026-01-01T00:00:00Z\tStatefulSet\tkeystone\t3\t-\t-' \
    $'2026-10-12T08:17:00Z\topenstack\tnova-api-0\tuid-c\t2026-01-01T00:00:00Z\tReplicaSet\tapi\t5\tOOMKilled\t2026-10-12T08:15:40Z' \
    $'2026-10-12T08:17:00Z\topenstack\tglance-0\tuid-d\t2026-01-01T00:00:00Z\tStatefulSet\tglance\t0\t-\t-' \
    >"$tmp/run/platform/containers.tsv"
  (
    in_runner "$tmp"
    cd "$tmp/run" || exit 1
    RUN_START="2026-10-12T08:15:00Z"
    derive_restarts
  )
  local table
  table="$(cat "$tmp/run/platform/restarts.tsv")"
  assert_eq "the header names the columns" \
    $'namespace\tpod\tcontainer\tpod_uid\towner_kind\trestarts\toom_kills' "$(head -n 1 <<<"$table")"
  assert_contains "a restartCount rising from 1 to 3 counts 2" "$table" \
    $'openstack\tkeystone-0\tkeystone\tuid-a\tStatefulSet\t2\t0'
  assert_contains "a pod created during the run counts from 0" "$table" \
    $'openstack\tneutron-1\tserver\tuid-b\tReplicaSet\t1\t0'
  assert_contains "an OOM kill inside the run counts once, one before it not at all" "$table" \
    $'openstack\tnova-api-0\tapi\tuid-c\tReplicaSet\t1\t1'
  assert_contains "an unchanged container counts 0" "$table" \
    $'openstack\tglance-0\tglance\tuid-d\tStatefulSet\t0\t0'
  assert_eq "one row per pod UID and container" "5" "$(grep -c . <<<"$table")"

  # No sample at all: the header alone.
  : >"$tmp/run/platform/containers.tsv"
  (
    in_runner "$tmp"
    cd "$tmp/run" || exit 1
    RUN_START="2026-10-12T08:15:00Z"
    derive_restarts
  )
  assert_eq "without samples restarts.tsv holds its header alone" "1" "$(grep -c . "$tmp/run/platform/restarts.tsv")"
}

test_derive_usage_summary() {
  echo "Test: derive_usage_summary averages the first and last tenth of each container's samples"

  local tmp i
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  mkdir -p "$tmp/run/platform"
  # keystone: 10 samples, memory 100 to 1000 MiB, CPU 10 to 100m; a tenth is
  # one sample. glance: 3 samples, a tenth is still one.
  for i in 1 2 3 4 5 6 7 8 9 10; do
    printf '2026-10-12T08:%02d:00Z\topenstack\tkeystone-0\tkeystone\t%d\t%d\n' "$i" "$((i * 10))" "$((i * 100 * 1048576))"
  done >"$tmp/run/platform/usage.tsv"
  for i in 1 2 3; do
    printf '2026-10-12T08:%02d:00Z\topenstack\tglance-0\tglance\t5\t%d\n' "$i" "$((200 * 1048576))"
  done >>"$tmp/run/platform/usage.tsv"
  (
    in_runner "$tmp"
    cd "$tmp/run" || exit 1
    derive_usage_summary
  )
  local table
  table="$(cat "$tmp/run/platform/usage-summary.tsv")"
  assert_eq "the header names the columns" \
    $'namespace\tpod\tcontainer\tsamples\tcpu_mean_m\tcpu_max_m\tmemory_first_mib\tmemory_last_mib\tmemory_max_mib\tgrowth_mib\tgrowth_percent' \
    "$(head -n 1 <<<"$table")"
  assert_contains "keystone: 10 samples, CPU mean 55 and max 100, memory 100 to 1000 MiB, +900 MiB, +900%" "$table" \
    $'openstack\tkeystone-0\tkeystone\t10\t55.0\t100.0\t100.0\t1000.0\t1000.0\t900.0\t900.0'
  assert_contains "glance: 3 samples without growth" "$table" \
    $'openstack\tglance-0\tglance\t3\t5.0\t5.0\t200.0\t200.0\t200.0\t0.0\t0.0'

  # Twenty samples: a tenth is two.
  for i in $(seq 1 20); do
    printf '2026-10-12T08:%02d:00Z\topenstack\tnova-0\tnova\t1\t%d\n' "$i" "$((i * 1048576))"
  done >"$tmp/run/platform/usage.tsv"
  (
    in_runner "$tmp"
    cd "$tmp/run" || exit 1
    derive_usage_summary
  )
  assert_contains "with 20 samples the first tenth is samples 1-2, the last 19-20" \
    "$(cat "$tmp/run/platform/usage-summary.tsv")" $'\t20\t1.0\t1.0\t1.5\t19.5\t20.0\t18.0\t1200.0'

  : >"$tmp/run/platform/usage.tsv"
  (
    in_runner "$tmp"
    cd "$tmp/run" || exit 1
    derive_usage_summary
  )
  assert_eq "without samples usage-summary.tsv holds its header alone" "1" \
    "$(grep -c . "$tmp/run/platform/usage-summary.tsv")"
}

test_conditions() {
  echo "Test: read_conditions skips an absent CRD silently, derive_conditions lists transitions inside the run"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  mkdir -p "$tmp/run/platform"
  : >"$tmp/run/platform/errors.log"
  (
    in_runner "$tmp" KUBECTL_ABSENT_KINDS="horizons.horizon.openstack.c5c3.io barbicans.barbican.openstack.c5c3.io"
    cd "$tmp/run" || exit 1
    # shellcheck disable=SC2034 # read by the sourced derive_conditions
    RUN_START="2026-10-12T08:15:00Z"
    read_conditions platform/conditions-start.json
    read_conditions platform/conditions-end.json
    derive_conditions
  )
  local table
  table="$(cat "$tmp/run/platform/conditions.tsv")"
  assert_eq "every kind is read, at the start and at the end" "26" "$(grep -c -- ' -A -o json$' "$CALL_LOG")"
  assert_eq "nothing is logged as an error" "" "$(cat "$tmp/run/platform/errors.log")"
  assert_eq "the absent kinds are not in the read" "0" \
    "$(jq '[.errors[]] | length' "$tmp/run/platform/conditions-end.json")"
  assert_not_contains "nor in conditions.tsv" "$table" "horizons"
  assert_contains "the Keystone and its transition inside the run are listed" "$table" \
    $'keystones.keystone.openstack.c5c3.io\topenstack\tcontrolplane-keystone\tTrue\tTrue\tReady=True 2099-01-01T00:00:00Z'
  assert_contains "the ControlPlane without a transition in the run shows -" "$table" \
    $'controlplanes.c5c3.io\topenstack\tcontrolplane\tTrue\tTrue\t-'
  assert_not_contains "a transition before the run is not listed" "$table" "DatabaseReady"

  # A Forbidden read is an error: logged, and in the read's errors.
  (
    in_runner "$tmp" KUBECTL_CP=forbidden
    cd "$tmp/run" || exit 1
    read_conditions platform/conditions-end.json
  )
  assert_contains "a Forbidden read is logged" "$(cat "$tmp/run/platform/errors.log")" \
    "conditions controlplanes.c5c3.io: Error from server (Forbidden)"
  assert_eq "and recorded in the read" "controlplanes.c5c3.io" \
    "$(jq -r '.errors[].kind' "$tmp/run/platform/conditions-end.json")"

  # An object without conditions: - for Ready and for its transitions.
  new_case "$tmp"
  mkdir -p "$tmp/run/platform"
  : >"$tmp/run/platform/errors.log"
  echo '{"items":[{"metadata":{"namespace":"openstack","name":"glance"},"status":{}}]}' \
    >"$tmp/bin/glances.glance.openstack.c5c3.io.json"
  (
    in_runner "$tmp"
    cd "$tmp/run" || exit 1
    # shellcheck disable=SC2034 # read by the sourced derive_conditions
    RUN_START="2026-10-12T08:15:00Z"
    read_conditions platform/conditions-start.json
    read_conditions platform/conditions-end.json
    derive_conditions
  )
  assert_contains "an object without a Ready condition shows - at both ends" \
    "$(cat "$tmp/run/platform/conditions.tsv")" $'glances.glance.openstack.c5c3.io\topenstack\tglance\t-\t-\t-'
}

test_conditions_beyond_one_argument() {
  echo "Test: read_conditions keeps a list larger than one command-line argument"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  mkdir -p "$tmp/run/platform"
  : >"$tmp/run/platform/errors.log"
  # 3000 Glances with a message of 500 bytes, about 2 MB: above the 128 KiB
  # Linux allows one argument and the 1 MiB macOS allows all of them.
  jq -n '{items: [range(3000) | {metadata: {namespace: "openstack", name: "glance-\(.)"},
    status: {conditions: [{type: "Ready", status: "False", lastTransitionTime: "2026-01-01T00:00:00Z",
      message: ("x" * 500)}]}}]}' >"$tmp/bin/glances.glance.openstack.c5c3.io.json"
  (
    in_runner "$tmp"
    cd "$tmp/run" || exit 1
    read_conditions platform/conditions-end.json
  )
  assert_eq "nothing is logged as an error" "" "$(cat "$tmp/run/platform/errors.log")"
  assert_eq "the read holds the ControlPlane, the Keystone and the 3000 Glances" "3002 0" \
    "$(jq -r '"\(.items | length) \(.errors | length)"' "$tmp/run/platform/conditions-end.json")"
}

# ---------------------------------------------------------------------------
# Test 3: a clean run
# ---------------------------------------------------------------------------
test_clean_run() {
  echo "Test: a clean run writes the full report and passes"

  local tmp rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  run_runner "$tmp"
  rc=$?
  assert_eq "the runner exits 0" "0" "$rc"
  assert_eq "the verdict is PASS" "PASS" "$(jq -r .verdict "$RUN_DIR/verdict.json" 2>/dev/null)"
  assert_eq "four checks ran and passed" "dizzy-exit=true leak-check=true controlplane-ready=true no-restarts=true" \
    "$(jq -r '[.checks[] | .name + "=" + (.pass | tostring)] | join(" ")' "$RUN_DIR/verdict.json" 2>/dev/null)"
  if [[ "$(basename "${RUN_DIR:-x}")" =~ ^[0-9]{8}T[0-9]{6}Z$ ]]; then
    echo "  PASS: the run directory is named after the UTC start ($(basename "$RUN_DIR"))"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the run directory '${RUN_DIR:-}' is not named <YYYYMMDD>T<HHMMSS>Z"
    FAIL=$((FAIL + 1))
  fi
  local f missing=""
  for f in scenario.yaml meta.json dizzy.log run-x.json dizzy-report.json dizzy-report.html dizzy-report.txt \
    verdict.json report.md platform/containers.tsv platform/usage.tsv platform/errors.log platform/pod-samples.log \
    platform/conditions-start.json platform/conditions-end.json platform/restarts.tsv platform/usage-summary.tsv \
    platform/conditions.tsv; do
    [[ -f "$RUN_DIR/$f" ]] || missing="$missing $f"
  done
  assert_eq "the run directory holds every file" "" "$missing"
  assert_eq "no scratch file is left" "" \
    "$(cd "$RUN_DIR" && find . -name '.*' -type f | sed 's#^\./##')"
  assert_eq "meta.json holds the run's parameters and end" "v0.3.0 mix 6h 1 openstack true 0" \
    "$(jq -r '[.dizzyVersion, .service, .duration, .sampleIntervalSeconds, (.namespaces | join(",")),
      (.endTime != null), .dizzyExitCode] | map(tostring) | join(" ")' "$RUN_DIR/meta.json")"
  assert_eq "and no threshold" "null null" "$(jq -r '"\(.maxErrorRatePercent) \(.maxP95Seconds)"' "$RUN_DIR/meta.json")"
  assert_eq "the scenario is copied into the run" "name: lab-soak" "$(cat "$RUN_DIR/scenario.yaml")"
  assert_contains "dizzy.log holds dizzy's output" "$(cat "$RUN_DIR/dizzy.log")" "$CLEAN_LEAK_LINE"
  assert_contains "and so does the pod log" "$(cat "$tmp/runner.log")" "churn finished"
  assert_contains "the pod log ends with report.md" "$(cat "$tmp/runner.log")" "Verdict: **PASS**"

  local chaos
  chaos="$(grep ' chaos ' "$DIZZY_CALLS")"
  assert_eq "dizzy runs once with the soak's arguments and nothing more" \
    "dizzy mix chaos --os-cloud dizzy-soak --scenario $tmp/scenario.yaml --duration 6h --otel" "${chaos%% |*}"
  assert_contains "with the clouds.yaml of the Secret" "$chaos" "OS_CLIENT_CONFIG_FILE=$tmp/clouds.yaml"
  assert_contains "exporting to the dizzy VictoriaMetrics" "$chaos" \
    "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://dizzy-victoria-metrics-server.dizzy.svc:8428/opentelemetry/v1/metrics"
  assert_contains "every 15 seconds" "$chaos" "OTEL_METRIC_EXPORT_INTERVAL=15000"
  assert_eq "the report renders the one record in three formats" \
    "json html table" "$(grep ' report ' "$DIZZY_CALLS" | sed 's/.*--format \([a-z]*\).*/\1/' | tr '\n' ' ' | sed 's/ $//')"
  assert_contains "with dizzy mix report --run run-x.json" "$(grep ' report ' "$DIZZY_CALLS" | head -n 1)" \
    "dizzy mix report --run run-x.json --format json"

  local containers
  containers="$(cat "$RUN_DIR/platform/containers.tsv")"
  assert_not_contains "kube-system is not sampled" "$containers" "coredns"
  assert_contains "init containers are sampled" "$containers" $'\tkeystone-db-sync\tuid-sync\t2026-01-01T00:00:00Z\tJob\twait\t0\t-\t-'
  assert_contains "a container line has the ten columns" "$containers" \
    $'\topenstack\tkeystone-0\tuid-keystone\t2026-01-01T00:00:00Z\tStatefulSet\tkeystone\t1\tError\t2026-01-02T00:00:00Z'
  assert_contains "usage lines carry millicores and bytes" "$(cat "$RUN_DIR/platform/usage.tsv")" \
    $'\topenstack\tkeystone-0\tkeystone\t1.5\t1048576'
  assert_gte "at least two pod samples were taken" "$(grep -c . "$RUN_DIR/platform/pod-samples.log")" 2
}

test_namespaces_setting() {
  echo "Test: DIZZY_SOAK_NAMESPACES limits the samples, and DIZZY_ARGS reaches dizzy split"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  run_runner "$tmp" DIZZY_SOAK_NAMESPACES="kube-system" DIZZY_ARGS="--set resources.servers=10"
  assert_contains "a listed namespace is sampled, kube-system too" "$(cat "$RUN_DIR/platform/containers.tsv")" "coredns"
  assert_not_contains "and another is not" "$(cat "$RUN_DIR/platform/containers.tsv")" "keystone-0"
  assert_eq "meta.json names the listed namespaces" "kube-system" "$(jq -r '.namespaces | join(",")' "$RUN_DIR/meta.json")"
  assert_eq "DIZZY_ARGS adds two arguments" \
    "dizzy mix chaos --os-cloud dizzy-soak --scenario $tmp/scenario.yaml --duration 6h --otel --set resources.servers=10" \
    "$(grep ' chaos ' "$DIZZY_CALLS" | sed 's/ |.*//')"
}

# ---------------------------------------------------------------------------
# Test 4: a TERM while dizzy runs
# ---------------------------------------------------------------------------
test_term_is_forwarded_once() {
  echo "Test: a TERM sent twice reaches dizzy once, and the runner waits for it and reports"

  local tmp pid rc waited=0
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  (
    in_runner "$tmp" FAKE_DIZZY_SECONDS=60
    main
  ) >"$tmp/runner.log" 2>&1 &
  pid=$!
  while ! grep -q ' chaos ' "$DIZZY_CALLS" 2>/dev/null && ((waited < 100)); do
    sleep 0.1
    waited=$((waited + 1))
  done
  sleep 0.3
  kill -TERM "$pid"
  sleep 0.3
  kill -TERM "$pid"
  wait "$pid"
  rc=$?
  RUN_DIR="$(find "$tmp/reports" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  assert_eq "dizzy received TERM once" "TERM" "$(cat "$DIZZY_SIGNALS")"
  assert_contains "the runner waited for dizzy's teardown" "$(cat "$RUN_DIR/dizzy.log" 2>/dev/null)" "$CLEAN_LEAK_LINE"
  assert_eq "dizzy's exit code is its own" "0" "$(jq -r .dizzyExitCode "$RUN_DIR/meta.json" 2>/dev/null)"
  assert_eq "the report is written" "true" "$([[ -s "$RUN_DIR/report.md" && -s "$RUN_DIR/dizzy-report.txt" ]] && echo true || echo false)"
  assert_eq "the runner exits by verdict (PASS)" "0 PASS" "$rc $(jq -r .verdict "$RUN_DIR/verdict.json" 2>/dev/null)"
  assert_contains "the stop is logged" "$(cat "$tmp/runner.log")" "Stopping dizzy (pid "
}

test_term_before_dizzy() {
  echo "Test: a TERM before dizzy starts skips dizzy, and the report names the early stop"

  local tmp pid rc waited=0
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  # The namespace read of write_meta blocks until the TERM is sent; bash
  # runs the trap once that read returns, before dizzy would start.
  (
    in_runner "$tmp" KUBECTL_BLOCK_NAMESPACES=1 KUBECTL_RELEASE="$tmp/release"
    main
  ) >"$tmp/runner.log" 2>&1 &
  pid=$!
  while ! grep -q '^kubectl get namespaces' "$CALL_LOG" 2>/dev/null && ((waited < 100)); do
    sleep 0.1
    waited=$((waited + 1))
  done
  kill -TERM "$pid"
  touch "$tmp/release"
  wait "$pid"
  rc=$?
  RUN_DIR="$(find "$tmp/reports" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  assert_eq "dizzy never runs" "" "$(cat "$DIZZY_CALLS")"
  assert_eq "dizzy-exit names the early stop" "false dizzy was not started: the soak was stopped before it began" \
    "$(check_of dizzy-exit)"
  assert_eq "meta.json has a null exit code" "null" "$(jq -r .dizzyExitCode "$RUN_DIR/meta.json" 2>/dev/null)"
  assert_eq "the report is still written" "true" "$([[ -s "$RUN_DIR/report.md" ]] && echo true || echo false)"
  assert_eq "and the runner exits 1" "1 FAIL" "$rc $(jq -r .verdict "$RUN_DIR/verdict.json" 2>/dev/null)"
  assert_contains "the stop is logged" "$(cat "$tmp/runner.log")" "The soak was stopped before dizzy started."
}

# ---------------------------------------------------------------------------
# Test 5: the checks that fail, and the ones that pass on little
# ---------------------------------------------------------------------------
test_dizzy_exit_check() {
  echo "Test: dizzy-exit fails on a non-zero exit and on a missing run record"

  local tmp rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_runner "$tmp" FAKE_DIZZY_RC=3
  rc=$?
  assert_eq "dizzy exiting 3 fails dizzy-exit with the code" "false dizzy exited 3; 1 run record(s)" "$(check_of dizzy-exit)"
  assert_eq "the verdict is FAIL" "FAIL" "$(jq -r .verdict "$RUN_DIR/verdict.json")"
  assert_eq "the report is still written" "true" "$([[ -s "$RUN_DIR/report.md" ]] && echo true || echo false)"
  assert_eq "the runner exits 1" "1" "$rc"

  new_case "$tmp"
  run_runner "$tmp" FAKE_DIZZY_RECORD=0
  rc=$?
  assert_eq "no record fails dizzy-exit" "false dizzy exited 0 and wrote no run record" "$(check_of dizzy-exit)"
  assert_eq "and no dizzy-report.* is rendered" "" "$(cd "$RUN_DIR" && find . -name 'dizzy-report*')"
  assert_eq "dizzy report is never called" "" "$(grep ' report ' "$DIZZY_CALLS" || true)"
  assert_contains "report.md says so" "$(cat "$RUN_DIR/report.md")" "No dizzy report"
  assert_eq "the runner exits 1" "1" "$rc"

  new_case "$tmp"
  run_runner "$tmp" FAKE_DIZZY_RECORD=2
  rc=$?
  assert_eq "two records fail dizzy-exit" "false dizzy exited 0 and wrote 2 run records" "$(check_of dizzy-exit)"
  assert_eq "dizzy report is not called" "" "$(grep ' report ' "$DIZZY_CALLS" || true)"
  assert_contains "the runner says why" "$(cat "$tmp/runner.log")" "No single run record to report on"
  assert_eq "the runner exits 1" "1" "$rc"
}

test_leak_check() {
  echo "Test: leak-check fails on resources left and on a missing leak line"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_runner "$tmp" FAKE_DIZZY_LEAK="leak check: 2 run-tagged resource(s) still present after teardown"
  assert_eq "resources left fail with the line" \
    "false leak check: 2 run-tagged resource(s) still present after teardown" "$(check_of leak-check)"
  assert_eq "although dizzy exited 0" "true" "$(check_of dizzy-exit | cut -d' ' -f1)"

  new_case "$tmp"
  run_runner "$tmp" FAKE_DIZZY_LEAK=none
  assert_eq "no leak line fails" "false leak check line not found" "$(check_of leak-check)"
  assert_contains "report.md says so" "$(cat "$RUN_DIR/report.md")" "No leak check line in dizzy.log."

  # The clean lines of glance and keystone, and the line of images left.
  local line
  for line in "leak check: no run-tagged images remain" "leak check: no run-named resources remain"; do
    new_case "$tmp"
    run_runner "$tmp" FAKE_DIZZY_LEAK="$line"
    assert_eq "'$line' passes" "true $line" "$(check_of leak-check)"
  done
  new_case "$tmp"
  run_runner "$tmp" FAKE_DIZZY_LEAK="leak check: 1 run-tagged image(s) still present after teardown"
  assert_eq "images left fail with the line" \
    "false leak check: 1 run-tagged image(s) still present after teardown" "$(check_of leak-check)"
}

test_controlplane_check() {
  echo "Test: controlplane-ready fails on Ready=False, on no ControlPlane and on a Forbidden read"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_runner "$tmp" KUBECTL_CP_END=notready
  assert_eq "Ready=False at the end fails" "false openstack/controlplane Ready=False" "$(check_of controlplane-ready)"
  assert_contains "conditions.tsv shows True at the start and False at the end" \
    "$(cat "$RUN_DIR/platform/conditions.tsv")" $'controlplanes.c5c3.io\topenstack\tcontrolplane\tTrue\tFalse'

  new_case "$tmp"
  run_runner "$tmp" KUBECTL_CP_END=none
  assert_eq "a ControlPlane gone at the end fails" "false no ControlPlane found" "$(check_of controlplane-ready)"
  assert_contains "conditions.tsv shows it absent at the end" \
    "$(cat "$RUN_DIR/platform/conditions.tsv")" $'controlplanes.c5c3.io\topenstack\tcontrolplane\tTrue\tabsent\t-'

  new_case "$tmp"
  run_runner "$tmp" KUBECTL_CP=none
  assert_eq "no ControlPlane fails" "false no ControlPlane found" "$(check_of controlplane-ready)"

  new_case "$tmp"
  run_runner "$tmp" KUBECTL_CP=forbidden
  assert_eq "a Forbidden read fails with kubectl's message" \
    "false Error from server (Forbidden): controlplanes.c5c3.io is forbidden: User \"system:serviceaccount:dizzy:dizzy-soak\" cannot list resource \"controlplanes\" in API group \"c5c3.io\" at the cluster scope" \
    "$(check_of controlplane-ready)"
  assert_eq "the verdict is FAIL" "FAIL" "$(jq -r .verdict "$RUN_DIR/verdict.json")"
}

test_restart_checks() {
  echo "Test: no-restarts fails on a restart, passes on no container and fails when no pod read succeeds"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  sed 's/"restartCount":1,/"restartCount":3,/' "$tmp/bin/pods.json" >"$tmp/pods-later.json"
  run_runner "$tmp" KUBECTL_PODS_LATER="$tmp/pods-later.json"
  assert_contains "restarts.tsv shows the 2 restarts" "$(cat "$RUN_DIR/platform/restarts.tsv")" \
    $'openstack\tkeystone-0\tkeystone\tuid-keystone\tStatefulSet\t2\t0'
  assert_eq "no-restarts fails" "false 2 restart(s) and 0 OOM kill(s) in 3 containers observed" "$(check_of no-restarts)"
  assert_contains "report.md lists the container" "$(cat "$RUN_DIR/report.md")" "| openstack | keystone-0 | keystone |"

  new_case "$tmp"
  echo '{"items":[]}' >"$tmp/empty.json"
  run_runner "$tmp" KUBECTL_PODS_FIRST="$tmp/empty.json"
  assert_eq "zero containers pass" "true 0 containers observed" "$(check_of no-restarts)"
  assert_eq "the verdict is PASS" "PASS" "$(jq -r .verdict "$RUN_DIR/verdict.json")"

  new_case "$tmp"
  run_runner "$tmp" KUBECTL_PODS_RC=1
  local last
  last="$(tail -n 1 "$RUN_DIR/platform/errors.log")"
  assert_contains "the last line of errors.log is the failed pod read" "$last" " pods: Error from server (Forbidden): pods is forbidden"
  assert_eq "no-restarts fails with that line" "false pod state could not be read: $last" "$(check_of no-restarts)"
  assert_eq "containers.tsv stays empty" "" "$(cat "$RUN_DIR/platform/containers.tsv")"
  assert_contains "report.md counts the errors" "$(cat "$RUN_DIR/report.md")" "platform/errors.log holds "
}

test_metrics_unavailable() {
  echo "Test: without the metrics API usage.tsv stays empty and the verdict holds"

  local tmp rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  run_runner "$tmp" KUBECTL_METRICS_RC=1
  rc=$?
  assert_eq "usage.tsv stays empty" "" "$(cat "$RUN_DIR/platform/usage.tsv")"
  assert_contains "report.md says Resource usage: unavailable" "$(cat "$RUN_DIR/report.md")" "Resource usage: unavailable"
  assert_contains "the failed reads are logged" "$(cat "$RUN_DIR/platform/errors.log")" \
    "usage: Error from server (NotFound): the server could not find the requested resource"
  assert_eq "the verdict does not change" "0 PASS" "$rc $(jq -r .verdict "$RUN_DIR/verdict.json")"
}

# ---------------------------------------------------------------------------
# Test 6: the thresholds
# ---------------------------------------------------------------------------
test_thresholds() {
  echo "Test: error-rate and p95 run only when set and decide against their setting"

  local tmp rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_runner "$tmp"
  assert_eq "without DIZZY_SOAK_MAX_ERROR_RATE error-rate is absent" "absent" "$(check_of error-rate)"
  assert_eq "without DIZZY_SOAK_MAX_P95_SECONDS p95 is absent" "absent" "$(check_of p95)"

  new_case "$tmp"
  run_runner "$tmp" DIZZY_SOAK_MAX_ERROR_RATE=5 \
    FAKE_REPORT_JSON='{"metrics":{"overall":{"attempted":100,"failed":4,"latency":{"p95":1}}}}'
  rc=$?
  assert_eq "4 of 100 failed passes 5%" "true 4 of 100 operations failed (4.00%), within the maximum of 5%" "$(check_of error-rate)"
  assert_eq "meta.json holds the threshold" "5" "$(jq -r .maxErrorRatePercent "$RUN_DIR/meta.json")"
  assert_eq "and the run passes" "0" "$rc"

  new_case "$tmp"
  run_runner "$tmp" DIZZY_SOAK_MAX_ERROR_RATE=5 \
    FAKE_REPORT_JSON='{"metrics":{"overall":{"attempted":100,"failed":6,"latency":{"p95":1}}}}'
  rc=$?
  assert_eq "6 of 100 failed fails 5%" "false 6 of 100 operations failed (6.00%), above the maximum of 5%" "$(check_of error-rate)"
  assert_eq "and the run fails" "1 FAIL" "$rc $(jq -r .verdict "$RUN_DIR/verdict.json")"

  new_case "$tmp"
  run_runner "$tmp" DIZZY_SOAK_MAX_ERROR_RATE=5 \
    FAKE_REPORT_JSON='{"metrics":{"overall":{"attempted":0,"failed":0,"latency":{"p95":0}}}}'
  assert_eq "0 attempted fails" "false no operations attempted" "$(check_of error-rate)"

  new_case "$tmp"
  run_runner "$tmp" DIZZY_SOAK_MAX_ERROR_RATE=5 FAKE_DIZZY_RECORD=0
  assert_eq "no dizzy report fails" "false no dizzy report to read the error rate from" "$(check_of error-rate)"

  new_case "$tmp"
  run_runner "$tmp" DIZZY_SOAK_MAX_ERROR_RATE=5 FAKE_REPORT_FAIL=json
  assert_eq "a json report that fails to render leaves no file, the others render" \
    "dizzy-report.html dizzy-report.txt" \
    "$(cd "$RUN_DIR" && find . -name 'dizzy-report*' | sed 's#^\./##' | sort | tr '\n' ' ' | sed 's/ $//')"
  assert_contains "the failure is logged with dizzy's message" "$(cat "$tmp/runner.log")" \
    "WARNING: dizzy mix report --format json failed: boom"
  assert_eq "and error-rate fails for want of the report" "false no dizzy report to read the error rate from" \
    "$(check_of error-rate)"

  new_case "$tmp"
  run_runner "$tmp" DIZZY_SOAK_MAX_P95_SECONDS=2 \
    FAKE_REPORT_JSON='{"metrics":{"overall":{"attempted":10,"failed":0,"latency":{"p95":1500000000}}}}'
  assert_eq "a p95 of 1.5s passes 2s" "true p95 1.500s, within the maximum of 2s" "$(check_of p95)"

  new_case "$tmp"
  run_runner "$tmp" DIZZY_SOAK_MAX_P95_SECONDS=2 \
    FAKE_REPORT_JSON='{"metrics":{"overall":{"attempted":10,"failed":0,"latency":{"p95":2500000000}}}}'
  rc=$?
  assert_eq "a p95 of 2.5s fails 2s" "false p95 2.500s, above the maximum of 2s" "$(check_of p95)"
  assert_eq "and the run fails" "1" "$rc"
}

# ---------------------------------------------------------------------------
# Test 7: the claim is full
# ---------------------------------------------------------------------------
test_free_space() {
  echo "Test: with less than 512 MiB free the runner exits 1 before dizzy starts"

  local tmp rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  run_runner "$tmp" DF_FREE_KIB=524287
  rc=$?
  assert_eq "it exits 1" "1" "$rc"
  assert_contains "naming the claim" "$(cat "$tmp/runner.log")" "the claim dizzy-soak-reports has 511 MiB free"
  assert_eq "dizzy never runs" "" "$(cat "$DIZZY_CALLS")"
  assert_eq "no run directory is created" "" "${RUN_DIR:-}"
  assert_eq "and the cluster is not read" "" "$(cat "$CALL_LOG")"

  new_case "$tmp"
  run_runner "$tmp" DF_FREE_KIB=524288
  rc=$?
  assert_eq "512 MiB free is enough" "0" "$rc"
}

# ---------------------------------------------------------------------------
# Test 8: report.md and the OTLP endpoint
# ---------------------------------------------------------------------------
test_report_md() {
  echo "Test: report.md holds every part of the report"

  local tmp report
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  # A pod created during the run, beside the Job's pod that does not count.
  jq '.items += [{"metadata":{"namespace":"openstack","name":"nova-api-1","uid":"uid-new","creationTimestamp":"2099-01-01T00:00:00Z",
    "ownerReferences":[{"kind":"ReplicaSet"}]},"status":{"containerStatuses":[{"name":"api","restartCount":0}]}},
    {"metadata":{"namespace":"openstack","name":"nova-db-sync","uid":"uid-job","creationTimestamp":"2099-01-01T00:00:00Z",
    "ownerReferences":[{"kind":"Job"}]},"status":{"containerStatuses":[{"name":"sync","restartCount":0}]}}]' \
    "$tmp/bin/pods.json" >"$tmp/pods-new.json"
  run_runner "$tmp" KUBECTL_PODS_FIRST="$tmp/pods-new.json"
  report="$(cat "$RUN_DIR/report.md")"
  local part
  for part in \
    "# dizzy soak $(basename "$RUN_DIR")" \
    'Verdict: **PASS**' \
    '| Check | Result | Detail |' \
    '| dizzy-exit | pass | dizzy exited 0 and wrote run-x.json |' \
    '## Parameters' \
    '| startTime | ' \
    '| endTime | ' \
    '| dizzyVersion | v0.3.0 |' \
    '## dizzy report' \
    'TABLE OF THE FAKE DIZZY' \
    '## Leak check' \
    "$CLEAN_LEAK_LINE" \
    '## Restarts and OOM kills' \
    'No container restarted or was OOM-killed during the run (5 containers observed).' \
    '## Pods created during the run' \
    '| openstack | 1 |' \
    'The 15 containers with the largest memory growth:' \
    'The 15 containers with the highest mean CPU:' \
    '| openstack | keystone-0 | keystone |' \
    '## Conditions' \
    '| controlplanes.c5c3.io | openstack | controlplane | True | True | - |' \
    'platform/errors.log holds 0 lines.'; do
    assert_contains "report.md holds '$part'" "$report" "$part"
  done
}

test_otlp_endpoint_is_the_grafana_datasource() {
  echo "Test: the OTLP endpoint is the VictoriaMetrics Service the Grafana datasource reads"

  local datasource endpoint
  datasource="$(sed -n 's/^[[:space:]]*url: \(http:[^[:space:]]*\)$/\1/p' "$PROJECT_ROOT/deploy/kind/dizzy/release-grafana.yaml" | head -n 1)"
  endpoint="$(sed -n 's/^OTLP_ENDPOINT="\(.*\)"$/\1/p' "$RUNNER_SH")"
  assert_not_empty "the Grafana release names a datasource URL" "$datasource"
  assert_eq "the endpoint is the datasource URL plus the OTLP path" "${datasource}/opentelemetry/v1/metrics" "$endpoint"
}

# ---------------------------------------------------------------------------
# Test 9: the leak lines of the pinned dizzy
# ---------------------------------------------------------------------------
# shellcheck disable=SC2016 # the pin line's literal text, not expanded here
test_leak_lines_of_the_pinned_dizzy() {
  echo "Test: leak-check passes on every clean leak line the pinned dizzy prints"

  local pin src lines line failing="" tmp
  pin="$(sed -n 's/^DIZZY_VERSION="${DIZZY_VERSION:-\(v[0-9.]*\)}"$/\1/p' "$PROJECT_ROOT/hack/dizzy.sh")"
  src="$PROJECT_ROOT/_output/dizzy/$pin/cmd/dizzy"
  if [[ ! -d "$src" ]]; then
    echo "  SKIP: no dizzy $pin source in _output/dizzy/$pin/; hack/dizzy.sh stage-dashboards caches it (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  lines="$(find "$src" -name '*.go' ! -name '*_test.go' -exec grep -ho '"leak check: no [^"\\]*' {} + |
    sed 's/^"//' | sort -u)"
  assert_not_empty "dizzy $pin prints a clean leak line" "$lines"

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    printf '%s\n' "$line" >"$tmp/dizzy.log"
    if [[ "$( (in_runner "$tmp" && cd "$tmp" && check_leak) | jq -r .pass)" != "true" ]]; then
      failing="$failing [$line]"
    fi
  done <<<"$lines"
  assert_eq "leak-check passes on each of them" "" "$failing"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq not installed (every check of this file skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_quantities
test_derive_restarts
test_derive_usage_summary
test_conditions
test_conditions_beyond_one_argument
test_clean_run
test_namespaces_setting
test_term_is_forwarded_once
test_term_before_dizzy
test_dizzy_exit_check
test_leak_check
test_controlplane_check
test_restart_checks
test_metrics_unavailable
test_thresholds
test_free_space
test_report_md
test_otlp_endpoint_is_the_grafana_datasource
test_leak_lines_of_the_pinned_dizzy

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
