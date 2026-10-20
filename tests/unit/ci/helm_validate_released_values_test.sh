#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the helm-validate job of .github/workflows/ci.yaml renders every
# operator chart with the values of its released chart, through
# make verify-helm-released-values.
#
# The render runs in its own step. The cinder and nova matrix tests replay every
# `helm template test "$chart"` line of the step named Helm template, so the
# check must not live there. The step pulls anonymously, as Flux does, so the
# job carries no registry login and fork pull requests need no credential. The
# helm filter is the sole gate on helm-validate: without the script and the
# waiver list in it, a pull request that edits only one of them never runs the
# job.
#
# Usage: bash tests/unit/ci/helm_validate_released_values_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
MAKEFILE="$PROJECT_ROOT/Makefile"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_render_step_runs_the_make_target() {
  echo "Test: the Render the released chart values step runs the make target"

  local step
  step="$(job_step helm-validate "Render the released chart values")"
  assert_not_empty "helm-validate has the step" "$step"
  assert_contains "the step runs make verify-helm-released-values" "$step" \
    "run: make verify-helm-released-values"
}

test_helm_template_step_does_not_carry_the_render() {
  echo "Test: the Helm template step does not run the released-values render"

  local step
  step="$(job_step helm-validate "Helm template")"
  # An empty slice would make the absence below hold for nothing.
  assert_contains "the Helm template step is readable" "$step" 'helm template test "$chart"'
  assert_not_contains "the Helm template step leaves the render alone" "$step" \
    "verify-helm-released-values"
}

test_job_needs_no_registry_login() {
  echo "Test: helm-validate pulls the released charts without a registry login"

  local job
  job="$(job_block helm-validate)"
  assert_not_empty "the helm-validate job is readable" "$job"
  assert_not_contains "the job carries no registry login" "$job" "registry login"
  assert_not_contains "the job uses no login action" "$job" "login-action"
  assert_not_contains "the job pipes no password" "$job" "password-stdin"
  # A comment in the job says "secrets.", so only the expression is matched.
  assert_eq "the job reads no secret" "" "$(grep -E '\$\{\{ *secrets\.' <<<"$job")"
}

test_helm_filter_lists_the_script_and_the_waiver_list() {
  echo "Test: the helm filter re-runs helm-validate for the script and the waiver list"

  local filter
  filter="$(filter_block helm)"
  assert_contains "the filter lists the script" "$filter" "'hack/verify-helm-released-values.sh'"
  assert_contains "the filter lists the waiver list" "$filter" "'hack/helm-removed-values.yaml'"
}

test_make_target_runs_the_script() {
  echo "Test: make verify-helm-released-values runs the script"

  local recipe
  recipe="$(awk '/^verify-helm-released-values:/ { on = 1; next } on && /^[^\t]/ { exit } on { print }' "$MAKEFILE")"
  assert_contains "the recipe runs hack/verify-helm-released-values.sh" "$recipe" \
    "hack/verify-helm-released-values.sh"
  assert_contains "the recipe passes OPERATOR to the script" "$recipe" \
    'hack/verify-helm-released-values.sh $(OPERATOR)'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_render_step_runs_the_make_target
test_helm_template_step_does_not_carry_the_render
test_job_needs_no_registry_login
test_helm_filter_lists_the_script_and_the_waiver_list
test_make_target_runs_the_script

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
