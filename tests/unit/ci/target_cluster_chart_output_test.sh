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
# three ci.yaml sides are asserted against the workflow file. The shared
# resolve-script scaffolding lives in tests/lib/ci_resolve.sh.
#
# Usage: bash tests/unit/ci/target_cluster_chart_output_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# The resolve script needs a non-empty ALL_OPERATORS to get past its own guard.
# This signal does not compose with any FILTER_<operator>, so any real operator
# name does — the list only has to be non-empty.
ALL_OPERATORS_FIXTURE="keystone c5c3"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Run the resolve script for the given ref and FILTER_target_cluster_chart
# value, and echo the target-cluster-chart line it emits.
run_resolve() {
  local ref="$1" filter="$2"

  resolve_output target-cluster-chart "$ref" "$ALL_OPERATORS_FIXTURE" \
    FILTER_target_cluster_chart="$filter"
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

  assert_eq "the output is emitted even with no FILTER_ env var" \
    "target-cluster-chart=false" \
    "$(resolve_output target-cluster-chart refs/heads/main "$ALL_OPERATORS_FIXTURE")"
}

test_tag_push_forces_the_chart() {
  echo "Test: a v* tag publishes the chart whatever the filter says"

  assert_eq "the release pipeline forces the chart on" \
    "target-cluster-chart=true" "$(run_resolve refs/tags/v1.2.3 false)"
}

test_ci_yaml_wires_both_ends() {
  echo "Test: ci.yaml passes the filter in and reads the output back out"

  assert_filter_is_wired target_cluster_chart target-cluster-chart
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
