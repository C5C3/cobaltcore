#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-check-node-budget.sh with a stubbed kubectl:
#   - a pod list within budget exits 0 and prints the table, the totals and
#     the budget; a CPU or memory total one unit over exits 1 with one
#     `over budget` line per exceeded resource;
#   - an empty pod list and a pod without requests count as 0;
#   - the effective request follows the scheduler's rule: the largest regular
#     init container wins over a smaller app sum, a restartPolicy: Always init
#     container adds to the app sum and to every regular init container
#     declared after it, and spec.overhead adds on top;
#   - the quantity parser reads plain numbers, decimals, exponents and every
#     decimal and binary suffix, and exits 2 on anything else;
#   - two nodes, a failing node listing and a failing pod listing exit 2 with
#     their message;
#   - NODE_BUDGET_SELECTOR reaches kubectl as one -l argument, and no -l is
#     passed when it is empty.
#
# Usage: bash tests/unit/hack/ci_check_node_budget_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
BUDGET_SH="$PROJECT_ROOT/hack/ci-check-node-budget.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The script does its arithmetic in jq. The stub PATH below is tightened to
# the stub dir plus /usr/bin and /bin, so a jq installed elsewhere (Homebrew,
# ~/.local) is linked into the stub dir.
JQ_BIN="$(command -v jq 2>/dev/null || true)"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stub <dir>
# kubectl logs every argument in brackets, one invocation per line, to
# $KUBECTL_LOG, so a test can tell one argument from two. `get nodes` prints
# $STUB_NODES (default: one node), or fails with a message on stderr when
# $STUB_NODES_FAIL is set. `get pods` prints $STUB_PODS_JSON, or fails with a
# message on stderr when $STUB_PODS_FAIL is set.
make_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
printf '[%s]' "$@" >>"$KUBECTL_LOG"
echo >>"$KUBECTL_LOG"
case "$1 $2" in
  "get nodes")
    if [ -n "${STUB_NODES_FAIL:-}" ]; then
      echo "error: stub apiserver unreachable" >&2
      exit 1
    fi
    printf '%s\n' "${STUB_NODES-node/stub-node}"
    exit 0
    ;;
  "get pods")
    if [ -n "${STUB_PODS_FAIL:-}" ]; then
      echo "error: stub apiserver down" >&2
      exit 1
    fi
    printf '%s\n' "$STUB_PODS_JSON"
    exit 0
    ;;
esac
echo "[kubectl-stub] unexpected invocation: $*" >&2
exit 64
STUB
  chmod +x "$dir/kubectl"
  ln -sf "$JQ_BIN" "$dir/jq"
}

# pod <namespace> <name> <cpu> <memory>
# One pod JSON object with a single container requesting <cpu> and <memory>.
# An empty <cpu> or <memory> leaves that request out.
pod() {
  jq -cn --arg ns "$1" --arg name "$2" --arg cpu "$3" --arg mem "$4" '
    {metadata: {namespace: $ns, name: $name},
     spec: {containers: [{name: "c", resources: {requests:
       ({} + (if $cpu == "" then {} else {cpu: $cpu} end)
           + (if $mem == "" then {} else {memory: $mem} end))}}]}}'
}

# pods <pod-json>...
# Wraps the given pod objects into a pod list.
pods() {
  printf '%s\n' "$@" | jq -cs '{items: .}'
}

# run_budget <tmp> [VAR=value...]
# Runs the script with the stub PATH and the given extra environment. The
# caller sets STUB_PODS_JSON (and optionally STUB_NODES / STUB_NODES_FAIL /
# STUB_PODS_FAIL).
run_budget() {
  local tmp="$1"
  shift
  (
    PATH="$tmp/bin:/usr/bin:/bin"
    export PATH
    export KUBECTL_LOG="$tmp/kubectl.log"
    unset NODE_BUDGET_CPU NODE_BUDGET_MEMORY NODE_BUDGET_SELECTOR
    for assignment in "$@"; do
      # shellcheck disable=SC2163 # export the NAME=value word as given
      export "$assignment"
    done
    bash "$BUDGET_SH"
  ) 2>&1
}

