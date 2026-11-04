#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-collect-e2e-coverage.sh with a stubbed PATH (kind, docker,
# kubectl, go):
#   - the counters of every node are merged into one profile per operator and
#     named in GITHUB_OUTPUT with their Codecov flags;
#   - the summary row counts the operator's own statements alone, under a
#     German locale too;
#   - a node without the directory, an operator without counters, a failed
#     merge, an empty profile and a directory that is no operator name are
#     skipped, and the script still exits 0;
#   - the final flush scales only the Deployments whose manager sets
#     GOCOVERDIR and waits on their selector, warns on a timeout and is off
#     under SCALE_DOWN=false;
#   - a wrong family and a missing GITHUB_OUTPUT exit 1.
#
# Usage: bash tests/unit/hack/ci_collect_e2e_coverage_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
COLLECT_SH="$PROJECT_ROOT/hack/ci-collect-e2e-coverage.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
NODE_DIR="/var/lib/cobaltcore-coverage"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs
# kind prints $KIND_NODES, one node per line. docker cp <node>:<dir>/. <dest>
# copies $NODE_ROOT/<node><dir> into <dest> and fails when it is missing.
# kubectl logs its argv, prints $DEPLOYMENTS_JSON for `get deployments` (fails
# under KUBECTL_GET_FAIL=1) and fails `wait` with a timeout under
# KUBECTL_WAIT_FAIL=timeout or with "no matching resources found" under
# KUBECTL_WAIT_FAIL=gone. go logs its argv and writes $CANNED/<op>.out, or a
# one-block profile of operators/<op>/, to its -o file, <op> being the base
# name of the first -i directory; it fails for $GO_FAIL_OP.
make_stubs() {
  mkdir -p "$STUB_DIR"

  cat >"$STUB_DIR/kind" <<'STUB'
#!/bin/bash
if [ -n "${KIND_NODES:-}" ]; then
  printf '%s\n' $KIND_NODES
fi
exit 0
STUB

  cat >"$STUB_DIR/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_LOG"
[ "${1:-}" = "cp" ] || exit 1
node="${2%%:*}"
src="${2#*:}"
src="${src%/.}"
[ -d "$NODE_ROOT/$node$src" ] || { echo "Error: Could not find the file $src in container $node" >&2; exit 1; }
cp -R "$NODE_ROOT/$node$src/." "$3"
STUB

  cat >"$STUB_DIR/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"$KUBECTL_LOG"
case "${1:-}" in
  get)
    [ "${KUBECTL_GET_FAIL:-}" = "1" ] && { echo "error: connection refused" >&2; exit 1; }
    printf '%s\n' "${DEPLOYMENTS_JSON:-{\"items\": []\}}"
    ;;
  wait)
    case "${KUBECTL_WAIT_FAIL:-}" in
      timeout) echo "error: timed out waiting for the condition on pods/keystone-operator-abc" >&2; exit 1 ;;
      gone) echo "error: no matching resources found" >&2; exit 1 ;;
    esac
    ;;
esac
exit 0
STUB

  cat >"$STUB_DIR/go" <<'STUB'
#!/bin/bash
echo "go $*" >>"$GO_LOG"
in="" out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -i=*) in="${1#-i=}" ;;
    -o) out="$2"; shift ;;
  esac
  shift
done
first="${in%%,*}"
op="${first##*/}"
if [ "$op" = "${GO_FAIL_OP:-}" ]; then
  echo "covdata: cannot read counter data" >&2
  exit 1
fi
if [ -f "$CANNED/$op.out" ]; then
  cp "$CANNED/$op.out" "$out"
else
  printf 'mode: atomic\ngithub.com/c5c3/cobaltcore/operators/%s/main.go:1.1,2.2 1 1\n' "$op" >"$out"
fi
STUB
  chmod +x "$STUB_DIR/kind" "$STUB_DIR/docker" "$STUB_DIR/kubectl" "$STUB_DIR/go"
}

