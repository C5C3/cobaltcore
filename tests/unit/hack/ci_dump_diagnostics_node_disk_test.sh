#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the disk-use section of hack/ci-dump-diagnostics.sh.
#
# Every image an e2e job loads into kind lands under /var/lib/containerd on
# the node, and each release under releases/ adds one image per service to
# the e2e-operator legs. Decision D6 of #1276 measures that growth from the
# job log, so the dump must emit, always and without failing the job:
#
#   - per kind node container, `df -h` and `du -sh` of /var/lib/containerd
#     under a `--- <node> ---` header, with du cut off after 30 s;
#   - an explicit line naming the node and the error when `docker exec` fails,
#     after whatever the command printed before it failed;
#   - a SKIP line when docker lists no node container for the cluster.
#
# The `command -v docker` guard itself is not exercised: on an Ubuntu runner
# docker lives in /usr/bin, inside the tightened PATH below, so a test that
# deleted the stub would run the real client.
#
# Usage: bash tests/unit/hack/ci_dump_diagnostics_node_disk_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DUMP_SH="$PROJECT_ROOT/hack/ci-dump-diagnostics.sh"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_stubs <dir> <disk_mode>
# kubectl: an empty cluster; every infrastructure read answers with nothing.
# docker: one container stub-cluster-control-plane whose dmesg holds no OOM
# line. <disk_mode> sets what the disk-use exec does: "ok" (df and du print,
# exit 0), "fail" (exit 1 with an error on stderr only), "partial" (du prints
# its total and exits 1 over a file that vanished), "timeout" (df prints, and
# timeout stops du with exit 124 and its --verbose line) or "nonode" (docker ps
# lists no container at all). The exec answers only a command that names
# /var/lib/containerd and the 30 s bound on du. Anything the dump does not
# invoke exits 64 with a marker on stderr.
write_stubs() {
  local dir="$1" disk_mode="$2"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
case "$1" in
  get)
    case "$2" in
      helmrelease|pods|daemonsets|nodes|events)
        exit 0
        ;;
      ns)
        exit 1
        ;;
      deployment)
        echo 'Error from server (NotFound): deployments.apps "orc-controller-manager" not found' >&2
        exit 1
        ;;
    esac
    ;;
  api-resources)
    exit 0
    ;;
esac
echo "[kubectl-stub] unexpected invocation: $*" >&2
exit 64
STUB
  chmod +x "$dir/kubectl"

  cat >"$dir/docker" <<STUB
#!/bin/bash
case "\$1" in
  ps)
    [ "${disk_mode}" = "nonode" ] || echo "stub-cluster-control-plane"
    exit 0
    ;;
  exec)
    # \$2 is the container, \$3 the command.
    if [ "\$3" = "dmesg" ]; then
      echo "[ 4242.000001] veth1 entered promiscuous mode"
      exit 0
    fi
    case "\$*" in
      *"df -h /var/lib/containerd && timeout --verbose 30 du -sh /var/lib/containerd"*)
        case "${disk_mode}" in
          ok)
            printf '%s\n' "Filesystem      Size  Used Avail Use% Mounted on" \\
              "overlay          72G   31G   41G  43% /"
            printf '18G\t/var/lib/containerd\n'
            exit 0
            ;;
          fail)
            echo "Error response from daemon: container stub-cluster-control-plane is not running" >&2
            exit 1
            ;;
          partial)
            printf '%s\n' "Filesystem      Size  Used Avail Use% Mounted on" \\
              "overlay          72G   31G   41G  43% /"
            echo "du: cannot access '/var/lib/containerd/tmpmounts/m1': No such file or directory" >&2
            printf '17G\t/var/lib/containerd\n'
            exit 1
            ;;
          timeout)
            printf '%s\n' "Filesystem      Size  Used Avail Use% Mounted on" \\
              "overlay          72G   31G   41G  43% /"
            echo "timeout: sending signal TERM to command 'du'" >&2
            exit 124
            ;;
        esac
        ;;
    esac
    ;;
esac
echo "[docker-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/docker"
}

# run_dump <stub_dir>
run_dump() {
  local stub_dir="$1"
  (
    PATH="$stub_dir:/usr/bin:/bin"
    export PATH
    OPERATOR=""
    export OPERATOR
    KIND_CLUSTER="stub-cluster"
    export KIND_CLUSTER
    bash "$DUMP_SH"
  ) 2>&1
}

