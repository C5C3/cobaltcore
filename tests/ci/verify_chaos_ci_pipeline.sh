#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify chaos E2E CI pipeline changes meet requirements
# Validates: path filter, output wiring, ci-resolve-changes.sh integration
# Usage: bash tests/ci/verify_chaos_ci_pipeline.sh

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

echo "=== Chaos E2E CI pipeline verification ==="
echo ""

# ── Helpers (review #1) ──────────────────────────────────────────

# Extract a YAML job section from a workflow file by job name.
# Centralizes the extraction pattern to reduce brittleness (review #1, comment 2).
extract_yaml_job_section() {
  local file="$1" job_name="$2"
  sed -n "/^  ${job_name}:/,/^  [a-zA-Z]/p" "$file"
}

# Assert that a job's needs list contains a specific dependency.
# Matches individual entries rather than the full line to tolerate
# reordering and formatting changes (review #1, comment 1).
assert_needs_entry() {
  local description="$1" section="$2" entry="$3"
  local needs_line entries
  needs_line=$(echo "$section" | grep "needs:" | head -1)
  # Strip YAML list syntax and whitespace, then match the exact entry
  entries=$(echo "$needs_line" | sed 's/.*\[//; s/\]//; s/ //g')
  if echo ",$entries," | grep -qF ",${entry},"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected needs entry: $entry"
    echo "    needs line: $needs_line"
    FAIL=$((FAIL + 1))
  fi
}

# Cache the e2e-chaos job section once for reuse across tests.
E2E_CHAOS_JOB_SECTION=$(extract_yaml_job_section "$CI_YAML" "e2e-chaos")

# ── ci.yaml path filter tests ──────────────────────────────────────────────

test_e2e_chaos_filter_block() {
  echo "Test: ci.yaml has e2e_chaos path filter block"

  assert_file_contains \
    "ci.yaml has e2e_chaos filter block" \
    "$CI_YAML" \
    "e2e_chaos:"

  assert_file_contains \
    "e2e_chaos filter includes tests/e2e-chaos/**" \
    "$CI_YAML" \
    "tests/e2e-chaos/\*\*"

  assert_file_contains \
    "e2e_chaos filter includes deploy/**" \
    "$CI_YAML" \
    "deploy/\*\*"
}

test_e2e_chaos_output() {
  echo "Test: ci.yaml changes job has e2e-chaos output"

  assert_file_contains \
    "changes job outputs e2e-chaos" \
    "$CI_YAML" \
    'e2e-chaos: ${{ steps.result.outputs.e2e-chaos }}'
}

test_e2e_chaos_env_var() {
  echo "Test: ci.yaml passes FILTER_e2e_chaos env var to resolve script"

  assert_file_contains \
    "FILTER_e2e_chaos env var is set" \
    "$CI_YAML" \
    "FILTER_e2e_chaos:"
}

# ── ci-resolve-changes.sh tests ────────────────────────────────────────────

test_resolve_script_documents_filter() {
  echo "Test: ci-resolve-changes.sh documents FILTER_e2e_chaos"

  assert_file_contains \
    "resolve script documents FILTER_e2e_chaos" \
    "$RESOLVE_SCRIPT" \
    "FILTER_e2e_chaos"
}

test_resolve_script_tag_push() {
  echo "Test: ci-resolve-changes.sh sets e2e-chaos=true on tag push"

  # Simulate tag push with minimal env.
  local output
  output=$(mktemp)
  trap 'rm -f "$output"' RETURN

  ALL_OPERATORS="keystone" \
  GITHUB_REF="refs/tags/v1.0.0" \
  GITHUB_OUTPUT="$output" \
  FILTER_keystone="false" \
  FILTER_docs="false" \
  FILTER_helm="false" \
  FILTER_e2e_infra="false" \
  FILTER_e2e_chaos="false" \
  FILTER_go_common="false" \
  bash "$RESOLVE_SCRIPT"

  local result
  result=$(cat "$output")

  assert_contains \
    "tag push emits e2e-chaos=true" \
    "$result" \
    "e2e-chaos=true"
}

