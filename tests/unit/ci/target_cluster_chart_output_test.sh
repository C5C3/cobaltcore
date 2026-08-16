#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the target-cluster-chart change signal reaches the
# helm-push-target-cluster job in .github/workflows/ci.yaml.
#
# The signal crosses four places, and a mismatch in any of them is silent:
# GitHub Actions resolves an unknown `needs.<job>.outputs.<name>` to the empty
# string rather than failing, so a renamed output or an unwired FILTER_ env var
# leaves the job permanently skipped and the chart permanently unpublished.
#
# hack/ci-resolve-changes.sh is executed for real in both of its branches; the
# three ci.yaml sides are asserted against the workflow file.
#
# Usage: bash tests/unit/ci/target_cluster_chart_output_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
RESOLVE_SH="$PROJECT_ROOT/hack/ci-resolve-changes.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Run the resolve script for the given ref and FILTER_target_cluster_chart
# value, and echo the target-cluster-chart line it emits.
run_resolve() {
  local ref="$1" filter="$2"
  local out
  out=$(mktemp)

  ALL_OPERATORS="keystone c5c3" \
    GITHUB_OUTPUT="$out" \
    GITHUB_REF="$ref" \
    FILTER_target_cluster_chart="$filter" \
    bash "$RESOLVE_SH" >/dev/null 2>&1

  grep '^target-cluster-chart=' "$out"
  rm -f "$out"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_branch_push_passes_the_filter_through() {
  echo "Test: on a branch the output mirrors FILTER_target_cluster_chart"

  assert_eq "a changed chart is signalled" \
    "target-cluster-chart=true" "$(run_resolve refs/heads/main true)"
  assert_eq "an untouched chart is not signalled" \
    "target-cluster-chart=false" "$(run_resolve refs/heads/main false)"
}

test_unset_filter_defaults_to_false() {
  echo "Test: an unwired filter defaults to false rather than tripping set -u"

  local out
  out=$(mktemp)
  ALL_OPERATORS="keystone" GITHUB_OUTPUT="$out" GITHUB_REF=refs/heads/main \
    bash "$RESOLVE_SH" >/dev/null 2>&1
  assert_eq "the output is emitted even with no FILTER_ env var" \
    "target-cluster-chart=false" "$(grep '^target-cluster-chart=' "$out")"
  rm -f "$out"
}

test_tag_push_forces_the_chart() {
  echo "Test: a v* tag publishes the chart whatever the filter says"

  assert_eq "the release pipeline forces the chart on" \
    "target-cluster-chart=true" "$(run_resolve refs/tags/v1.2.3 false)"
}

test_ci_yaml_wires_both_ends() {
  echo "Test: ci.yaml passes the filter in and reads the output back out"

  assert_file_contains "the paths filter exists" "$CI_YAML" \
    "target_cluster_chart:"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    'FILTER_target_cluster_chart: ${{ steps.filter.outputs.target_cluster_chart }}'
  assert_file_contains "the changes job exports the output" "$CI_YAML" \
    'target-cluster-chart: ${{ steps.result.outputs.target-cluster-chart }}'
  assert_file_contains "the push job gates on it" "$CI_YAML" \
    "needs.changes.outputs.target-cluster-chart == 'true'"
}

test_helm_filter_covers_the_chart() {
  echo "Test: a chart change also re-runs helm-validate"

  # The helm filter block ends at the next filter key at the same indent.
  local helm_block
  helm_block=$(awk '
    /^            helm:$/ { in_block = 1; next }
    in_block && /^            [a-z_]+:$/ { exit }
    in_block { print }
  ' "$CI_YAML")

  assert_contains "the helm filter lists the target-cluster path" \
    "$helm_block" "deploy/target-cluster/**"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_branch_push_passes_the_filter_through
test_unset_filter_defaults_to_false
test_tag_push_forces_the_chart
test_ci_yaml_wires_both_ends
test_helm_filter_covers_the_chart

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
