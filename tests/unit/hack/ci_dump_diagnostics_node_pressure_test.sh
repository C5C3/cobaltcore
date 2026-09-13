#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the node-pressure section of hack/ci-dump-diagnostics.sh.
#
# A kind node that runs out of RAM leaves almost nothing in the pod table:
# the kernel OOM killer restarts the largest BestEffort container in place
# and `kubectl get pods` shows a restart count. CI run 34718789784 lost the
# shared MariaDB five times that way before the cause was read out of a pod's
# previous log. The dump must therefore emit, always and without failing the
# job:
#
#   - the node's Capacity / Allocatable and its Allocated resources block;
#   - only the pods whose containers restarted, with the last termination
#     reason and QoS class, never the zero-restart rows;
#   - the per-pod memory working set from the kubelet summary API, sorted
#     largest first under the node line;
#   - the kernel's OOM lines from inside the kind node container, with an
#     explicit line when dmesg is denied, when it holds no OOM line, and when
#     docker lists no node container for the cluster.
#
# The `command -v docker` guard itself is not exercised: on an Ubuntu runner
# docker lives in /usr/bin, inside the tightened PATH below, so a test that
# deleted the stub would run the real client.
#
# Usage: bash tests/unit/hack/ci_dump_diagnostics_node_pressure_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DUMP_SH="$PROJECT_ROOT/hack/ci-dump-diagnostics.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# jq is a real dependency of the working-set block. The stub PATH below is
# tightened to the stub dir plus /usr/bin and /bin, so a jq installed
# elsewhere (Homebrew, ~/.local) has to be linked into the stub dir.
JQ_BIN="$(command -v jq 2>/dev/null || true)"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_stubs <dir> <dmesg_mode>
# kubectl: one node named stub-node, a pod table with one restarted and one
# clean pod, a kubelet summary with three pods in unsorted order.
# docker: one container stub-cluster-control-plane; <dmesg_mode> is
# "oom" (dmesg prints an OOM kill), "quiet" (dmesg prints unrelated lines),
# "denied" (dmesg exits 1 with an error on stderr) or "nonode" (docker ps
# lists no container at all).
# Anything the dump does not invoke exits 64 with a marker on stderr.
write_stubs() {
  local dir="$1" dmesg_mode="$2"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
case "\$1" in
  get)
    case "\$2" in
      nodes)
        case "\$*" in
          *"-o name"*) echo "node/stub-node" ;;
          *jsonpath*)  echo "stub-node" ;;
        esac
        exit 0
        ;;
      pods)
        case "\$*" in
          *custom-columns*)
            printf '%s\n' \\
              "NAMESPACE   POD              RESTARTS   LAST_REASON   LAST_EXIT   QOS" \\
              "openstack   openstack-db-0   5          OOMKilled     137         BestEffort" \\
              "openstack   memcached-0      0          <none>        <none>      BestEffort" \\
              "openstack   neutron-api-1    0,2        <none>,Error  <none>,1    Burstable"
            ;;
        esac
        exit 0
        ;;
      --raw)
        cat <<'JSON'
{"node":{"memory":{"workingSetBytes":6442450944,"availableBytes":1073741824}},
 "pods":[
  {"podRef":{"namespace":"openstack","name":"memcached-0"},"memory":{"workingSetBytes":52428800}},
  {"podRef":{"namespace":"openstack","name":"openstack-db-0"},"memory":{"workingSetBytes":734003200}},
  {"podRef":{"namespace":"openstack","name":"neutron-api-1"},"memory":{"workingSetBytes":419430400}}
 ]}
JSON
        exit 0
        ;;
      ns)
        exit 1
        ;;
      helmrelease|daemonsets|events|fluxinstance,fluxreport)
        exit 0
        ;;
    esac
    ;;
  describe)
    if [ "\$2" = "node/stub-node" ]; then
      printf '%s\n' \\
        "Name:               stub-node" \\
        "Capacity:" \\
        "  cpu:                4" \\
        "  memory:             16374504Ki" \\
        "Allocatable:" \\
        "  cpu:                4" \\
        "  memory:             16374504Ki" \\
        "System Info:" \\
        "  Kernel Version:             6.8.0" \\
        "Allocated resources:" \\
        "  Resource           Requests      Limits" \\
        "  cpu                2430m (60%)   1500m (37%)" \\
        "  memory             5250Mi (32%)  9450Mi (59%)" \\
        "Events:              <none>"
      exit 0
    fi
    ;;
  api-resources)
    exit 0
    ;;
esac
echo "[kubectl-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/kubectl"

  cat >"$dir/docker" <<STUB
