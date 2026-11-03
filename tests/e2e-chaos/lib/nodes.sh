#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Node faults of the multinode chaos leg: the commands that prepare its kind
# cluster of one control-plane node and two workers
# (hack/kind-config-multinode.yaml), and that kill, freeze and restore a
# worker.
#
# A kind node is a Docker container on the runner, so a node fault is a
# runner-side docker call and no Chaos Mesh resource. Its blast radius is one
# whole worker: every pod on it, its kubelet, kindnet, kube-proxy and
# chaos-daemon. The control-plane node is never faulted; it runs the API
# server and, on this leg, every singleton of the stack.
#
# Usage:
#   nodes.sh prepare
#   nodes.sh victim | survivor
#   nodes.sh kill <node> | pause <node>
#   nodes.sh restore <node>
#   nodes.sh restore-all
#   nodes.sh zones | zones-clear
#   nodes.sh open | close [<node>]
#
# prepare      Waits until two workers are registered and every node is Ready,
#              removes the NoSchedule taint of the control-plane node and
#              cordons both workers. The cordon is the leg's default state:
#              everything scheduled lands on the control-plane node, while
#              DaemonSet pods and pods with nodeName still reach the workers.
#              Fewer than two workers exit 1 and change no taint.
# victim       Prints the first name of the sorted workers.
# survivor     Prints the second.
# kill         Waits for Ready=True, runs docker kill -s KILL <node> and waits
#              for Ready!=True. A worker that is still joining does not come
#              back from a kill, hence the first wait.
# pause        The same with docker pause: the node's processes freeze, and its
#              kernel still answers ICMP and completes TCP handshakes.
# restore      Unpauses a paused container, starts an exited one, leaves a
#              running one alone, and waits for Ready=True.
# restore-all  Prints the Docker state and the Ready condition of every
#              worker and restores each one that is not running and Ready.
#              Exits 0 when every worker ends Ready, and 1 when the workers
#              cannot be listed.
# zones        Labels every node topology.kubernetes.io/zone=chaos-zone-<n>,
#              in name order, one zone per node as a production cluster has.
# zones-clear  Removes that label from every node.
# open         Uncordons both workers.
# close        Cordons both workers, or the one named.
#
# kill, pause and restore are the only commands that touch a container. They
# refuse, with exit 1 and "refusing to fault <node>: <reason>" and before any
# other docker call, a name whose container carries no io.x-k8s.kind.cluster
# label, the control-plane node, and a name that is no worker Node of the
# cluster. The script holds a docker kill on a runner that is reused between
# jobs, and this guard is its trust boundary. A usage error exits 2.
#
# Environment overrides:
#   CHAOS_NODES_FAULT_SECONDS    the Ready waits of kill and pause (120)
#   CHAOS_NODES_RESTORE_SECONDS  the Ready wait of restore (300)
#   CHAOS_NODES_PREPARE_SECONDS  how long prepare waits for the nodes (300)
#   CHAOS_NODES_POLL_SECONDS     the poll interval of every wait (5)
#
# docs/reference/testing/chaos-e2e-tests.md describes the node faults (Test
# Patterns, Node faults).

set -euo pipefail

FAULT_SECONDS="${CHAOS_NODES_FAULT_SECONDS:-120}"
RESTORE_SECONDS="${CHAOS_NODES_RESTORE_SECONDS:-300}"
PREPARE_SECONDS="${CHAOS_NODES_PREPARE_SECONDS:-300}"
POLL_SECONDS="${CHAOS_NODES_POLL_SECONDS:-5}"
CP_LABEL="node-role.kubernetes.io/control-plane"
ZONE_LABEL="topology.kubernetes.io/zone"
KIND_LABEL="io.x-k8s.kind.cluster"
# The Ready condition the last wait_ready read.
LAST_READY=""

usage() {
  echo "usage: nodes.sh prepare" >&2
  echo "       nodes.sh victim|survivor" >&2
  echo "       nodes.sh kill|pause|restore <node>" >&2
  echo "       nodes.sh restore-all" >&2
  echo "       nodes.sh zones|zones-clear" >&2
  echo "       nodes.sh open|close [<node>]" >&2
  exit 2
}

# names <selector>: the names of the Nodes the selector matches, sorted.
names() {
  kubectl get nodes -l "$1" -o name | sed 's|^node/||' | LC_ALL=C sort
}

workers() {
  names "!${CP_LABEL}"
}

control_plane() {
  names "${CP_LABEL}"
}

