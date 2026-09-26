#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-check-node-budget.sh — Fail when the pods on the kind node request
# more than one 4 vCPU / 16 GiB node offers.
#
# The kind node of the self-hosted runner is larger than the machine the
# devstack targets, so a stack that outgrows that machine still schedules in
# CI. This script turns the target into a check: it sums the effective
# requests of every non-terminated pod on the single node and compares the
# totals with the budget. It reads the cluster and changes nothing.
#
# The effective request of a pod follows the scheduler's rule, per resource:
# the larger of (the sum over its containers plus the sum over its init
# containers with restartPolicy: Always) and the largest request among its
# other init containers, each counted with the restartPolicy: Always init
# containers declared before it, plus spec.overhead. A missing request counts
# as 0.
# All arithmetic runs in jq; the self-hosted runners ship it, and
# hack/deploy-infra.sh and hack/ci-dump-diagnostics.sh rely on it too.
#
# Required env vars:
#   (none)
#
# Optional env vars:
#   NODE_BUDGET_CPU      — CPU budget as a Kubernetes quantity (default: 4000m)
#   NODE_BUDGET_MEMORY   — memory budget as a Kubernetes quantity (default: 16Gi)
#   NODE_BUDGET_SELECTOR — label selector passed to `kubectl get pods -l`; only
#                          the pods it matches are summed (use `notin (...)` to
#                          leave pods out). Unset or empty counts every pod on
#                          the node.
#
# Usage:
#   hack/ci-check-node-budget.sh
#   NODE_BUDGET_SELECTOR='app.kubernetes.io/name notin (ovnchassis)' \
#     hack/ci-check-node-budget.sh
#
# Prints one row per pod (NAMESPACE/POD, CPU in millicores, memory in MiB,
# sorted by CPU descending), then the totals and the budget.
#
# Exit codes:
#   0 — both totals are within the budget
#   1 — at least one total is over the budget; one line per exceeded resource
#   2 — the requests cannot be measured (no single node, a failing kubectl, an
#       unparsable quantity, jq missing)

set -euo pipefail

NODE_BUDGET_CPU="${NODE_BUDGET_CPU:-4000m}"
NODE_BUDGET_MEMORY="${NODE_BUDGET_MEMORY:-16Gi}"
NODE_BUDGET_SELECTOR="${NODE_BUDGET_SELECTOR:-}"

# ---------------------------------------------------------------------------
# die — Print a prefixed message to stderr and exit 2 (cannot measure).
# ---------------------------------------------------------------------------
die() {
  echo "ci-check-node-budget: $*" >&2
  exit 2
}

command -v jq >/dev/null 2>&1 || die "jq is required"

errfile="$(mktemp)"
trap 'rm -f "${errfile}"' EXIT

# ---------------------------------------------------------------------------
# 1. The node
# ---------------------------------------------------------------------------
# The budget describes one machine. A second node would split the pods, and
# summing both would compare two machines with the budget of one.
if ! nodes="$(kubectl get nodes -o name 2>"${errfile}")"; then
  die "listing nodes failed: $(cat "${errfile}")"
fi
node_count="$(printf '%s\n' "${nodes}" | grep -c . || true)"
if [[ "${node_count}" -ne 1 ]]; then
  die "expected exactly one node, found ${node_count}"
fi
node="${nodes#node/}"

# ---------------------------------------------------------------------------
# 2. The pods on it
# ---------------------------------------------------------------------------
# Succeeded and Failed pods hold no resources, so the scheduler ignores them.
# The selector travels as one argv element and is never evaluated by a shell.
kubectl_args=(
  get pods -A
  --field-selector "spec.nodeName=${node},status.phase!=Succeeded,status.phase!=Failed"
  -o json
)
if [[ -n "${NODE_BUDGET_SELECTOR}" ]]; then
  kubectl_args+=(-l "${NODE_BUDGET_SELECTOR}")
fi
if ! pods_json="$(kubectl "${kubectl_args[@]}" 2>"${errfile}")"; then
  die "listing pods failed: $(cat "${errfile}")"
fi

echo "Node budget for ${node}: cpu ${NODE_BUDGET_CPU}, memory ${NODE_BUDGET_MEMORY} (pod selector: ${NODE_BUDGET_SELECTOR:-<all pods>})"

