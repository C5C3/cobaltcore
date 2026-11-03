#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the node faults of the multinode chaos leg
# (tests/e2e-chaos/lib/nodes.sh, #1392) against a stub kubectl and a stub
# docker:
#   1. The guard refuses the control-plane node, by its container's role
#      label and by its Node label alone, a container without the
#      io.x-k8s.kind.cluster label, a name without a container and a name that
#      is no worker, before any docker kill or pause.
#   2. kill and pause without a node, or with an empty one, exit 2 with the
#      usage; an unknown command does too.
#   3. kill waits for Ready=True, runs docker kill -s KILL and waits for
#      Ready!=True; a failing docker kill exits 1 naming the Docker error, a
#      node that is not Ready is not faulted, and one that stays Ready fails.
#   4. prepare refuses a cluster with fewer than two workers and changes no
#      taint; with two it removes the control-plane taint, cordons both
#      workers, and a second run finds the taint gone and passes. Any other
#      taint error exits 1 and cordons nothing.
#   5. restore unpauses a paused container, starts an exited one and leaves a
#      running one alone, each ending in a Ready wait; a node that never turns
#      Ready fails after the restore wait. A Docker state it cannot handle and
#      a failing docker pause, unpause or start exit 1 with the reason.
#   6. restore-all prints every worker's state, restores the one that is not
#      running and Ready, and does nothing when every worker is Ready. A
#      worker list it cannot read, or an empty one, exits 1 instead of OK.
#   7. victim and survivor follow the sorted names, zones labels every node in
#      name order and zones-clear unlabels each zoned node, both fail on an
#      unreadable node list, open and close cordon the workers, and close
#      refuses the control plane.
#
# Usage: bash tests/unit/ci/chaos_nodes_lib_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LIB="$PROJECT_ROOT/tests/e2e-chaos/lib/nodes.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
STATE="$TMP_DIR/state"
mkdir -p "$TMP_DIR/bin"

# The stub kubectl logs every call to $STATE/kubectl.log. The workers are
# STUB_WORKERS, the control plane STUB_CP and the nodes with a zone label
# STUB_ZONED, space-separated; listing nodes fails when STUB_NODES_FAIL is set.
# A node's Ready condition is read from $STATE/ready-<node>, one status per
# line: each read takes the first line, and the last one stays (True without a
# file). A taint removal fails with "not found" when STUB_TAINT_GONE is set,
# and with a Forbidden error when STUB_TAINT_FAIL is.
cat >"$TMP_DIR/bin/kubectl" <<'SH'
#!/bin/bash
echo "$*" >>"$STATE/kubectl.log"
case "$1 $2" in
  "get nodes")
    if [ -n "${STUB_NODES_FAIL:-}" ]; then
      echo "Unable to connect to the server: net/http: TLS handshake timeout" >&2
      exit 1
    fi
    case "$4" in
      '!node-role.kubernetes.io/control-plane') list="${STUB_WORKERS:-}" ;;
      'node-role.kubernetes.io/control-plane') list="${STUB_CP:-}" ;;
      'kubernetes.io/hostname') list="${STUB_CP:-} ${STUB_WORKERS:-}" ;;
      'topology.kubernetes.io/zone') list="${STUB_ZONED:-}" ;;
      *) list="" ;;
    esac
    for n in $list; do echo "node/$n"; done
    ;;
  "get node")
    f="$STATE/ready-$3"
    if [ -f "$f" ]; then
      head -1 "$f"
      if [ "$(wc -l <"$f" | tr -d ' ')" -gt 1 ]; then
        tail -n +2 "$f" >"$f.next" && mv "$f.next" "$f"
      fi
    else
      echo True
    fi
    ;;
  "taint nodes")
    if [ -n "${STUB_TAINT_GONE:-}" ]; then
      echo 'error: taint "node-role.kubernetes.io/control-plane:NoSchedule" not found' >&2
      exit 1
    fi
    if [ -n "${STUB_TAINT_FAIL:-}" ]; then
      echo 'Error from server (Forbidden): nodes "cp" is forbidden' >&2
      exit 1
    fi
    ;;
esac
exit 0
SH

