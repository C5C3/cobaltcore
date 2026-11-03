#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `make e2e-chaos` recipe gates execution behind a chaos-mesh
# namespace preflight. Chaos Mesh is now opt-in in the kind
# Quick Start, so invoking the chaos suite without the namespace must
# fast-fail with a clear remediation message instead of letting chainsaw
# attempt the suite against a cluster that lacks the dependency.
#
# The sweep leaves out the suites labelled chaos.c5c3.io/topology:
# multinode (#1392), and `make e2e-chaos-multinode` runs them alone, after a
# two-worker preflight that names the bring-up.
#
# Implementation: bash + tests/lib/assertions.sh (project-native, matches the
# sibling tests/unit/hack/deploy_infra_*_test.sh pattern). The repository has
# zero .bats files and no bats binary on CI, so introducing one would add an
# undeclared dependency.
#
# Usage: bash tests/unit/hack/e2e_chaos_preflight_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MAKEFILE="$PROJECT_ROOT/Makefile"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_stubs <dir> <kubectl_get_ns_exit> [<kubectl_version_exit>] [<workers>]
# Creates kubectl + chainsaw shims in <dir>:
#   - kubectl ONLY answers the verbs the two recipes invoke today
#     (`kubectl version`, `kubectl get ns chaos-mesh` and the worker list
#     of e2e-chaos-multinode); for the first two it exits with the supplied
#     per-verb exit code (<kubectl_version_exit> defaults to 0 = cluster
#     reachable), and the worker list names <workers> nodes (default 0). Any
#     other invocation is treated as a test-harness drift signal and
#     exits non-zero with a marker line on stderr — a future change that
#     adds another kubectl call expecting fail-fast behaviour will
#     surface here instead of silently passing under a blanket `exit 0`.
#   - chainsaw writes its arguments to the sentinel file
#     `$dir/CHAINSAW_INVOKED` and exits 0, so the test can assert whether
#     and how the recipe invoked it.
write_stubs() {
  local dir="$1" ns_exit="$2" version_exit="${3:-0}" workers="${4:-0}"
  mkdir -p "$dir"

  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Test stub kubectl dispatcher tightened to the exact verbs the
# Makefile recipe under test invokes (\`kubectl version\` and
# \`kubectl get ns chaos-mesh\`). Any other call exits non-zero so a
# future kubectl call introduced by the recipe cannot accidentally pass
# under a blanket exit 0.
if [ "\$1" = "version" ]; then
  exit ${version_exit}
fi
if [ "\$1" = "get" ] && [ "\$2" = "ns" ] && [ "\$3" = "chaos-mesh" ]; then
  exit ${ns_exit}
fi
if [ "\$*" = "get nodes -l !node-role.kubernetes.io/control-plane -o name" ]; then
  for i in \$(seq 1 ${workers}); do echo "node/worker\$i"; done
  exit 0
fi
echo "[kubectl-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/kubectl"

  cat >"$dir/chainsaw" <<STUB
#!/bin/bash
# Test stub record invocation so the test can assert whether the
# Makefile recipe reached the chainsaw step after the preflight.
echo "\$*" >"${dir}/CHAINSAW_INVOKED"
exit 0
STUB
  chmod +x "$dir/chainsaw"
}

# run_make_e2e_chaos <stub_dir> <stdout_file> <stderr_file> [<target>]
# Runs `make <target>` (default e2e-chaos) against the project Makefile with PATH limited to
# <stub_dir> plus /usr/bin and /bin so make itself, plus core utilities the
# recipe uses (echo), still resolve. Captures stdout and stderr to separate
# files so callers can assert per-stream contents (specifies the remediation message must land on stderr). Returns the make exit code.
run_make_e2e_chaos() {
  local stub_dir="$1" stdout_file="$2" stderr_file="$3" target="${4:-e2e-chaos}"
  (
    PATH="$stub_dir:/usr/bin:/bin"
    export PATH
    cd "$PROJECT_ROOT" || exit 1
    make -f "$MAKEFILE" "$target"
  ) >"$stdout_file" 2>"$stderr_file"
}