test_resolve_script_non_tag_passthrough() {
  echo "Test: ci-resolve-changes.sh passes through FILTER_e2e_chaos on non-tag push"

  # Case 1: FILTER_e2e_chaos=true
  local output output2
  output=$(mktemp)
  output2=$(mktemp)
  trap 'rm -f "$output" "$output2"' RETURN

  ALL_OPERATORS="keystone" \
  GITHUB_REF="refs/heads/main" \
  GITHUB_OUTPUT="$output" \
  FILTER_keystone="false" \
  FILTER_docs="false" \
  FILTER_helm="false" \
  FILTER_e2e_infra="false" \
  FILTER_e2e_chaos="true" \
  FILTER_go_common="false" \
  bash "$RESOLVE_SCRIPT"

  local result
  result=$(cat "$output")

  assert_contains \
    "non-tag push passes through e2e-chaos=true" \
    "$result" \
    "e2e-chaos=true"

  # Case 2: FILTER_e2e_chaos=false
  ALL_OPERATORS="keystone" \
  GITHUB_REF="refs/heads/main" \
  GITHUB_OUTPUT="$output2" \
  FILTER_keystone="false" \
  FILTER_docs="false" \
  FILTER_helm="false" \
  FILTER_e2e_infra="false" \
  FILTER_e2e_chaos="false" \
  FILTER_go_common="false" \
  bash "$RESOLVE_SCRIPT"

  result=$(cat "$output2")

  assert_contains \
    "non-tag push passes through e2e-chaos=false" \
    "$result" \
    "e2e-chaos=false"
}

test_resolve_script_default_value() {
  echo "Test: ci-resolve-changes.sh defaults e2e-chaos to false when env var unset"

  local output
  output=$(mktemp)
  trap 'rm -f "$output"' RETURN

  ALL_OPERATORS="keystone" \
  GITHUB_REF="refs/heads/feature" \
  GITHUB_OUTPUT="$output" \
  FILTER_keystone="false" \
  FILTER_docs="false" \
  FILTER_helm="false" \
  FILTER_e2e_infra="false" \
  FILTER_go_common="false" \
  bash "$RESOLVE_SCRIPT"

  local result
  result=$(cat "$output")

  assert_contains \
    "unset FILTER_e2e_chaos defaults to e2e-chaos=false" \
    "$result" \
    "e2e-chaos=false"
}

# ── ci-resolve-changes.sh go_changed tests ────────────────────────────────

test_resolve_script_go_changed() {
  echo "Test: ci-resolve-changes.sh sets e2e-chaos=true when go_changed is true"

  local output
  output=$(mktemp)
  trap 'rm -f "$output"' RETURN

  ALL_OPERATORS="keystone" \
  GITHUB_REF="refs/heads/main" \
  GITHUB_OUTPUT="$output" \
  FILTER_keystone="false" \
  FILTER_docs="false" \
  FILTER_helm="false" \
  FILTER_e2e_infra="false" \
  FILTER_e2e_chaos="false" \
  FILTER_go_common="true" \
  bash "$RESOLVE_SCRIPT"

  local result
  result=$(cat "$output")

  assert_contains \
    "go_common=true triggers e2e-chaos=true" \
    "$result" \
    "e2e-chaos=true"
}

test_resolve_script_operator_go_change() {
  echo "Test: ci-resolve-changes.sh sets e2e-chaos=true when operator Go code changes (without go_common)"

  local output
  output=$(mktemp)
  trap 'rm -f "$output"' RETURN

  ALL_OPERATORS="keystone" \
  GITHUB_REF="refs/heads/main" \
  GITHUB_OUTPUT="$output" \
  FILTER_keystone="true" \
  FILTER_docs="false" \
  FILTER_helm="false" \
  FILTER_e2e_infra="false" \
  FILTER_e2e_chaos="false" \
  FILTER_go_common="false" \
  bash "$RESOLVE_SCRIPT"

  local result
  result=$(cat "$output")

  assert_contains \
    "operator-specific Go change (keystone=true, go_common=false) triggers e2e-chaos=true" \
    "$result" \
    "e2e-chaos=true"
}

