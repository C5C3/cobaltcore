#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `cap_node_nofile`: it lowers containerd's
# RLIMIT_NOFILE on every kind node so uWSGI workloads (Keystone) do not
# OOM-crashloop under Docker Desktop's LimitNOFILE=infinity default (#546).
#
# The function is exercised in a subshell with recording stubs for kind/docker/
# kubectl prepended to PATH, so no real cluster is touched. The docker stub
# appends its argv to a log file, letting us assert the drop-in content and the
# containerd restart without a running node.
#
# Usage: bash tests/unit/hack/deploy_infra_nofile_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_recording_stubs <dir>
# Writes kind/docker/kubectl shims into <dir>:
#   kind    — `kind get nodes --name X` echoes $KIND_NODES (space/newline sep).
#   docker  — appends its full argv to $DOCKER_LOG and exits $DOCKER_EXIT.
#   kubectl — `kubectl get nodes -o json` returns one Ready node so
#             wait_for_node_ready returns on the first poll (no sleep).
make_recording_stubs() {
  local dir="$1"
  mkdir -p "$dir"

  cat >"$dir/kind" <<'STUB'
#!/bin/bash
if [ "${1:-}" = "get" ] && [ "${2:-}" = "nodes" ]; then
  printf '%s\n' ${KIND_NODES:-}
fi
STUB

  cat >"$dir/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >> "$DOCKER_LOG"
exit "${DOCKER_EXIT:-0}"
STUB

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
if [ "${1:-}" = "get" ] && [ "${2:-}" = "nodes" ]; then
  echo '{"items":[{"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}'
fi
STUB

  chmod +x "$dir/kind" "$dir/docker" "$dir/kubectl"
}