# new_case — a fresh work directory with empty logs, node root and canned
# profiles; sets WORK.
new_case() {
  WORK="$(mktemp -d "$TMP_DIR/case.XXXXXX")"
  mkdir -p "$WORK/nodes" "$WORK/canned" "$WORK/run"
  : >"$WORK/docker.log"
  : >"$WORK/kubectl.log"
  : >"$WORK/go.log"
}

# node_counters <node> <op> [meta-only] — the files a manager leaves in its
# node directory: a covmeta file and, unless meta-only, a covcounters file.
node_counters() {
  local dir="$WORK/nodes/$1$NODE_DIR/$2"
  mkdir -p "$dir"
  : >"$dir/covmeta.0123abcd"
  if [ -z "${3:-}" ]; then
    : >"$dir/covcounters.0123abcd.1.1792669149"
  fi
}

# run_collect [VAR=value ...] -- <args...>
# Runs the collector in $WORK/run with the stubs first on PATH. Sets OUTPUT,
# RC, OUT (the GITHUB_OUTPUT lines) and SUMMARY.
run_collect() {
  local -a assignments=()
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do
    assignments+=("$1")
    shift
  done
  shift
  RC=0
  : >"$WORK/output"
  : >"$WORK/summary"
  OUTPUT="$(
    cd "$WORK/run" &&
      env PATH="$STUB_DIR:$PATH" \
        NODE_ROOT="$WORK/nodes" CANNED="$WORK/canned" \
        DOCKER_LOG="$WORK/docker.log" KUBECTL_LOG="$WORK/kubectl.log" GO_LOG="$WORK/go.log" \
        GITHUB_OUTPUT="$WORK/output" GITHUB_STEP_SUMMARY="$WORK/summary" \
        ${assignments[@]+"${assignments[@]}"} bash "$COLLECT_SH" "$@" 2>&1
  )" || RC=$?
  OUT="$(cat "$WORK/output")"
  SUMMARY="$(cat "$WORK/summary")"
}