test_resolve_script_docs_only() {
  echo "Test: ci-resolve-changes.sh sets e2e-chaos=false when only docs change"

  local output
  output=$(mktemp)
  trap 'rm -f "$output"' RETURN

  ALL_OPERATORS="keystone" \
  GITHUB_REF="refs/heads/main" \
  GITHUB_OUTPUT="$output" \
  FILTER_keystone="false" \
  FILTER_docs="true" \
  FILTER_helm="false" \
  FILTER_e2e_infra="false" \
  FILTER_e2e_chaos="false" \
  FILTER_go_common="false" \
  bash "$RESOLVE_SCRIPT"

  local result
  result=$(cat "$output")

  assert_contains \
    "docs-only change emits e2e-chaos=false" \
    "$result" \
    "e2e-chaos=false"
}

# ── ci.yaml e2e-chaos job structure tests ──────────────────────────────────

test_e2e_chaos_job_exists() {
  echo "Test: e2e-chaos job exists in ci.yaml"

  assert_file_contains \
    "e2e-chaos job defined" \
    "$CI_YAML" \
    "^  e2e-chaos:"
}

test_e2e_chaos_job_dependencies() {
  echo "Test: e2e-chaos depends on pre-flight checks and e2e-operator"

  local expected_deps=(changes lint shellcheck test test-integration verify-codegen e2e-operator)
  for dep in "${expected_deps[@]}"; do
    assert_needs_entry \
      "e2e-chaos needs includes $dep" \
      "$E2E_CHAOS_JOB_SECTION" \
      "$dep"
  done
}

test_e2e_chaos_if_condition() {
  echo "Test: e2e-chaos if condition gates on e2e-chaos output and blocks on failure"

  assert_contains \
    "e2e-chaos checks e2e-chaos output" \
    "$E2E_CHAOS_JOB_SECTION" \
    "needs.changes.outputs.e2e-chaos == 'true'"

  assert_contains \
    "e2e-chaos blocks on failure" \
    "$E2E_CHAOS_JOB_SECTION" \
    "!contains(needs.*.result, 'failure')"

  assert_contains \
    "e2e-chaos blocks on cancelled" \
    "$E2E_CHAOS_JOB_SECTION" \
    "!contains(needs.*.result, 'cancelled')"
}

test_e2e_chaos_timeout() {
  echo "Test: e2e-chaos job timeout is 60 minutes"

  assert_contains \
    "e2e-chaos timeout-minutes is 60" \
    "$E2E_CHAOS_JOB_SECTION" \
    "timeout-minutes: 60"
}

test_e2e_chaos_continue_on_error() {
  echo "Test: e2e-chaos job has continue-on-error: true"

  assert_contains \
    "e2e-chaos continue-on-error is true" \
    "$E2E_CHAOS_JOB_SECTION" \
    "continue-on-error: true"
}

test_e2e_chaos_chainsaw_command() {
  echo "Test: e2e-chaos runs correct chainsaw command"

  assert_contains \
    "e2e-chaos runs chainsaw with chaos config" \
    "$E2E_CHAOS_JOB_SECTION" \
    "chainsaw test --config tests/e2e-chaos/chainsaw-config.yaml tests/e2e-chaos/"

  assert_contains \
    "e2e-chaos creates _output/reports directory" \
    "$E2E_CHAOS_JOB_SECTION" \
    "mkdir -p _output/reports"
}

test_e2e_chaos_kind_cluster() {
  echo "Test: e2e-chaos creates kind cluster and deploys infra"

  assert_contains \
    "e2e-chaos uses kind-action" \
    "$E2E_CHAOS_JOB_SECTION" \
    "helm/kind-action@"

  assert_contains \
    "e2e-chaos uses kind-config.yaml" \
    "$E2E_CHAOS_JOB_SECTION" \
    "config: hack/kind-config.yaml"

  assert_contains \
    "e2e-chaos cluster name resolves to the shared KIND_CLUSTER env" \
    "$E2E_CHAOS_JOB_SECTION" \
    "cluster_name: \${{ env.KIND_CLUSTER }}"

  assert_contains \
    "e2e-chaos uses setup-e2e-infra composite" \
    "$E2E_CHAOS_JOB_SECTION" \
    "./.github/actions/setup-e2e-infra"
}

