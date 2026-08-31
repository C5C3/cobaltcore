#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the e2e-controlplane change signal reaches the jobs that consume it in
# .github/workflows/ci.yaml.
#
# The signal crosses four places and a mismatch in any of them is silent:
# GitHub Actions resolves an unknown `needs.<job>.outputs.<name>` to the empty
# string rather than failing, so a renamed output or an unwired FILTER_ env var
# leaves the consuming jobs permanently skipped. The registration suites carry
# a presence guard that SKIPs on the other CI legs, so a suite that stops being
# scheduled skips quietly everywhere instead of failing anywhere.
#
# hack/ci-resolve-changes.sh is executed for real in all of its branches; the
# ci.yaml sides are asserted against the workflow file. Modelled on the sibling
# tests/unit/ci/e2e_multicluster_output_test.sh, with the shared resolve-script
# scaffolding in tests/lib/ci_resolve.sh.
#
# Usage: bash tests/unit/ci/e2e_controlplane_output_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# The real list from the ci.yaml resolve step env block. The resolve script
# reads FILTER_${op} only for operators named here, so a shorter list would
# make the FILTER_c5c3 scenario below assert nothing.
ALL_OPERATORS_FIXTURE="keystone c5c3 horizon glance placement barbican ovn neutron"

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

# Run the resolve script for the given ref and FILTER_ values, and echo the
# e2e-controlplane line it emits. Extra FILTER_ assignments are passed through
# so each input can be exercised one at a time.
run_resolve() {
  local ref="$1" filter="$2"
  shift 2

  resolve_output e2e-controlplane "$ref" "$ALL_OPERATORS_FIXTURE" \
    FILTER_tests_controlplane="$filter" "$@"
}

