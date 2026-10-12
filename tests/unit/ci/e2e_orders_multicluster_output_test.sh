#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the e2e-orders-multicluster change signal reaches the
# e2e-orders-multicluster job in .github/workflows/ci.yaml, and that the job
# brings up the lean stack and the two-key registration its suite needs.
#
# The signal crosses four places and a mismatch in any of them is silent:
# GitHub Actions resolves an unknown `needs.<job>.outputs.<name>` to the empty
# string rather than failing, so a renamed output or an unwired FILTER_ env var
# leaves the job permanently skipped and the delivery of an order to a target
# cluster permanently unexercised.
#
# hack/ci-resolve-changes.sh is executed for real in all of its branches; the
# ci.yaml sides are asserted against the workflow file. Modelled on the sibling
# tests/unit/ci/e2e_multicluster_output_test.sh, with the shared resolve-script
# scaffolding in tests/lib/ci_resolve.sh.
#
# Usage: bash tests/unit/ci/e2e_orders_multicluster_output_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# The resolve script reads FILTER_${op} only for operators named here, so a
# list without keystone would make the FILTER_keystone scenario below assert
# nothing, and one without c5c3 the FILTER_c5c3 one.
ALL_OPERATORS_FIXTURE="keystone c5c3 barbican ovn neutron"

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

# Run the resolve script for the given ref and the job's own FILTER_ value, and
# echo the e2e-orders-multicluster line it emits. Extra FILTER_ assignments and
# PR_LABELS are passed through, so each input is exercised on its own.
run_resolve() {
  local ref="$1" filter="$2"
  shift 2

  resolve_output e2e-orders-multicluster "$ref" "$ALL_OPERATORS_FIXTURE" \
    FILTER_tests_orders_multicluster="$filter" "$@"
}

