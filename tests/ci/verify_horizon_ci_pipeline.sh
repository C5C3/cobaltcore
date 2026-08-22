#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify horizon operator CI pipeline wiring meets requirements.
# Validates: horizon paths-filter block, FILTER_horizon env, ALL_OPERATORS
# membership, test/helm-validate/cleanup matrices, and that
# ci-resolve-changes.sh emits horizon in the e2e-operators matrix once horizon
# is a known operator.
# Usage: bash tests/ci/verify_horizon_ci_pipeline.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
RESOLVE_SCRIPT="$PROJECT_ROOT/hack/ci-resolve-changes.sh"

echo "=== horizon operator CI pipeline verification ==="
echo ""

# ── Helpers ─────────────────────────────────────────────────────────────────

# Run ci-resolve-changes.sh with the supplied env and echo the GITHUB_OUTPUT
# contents. ALL_OPERATORS deliberately mirrors the ci.yaml value ("keystone
# c5c3 horizon glance") so the behavioural assertions exercise the real
# resolution codepath. Args are passed as KEY=VALUE pairs through the caller's
# env block.
run_resolve() {
  local out
  out=$(mktemp)
  GITHUB_OUTPUT="$out" bash "$RESOLVE_SCRIPT" >/dev/null
  cat "$out"
  rm -f "$out"
}

# Extract the value of a single GITHUB_OUTPUT key from resolve output.
output_value() {
  local resolved="$1" key="$2"
  echo "$resolved" | grep "^${key}=" | head -1 | cut -d= -f2-
}

# ── ci.yaml paths-filter / env wiring tests ─────────────────────────────────

test_horizon_filter_block() {
  echo "Test: ci.yaml has a horizon paths-filter block"

  assert_file_contains \
    "ci.yaml declares a horizon filter" \
    "$CI_YAML" \
    "^            horizon:"

  assert_file_contains \
    "horizon filter includes operators/horizon/**" \
    "$CI_YAML" \
    "operators/horizon/\*\*"

  assert_file_contains \
    "horizon filter includes images/horizon/**" \
    "$CI_YAML" \
    "images/horizon/\*\*"
}

test_horizon_all_operators() {
  echo "Test: ci.yaml ALL_OPERATORS includes horizon"

  local all_operators_line
  all_operators_line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)

  assert_contains \
    "ALL_OPERATORS lists keystone" \
    "$all_operators_line" \
    "keystone"

  assert_contains \
    "ALL_OPERATORS lists horizon" \
    "$all_operators_line" \
    "horizon"
}

test_horizon_filter_env_var() {
  echo "Test: ci.yaml passes FILTER_horizon env var to the resolve step"

  assert_file_contains \
    "FILTER_horizon env var is wired from steps.filter.outputs.horizon" \
    "$CI_YAML" \
    'FILTER_horizon: ${{ steps.filter.outputs.horizon }}'
}

test_horizon_test_matrices() {
  echo "Test: unit and integration test matrices include horizon"

  local matrix_count
  matrix_count=$(grep -c "target: \[common, keystone, c5c3, horizon, glance, placement, barbican\]" "$CI_YAML")

  assert_eq \
    "both test and test-integration matrices list horizon" \
    "2" \
    "$matrix_count"
}

test_horizon_helm_validate_loops() {
  echo "Test: helm-validate loops include the horizon-operator chart"

  local loop_count
  loop_count=$(grep -c "operators/horizon/helm/horizon-operator" "$CI_YAML")

  if [ "$loop_count" -ge 3 ]; then
    echo "  PASS: helm-validate references the horizon-operator chart in $loop_count loops"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: expected >=3 helm-validate references to the horizon-operator chart, found $loop_count"
    FAIL=$((FAIL + 1))
  fi
}

test_cleanup_matrix_includes_horizon() {
  echo "Test: the derived cleanup package lists cover horizon"

  # Both cleanup-images.yaml and ci.yaml's cleanup-e2e-tags build their package
  # matrix from this script, so coverage is a property of its output rather than
  # of a list someone has to remember to extend.
  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(echo "$matrix" | sed -n 's/^cleanup-packages=//p')
  e2e_packages=$(echo "$matrix" | sed -n 's/^cleanup-e2e-packages=//p')

  assert_contains \
    "the nightly cleanup covers horizon-operator" \
    "$all_packages" \
    '"horizon-operator"'

  assert_contains \
    "the per-run e2e cleanup covers horizon-operator" \
    "$e2e_packages" \
    '"horizon-operator"'

  assert_contains \
    "the nightly cleanup covers horizon" \
    "$all_packages" \
    '"horizon"'

  assert_contains \
    "the per-run e2e cleanup covers horizon" \
    "$e2e_packages" \
    '"horizon"'

}