# output_value <name> — the value of that GITHUB_OUTPUT line.
output_value() {
  sed -n "s/^$1=//p" <<<"$OUT"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_merges_two_operators() {
  echo "Test: the counters of keystone and glance become one profile each, named with their flags"
  new_case
  node_counters cobaltcore-control-plane keystone
  node_counters cobaltcore-control-plane glance
  node_counters cobaltcore-worker keystone

  run_collect SCALE_DOWN=false KIND_NODES="cobaltcore-control-plane cobaltcore-worker" -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_eq "found is true" "true" "$(output_value found)"
  assert_eq "files names both profiles, sorted by operator" \
    "_output/coverage/e2e-glance.out,_output/coverage/e2e-keystone.out" "$(output_value files)"
  assert_eq "flags names both operators, then common" \
    "e2e-glance,e2e-keystone,e2e-common" "$(output_value flags)"
  assert_eq "the glance profile is written" "true" \
    "$([ -s "$WORK/run/_output/coverage/e2e-glance.out" ] && echo true || echo false)"
  assert_eq "the keystone profile is written" "true" \
    "$([ -s "$WORK/run/_output/coverage/e2e-keystone.out" ] && echo true || echo false)"
  assert_file_contains_fixed "keystone merges the directories of both nodes" "$WORK/go.log" \
    "go tool covdata textfmt -i=_output/coverage/raw/cobaltcore-control-plane/keystone,_output/coverage/raw/cobaltcore-worker/keystone -o _output/coverage/e2e-keystone.out"
  assert_file_contains_fixed "each node's directory is copied" "$WORK/docker.log" \
    "docker cp cobaltcore-worker:/var/lib/cobaltcore-coverage/. _output/coverage/raw/cobaltcore-worker/"
  assert_contains "the summary has its heading" "$SUMMARY" "### Operator coverage (e2e)"
  assert_contains "the summary has the table header" "$SUMMARY" \
    "| Operator | Statements | Covered | Coverage |"
  assert_contains "the summary has a glance row" "$SUMMARY" "| glance | 1 | 1 | 100.0% |"
}

test_summary_row() {
  echo "Test: the summary row counts the operator's own statements, also under a German locale"
  new_case
  node_counters cobaltcore-control-plane keystone
  cat >"$WORK/canned/keystone.out" <<'EOF'
mode: atomic
github.com/c5c3/cobaltcore/operators/keystone/main.go:10.2,12.3 1 4
github.com/c5c3/cobaltcore/operators/keystone/internal/controller/reconcile.go:20.2,24.3 2 1
github.com/c5c3/cobaltcore/operators/keystone/internal/controller/reconcile.go:30.2,31.3 1 0
github.com/c5c3/cobaltcore/internal/common/bootstrap/manager.go:40.2,48.3 5 2
github.com/c5c3/cobaltcore/internal/common/apply/apply.go:50.2,50.30 3 0
EOF

  local locale
  for locale in C de_DE.UTF-8; do
    run_collect SCALE_DOWN=false KIND_NODES=cobaltcore-control-plane LC_ALL="$locale" -- chaos
    assert_eq "LC_ALL=$locale: the collector exits 0" "0" "$RC"
    assert_contains "LC_ALL=$locale: the keystone row counts 4 statements, 3 covered" "$SUMMARY" \
      "| keystone | 4 | 3 | 75.0% |"
    assert_contains "LC_ALL=$locale: the heading names the family" "$SUMMARY" \
      "### Operator coverage (chaos)"
  done
  assert_eq "the flags carry the chaos family" "chaos-keystone,chaos-common" "$(output_value flags)"
}

test_meta_only_dir_is_skipped() {
  echo "Test: an operator directory with a covmeta file alone is skipped"
  new_case
  node_counters cobaltcore-control-plane keystone meta-only
  node_counters cobaltcore-control-plane glance

  run_collect SCALE_DOWN=false KIND_NODES=cobaltcore-control-plane -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_contains "it says keystone has no counters" "$OUTPUT" "::notice::keystone: no coverage counters"
  assert_eq "files names glance alone" "_output/coverage/e2e-glance.out" "$(output_value files)"
  assert_eq "flags names glance alone, then common" "e2e-glance,e2e-common" "$(output_value flags)"
  assert_file_not_contains "keystone is never merged" "$WORK/go.log" "keystone"
}

test_no_nodes() {
  echo "Test: a cluster without nodes finds nothing, says so and exits 0"
  new_case
  mkdir -p "$WORK/run/_output/coverage"
  echo "mode: atomic" >"$WORK/run/_output/coverage/tempest-stale.out"

  run_collect SCALE_DOWN=false KIND_NODES="" -- tempest

  assert_eq "the collector exits 0" "0" "$RC"
  assert_eq "found is false" "false" "$(output_value found)"
  assert_eq "files is empty" "" "$(output_value files)"
  assert_eq "flags is empty" "" "$(output_value flags)"
  assert_contains "the summary says nothing was collected" "$SUMMARY" \
    "No operator coverage counters were collected."
  assert_eq "a profile an earlier run left behind is removed" "false" \
    "$([ -e "$WORK/run/_output/coverage/tempest-stale.out" ] && echo true || echo false)"
}

test_node_without_dir() {
  echo "Test: a node without the coverage directory is skipped, the others are merged"
  new_case
  mkdir -p "$WORK/nodes/cobaltcore-control-plane"
  node_counters cobaltcore-worker keystone

  run_collect SCALE_DOWN=false KIND_NODES="cobaltcore-control-plane cobaltcore-worker" -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_contains "it names the node" "$OUTPUT" \
    "::notice::cobaltcore-control-plane has no /var/lib/cobaltcore-coverage; skipped"
  assert_eq "the other node's counters are merged" "_output/coverage/e2e-keystone.out" \
    "$(output_value files)"
  assert_file_contains_fixed "the merge reads the worker's directory alone" "$WORK/go.log" \
    "-i=_output/coverage/raw/cobaltcore-worker/keystone -o"
}

test_covdata_failure() {
  echo "Test: a failed merge drops that operator and keeps the others"
  new_case
  node_counters cobaltcore-control-plane keystone
  node_counters cobaltcore-control-plane glance

  run_collect SCALE_DOWN=false KIND_NODES=cobaltcore-control-plane GO_FAIL_OP=glance -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_contains "it warns about glance" "$OUTPUT" "::warning::glance: go tool covdata failed"
  assert_eq "files names keystone alone" "_output/coverage/e2e-keystone.out" "$(output_value files)"
  assert_eq "flags names keystone alone, then common" "e2e-keystone,e2e-common" "$(output_value flags)"
  assert_eq "no glance profile is left" "false" \
    "$([ -e "$WORK/run/_output/coverage/e2e-glance.out" ] && echo true || echo false)"
}

test_mode_only_profile_dropped() {
  echo "Test: a profile holding only its mode line is dropped"
  new_case
  node_counters cobaltcore-control-plane keystone
  echo "mode: atomic" >"$WORK/canned/keystone.out"

  run_collect SCALE_DOWN=false KIND_NODES=cobaltcore-control-plane -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_eq "found is false" "false" "$(output_value found)"
  assert_eq "files is empty" "" "$(output_value files)"
  assert_eq "the empty profile is removed" "false" \
    "$([ -e "$WORK/run/_output/coverage/e2e-keystone.out" ] && echo true || echo false)"
  assert_contains "the summary says nothing was collected" "$SUMMARY" \
    "No operator coverage counters were collected."
}

test_bad_directory_name() {
  echo "Test: a directory that is no operator name is skipped with a warning"
  new_case
  node_counters cobaltcore-control-plane Bad_Name
  node_counters cobaltcore-control-plane keystone

  run_collect SCALE_DOWN=false KIND_NODES=cobaltcore-control-plane -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_contains "it warns about the name" "$OUTPUT" "::warning::skipping the coverage directory 'Bad_Name'"
  assert_file_not_contains "Bad_Name is never merged" "$WORK/go.log" "Bad_Name"
  assert_eq "flags names keystone alone, then common" "e2e-keystone,e2e-common" "$(output_value flags)"
}

# A Deployment list with one instrumented manager, a manager without
# GOCOVERDIR (a suite's own metrics release) and GOCOVERDIR on a container that
# is not the manager.
DEPLOYMENTS_FIXTURE='{"items": [
  {"metadata": {"namespace": "keystone-system", "name": "keystone-operator"},
   "spec": {"selector": {"matchLabels": {"app.kubernetes.io/name": "keystone-operator", "app.kubernetes.io/instance": "keystone-operator"}},
            "template": {"spec": {"containers": [{"name": "manager", "env": [{"name": "GOCOVERDIR", "value": "/coverage"}]}]}}}},
  {"metadata": {"namespace": "keystone-system", "name": "keystone-operator-metrics"},
   "spec": {"selector": {"matchLabels": {"app.kubernetes.io/name": "keystone-operator-metrics"}},
            "template": {"spec": {"containers": [{"name": "manager"}]}}}},
  {"metadata": {"namespace": "other", "name": "other"},
   "spec": {"selector": {"matchLabels": {"app": "other"}},
            "template": {"spec": {"containers": [{"name": "sidecar", "env": [{"name": "GOCOVERDIR", "value": "/coverage"}]}]}}}}
]}'