# The stub docker logs every call to $STATE/docker.log. inspect answers the
# kind label from $STATE/label-<node> (cobaltcore without a file), the role
# label from $STATE/role-<node> (worker) and the state from $STATE/state-<node>
# (running); a node with a $STATE/gone-<node> file has no container. kill,
# pause, unpause and start move the state; the one STUB_DOCKER_FAIL names
# fails instead.
cat >"$TMP_DIR/bin/docker" <<'SH'
#!/bin/bash
echo "$*" >>"$STATE/docker.log"
if [ "$1" = "${STUB_DOCKER_FAIL:-}" ]; then
  echo "Error response from daemon: cannot $1 container: ${*: -1}: container is not running" >&2
  exit 1
fi
case "$1" in
  inspect)
    node="$4"
    if [ -e "$STATE/gone-$node" ]; then
      echo "Error: No such object: $node" >&2
      exit 1
    fi
    case "$3" in
      *io.x-k8s.kind.cluster*) cat "$STATE/label-$node" 2>/dev/null || echo cobaltcore ;;
      *io.x-k8s.kind.role*) cat "$STATE/role-$node" 2>/dev/null || echo worker ;;
      *State.Status*) cat "$STATE/state-$node" 2>/dev/null || echo running ;;
    esac
    ;;
  kill) echo exited >"$STATE/state-$4" ;;
  pause) echo paused >"$STATE/state-$2" ;;
  unpause | start) echo running >"$STATE/state-$2" ;;
esac
exit 0
SH
chmod +x "$TMP_DIR/bin/kubectl" "$TMP_DIR/bin/docker"

# reset: empty the stub state and the logs.
reset() {
  rm -rf "$STATE"
  mkdir -p "$STATE"
  : >"$STATE/kubectl.log"
  : >"$STATE/docker.log"
}

# ready <node> <status...>: the node's Ready reads, in order.
ready() {
  local node="$1"
  shift
  printf '%s\n' "$@" >"$STATE/ready-$node"
}

# run_nodes [VAR=value...] -- <args...>: run nodes.sh with the stubs first on
# PATH, a cluster of cp, w1 and w2, and short waits; OUT and RC hold the
# result.
run_nodes() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  RC=0
  OUT="$(env PATH="$TMP_DIR/bin:$PATH" STATE="$STATE" STUB_CP=cp STUB_WORKERS="w1 w2" \
    CHAOS_NODES_FAULT_SECONDS=2 CHAOS_NODES_RESTORE_SECONDS=2 \
    CHAOS_NODES_PREPARE_SECONDS=1 CHAOS_NODES_POLL_SECONDS=1 \
    ${envs[@]+"${envs[@]}"} bash "$LIB" "$@" 2>&1)" || RC=$?
}

docker_log() {
  cat "$STATE/docker.log"
}

kubectl_log() {
  cat "$STATE/kubectl.log"
}

# --- Test 1 ---------------------------------------------------------------
test_the_guard_refuses() {
  echo "Test: kill and pause refuse the control plane and anything outside kind"
  reset
  echo control-plane >"$STATE/role-cp"
  run_nodes -- kill cp
  assert_eq "kill of the control plane exits 1" "1" "$RC"
  assert_contains "it says why" "$OUT" "refusing to fault cp: it is the control plane"
  assert_not_contains "and no docker kill ran" "$(docker_log)" "kill"

  reset
  run_nodes -- pause cp
  assert_eq "a control plane known by its Node label alone is refused too" "1" "$RC"
  assert_contains "with the same reason" "$OUT" "refusing to fault cp: it is the control plane"
  assert_not_contains "and no docker pause ran" "$(docker_log)" "pause"

  reset
  : >"$STATE/label-w1"
  run_nodes -- pause w1
  assert_eq "pause of a container without the kind label exits 1" "1" "$RC"
  assert_contains "it names the label" "$OUT" \
    "refusing to fault w1: its container carries no io.x-k8s.kind.cluster label"
  assert_not_contains "and no docker pause ran" "$(docker_log)" "pause"

  reset
  echo '<no value>' >"$STATE/label-w1"
  run_nodes -- kill w1
  assert_contains "a <no value> label is no label" "$OUT" \
    "refusing to fault w1: its container carries no io.x-k8s.kind.cluster label"

  reset
  touch "$STATE/gone-db"
  run_nodes -- kill db
  assert_eq "a name without a container exits 1" "1" "$RC"
  assert_contains "it is refused by the label check" "$OUT" "refusing to fault db: its container carries no"

  reset
  run_nodes -- kill other-cluster-worker
  assert_eq "a kind container that is no worker of this cluster exits 1" "1" "$RC"
  assert_contains "it says so" "$OUT" "refusing to fault other-cluster-worker: it is no worker Node of this cluster"
  assert_not_contains "and no docker kill ran" "$(docker_log)" "kill"

  reset
  echo exited >"$STATE/state-cp"
  echo control-plane >"$STATE/role-cp"
  run_nodes -- restore cp
  assert_eq "restore refuses the control plane too" "1" "$RC"
  assert_not_contains "and starts nothing" "$(docker_log)" "start"
}