# disk_section <output> prints the disk-use section alone.
disk_section() {
  printf '%s\n' "$1" | sed -n '/=== Disk use on the kind node(s) ===/,/=== DaemonSet chaos-daemon detail ===/p'
}

# ---------------------------------------------------------------------------
# Test A: a node that answers
# ---------------------------------------------------------------------------
test_node_disk_use_emitted() {
  echo "Test: prints df and du of /var/lib/containerd under the node header"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  write_stubs "$tmp" ok

  local output exit_code section
  output="$(run_dump "$tmp")"
  exit_code=$?
  section="$(disk_section "$output")"

  assert_eq "dump script exits 0" "0" "$exit_code"
  assert_not_contains "no kubectl call the stub does not recognise" \
    "$output" "[kubectl-stub] unexpected invocation"
  assert_not_contains "no docker call the stub does not recognise" \
    "$output" "[docker-stub] unexpected invocation"
  assert_contains "the section has its header" "$output" "=== Disk use on the kind node(s) ==="
  assert_eq "the node header is followed by the df header" \
    "--- stub-cluster-control-plane ---|Filesystem      Size  Used Avail Use% Mounted on" \
    "$(printf '%s\n' "$section" | grep -A1 -- '^--- stub-cluster-control-plane ---$' | paste -sd'|' -)"
  assert_contains "the df line is printed" "$section" "overlay          72G   31G   41G  43% /"
  assert_contains "the du total is printed" "$section" "18G	/var/lib/containerd"
  assert_not_contains "no unavailable line for a node that answers" "$section" "disk use unavailable"
}

# ---------------------------------------------------------------------------
# Test B: docker exec fails, with and without a total
# ---------------------------------------------------------------------------
test_failed_exec_is_reported() {
  echo "Test: names the node and the error when docker exec fails, and goes on"
  local tmp output section
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_stubs "$tmp" fail
  output="$(run_dump "$tmp")"
  section="$(disk_section "$output")"
  assert_contains "a failing exec is reported with docker's error" "$section" \
    "(disk use unavailable in stub-cluster-control-plane: Error response from daemon: container stub-cluster-control-plane is not running)"
  assert_contains "the dump goes on to the next section" "$output" "=== DaemonSet chaos-daemon detail ==="
  assert_eq "dump script exits 0 when docker exec fails" "0" "$(run_dump "$tmp" >/dev/null; echo $?)"

  write_stubs "$tmp" partial
  output="$(run_dump "$tmp")"
  section="$(disk_section "$output")"
  assert_contains "a du that exits 1 still prints its total" "$section" "17G	/var/lib/containerd"
  assert_contains "and the reason it exited 1" "$section" \
    "(disk use unavailable in stub-cluster-control-plane: du: cannot access '/var/lib/containerd/tmpmounts/m1': No such file or directory)"

  write_stubs "$tmp" timeout
  output="$(run_dump "$tmp")"
  section="$(disk_section "$output")"
  assert_contains "a du cut off by timeout still leaves the df line" "$section" \
    "overlay          72G   31G   41G  43% /"
  assert_contains "and names the timeout as the reason" "$section" \
    "(disk use unavailable in stub-cluster-control-plane: timeout: sending signal TERM to command 'du')"
  assert_eq "dump script exits 0 when du times out" "0" "$(run_dump "$tmp" >/dev/null; echo $?)"
}

# ---------------------------------------------------------------------------
# Test C: no node container
# ---------------------------------------------------------------------------
test_no_node_container() {
  echo "Test: says so when docker lists no node container for the cluster"
  local tmp output section
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_stubs "$tmp" nonode
  output="$(run_dump "$tmp")"
  section="$(disk_section "$output")"
  assert_contains "a host without a node container for the cluster gets a SKIP line" \
    "$section" "SKIP: no kind node container for cluster 'stub-cluster' on this host"
  assert_not_contains "no per-node header is printed when there is no node" \
    "$section" "--- stub-cluster-control-plane ---"
  assert_eq "dump script exits 0 without a node container" "0" "$(run_dump "$tmp" >/dev/null; echo $?)"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_node_disk_use_emitted
test_failed_exec_is_reported
test_no_node_container

echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
