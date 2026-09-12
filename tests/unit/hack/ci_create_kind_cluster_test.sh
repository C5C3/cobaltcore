#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-create-kind-cluster.sh creates the e2e cluster the way the CI
# jobs depend on: reset the runner, create, and on a failed creation reset again
# before retrying — the order that turns the failure mode of run 34707123151
# (`node(s) already exist for a cluster with the name "cobaltcore"`, a cluster a
# cancelled job left on a self-hosted runner) into a recovered job.
#
# The kind stub models the two behaviours that make the order matter: a creation
# refuses to run while node containers of that name exist, exactly as kind does,
# and a creation that fails leaves its partial cluster behind. A retry therefore
# only succeeds here if the script really reset the runner in front of it, so
# dropping that reset fails this test rather than surfacing a month later as a
# job that retries twice and fails twice.
#
# Usage: bash tests/unit/hack/ci_create_kind_cluster_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CREATE_SH="$PROJECT_ROOT/hack/ci-create-kind-cluster.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# tool_cache_arch — The architecture segment helm/kind-action's tool cache path
# carries, mirroring the case statement in the script under test.
tool_cache_arch() {
  case "$(uname -m)" in
    x86_64) echo "amd64" ;;
    aarch64 | arm64) echo "arm64" ;;
    *) echo "" ;;
  esac
}

# write_stubs <dir> <state> <log>
# Writes kind, docker and reset-script stubs. <state> is the host's kind node
# inventory, one `<id> <cluster>` line per node container; every stub appends its
# invocation to <log>.
#
#   STUB_KIND_CREATE_FAILS  — how many leading create attempts fail (default 0)
#   STUB_KIND_CREATE_SILENT — `true` exits 0 without registering the cluster
#   STUB_RESET_EXIT         — exit code of the reset stub (default 0)
write_stubs() {
  local dir="$1" state="$2" log="$3"
  mkdir -p "$dir"

  cat >"$dir/kind" <<STUB
#!/bin/bash
# Test stub kind — \`create cluster\`, \`get clusters\`. Creation behaves like the
# real thing: it refuses while node containers of that name exist, and a failed
# attempt leaves a partial cluster behind. See write_stubs() in
# tests/unit/hack/ci_create_kind_cluster_test.sh.
STATE="$state"
ATTEMPTS="$dir/create-attempts"
echo "kind \$*" >>"$log"

if [ "\$1" = get ] && [ "\$2" = clusters ]; then
  awk '{ print \$2 }' "\$STATE" | sort -u
  exit 0
fi

if [ "\$1" = create ] && [ "\$2" = cluster ]; then
  name=""
  for arg in "\$@"; do
    case "\$arg" in --name=*) name="\${arg#--name=}" ;; esac
  done

  if awk -v c="\$name" '\$2 == c { found = 1 } END { exit !found }' "\$STATE"; then
    echo "ERROR: failed to create cluster: node(s) already exist for a cluster with the name \"\$name\"" >&2
    exit 1
  fi

  attempt=\$((\$(cat "\$ATTEMPTS" 2>/dev/null || echo 0) + 1))
  echo "\$attempt" >"\$ATTEMPTS"

  if [ "\$attempt" -le "\${STUB_KIND_CREATE_FAILS:-0}" ]; then
    # A failed creation leaves its partial cluster on the host.
    echo "partial-\$attempt \$name" >>"\$STATE"
    echo "ERROR: failed to create cluster: could not start control-plane" >&2
    exit 1
  fi

  if [ "\${STUB_KIND_CREATE_SILENT:-false}" != true ]; then
    echo "node-\$attempt \$name" >>"\$STATE"
  fi
  exit 0
fi

echo "[kind-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/kind"

  cat >"$dir/docker" <<STUB
#!/bin/bash
# Test stub docker — the diagnostic reads of the script under test.
STATE="$state"
echo "docker \$*" >>"$log"
case "\$1" in
  ps)
    for arg in "\$@"; do
      case "\$arg" in
        label=io.x-k8s.kind.cluster=*)
          awk -v c="\${arg#label=io.x-k8s.kind.cluster=}" '\$2 == c { print \$1 }' "\$STATE"
          exit 0
          ;;
      esac
    done
    awk '{ print \$1 "\t" \$2 }' "\$STATE"
    ;;
  logs) echo "[stub node log]" ;;
  *)
    echo "[docker-stub] unexpected invocation: \$*" >&2
    exit 64
    ;;
esac
exit 0
STUB
  chmod +x "$dir/docker"

  cat >"$dir/reset.sh" <<STUB
#!/bin/bash
# Test stub reset — drops the named cluster's node containers, the part the
# create script depends on. The real script's own behaviour is pinned in
# tests/unit/hack/ci_reset_kind_cluster_test.sh.
STATE="$state"
echo "reset \$*" >>"$log"
if [ "\${STUB_RESET_EXIT:-0}" != 0 ]; then
  echo "::error::stub reset failed" >&2
  exit "\${STUB_RESET_EXIT}"
