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
# Implementation: bash + tests/lib/assertions.sh (project-native, matches the
# sibling tests/unit/hack/deploy_infra_*_test.sh pattern). The repository has
# zero .bats files and no bats binary on CI, so introducing one would add an
# undeclared dependency.
#
# Usage: bash tests/unit/Makefile/test_e2e_chaos_preflight.sh

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

# write_stubs <dir> <kubectl_get_ns_exit> [<kubectl_version_exit>]
# Creates kubectl + chainsaw shims in <dir>:
#   - kubectl ONLY answers the two verbs the `make e2e-chaos` preflight
#     invokes today (`kubectl version` and `kubectl get ns chaos-mesh`);
#     for each verb it exits with the supplied per-verb exit code
#     (<kubectl_version_exit> defaults to 0 = cluster reachable). Any
#     other invocation is treated as a test-harness drift signal and
#     exits non-zero with a marker line on stderr — a future change that
#     adds a third kubectl call expecting fail-fast behaviour will
#     surface here instead of silently passing under a blanket `exit 0`.
#   - chainsaw writes a sentinel file `$dir/CHAINSAW_INVOKED` and exits 0
#     so the test can assert whether the recipe invoked it.
write_stubs() {
  local dir="$1" ns_exit="$2" version_exit="${3:-0}"
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
echo "[kubectl-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/kubectl"

  cat >"$dir/chainsaw" <<STUB
#!/bin/bash
# Test stub record invocation so the test can assert whether the
# Makefile recipe reached the chainsaw step after the preflight.
touch "${dir}/CHAINSAW_INVOKED"
exit 0
STUB
  chmod +x "$dir/chainsaw"
}

# run_make_e2e_chaos <stub_dir> <stdout_file> <stderr_file>
# Runs `make e2e-chaos` against the project Makefile with PATH limited to
# <stub_dir> plus /usr/bin and /bin so make itself, plus core utilities the
# recipe uses (echo), still resolve. Captures stdout and stderr to separate
# files so callers can assert per-stream contents (specifies the remediation message must land on stderr). Returns the make exit code.
run_make_e2e_chaos() {
  local stub_dir="$1" stdout_file="$2" stderr_file="$3"
  (
    PATH="$stub_dir:/usr/bin:/bin"
    export PATH
    cd "$PROJECT_ROOT"
    make -f "$MAKEFILE" e2e-chaos
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
# Run
# ---------------------------------------------------------------------------
test_fast_fail_when_namespace_absent
test_happy_path_when_namespace_present
test_fast_fail_when_kubectl_unreachable

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
