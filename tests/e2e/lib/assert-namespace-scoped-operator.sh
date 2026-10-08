#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/lib/assert-namespace-scoped-operator.sh: prove that the operator a
# namespace-scoped-rbac suite installed did the work itself.
#
# Every such suite runs beside the cluster-wide operator of the same chart,
# which watches every namespace and drives the suite's CR to the same Ready
# condition. A status condition does not say which operator wrote it, so the
# suite calls this script after the CR is Ready and reads the namespace-scoped
# release's single pod instead:
#   1. the release has exactly one pod;
#   2. its manager container never restarted;
#   3. the suite's CR is the only object of its kind in the namespace, so the
#      reconcile count of check 8 belongs to that CR;
#   4. the manager log is not empty;
#   5. the log carries the startup line of namespace-scoped mode for the
#      namespace;
#   6. the log carries no "Failed to watch" line for a forbidden list, which
#      the reflector writes on every retry of a cluster-scoped list the Role
#      does not grant;
#   7. the metrics endpoint answers through the API server's pod proxy;
#   8. the controller records a successful reconcile within
#      RECONCILE_WAIT_SECONDS (default 60), which a manager stuck in its cache
#      sync never does, and the manager still has not restarted at its end.
#
# Usage:
#   assert-namespace-scoped-operator.sh <namespace> <release> <controller> <resource>
#
# <release> is the Helm release name, <controller> the controller label of the
# metric (the lowercased kind, e.g. keystone), <resource> the fully qualified
# plural (e.g. keystones.keystone.openstack.c5c3.io). Each check prints one OK:
# line, or a FAIL: line with its evidence and exits 1. A missing or empty
# argument prints the usage line on stderr and exits 2.
#
# Chainsaw runs script steps in the suite directory, so a suite calls this as
# "../../lib/assert-namespace-scoped-operator.sh". The script only reads.

set -euo pipefail

usage() {
  echo "usage: assert-namespace-scoped-operator.sh <namespace> <release> <controller> <resource>" >&2
  exit 2
}

if [ "$#" -ne 4 ] || [ -z "$1" ] || [ -z "$2" ] || [ -z "$3" ] || [ -z "$4" ]; then
  usage
fi

# Not NAMESPACE: chainsaw injects that variable into every script step.
ns="$1"
release="$2"
controller="$3"
resource="$4"

# The chart's metrics.port, which the Deployment passes as
# --metrics-bind-address. The endpoint serves plain HTTP without
# authentication.
metrics_port=8080

# count_lines <text>: the number of non-empty lines. grep -c prints 0 and exits
# 1 on no match.
count_lines() {
  grep -c . <<<"$1" || true
}

# ── Check 1: the release has exactly one pod ──────────────────────────────
if ! pods="$(kubectl get pods -n "$ns" -l "app.kubernetes.io/instance=$release" \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[?(@.name=="manager")].restartCount}{"\n"}{end}')"; then
  echo "FAIL: cannot list the pods of release $release"
  exit 1
fi
pod_count="$(count_lines "$pods")"
if [ "$pod_count" -eq 0 ]; then
  echo "FAIL: no pod of release $release in $ns"
  exit 1
fi
if [ "$pod_count" -ne 1 ]; then
  echo "FAIL: expected one pod of release $release, found $pod_count"
  awk '{ print $1 }' <<<"$pods"
  exit 1
fi
read -r pod restarts <<<"$pods"
echo "OK: release $release runs one pod, $pod"

# ── Check 2: the manager container never restarted ───────────────────────
if [ "$restarts" != "0" ]; then
  echo "FAIL: pod $pod restarted $restarts times"
  kubectl logs -n "$ns" "$pod" -c manager --previous --tail=40 || true
  exit 1
fi
echo "OK: pod $pod has not restarted"

# ── Check 3: the suite's CR is the only one of its kind ───────────────────
if ! crs="$(kubectl get "$resource" -n "$ns" -o name)"; then
  echo "FAIL: cannot list the $resource in $ns"
  exit 1
fi
cr_count="$(count_lines "$crs")"
if [ "$cr_count" -ne 1 ]; then
  echo "FAIL: expected one $resource in $ns, found $cr_count"
  if [ -n "$crs" ]; then
    echo "$crs"
  fi
  exit 1
fi
echo "OK: $crs is the only $resource in $ns"

# ── Check 4: the manager log is not empty ─────────────────────────────────
# --tail=-1 keeps the startup line of check 5 however long the log has grown.
if ! logs="$(kubectl logs -n "$ns" "$pod" -c manager --tail=-1)"; then
  echo "FAIL: cannot read the log of pod $pod"
  exit 1
