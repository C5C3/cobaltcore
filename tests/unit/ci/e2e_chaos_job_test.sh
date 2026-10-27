#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the shape of the e2e-chaos job in .github/workflows/ci.yaml: its
# trigger filter, its needs and gate, its per-suite wall, the health check that
# opens every matrix entry, the chainsaw run, the diagnostics and the report,
# and that no publish job waits for it.
#
# Each of these fails silently. A needs entry dropped from the list lets the
# chaos legs start before the gate they rely on has run, and the
# failure/cancelled guard in the if: only sees the jobs the list names. One wall
# for every suite either cuts the nova and network legs short or gives the
# 45-minute ovn leg two hours to hang in, and a pod leg made non-blocking stops
# gating merges without failing anything. An entry that does not open with the
# Chaos Mesh health check runs its suites against a controller that may not be
# up, and every NetworkChaos or PodChaos in it times out instead of failing on
# the cause. The report artifact is uploaded once per matrix entry, so a name
# without the suite makes the uploads collide on one artifact name and only
# the first report is kept. A publish job that waits for e2e-chaos holds
# a release on a leg that is allowed to fail.
#
# The resolver's chaos outputs, the label gate and the kind-cluster composite
# are pinned by resolve_changes_scenarios_test.sh,
# change_classes_wiring_test.sh and create_kind_cluster_wiring_test.sh, the
# controlplane entry by controlplane_chaos_matrix_test.sh, and every suite
# sitting in exactly one entry by tests/unit/docs/chaos_suite_inventory_test.sh.
#
# Usage: bash tests/unit/ci/e2e_chaos_job_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the entries of the job's needs: list, one per line. Matching whole
# entries keeps test from passing on test-integration.
needs_entries() {
  job_block "$1" | grep -E '^    needs: ' | head -1 |
    sed 's/^    needs: *//; s/[][]//g; s/ //g' | tr ',' '\n'
}