# ---------------------------------------------------------------------------
# 3. Sum, print, compare
# ---------------------------------------------------------------------------
# qty parses a quantity into millicores (cpu) or bytes (memory), rounded up
# the way Quantity.MilliValue() and Quantity.Value() round. The 1e-9 absorbs
# floating-point noise such as 0.1 * 1000 = 100.00000000000001. The table,
# the totals and the over-budget lines go to stdout in that order. halt_error
# sets the exit code: 2 with the parse error on stderr, or 1 when over budget.
# shellcheck disable=SC2016 # $vars below are jq variables, not shell ones
jq_program='
def factors:
  {
    cpu: {"": 1000, m: 1, k: 1e6, M: 1e9, G: 1e12, T: 1e15,
          Ki: 1024000, Mi: 1048576000, Gi: 1073741824000, Ti: 1099511627776000},
    memory: {"": 1, m: 0.001, k: 1e3, M: 1e6, G: 1e9, T: 1e12,
             Ki: 1024, Mi: 1048576, Gi: 1073741824, Ti: 1099511627776}
  };

def roundup: if . > 0 then . - 1e-9 | ceil else 0 end;

def qty($res; $where):
  tostring as $q
  | ([$q | capture("^(?<n>[+-]?([0-9]+(\\.[0-9]*)?|\\.[0-9]+)([eE][+-]?[0-9]+)?)(?<s>Ki|Mi|Gi|Ti|m|k|M|G|T)?$")] | first) as $c
  | if $c == null then
      "ci-check-node-budget: cannot parse quantity \"\($q)\" on \($where)\n" | halt_error(2)
    else
      ($c.n | tonumber) * factors[$res][$c.s // ""] | roundup
    end;

def request($res; $where): (.resources.requests[$res] // "0") | qty($res; $where);

# The init containers run in order, so a regular one starts beside every
# restartPolicy: Always init container declared before it, and a sidecar
# starts beside the sidecars before it.
def effective($res):
  "\(.metadata.namespace)/\(.metadata.name)" as $where
  | ([.spec.containers[]? | request($res; $where)] | add // 0) as $app
  | (reduce (.spec.initContainers // [])[] as $c ({sidecars: 0, init: 0};
      ($c | request($res; $where)) as $r
      | if $c.restartPolicy == "Always"
        then .sidecars += $r | .init = ([.init, .sidecars] | max)
        else .init = ([.init, $r + .sidecars] | max)
        end)) as $acc
  | ([$app + $acc.sidecars, $acc.init] | max) + ((.spec.overhead[$res] // "0") | qty($res; $where));

def mib: . / 1048576 | roundup;
def lpad($w): tostring | (" " * ($w - length)) + .;
def rpad($w): tostring | . + (" " * ($w - length));

[.items[] | {name: "\(.metadata.namespace)/\(.metadata.name)", cpu: effective("cpu"), memory: effective("memory")}]
| sort_by(-.cpu, .name) as $rows
| {cpu: ($rows | map(.cpu) | add // 0), memory: ($rows | map(.memory) | add // 0)} as $total
| {cpu: ($cpu | qty("cpu"; "NODE_BUDGET_CPU")), memory: ($memory | qty("memory"; "NODE_BUDGET_MEMORY"))} as $budget
| "NAMESPACE/POD" as $hdr
| ([($rows[].name | length), ($hdr | length)] | max) as $w
| def line($name; $cpu; $mem): "\($name | rpad($w))  \($cpu | lpad(6))  \($mem | lpad(10))";
  line($hdr; "CPU(m)"; "MEMORY(Mi)"),
  ($rows[] | line(.name; .cpu; (.memory | mib))),
  line("TOTAL"; $total.cpu; ($total.memory | mib)),
  line("BUDGET"; $budget.cpu; ($budget.memory | mib)),
  (if $total.cpu > $budget.cpu then "ci-check-node-budget: over budget: cpu \($total.cpu)m > \($budget.cpu)m" else empty end),
  (if $total.memory > $budget.memory then "ci-check-node-budget: over budget: memory \($total.memory | mib)Mi > \($budget.memory | mib)Mi" else empty end),
  (if $total.cpu > $budget.cpu or $total.memory > $budget.memory then "" | halt_error(1) else empty end)
'

rc=0
printf '%s' "${pods_json}" \
  | jq -r --arg cpu "${NODE_BUDGET_CPU}" --arg memory "${NODE_BUDGET_MEMORY}" "${jq_program}" \
  || rc=$?
case "${rc}" in
  0) exit 0 ;;
  1) exit 1 ;;
  *) exit 2 ;;
esac
