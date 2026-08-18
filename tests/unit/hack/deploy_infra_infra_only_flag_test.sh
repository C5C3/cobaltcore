#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/deploy-infra.sh gates the target-cluster posture behind
# INFRA_ONLY: a cluster deployed with it runs the third-party infrastructure
# and no CobaltCore operator, so the c5c3-operator HelmRelease is suspended AND its
# Deployment scaled to zero whatever the ControlPlane branch above did.
#
# Implementation: bash + tests/lib/assertions.sh, mirroring the sibling
# tests/unit/hack/deploy_infra_chaos_flag_test.sh. The script is sourced (its
# `BASH_SOURCE[0] == ${0}` guard keeps main() from running) to observe the
# resolved default, and grepped to pin the two commands and the banner line.
#
# Usage: bash tests/unit/hack/deploy_infra_infra_only_flag_test.sh

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

# resolve_infra_only [env_var=value...]
# Sources deploy-infra.sh in a subshell with the supplied env overrides and
# echoes the resolved value of INFRA_ONLY.
resolve_infra_only() {
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    printf '%s' "${INFRA_ONLY}"
  )
}

# ---------------------------------------------------------------------------
# Test 1: INFRA_ONLY defaults to false
# ---------------------------------------------------------------------------
test_default_is_false() {
  echo "Test: INFRA_ONLY defaults to false"

  local resolved
  resolved="$(unset INFRA_ONLY; resolve_infra_only)"
  assert_eq "INFRA_ONLY defaults to false" "false" "$resolved"
}

