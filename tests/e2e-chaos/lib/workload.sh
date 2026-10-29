#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Chaos workload harness: the commands a chaos suite calls to send OpenStack
# traffic through its fault and to judge that traffic afterwards.
#
# Run from the suite directory as ../lib/workload.sh, like ../diagnostics.sh,
# with NAMESPACE set by Chainsaw. <id> names the workload pod chaos-wl-<id> and
# matches ^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$; suites use their directory
# name.
#
# Usage:
#   workload.sh start <id> -- <run options>
#   workload.sh mark <id> fault-start|fault-end
#   workload.sh verify <id> [--placement-audit <nova-deployment>] [--ovn-db-sync <neutron-cr>]
#   workload.sh dump <id>
#   workload.sh stop <id>
#
# start   Starts `workload.py run` in a pod on the Tempest image and waits
#         until every stream of --streams printed READY. The pod carries the
#         label app.kubernetes.io/name=chaos-workload, which no chaos selector
#         matches. OS_PASSWORD is set from the admin password Secret when the
#         run options carry --keystone.
# mark    Writes the pod clock to the pod annotation chaos.c5c3.io/<mark>, so
#         the marks and the records are read off one clock.
# verify  Reads the budget line of ./chainsaw-test.yaml, waits until the pod
#         clock passes fault_end + recovery + 30 s (30 s from now without
#         marks), creates the stop file, waits for DONE, runs `workload.py
#         evaluate` on the log and then the requested out-of-band checks. It
#         deletes the pod and exits 1 when anything failed.
# dump    Prints the pod's phase, its marks and the last 200 log lines, for
#         catch blocks. Always exits 0.
# stop    Deletes the pod, for cleanup blocks. Always exits 0.
#
# Environment overrides:
#   CHAOS_WORKLOAD_IMAGE            the pod image (ghcr.io/c5c3/tempest:2026.1)
#   CHAOS_WORKLOAD_READY_SECONDS    how long start waits for READY (300)
#   CHAOS_WORKLOAD_DONE_SECONDS     how long verify waits for DONE (420)
#   CHAOS_WORKLOAD_DB_SYNC_SECONDS  how long --ovn-db-sync waits for its Job (600)
#   CHAOS_WORKLOAD_ADMIN_SECRET     the Secret in NAMESPACE that start reads the
#                                   admin password from (keystone-admin); a
#                                   managed ControlPlane materializes it as
#                                   <cp>-keystone-admin-credentials
#
# docs/reference/testing/chaos-e2e-tests.md describes the harness.

set -euo pipefail

LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKLOAD_PY="${LIB_DIR}/workload.py"
# placement_audit and ovn_db_sync, the out-of-band checks of verify.
# shellcheck source=tests/e2e-chaos/lib/consistency.sh
. "${LIB_DIR}/consistency.sh"
DEFAULT_IMAGE="ghcr.io/c5c3/tempest:2026.1"
STOP_FILE=/tmp/chaos-workload.stop
# How long verify keeps observing past fault_end + recovery, or past its own
# start without marks, before it creates the stop file.
OBSERVE_SLACK=30
BUDGET_RE='^# Chaos budget: failures=([0-9]+) recovery=([0-9]+)s$'
ID_RE='^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$'
NAMESPACE="${NAMESPACE:-}"
POD=""
CLOCK=""

usage() {
  echo "usage: workload.sh start <id> -- <run options>" >&2
  echo "       workload.sh mark <id> fault-start|fault-end" >&2
  echo "       workload.sh verify <id> [--placement-audit <nova-deployment>] [--ovn-db-sync <neutron-cr>]" >&2
  echo "       workload.sh dump <id>" >&2
  echo "       workload.sh stop <id>" >&2
  exit 2
}

# set_pod <id> [strict] — name the pod; with strict, refuse an invalid id or a
# missing NAMESPACE with exit 2.
set_pod() {
  if [ "${2:-}" = strict ]; then
    if ! [[ "$1" =~ $ID_RE ]]; then
      echo "workload.sh: invalid id '$1': it must match ${ID_RE}" >&2
      exit 2
    fi
    if [ -z "$NAMESPACE" ]; then
      echo "workload.sh: NAMESPACE is not set; run it from a Chainsaw script" >&2
      exit 2
    fi
  fi
  POD="chaos-wl-$1"
}