test_scale_down_selects_instrumented_managers() {
  echo "Test: the default final flush scales only the instrumented managers and waits on their selector"
  new_case
  node_counters cobaltcore-control-plane keystone

  run_collect KIND_NODES=cobaltcore-control-plane DEPLOYMENTS_JSON="$DEPLOYMENTS_FIXTURE" -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_eq "kubectl lists, scales the keystone manager and waits on its pods" \
    "kubectl get deployments --all-namespaces -o json
kubectl scale deployment keystone-operator -n keystone-system --replicas=0
kubectl wait pod -n keystone-system -l app.kubernetes.io/name=keystone-operator,app.kubernetes.io/instance=keystone-operator --for=delete --timeout=60s" \
    "$(cat "$WORK/kubectl.log")"
  assert_not_contains "no warning is printed" "$OUTPUT" "::warning::"
  assert_eq "the profile is still produced" "e2e-keystone,e2e-common" "$(output_value flags)"

  run_collect KIND_NODES=cobaltcore-control-plane DEPLOYMENTS_JSON="$DEPLOYMENTS_FIXTURE" \
    KUBECTL_WAIT_FAIL=gone -- e2e
  assert_not_contains "pods already gone are not a warning" "$OUTPUT" "::warning::"
}

test_no_scale_down_when_disabled() {
  echo "Test: SCALE_DOWN=false never scales a Deployment"
  new_case
  node_counters cobaltcore-control-plane keystone

  run_collect SCALE_DOWN=false KIND_NODES=cobaltcore-control-plane DEPLOYMENTS_JSON="$DEPLOYMENTS_FIXTURE" -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_eq "kubectl is never called" "" "$(cat "$WORK/kubectl.log")"
  assert_eq "the counters are still collected" "e2e-keystone,e2e-common" "$(output_value flags)"
}