# run_cap <stub_dir>
# Sources deploy-infra.sh in a subshell with <stub_dir> prepended to PATH and
# invokes cap_node_nofile. The caller controls behaviour via the exported env:
# KIND_NODES, DOCKER_LOG, DOCKER_EXIT, and NODE_NOFILE_LIMIT (set/unset).
# Echoes combined stdout/stderr.
run_cap() {
  local stub_dir="$1"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    cap_node_nofile
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: default limit is written and containerd restarted on the one node
# ---------------------------------------------------------------------------
test_default_limit_applied() {
  echo "Test: cap_node_nofile writes the default 1048576 drop-in and restarts containerd"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_recording_stubs "$tmp/bin"

  export KIND_NODES="forge-control-plane"
  export DOCKER_LOG="$tmp/docker.log"
  export DOCKER_EXIT=0
  : >"$DOCKER_LOG"
  unset NODE_NOFILE_LIMIT   # exercise the built-in 1048576 default

  local output
  output="$(run_cap "$tmp/bin")"

  local docker_log
  docker_log="$(cat "$DOCKER_LOG")"

  assert_contains "docker exec targets the node" "$docker_log" "docker exec forge-control-plane"
  assert_contains "drop-in sets the default LimitNOFILE" "$docker_log" "LimitNOFILE=1048576"
  assert_contains "drop-in path is the containerd service dir" "$docker_log" "/etc/systemd/system/containerd.service.d/nofile.conf"
  assert_contains "containerd is restarted" "$docker_log" "systemctl restart containerd"
  assert_contains "log confirms the applied limit" "$output" "containerd RLIMIT_NOFILE set to 1048576"
}

# ---------------------------------------------------------------------------
# Test 2: NODE_NOFILE_LIMIT override flows into the drop-in
# ---------------------------------------------------------------------------
test_override_limit() {
  echo "Test: cap_node_nofile honours a NODE_NOFILE_LIMIT override"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_recording_stubs "$tmp/bin"

  export KIND_NODES="forge-control-plane"
  export DOCKER_LOG="$tmp/docker.log"
  export DOCKER_EXIT=0
  : >"$DOCKER_LOG"
  export NODE_NOFILE_LIMIT=524288

  run_cap "$tmp/bin" >/dev/null

  local docker_log
  docker_log="$(cat "$DOCKER_LOG")"
  assert_contains "override value lands in the drop-in" "$docker_log" "LimitNOFILE=524288"
  assert_not_contains "default value is not used" "$docker_log" "LimitNOFILE=1048576"
}

# ---------------------------------------------------------------------------
# Test 3: empty NODE_NOFILE_LIMIT opts out entirely
# ---------------------------------------------------------------------------
test_empty_limit_skips() {
  echo "Test: an explicitly-empty NODE_NOFILE_LIMIT skips the cap"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_recording_stubs "$tmp/bin"

  export KIND_NODES="forge-control-plane"
  export DOCKER_LOG="$tmp/docker.log"
  export DOCKER_EXIT=0
  : >"$DOCKER_LOG"
  export NODE_NOFILE_LIMIT=""   # explicit opt-out (preserved by `-`, not `:-`)

  local output
  output="$(run_cap "$tmp/bin")"

  assert_contains "log reports the skip" "$output" "skipping containerd RLIMIT_NOFILE cap"
  assert_eq "docker is never invoked when opted out" "" "$(cat "$DOCKER_LOG")"
}

# ---------------------------------------------------------------------------
# Test 4: no kind nodes → warn and return without touching docker
# ---------------------------------------------------------------------------
test_no_nodes_warns() {
  echo "Test: cap_node_nofile warns and no-ops when the cluster has no nodes"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_recording_stubs "$tmp/bin"

  export KIND_NODES=""          # kind returns no node names
  export DOCKER_LOG="$tmp/docker.log"
  export DOCKER_EXIT=0
  : >"$DOCKER_LOG"
  unset NODE_NOFILE_LIMIT

  local output
  output="$(run_cap "$tmp/bin")"

  assert_contains "log warns about the missing nodes" "$output" "no kind nodes found"
  assert_eq "docker is never invoked without nodes" "" "$(cat "$DOCKER_LOG")"
}

# ---------------------------------------------------------------------------
# Test 5: every node in a multi-node cluster is patched
# ---------------------------------------------------------------------------
test_multi_node() {
  echo "Test: cap_node_nofile patches every node in a multi-node cluster"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_recording_stubs "$tmp/bin"

  export KIND_NODES=$'forge-control-plane\nforge-worker'
  export DOCKER_LOG="$tmp/docker.log"
  export DOCKER_EXIT=0
  : >"$DOCKER_LOG"
  unset NODE_NOFILE_LIMIT

  run_cap "$tmp/bin" >/dev/null

  local docker_log
  docker_log="$(cat "$DOCKER_LOG")"
  assert_contains "control-plane node is patched" "$docker_log" "docker exec forge-control-plane"
  assert_contains "worker node is patched" "$docker_log" "docker exec forge-worker"
}

# ---------------------------------------------------------------------------
# Test 6: a docker failure warns but does not abort the deploy
# ---------------------------------------------------------------------------
test_docker_failure_is_best_effort() {
  echo "Test: a failing docker exec warns but cap_node_nofile still returns 0"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_recording_stubs "$tmp/bin"

  export KIND_NODES="forge-control-plane"
  export DOCKER_LOG="$tmp/docker.log"
  export DOCKER_EXIT=1          # docker exec fails
  : >"$DOCKER_LOG"
  unset NODE_NOFILE_LIMIT

  local output exit_code
  output="$(run_cap "$tmp/bin")"
  exit_code=$?

  assert_eq "cap_node_nofile returns 0 despite the failure" "0" "$exit_code"
  assert_contains "log warns about the failed node" "$output" "failed to cap RLIMIT_NOFILE on forge-control-plane"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_limit_applied
test_override_limit
test_empty_limit_skips
test_no_nodes_warns
test_multi_node
test_docker_failure_is_best_effort

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