# row_of <output> <namespace/pod>
# Prints the table row of one pod with its columns squeezed to single spaces.
row_of() {
  printf '%s\n' "$1" | awk -v n="$2" '$1 == n {print $1, $2, $3}'
}

new_tmp() {
  local tmp
  tmp="$(mktemp -d)"
  make_stub "$tmp/bin"
  echo "$tmp"
}

# ---------------------------------------------------------------------------
# Test 1: within budget prints the table, sorted by CPU, and exits 0
# ---------------------------------------------------------------------------
test_within_budget() {
  echo "Test: a pod list within budget exits 0 with the table and totals"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_PODS_JSON="$(pods "$(pod openstack small 500m 1Gi)" "$(pod kube-system big 1500m 2Gi)")"
  output="$(run_budget "$tmp")"
  rc=$?

  assert_eq "exit code is 0" "0" "$rc"
  assert_contains "the header names the columns" "$output" "NAMESPACE/POD"
  assert_contains "the header names the CPU column" "$output" "CPU(m)  MEMORY(Mi)"
  local order
  order="$(printf '%s\n' "$output" | awk '$1 ~ /\// && $1 != "NAMESPACE/POD" {print $1}' | tr '\n' ' ')"
  assert_eq "rows are sorted by CPU descending" "kube-system/big openstack/small " "$order"
  assert_eq "the big pod's row" "kube-system/big 1500 2048" "$(row_of "$output" kube-system/big)"
  assert_eq "the totals" "TOTAL 2000 3072" "$(row_of "$output" TOTAL)"
  assert_eq "the default budget" "BUDGET 4000 16384" "$(row_of "$output" BUDGET)"
  assert_not_contains "no over-budget line" "$output" "over budget"
}

# ---------------------------------------------------------------------------
# Test 2: one millicore over the CPU budget exits 1
# ---------------------------------------------------------------------------
test_cpu_over() {
  echo "Test: a CPU total of 4001m exits 1"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_PODS_JSON="$(pods "$(pod a p1 4000m 1Gi)" "$(pod a p2 1m 1Gi)")"
  output="$(run_budget "$tmp")"
  rc=$?

  assert_eq "exit code is 1" "1" "$rc"
  assert_contains "the CPU line" "$output" "ci-check-node-budget: over budget: cpu 4001m > 4000m"
  assert_not_contains "no memory line" "$output" "over budget: memory"
  assert_contains "the table is still printed" "$output" "TOTAL"
}

# ---------------------------------------------------------------------------
# Test 3: one MiB over the memory budget exits 1
# ---------------------------------------------------------------------------
test_memory_over() {
  echo "Test: a memory total of 16385Mi exits 1"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_PODS_JSON="$(pods "$(pod a p1 100m 16Gi)" "$(pod a p2 100m 1Mi)")"
  output="$(run_budget "$tmp")"
  rc=$?

  assert_eq "exit code is 1" "1" "$rc"
  assert_contains "the memory line" "$output" "ci-check-node-budget: over budget: memory 16385Mi > 16384Mi"
  assert_not_contains "no CPU line" "$output" "over budget: cpu"
}

# ---------------------------------------------------------------------------
# Test 4: both over prints both lines
# ---------------------------------------------------------------------------
test_both_over() {
  echo "Test: both totals over prints both lines"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_PODS_JSON="$(pods "$(pod a p1 5 17Gi)")"
  output="$(run_budget "$tmp")"
  rc=$?

  assert_eq "exit code is 1" "1" "$rc"
  assert_contains "the CPU line" "$output" "over budget: cpu 5000m > 4000m"
  assert_contains "the memory line" "$output" "over budget: memory 17408Mi > 16384Mi"
}

