#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-changes.sh emits the e2e-ovn-overlay change signal and
# that .github/workflows/ci.yaml wires it up.
#
# The signal crosses three places and a mismatch in any of them is silent:
# GitHub Actions resolves an unknown `needs.<job>.outputs.<name>` to the empty
# string rather than failing, so a renamed output or an unwired FILTER_ env var
# leaves the overlay job permanently skipped and the datapath it proves
# permanently unexercised.
#
# The resolve script is executed for real in all of its branches; the ci.yaml
# sides are asserted against the workflow file. Modelled on the sibling
# tests/unit/ci/e2e_multicluster_output_test.sh, with the shared resolve-script
# scaffolding in tests/lib/ci_resolve.sh.
#
# Usage: bash tests/unit/hack/ci_resolve_changes_ovn_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# The resolve script reads FILTER_${op} only for operators named here, so ovn
# must be in the list for the operator-change scenario to assert anything.
ALL_OPERATORS_FIXTURE="keystone ovn"

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

# Run the resolve script for the given ref and overlay filter value, and echo
# the e2e-ovn-overlay line it emits. Extra FILTER_ assignments are passed
# through so the composed shape can be exercised one input at a time.
run_resolve() {
  local ref="$1" filter="$2"
  shift 2

  resolve_output e2e-ovn-overlay "$ref" "$ALL_OPERATORS_FIXTURE" \
    FILTER_tests_ovn_overlay="$filter" "$@"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_own_filter_is_honoured() {
  echo "Test: on a branch the job follows its own filter"

  assert_eq "a changed suite is signalled" \
    "e2e-ovn-overlay=true" "$(run_resolve refs/heads/main true)"
  assert_eq "an untouched suite is not signalled" \
    "e2e-ovn-overlay=false" "$(run_resolve refs/heads/main false)"
}

test_only_its_own_inputs_schedule_the_job() {
  echo "Test: only the suite, the operator, the daemon image and ci:full schedule it"

  # What the job proves is a packet crossing a Geneve tunnel between two kind
  # workers, and what builds that datapath is the ovn-operator plus the OVS/OVN
  # daemons of images/ovn. Those three are its inputs.
  assert_eq "an ovn-operator change schedules the job" \
    "e2e-ovn-overlay=true" "$(run_resolve refs/heads/main false FILTER_ovn=true)"
  assert_eq "an OVN daemon image change schedules the job" \
    "e2e-ovn-overlay=true" "$(run_resolve refs/heads/main false FILTER_image_ovn=true)"

  # The job runs alone on a self-hosted runner with a multi-node cluster of its
  # own, so it follows the ControlPlane trio rather than the upgrade suite: a
  # shared Go change puts every operator in op_changed and must not pull this
  # job in behind them.
  assert_eq "a shared Go change does not schedule the job" \
    "e2e-ovn-overlay=false" "$(run_resolve refs/heads/main false FILTER_go_common=true)"
  assert_eq "another operator's change does not schedule the job" \
    "e2e-ovn-overlay=false" "$(run_resolve refs/heads/main false FILTER_keystone=true)"
  assert_eq "another suite does not schedule the job" \
    "e2e-ovn-overlay=false" \
    "$(run_resolve refs/heads/main false FILTER_tests_multicluster=true)"
  assert_eq "ci:full schedules the job" \
    "e2e-ovn-overlay=true" "$(run_resolve refs/heads/main false PR_LABELS='["ci:full"]')"
}

test_unset_filter_defaults_to_false() {
  echo "Test: an unwired filter defaults to false rather than tripping set -u"

  # The script runs under `set -u`, so the `:-false` default is the only thing
  # between a filter that ci.yaml does not pass in and an aborted changes job.
  assert_eq "the output is emitted even with no FILTER_ env var" \
    "e2e-ovn-overlay=false" \
    "$(resolve_output e2e-ovn-overlay refs/heads/main "$ALL_OPERATORS_FIXTURE")"
}

test_noop_run_still_emits_the_output() {
  echo "Test: the labeled no-op path emits an explicit false"

  # An output the no-op path forgets resolves to the empty string in the
  # consuming job, which is neither 'true' nor a failure — just a silent skip.
  assert_eq "the no-op path emits the output" \
    "e2e-ovn-overlay=false" \
    "$(run_resolve refs/heads/main true EVENT_ACTION=labeled EVENT_LABEL=bug)"
}

test_tag_push_forces_the_job() {
  echo "Test: a v* tag runs the job whatever the filter says"

  assert_eq "the release pipeline forces the job on" \
    "e2e-ovn-overlay=true" "$(run_resolve refs/tags/v1.2.3 false)"
}

test_the_signal_reaches_the_image_build() {
  echo "Test: scheduling the job also schedules the images it loads"

  # The job gates on `needs.build-e2e-images.result == 'success'`, so a suite
  # edit that switches the overlay on without switching the build on leaves it
  # skipped on a dependency that never ran.
  assert_contains "a suite-only change still builds the E2E images" \
    "$(resolve_outputs refs/heads/main "$ALL_OPERATORS_FIXTURE" \
      FILTER_tests_ovn_overlay=true)" \
    "build-e2e-images=true"
}

test_ci_yaml_wires_the_signal() {
  echo "Test: ci.yaml declares the filter, passes it in and exports the output"

  assert_filter_is_wired tests_ovn_overlay e2e-ovn-overlay
}

test_filter_names_the_suite_and_nothing_else() {
  echo "Test: the filter lists the overlay suite only"

  # The filter block ends at the next filter key at the same indent.
  local block
  block=$(awk '
    /^            tests_ovn_overlay:$/ { in_block = 1; next }
    in_block && /^            [a-z0-9_]+:$/ { exit }
    in_block { print }
  ' "$CI_YAML")

  assert_contains "the filter lists the overlay suite" \
    "$block" "tests/e2e-ovn-overlay/**"
  # The operator and the daemon image reach the job through the `ovn` and
  # `image_ovn` filters the resolver reads, and the deploy stack and the hack
  # scripts through the canary. Listing them here would schedule a self-hosted
  # multi-node run for every helper-script edit.
  assert_not_contains "the filter does not carry the operator tree" \
    "$block" "operators/ovn/**"
  assert_not_contains "the filter does not carry the hack scripts" \
    "$block" "hack/**"
  assert_not_contains "the filter does not carry the deploy stack" \
    "$block" "deploy/**"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_own_filter_is_honoured
test_only_its_own_inputs_schedule_the_job
test_unset_filter_defaults_to_false
test_noop_run_still_emits_the_output
test_tag_push_forces_the_job
test_the_signal_reaches_the_image_build
test_ci_yaml_wires_the_signal
test_filter_names_the_suite_and_nothing_else

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