fi
awk -v c="\$1" '\$2 != c' "\$STATE" >"\$STATE.next"
mv "\$STATE.next" "\$STATE"
exit 0
STUB
  chmod +x "$dir/reset.sh"
}

# new_case <state lines…> — Fresh temp dir with the given node inventory.
new_case() {
  local tmp
  tmp="$(mktemp -d)"
  if [ "$#" -gt 0 ]; then
    printf '%s\n' "$@" >"$tmp/state"
  else
    : >"$tmp/state"
  fi
  write_stubs "$tmp/bin" "$tmp/state" "$tmp/log"
  : >"$tmp/log"
  echo "$tmp"
}

# run_create <tmp> [env assignments…] — Run the script under test with PATH
# limited to the stubs. Echoes combined stdout/stderr; returns its exit code.
run_create() {
  local tmp="$1"
  shift
  (
    export PATH="$tmp/bin:/usr/bin:/bin"
    env -u CLUSTER_NAME -u KIND_CLUSTER -u KIND_CONFIG \
      KIND_RESET_SCRIPT="$tmp/bin/reset.sh" \
      KIND_CREATE_RETRY_DELAY=0 \
      "$@" bash "$CREATE_SH"
  ) 2>&1
}

# count_in_log <tmp> <pattern> — How many log lines match.
count_in_log() {
  grep -c -- "$2" "$1/log" | tr -d ' '
}

# ---------------------------------------------------------------------------
# Test A: the ordinary path
# ---------------------------------------------------------------------------
test_creates_cluster() {
  echo "Test: resets the runner, then creates the cluster"

  local tmp
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore)"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_eq "resets the runner once" "1" "$(count_in_log "$tmp" "reset cobaltcore")"
  assert_eq "creates the cluster once" "1" "$(count_in_log "$tmp" "kind create cluster")"
  assert_contains "creates it with the name and the default wait" \
    "$(cat "$tmp/log")" "kind create cluster --name=cobaltcore --wait=60s"
  assert_not_contains "passes no kind config when none is given" \
    "$(cat "$tmp/log")" "--config"
  assert_file_contains "verifies kind knows the cluster" "$tmp/log" "kind get clusters"
  assert_contains "reports the cluster as created" "$output" "Kind cluster 'cobaltcore' created."
}

# ---------------------------------------------------------------------------
# Test B: the kind config reaches kind
# ---------------------------------------------------------------------------
test_passes_config() {
  echo "Test: passes the kind config through to the creation"

  local tmp
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore \
    KIND_CONFIG="$PROJECT_ROOT/hack/kind-config.yaml")"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "hands the config file to kind" \
    "$(cat "$tmp/log")" "--config=$PROJECT_ROOT/hack/kind-config.yaml"

  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore KIND_CONFIG="$tmp/does-not-exist.yaml")"
  exit_code=$?

  assert_eq "a missing config is a usage error" "2" "$exit_code"
  assert_contains "names the missing file" "$output" "::error::kind config '$tmp/does-not-exist.yaml' does not exist."
}

# ---------------------------------------------------------------------------
# Test C: the leftover that broke the CI job
# ---------------------------------------------------------------------------
test_recovers_from_leftover_cluster() {
  echo "Test: creates the cluster even when a cancelled job left one behind"

  local tmp
  tmp="$(new_case "stale1 cobaltcore")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore)"
  exit_code=$?

  assert_eq "exits 0 instead of failing the job" "0" "$exit_code"
  assert_eq "needs only one attempt" "1" "$(count_in_log "$tmp" "kind create cluster")"
  assert_not_contains "never hits the error that broke run 34707123151" \
    "$output" "node(s) already exist"
  assert_eq "the stale node is gone" "node-1 cobaltcore" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Test D: a failed creation is retried on a reset runner
# ---------------------------------------------------------------------------
test_retries_failed_creation() {
  echo "Test: retries a failed creation after resetting the partial cluster"

  local tmp
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore STUB_KIND_CREATE_FAILS=1)"
  exit_code=$?

  assert_eq "exits 0 on the second attempt" "0" "$exit_code"
  assert_eq "attempts the creation twice" "2" "$(count_in_log "$tmp" "kind create cluster")"
  assert_eq "resets the runner before each attempt" "2" "$(count_in_log "$tmp" "reset cobaltcore")"
  assert_contains "says the first attempt failed" \
    "$output" "WARNING: creating 'cobaltcore' failed on attempt 1/2"
  assert_contains "dumps what the host looked like" \
    "$output" "kind node containers on this host"
  assert_eq "only the cluster of the successful attempt remains" \
    "node-2 cobaltcore" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Test E: a creation that keeps failing fails the step
# ---------------------------------------------------------------------------
test_fails_after_all_attempts() {
  echo "Test: fails the step when every attempt fails"

  local tmp
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore \
    STUB_KIND_CREATE_FAILS=9 KIND_CREATE_ATTEMPTS=3)"
  exit_code=$?

  assert_nonzero_exit "exits non-zero" "$exit_code"
  assert_eq "honours KIND_CREATE_ATTEMPTS" "3" "$(count_in_log "$tmp" "kind create cluster")"
  assert_contains "annotates the failure for the job log" \
    "$output" "::error::kind cluster 'cobaltcore' could not be created in 3 attempt(s)."
  assert_file_contains "dumps the node logs for the failed attempts" "$tmp/log" "docker logs --tail 30"
}