# Echo the body of the top-level ci.yaml job <name>. The block ends at the next
# 2-space line that is not part of the body — the next job key, or the comment
# header introducing it. Every line of a job body is indented four spaces or
# more, so stopping on '  #' as well keeps the neighbouring job's prose out of
# the exact counts below.
job_block() {
  awk -v key="  $1:" '
    $0 == key { in_block = 1; next }
    in_block && /^  [#a-z0-9-]/ { exit }
    in_block { print }
  ' "$CI_YAML"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_own_filter_is_honoured() {
  echo "Test: on a branch the job follows its own filter"

  assert_eq "a changed ControlPlane path is signalled" \
    "e2e-controlplane=true" "$(run_resolve refs/heads/main true)"
  assert_eq "an untouched ControlPlane path is not signalled" \
    "e2e-controlplane=false" "$(run_resolve refs/heads/main false)"
}

test_upstream_signals_force_the_job() {
  echo "Test: a c5c3 change and the ci:controlplane label each force the job on"

  assert_eq "a change under operators/c5c3/** forces the job on" \
    "e2e-controlplane=true" "$(run_resolve refs/heads/main false FILTER_c5c3=true)"
  assert_eq "the ci:controlplane label forces the job on" \
    "e2e-controlplane=true" \
    "$(run_resolve refs/heads/main false PR_LABELS='["ci:controlplane"]')"
  assert_eq "ci:full forces the job on" \
    "e2e-controlplane=true" \
    "$(run_resolve refs/heads/main false PR_LABELS='["ci:full"]')"
}

test_shared_changes_no_longer_force_the_job() {
  echo "Test: a shared Go change and another operator's suite leave the job off"

  # This job and its two siblings are the most expensive in the pipeline (up to
  # 195 minutes each). They used to run on any Go change and on any edit under
  # tests/e2e/**, which is what made a one-line dependency bump cost a full
  # pipeline. A shared change still runs the c5c3 e2e leg; ci:controlplane and
  # ci:full are how you ask for the chain itself.
  assert_eq "a shared Go change does not schedule the job" \
    "e2e-controlplane=false" "$(run_resolve refs/heads/main false FILTER_go_common=true)"
  assert_eq "another operator's suite does not schedule the job" \
    "e2e-controlplane=false" \
    "$(run_resolve refs/heads/main false FILTER_tests_e2e_glance=true)"
  assert_eq "the keystone operator does not schedule the job" \
    "e2e-controlplane=false" "$(run_resolve refs/heads/main false FILTER_keystone=true)"
}

test_unrelated_change_stays_off() {
  echo "Test: an unrelated change leaves the job off"

  assert_eq "a docs-only change does not schedule the job" \
    "e2e-controlplane=false" "$(run_resolve refs/heads/main false FILTER_docs=true)"
}

test_unset_filter_defaults_to_false() {
  echo "Test: an unwired filter defaults to false rather than tripping set -u"

  assert_eq "the output is emitted even with no FILTER_ env var" \
    "e2e-controlplane=false" \
    "$(resolve_output e2e-controlplane refs/heads/main "$ALL_OPERATORS_FIXTURE")"
}

test_tag_push_forces_the_job() {
  echo "Test: a v* tag runs the job whatever the filter says"

  assert_eq "the release pipeline forces the job on" \
    "e2e-controlplane=true" "$(run_resolve refs/tags/v1.2.3 false)"
}

test_resolve_script_errors_without_operators() {
  echo "Test: the resolve script refuses to run without ALL_OPERATORS"

  # Pins the `::error::ALL_OPERATORS must be set` guard: a misconfigured caller
  # fails loudly instead of emitting empty outputs that the downstream jobs
  # would read as "skip everything".
  local out rc
  out=$(mktemp)
  env ALL_OPERATORS="" GITHUB_OUTPUT="$out" GITHUB_REF=refs/heads/main \
    bash "$PROJECT_ROOT/hack/ci-resolve-changes.sh" >/dev/null 2>&1
  rc=$?
  assert_nonzero_exit "an empty ALL_OPERATORS fails loudly" "$rc"
  rm -f "$out"
}

test_ci_yaml_wires_all_four_sides() {
  echo "Test: ci.yaml declares each filter, passes it in, exports it and gates on it"

  assert_filter_is_wired tests_controlplane e2e-controlplane
  assert_filter_is_wired tests_controlplane_sso e2e-controlplane-sso
  assert_filter_is_wired tests_external_keystone e2e-external-keystone

  # The three jobs run on three separate clusters and each owns its own suite,
  # so each gates on an output of its own. A file-wide grep would let a typo in
  # any one of them hide behind the other two, which is exactly the
  # permanently-skipped job this file exists to catch.
  local job
  for job in e2e-controlplane e2e-controlplane-sso e2e-external-keystone; do
    assert_contains "$job gates on its own output" \
      "$(job_block "$job")" "needs.changes.outputs.${job} == 'true'"
  done

  # build-e2e-images no longer reads the ControlPlane output at all: the
  # resolver folds the images those jobs load into build-operators. Broken, this
  # job does not skip — it fails the e2e jobs an hour later at "Load E2E images"
  # with a dev tag that was never pushed.
  local build_block
  build_block=$(job_block build-e2e-images)
  assert_contains "build-e2e-images gates on its own flag" \
    "$build_block" "needs.changes.outputs.build-e2e-images == 'true'"
  assert_contains "build-e2e-images takes its image list from the resolver" \
    "$build_block" "needs.changes.outputs.build-operators"
  assert_not_contains "build-e2e-images no longer reads the ControlPlane output" \
    "$build_block" "needs.changes.outputs.e2e-controlplane"
}

test_filter_covers_the_machinery_and_the_suites() {
  echo "Test: each filter lists exactly the suite its job runs"

  # A filter block ends at the next filter key at the same indent. The
  # terminator class carries a digit because several keys have one.
  filter_block() {
    awk -v key="            $1:" '
      $0 == key { in_block = 1; next }
      in_block && /^            [a-z0-9_]+:$/ { exit }
      in_block { print }
    ' "$CI_YAML"
  }

  local block
  block=$(filter_block tests_controlplane)
  assert_contains "the filter lists the full-chain suite" \
    "$block" "tests/e2e/c5c3/full-controlplane-keystone/**"
  assert_contains "the filter lists the own-namespace registration suite" \
    "$block" "tests/e2e/c5c3/keystone-service/**"
  assert_contains "the filter lists the foreign-namespace registration suite" \
    "$block" "tests/e2e/c5c3/keystone-service-foreign-namespace/**"
  # The operator code and the shared scripts reach this job through the c5c3
  # filter and the canary respectively, not through the suite filter. Listing
  # them here again is what made every Go change schedule three 195-minute jobs.
  assert_not_contains "the suite filter does not carry the operator tree" \
    "$block" "operators/c5c3/**"
  assert_not_contains "the suite filter does not carry the hack scripts" \
    "$block" "hack/**"

  block=$(filter_block tests_controlplane_sso)
  assert_contains "the federated suite has a filter of its own" \
    "$block" "tests/e2e-controlplane-sso/**"

  block=$(filter_block tests_external_keystone)
  assert_contains "the External-mode suite has a filter of its own" \
    "$block" "tests/e2e/c5c3/external-keystone/**"

  # The sidecar reaches the keystone e2e leg through image_proxy; the SSO suite
  # pins it to the locally built :dev tag and is scheduled by its own filter.
  block=$(filter_block image_proxy)
  assert_contains "the federation-proxy sidecar has a filter of its own" \
    "$block" "images/keystone-federation-proxy/**"
}

test_job_runs_all_three_suites() {
  echo "Test: the job schedules all three chainsaw suites"

  local block
  block=$(job_block e2e-controlplane)

  # Assert the block first, so a renamed job key fails here instead of letting
  # every pattern below pass vacuously against an empty string.
  assert_not_empty "the job exists" "$block"
  assert_contains "the job runs the full-chain suite" \
    "$block" "tests/e2e/c5c3/full-controlplane-keystone/"
  assert_contains "the job runs the foreign-namespace registration suite" \
    "$block" "tests/e2e/c5c3/keystone-service-foreign-namespace/"
  # The trailing slash keeps this from matching the foreign-namespace directory.
  assert_contains "the job runs the own-namespace registration suite" \
    "$block" "tests/e2e/c5c3/keystone-service/"
  assert_contains "the own-namespace suite reports under its own name" \
    "$block" "chainsaw-report-keystone-service-own-namespace"

  # The `--` keeps grep from reading the pattern as flags. Matching the quoted
  # env form keeps the job's own prose comment about the unquoted
  # E2E_REQUIRE_CONTROLPLANE_STACK=true out of the count.
  assert_eq "the job runs exactly two named reports" \
    "2" "$(grep -c -- '--report-name' <<<"$block")"
  assert_eq "all three suites harden the presence guard" \
    "3" "$(grep -c 'E2E_REQUIRE_CONTROLPLANE_STACK: "true"' <<<"$block")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_own_filter_is_honoured
test_upstream_signals_force_the_job
test_shared_changes_no_longer_force_the_job
test_unrelated_change_stays_off
test_unset_filter_defaults_to_false
test_tag_push_forces_the_job
test_resolve_script_errors_without_operators
test_ci_yaml_wires_all_four_sides
test_filter_covers_the_machinery_and_the_suites
test_job_runs_all_three_suites

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
