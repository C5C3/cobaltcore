#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify c5c3 operator CI pipeline wiring meets requirements.
# Validates: c5c3 paths-filter block, FILTER_c5c3 env, ALL_OPERATORS membership,
# cleanup-e2e-tags package list, and that ci-resolve-changes.sh emits c5c3 in the
# e2e-operators matrix once c5c3 is a known operator.
# Usage: bash tests/ci/verify_c5c3_ci_pipeline.sh

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

echo "=== c5c3 operator CI pipeline verification ==="
echo ""

# ── Helpers ─────────────────────────────────────────────────────────────────

# Extract a YAML job section from a workflow file by job name.
extract_yaml_job_section() {
  local file="$1" job_name="$2"
  sed -n "/^  ${job_name}:/,/^  [a-zA-Z]/p" "$file"
}

# Run ci-resolve-changes.sh with the supplied env and echo the GITHUB_OUTPUT
# contents. ALL_OPERATORS deliberately mirrors the ci.yaml value ("keystone
# c5c3") so the behavioural assertions exercise the real resolution codepath.
# Args are passed as KEY=VALUE pairs through the caller's env block.
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

test_c5c3_filter_block() {
  echo "Test: ci.yaml has a c5c3 paths-filter block"

  assert_file_contains \
    "ci.yaml declares a c5c3 filter" \
    "$CI_YAML" \
    "^            c5c3:"

  assert_file_contains \
    "c5c3 filter includes operators/c5c3/**" \
    "$CI_YAML" \
    "operators/c5c3/\*\*"
}

test_c5c3_all_operators() {
  echo "Test: ci.yaml ALL_OPERATORS includes c5c3"

  local all_operators_line
  all_operators_line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)

  assert_contains \
    "ALL_OPERATORS lists keystone" \
    "$all_operators_line" \
    "keystone"

  assert_contains \
    "ALL_OPERATORS lists c5c3" \
    "$all_operators_line" \
    "c5c3"
}

test_c5c3_filter_env_var() {
  echo "Test: ci.yaml passes FILTER_c5c3 env var to the resolve step"

  assert_file_contains \
    "FILTER_c5c3 env var is wired from steps.filter.outputs.c5c3" \
    "$CI_YAML" \
    'FILTER_c5c3: ${{ steps.filter.outputs.c5c3 }}'
}

test_cleanup_matrix_includes_c5c3_operator() {
  echo "Test: cleanup-e2e-tags matrix includes c5c3-operator"

  local cleanup_section
  cleanup_section=$(extract_yaml_job_section "$CI_YAML" "cleanup-e2e-tags")

  assert_contains \
    "cleanup-e2e-tags package matrix lists c5c3-operator" \
    "$cleanup_section" \
    "c5c3-operator"
}

# ── ci-resolve-changes.sh documentation ─────────────────────────────────────

test_resolve_script_documents_filter() {
  echo "Test: ci-resolve-changes.sh documents FILTER_c5c3"

  assert_file_contains \
    "resolve script documents FILTER_c5c3" \
    "$RESOLVE_SCRIPT" \
    "FILTER_c5c3"
}

# ── ci-resolve-changes.sh behavioural tests ─────────────────────────────────

test_resolve_emits_c5c3_on_operator_change() {
  echo "Test: ci-resolve-changes.sh emits c5c3 in e2e-operators on a c5c3-only change"

  local resolved operators has
  resolved=$(
    ALL_OPERATORS="keystone c5c3" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="true" \
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
    "c5c3-only change emits c5c3 in the e2e-operators matrix" \
    "$operators" \
    '"c5c3"' # JSON array entry

  assert_not_contains \
    "c5c3-only change does not pull in keystone" \
    "$operators" \
    '"keystone"'

  assert_eq \
    "c5c3-only change sets has-e2e-operators=true" \
    "true" \
    "$has"
}

test_resolve_emits_both_on_go_common_change() {
  echo "Test: ci-resolve-changes.sh emits keystone and c5c3 on a go_common change"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
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
    "go_common change includes c5c3" \
    "$operators" \
    '"c5c3"'
}

test_resolve_emits_c5c3_on_tag_push() {
  echo "Test: ci-resolve-changes.sh emits c5c3 in e2e-operators on a tag push"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3" \
    GITHUB_REF="refs/tags/v1.0.0" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_docs="false" \
    FILTER_helm="false" \
    FILTER_e2e_infra="false" \
    FILTER_e2e_chaos="false" \
    FILTER_go_common="false" \
    run_resolve
  )

  operators=$(output_value "$resolved" "e2e-operators")

  assert_contains \
    "tag push forces c5c3 into the e2e-operators matrix" \
    "$operators" \
    '"c5c3"'
}

test_resolve_excludes_c5c3_on_keystone_only_change() {
  echo "Test: ci-resolve-changes.sh excludes c5c3 on a keystone-only change"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="true" \
    FILTER_c5c3="false" \
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
    "keystone-only change excludes c5c3" \
    "$operators" \
    '"c5c3"'
}

# ── Run all tests ────────────────────────────────────────────────────────────
test_c5c3_filter_block
echo ""
test_c5c3_all_operators
echo ""
test_c5c3_filter_env_var
echo ""
test_cleanup_matrix_includes_c5c3_operator
echo ""
test_resolve_script_documents_filter
echo ""
test_resolve_emits_c5c3_on_operator_change
echo ""
test_resolve_emits_both_on_go_common_change
echo ""
test_resolve_emits_c5c3_on_tag_push
echo ""
test_resolve_excludes_c5c3_on_keystone_only_change
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
