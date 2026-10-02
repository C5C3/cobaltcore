#!/bin/bash
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
# Usage: bash tests/unit/ci/metadata_agent_memory_lib_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

LIB="$PROJECT_ROOT/tests/e2e/neutron/metadata-agent/memory-lib.sh"
MI=1048576

STUBS="$(mktemp -d)"
trap 'rm -rf "$STUBS"' EXIT

# The kubectl stub answers the calls the library makes and reads its behaviour
# from the environment:
#   STUB_PODS      the agent pods `get pods` lists
#   STUB_DELETING  the pod that carries a deletionTimestamp
#   STUB_LIMIT     the memory limit of the agent container
#   STUB_RESTARTS  its restart count
#   STUB_EXEC_RC   the exit code of the exec that reads the cgroup
#   STUB_CURRENT, STUB_INACTIVE, STUB_PEAK  bytes of memory.current, of
#                  inactive_file in memory.stat and of memory.peak
#   STUB_OOM_KILL  oom_kill of memory.events
#   STUB_HAPROXY   the number of haproxy processes in the container
cat >"$STUBS/kubectl" <<'STUB'
#!/bin/bash
case "$*" in
  "get pods "*)
    # shellcheck disable=SC2086 # one line per pod name
    printf '%s\n' $STUB_PODS
    ;;
  *deletionTimestamp*)
    if [ "$2" = "$STUB_DELETING" ]; then echo "2026-10-02T00:00:00Z"; fi
    ;;
  *limits.memory*) printf '%s' "$STUB_LIMIT" ;;
  *restartCount*) printf '%s' "$STUB_RESTARTS" ;;
  "exec "*/proc/self/cgroup*)
    if [ "$STUB_EXEC_RC" -ne 0 ]; then
      echo "no cgroup v2 entry in /proc/self/cgroup" >&2
      exit "$STUB_EXEC_RC"
    fi
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
    ;;
esac
exit 0
STUB
chmod +x "$STUBS/kubectl"

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
# stub. Echoes the phase's output and returns its exit code.
#
# phase waits on bash's SECONDS, so the stand-in for sleep moves that clock
# instead of the wall clock.
run_phase() {
  (
    PATH="$STUBS:$PATH"
    NAMESPACE=openstack
    export PATH NAMESPACE STUB_PODS STUB_DELETING STUB_LIMIT STUB_RESTARTS \
      STUB_EXEC_RC STUB_CURRENT STUB_INACTIVE STUB_PEAK STUB_OOM_KILL STUB_HAPROXY
    set -euo pipefail
    # shellcheck source=tests/e2e/neutron/metadata-agent/memory-lib.sh
    source "$LIB"
    sleep() { SECONDS=$((SECONDS + $1)); }
    agent_pod
    phase "$@"
  ) 2>&1
}

# --- Test 1: the MEMORY-READING line ---
test_a_phase_prints_the_reading() {
  echo "Test: a phase prints the working set, the peak, the OOM kills, the haproxy count and the limit"

  local out rc
  stub_defaults
  out="$(run_phase 2025.2 32 600)"
  rc=$?
  assert_eq "a phase inside the limit passes" "0" "$rc"
  assert_eq "the line carries memory.current less inactive_file, rounded up to MiB" \
    "MEMORY-READING release=2025.2 networks=32 working_set_mi=601 peak_mi=800 oom_kill=0 haproxy=32 limit_mi=2048" \
    "$out"
}

# --- Test 2: the 90% check ---
test_a_phase_fails_above_90_percent_of_the_limit() {
  echo "Test: a phase fails on a working set above 90% of the container's memory limit"

  local out rc
  stub_defaults
  STUB_LIMIT="768Mi"
  STUB_INACTIVE=0

  # 90% of 768Mi is 691.2Mi.
  STUB_CURRENT=$((691 * MI))
  out="$(run_phase 2025.2 32 600)"
  rc=$?
  assert_eq "691Mi of a 768Mi limit passes" "0" "$rc"
  assert_contains "the limit is read in Mi" "$out" "working_set_mi=691 "
  assert_contains "the line names the 768Mi limit" "$out" "limit_mi=768"

  STUB_CURRENT=$((692 * MI))
  out="$(run_phase 2025.2 32 600)"
  rc=$?
  assert_nonzero_exit "692Mi of a 768Mi limit fails" "$rc"
  assert_contains "the failure names the working set and the limit" "$out" \
    "FAIL: working set 692Mi is above 90% of the 768Mi limit"
}