# The job's text: from its key to the next job key or top-level comment, so
# an assertion cannot pass on a line another job carries.
job_text() {
  awk '
    /^  e2e-orders-multicluster:$/ { in_job = 1; next }
    in_job && /^  ([a-z0-9-]+:$|#)/ { exit }
    in_job { print }
  ' "$CI_YAML"
}

# One step of the job, from its name to the next step.
job_step() { # <step name>
  job_text | awk -v name="      - name: $1" '
    $0 == name { in_step = 1; next }
    in_step && /^      - / { exit }
    in_step { print }
  '
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_own_filter_is_honoured() {
  echo "Test: on a branch the job follows its own filter"

  assert_eq "a changed suite or chart is signalled" \
    "e2e-orders-multicluster=true" "$(run_resolve refs/heads/main true)"
  assert_eq "an untouched suite is not signalled" \
    "e2e-orders-multicluster=false" "$(run_resolve refs/heads/main false)"
}

test_c5c3_and_the_labels_schedule_the_job() {
  echo "Test: a c5c3 change and the controlplane, multicluster and full labels schedule the job"

  # The suite is where an order's delivery to a target cluster is exercised,
  # so the c5c3-operator's own changes pay for it, as they pay for the
  # ControlPlane jobs.
  assert_eq "a c5c3 change schedules the job" \
    "e2e-orders-multicluster=true" "$(run_resolve refs/heads/main false FILTER_c5c3=true)"
  assert_eq "ci:controlplane schedules the job" \
    "e2e-orders-multicluster=true" \
    "$(run_resolve refs/heads/main false PR_LABELS='["ci:controlplane"]')"
  assert_eq "ci:multicluster schedules the job" \
    "e2e-orders-multicluster=true" \
    "$(run_resolve refs/heads/main false PR_LABELS='["ci:multicluster"]')"
  assert_eq "ci:full schedules the job" \
    "e2e-orders-multicluster=true" \
    "$(run_resolve refs/heads/main false PR_LABELS='["ci:full"]')"
}

test_other_inputs_do_not_schedule_the_job() {
  echo "Test: a keystone change, a shared Go change and another suite leave the job off"

  # Two kind clusters on a self-hosted runner: a keystone one-liner or a
  # shared Go edit must not pay for them.
  assert_eq "a keystone change does not schedule the job" \
    "e2e-orders-multicluster=false" "$(run_resolve refs/heads/main false FILTER_keystone=true)"
  assert_eq "a shared Go change does not schedule the job" \
    "e2e-orders-multicluster=false" "$(run_resolve refs/heads/main false FILTER_go_common=true)"
  assert_eq "another operator's suite does not schedule the job" \
    "e2e-orders-multicluster=false" "$(run_resolve refs/heads/main false FILTER_tests_e2e_glance=true)"
  assert_eq "the placed-services suite does not schedule the job" \
    "e2e-orders-multicluster=false" "$(run_resolve refs/heads/main false FILTER_tests_multicluster=true)"
}

test_unset_filter_defaults_to_false() {
  echo "Test: an unwired filter defaults to false rather than tripping set -u"

  assert_eq "the output is emitted even with no FILTER_ env var" \
    "e2e-orders-multicluster=false" \
    "$(resolve_output e2e-orders-multicluster refs/heads/main "$ALL_OPERATORS_FIXTURE")"
}

test_tag_push_forces_the_job() {
  echo "Test: a v* tag runs the job whatever the filter says"

  assert_eq "the release pipeline forces the job on" \
    "e2e-orders-multicluster=true" "$(run_resolve refs/tags/v1.2.3 false)"
}

test_ci_yaml_wires_all_four_sides() {
  echo "Test: ci.yaml declares the filter, passes it in, exports it and gates on it"

  assert_filter_is_wired tests_orders_multicluster e2e-orders-multicluster
  assert_file_contains "the job gates on it" "$CI_YAML" \
    "needs.changes.outputs.e2e-orders-multicluster == 'true'"
}

test_filter_covers_the_suite_and_the_chart() {
  echo "Test: the filter lists the suite and the chart the job runs on"

  # The filter block ends at the next filter key at the same indent.
  local block
  block=$(awk '
    /^            tests_orders_multicluster:$/ { in_block = 1; next }
    in_block && /^            [a-z0-9_]+:$/ { exit }
    in_block { print }
  ' "$CI_YAML")

  assert_contains "the filter lists the orders suite" \
    "$block" "tests/e2e-orders-multicluster/**"
  # The chart's assigned Role is what the c5c3-operator writes the orders'
  # status and Secrets through on the target.
  assert_contains "the filter lists the target-cluster chart" \
    "$block" "deploy/target-cluster/**"
  assert_not_contains "the filter does not carry the hack scripts" \
    "$block" "hack/**"
}

test_job_runs_the_makefile_target() {
  echo "Test: the job runs the Makefile target rather than an inline chainsaw call"

  # CI-to-Makefile parity: a developer reproduces the job by running the same
  # target.
  assert_contains "the job invokes make e2e-orders-multicluster" \
    "$(job_text)" "make e2e-orders-multicluster"

  local makefile="$PROJECT_ROOT/Makefile"
  assert_file_contains "the Makefile declares the target" "$makefile" \
    "^e2e-orders-multicluster:"
  assert_file_contains "the target runs the suite through its own config" "$makefile" \
    "chainsaw test --config tests/e2e-orders-multicluster/chainsaw-config.yaml tests/e2e-orders-multicluster/"
}

test_job_deploys_the_lean_stack() {
  echo "Test: the job deploys the keystone- and c5c3-operator, K-ORC and nothing else"

  local job
  job="$(job_text)"
  assert_not_empty "the job exists" "$job"
  assert_contains "the job deploys the keystone-operator" "$job" "OPERATOR: keystone"
  assert_contains "the job deploys the c5c3-operator" "$job" "OPERATOR: c5c3"
  assert_contains "the job deploys K-ORC" "$job" "hack/ci-deploy-korc.sh"
  assert_contains "the infrastructure runs the operators outside Flux" \
    "$job" "CONTROLPLANE_OPERATORS: external"
  local op
  for op in horizon glance placement barbican ovn neutron cinder nova; do
    assert_not_contains "the job deploys no ${op}-operator" "$job" "OPERATOR: ${op}"
  done
  # The suite's plane brings its own bus, and nothing in it mounts a share or
  # opens a Geneve tunnel. The keys are matched as env keys, so the comment
  # that says why they are absent does not count.
  assert_not_contains "the job installs no NFS export" "$job" "WITH_NFS:"
  assert_not_contains "the job installs no shared broker" "$job" "WITH_MESSAGING:"
  assert_not_contains "the job loads no OVN kernel modules" "$job" "WITH_OVN_KERNEL_MODULES:"
  assert_contains "the suite's guard fails rather than skips" \
    "$job" 'E2E_REQUIRE_CONTROLPLANE_STACK: "true"'
}

test_job_registers_two_keys_from_the_chart_token() {
  echo "Test: the registration mints the chart's token and carries both scope keys"

  local block
  block="$(job_step "Register the target cluster")"
  assert_not_empty "the registration step exists" "$block"
  assert_contains "the registration reads the chart token Secret" \
    "$block" "target-cluster-access-token"
  assert_contains "the registration sets a token credential" \
    "$block" "kubectl config set-credentials"
  assert_contains "the Secret carries the placed namespace" \
    "$block" "--from-literal=namespaces=openstack"
  # Under the namespaces key the keystone-operator's informers would span the
  # tenant namespaces, where the chart grants it nothing.
  assert_contains "the Secret carries the tenant namespaces under the second key" \
    "$block" "--from-literal=assignedNamespaces=orders-tenant,orders-outsider"
  assert_contains "the Secret carries the provider label" \
    "$block" "sigs.k8s.io/multicluster-runtime-kubeconfig=true"
  assert_not_contains "the registration does not embed an admin kubeconfig" \
    "$block" "kubeconfig=\$workdir/internal.kubeconfig"

  local chart
  chart="$(job_step "Install the target-cluster-access chart on the target")"
  assert_contains "the chart assigns the same two namespaces" \
    "$chart" "assignedNamespaces={orders-tenant,orders-outsider}"
  assert_contains "the chart places openstack" "$chart" "namespaces={openstack}"
}

test_job_waits_for_both_operators_to_engage() {
  echo "Test: the job waits until the keystone- and the c5c3-operator engaged the target"

  local block
  block="$(job_step "Wait for both operators to engage the target")"
  assert_not_empty "the wait step exists" "$block"
  assert_contains "the wait reads the engagement line" "$block" "cluster engaged"
  assert_contains "the wait reads the keystone-operator" "$block" "keystone-system keystone-operator"
  assert_contains "the wait reads the c5c3-operator" "$block" "c5c3-system c5c3-operator"
}

test_job_checks_the_keystone_operator_stays_off_the_assigned_namespaces() {
  echo "Test: after the suite, a forbidden LIST of the keystone-operator in an assigned namespace fails the job"

  local block
  block="$(job_step "Check the keystone-operator never lists an assigned namespace")"
  assert_not_empty "the check step exists" "$block"
  assert_contains "the check reads the keystone-operator" \
    "$block" "-n keystone-system -l app.kubernetes.io/name=keystone-operator"
  assert_contains "the check looks for a refused LIST" "$block" "grep 'forbidden'"
  assert_contains "the check names both assigned namespaces" "$block" "orders-(tenant|outsider)"

  # Engagement waits for no informer, so the check runs once the informers
  # have long had their first LIST: after the suite.
  local steps suite_at check_at
  steps="$(job_text | grep -n '^      - name: ')"
  suite_at="$(grep -m1 'Run the orders two-cluster E2E suite' <<<"$steps" | cut -d: -f1)"
  check_at="$(grep -m1 'Check the keystone-operator never lists' <<<"$steps" | cut -d: -f1)"
  assert_gte "the check runs after the suite" "${check_at:-0}" "$((${suite_at:-0} + 1))"
}

test_job_creates_two_clusters_through_the_action() {
  echo "Test: the job creates both clusters through the composite action, the target without a config"

  local job creates
  job="$(job_text)"
  creates="$(grep -c "^          cluster-name: " <<<"$job" | tr -d ' ')"
  assert_eq "the job creates two clusters" "2" "$creates"
  assert_eq "both through the composite action" "2" \
    "$(grep -c "uses: ./.github/actions/create-kind-cluster$" <<<"$job" | tr -d ' ')"
  local target_step
  target_step="$(job_step "Create target kind cluster")"
  assert_contains "the target step names the target cluster" \
    "$target_step" 'cluster-name: ${{ env.TARGET_CLUSTER }}'
  assert_not_contains "the bare target takes no kind config" "$target_step" "config:"
}

test_cleanup_waits_for_the_job() {
  echo "Test: cleanup-e2e-tags waits for the job before it prunes the images"

  local needs
  needs="$(awk '/^  cleanup-e2e-tags:$/ { getline; print; exit }' "$CI_YAML")"
  assert_contains "cleanup-e2e-tags needs the job" "$needs" "e2e-orders-multicluster"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_own_filter_is_honoured
test_c5c3_and_the_labels_schedule_the_job
test_other_inputs_do_not_schedule_the_job
test_unset_filter_defaults_to_false
test_tag_push_forces_the_job
test_ci_yaml_wires_all_four_sides
test_filter_covers_the_suite_and_the_chart
test_job_runs_the_makefile_target
test_job_deploys_the_lean_stack
test_job_registers_two_keys_from_the_chart_token
test_job_waits_for_both_operators_to_engage
test_job_checks_the_keystone_operator_stays_off_the_assigned_namespaces
test_job_creates_two_clusters_through_the_action
test_cleanup_waits_for_the_job

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
