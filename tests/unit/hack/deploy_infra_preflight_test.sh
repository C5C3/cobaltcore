#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh `preflight_checks` after the FluxInstance
# bootstrap migration: the Flux CLI is no longer required, but kubectl still
# is (CC-0085, REQ-005).
#
# DECISION: bats vs project-native bash test runner
# Ambiguity: task spec asked for `*.bats`, but the repo has zero .bats files,
#   no bats binary anywhere in CI, and a Makefile that only wires `make
#   shellcheck`. The established pattern is bash + tests/lib/assertions.sh
#   (see tests/unit/deploy/flux_system_fluxinstance_test.sh).
# Chose: project-native bash test (tests/lib/assertions.sh).
# Reason: introducing bats would add an undeclared CI dependency for a
#   single-feature test pair while the existing helpers cover the assertions
#   needed here.
# Reviewer: please verify this matches intent — switching to bats later is a
#   mechanical rewrite if you prefer.
#
# Usage: bash tests/unit/hack/deploy_infra_preflight_test.sh

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

# make_stub_path <dir> <cmd>...
# Creates an executable shim for each <cmd> inside <dir> that always exits 0.
# Use to populate a fake PATH containing only the listed commands.
make_stub_path() {
  local dir="$1"
  shift
  mkdir -p "$dir"
  for cmd in "$@"; do
    cat >"$dir/$cmd" <<'STUB'
#!/bin/bash
exit 0
STUB
    chmod +x "$dir/$cmd"
  done
}

# run_preflight <stub_dir>
# Sources deploy-infra.sh in a subshell with PATH limited to <stub_dir> and
# invokes preflight_checks. Echoes combined stdout/stderr; returns the exit
# status of preflight_checks.
run_preflight() {
  local stub_dir="$1"
  (
    PATH="$stub_dir"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    preflight_checks
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: preflight passes when flux is absent (CC-0085, REQ-005)
# ---------------------------------------------------------------------------
test_preflight_passes_without_flux() {
  echo "Test: preflight_checks passes without flux on PATH (CC-0085, REQ-005)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Provide every required tool EXCEPT flux. `docker info` must succeed,
  # so the stub returns 0 unconditionally.
  make_stub_path "$tmp" docker kind kubectl jq

  local output exit_code
  output="$(run_preflight "$tmp")"
  exit_code=$?

  assert_eq "preflight_checks exits 0 without flux on PATH" "0" "$exit_code"
  assert_contains "preflight log reports success" "$output" "Pre-flight checks passed."
  assert_not_contains "preflight does not mention flux as missing" "$output" "'flux' is not installed"
}

# ---------------------------------------------------------------------------
# Test 2: preflight fails when kubectl is absent (CC-0085, REQ-005)
# ---------------------------------------------------------------------------
test_preflight_fails_without_kubectl() {
  echo "Test: preflight_checks fails when kubectl is missing (CC-0085, REQ-005)"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Provide everything except kubectl.
  make_stub_path "$tmp" docker kind jq

  local output exit_code
  output="$(run_preflight "$tmp")"
  exit_code=$?

  assert_nonzero_exit "preflight_checks exits non-zero without kubectl" "$exit_code"
  assert_contains "preflight reports kubectl as missing" "$output" "'kubectl' is not installed"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_preflight_passes_without_flux
test_preflight_fails_without_kubectl

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