test_e2e_chaos_builds_keystone() {
  echo "Test: e2e-chaos builds and deploys keystone operator"

  assert_contains \
    "e2e-chaos builds keystone operator image" \
    "$E2E_CHAOS_JOB_SECTION" \
    "make docker-build OPERATOR=keystone"

  assert_contains \
    "e2e-chaos builds service image" \
    "$E2E_CHAOS_JOB_SECTION" \
    "hack/ci-build-service-image.sh"

  assert_contains \
    "e2e-chaos loads images into kind" \
    "$E2E_CHAOS_JOB_SECTION" \
    "kind load docker-image"

  assert_contains \
    "e2e-chaos deploys operator" \
    "$E2E_CHAOS_JOB_SECTION" \
    "hack/ci-deploy-operator.sh"
}

test_e2e_chaos_junit_report() {
  echo "Test: e2e-chaos uploads JUnit report correctly"

  assert_contains \
    "JUnit artifact name is e2e-chaos-junit-report" \
    "$E2E_CHAOS_JOB_SECTION" \
    "name: e2e-chaos-junit-report"

  assert_contains \
    "JUnit report path is _output/reports/" \
    "$E2E_CHAOS_JOB_SECTION" \
    "path: _output/reports/"

  assert_contains \
    "JUnit retention is 14 days" \
    "$E2E_CHAOS_JOB_SECTION" \
    "retention-days: 14"
}

test_e2e_chaos_junit_always() {
  echo "Test: JUnit report uploaded regardless of test outcome"

  # Count occurrences of "if: always()" in the job section - should have at least 2
  # (one for diagnostics, one for upload)
  local always_count
  always_count=$(echo "$E2E_CHAOS_JOB_SECTION" | grep -c "if: always()" || true)

  assert_gte \
    "e2e-chaos has at least 2 'if: always()' steps (diagnostics + upload)" \
    "$always_count" \
    2
}

test_e2e_chaos_diagnostics() {
  echo "Test: e2e-chaos dumps diagnostics with OPERATOR=keystone"

  assert_contains \
    "e2e-chaos runs diagnostics script" \
    "$E2E_CHAOS_JOB_SECTION" \
    "hack/ci-dump-diagnostics.sh"

  assert_contains \
    "diagnostics uses OPERATOR: keystone" \
    "$E2E_CHAOS_JOB_SECTION" \
    "OPERATOR: keystone"
}

test_e2e_chaos_publish_no_dependency() {
  echo "Test: publish jobs do not depend on e2e-chaos"

  local publish_jobs=("build-and-push" "helm-push" "merge-operator-images" "github-release")

  for job in "${publish_jobs[@]}"; do
    local needs_line
    needs_line=$(extract_yaml_job_section "$CI_YAML" "$job" | grep "needs:" | head -1)

    if [[ -z "$needs_line" ]]; then
      echo "  SKIP: could not find needs line for $job"
      SKIP=$((SKIP + 1))
      continue
    fi

    assert_not_contains \
      "$job needs does not include e2e-chaos" \
      "$needs_line" \
      "e2e-chaos"
  done
}

# ── Run all tests ──────────────────────────────────────────────────────────
test_e2e_chaos_filter_block
echo ""
test_e2e_chaos_output
echo ""
test_e2e_chaos_env_var
echo ""
test_resolve_script_documents_filter
echo ""
test_resolve_script_tag_push
echo ""
test_resolve_script_non_tag_passthrough
echo ""
test_resolve_script_default_value
echo ""
test_resolve_script_go_changed
echo ""
test_resolve_script_operator_go_change
echo ""
test_resolve_script_docs_only
echo ""
test_e2e_chaos_job_exists
echo ""
test_e2e_chaos_job_dependencies
echo ""
test_e2e_chaos_if_condition
echo ""
test_e2e_chaos_timeout
echo ""
test_e2e_chaos_continue_on_error
echo ""
test_e2e_chaos_chainsaw_command
echo ""
test_e2e_chaos_kind_cluster
echo ""
test_e2e_chaos_builds_keystone
echo ""
test_e2e_chaos_junit_report
echo ""
test_e2e_chaos_junit_always
echo ""
test_e2e_chaos_diagnostics
echo ""
test_e2e_chaos_publish_no_dependency
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