# Echo the job-level <key>: line of the job, indent stripped. The 4-space
# indent selects the job's own key over a step's.
job_line() {
  job_block "$1" | grep -E "^    $2: " | head -1 | sed 's/^    //'
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_chaos_filter_lists_the_suites_alone() {
  echo "Test: the tests_chaos filter lists the chaos suites alone"

  # deploy/** reaches the chaos legs through the canary instead. Listing it here
  # meant every OpenBao or Flux bump paid for two chaos legs on top of the
  # pipeline it already triggered.
  local block
  block=$(filter_block tests_chaos)
  assert_contains "the filter covers the chaos suites" "$block" "tests/e2e-chaos/**"
  assert_not_contains "the filter does not carry the deploy stack" "$block" "deploy/**"
}

test_the_job_needs_every_gate() {
  echo "Test: e2e-chaos needs every gate it relies on"

  local entries joined dep
  entries=$(needs_entries e2e-chaos)
  assert_not_empty "the job has a needs list" "$entries"
  joined=",$(tr '\n' ',' <<<"$entries")"
  for dep in changes lint shellcheck test test-integration verify-codegen \
    chainsaw-lint build-e2e-images e2e-operator; do
    assert_contains "e2e-chaos needs $dep" "$joined" ",$dep,"
  done
}

test_the_gate_blocks_on_a_failed_or_cancelled_need() {
  echo "Test: the e2e-chaos gate runs on a skipped need and blocks on a failed one"

  # always() lets the job run when the Go jobs were skipped for a pull request
  # that touches only suites; the two contains() guards keep a failed or a
  # cancelled need blocking all the same. A scheduled or dispatched run is the
  # nightly chaos run, and a push never runs the legs.
  local block
  block=$(job_block e2e-chaos)
  assert_contains "the gate survives a skipped need" "$block" "always() &&"
  assert_contains "the gate runs on a pull request and on the night" "$block" \
    "(github.event_name == 'pull_request' || github.event_name == 'schedule' || github.event_name == 'workflow_dispatch') &&"
  assert_not_contains "the gate never runs on a push" "$block" "'push'"
  assert_contains "the gate reads the resolver's chaos flag" "$block" \
    "needs.changes.outputs.e2e-chaos == 'true'"
  assert_contains "the gate waits for the images" "$block" \
    "needs.build-e2e-images.result == 'success'"
  assert_contains "a failed need blocks the job" "$block" \
    "!contains(needs.*.result, 'failure')"
  assert_contains "a cancelled need blocks the job" "$block" \
    "!contains(needs.*.result, 'cancelled')"
}

test_each_suite_has_its_own_wall() {
  echo "Test: each e2e-chaos suite has its own wall and pod alone blocks"

  assert_eq "the wall is sized per suite" \
    "timeout-minutes: \${{ matrix.suite == 'nova' && 170 || matrix.suite == 'network' && 135 || matrix.suite == 'controlplane' && 120 || matrix.suite == 'upgrade' && 80 || matrix.suite == 'ovn' && 45 || 135 }}" \
    "$(job_line e2e-chaos timeout-minutes)"
  assert_eq "every suite but pod is non-blocking, each by name" \
    "continue-on-error: \${{ matrix.suite == 'network' || matrix.suite == 'ovn' || matrix.suite == 'nova' || matrix.suite == 'controlplane' || matrix.suite == 'upgrade' }}" \
    "$(job_line e2e-chaos continue-on-error)"
}

test_every_entry_starts_with_the_health_check() {
  echo "Test: every e2e-chaos matrix entry opens with the Chaos Mesh health check"

  local suites suite entry first
  suites=$(job_block e2e-chaos | sed -n "s/^          - suite: //p")
  assert_not_empty "the e2e-chaos matrix has entries" "$suites"
  for suite in $suites; do
    entry=$(e2e_chaos_matrix_entry "$suite")
    assert_not_empty "the $suite entry exists" "$entry"
    first=$(grep -oE 'tests/[^ ]+' <<<"$entry" | head -1)
    assert_eq "the $suite entry starts with the health check" \
      "tests/e2e/infrastructure/chaos-mesh-health" "$first"
  done
}

test_the_run_step_drives_chainsaw_over_the_entry() {
  echo "Test: the run step drives chainsaw over the entry's test directories"

  local step
  step=$(job_step e2e-chaos "Run chaos E2E tests")
  assert_contains "the report directory is created" "$step" "mkdir -p _output/reports"
  assert_contains "chainsaw runs with the chaos config" "$step" \
    "chainsaw test --config tests/e2e-chaos/chainsaw-config.yaml \\"
  assert_contains "over the entry's test directories" "$step" \
    "\${{ matrix.test_dirs }}"

  # The config's failFast: true holds on a pull request; a night runs every
  # suite of the leg, so one red suite hides no other from the report.
  assert_eq "a run that is no pull request turns failFast off, between the config and the directories" \
    "          chainsaw test --config tests/e2e-chaos/chainsaw-config.yaml \\
            \${{ github.event_name != 'pull_request' && '--fail-fast=false' || '' }} \\
            \${{ matrix.test_dirs }}" \
    "$(grep -A2 'chainsaw test --config tests/e2e-chaos/chainsaw-config.yaml' <<<"$step")"
}

test_diagnostics_and_the_report_survive_a_failure() {
  echo "Test: the diagnostics and the JUnit report survive a failed run"

  local step
  step=$(job_step e2e-chaos "Dump diagnostic info")
  assert_contains "the diagnostics run after a failure" "$step" "if: always()"
  assert_contains "they go through the shared dump script" "$step" \
    "run: hack/ci-dump-diagnostics.sh"
  assert_contains "the ovn leg dumps the ovn operator" "$step" \
    "OPERATOR: \${{ matrix.suite == 'ovn' && 'ovn' || 'keystone' }}"

  step=$(job_step e2e-chaos "Upload JUnit report")
  assert_contains "the report is uploaded after a failure" "$step" "if: always()"
  assert_contains "the report name carries the suite" "$step" \
    "name: e2e-chaos-junit-report-\${{ matrix.suite }}"
  assert_contains "the report comes from the run's report directory" "$step" \
    "path: _output/reports/"
  assert_contains "the report is kept for two weeks" "$step" "retention-days: 14"
}

test_the_publish_jobs_do_not_wait_for_chaos() {
  echo "Test: the publish jobs do not wait for e2e-chaos"

  local job line
  for job in build-and-push merge-operator-images helm-push github-release; do
    line=$(job_line "$job" needs)
    assert_not_empty "$job has a needs line" "$line"
    assert_not_contains "$job does not wait for e2e-chaos" "$line" "e2e-chaos"
  done
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_the_chaos_filter_lists_the_suites_alone
test_the_job_needs_every_gate
test_the_gate_blocks_on_a_failed_or_cancelled_need
test_each_suite_has_its_own_wall
test_every_entry_starts_with_the_health_check
test_the_run_step_drives_chainsaw_over_the_entry
test_diagnostics_and_the_report_survive_a_failure
test_the_publish_jobs_do_not_wait_for_chaos

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
