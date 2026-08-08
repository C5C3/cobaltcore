#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify placement operator CI pipeline wiring meets requirements.
# Validates: placement paths-filter block, the helm filter entry,
# FILTER_placement env, ALL_OPERATORS membership, test/helm-validate/cleanup
# matrices, the e2e-chaos pod-kill leg and its pod-leg-only image load/deploy,
# the placement entry in the build-e2e-images operator union, and
# that ci-resolve-changes.sh emits placement in the e2e-operators matrix once
# placement is a known operator.
# Placement ships no tempest plugin, so the tempest job carries no placement
# leg to assert.
# Usage: bash tests/ci/verify_placement_ci_pipeline.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
CLEANUP_YAML="$PROJECT_ROOT/.github/workflows/cleanup-images.yaml"
RESOLVE_SCRIPT="$PROJECT_ROOT/hack/ci-resolve-changes.sh"

echo "=== placement operator CI pipeline verification ==="
echo ""

# ── Helpers ─────────────────────────────────────────────────────────────────

# Extract a YAML job section from a workflow file by job name.
extract_yaml_job_section() {
  local file="$1" job_name="$2"
  sed -n "/^  ${job_name}:/,/^  [a-zA-Z]/p" "$file"
}

# Extract a single paths-filter block from ci.yaml by filter name. Filter keys
# sit at 12-space indent and their path entries deeper, so the next 12-space key
# ends the block. Scoping matters for shared entries such as
# operators/Dockerfile, which every operator filter lists.
extract_paths_filter_block() {
  local file="$1" filter_name="$2"
  sed -n "/^            ${filter_name}:/,/^            [a-z]/p" "$file"
}

# Extract a single step from a job section by step name. Steps sit at 6-space
# indent, so the next 6-space "- name:" ends the block. Needed to assert on a
# step's own `if:` — a job-wide grep cannot tell which step carries a gate.
extract_yaml_step() {
  local section="$1" step_name="$2"
  echo "$section" | sed -n "/^      - name: ${step_name}\$/,/^      - name: /p"
}

# Run ci-resolve-changes.sh with the supplied env and echo the GITHUB_OUTPUT
# contents. ALL_OPERATORS deliberately mirrors the ci.yaml value ("keystone
# c5c3 horizon glance placement barbican") so the behavioural assertions
# exercise the real resolution codepath. Args are passed as KEY=VALUE pairs
# through the caller's env block.
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

test_placement_filter_block() {
  echo "Test: ci.yaml has a placement paths-filter block"

  assert_file_contains \
    "ci.yaml declares a placement filter" \
    "$CI_YAML" \
    "^            placement:"

  local filter_block
  filter_block=$(extract_paths_filter_block "$CI_YAML" "placement")

  assert_contains \
    "placement filter includes operators/placement/**" \
    "$filter_block" \
    "operators/placement/**"

  assert_contains \
    "placement filter includes operators/Dockerfile" \
    "$filter_block" \
    "operators/Dockerfile"

  assert_contains \
    "placement filter includes images/placement/**" \
    "$filter_block" \
    "images/placement/**"
}

test_placement_helm_filter() {
  echo "Test: the helm paths-filter covers the placement operator chart"

  local helm_block
  helm_block=$(extract_paths_filter_block "$CI_YAML" "helm")

  assert_contains \
    "helm filter includes operators/placement/helm/**" \
    "$helm_block" \
    "operators/placement/helm/**"
}

test_placement_all_operators() {
  echo "Test: ci.yaml ALL_OPERATORS includes placement"

  local all_operators_line
  all_operators_line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)

  assert_contains \
    "ALL_OPERATORS lists keystone" \
    "$all_operators_line" \
    "keystone"

  assert_contains \
    "ALL_OPERATORS lists placement" \
    "$all_operators_line" \
    "placement"
}

test_placement_filter_env_var() {
  echo "Test: ci.yaml passes FILTER_placement env var to the resolve step"

  assert_file_contains \
    "FILTER_placement env var is wired from steps.filter.outputs.placement" \
    "$CI_YAML" \
    'FILTER_placement: ${{ steps.filter.outputs.placement }}'
}

test_placement_test_matrices() {
  echo "Test: unit and integration test matrices include placement"

  local matrix_count
  matrix_count=$(grep -c "target: \[common, keystone, c5c3, horizon, glance, placement, barbican\]" "$CI_YAML")

  assert_eq \
    "both test and test-integration matrices list placement" \
    "2" \
    "$matrix_count"
}