# ---------------------------------------------------------------------------
# Test 5: an empty pod list and a pod without requests count as 0
# ---------------------------------------------------------------------------
test_empty_and_missing_requests() {
  echo "Test: an empty list and a pod without requests count as 0"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_PODS_JSON='{"items":[]}'
  output="$(run_budget "$tmp")"
  rc=$?
  assert_eq "empty list: exit code is 0" "0" "$rc"
  assert_eq "empty list: totals are 0" "TOTAL 0 0" "$(row_of "$output" TOTAL)"

  STUB_PODS_JSON='{"items":[{"metadata":{"namespace":"a","name":"bare"},"spec":{"containers":[{"name":"c"},{"name":"d","resources":{}}]}}]}'
  output="$(run_budget "$tmp")"
  rc=$?
  assert_eq "no requests: exit code is 0" "0" "$rc"
  assert_eq "no requests: the row reads 0 / 0" "a/bare 0 0" "$(row_of "$output" a/bare)"
  assert_eq "no requests: totals are 0" "TOTAL 0 0" "$(row_of "$output" TOTAL)"
}

# ---------------------------------------------------------------------------
# Test 6: the effective request follows the scheduler's rule
# ---------------------------------------------------------------------------
test_effective_request_rule() {
  echo "Test: init containers, sidecars and overhead follow the scheduler's rule"
  local tmp output
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  # init-wins: app containers sum 100m, a regular init container asks 500m.
  # sidecar: a 100m app, a 200m restartPolicy: Always init container, then a
  # 250m regular one that runs beside it: 250m + 200m beats 100m + 200m.
  # sidecar-after: the same two init containers the other way round; the
  # regular one runs alone, so the app sum plus the sidecar wins.
  # overhead: a 100m app plus spec.overhead of 10m CPU and 16Mi.
  STUB_PODS_JSON='{"items":[
    {"metadata":{"namespace":"a","name":"init-wins"},"spec":{
      "initContainers":[{"name":"i","resources":{"requests":{"cpu":"500m","memory":"64Mi"}}}],
      "containers":[{"name":"c1","resources":{"requests":{"cpu":"60m","memory":"100Mi"}}},
                    {"name":"c2","resources":{"requests":{"cpu":"40m","memory":"100Mi"}}}]}},
    {"metadata":{"namespace":"a","name":"sidecar"},"spec":{
      "initContainers":[{"name":"s","restartPolicy":"Always","resources":{"requests":{"cpu":"200m","memory":"32Mi"}}},
                        {"name":"i","resources":{"requests":{"cpu":"250m","memory":"8Mi"}}}],
      "containers":[{"name":"c","resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}},
    {"metadata":{"namespace":"a","name":"sidecar-after"},"spec":{
      "initContainers":[{"name":"i","resources":{"requests":{"cpu":"250m","memory":"8Mi"}}},
                        {"name":"s","restartPolicy":"Always","resources":{"requests":{"cpu":"200m","memory":"32Mi"}}}],
      "containers":[{"name":"c","resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}},
    {"metadata":{"namespace":"a","name":"overhead"},"spec":{
      "overhead":{"cpu":"10m","memory":"16Mi"},
      "containers":[{"name":"c","resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}}
  ]}'
  output="$(run_budget "$tmp")"

  assert_eq "a 500m init container beats a 100m app sum" \
    "a/init-wins 500 200" "$(row_of "$output" a/init-wins)"
  assert_eq "a restartPolicy: Always init container adds to a later regular one" \
    "a/sidecar 450 96" "$(row_of "$output" a/sidecar)"
  assert_eq "a restartPolicy: Always init container adds to the app sum" \
    "a/sidecar-after 300 96" "$(row_of "$output" a/sidecar-after)"
  assert_eq "spec.overhead adds on top" \
    "a/overhead 110 80" "$(row_of "$output" a/overhead)"
}

# ---------------------------------------------------------------------------
# Test 7: quantities parse to millicores and MiB
# ---------------------------------------------------------------------------
test_quantities() {
  echo "Test: quantities parse with every supported form"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_PODS_JSON="$(pods \
    "$(pod q one 1 512Mi)" \
    "$(pod q half 0.5 1Gi)" \
    "$(pod q milli 250m 1G)" \
    "$(pod q exp 1e3m 134217728)")"
  output="$(run_budget "$tmp")"
  rc=$?

  assert_eq "exit code is 0" "0" "$rc"
  assert_eq "cpu 1 is 1000m, memory 512Mi is 512" "q/one 1000 512" "$(row_of "$output" q/one)"
  assert_eq "cpu 0.5 is 500m, memory 1Gi is 1024" "q/half 500 1024" "$(row_of "$output" q/half)"
  assert_eq "cpu 250m is 250m, memory 1G rounds up to 954" "q/milli 250 954" "$(row_of "$output" q/milli)"
  assert_eq "cpu 1e3m is 1000m, memory 134217728 is 128" "q/exp 1000 128" "$(row_of "$output" q/exp)"

  # Every other suffix of both factor tables, one row each. The totals exceed
  # the budget here, which does not stop the rows from printing.
  STUB_PODS_JSON="$(pods \
    "$(pod q k 1k 1048576k)" \
    "$(pod q M 1M 1048576M)" \
    "$(pod q G 1G 1048576000m)" \
    "$(pod q T 1T 1T)" \
    "$(pod q Ki 1Ki 1024Ki)" \
    "$(pod q Mi 1Mi 1Ti)" \
    "$(pod q Gi 1Gi '')" \
    "$(pod q Ti 1Ti '')")"
  output="$(run_budget "$tmp")"
  assert_eq "cpu 1k, memory 1048576k is 1000Mi" "q/k 1000000 1000" "$(row_of "$output" q/k)"
  assert_eq "cpu 1M, memory 1048576M is 1000000Mi" "q/M 1000000000 1000000" "$(row_of "$output" q/M)"
  assert_eq "cpu 1G, memory 1048576000m is 1Mi" "q/G 1000000000000 1" "$(row_of "$output" q/G)"
  assert_eq "cpu 1T, memory 1T rounds up to 953675Mi" "q/T 1000000000000000 953675" "$(row_of "$output" q/T)"
  assert_eq "cpu 1Ki, memory 1024Ki is 1Mi" "q/Ki 1024000 1" "$(row_of "$output" q/Ki)"
  assert_eq "cpu 1Mi, memory 1Ti is 1048576Mi" "q/Mi 1048576000 1048576" "$(row_of "$output" q/Mi)"
  assert_eq "cpu 1Gi" "q/Gi 1073741824000 0" "$(row_of "$output" q/Gi)"
  assert_eq "cpu 1Ti" "q/Ti 1099511627776000 0" "$(row_of "$output" q/Ti)"

  STUB_PODS_JSON="$(pods "$(pod q bad 12xy 1Gi)")"
  output="$(run_budget "$tmp")"
  rc=$?
  assert_eq "an unparsable quantity exits 2" "2" "$rc"
  assert_contains "the message names the quantity and the pod" \
    "$output" 'ci-check-node-budget: cannot parse quantity "12xy" on q/bad'
  assert_not_contains "no table is printed" "$output" "TOTAL"
}

# ---------------------------------------------------------------------------
# Test 8: a custom budget from the environment
# ---------------------------------------------------------------------------
test_custom_budget() {
  echo "Test: NODE_BUDGET_CPU and NODE_BUDGET_MEMORY override the budget"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_PODS_JSON="$(pods "$(pod a p1 1500m 1Gi)")"
  output="$(run_budget "$tmp" NODE_BUDGET_CPU=1000m NODE_BUDGET_MEMORY=2Gi)"
  rc=$?
  assert_eq "exit code is 1" "1" "$rc"
  assert_contains "the CPU line uses the custom budget" "$output" "over budget: cpu 1500m > 1000m"
  assert_eq "the budget row shows the custom budget" "BUDGET 1000 2048" "$(row_of "$output" BUDGET)"

  output="$(run_budget "$tmp" NODE_BUDGET_MEMORY=lots)"
  rc=$?
  assert_eq "an unparsable budget exits 2" "2" "$rc"
  assert_contains "the message names the budget variable" \
    "$output" 'cannot parse quantity "lots" on NODE_BUDGET_MEMORY'
}

# ---------------------------------------------------------------------------
# Test 9: anything but exactly one node exits 2
# ---------------------------------------------------------------------------
test_node_count() {
  echo "Test: two nodes or none exit 2"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  STUB_PODS_JSON='{"items":[]}'
  output="$(run_budget "$tmp" STUB_NODES="$(printf 'node/a\nnode/b')")"
  rc=$?
  assert_eq "two nodes: exit code is 2" "2" "$rc"
  assert_contains "two nodes: the message" "$output" \
    "ci-check-node-budget: expected exactly one node, found 2"
  assert_file_not_contains "two nodes: pods are not listed" "$tmp/kubectl.log" "\[pods\]"

  output="$(run_budget "$tmp" STUB_NODES=)"
  rc=$?
  assert_eq "no node: exit code is 2" "2" "$rc"
  assert_contains "no node: the message" "$output" "expected exactly one node, found 0"
}

# ---------------------------------------------------------------------------
# Test 9b: a failing node listing exits 2 with kubectl's stderr
# ---------------------------------------------------------------------------
test_nodes_listing_fails() {
  echo "Test: a failing kubectl get nodes exits 2"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_budget "$tmp" STUB_NODES_FAIL=1)"
  rc=$?
  assert_eq "exit code is 2" "2" "$rc"
  assert_contains "the message carries kubectl's stderr" "$output" \
    "ci-check-node-budget: listing nodes failed: error: stub apiserver unreachable"
  assert_file_not_contains "pods are not listed" "$tmp/kubectl.log" "\[pods\]"
}

# ---------------------------------------------------------------------------
# Test 10: a failing pod listing exits 2 with kubectl's stderr
# ---------------------------------------------------------------------------
test_pods_listing_fails() {
  echo "Test: a failing kubectl get pods exits 2"
  local tmp output rc
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN

  output="$(run_budget "$tmp" STUB_PODS_FAIL=1)"
  rc=$?
  assert_eq "exit code is 2" "2" "$rc"
  assert_contains "the message carries kubectl's stderr" "$output" \
    "ci-check-node-budget: listing pods failed: error: stub apiserver down"
}

# ---------------------------------------------------------------------------
# Test 11: NODE_BUDGET_SELECTOR becomes one -l argument, or none
# ---------------------------------------------------------------------------
test_pod_selector() {
  echo "Test: NODE_BUDGET_SELECTOR reaches kubectl as one -l argument"
  local tmp selector
  tmp="$(new_tmp)"
  trap 'rm -rf "$tmp"' RETURN
  selector='app.kubernetes.io/name notin (ovnchassis,neutronmetadataagent,nova-fake-compute)'

  STUB_PODS_JSON='{"items":[]}'
  run_budget "$tmp" "NODE_BUDGET_SELECTOR=$selector" >/dev/null
  assert_file_contains_fixed "the selector follows -l as one argument" \
    "$tmp/kubectl.log" "[-l][$selector]"
  assert_file_contains_fixed "pods are listed on the node, running ones only" \
    "$tmp/kubectl.log" "[get][pods][-A][--field-selector][spec.nodeName=stub-node,status.phase!=Succeeded,status.phase!=Failed][-o][json]"

  : >"$tmp/kubectl.log"
  run_budget "$tmp" NODE_BUDGET_SELECTOR= >/dev/null
  assert_file_not_contains "an empty selector passes no -l" "$tmp/kubectl.log" "\[-l\]"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if [ -z "$JQ_BIN" ]; then
  echo "SKIP: jq not installed (every case needs it)"
  SKIP=$((SKIP + 1))
else
  export STUB_PODS_JSON=""
  test_within_budget
  test_cpu_over
  test_memory_over
  test_both_over
  test_empty_and_missing_requests
  test_effective_request_rule
  test_quantities
  test_custom_budget
  test_node_count
  test_nodes_listing_fails
  test_pods_listing_fails
  test_pod_selector
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
