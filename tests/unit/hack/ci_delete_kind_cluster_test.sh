#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-delete-kind-cluster.sh tears a job's cluster down the way the
# e2e jobs need it torn down: every cluster on the runner goes, and nothing that
# happens while it goes can fail the job.
#
# That second half is the point of the script. Run 34714006750 passed every
# suite of e2e-infra and was reported red because the post step of
# helm/kind-action lost a docker race on its way out; a teardown that can do
# that is a teardown that reports on the runner's mood rather than on the code
# under test. So the cases below pin both directions: a sweep that works is
# silent and green, and a sweep that fails is a warning and still green.
#
# The sweep itself is stubbed here — hack/ci-reset-kind-cluster.sh has its own
# test for what it removes — except in the last case, which runs the real one
# against a docker stub to prove the two are actually wired together.
#
# Usage: bash tests/unit/hack/ci_delete_kind_cluster_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DELETE_SH="$PROJECT_ROOT/hack/ci-delete-kind-cluster.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_sweep_stub <path> <exit code>
# Writes a stand-in for hack/ci-reset-kind-cluster.sh that records the
# environment it was called with in <path>.env and exits with the given code.
write_sweep_stub() {
  local path="$1" code="$2"
  cat >"$path" <<STUB
#!/bin/bash
{
  echo "KIND_RESET_SCOPE=\${KIND_RESET_SCOPE:-<unset>}"
  echo "KIND_RM_ATTEMPTS=\${KIND_RM_ATTEMPTS:-<unset>}"
  echo "args=\$*"
} >"$path.env"
echo "sweep ran"
exit $code
STUB
  chmod +x "$path"
}

# run_delete <tmp> [env assignments…] — Runs the teardown script.
# Echoes combined stdout/stderr; returns its exit code.
run_delete() {
  local tmp="$1"
  shift
  (
    export PATH="$tmp/bin:/usr/bin:/bin"
    env "$@" bash "$DELETE_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test A: the sweep is run at scope all
# ---------------------------------------------------------------------------
test_sweeps_every_cluster() {
  echo "Test: runs the sweep over every cluster on the runner"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/bin"
  write_sweep_stub "$tmp/sweep" 0

  local output exit_code
  output="$(run_delete "$tmp" KIND_TEARDOWN_SCRIPT="$tmp/sweep")"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "says what it is doing" "$output" "Tearing down every kind cluster"
  assert_contains "reports the teardown finished" "$output" "Teardown complete."
  # Scope all is what makes this work for e2e-multicluster, which brings up two
  # clusters and would otherwise need both names threaded into the step.
  assert_file_contains "asks for scope all" "$tmp/sweep.env" "KIND_RESET_SCOPE=all"
  assert_eq "needs no cluster name" "args=" "$(grep '^args=' "$tmp/sweep.env")"
  assert_not_contains "emits no warning on a clean teardown" "$output" "::warning::"
}

# ---------------------------------------------------------------------------
# Test B: a failed sweep warns instead of failing the job
# ---------------------------------------------------------------------------
test_failed_sweep_is_a_warning() {
  echo "Test: a cluster that survives the teardown is a warning, not a failure"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/bin"
  write_sweep_stub "$tmp/sweep" 1

  local output exit_code
  output="$(run_delete "$tmp" KIND_TEARDOWN_SCRIPT="$tmp/sweep")"
  exit_code=$?

  assert_eq "exits 0 so the green test run stays green" "0" "$exit_code"
  assert_contains "annotates the surviving cluster" \
    "$output" "::warning::a kind cluster survived the teardown"
  assert_contains "says who clears it instead" "$output" "next e2e job on this runner"
  assert_not_contains "does not annotate an error" "$output" "::error::"
}

# ---------------------------------------------------------------------------
# Test C: a missing sweep script is a warning too
# ---------------------------------------------------------------------------
test_missing_sweep_script() {
  echo "Test: a missing sweep script warns rather than failing the job"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/bin"

  local output exit_code
  output="$(run_delete "$tmp" KIND_TEARDOWN_SCRIPT="$tmp/does-not-exist")"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "names the script it could not find" \
    "$output" "::warning::kind teardown skipped"
}

# ---------------------------------------------------------------------------
# Test D: the retry knobs of the sweep reach it
# ---------------------------------------------------------------------------
test_passes_retry_knobs_through() {
  echo "Test: passes the sweep's retry settings through untouched"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/bin"
  write_sweep_stub "$tmp/sweep" 0

  local exit_code
  run_delete "$tmp" KIND_TEARDOWN_SCRIPT="$tmp/sweep" KIND_RM_ATTEMPTS=7 >/dev/null
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_file_contains "KIND_RM_ATTEMPTS reaches the sweep" \
    "$tmp/sweep.env" "KIND_RM_ATTEMPTS=7"
}

# ---------------------------------------------------------------------------
# Test E: the real sweep, against a docker stub
# ---------------------------------------------------------------------------
test_removes_the_clusters_for_real() {
  echo "Test: the default sweep removes every kind cluster on the host"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/bin" "$tmp/runner-temp"
  printf '%s\n' "n1 cobaltcore-mgmt" "n2 cobaltcore-target" "cache1 -" >"$tmp/state"

  # docker stub: the same inventory shape the reset test uses — `<id> <cluster>`
  # per container, `-` for one that carries no kind label.
  cat >"$tmp/bin/docker" <<STUB
#!/bin/bash
STATE="$tmp/state"
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
    awk '\$2 != "-" { print \$2 }' "\$STATE"
    ;;
  rm)
    for arg in "\$@"; do
      case "\$arg" in
        rm | -f | -v) ;;
        *)
          grep -v "^\$arg " "\$STATE" >"\$STATE.next" || true
          mv "\$STATE.next" "\$STATE"
          ;;
      esac
    done
    ;;
esac
exit 0
STUB
  chmod +x "$tmp/bin/docker"

  local output exit_code
  output="$(
    (
      export PATH="$tmp/bin:/usr/bin:/bin"
      export RUNNER_TEMP="$tmp/runner-temp"
      # No cluster name anywhere: the step in ci.yaml passes none either.
      env -u CLUSTER_NAME -u KIND_CLUSTER bash "$DELETE_SH"
    ) 2>&1
  )"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "both clusters are reported" \
    "$output" "cobaltcore-mgmt cobaltcore-target"
  assert_eq "every kind cluster is gone, the unlabelled container is not" \
    "cache1 -" "$(cat "$tmp/state")"
  assert_not_contains "nothing survives to warn about" "$output" "::warning::"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_sweeps_every_cluster
test_failed_sweep_is_a_warning
test_missing_sweep_script
test_passes_retry_knobs_through
test_removes_the_clusters_for_real

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