# ── ci-resolve-changes.sh documentation ─────────────────────────────────────

test_resolve_script_documents_filter() {
  echo "Test: ci-resolve-changes.sh documents FILTER_horizon"

  assert_file_contains \
    "resolve script documents FILTER_horizon" \
    "$RESOLVE_SCRIPT" \
    "FILTER_horizon"
}

# ── ci-resolve-changes.sh behavioural tests ─────────────────────────────────

test_resolve_emits_horizon_on_operator_change() {
  echo "Test: ci-resolve-changes.sh emits horizon in e2e-operators on a horizon-only change"

  local resolved operators has
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="true" \
    FILTER_docs="false" \
    FILTER_helm="false" \
    FILTER_e2e_infra="false" \
    FILTER_e2e_chaos="false" \
    FILTER_go_common="false" \
    run_resolve
  )

  operators=$(output_value "$resolved" "e2e-operators")
  has=$(output_value "$resolved" "has-e2e-operators")

  assert_contains \
    "horizon-only change emits horizon in the e2e-operators matrix" \
    "$operators" \
    '"horizon"' # JSON array entry

  assert_not_contains \
    "horizon-only change does not pull in keystone" \
    "$operators" \
    '"keystone"'

  assert_eq \
    "horizon-only change sets has-e2e-operators=true" \
    "true" \
    "$has"
}

test_resolve_emits_all_on_go_common_change() {
  echo "Test: ci-resolve-changes.sh emits horizon on a go_common change"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_docs="false" \
    FILTER_helm="false" \
    FILTER_e2e_infra="false" \
    FILTER_e2e_chaos="false" \
    FILTER_go_common="true" \
    run_resolve
  )

  operators=$(output_value "$resolved" "e2e-operators")

  assert_contains \
    "go_common change includes keystone" \
    "$operators" \
    '"keystone"'

  assert_contains \
    "go_common change includes horizon" \
    "$operators" \
    '"horizon"'
}

test_resolve_emits_horizon_on_tag_push() {
  echo "Test: ci-resolve-changes.sh emits horizon in e2e-operators on a tag push"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance" \
    GITHUB_REF="refs/tags/v1.0.0" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_docs="false" \
    FILTER_helm="false" \
    FILTER_e2e_infra="false" \
    FILTER_e2e_chaos="false" \
    FILTER_go_common="false" \
    run_resolve
  )

  operators=$(output_value "$resolved" "e2e-operators")

  assert_contains \
    "tag push forces horizon into the e2e-operators matrix" \
    "$operators" \
    '"horizon"'
}

test_resolve_excludes_horizon_on_keystone_only_change() {
  echo "Test: ci-resolve-changes.sh excludes horizon on a keystone-only change"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="true" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_docs="false" \
    FILTER_helm="false" \
    FILTER_e2e_infra="false" \
    FILTER_e2e_chaos="false" \
    FILTER_go_common="false" \
    run_resolve
  )

  operators=$(output_value "$resolved" "e2e-operators")

  assert_contains \
    "keystone-only change includes keystone" \
    "$operators" \
    '"keystone"'

  assert_not_contains \
    "keystone-only change excludes horizon" \
    "$operators" \
    '"horizon"'
}

# ── Run all tests ────────────────────────────────────────────────────────────
test_horizon_filter_block
echo ""
test_horizon_all_operators
echo ""
test_horizon_filter_env_var
echo ""
test_horizon_test_matrices
echo ""
test_horizon_helm_validate_loops
echo ""
test_cleanup_matrix_includes_horizon
echo ""
test_resolve_script_documents_filter
echo ""
test_resolve_emits_horizon_on_operator_change
echo ""
test_resolve_emits_all_on_go_common_change
echo ""
test_resolve_emits_horizon_on_tag_push
echo ""
test_resolve_excludes_horizon_on_keystone_only_change
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
