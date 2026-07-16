#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify glance operator CI pipeline wiring meets requirements.
# Validates: glance paths-filter block, FILTER_glance env, ALL_OPERATORS
# membership, test/helm-validate/cleanup matrices, the e2e-chaos deploy step and
# test_dirs, the service-dimension tempest legs, and that ci-resolve-changes.sh
# emits glance in the e2e-operators matrix once glance is a known operator.
# Usage: bash tests/ci/verify_glance_ci_pipeline.sh

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

echo "=== glance operator CI pipeline verification ==="
echo ""

# ── Helpers ─────────────────────────────────────────────────────────────────

# Extract a YAML job section from a workflow file by job name.
extract_yaml_job_section() {
  local file="$1" job_name="$2"
  sed -n "/^  ${job_name}:/,/^  [a-zA-Z]/p" "$file"
}

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

test_glance_filter_block() {
  echo "Test: ci.yaml has a glance paths-filter block"

  assert_file_contains \
    "ci.yaml declares a glance filter" \
    "$CI_YAML" \
    "^            glance:"

  assert_file_contains \
    "glance filter includes operators/glance/**" \
    "$CI_YAML" \
    "operators/glance/\*\*"

  assert_file_contains \
    "glance filter includes images/glance/**" \
    "$CI_YAML" \
    "images/glance/\*\*"
}

test_glance_all_operators() {
  echo "Test: ci.yaml ALL_OPERATORS includes glance"

  local all_operators_line
  all_operators_line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)

  assert_contains \
    "ALL_OPERATORS lists keystone" \
    "$all_operators_line" \
    "keystone"

  assert_contains \
    "ALL_OPERATORS lists glance" \
    "$all_operators_line" \
    "glance"
}

test_glance_filter_env_var() {
  echo "Test: ci.yaml passes FILTER_glance env var to the resolve step"

  assert_file_contains \
    "FILTER_glance env var is wired from steps.filter.outputs.glance" \
    "$CI_YAML" \
    'FILTER_glance: ${{ steps.filter.outputs.glance }}'
}

test_glance_test_matrices() {
  echo "Test: unit and integration test matrices include glance"

  local matrix_count
  matrix_count=$(grep -c "target: \[common, keystone, c5c3, horizon, glance\]" "$CI_YAML")

  assert_eq \
    "both test and test-integration matrices list glance" \
    "2" \
    "$matrix_count"
}

test_glance_helm_validate_loops() {
  echo "Test: helm-validate loops include the glance-operator chart"

  local loop_count
  loop_count=$(grep -c "operators/glance/helm/glance-operator" "$CI_YAML")

  if [ "$loop_count" -ge 3 ]; then
    echo "  PASS: helm-validate references the glance-operator chart in $loop_count loops"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: expected >=3 helm-validate references to the glance-operator chart, found $loop_count"
    FAIL=$((FAIL + 1))
  fi
}

test_cleanup_matrix_includes_glance() {
  echo "Test: cleanup-e2e-tags matrix includes glance-operator and glance"

  local cleanup_section
  cleanup_section=$(extract_yaml_job_section "$CI_YAML" "cleanup-e2e-tags")

  assert_contains \
    "cleanup-e2e-tags package matrix lists glance-operator" \
    "$cleanup_section" \
    "glance-operator"

  assert_contains \
    "cleanup-e2e-tags package matrix lists the glance service image" \
    "$cleanup_section" \
    "glance-operator, glance,"
}

# ── e2e-chaos wiring ────────────────────────────────────────────────────────

test_glance_chaos_wiring() {
  echo "Test: e2e-chaos deploys the glance operator and runs the glance suites"

  # Scope to the e2e-chaos job: both needles below also occur in the tempest
  # job, so a whole-file grep would still pass with the e2e-chaos image-load
  # and deploy steps deleted — leaving glance-operator-pod-kill to run against
  # a cluster with no glance-operator and an empty pod selector.
  local chaos_section
  chaos_section=$(extract_yaml_job_section "$CI_YAML" "e2e-chaos")

  assert_contains \
    "e2e-chaos loads the glance-operator image" \
    "$chaos_section" \
    "IMAGE_PREFIX }}/glance-operator:dev"

  assert_contains \
    "e2e-chaos deploys the glance operator into glance-system" \
    "$chaos_section" \
    "NAMESPACE: glance-system"

  # The two test_dirs needles are unique to e2e-chaos, so they stay file-wide.
  assert_file_contains \
    "pod-leg test_dirs include the glance operator-kill suite" \
    "$CI_YAML" \
    "tests/e2e-chaos/glance-operator-pod-kill"

  assert_file_contains \
    "network-leg test_dirs include the glance garage-outage suite" \
    "$CI_YAML" \
    "tests/e2e-chaos/glance-garage-outage"
}