# ready_of <node>: the status of the node's Ready condition, empty when the
# read fails.
ready_of() {
  kubectl get node "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

# docker_state <node>: the container's state, or "missing" when it has none.
docker_state() {
  docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null || echo missing
}

# wait_ready <node> True|notTrue <seconds>: poll the Ready condition until it
# is True, or until it is anything but True; an unreadable condition counts as
# neither. Leaves the last read in LAST_READY.
wait_ready() {
  local node="$1" want="$2" deadline
  deadline=$(($(date +%s) + $3))
  while true; do
    LAST_READY="$(ready_of "$node")"
    case "$want:$LAST_READY" in
      True:True) return 0 ;;
      notTrue:True | notTrue:) ;;
      notTrue:*) return 0 ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      return 1
    fi
    sleep "$POLL_SECONDS"
  done
}

refuse() {
  echo "FAIL: refusing to fault $1: $2"
  return 1
}

# guard <node>: refuse a container outside kind, the control plane, and a name
# that is no worker of this cluster.
guard() {
  local node="$1" cluster role
  cluster="$(docker inspect -f "{{index .Config.Labels \"${KIND_LABEL}\"}}" "$node" 2>/dev/null || true)"
  if [ -z "$cluster" ] || [ "$cluster" = "<no value>" ]; then
    refuse "$node" "its container carries no ${KIND_LABEL} label"
    return 1
  fi
  role="$(docker inspect -f '{{index .Config.Labels "io.x-k8s.kind.role"}}' "$node" 2>/dev/null || true)"
  if [ "$role" = control-plane ] || grep -qxF -- "$node" <<<"$(control_plane)"; then
    refuse "$node" "it is the control plane"
    return 1
  fi
  if ! grep -qxF -- "$node" <<<"$(workers)"; then
    refuse "$node" "it is no worker Node of this cluster"
    return 1
  fi
}

cmd_prepare() {
  [ $# -eq 0 ] || usage
  local deadline list count cp out node notready
  deadline=$(($(date +%s) + PREPARE_SECONDS))
  while true; do
    list="$(workers)"
    count="$(grep -c . <<<"$list" || true)"
    notready=""
    for node in $(control_plane) $list; do
      if [ "$(ready_of "$node")" != True ]; then
        notready="${notready} ${node}"
      fi
    done
    if [ "$count" -ge 2 ] && [ -z "$notready" ]; then
      break
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      if [ "$count" -lt 2 ]; then
        echo "FAIL: the multinode leg needs two workers, found ${count}"
      else
        echo "FAIL: not Ready within ${PREPARE_SECONDS}s:${notready}"
      fi
      exit 1
    fi
    sleep "$POLL_SECONDS"
  done
  cp="$(control_plane)"
  # A second prepare finds the taint gone, which kubectl reports as not found.
  if ! out="$(kubectl taint nodes "$cp" "${CP_LABEL}:NoSchedule-" 2>&1)" && ! grep -q 'not found' <<<"$out"; then
    echo "FAIL: removing the control-plane taint of ${cp} failed: ${out}"
    exit 1
  fi
  # shellcheck disable=SC2086 # one name per word
  kubectl cordon $list >/dev/null
  echo "OK: ${cp} is schedulable, workers $(xargs <<<"$list") are cordoned"
}

# cmd_pick <n>: the name of the n-th worker.
cmd_pick() {
  local n="$1" name
  shift
  [ $# -eq 0 ] || usage
  name="$(workers | sed -n "${n}p")"
  if [ -z "$name" ]; then
    echo "FAIL: the cluster has no worker number ${n}"
    exit 1
  fi
  echo "$name"
}

# cmd_fault kill|pause <node>
cmd_fault() {
  local verb="$1" node="${2:-}" err start
  shift 2 || true
  [ -n "$node" ] && [ $# -eq 0 ] || usage
  guard "$node" || exit 1
  if ! wait_ready "$node" True "$FAULT_SECONDS"; then
    echo "FAIL: ${node} is not Ready within ${FAULT_SECONDS}s (Ready ${LAST_READY:-unknown}), so it is not faulted"
    exit 1
  fi
  start="$(date +%s)"
  case "$verb" in
    kill) err="$(docker kill -s KILL "$node" 2>&1 >/dev/null)" || {
      echo "FAIL: docker kill ${node} failed: ${err}"
      exit 1
    } ;;
    pause) err="$(docker pause "$node" 2>&1 >/dev/null)" || {
      echo "FAIL: docker pause ${node} failed: ${err}"
      exit 1
    } ;;
  esac
  if ! wait_ready "$node" notTrue "$FAULT_SECONDS"; then
    echo "FAIL: ${node} still reads Ready=True ${FAULT_SECONDS}s after docker ${verb}"
    exit 1
  fi
  case "$verb" in
    kill) echo "KILLED ${node}: Ready=${LAST_READY} after $(($(date +%s) - start))s" ;;
    pause) echo "PAUSED ${node}: Ready=${LAST_READY} after $(($(date +%s) - start))s" ;;
  esac
}