test_wait_timeout_warns_and_copies() {
  echo "Test: a wait that times out warns and the counters are still copied"
  new_case
  node_counters cobaltcore-control-plane keystone

  run_collect KIND_NODES=cobaltcore-control-plane DEPLOYMENTS_JSON="$DEPLOYMENTS_FIXTURE" \
    KUBECTL_WAIT_FAIL=timeout -- e2e

  assert_eq "the collector exits 0" "0" "$RC"
  assert_contains "it warns, naming the Deployment" "$OUTPUT" \
    "::warning::the pods of keystone-system/keystone-operator are not gone after 60s"
  assert_file_contains_fixed "the copy still runs" "$WORK/docker.log" \
    "docker cp cobaltcore-control-plane:/var/lib/cobaltcore-coverage/."
  assert_eq "the profile is still produced" "e2e-keystone,e2e-common" "$(output_value flags)"

  run_collect KIND_NODES=cobaltcore-control-plane KUBECTL_GET_FAIL=1 -- e2e
  assert_eq "a failed listing exits 0" "0" "$RC"
  assert_contains "a failed listing warns" "$OUTPUT" \
    "::warning::cannot list the Deployments; the managers get no final flush"
  assert_eq "a failed listing still collects" "e2e-keystone,e2e-common" "$(output_value flags)"
}

test_usage_errors() {
  echo "Test: a wrong family and a missing GITHUB_OUTPUT exit 1"
  new_case

  run_collect SCALE_DOWN=false KIND_NODES=cobaltcore-control-plane -- unit
  assert_eq "family unit exits 1" "1" "$RC"
  assert_contains "it names the family" "$OUTPUT" "::error::family must be e2e, chaos or tempest, got 'unit'"

  run_collect SCALE_DOWN=false KIND_NODES=cobaltcore-control-plane --
  assert_eq "no family exits 1" "1" "$RC"
  assert_contains "it names the empty family" "$OUTPUT" "::error::family must be e2e, chaos or tempest, got ''"

  local rc=0 output
  output="$(cd "$WORK/run" && env -u GITHUB_OUTPUT PATH="$STUB_DIR:$PATH" bash "$COLLECT_SH" e2e 2>&1)" || rc=$?
  assert_eq "a missing GITHUB_OUTPUT exits 1" "1" "$rc"
  assert_contains "it says GITHUB_OUTPUT is required" "$output" "::error::GITHUB_OUTPUT is required"
  assert_eq "docker is never called" "" "$(cat "$WORK/docker.log")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
make_stubs

test_merges_two_operators
test_summary_row
test_meta_only_dir_is_skipped
test_no_nodes
test_node_without_dir
test_covdata_failure
test_mode_only_profile_dropped
test_bad_directory_name
test_no_scale_down_when_disabled
test_usage_errors

# The final flush reads the Deployment list with jq.
if command -v jq >/dev/null 2>&1; then
  test_scale_down_selects_instrumented_managers
  test_wait_timeout_warns_and_copies
else
  echo "SKIP: jq not installed (the final-flush checks are skipped)"
  SKIP=$((SKIP + 1))
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