# ── tempest service-dimension legs ──────────────────────────────────────────

test_glance_tempest_wiring() {
  echo "Test: tempest job carries the glance service leg"

  assert_file_contains \
    "tempest artifact name is namespaced by service" \
    "$CI_YAML" \
    "tempest-\${{ matrix.service }}-\${{ matrix.release }}-results"

  assert_file_contains \
    "tempest bootstraps the glance image catalog" \
    "$CI_YAML" \
    "job/glance-tempest-catalog-setup"

  assert_file_contains \
    "tempest deploys the Glance CR for the glance leg" \
    "$CI_YAML" \
    "kubectl wait glance/\${{ matrix.glance-cr-name }}"

  assert_file_contains \
    "tempest passes GLANCE_K8S_NAME to the Tempest wrapper" \
    "$CI_YAML" \
    "GLANCE_K8S_NAME: \${{ matrix.glance-cr-name }}"
}

# ── ci-resolve-changes.sh documentation ─────────────────────────────────────

test_resolve_script_documents_filter() {
  echo "Test: ci-resolve-changes.sh documents FILTER_glance"

  assert_file_contains \
    "resolve script documents FILTER_glance" \
    "$RESOLVE_SCRIPT" \
    "FILTER_glance"
}

# ── ci-resolve-changes.sh behavioural tests ─────────────────────────────────

test_resolve_emits_glance_on_operator_change() {
  echo "Test: ci-resolve-changes.sh emits glance in e2e-operators on a glance-only change"

  local resolved operators has
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="true" \
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
    "glance-only change emits glance in the e2e-operators matrix" \
    "$operators" \
    '"glance"' # JSON array entry

  assert_not_contains \
    "glance-only change does not pull in keystone" \
    "$operators" \
    '"keystone"'

  assert_eq \
    "glance-only change sets has-e2e-operators=true" \
    "true" \
    "$has"
}

test_resolve_emits_all_on_go_common_change() {
  echo "Test: ci-resolve-changes.sh emits all four operators on a go_common change"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="false" \
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

  assert_contains \
    "go_common change includes glance" \
    "$operators" \
    '"glance"'
}

test_resolve_emits_glance_on_tag_push() {
  echo "Test: ci-resolve-changes.sh emits glance in e2e-operators on a tag push"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance" \
    GITHUB_REF="refs/tags/v1.0.0" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="false" \
    FILTER_docs="false" \
    FILTER_helm="false" \
    FILTER_e2e_infra="false" \
    FILTER_e2e_chaos="false" \
    FILTER_go_common="false" \
    run_resolve
  )

  operators=$(output_value "$resolved" "e2e-operators")

  assert_contains \
    "tag push forces glance into the e2e-operators matrix" \
    "$operators" \
    '"glance"'
}

test_resolve_excludes_glance_on_keystone_only_change() {
  echo "Test: ci-resolve-changes.sh excludes glance on a keystone-only change"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="true" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="false" \
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
    "keystone-only change excludes glance" \
    "$operators" \
    '"glance"'
}

# ── Run all tests ────────────────────────────────────────────────────────────
test_glance_filter_block
echo ""
test_glance_all_operators
echo ""
test_glance_filter_env_var
echo ""
test_glance_test_matrices
echo ""
test_glance_helm_validate_loops
echo ""
test_cleanup_matrix_includes_glance
echo ""
test_glance_chaos_wiring
echo ""
test_glance_tempest_wiring
echo ""
test_resolve_script_documents_filter
echo ""
test_resolve_emits_glance_on_operator_change
echo ""
test_resolve_emits_all_on_go_common_change
echo ""
test_resolve_emits_glance_on_tag_push
echo ""
test_resolve_excludes_glance_on_keystone_only_change
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