fi
if [ -z "$logs" ]; then
  echo "FAIL: pod $pod has no log"
  exit 1
fi
echo "OK: pod $pod has a log"

# ── Check 5: the manager started in namespace-scoped mode ─────────────────
# The startup line internal/common/bootstrap/manager.go logs. Two fixed-string
# matches, so the order of the JSON keys does not matter.
mode_lines="$(grep -F '"msg":"namespace-scoped mode enabled"' <<<"$logs" |
  grep -cF "\"namespace\":\"$ns\"" || true)"
if [ "$mode_lines" -eq 0 ]; then
  echo "FAIL: pod $pod did not start in namespace-scoped mode for $ns"
  exit 1
fi
echo "OK: pod $pod started in namespace-scoped mode for $ns"

# ── Check 6: no watch was forbidden ───────────────────────────────────────
# Only an RBAC denial, whose error says "forbidden", is the defect this check
# exists for. A transient API-server error also logs "Failed to watch", and the
# reflector recovers from it.
forbidden_watches="$(grep -F '"msg":"Failed to watch"' <<<"$logs" | grep -F 'forbidden' || true)"
watch_failures="$(count_lines "$forbidden_watches")"
if [ "$watch_failures" -ne 0 ]; then
  echo "FAIL: pod $pod logged $watch_failures forbidden 'Failed to watch' lines"
  # The first line names the kind the operator may not list.
  head -n1 <<<"$forbidden_watches"
  exit 1
fi
echo "OK: pod $pod logged no forbidden 'Failed to watch' line"

# ── Check 7: the metrics endpoint answers ─────────────────────────────────
if ! metrics="$(kubectl get --raw "/api/v1/namespaces/$ns/pods/$pod:$metrics_port/proxy/metrics")"; then
  echo "FAIL: metrics endpoint of pod $pod unreachable"
  exit 1
fi
if [ -z "$metrics" ]; then
  echo "FAIL: metrics endpoint of pod $pod returned nothing"
  exit 1
fi
echo "OK: metrics endpoint of pod $pod answers"

# ── Check 8: the controller reconciled successfully ───────────────────────
# Ready does not mean this operator has finished a successful pass: the
# cluster-wide operator may have written it, and the CR's own watch drops
# status-only updates, so that write does not wake this operator. A pass that
# ends in a requeue counts under requeue_after, and the next one can be tens of
# seconds away, so the check re-reads the metrics every 5 s until
# RECONCILE_WAIT_SECONDS run out. The unit test sets 0.
# client_golang sorts the labels, so controller precedes result. The closing
# quote of the prefix keeps "keystone" from matching
# "keystoneidentitybackend". A counter prints as 12 or as 1.2e+06, so awk
# compares the value as a number, under LC_ALL=C so that no locale reads the
# exponent form with a decimal comma.
reconcile_wait="${RECONCILE_WAIT_SECONDS:-60}"
success_prefix="controller_runtime_reconcile_total{controller=\"$controller\",result=\"success\"} "
has_success() {
  LC_ALL=C awk -v p="$success_prefix" '
    index($0, p) == 1 && $2 + 0 >= 1 { ok = 1 }
    END { exit !ok }' <<<"$metrics"
}
deadline=$((SECONDS + reconcile_wait))
until has_success; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "FAIL: pod $pod reports no successful reconcile of controller $controller after ${reconcile_wait}s"
    grep -F "controller_runtime_reconcile_total{controller=\"$controller\"," <<<"$metrics" || true
    exit 1
  fi
  sleep 5
  metrics="$(kubectl get --raw "/api/v1/namespaces/$ns/pods/$pod:$metrics_port/proxy/metrics")" || metrics=""
done
# The wait can end a minute after check 2, and a restarted container keeps the
# pod name, so the counter read above may belong to a later incarnation. The
# restart count is read again for that window.
if ! restarts="$(kubectl get pod -n "$ns" "$pod" \
  -o jsonpath='{.status.containerStatuses[?(@.name=="manager")].restartCount}')"; then
  echo "FAIL: cannot re-read the restart count of pod $pod"
  exit 1
fi
if [ "$restarts" != "0" ]; then
  echo "FAIL: pod $pod restarted $restarts times while check 8 waited"
  kubectl logs -n "$ns" "$pod" -c manager --previous --tail=40 || true
  exit 1
fi
echo "OK: pod $pod reports a successful reconcile of controller $controller"