# --- Test 2 ---------------------------------------------------------------
test_usage_errors() {
  echo "Test: a missing or empty node and an unknown command exit 2"
  reset
  run_nodes -- kill
  assert_eq "bare kill exits 2" "2" "$RC"
  assert_contains "with the usage" "$OUT" "usage: nodes.sh prepare"
  run_nodes -- kill ''
  assert_eq "kill '' exits 2" "2" "$RC"
  run_nodes -- pause
  assert_eq "bare pause exits 2" "2" "$RC"
  run_nodes -- restore
  assert_eq "bare restore exits 2" "2" "$RC"
  run_nodes -- kill w1 w2
  assert_eq "two nodes exit 2" "2" "$RC"
  run_nodes -- reboot w1
  assert_eq "an unknown command exits 2" "2" "$RC"
  run_nodes --
  assert_eq "no command exits 2" "2" "$RC"
  assert_eq "no docker call ran" "" "$(docker_log)"
}

# --- Test 3 ---------------------------------------------------------------
test_kill_and_pause() {
  echo "Test: kill waits for Ready, kills the container and waits for NotReady"
  reset
  ready w1 True Unknown
  run_nodes -- kill w1
  assert_eq "it exits 0" "0" "$RC"
  assert_contains "it reports the node's new condition" "$OUT" "KILLED w1: Ready=Unknown after"
  assert_contains "it sent SIGKILL" "$(docker_log)" "kill -s KILL w1"
  assert_eq "it read Ready before and after the kill" "2" \
    "$(grep -c '^get node w1 ' "$STATE/kubectl.log")"

  reset
  ready w1 True Unknown
  run_nodes -- pause w1
  assert_eq "pause exits 0" "0" "$RC"
  assert_contains "it reports the frozen node" "$OUT" "PAUSED w1: Ready=Unknown after"
  assert_contains "it paused the container" "$(docker_log)" "pause w1"

  reset
  run_nodes STUB_DOCKER_FAIL=kill -- kill w1
  assert_eq "a failing docker kill exits 1" "1" "$RC"
  assert_contains "it names the Docker error" "$OUT" \
    "FAIL: docker kill w1 failed: Error response from daemon: cannot kill container: w1: container is not running"

  reset
  run_nodes STUB_DOCKER_FAIL=pause -- pause w1
  assert_eq "a failing docker pause exits 1" "1" "$RC"
  assert_contains "it names the Docker error" "$OUT" \
    "FAIL: docker pause w1 failed: Error response from daemon: cannot pause container: w1"

  reset
  ready w1 False
  run_nodes -- kill w1
  assert_eq "a node that is not Ready exits 1" "1" "$RC"
  assert_contains "and is not faulted" "$OUT" "FAIL: w1 is not Ready within 2s (Ready False), so it is not faulted"
  assert_not_contains "no docker kill ran" "$(docker_log)" "kill"

  reset
  ready w1 True '' True
  run_nodes -- pause w1
  assert_eq "an unreadable condition is not taken for NotReady" "1" "$RC"
  assert_contains "the node that stays Ready fails the fault" "$OUT" \
    "FAIL: w1 still reads Ready=True 2s after docker pause"
}

# --- Test 4 ---------------------------------------------------------------
test_prepare() {
  echo "Test: prepare needs two workers, then untaints the control plane and cordons the workers"
  reset
  run_nodes STUB_WORKERS= -- prepare
  assert_eq "a single-node cluster exits 1" "1" "$RC"
  assert_contains "it counts the workers" "$OUT" "FAIL: the multinode leg needs two workers, found 0"
  assert_not_contains "it changes no taint" "$(kubectl_log)" "taint"
  assert_not_contains "and cordons nothing" "$(kubectl_log)" "cordon"

  reset
  ready w2 False
  run_nodes -- prepare
  assert_eq "a worker that is not Ready exits 1" "1" "$RC"
  assert_contains "it names the node" "$OUT" "FAIL: not Ready within 1s: w2"
  assert_not_contains "it changes no taint" "$(kubectl_log)" "taint"

  reset
  run_nodes -- prepare
  assert_eq "two Ready workers pass" "0" "$RC"
  assert_contains "it removes the control-plane taint" "$(kubectl_log)" \
    "taint nodes cp node-role.kubernetes.io/control-plane:NoSchedule-"
  assert_contains "it cordons both workers" "$(kubectl_log)" "cordon w1 w2"
  assert_contains "it says so" "$OUT" "OK: cp is schedulable, workers w1 w2 are cordoned"

  reset
  run_nodes STUB_TAINT_GONE=1 -- prepare
  assert_eq "a second run finds the taint gone and passes" "0" "$RC"

  reset
  run_nodes STUB_TAINT_FAIL=1 -- prepare
  assert_eq "any other taint error exits 1" "1" "$RC"
  assert_contains "it names the error" "$OUT" \
    "FAIL: removing the control-plane taint of cp failed: Error from server (Forbidden)"
  assert_not_contains "and cordons nothing" "$(kubectl_log)" "cordon"
}