pod_field() {
  kubectl get pod "$POD" -n "$NAMESPACE" -o "jsonpath=$1" 2>/dev/null || true
}

# show_pod — the log and the pod description. The description leaves out the
# container command, which carries all of workload.py, and the admin password
# the pod carries in its env.
show_pod() {
  echo "=== workload pod ${POD}: log ==="
  kubectl logs "$POD" -n "$NAMESPACE" --tail=200 2>&1 || true
  echo "=== workload pod ${POD}: status ==="
  kubectl describe pod "$POD" -n "$NAMESPACE" 2>&1 |
    awk '/^    Command:/ { print "    Command:      python3 -c <lib/workload.py> run ..."; skip = 1; next }
         skip && /^    [A-Za-z]/ { skip = 0 }
         !skip { print }' |
    sed -E 's/^([[:space:]]*OS_PASSWORD:[[:space:]]*).*/\1<redacted>/' || true
}

# read_clock <date format> — set CLOCK to the pod clock, or return 1 with the
# error in CLOCK.
read_clock() {
  local err
  err="$(mktemp)"
  if ! CLOCK="$(kubectl exec "$POD" -n "$NAMESPACE" -- date "$1" 2>"$err")"; then
    CLOCK="$(cat "$err")"
    rm -f "$err"
    return 1
  fi
  rm -f "$err"
  if ! [[ "$CLOCK" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    CLOCK="the pod answered '${CLOCK}', not a time"
    return 1
  fi
}

# The pod's resources and securityContext. kubectl run names the container
# after the pod, and a strategic merge keeps the image and command it set.
# workload.py never calls the Kubernetes API, so the pod mounts no token.
overrides() {
  cat <<EOF
{"apiVersion": "v1", "spec": {"automountServiceAccountToken": false,
  "containers": [{"name": "${POD}",
  "resources": {"requests": {"cpu": "50m", "memory": "128Mi"},
                "limits": {"memory": "256Mi"}},
  "securityContext": {"runAsNonRoot": true, "runAsUser": 42424,
                      "runAsGroup": 42424, "allowPrivilegeEscalation": false,
                      "capabilities": {"drop": ["ALL"]},
                      "seccompProfile": {"type": "RuntimeDefault"}}}]}}
EOF
}

cmd_start() {
  if [ $# -lt 2 ] || [ "$2" != "--" ]; then
    usage
  fi
  local id="$1"
  set_pod "$id" strict
  shift 2

  local arg prev="" streams="" keystone=false
  for arg in "$@"; do
    if [ "$prev" = "--streams" ]; then
      streams="$arg"
    fi
    case "$arg" in
      --streams=*) streams="${arg#--streams=}" ;;
      --keystone | --keystone=*) keystone=true ;;
    esac
    prev="$arg"
  done
  if [ -z "$streams" ]; then
    echo "workload.sh: start ${id}: the run options name no --streams" >&2
    exit 2
  fi

  kubectl delete pod "$POD" -n "$NAMESPACE" --ignore-not-found --wait=true --timeout=60s

  local env_args=() password admin_secret="${CHAOS_WORKLOAD_ADMIN_SECRET:-keystone-admin}"
  if [ "$keystone" = true ]; then
    password="$(kubectl get secret "$admin_secret" -n "$NAMESPACE" -o jsonpath='{.data.password}' | base64 -d)"
    if [ -z "$password" ]; then
      echo "FAIL: start ${id}: Secret ${admin_secret} holds no password"
      exit 1
    fi
    env_args=(--env "OS_PASSWORD=${password}")
  fi

  kubectl run "$POD" -n "$NAMESPACE" --image="${CHAOS_WORKLOAD_IMAGE:-$DEFAULT_IMAGE}" \
    --restart=Never \
    --labels="app.kubernetes.io/name=chaos-workload,chaos.c5c3.io/workload=${id}" \
    ${env_args[@]+"${env_args[@]}"} \
    --override-type=strategic --overrides="$(overrides)" \
    --command -- python3 -c "$(cat "$WORKLOAD_PY")" \
    run --id "$id" --stop-file "$STOP_FILE" "$@"

  local limit="${CHAOS_WORKLOAD_READY_SECONDS:-300}"
  local deadline=$(($(date +%s) + limit))
  local phase log missing stream list
  IFS=, read -ra list <<<"$streams"
  while true; do
    # The phase is read before the log, so a pod that ended has written all
    # of its log by the time the log is searched.
    phase="$(pod_field '{.status.phase}')"
    log="$(kubectl logs "$POD" -n "$NAMESPACE" 2>/dev/null || true)"
    missing=""
    for stream in "${list[@]}"; do
      if ! grep -qxF "READY ${stream}" <<<"$log"; then
        missing="${missing} ${stream}"
      fi
    done
    if [ -z "$missing" ]; then
      echo "OK: workload ${POD} is READY on ${streams}"
      return 0
    fi
    case "$phase" in
      Succeeded | Failed)
        echo "FAIL: start ${id}: the pod reached ${phase} before READY on:${missing}"
        show_pod
        exit 1
        ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: start ${id}: no READY within ${limit}s on:${missing}"
      show_pod
      exit 1
    fi
    sleep 5
  done
}

