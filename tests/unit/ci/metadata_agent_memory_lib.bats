#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/e2e/neutron/metadata-agent/memory-lib.sh, the measurement steps
# 9 to 11 of the metadata-agent suite take their readings through. Its
# MEMORY-READING lines are the input of the operator's memory default for the
# agent, and its checks are the guard that the default holds 32 networks:
#   - a phase prints the largest working set, memory.current less
#     inactive_file rounded up to MiB, with the peak, the OOM kills, the
#     haproxy count and the container's limit
#   - a phase fails on a working set above 90% of the limit, on an OOM kill,
#     on a container restart, on a haproxy count that never reaches the
#     networks, on a limit it cannot convert and on a cgroup it cannot read
#   - the reading comes from the agent pod that is not being deleted, and a
#     phase without one fails
#   - the probe payload is JSON that runs probe.sh with the mode and the range
# Usage: bin/bats tests/unit/ci/metadata_agent_memory_lib.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

LIB="$PROJECT_ROOT/tests/e2e/neutron/metadata-agent/memory-lib.sh"
MI=1048576

setup() {
  stub_on kubectl '*'
}

# The kubectl stub answers the calls the library makes, by the rules
# stub_kubectl declares from these variables before each run_phase:
#   STUB_PODS      the agent pods `get pods` lists
#   STUB_DELETING  the pod that carries a deletionTimestamp
#   STUB_LIMIT     the memory limit of the agent container
#   STUB_RESTARTS  its restart count
#   STUB_EXEC_RC   the exit code of the exec that reads the cgroup
#   STUB_CURRENT, STUB_INACTIVE, STUB_PEAK  bytes of memory.current, of
#                  inactive_file in memory.stat and of memory.peak
#   STUB_OOM_KILL  oom_kill of memory.events
#   STUB_HAPROXY   the number of haproxy processes in the container
# Every other call prints nothing and passes. A rule declared later wins, so
# each run_phase answers from the values of the moment.
stub_kubectl() {
  if [ "$STUB_EXEC_RC" -ne 0 ]; then
    stub_on kubectl 'exec */proc/self/cgroup*' --rc "$STUB_EXEC_RC" \
      --err "no cgroup v2 entry in /proc/self/cgroup"
  else
    stub_on kubectl 'exec */proc/self/cgroup*' --out-raw "$(cgroup_reading)"$'\n'
  fi
  stub_on kubectl '*restartCount*' --out-raw "$STUB_RESTARTS"
  stub_on kubectl '*limits.memory*' --out-raw "$STUB_LIMIT"
  # The pod being deleted is the second argument of its read.
  stub_on kubectl '*deletionTimestamp*'
  stub_on kubectl "get $STUB_DELETING *deletionTimestamp*" --out "2026-10-02T00:00:00Z"
  # shellcheck disable=SC2086 # one line per pod name
  stub_on kubectl 'get pods *' --out-raw "$(printf '%s\n' $STUB_PODS)"$'\n'
}

# cgroup_reading: what the exec that reads the cgroup prints.
cgroup_reading() {
  local i
  echo "@current"
  echo "$STUB_CURRENT"
  echo "@stat"
  echo "anon 4096"
  echo "inactive_file $STUB_INACTIVE"
  echo "active_file 8192"
  echo "@events"
  echo "oom 0"
  echo "oom_kill $STUB_OOM_KILL"
  echo "oom_group_kill 0"
  echo "@peak"
  echo "$STUB_PEAK"
  echo "@comm"
  echo "neutron-ovn-met"
  echo "privsep-helper"
  i=0
  while [ "$i" -lt "$STUB_HAPROXY" ]; do
    echo "haproxy"
    i=$((i + 1))
  done
  echo "cat"
}

# stub_defaults
# An agent with 32 networks well inside a 2Gi limit: 601Mi of working set (one
# byte above 600Mi, which rounds up) and a peak of 800Mi.
stub_defaults() {
  STUB_PODS="pod/agent-old pod/agent-new"
  STUB_DELETING="pod/agent-old"
  STUB_LIMIT="2Gi"
  STUB_RESTARTS=0
  STUB_EXEC_RC=0
  STUB_CURRENT=$((700 * MI + 1))
  STUB_INACTIVE=$((100 * MI))
  STUB_PEAK=$((800 * MI))
  STUB_OOM_KILL=0
  STUB_HAPROXY=32
}

# run_phase <release> <networks> <timeout-seconds>
# Sources the library the way a step script does and runs one phase against the
# stub. Echoes the phase's output and returns its exit code; a test runs it
# through run.
#
# phase waits on bash's SECONDS, so the stand-in for sleep moves that clock
# instead of the wall clock.
run_phase() {
  stub_kubectl
  (
    PATH="$STUB_DIR/bin:$PATH"
    NAMESPACE=openstack
    export PATH NAMESPACE
    set -euo pipefail
    # shellcheck source=tests/e2e/neutron/metadata-agent/memory-lib.sh
    source "$LIB"
    sleep() { SECONDS=$((SECONDS + $1)); }
    agent_pod
    phase "$@"
  ) 2>&1
}