#!/bin/bash
case "\$1" in
  ps)
    [ "${dmesg_mode}" = "nonode" ] || echo "stub-cluster-control-plane"
    exit 0
    ;;
  exec)
    # \$2 is the container, \$3 the command.
    case "${dmesg_mode}" in
      oom)
        printf '%s\n' \\
          "[ 4242.000001] veth1 entered promiscuous mode" \\
          "[ 4242.424242] Out of memory: Killed process 31337 (mariadbd) total-vm:2201724kB, anon-rss:701200kB" \\
          "[ 4242.424243] oom-kill:constraint=CONSTRAINT_NONE,cpuset=cri-containerd-abc,mems_allowed=0,oom_memcg=/kubepods.slice/kubepods-besteffort.slice"
        exit 0
        ;;
      quiet)
        echo "[ 4242.000001] veth1 entered promiscuous mode"
        exit 0
        ;;
      denied)
        echo "dmesg: read kernel buffer failed: Operation not permitted" >&2
        exit 1
        ;;
    esac
    ;;
esac
echo "[docker-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/docker"

  cat >"$dir/flux" <<'STUB'
#!/bin/bash
exit 0
STUB
  chmod +x "$dir/flux"

  if [ -n "$JQ_BIN" ]; then
    ln -sf "$JQ_BIN" "$dir/jq"
  fi
}

# run_dump <stub_dir> [KIND_CLUSTER]
run_dump() {
  local stub_dir="$1" cluster="${2:-stub-cluster}"
  (
    PATH="$stub_dir:/usr/bin:/bin"
    export PATH
    OPERATOR=""
    export OPERATOR
    KIND_CLUSTER="$cluster"
    export KIND_CLUSTER
    bash "$DUMP_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test A: capacity, restarts and OOM lines are all emitted
# ---------------------------------------------------------------------------
test_node_pressure_blocks_emitted() {
  echo "Test: emits node capacity, restarted containers and kernel OOM lines"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  write_stubs "$tmp" oom

  local output exit_code
  output="$(run_dump "$tmp")"
  exit_code=$?

  assert_eq "dump script exits 0" "0" "$exit_code"
  assert_not_contains "no kubectl call the stub does not recognise" \
    "$output" "[kubectl-stub] unexpected invocation"
  assert_not_contains "no docker call the stub does not recognise" \
    "$output" "[docker-stub] unexpected invocation"

  assert_contains "node Allocatable block is printed" "$output" "Allocatable:"
  assert_contains "node Allocated resources block is printed" \
    "$output" "memory             5250Mi (32%)  9450Mi (59%)"
  assert_not_contains "System Info is cut out of the node describe" \
    "$output" "Kernel Version:"

  assert_contains "restarted single-container pod is listed with its reason" \
    "$output" "openstack-db-0   5          OOMKilled     137         BestEffort"
  assert_contains "restarted multi-container pod is listed" \
    "$output" "neutron-api-1    0,2"
  assert_not_contains "zero-restart pod is filtered out" \
    "$output" "memcached-0      0          <none>"

  assert_contains "kernel OOM kill line is printed" \
    "$output" "Out of memory: Killed process 31337 (mariadbd)"
  assert_not_contains "unrelated dmesg lines are filtered out" \
    "$output" "entered promiscuous mode"
}

# ---------------------------------------------------------------------------
# Test B: working set is sorted largest first under the node line
# ---------------------------------------------------------------------------
test_working_set_sorted() {
  echo "Test: prints the per-pod working set largest first under the node line"
  if [ -z "$JQ_BIN" ]; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  write_stubs "$tmp" quiet

  local output section
  output="$(run_dump "$tmp")"
  section="$(printf '%s\n' "$output" \
    | sed -n '/=== Memory working set per pod/,/=== Kernel OOM events/p')"

  assert_contains "node line carries working set and available memory" \
    "$section" "node stub-node: workingSet 6144 MiB, available 1024 MiB"
  local order
  order="$(printf '%s\n' "$section" | grep -E '^[0-9]+ MiB' | awk '{print $1}' | tr '\n' ' ')"
  assert_eq "pods are sorted by working set, largest first" "700 400 50 " "$order"
  assert_contains "the largest pod is the database" \
    "$section" "700 MiB	openstack/openstack-db-0"
}

# ---------------------------------------------------------------------------
# Test C: dmesg unavailable, no OOM lines, no docker
# ---------------------------------------------------------------------------
test_dmesg_edge_cases() {
  echo "Test: says so when dmesg is denied, when it has no OOM lines, and when no node container exists"
  local tmp output
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_stubs "$tmp" denied
  output="$(run_dump "$tmp")"
  assert_contains "denied dmesg is reported with the kernel's error" \
    "$output" "(dmesg unavailable in stub-cluster-control-plane: dmesg: read kernel buffer failed: Operation not permitted)"

  write_stubs "$tmp" quiet
  output="$(run_dump "$tmp")"
  assert_contains "a dmesg without OOM lines says so" "$output" "(no OOM lines in dmesg)"

  write_stubs "$tmp" nonode
  output="$(run_dump "$tmp")"
  assert_contains "a host without a node container for the cluster gets a SKIP line" \
    "$output" "SKIP: no kind node container for cluster 'stub-cluster' on this host"
  assert_not_contains "no per-node header is printed when there is no node" "$output" "--- stub-cluster-control-plane ---"
  assert_eq "dump script exits 0 without a node container" "0" "$(run_dump "$tmp" >/dev/null; echo $?)"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_node_pressure_blocks_emitted
test_working_set_sorted
test_dmesg_edge_cases

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