cmd_mark() {
  if [ $# -ne 2 ]; then
    usage
  fi
  case "$2" in
    fault-start | fault-end) ;;
    *) usage ;;
  esac
  set_pod "$1" strict
  if ! read_clock +%s.%N; then
    echo "FAIL: mark $2: reading the clock of ${POD} failed: ${CLOCK}"
    exit 1
  fi
  kubectl annotate pod "$POD" -n "$NAMESPACE" "chaos.c5c3.io/$2=${CLOCK}" --overwrite >/dev/null
  echo "OK: ${POD} chaos.c5c3.io/$2=${CLOCK}"
}

# verify_failed <reason> — print the reason, the log and the pod, and exit 1.
# The pod stays for the catch block's dump; the step's cleanup deletes it.
verify_failed() {
  echo "FAIL: verify ${POD}: $1"
  show_pod
  exit 1
}

cmd_verify() {
  if [ $# -lt 1 ]; then
    usage
  fi
  local id="$1"
  set_pod "$id" strict
  shift
  local audit="" ovn=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --placement-audit | --ovn-db-sync)
        if [ $# -lt 2 ]; then
          usage
        fi
        if [ "$1" = "--placement-audit" ]; then audit="$2"; else ovn="$2"; fi
        shift 2
        ;;
      *) usage ;;
    esac
  done

  # The budget line of the suite: exactly one, in the one format #1388 reads.
  local suite="${PWD}/chainsaw-test.yaml" count line failures recovery
  count="$(grep -c '^# Chaos budget:' "$suite" 2>/dev/null || true)"
  if [ "${count:-0}" -eq 0 ]; then
    echo "FAIL: verify ${id}: ${suite} has no '# Chaos budget: failures=<N> recovery=<S>s' line"
    exit 1
  fi
  if [ "$count" -gt 1 ]; then
    echo "FAIL: verify ${id}: ${suite} has ${count} '# Chaos budget:' lines, want one"
    exit 1
  fi
  line="$(grep '^# Chaos budget:' "$suite")"
  if ! [[ "$line" =~ $BUDGET_RE ]]; then
    echo "FAIL: verify ${id}: '${line}' in ${suite} does not read '# Chaos budget: failures=<N> recovery=<S>s'"
    exit 1
  fi
  failures="${BASH_REMATCH[1]}"
  recovery="${BASH_REMATCH[2]}"

  local err phase
  err="$(mktemp)"
  if ! phase="$(kubectl get pod "$POD" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>"$err")"; then
    phase="$(cat "$err")"
    rm -f "$err"
    verify_failed "the workload pod is missing: ${phase}"
  fi
  rm -f "$err"
  local start end target
  start="$(pod_field '{.metadata.annotations.chaos\.c5c3\.io/fault-start}')"
  end="$(pod_field '{.metadata.annotations.chaos\.c5c3\.io/fault-end}')"

  # Wait on the pod clock, the clock the marks and the records were read off.
  if ! read_clock +%s; then
    verify_failed "reading the clock failed: ${CLOCK}"
  fi
  if [ -n "$end" ]; then
    target=$((${end%%.*} + recovery + OBSERVE_SLACK))
  else
    target=$((CLOCK + OBSERVE_SLACK))
  fi
  echo "verify ${id}: budget failures=${failures} recovery=${recovery}s, fault-start=${start:-none} fault-end=${end:-none}; observing until ${target}"
  while [ "$CLOCK" -lt "$target" ]; do
    sleep "$((target - CLOCK < 5 ? target - CLOCK : 5))"
    if ! read_clock +%s; then
      verify_failed "reading the clock failed: ${CLOCK}"
    fi
  done

  local out
  if ! out="$(kubectl exec "$POD" -n "$NAMESPACE" -- touch "$STOP_FILE" 2>&1)"; then
    verify_failed "creating the stop file failed: ${out}"
  fi

  local limit="${CHAOS_WORKLOAD_DONE_SECONDS:-420}"
  local deadline=$(($(date +%s) + limit)) log
  while true; do
    phase="$(pod_field '{.status.phase}')"
    log="$(kubectl logs "$POD" -n "$NAMESPACE" 2>/dev/null || true)"
    if grep -qx 'DONE' <<<"$log"; then
      break
    fi
    case "$phase" in
      Succeeded | Failed) verify_failed "the workload ended ${phase} without DONE" ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      verify_failed "no DONE within ${limit}s of the stop file"
    fi
    sleep 5
  done

  local logfile failed=0 rc=0 marks=()
  logfile="$(mktemp)"
  printf '%s\n' "$log" >"$logfile"
  echo "=== workload ${POD}: the lines besides the records ==="
  grep -v '^{' "$logfile" || true
  if [ -n "$start" ]; then marks+=(--fault-start "$start"); fi
  if [ -n "$end" ]; then marks+=(--fault-end "$end"); fi
  echo "=== workload ${POD}: evaluate ==="
  python3 "$WORKLOAD_PY" evaluate --log "$logfile" --budget-failures "$failures" \
    --recovery "$recovery" ${marks[@]+"${marks[@]}"} || rc=$?
  rm -f "$logfile"
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: workload.py evaluate exited ${rc}"
    failed=1
  fi

  if [ -n "$audit" ]; then
    placement_audit "$audit" || failed=1
  fi
  if [ -n "$ovn" ]; then
    ovn_db_sync "$id" "$ovn" || failed=1
  fi

  if [ "$failed" -ne 0 ]; then
    echo "=== workload pod ${POD}: last 200 log lines ==="
    tail -n 200 <<<"$log"
  fi
  kubectl delete pod "$POD" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [ "$failed" -ne 0 ]; then
    echo "FAIL: verify ${id}"
    exit 1
  fi
  echo "OK: verify ${id}"
}

cmd_dump() {
  if [ $# -ne 1 ]; then
    usage
  fi
  set_pod "$1"
  echo "=== chaos workload ${POD} ==="
  kubectl get pod "$POD" -n "$NAMESPACE" \
    -o 'jsonpath=phase={.status.phase} fault-start={.metadata.annotations.chaos\.c5c3\.io/fault-start} fault-end={.metadata.annotations.chaos\.c5c3\.io/fault-end}{"\n"}' 2>&1 || true
  kubectl logs "$POD" -n "$NAMESPACE" --tail=200 2>&1 || true
  exit 0
}

cmd_stop() {
  if [ $# -ne 1 ]; then
    usage
  fi
  set_pod "$1"
  kubectl delete pod "$POD" -n "$NAMESPACE" --ignore-not-found --wait=false 2>&1 || true
  exit 0
}

if [ $# -lt 1 ]; then
  usage
fi
command="$1"
shift
case "$command" in
  start) cmd_start "$@" ;;
  mark) cmd_mark "$@" ;;
  verify) cmd_verify "$@" ;;
  dump) cmd_dump "$@" ;;
  stop) cmd_stop "$@" ;;
  *) usage ;;
esac