test_placement_helm_validate_loops() {
  echo "Test: helm-validate loops include the placement-operator chart"

  local loop_count
  loop_count=$(grep -c "operators/placement/helm/placement-operator" "$CI_YAML")

  if [ "$loop_count" -ge 3 ]; then
    echo "  PASS: helm-validate references the placement-operator chart in $loop_count loops"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: expected >=3 helm-validate references to the placement-operator chart, found $loop_count"
    FAIL=$((FAIL + 1))
  fi
}

test_cleanup_matrices_include_placement() {
  echo "Test: cleanup matrices include placement-operator and placement"

  local cleanup_section
  cleanup_section=$(extract_yaml_job_section "$CI_YAML" "cleanup-e2e-tags")

  assert_contains \
    "cleanup-e2e-tags package matrix lists placement-operator" \
    "$cleanup_section" \
    "placement-operator"

  assert_contains \
    "cleanup-e2e-tags package matrix lists the placement service image" \
    "$cleanup_section" \
    "placement-operator, placement,"

  local operator_images_section stale_tags_section
  operator_images_section=$(extract_yaml_job_section "$CLEANUP_YAML" "cleanup-operator-images")
  stale_tags_section=$(extract_yaml_job_section "$CLEANUP_YAML" "cleanup-e2e-stale-tags")

  assert_contains \
    "cleanup-operator-images matrix lists placement-operator" \
    "$operator_images_section" \
    "placement-operator"

  assert_contains \
    "cleanup-e2e-stale-tags matrix lists placement-operator and the placement service image" \
    "$stale_tags_section" \
    "placement-operator, placement,"
}

# ── e2e-chaos wiring ────────────────────────────────────────────────────────

test_placement_chaos_wiring() {
  echo "Test: e2e-chaos deploys the placement operator and runs the pod-kill suite"

  # Scope every needle to the e2e-chaos job: the image name also occurs in the
  # build and load jobs, so a whole-file grep would still pass with the
  # e2e-chaos image-load and deploy steps deleted — leaving
  # placement-operator-pod-kill to run against a cluster with no
  # placement-operator and an empty pod selector.
  local chaos_section
  chaos_section=$(extract_yaml_job_section "$CI_YAML" "e2e-chaos")

  assert_contains \
    "pod-leg test_dirs include the placement operator-kill suite" \
    "$chaos_section" \
    "tests/e2e-chaos/placement-operator-pod-kill"

  # Loading placement onto the pod leg takes two steps with two different
  # renderings, so each needs its own needle. The GHCR pull step interpolates
  # through format() to stay blank on the network leg, and therefore contains
  # no `IMAGE_PREFIX }}/placement...` literal at all — a single chaos_section
  # substring check silently covers only the kind-load step.
  local pull_step
  pull_step=$(extract_yaml_step "$chaos_section" "Load E2E images")

  assert_contains \
    "the GHCR pull step lists the placement-operator image on the pod leg" \
    "$pull_step" \
    "format('{0}/placement-operator:dev', env.IMAGE_PREFIX)"

  assert_contains \
    "the GHCR pull step lists the placement service image on the pod leg" \
    "$pull_step" \
    "format('{0}/placement:2025.2', env.IMAGE_PREFIX)"

  local kind_load_step
  kind_load_step=$(extract_yaml_step "$chaos_section" "Load placement images into kind")

  assert_contains \
    "the kind-load step loads the placement-operator image" \
    "$kind_load_step" \
    "IMAGE_PREFIX }}/placement-operator:dev"

  assert_contains \
    "the kind-load step loads the placement service image" \
    "$kind_load_step" \
    "IMAGE_PREFIX }}/placement:2025.2"

  assert_contains \
    "e2e-chaos deploys the placement operator into placement-system" \
    "$chaos_section" \
    "NAMESPACE: placement-system"

  # placement-operator-pod-kill is the only suite touching placement and it is
  # on the pod leg; the network leg (mariadb-network-latency,
  # mariadb-network-partition, glance-garage-outage, chaos-mesh-health) never
  # references it. Ungated, that leg kind-loads and helm-installs placement on
  # every run for zero coverage.
  assert_contains \
    "the placement kind-load step is gated on the pod leg" \
    "$kind_load_step" \
    "if: matrix.suite == 'pod'"

  assert_contains \
    "the placement operator deploy step is gated on the pod leg" \
    "$(extract_yaml_step "$chaos_section" "Deploy placement operator")" \
    "if: matrix.suite == 'pod'"
}

# ── build-e2e-images wiring ─────────────────────────────────────────────────