# --- Test 3: the other checks of a phase ---
test_a_phase_fails_on_an_agent_that_did_not_hold() {
  echo "Test: a phase fails on an OOM kill, a restart, missing haproxy processes, an unknown limit and an unreadable cgroup"

  local out rc

  stub_defaults
  STUB_OOM_KILL=1
  out="$(run_phase 2025.2 32 600)"
  rc=$?
  assert_nonzero_exit "an OOM kill in the cgroup fails the phase" "$rc"
  assert_contains "the failure counts the OOM kills" "$out" \
    "FAIL: pod/agent-new counts 1 OOM kill(s) in its cgroup"

  stub_defaults
  STUB_RESTARTS=2
  out="$(run_phase 2025.2 32 600)"
  rc=$?
  assert_nonzero_exit "a restarted agent container fails the phase" "$rc"
  assert_contains "the failure counts the restarts" "$out" \
    "FAIL: pod/agent-new container metadata-agent restarted 2 time(s)"

  stub_defaults
  STUB_HAPROXY=31
  out="$(run_phase 2026.1 32 60)"
  rc=$?
  assert_nonzero_exit "an agent that never runs one haproxy per network fails the phase" "$rc"
  assert_contains "the reading is printed before the verdict" "$out" \
    "MEMORY-READING release=2026.1 networks=32 working_set_mi=601 peak_mi=800 oom_kill=0 haproxy=31 limit_mi=2048"
  assert_contains "the failure names the count and the timeout" "$out" \
    "FAIL: the agent runs 31 of 32 haproxy processes after 60s"

  stub_defaults
  STUB_LIMIT="2G"
  out="$(run_phase 2025.2 32 600)"
  rc=$?
  assert_nonzero_exit "a limit that is neither Mi nor Gi fails the phase" "$rc"
  assert_contains "the failure names the limit" "$out" "FAIL: unexpected memory limit 2G"

  stub_defaults
  STUB_EXEC_RC=1
  out="$(run_phase 2025.2 32 600)"
  rc=$?
  assert_nonzero_exit "a cgroup the exec cannot read fails the phase" "$rc"
  assert_contains "the container's own message reaches the output" "$out" \
    "no cgroup v2 entry in /proc/self/cgroup"
  assert_contains "the failure names the pod" "$out" \
    "FAIL: cannot read the cgroup of pod/agent-new"
  assert_not_contains "no reading is printed from an unreadable cgroup" "$out" "MEMORY-READING"
}

# --- Test 4: which pod is read ---
test_a_phase_reads_the_pod_that_is_not_being_deleted() {
  echo "Test: a phase reads the agent pod that is not being deleted, and fails without one"

  local out rc

  # The restart check names the pod the phase read.
  stub_defaults
  STUB_PODS="pod/agent-new pod/agent-old"
  STUB_RESTARTS=1
  out="$(run_phase 2025.2 32 600)"
  assert_contains "the pod being deleted is skipped when it is listed last" "$out" \
    "FAIL: pod/agent-new container"

  stub_defaults
  STUB_PODS=""
  out="$(run_phase 2025.2 32 600)"
  rc=$?
  assert_nonzero_exit "a DaemonSet without a pod fails the phase" "$rc"
  assert_eq "the failure says no pod was found" \
    "FAIL: no running pod of the metadata-agent DaemonSet found" "$out"
}

# --- Test 5: the probe pod's payload ---
test_the_probe_payload_runs_the_probe_script() {
  echo "Test: probe_overrides prints JSON that runs probe.sh with the mode and the range"

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
test_a_phase_prints_the_reading
test_a_phase_fails_above_90_percent_of_the_limit
test_a_phase_fails_on_an_agent_that_did_not_hold
test_a_phase_reads_the_pod_that_is_not_being_deleted
test_the_probe_payload_runs_the_probe_script

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