# ---------------------------------------------------------------------------
# Test 1: chaos-mesh namespace ABSENT -> fast-fail, no chainsaw
# ---------------------------------------------------------------------------
test_fast_fail_when_namespace_absent() {
  echo "Test: e2e-chaos fast-fails when chaos-mesh namespace is absent"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_stubs "$tmp" 1

  local stdout_file="$tmp/stdout" stderr_file="$tmp/stderr" exit_code
  run_make_e2e_chaos "$tmp" "$stdout_file" "$stderr_file"
  exit_code=$?

  local stdout stderr
  stdout="$(cat "$stdout_file")"
  stderr="$(cat "$stderr_file")"

  assert_nonzero_exit "make e2e-chaos exits non-zero when chaos-mesh namespace absent" "$exit_code"
  assert_contains "remediation message on stderr names WITH_CHAOS_MESH=true make deploy-infra" \
    "$stderr" "WITH_CHAOS_MESH=true make deploy-infra"
  assert_contains "remediation message on stderr states chaos-mesh is not installed" \
    "$stderr" "chaos-mesh is not installed"
  assert_not_contains "remediation message does NOT land on stdout (stderr contract)" \
    "$stdout" "chaos-mesh is not installed"

  if [ -e "$tmp/CHAINSAW_INVOKED" ]; then
    echo "  FAIL: chainsaw must NOT be invoked when preflight fails"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: chainsaw was not invoked when preflight failed"
    PASS=$((PASS + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 2: chaos-mesh namespace PRESENT -> chainsaw runs
# ---------------------------------------------------------------------------
test_happy_path_when_namespace_present() {
  echo "Test: e2e-chaos invokes chainsaw when chaos-mesh namespace present"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_stubs "$tmp" 0

  local stdout_file="$tmp/stdout" stderr_file="$tmp/stderr" exit_code
  run_make_e2e_chaos "$tmp" "$stdout_file" "$stderr_file"
  exit_code=$?

  local stdout stderr
  stdout="$(cat "$stdout_file")"
  stderr="$(cat "$stderr_file")"

  assert_eq "make e2e-chaos exits 0 when chaos-mesh namespace present" "0" "$exit_code"
  assert_not_contains "preflight failure message is not emitted on stdout on happy path" \
    "$stdout" "chaos-mesh is not installed"
  assert_not_contains "preflight failure message is not emitted on stderr on happy path" \
    "$stderr" "chaos-mesh is not installed"

  if [ -e "$tmp/CHAINSAW_INVOKED" ]; then
    echo "  PASS: chainsaw was invoked after preflight succeeded"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: chainsaw should be invoked when preflight succeeds"
    FAIL=$((FAIL + 1))
  fi
  assert_contains "the sweep leaves out the multinode suites" \
    "$(cat "$tmp/CHAINSAW_INVOKED" 2>/dev/null)" \
    "--selector chaos.c5c3.io/topology!=multinode tests/e2e-chaos/"
}

# ---------------------------------------------------------------------------
# Test 3: kubectl cannot reach the cluster -> distinct fast-fail message,
# namespace probe AND chainsaw are short-circuited
#
# Pins the split-failure-mode behaviour introduced for the external review
# (berendt I-002): the `kubectl version` probe must run first, emit its own
# actionable message on stderr, and prevent both the `kubectl get ns
# chaos-mesh` probe and the chainsaw invocation from ever running. A
# regression that drops the version probe, swaps the probe order, or alters
# the wording of the cluster-unreachable message would cause this test to
# fail.
# ---------------------------------------------------------------------------
test_fast_fail_when_kubectl_unreachable() {
  echo "Test: e2e-chaos fast-fails with cluster-unreachable message when kubectl version exits non-zero"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # ns_exit=0 would let the namespace probe pass on its own; we set it to 0
  # deliberately so that, if the recipe's preflight order regressed and the
  # version probe were skipped, the test would still observe the wrong
  # (chaos-mesh) message instead of silently passing.
  write_stubs "$tmp" 0 1

  local stdout_file="$tmp/stdout" stderr_file="$tmp/stderr" exit_code
  run_make_e2e_chaos "$tmp" "$stdout_file" "$stderr_file"
  exit_code=$?

  local stdout stderr
  stdout="$(cat "$stdout_file")"
  stderr="$(cat "$stderr_file")"

  assert_nonzero_exit "make e2e-chaos exits non-zero when kubectl version probe fails" "$exit_code"
  assert_contains "stderr names the cluster-unreachable failure mode (distinct from chaos-mesh-missing)" \
    "$stderr" "kubectl is not configured or no cluster is reachable"
  assert_not_contains "stderr does NOT mention chaos-mesh — namespace probe must be short-circuited" \
    "$stderr" "chaos-mesh is not installed"
  assert_not_contains "stdout does NOT mention chaos-mesh — namespace probe must be short-circuited" \
    "$stdout" "chaos-mesh is not installed"

  if [ -e "$tmp/CHAINSAW_INVOKED" ]; then
    echo "  FAIL: chainsaw must NOT be invoked when kubectl is unreachable"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: chainsaw was not invoked when kubectl is unreachable"
    PASS=$((PASS + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 4: e2e-chaos-multinode on a single-node cluster -> fast-fail naming
# the two-worker bring-up, no chainsaw
# ---------------------------------------------------------------------------
test_multinode_fast_fails_without_two_workers() {
  echo "Test: e2e-chaos-multinode fast-fails on a cluster without two workers"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_stubs "$tmp" 0 0 1

  local stdout_file="$tmp/stdout" stderr_file="$tmp/stderr" exit_code
  run_make_e2e_chaos "$tmp" "$stdout_file" "$stderr_file" e2e-chaos-multinode
  exit_code=$?

  local stderr
  stderr="$(cat "$stderr_file")"

  assert_nonzero_exit "make e2e-chaos-multinode exits non-zero with one worker" "$exit_code"
  assert_contains "stderr names the two-worker config" \
    "$stderr" "kind create cluster --name cobaltcore --config hack/kind-config-multinode.yaml"
  assert_contains "stderr names the prepare step" \
    "$stderr" "run tests/e2e-chaos/lib/nodes.sh prepare before make deploy-infra"
  assert_not_contains "the chaos-mesh probe is not reached" \
    "$stderr" "chaos-mesh is not installed"

  if [ -e "$tmp/CHAINSAW_INVOKED" ]; then
    echo "  FAIL: chainsaw must NOT be invoked on a single-node cluster"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: chainsaw was not invoked on a single-node cluster"
    PASS=$((PASS + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 5: e2e-chaos-multinode on two workers -> chainsaw runs the multinode
# suites alone
# ---------------------------------------------------------------------------
test_multinode_runs_the_multinode_suites() {
  echo "Test: e2e-chaos-multinode runs the multinode suites on two workers"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  write_stubs "$tmp" 0 0 2

  local stdout_file="$tmp/stdout" stderr_file="$tmp/stderr" exit_code
  run_make_e2e_chaos "$tmp" "$stdout_file" "$stderr_file" e2e-chaos-multinode
  exit_code=$?

  assert_eq "make e2e-chaos-multinode exits 0 on two workers" "0" "$exit_code"
  assert_contains "chainsaw selects the multinode suites" \
    "$(cat "$tmp/CHAINSAW_INVOKED" 2>/dev/null)" \
    "test --config tests/e2e-chaos/chainsaw-config.yaml --selector chaos.c5c3.io/topology=multinode tests/e2e-chaos/"

  write_stubs "$tmp" 1 0 2
  rm -f "$tmp/CHAINSAW_INVOKED"
  run_make_e2e_chaos "$tmp" "$stdout_file" "$stderr_file" e2e-chaos-multinode
  exit_code=$?
  assert_nonzero_exit "without chaos-mesh it exits non-zero" "$exit_code"
  assert_contains "and says so" "$(cat "$stderr_file")" "chaos-mesh is not installed"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_fast_fail_when_namespace_absent
test_happy_path_when_namespace_present
test_fast_fail_when_kubectl_unreachable
test_multinode_fast_fails_without_two_workers
test_multinode_runs_the_multinode_suites

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