# ---------------------------------------------------------------------------
# Test 2: explicit values pass through verbatim
# A typo like INFRA_ONLY=yes must NOT take the branch: the single gate uses the
# strict `== "true"` comparison, so anything else keeps the operator running.
# ---------------------------------------------------------------------------
test_explicit_values_pass_through() {
  echo "Test: explicit INFRA_ONLY values pass through verbatim"

  assert_eq "INFRA_ONLY=true is preserved" "true" "$(resolve_infra_only INFRA_ONLY=true)"
  assert_eq "INFRA_ONLY=false is preserved" "false" "$(resolve_infra_only INFRA_ONLY=false)"
  assert_eq "INFRA_ONLY=yes is preserved verbatim" "yes" "$(resolve_infra_only INFRA_ONLY=yes)"

  local gate_count
  gate_count="$(grep -cE '"\$\{INFRA_ONLY\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh has exactly 1 strict INFRA_ONLY==true gate" "1" "$gate_count"
}

# ---------------------------------------------------------------------------
# Test 3: the gate suspends the HelmRelease AND scales the Deployment, for
# EVERY CobaltCore operator
# The scale is the half that matters on a re-used cluster: a HelmRelease
# suspended after the chart installed leaves the Deployment running. Covering
# only c5c3 would leave an INFRA_ONLY=true WITH_CONTROLPLANE=true target running
# the five service operators the flux branch un-suspends, and two controller
# sets applying the same children is what the split exists to prevent.
# ---------------------------------------------------------------------------
test_gate_suspends_and_scales() {
  echo "Test: the INFRA_ONLY gate suspends and scales down every CobaltCore operator"

  local gate_line suspend_line scale_line loop_line
  gate_line="$(grep -n '"${INFRA_ONLY}" == "true"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  assert_not_empty "the INFRA_ONLY gate is found" "$gate_line"

  # The suspend patch and the scale must both live inside the gate — i.e. after
  # it and before the closing `fi` of that if-block. Bounding the search at the
  # next line that is exactly `  fi` at the gate's indentation works through the
  # nested for-loop, whose own terminator is `    done`.
  local block_end
  block_end="$(awk -v start="${gate_line:-0}" 'NR > start && /^  fi$/ { print NR; exit }' "$DEPLOY_INFRA_SH")"
  assert_not_empty "the INFRA_ONLY gate block is closed" "$block_end"

  in_gate() {
    grep -n "$1" "$DEPLOY_INFRA_SH" \
      | awk -F: -v lo="${gate_line:-0}" -v hi="${block_end:-0}" '$1 > lo && $1 < hi { print $1; exit }'
  }

  loop_line="$(in_gate 'for operator in ')"
  suspend_line="$(in_gate 'kubectl patch helmrelease "\${operator%%:\*}-operator"')"
  scale_line="$(in_gate 'kubectl scale deployment -n "\${operator##\*:}"')"

  assert_not_empty "the operator loop is inside the gate" "$loop_line"
  assert_not_empty "the suspend patch is inside the gate" "$suspend_line"
  assert_not_empty "the scale-to-zero is inside the gate" "$scale_line"

  # All six: c5c3 plus the five service operators the WITH_CONTROLPLANE=true /
  # flux branch un-suspends right above this gate.
  local operator
  for operator in c5c3:c5c3-system keystone:keystone-system horizon:horizon-system \
                  glance:glance-system placement:placement-system barbican:barbican-system; do
    assert_not_empty "the loop covers ${operator%%:*}-operator" \
      "$(awk -v lo="${gate_line:-0}" -v hi="${block_end:-0}" -v want="$operator" \
        'NR > lo && NR < hi && index($0, want) { print NR; exit }' "$DEPLOY_INFRA_SH")"
  done

  # Both tolerate absence: on a fresh cluster the HelmRelease may not be applied
  # yet, and the Deployment exists only where the chart already installed once.
  # Without the `|| true` a missing object aborts the whole deploy under
  # `set -euo pipefail`.
  assert_not_empty "the scale tolerates a missing Deployment" \
    "$(in_gate 'kubectl scale deployment .* --replicas=0 2>/dev/null || true')"
}

# ---------------------------------------------------------------------------
# Test 4: the gate runs after the three-way ControlPlane branch
# INFRA_ONLY has to override every branch, including the WITH_CONTROLPLANE ones
# that deploy the operator. A gate placed inside the else-branch would leave a
# WITH_CONTROLPLANE target cluster running one.
# ---------------------------------------------------------------------------
test_gate_runs_after_the_controlplane_branch() {
  echo "Test: the INFRA_ONLY gate runs after the ControlPlane suspend branch"

  local flux_branch_line gate_line
  flux_branch_line="$(grep -n '"${WITH_CONTROLPLANE}" == "true" && "${CONTROLPLANE_OPERATORS}" == "flux"' \
    "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"
  gate_line="$(grep -n '"${INFRA_ONLY}" == "true"' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)"

  assert_not_empty "the ControlPlane flux branch is found" "$flux_branch_line"
  assert_not_empty "the INFRA_ONLY gate is found" "$gate_line"

  if [ "${gate_line:-0}" -gt "${flux_branch_line:-0}" ]; then
    echo "  PASS: the INFRA_ONLY gate sits after the ControlPlane branch"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the INFRA_ONLY gate sits at line ${gate_line} — before the ControlPlane branch at ${flux_branch_line}; a WITH_CONTROLPLANE deploy would re-deploy the operator after it"
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 5: the run banner reports the resolved value
# The banner is the only place a mistyped INFRA_ONLY surfaces before the deploy
# has finished, so it must name the variable and its override.
# ---------------------------------------------------------------------------
test_banner_reports_the_flag() {
  echo "Test: main()'s banner reports INFRA_ONLY"

  assert_file_contains "the banner echoes the resolved INFRA_ONLY value" \
    "$DEPLOY_INFRA_SH" \
    'Infrastructure only : ${INFRA_ONLY}'
  assert_file_contains "the banner names the override" \
    "$DEPLOY_INFRA_SH" \
    'set INFRA_ONLY=true'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_default_is_false
test_explicit_values_pass_through
test_gate_suspends_and_scales
test_gate_runs_after_the_controlplane_branch
test_banner_reports_the_flag

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