# ---------------------------------------------------------------------------
# Test F: a runner that cannot be cleared is not built on
# ---------------------------------------------------------------------------
test_stops_when_reset_fails() {
  echo "Test: does not attempt a creation the leftover would break"

  local tmp
  tmp="$(new_case "stale1 cobaltcore")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore STUB_RESET_EXIT=1)"
  exit_code=$?

  assert_nonzero_exit "exits non-zero" "$exit_code"
  assert_eq "never calls kind create" "0" "$(count_in_log "$tmp" "kind create cluster")"
  assert_contains "says why it stopped" \
    "$output" "::error::the runner still carries a kind cluster that could not be removed"
}

# ---------------------------------------------------------------------------
# Test G: a creation that reports success without a cluster
# ---------------------------------------------------------------------------
test_verifies_the_cluster_exists() {
  echo "Test: fails when kind reports success but lists no cluster"

  local tmp
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore STUB_KIND_CREATE_SILENT=true)"
  exit_code=$?

  assert_nonzero_exit "exits non-zero" "$exit_code"
  assert_contains "annotates the mismatch" \
    "$output" "::error::'kind create cluster' reported success but kind does not list a cluster named 'cobaltcore'."
}

# ---------------------------------------------------------------------------
# Test H: where the cluster name and the kind binary come from
# ---------------------------------------------------------------------------
test_inputs() {
  echo "Test: resolves the cluster name and the kind binary"

  local tmp
  tmp="$(new_case)"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code

  # KIND_CLUSTER is what the workflow sets; CLUSTER_NAME is the explicit form.
  output="$(run_create "$tmp" KIND_CLUSTER=cobaltcore)"
  exit_code=$?
  assert_eq "falls back to KIND_CLUSTER" "0" "$exit_code"
  assert_file_contains "creates the cluster KIND_CLUSTER names" \
    "$tmp/log" "kind create cluster --name=cobaltcore"

  output="$(run_create "$tmp")"
  exit_code=$?
  assert_eq "no cluster name is a usage error" "2" "$exit_code"
  assert_contains "says what it needs" \
    "$output" "::error::hack/ci-create-kind-cluster.sh needs CLUSTER_NAME"

  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore KIND_CREATE_ATTEMPTS=none)"
  exit_code=$?
  assert_eq "a non-numeric attempt count is a usage error" "2" "$exit_code"
  assert_contains "says what a valid value looks like" \
    "$output" "::error::KIND_CREATE_ATTEMPTS must be a positive integer"

  # helm/kind-action puts kind in the runner tool cache and appends the directory
  # to GITHUB_PATH. A PATH that did not carry over must not take the job down:
  # the cache layout is the fallback.
  local arch
  arch="$(tool_cache_arch)"
  if [ -z "$arch" ]; then
    echo "  SKIP: unknown architecture $(uname -m) has no tool-cache mapping"
    SKIP=$((SKIP + 1))
    return 0
  fi

  local cache="$tmp/tool-cache/kind/v0.32.0/$arch/kind/bin"
  mkdir -p "$cache"
  mv "$tmp/bin/kind" "$cache/kind"
  : >"$tmp/state"
  : >"$tmp/log"

  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore \
    RUNNER_TOOL_CACHE="$tmp/tool-cache" KIND_VERSION=v0.32.0)"
  exit_code=$?

  assert_eq "finds kind in the runner tool cache" "0" "$exit_code"
  assert_contains "says which binary it used" "$output" "Kind binary  : $cache/kind"

  rm -rf "$tmp/tool-cache"
  : >"$tmp/state"
  : >"$tmp/log"

  output="$(run_create "$tmp" CLUSTER_NAME=cobaltcore)"
  exit_code=$?

  assert_eq "no kind anywhere is a usage error" "2" "$exit_code"
  assert_contains "says kind is missing" "$output" "::error::kind is not on PATH"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_creates_cluster
test_passes_config
test_recovers_from_leftover_cluster
test_retries_failed_creation
test_fails_after_all_attempts
test_stops_when_reset_fails
test_verifies_the_cluster_exists
test_inputs

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
