#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-changes.sh emits the e2e-nova-libvirt change signal and
# that .github/workflows/ci.yaml wires it up.
#
# The signal crosses three places and a mismatch in any of them is silent:
# GitHub Actions resolves an unknown `needs.<job>.outputs.<name>` to the empty
# string rather than failing, so a renamed output or an unwired FILTER_ env var
# leaves the libvirt job permanently skipped and the libvirt path of
# nova-compute permanently unexercised.
#
# The resolve script is executed for real in all of its branches; the ci.yaml
# sides are asserted against the workflow file. The last two tests follow the
# signal to its two consumers: the job that reads it, and the Makefile target
# that job calls, which is also how a developer reproduces the job locally.
#
# Modelled on the sibling ci_resolve_changes_ovn_overlay_test.sh, with the
# shared scaffolding in tests/lib/ci_resolve.sh and tests/lib/ci_yaml.sh.
#
# Usage: bash tests/unit/hack/ci_resolve_changes_nova_libvirt_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC2034 # read by tests/lib/ci_resolve.sh and tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
MAKEFILE="$PROJECT_ROOT/Makefile"

# The resolve script reads FILTER_${op} only for operators named here, so nova
# must be in the list for the operator-change scenario to assert anything.
ALL_OPERATORS_FIXTURE="keystone nova"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Run the resolve script for the given ref and suite filter value, and echo the
# e2e-nova-libvirt line it emits. Extra FILTER_ assignments are passed through
# so each input can be exercised alone.
run_resolve() {
  local ref="$1" filter="$2"
  shift 2

  resolve_output e2e-nova-libvirt "$ref" "$ALL_OPERATORS_FIXTURE" \
    FILTER_tests_nova_libvirt="$filter" "$@"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_each_input_schedules_the_job() {
  echo "Test: the suite, the nova-operator and the Nova images each schedule the job"

  assert_eq "a changed suite schedules the job" \
    "e2e-nova-libvirt=true" "$(run_resolve refs/heads/main true)"
  assert_eq "a nova-operator change schedules the job" \
    "e2e-nova-libvirt=true" "$(run_resolve refs/heads/main false FILTER_nova=true)"
  assert_eq "a Nova or nova-compute image change schedules the job" \
    "e2e-nova-libvirt=true" "$(run_resolve refs/heads/main false FILTER_image_nova=true)"
}

test_other_changes_do_not_schedule_the_job() {
  echo "Test: nothing else schedules the job"

  assert_eq "an untouched suite is not signalled" \
    "e2e-nova-libvirt=false" "$(run_resolve refs/heads/main false)"
  # The job brings up a full Nova stack alone on a self-hosted runner, so it
  # follows the overlay rather than the upgrade suite: a shared Go change puts
  # every operator in op_changed and must not pull this job in behind them.
  assert_eq "a shared Go change does not schedule the job" \
    "e2e-nova-libvirt=false" "$(run_resolve refs/heads/main false FILTER_go_common=true)"
  assert_eq "another operator's change does not schedule the job" \
    "e2e-nova-libvirt=false" "$(run_resolve refs/heads/main false FILTER_keystone=true)"
  assert_eq "an ovn-operator change does not schedule the job" \
    "e2e-nova-libvirt=false" "$(run_resolve refs/heads/main false FILTER_ovn=true)"
  # The cheap changes resolve_changes_scenarios_test.sh keeps every expensive
  # job off for: the canary's two inputs, and another nova suite's edit.
  assert_eq "a substrate change does not schedule the job" \
    "e2e-nova-libvirt=false" "$(run_resolve refs/heads/main false FILTER_e2e_shared=true)"
  assert_eq "a workflow plumbing change does not schedule the job" \
    "e2e-nova-libvirt=false" "$(run_resolve refs/heads/main false FILTER_ci_plumbing=true)"
  assert_eq "another nova suite's edit does not schedule the job" \
    "e2e-nova-libvirt=false" "$(run_resolve refs/heads/main false FILTER_tests_e2e_nova=true)"
}

test_unset_filter_defaults_to_false() {
  echo "Test: an unwired filter defaults to false rather than tripping set -u"

  # The script runs under `set -u`, so the `:-false` default of filter_on is
  # the only thing between a filter that ci.yaml does not pass in and an
  # aborted changes job. resolve_output prints the exit status instead of the
  # line when the script fails, so the equality also proves the zero exit.
  assert_eq "the output is emitted even with no FILTER_ env var" \
    "e2e-nova-libvirt=false" \
    "$(resolve_output e2e-nova-libvirt refs/heads/main "$ALL_OPERATORS_FIXTURE")"
}

test_noop_run_still_emits_the_output() {
  echo "Test: the labeled no-op path emits an explicit false"

  # An output the no-op path forgets resolves to the empty string in the
  # consuming job, which is neither 'true' nor a failure, only a silent skip.
  assert_eq "the no-op path emits the output" \
    "e2e-nova-libvirt=false" \
    "$(run_resolve refs/heads/main true EVENT_ACTION=labeled EVENT_LABEL=bug)"
}

test_forced_runs_schedule_the_job() {
  echo "Test: ci:full and a v* tag set the output whatever the filters say"

  assert_eq "ci:full schedules the job" \
    "e2e-nova-libvirt=true" "$(run_resolve refs/heads/main false PR_LABELS='["ci:full"]')"
  # The output has no consumer on a tag: the job runs on pull requests only,
  # which test_the_job_reads_the_signal asserts.
  assert_eq "a v* tag sets the output" \
    "e2e-nova-libvirt=true" "$(run_resolve refs/tags/v1.2.3 false)"
}

test_the_signal_reaches_the_image_build() {
  echo "Test: scheduling the job also schedules the images it loads"

  # The job gates on `needs.build-e2e-images.result == 'success'`, so a suite
  # edit that switches the job on without switching the build on leaves it
  # skipped on a dependency that never ran. No operator is changed here, so
  # only the libvirt signal can switch the build on.
  assert_contains "a suite-only change still builds the E2E images" \
    "$(resolve_outputs refs/heads/main "$ALL_OPERATORS_FIXTURE" \
      FILTER_tests_nova_libvirt=true)" \
    "build-e2e-images=true"
}

test_ci_yaml_wires_the_signal() {
  echo "Test: ci.yaml declares the filter, passes it in and exports the output"

  assert_filter_is_wired tests_nova_libvirt e2e-nova-libvirt
}

test_filter_names_the_suite_and_its_fixtures() {
  echo "Test: the filter lists the suite and the pool fixtures it applies"

  local block
  block=$(filter_block tests_nova_libvirt)

  assert_contains "the filter lists the libvirt suite" \
    "$block" "tests/e2e-nova-libvirt/**"
  assert_contains "the filter lists the fixtures the suite applies by path" \
    "$block" "tests/e2e/nova/compute-node-pool/**"
  assert_contains "the filter lists the vhost script the suite runs by path" \
    "$block" "tests/e2e/cinder/broker-vhost.sh"
  # The operator and the images reach the job through the nova and image_nova
  # filters the resolver reads, and the deploy stack and the hack scripts
  # through the canary. Listing them here would schedule a self-hosted full
  # Nova stack for every helper-script edit.
  assert_not_contains "the filter does not carry the operator tree" \
    "$block" "operators/nova/**"
  assert_not_contains "the filter does not carry the hack scripts" \
    "$block" "hack/**"
  assert_not_contains "the filter does not carry the deploy stack" \
    "$block" "deploy/**"
}

test_the_job_reads_the_signal() {
  echo "Test: the job gates on the output and runs the suite non-blocking"

  local job kind_load
  job=$(job_block e2e-nova-libvirt)
  kind_load=$(job_step e2e-nova-libvirt "Load images into kind")

  assert_not_empty "the e2e-nova-libvirt job exists" "$job"
  assert_contains "the job reads the output the resolve script wrote" "$job" \
    "needs.changes.outputs.e2e-nova-libvirt == 'true'"
  assert_contains "the job runs on pull requests only" "$job" \
    "github.event_name == 'pull_request'"
  assert_contains "the job runs on the self-hosted runners" "$job" \
    "runs-on: self-hosted"
  # Non-blocking under the kernel-module rule of ci-workflow.md: the pool's
  # chassis gate needs openvswitch on the runner host.
  assert_contains "the job does not block the pull request" "$job" \
    "continue-on-error: true"
  assert_contains "the job creates the single-node cluster" "$job" \
    "config: hack/kind-config.yaml"
  assert_contains "the job loads the OVN kernel modules" "$job" \
    'WITH_OVN_KERNEL_MODULES: "true"'
  assert_contains "the job deploys the message bus" "$job" \
    'WITH_MESSAGING: "true"'
  assert_contains "the job runs the suite through the Makefile" "$job" \
    "make e2e-nova-libvirt"
  assert_contains "the job uploads its JUnit report" "$job" \
    "name: e2e-nova-libvirt-junit-report"
  assert_contains "the job pulls the nova-compute image the pool runs" \
    "$(job_step e2e-nova-libvirt "Load E2E images")" "nova-compute:2025.2"
  # Without the kind load the node pulls main's published image and the job
  # tests that instead of the PR's build.
  assert_contains "the job loads that image into kind" "$kind_load" \
    "\${{ env.IMAGE_PREFIX }}/nova-compute:2025.2"
  assert_contains "the job loads the Nova service image into kind" "$kind_load" \
    "\${{ env.IMAGE_PREFIX }}/nova:2025.2"

  # cleanup-e2e-tags prunes the run-scoped image tags this job pulls, so it
  # has to wait for the job.
  assert_contains "cleanup-e2e-tags waits for the job" \
    "$(job_block cleanup-e2e-tags)" "e2e-nova-libvirt"
}

test_makefile_target() {
  echo "Test: the Makefile target exists and names each missing precondition"

  assert_file_contains "the Makefile declares the target" "$MAKEFILE" \
    "^e2e-nova-libvirt:$"
  assert_file_contains "the target is phony" "$MAKEFILE" \
    "^\\.PHONY: e2e-nova-libvirt$"
  assert_file_contains_fixed "the first preflight names an unreachable cluster" \
    "$MAKEFILE" "kubectl is not configured or no cluster is reachable"
  assert_file_contains_fixed "the second preflight names the six operators" \
    "$MAKEFILE" \
    "the libvirt suite needs the nova-operator and its five siblings (keystone, placement, glance, ovn, neutron); deploy each with hack/ci-deploy-operator.sh first"
  assert_file_contains_fixed "the third preflight names the one amd64 node and the cluster's architectures" \
    "$MAKEFILE" \
    "the libvirt suite boots an x86_64 guest on one amd64 node; this cluster's nodes are: \$\$archs"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_each_input_schedules_the_job
test_other_changes_do_not_schedule_the_job
test_unset_filter_defaults_to_false
test_noop_run_still_emits_the_output
test_forced_runs_schedule_the_job
test_the_signal_reaches_the_image_build
test_ci_yaml_wires_the_signal
test_filter_names_the_suite_and_its_fixtures
test_the_job_reads_the_signal
test_makefile_target

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