# --- Test 5 ---------------------------------------------------------------
test_restore() {
  local state want
  for state in paused exited running; do
    echo "Test: restore of a ${state} container"
    reset
    echo "$state" >"$STATE/state-w1"
    ready w1 False True
    run_nodes -- restore w1
    assert_eq "it exits 0" "0" "$RC"
    assert_contains "it waits for Ready" "$OUT" "RESTORED w1: docker ${state}, Ready=True after"
    case "$state" in
      paused) want="unpause w1" ;;
      exited) want="start w1" ;;
      running) want="" ;;
    esac
    if [ -n "$want" ]; then
      assert_contains "it runs docker ${want%% *}" "$(docker_log)" "$want"
    else
      assert_not_contains "it starts nothing" "$(docker_log)" "start"
      assert_not_contains "it unpauses nothing" "$(docker_log)" "unpause"
    fi
  done

  echo "Test: restore of a node that never turns Ready fails after the wait"
  reset
  echo exited >"$STATE/state-w1"
  ready w1 False
  run_nodes -- restore w1
  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it names the wait, the container and the condition" "$OUT" \
    "FAIL: w1 is not Ready 2s after the restore (docker running, Ready False)"

  for state in dead restarting removing; do
    echo "Test: restore of a ${state} container fails without a docker call"
    reset
    echo "$state" >"$STATE/state-w1"
    run_nodes -- restore w1
    assert_eq "it exits 1" "1" "$RC"
    assert_contains "it names the state" "$OUT" \
      "FAIL: w1 is in Docker state ${state}, which restore cannot bring back"
    assert_not_contains "it starts nothing" "$(docker_log)" "start"
    assert_not_contains "it unpauses nothing" "$(docker_log)" "unpause"
  done

  echo "Test: a failing docker unpause or start fails the restore"
  reset
  echo paused >"$STATE/state-w1"
  run_nodes STUB_DOCKER_FAIL=unpause -- restore w1
  assert_eq "a failing unpause exits 1" "1" "$RC"
  assert_contains "it names the Docker error" "$OUT" \
    "FAIL: docker unpause w1 failed: Error response from daemon: cannot unpause container: w1"
  reset
  echo exited >"$STATE/state-w1"
  run_nodes STUB_DOCKER_FAIL=start -- restore w1
  assert_eq "a failing start exits 1" "1" "$RC"
  assert_contains "it names the Docker error" "$OUT" \
    "FAIL: docker start w1 failed: Error response from daemon: cannot start container: w1"
}

