#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the e2e-multicluster change signal reaches the e2e-multicluster job in
# .github/workflows/ci.yaml.
#
# The signal crosses four places and a mismatch in any of them is silent:
# GitHub Actions resolves an unknown `needs.<job>.outputs.<name>` to the empty
# string rather than failing, so a renamed output or an unwired FILTER_ env var
# leaves the two-cluster job permanently skipped and the placement path
# permanently unexercised.
#
# hack/ci-resolve-changes.sh is executed for real in all of its branches; the
# ci.yaml sides are asserted against the workflow file. Modelled on the sibling
# tests/unit/ci/target_cluster_chart_output_test.sh.
#
# Usage: bash tests/unit/ci/e2e_multicluster_output_test.sh

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

# Run the resolve script for the given ref and FILTER_ values, and echo the
# e2e-multicluster line it emits. Extra FILTER_ assignments are passed through
# so the composed shape (own filter, go change, any e2e test change) can be
# exercised one input at a time.
run_resolve() {
  local ref="$1" filter="$2"
  shift 2
  local out
  out=$(mktemp)

  env "$@" \
    ALL_OPERATORS="keystone barbican" \
    GITHUB_OUTPUT="$out" \
    GITHUB_REF="$ref" \
    FILTER_e2e_multicluster="$filter" \
    bash "$RESOLVE_SH" >/dev/null 2>&1

  grep '^e2e-multicluster=' "$out"
  rm -f "$out"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_own_filter_is_honoured() {
  echo "Test: on a branch the job follows its own filter"

  assert_eq "a changed suite is signalled" \
    "e2e-multicluster=true" "$(run_resolve refs/heads/main true)"
  assert_eq "an untouched suite is not signalled" \
    "e2e-multicluster=false" "$(run_resolve refs/heads/main false)"
}

test_composed_shape_matches_e2e_controlplane() {
  echo "Test: a Go change and an E2E test change both force the job on"

  assert_eq "an operator code change forces the job on" \
    "e2e-multicluster=true" "$(run_resolve refs/heads/main false FILTER_keystone=true)"
  assert_eq "an E2E test-definition change forces the job on" \
    "e2e-multicluster=true" "$(run_resolve refs/heads/main false FILTER_tests_e2e_operator=true)"
}

test_unset_filter_defaults_to_false() {
  echo "Test: an unwired filter defaults to false rather than tripping set -u"

  local out
  out=$(mktemp)
  ALL_OPERATORS="keystone" GITHUB_OUTPUT="$out" GITHUB_REF=refs/heads/main \
    bash "$RESOLVE_SH" >/dev/null 2>&1
  assert_eq "the output is emitted even with no FILTER_ env var" \
    "e2e-multicluster=false" "$(grep '^e2e-multicluster=' "$out")"
  rm -f "$out"
}

test_tag_push_forces_the_job() {
  echo "Test: a v* tag runs the job whatever the filter says"

  assert_eq "the release pipeline forces the job on" \
    "e2e-multicluster=true" "$(run_resolve refs/tags/v1.2.3 false)"
}

test_ci_yaml_wires_all_four_sides() {
  echo "Test: ci.yaml declares the filter, passes it in, exports it and gates on it"

  assert_file_contains "the paths filter exists" "$CI_YAML" \
    "e2e_multicluster:"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    'FILTER_e2e_multicluster: ${{ steps.filter.outputs.e2e_multicluster }}'
  assert_file_contains "the changes job exports the output" "$CI_YAML" \
    'e2e-multicluster: ${{ steps.result.outputs.e2e-multicluster }}'
  assert_file_contains "the job gates on it" "$CI_YAML" \
    "needs.changes.outputs.e2e-multicluster == 'true'"
}

test_filter_covers_the_suite_and_the_chart() {
  echo "Test: the filter lists the suite and the chart the job runs on"

  # The filter block ends at the next filter key at the same indent.
  local block
  block=$(awk '
    /^            e2e_multicluster:$/ { in_block = 1; next }
    in_block && /^            [a-z_]+:$/ { exit }
    in_block { print }
  ' "$CI_YAML")

  assert_contains "the filter lists the two-cluster suite" \
    "$block" "tests/e2e-multicluster/**"
  assert_contains "the filter lists the target-cluster chart" \
    "$block" "deploy/target-cluster/**"
  assert_contains "the filter lists the hack scripts" \
    "$block" "hack/**"
}

test_job_runs_the_makefile_target() {
  echo "Test: the job runs the Makefile target rather than an inline chainsaw call"

  # CI-to-Makefile parity: a developer reproduces the job by running the same
  # target, so the job must not drift into its own chainsaw invocation.
  assert_file_contains "the job invokes make e2e-multicluster" "$CI_YAML" \
    "make e2e-multicluster"

  local makefile="$PROJECT_ROOT/Makefile"
  assert_file_contains "the Makefile declares the target" "$makefile" \
    "^e2e-multicluster:"
  assert_file_contains "the target runs the suite through its own config" "$makefile" \
    "chainsaw test --config tests/e2e-multicluster/chainsaw-config.yaml tests/e2e-multicluster/"
}

test_job_never_registers_an_admin_kubeconfig() {
  echo "Test: the registration step mints the chart's token instead of taking the admin kubeconfig"

  # The registration Secret must carry the ServiceAccount token. `kind get
  # kubeconfig` produces a client certificate for kubernetes-admin, and piping
  # that into the Secret would let the job pass with any grant set at all —
  # which is the one thing this job exists to check.
  local block
  block=$(awk '
    /^      - name: Register the target cluster$/ { in_block = 1; next }
    in_block && /^      - name: / { exit }
    in_block { print }
  ' "$CI_YAML")

  assert_not_empty "the registration step exists" "$block"
  assert_contains "the registration reads the chart token Secret" \
    "$block" "target-cluster-access-token"
  assert_contains "the registration sets a token credential" \
    "$block" "kubectl config set-credentials"
  assert_contains "the Secret carries the namespaces key" \
    "$block" "namespaces=openstack"
  assert_contains "the Secret carries the provider label" \
    "$block" "sigs.k8s.io/multicluster-runtime-kubeconfig=true"
  assert_not_contains "the registration does not embed an admin kubeconfig" \
    "$block" "kubeconfig=\$workdir/internal.kubeconfig"
}

test_setup_action_threads_the_target_flags() {
  echo "Test: the composite action threads INFRA_ONLY and CLUSTER_NAME"

  local action="$PROJECT_ROOT/.github/actions/setup-e2e-infra/action.yaml"
  assert_file_contains "INFRA_ONLY reaches deploy-infra.sh" "$action" \
    'INFRA_ONLY: ${{ env.INFRA_ONLY }}'
  assert_file_contains "CLUSTER_NAME reaches deploy-infra.sh" "$action" \
    'CLUSTER_NAME: ${{ env.CLUSTER_NAME }}'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_own_filter_is_honoured
test_composed_shape_matches_e2e_controlplane
test_unset_filter_defaults_to_false
test_tag_push_forces_the_job
test_ci_yaml_wires_all_four_sides
test_filter_covers_the_suite_and_the_chart
test_job_runs_the_makefile_target
test_job_never_registers_an_admin_kubeconfig
test_setup_action_threads_the_target_flags

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