test_placement_build_is_unconditional() {
  echo "Test: build-e2e-images always builds placement"

  local build_section resolve_step
  build_section=$(extract_yaml_job_section "$CI_YAML" "build-e2e-images")
  resolve_step=$(extract_yaml_step "$build_section" "Resolve build operators")

  # The e2e-chaos pod leg consumes placement-operator:dev and placement:2025.2
  # unconditionally, so placement joins keystone and glance in the fixed union.
  assert_contains \
    "the unconditional build union lists keystone, glance and placement" \
    "$resolve_step" \
    "for base in keystone glance placement barbican; do"

  # Gating the union on a hand-copied duplicate of the e2e-chaos `if:` is the
  # failure this asserts against: the two conditions drift, build-e2e-images
  # stops pushing the placement tags, and the blocking pod leg fails on
  # `manifest unknown` an hour into the run. A non-empty operator matrix
  # already implies e2e-chaos runs, so the gate saved almost nothing anyway.
  assert_not_contains \
    "the build union does not re-derive the e2e-chaos trigger condition" \
    "$resolve_step" \
    "needs.changes.outputs.e2e-chaos"

  assert_not_contains \
    "the build union does not re-derive the run-chaos label condition" \
    "$resolve_step" \
    "run-chaos"
}

# ── ci-resolve-changes.sh documentation ─────────────────────────────────────

test_resolve_script_documents_filter() {
  echo "Test: ci-resolve-changes.sh documents FILTER_placement"

  assert_file_contains \
    "resolve script documents FILTER_placement" \
    "$RESOLVE_SCRIPT" \
    "FILTER_placement"
}

# ── ci-resolve-changes.sh behavioural tests ─────────────────────────────────

test_resolve_emits_placement_on_operator_change() {
  echo "Test: ci-resolve-changes.sh emits placement in e2e-operators on a placement-only change"

  local resolved operators has
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="false" \
    FILTER_placement="true" \
    FILTER_barbican="false" \
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
    "placement-only change emits placement in the e2e-operators matrix" \
    "$operators" \
    '"placement"' # JSON array entry

  assert_not_contains \
    "placement-only change does not pull in keystone" \
    "$operators" \
    '"keystone"'

  assert_eq \
    "placement-only change sets has-e2e-operators=true" \
    "true" \
    "$has"
}

test_resolve_excludes_placement_on_keystone_only_change() {
  echo "Test: ci-resolve-changes.sh excludes placement on a keystone-only change"

  local resolved operators
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="true" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="false" \
    FILTER_placement="false" \
    FILTER_barbican="false" \
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
    "keystone-only change excludes placement" \
    "$operators" \
    '"placement"'
}

test_resolve_emits_all_on_go_common_change() {
  echo "Test: ci-resolve-changes.sh emits all six operators on a go_common change"

  local resolved operators op
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="false" \
    FILTER_placement="false" \
    FILTER_barbican="false" \
    FILTER_docs="false" \
    FILTER_helm="false" \
    FILTER_e2e_infra="false" \
    FILTER_e2e_chaos="false" \
    FILTER_go_common="true" \
    run_resolve
  )

  operators=$(output_value "$resolved" "e2e-operators")

  for op in keystone c5c3 horizon glance placement barbican; do
    assert_contains \
      "go_common change includes $op" \
      "$operators" \
      "\"$op\""
  done
}

test_resolve_emits_all_on_tag_push() {
  echo "Test: ci-resolve-changes.sh emits all six operators on a tag push"

  local resolved operators op
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican" \
    GITHUB_REF="refs/tags/v1.0.0" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="false" \
    FILTER_placement="false" \
    FILTER_barbican="false" \
    FILTER_docs="false" \
    FILTER_helm="false" \
    FILTER_e2e_infra="false" \
    FILTER_e2e_chaos="false" \
    FILTER_go_common="false" \
    run_resolve
  )

  operators=$(output_value "$resolved" "e2e-operators")

  for op in keystone c5c3 horizon glance placement barbican; do
    assert_contains \
      "tag push forces $op into the e2e-operators matrix" \
      "$operators" \
      "\"$op\""
  done
}

# ── Run all tests ────────────────────────────────────────────────────────────
test_placement_filter_block
echo ""
test_placement_helm_filter
echo ""
test_placement_all_operators
echo ""
test_placement_filter_env_var
echo ""
test_placement_test_matrices
echo ""
test_placement_helm_validate_loops
echo ""
test_cleanup_matrices_include_placement
echo ""
test_placement_chaos_wiring
echo ""
test_placement_build_is_unconditional
echo ""
test_resolve_script_documents_filter
echo ""
test_resolve_emits_placement_on_operator_change
echo ""
test_resolve_excludes_placement_on_keystone_only_change
echo ""
test_resolve_emits_all_on_go_common_change
echo ""
test_resolve_emits_all_on_tag_push
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
