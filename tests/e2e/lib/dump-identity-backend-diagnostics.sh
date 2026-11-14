#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/lib/dump-identity-backend-diagnostics.sh: print what a failed step
# of a keystone identity-backend suite needs to tell a reconcile pass that
# hangs from one that fails.
#
# Every catch block of oidc-federation, saml-federation and
# ldap-domain-backend runs it. It prints six sections, each under a
# "=== <title> ===" line:
#   1. the events of every KeystoneIdentityBackend in <cr-namespace>, with
#      their last and first time, count, type, reason, object and message;
#   2. the events of the Keystone <keystone-name>, in the same columns;
#   3. the last 30 events of <cr-namespace>, which several suites share;
#   4. per operator pod, the log lines of the last 30 minutes that contain
#      "controllerKind":"KeystoneIdentityBackend" or <keystone-name>, the
#      last 300 of them;
#   5. per operator pod, the last 60 log lines of the running containers and
#      of the previous ones;
#   6. per operator pod, the workqueue and reconcile families of its metrics
#      endpoint, read through the API server's pod proxy, for every
#      controller of the operator. workqueue_longest_running_processor_seconds
#      is the age of the oldest pass still running, and
#      controller_runtime_active_workers the number of busy workers.
# Sections 4 to 6 print a "--- <pod> ---" line before each pod's part. The
# chart runs two replicas and only the leader reconciles, so every pod is
# read.
#
# Usage:
#   dump-identity-backend-diagnostics.sh <cr-namespace> <keystone-name>
#
# The script is a dump, not a check. It only reads, prints kubectl's errors
# where the output they replace would be, and exits 0 whatever kubectl does.
# A part without output prints one line instead: "(no operator pod in
# keystone-system)", "(no matching line)", or "(metrics endpoint of <pod>
# unreachable)" after kubectl's error. Fewer or more than two arguments, or
# an empty one, print the usage line on stderr and exit 2.
#
# Chainsaw runs scripts in the suite directory, so a suite calls this as
# "../../lib/dump-identity-backend-diagnostics.sh".

set -uo pipefail

usage() {
  echo "usage: dump-identity-backend-diagnostics.sh <cr-namespace> <keystone-name>" >&2
  exit 2
}

if [ "$#" -ne 2 ] || [ -z "$1" ] || [ -z "$2" ]; then
  usage
fi

# Not NAMESPACE: chainsaw injects that variable into every script step.
cr_ns="$1"
keystone="$2"

operator_ns="keystone-system"
# The chart's metrics.port. The endpoint serves plain HTTP without
# authentication.
metrics_port=8080
columns='LAST:.lastTimestamp,FIRST:.firstTimestamp,COUNT:.count,TYPE:.type,REASON:.reason,OBJECT:.involvedObject.name,MESSAGE:.message'
# The families of section 6. Each of their lines carries labels.
families='^(workqueue_longest_running_processor_seconds|workqueue_unfinished_work_seconds|workqueue_depth|controller_runtime_active_workers|controller_runtime_max_concurrent_reconciles|controller_runtime_reconcile_total|controller_runtime_reconcile_errors_total)\{'

# print_nonempty <text>: <text> and a newline, or nothing for empty <text>.
print_nonempty() {
  if [ -n "$1" ]; then
    printf '%s\n' "$1"
  fi
}

# each_pod <function>: runs <function> <pod> for every operator pod after a
# "--- <pod> ---" line. Without pods it prints kubectl's error or the
# empty-list line.
each_pod() {
  local pod
  if [ -n "$pods_error" ]; then
    printf '%s\n' "$pods_error"
    return
  fi
  if [ -z "$pods" ]; then
    echo "(no operator pod in $operator_ns)"
    return
  fi
  for pod in $pods; do
    echo "--- $pod ---"
    "$1" "$pod"
  done
}

# filtered_log <pod>: section 4. kubectl's error is printed unfiltered.
filtered_log() {
  local log kept
  if ! log="$(kubectl logs -n "$operator_ns" "$1" --all-containers --since=30m --tail=-1 2>&1)"; then
    print_nonempty "$(tail -n 300 <<<"$log")"
    return
  fi
  kept="$(grep -F -e '"controllerKind":"KeystoneIdentityBackend"' -e "$keystone" <<<"$log" | tail -n 300)"
  if [ -z "$kept" ]; then
    echo "(no matching line)"
    return
  fi
  printf '%s\n' "$kept"
}

# recent_log <pod>: section 5.
recent_log() {
  kubectl logs -n "$operator_ns" "$1" --all-containers --tail=60 2>&1 || true
  kubectl logs -n "$operator_ns" "$1" --all-containers --tail=60 --previous 2>&1 || true
}

# metrics <pod>: section 6.
metrics() {
  local body kept
  if ! body="$(kubectl get --raw "/api/v1/namespaces/$operator_ns/pods/$1:$metrics_port/proxy/metrics" 2>&1)"; then
    print_nonempty "$body"
    echo "(metrics endpoint of $1 unreachable)"
    return
  fi
  kept="$(grep -E "$families" <<<"$body")"
  if [ -z "$kept" ]; then
    echo "(no matching line)"
    return
  fi
  printf '%s\n' "$kept"
}

echo "=== KeystoneIdentityBackend events in $cr_ns ==="
kubectl get events -n "$cr_ns" --field-selector involvedObject.kind=KeystoneIdentityBackend \
  --sort-by=.lastTimestamp -o custom-columns="$columns" 2>&1 || true

echo "=== Keystone $keystone events ==="
kubectl get events -n "$cr_ns" --field-selector "involvedObject.kind=Keystone,involvedObject.name=$keystone" \
  --sort-by=.lastTimestamp -o custom-columns="$columns" 2>&1 || true

echo "=== Events in $cr_ns (last 30) ==="
kubectl get events -n "$cr_ns" --sort-by=.lastTimestamp 2>&1 | tail -n 30 || true

# The operator pods for sections 4 to 6, read once: their names without the
# pod/ prefix, which also drops kubectl's "No resources found" line, or
# kubectl's error in pods_error.
pods=""
pods_error=""
if pod_list="$(kubectl get pods -n "$operator_ns" -l app.kubernetes.io/name=keystone-operator -o name 2>&1)"; then
  pods="$(sed -n 's|^pod/||p' <<<"$pod_list")"
else
  pods_error="$pod_list"
fi

echo "=== operator log: KeystoneIdentityBackend controller and $keystone (since 30m) ==="
each_pod filtered_log

echo "=== operator log (last 60 lines) ==="
each_pod recent_log

echo "=== operator workqueue and reconcile metrics ==="
each_pod metrics

exit 0