# --- Test 1: the MEMORY-READING line ---
@test "a phase prints the working set, the peak, the OOM kills, the haproxy count and the limit" {
  stub_defaults
  run run_phase 2025.2 32 600
  assert_eq "a phase inside the limit passes" "0" "$status"
  assert_eq "the line carries memory.current less inactive_file, rounded up to MiB" \
    "MEMORY-READING release=2025.2 networks=32 working_set_mi=601 peak_mi=800 oom_kill=0 haproxy=32 limit_mi=2048" \
    "$output"
}

# --- Test 2: the 90% check ---
@test "a phase fails on a working set above 90% of the container's memory limit" {
  stub_defaults
  STUB_LIMIT="768Mi"
  STUB_INACTIVE=0

  # 90% of 768Mi is 691.2Mi.
  STUB_CURRENT=$((691 * MI))
  run run_phase 2025.2 32 600
  assert_eq "691Mi of a 768Mi limit passes" "0" "$status"
  assert_contains "the limit is read in Mi" "$output" "working_set_mi=691 "
  assert_contains "the line names the 768Mi limit" "$output" "limit_mi=768"

  STUB_CURRENT=$((692 * MI))
  run run_phase 2025.2 32 600
  assert_nonzero_exit "692Mi of a 768Mi limit fails" "$status"
  assert_contains "the failure names the working set and the limit" "$output" \
    "FAIL: working set 692Mi is above 90% of the 768Mi limit"
}

# --- Test 3: the other checks of a phase ---
@test "a phase fails on an OOM kill, a restart, missing haproxy processes, an unknown limit and an unreadable cgroup" {
  stub_defaults
  STUB_OOM_KILL=1
  run run_phase 2025.2 32 600
  assert_nonzero_exit "an OOM kill in the cgroup fails the phase" "$status"
  assert_contains "the failure counts the OOM kills" "$output" \
    "FAIL: pod/agent-new counts 1 OOM kill(s) in its cgroup"

  stub_defaults
  STUB_RESTARTS=2
  run run_phase 2025.2 32 600
  assert_nonzero_exit "a restarted agent container fails the phase" "$status"
  assert_contains "the failure counts the restarts" "$output" \
    "FAIL: pod/agent-new container metadata-agent restarted 2 time(s)"

  stub_defaults
  STUB_HAPROXY=31
  run run_phase 2026.1 32 60
  assert_nonzero_exit "an agent that never runs one haproxy per network fails the phase" "$status"
  assert_contains "the reading is printed before the verdict" "$output" \
    "MEMORY-READING release=2026.1 networks=32 working_set_mi=601 peak_mi=800 oom_kill=0 haproxy=31 limit_mi=2048"
  assert_contains "the failure names the count and the timeout" "$output" \
    "FAIL: the agent runs 31 of 32 haproxy processes after 60s"

  stub_defaults
  STUB_LIMIT="2G"
  run run_phase 2025.2 32 600
  assert_nonzero_exit "a limit that is neither Mi nor Gi fails the phase" "$status"
  assert_contains "the failure names the limit" "$output" "FAIL: unexpected memory limit 2G"

  stub_defaults
  STUB_EXEC_RC=1
  run run_phase 2025.2 32 600
  assert_nonzero_exit "a cgroup the exec cannot read fails the phase" "$status"
  assert_contains "the container's own message reaches the output" "$output" \
    "no cgroup v2 entry in /proc/self/cgroup"
  assert_contains "the failure names the pod" "$output" \
    "FAIL: cannot read the cgroup of pod/agent-new"
  assert_not_contains "no reading is printed from an unreadable cgroup" "$output" "MEMORY-READING"
}

# --- Test 4: which pod is read ---
@test "a phase reads the agent pod that is not being deleted, and fails without one" {
  # The restart check names the pod the phase read.
  stub_defaults
  STUB_PODS="pod/agent-new pod/agent-old"
  STUB_RESTARTS=1
  run run_phase 2025.2 32 600
  assert_contains "the pod being deleted is skipped when it is listed last" "$output" \
    "FAIL: pod/agent-new container"

  stub_defaults
  STUB_PODS=""
  run run_phase 2025.2 32 600
  assert_nonzero_exit "a DaemonSet without a pod fails the phase" "$status"
  assert_eq "the failure says no pod was found" \
    "FAIL: no running pod of the metadata-agent DaemonSet found" "$output"
}

# --- Test 5: the probe pod's payload ---
@test "probe_overrides prints JSON that runs probe.sh with the mode and the range" {
  local out
  out="$(
    # shellcheck source=tests/e2e/neutron/metadata-agent/memory-lib.sh
    source "$LIB"
    # shellcheck disable=SC2034 # read by probe_overrides
    IMG="registry.example/ovn:25.03"
    # shellcheck disable=SC2034 # read by probe_overrides
    NB="ssl:nb.openstack.svc:6641"
    probe_overrides neutron-agent-memory-probe-cleanup nb-cleanup 1 "$NETWORKS" |
      jq -c '.spec.containers[0] | [.name, .image, .command, .env[0].value]'
  )"
  assert_eq "the container runs probe.sh over all 32 networks against the Northbound address" \
    '["neutron-agent-memory-probe-cleanup","registry.example/ovn:25.03",["bash","/probe/probe.sh","nb-cleanup","1","32"],"ssl:nb.openstack.svc:6641"]' \
    "$out"
}

# --- Run ---