# --- Test 6 ---------------------------------------------------------------
test_restore_all() {
  echo "Test: restore-all does nothing when every worker is Ready"
  reset
  run_nodes -- restore-all
  assert_eq "it exits 0" "0" "$RC"
  assert_contains "it prints the first worker" "$OUT" "w1 docker=running ready=True"
  assert_contains "and the second" "$OUT" "w2 docker=running ready=True"
  assert_not_contains "it starts nothing" "$(docker_log)" "start"
  assert_not_contains "it unpauses nothing" "$(docker_log)" "unpause"

  echo "Test: restore-all brings back a paused worker"
  reset
  echo paused >"$STATE/state-w2"
  ready w2 Unknown Unknown True
  run_nodes -- restore-all
  assert_eq "it exits 0" "0" "$RC"
  assert_contains "it prints the paused worker" "$OUT" "w2 docker=paused ready=Unknown"
  assert_contains "it unpauses it" "$(docker_log)" "unpause w2"
  assert_contains "it ends Ready" "$OUT" "OK: every worker is running and Ready"

  echo "Test: restore-all exits 1 for a worker that does not come back, after trying every one"
  reset
  echo exited >"$STATE/state-w1"
  echo paused >"$STATE/state-w2"
  ready w1 Unknown
  ready w2 Unknown Unknown True
  run_nodes -- restore-all
  assert_eq "it exits 1" "1" "$RC"
  assert_contains "it names the worker" "$OUT" "FAIL: w1 is not Ready 2s after the restore"
  assert_contains "and still restored the other one" "$(docker_log)" "unpause w2"

  echo "Test: restore-all fails when it cannot list the workers"
  reset
  run_nodes STUB_NODES_FAIL=1 -- restore-all
  assert_eq "an unreadable node list exits 1" "1" "$RC"
  assert_contains "it says so" "$OUT" "FAIL: cannot list the workers of this cluster"
  assert_not_contains "and reports no healthy worker" "$OUT" "OK: every worker is running and Ready"
  reset
  run_nodes STUB_WORKERS= -- restore-all
  assert_eq "a cluster without workers exits 1" "1" "$RC"
  assert_contains "with the same reason" "$OUT" "FAIL: cannot list the workers of this cluster"
}

# --- Test 7 ---------------------------------------------------------------
test_names_zones_and_cordons() {
  echo "Test: victim and survivor follow the sorted worker names"
  reset
  run_nodes STUB_WORKERS="wb wa" -- victim
  assert_eq "the victim is the first name" "wa" "$OUT"
  run_nodes STUB_WORKERS="wb wa" -- survivor
  assert_eq "the survivor is the second" "wb" "$OUT"
  run_nodes STUB_WORKERS="wa" -- survivor
  assert_eq "one worker has no survivor" "1" "$RC"
  assert_contains "it says so" "$OUT" "FAIL: the cluster has no worker number 2"

  echo "Test: zones labels every node in name order"
  reset
  run_nodes -- zones
  assert_eq "it exits 0" "0" "$RC"
  assert_contains "cp is zone 1" "$(kubectl_log)" "label node cp topology.kubernetes.io/zone=chaos-zone-1 --overwrite"
  assert_contains "w1 is zone 2" "$(kubectl_log)" "label node w1 topology.kubernetes.io/zone=chaos-zone-2 --overwrite"
  assert_contains "w2 is zone 3" "$(kubectl_log)" "label node w2 topology.kubernetes.io/zone=chaos-zone-3 --overwrite"
  reset
  run_nodes STUB_NODES_FAIL=1 -- zones
  assert_eq "zones fails on an unreadable node list" "1" "$RC"
  assert_not_contains "and labels nothing" "$(kubectl_log)" "label node"

  echo "Test: zones-clear removes the zone label from every zoned node"
  reset
  run_nodes STUB_ZONED="cp w2" -- zones-clear
  assert_eq "it exits 0" "0" "$RC"
  assert_contains "it unlabels cp" "$(kubectl_log)" "label node cp topology.kubernetes.io/zone-"
  assert_contains "and w2" "$(kubectl_log)" "label node w2 topology.kubernetes.io/zone-"
  assert_not_contains "and leaves w1 alone" "$(kubectl_log)" "label node w1"
  assert_contains "it says so" "$OUT" "OK: no node carries topology.kubernetes.io/zone"
  reset
  run_nodes STUB_NODES_FAIL=1 -- zones-clear
  assert_eq "zones-clear fails on an unreadable node list" "1" "$RC"
  assert_not_contains "instead of reporting no zone" "$OUT" "OK: no node carries"

  echo "Test: open and close cordon the workers, and close refuses the control plane"
  reset
  run_nodes -- open
  assert_contains "open uncordons both workers" "$(kubectl_log)" "uncordon w1 w2"
  run_nodes -- close
  assert_contains "close cordons both" "$(kubectl_log)" "cordon w1 w2"
  reset
  run_nodes -- close w2
  assert_contains "close <node> cordons that one" "$(kubectl_log)" "cordon w2"
  assert_not_contains "and not the other" "$(kubectl_log)" "cordon w1"
  reset
  run_nodes -- close cp
  assert_eq "close of the control plane exits 1" "1" "$RC"
  assert_contains "it says why" "$OUT" "FAIL: cp is no worker of this cluster"
  assert_not_contains "and cordons nothing" "$(kubectl_log)" "cordon"
}

# --- Run ---
test_the_guard_refuses
test_usage_errors
test_kill_and_pause
test_prepare
test_restore
test_restore_all
test_names_zones_and_cordons

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
