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
ALL_OPERATORS_FIXTURE="keystone c5c3 horizon glance placement barbican"

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
# so the composed shape (own filter, go change, any e2e test change) can be
# exercised one input at a time.
run_resolve() {
  local ref="$1" filter="$2"
  shift 2

  resolve_output e2e-controlplane "$ref" "$ALL_OPERATORS_FIXTURE" \
    FILTER_e2e_controlplane="$filter" "$@"
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
  echo "Test: a c5c3 change, a shared Go change and a suite edit each force the job on"

  assert_eq "a change under operators/c5c3/** forces the job on" \
    "e2e-controlplane=true" "$(run_resolve refs/heads/main false FILTER_c5c3=true)"
  assert_eq "a shared Go change forces the job on" \
    "e2e-controlplane=true" "$(run_resolve refs/heads/main false FILTER_go_common=true)"
  assert_eq "a suite edit under tests/e2e/** forces the job on" \
    "e2e-controlplane=true" "$(run_resolve refs/heads/main false FILTER_tests_e2e_operator=true)"
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
  echo "Test: ci.yaml declares the filter, passes it in, exports it and gates on it"

  assert_filter_is_wired e2e_controlplane e2e-controlplane

  # Three jobs gate on the output. A file-wide grep would let a typo in any one
  # of them hide behind the other two, which is exactly the permanently-skipped
  # job this file exists to catch — so each is pinned in its own block.
  local job
  for job in e2e-controlplane e2e-controlplane-sso e2e-external-keystone; do
    assert_contains "$job gates on the output" \
      "$(job_block "$job")" "needs.changes.outputs.e2e-controlplane == 'true'"
  done

  # build-e2e-images is the fourth consumer and reads the output twice: once as
  # one OR term of its own `if:`, and once inline to add c5c3/horizon to
  # BUILD_OPERATORS. Broken, this job does not skip — it fails the e2e jobs an
  # hour later at "Load E2E images" with a dev tag that was never pushed.
  local build_block
  build_block=$(job_block build-e2e-images)
  assert_contains "build-e2e-images gates on the output" \
    "$build_block" "needs.changes.outputs.e2e-controlplane == 'true'"
  assert_contains "build-e2e-images reads it for the dev image list" \
    "$build_block" "needs.changes.outputs.e2e-controlplane }}' = 'true' ]"
}

test_filter_covers_the_machinery_and_the_suites() {
  echo "Test: the filter lists the operator code and the suites the job runs"

  # The filter block ends at the next filter key at the same indent. The
  # terminator class carries a digit because the next key is e2e_multicluster:,
  # and without it the block would run on into that filter, whose own 'hack/**'
  # entry would mask a removal here.
  local block
  block=$(awk '
    /^            e2e_controlplane:$/ { in_block = 1; next }
    in_block && /^            [a-z0-9_]+:$/ { exit }
    in_block { print }
  ' "$CI_YAML")

  assert_contains "the filter lists the ControlPlane operator" \
    "$block" "operators/c5c3/**"
  assert_contains "the filter lists the ControlPlane suites" \
    "$block" "tests/e2e/c5c3/**"
  assert_contains "the filter lists the hack scripts" \
    "$block" "hack/**"
  # These two reach the job through this filter alone: tests_e2e_operator's
  # 'tests/e2e/**' sees neither, and neither sets go_changed. Dropping either
  # one silently unschedules the ControlPlane jobs for their own edits.
  assert_contains "the filter lists the federated suite" \
    "$block" "tests/e2e-controlplane-sso/**"
  assert_contains "the filter lists the federation-proxy sidecar" \
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