# restore_node <node>: bring the container back and wait for Ready; returns 1
# with the reason printed.
restore_node() {
  local node="$1" state err start
  guard "$node" || return 1
  state="$(docker_state "$node")"
  case "$state" in
    paused) err="$(docker unpause "$node" 2>&1 >/dev/null)" || {
      echo "FAIL: docker unpause ${node} failed: ${err}"
      return 1
    } ;;
    exited | created) err="$(docker start "$node" 2>&1 >/dev/null)" || {
      echo "FAIL: docker start ${node} failed: ${err}"
      return 1
    } ;;
    running) ;;
    *)
      echo "FAIL: ${node} is in Docker state ${state}, which restore cannot bring back"
      return 1
      ;;
  esac
  start="$(date +%s)"
  if ! wait_ready "$node" True "$RESTORE_SECONDS"; then
    echo "FAIL: ${node} is not Ready ${RESTORE_SECONDS}s after the restore (docker $(docker_state "$node"), Ready ${LAST_READY:-unknown})"
    return 1
  fi
  echo "RESTORED ${node}: docker ${state}, Ready=True after $(($(date +%s) - start))s"
}

cmd_restore() {
  [ $# -eq 1 ] && [ -n "$1" ] || usage
  restore_node "$1" || exit 1
}

cmd_restore_all() {
  [ $# -eq 0 ] || usage
  local node state ready failed=0 list
  # A for loop over a failing command substitution runs zero times and would
  # report every worker healthy, so the list is read first.
  if ! list="$(workers)" || [ -z "$list" ]; then
    echo "FAIL: cannot list the workers of this cluster"
    exit 1
  fi
  for node in $list; do
    state="$(docker_state "$node")"
    ready="$(ready_of "$node")"
    echo "${node} docker=${state} ready=${ready:-unknown}"
    if [ "$state" != running ] || [ "$ready" != True ]; then
      restore_node "$node" || failed=1
    fi
  done
  if [ "$failed" -ne 0 ]; then
    echo "FAIL: not every worker is back"
    exit 1
  fi
  echo "OK: every worker is running and Ready"
}

cmd_zones() {
  [ $# -eq 0 ] || usage
  local node n=0 list
  list="$(names "kubernetes.io/hostname")"
  for node in $list; do
    n=$((n + 1))
    kubectl label node "$node" "${ZONE_LABEL}=chaos-zone-${n}" --overwrite >/dev/null
    echo "OK: ${node} is in chaos-zone-${n}"
  done
}

cmd_zones_clear() {
  [ $# -eq 0 ] || usage
  local node list
  list="$(names "${ZONE_LABEL}")"
  for node in $list; do
    kubectl label node "$node" "${ZONE_LABEL}-" >/dev/null
  done
  echo "OK: no node carries ${ZONE_LABEL}"
}

cmd_open() {
  [ $# -eq 0 ] || usage
  # shellcheck disable=SC2046 # one name per word
  kubectl uncordon $(workers) >/dev/null
  echo "OK: workers $(workers | xargs) are schedulable"
}

cmd_close() {
  [ $# -le 1 ] || usage
  local list
  list="$(workers)"
  if [ $# -eq 1 ]; then
    if ! grep -qxF -- "$1" <<<"$list"; then
      echo "FAIL: $1 is no worker of this cluster"
      exit 1
    fi
    list="$1"
  fi
  # shellcheck disable=SC2086 # one name per word
  kubectl cordon $list >/dev/null
  echo "OK: $(xargs <<<"$list") cordoned"
}

if [ $# -lt 1 ]; then
  usage
fi
command="$1"
shift
case "$command" in
  prepare) cmd_prepare "$@" ;;
  victim) cmd_pick 1 "$@" ;;
  survivor) cmd_pick 2 "$@" ;;
  kill | pause) cmd_fault "$command" "$@" ;;
  restore) cmd_restore "$@" ;;
  restore-all) cmd_restore_all "$@" ;;
  zones) cmd_zones "$@" ;;
  zones-clear) cmd_zones_clear "$@" ;;
  open) cmd_open "$@" ;;
  close) cmd_close "$@" ;;
  *) usage ;;
esac
