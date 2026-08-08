#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify barbican operator CI pipeline wiring meets requirements.
# Validates: barbican paths-filter block, the helm filter entry,
# FILTER_barbican env, ALL_OPERATORS membership, test/helm-validate/cleanup
# matrices, the codecov flags, the BARBICAN_SECRET_STORE_GRANTS passthrough
# on the e2e-operator deploy step, both e2e-chaos legs with their ungated image
# load and operator deploy, the barbican entry in the build-e2e-images operator
# union, the tempest service leg, and that ci-resolve-changes.sh emits barbican
# in the e2e-operators matrix once barbican is a known operator.
# No CI job runs this script; it is a hand-run check of the workflow wiring.
# Usage: bash tests/ci/verify_barbican_ci_pipeline.sh

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
CODECOV_YML="$PROJECT_ROOT/.codecov.yml"
RESOLVE_SCRIPT="$PROJECT_ROOT/hack/ci-resolve-changes.sh"
TEMPEST_MATRIX_SCRIPT="$PROJECT_ROOT/hack/ci-generate-tempest-matrix.sh"

echo "=== barbican operator CI pipeline verification ==="
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

# Extract one e2e-chaos matrix leg from the job section. Legs sit at 10-space
# indent under matrix.include, so the next leg or the job's 4-space `runs-on:`
# key ends the block. Both legs list test_dirs, so a job-wide grep cannot tell
# which leg a chaos suite runs on.
extract_chaos_leg() {
  local section="$1" suite="$2"
  echo "$section" | sed -nE "/^          - suite: ${suite}\$/,/^(          - suite:|    runs-on:)/p"
}

# Extract a single flag block from .codecov.yml by flag name. Flag names sit at
# 4-space indent, so the next flag ends the block.
extract_codecov_flag_block() {
  local flag_name="$1"
  sed -n "/^    - name: ${flag_name}\$/,/^    - name: /p" "$CODECOV_YML"
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

test_barbican_filter_block() {
  echo "Test: ci.yaml has a barbican paths-filter block"

  assert_file_contains \
    "ci.yaml declares a barbican filter" \
    "$CI_YAML" \
    "^            barbican:"

  local filter_block
  filter_block=$(extract_paths_filter_block "$CI_YAML" "barbican")

  assert_contains \
    "barbican filter includes operators/barbican/**" \
    "$filter_block" \
    "operators/barbican/**"

  assert_contains \
    "barbican filter includes operators/Dockerfile" \
    "$filter_block" \
    "operators/Dockerfile"

  assert_contains \
    "barbican filter includes images/barbican/**" \
    "$filter_block" \
    "images/barbican/**"
}

test_barbican_helm_filter() {
  echo "Test: the helm paths-filter covers the barbican operator chart"

  local helm_block
  helm_block=$(extract_paths_filter_block "$CI_YAML" "helm")

  assert_contains \
    "helm filter includes operators/barbican/helm/**" \
    "$helm_block" \
    "- 'operators/barbican/helm/**'"
}

test_barbican_all_operators() {
  echo "Test: ci.yaml ALL_OPERATORS includes barbican"

  local all_operators_line
  all_operators_line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)

  assert_contains \
    "ALL_OPERATORS lists keystone" \
    "$all_operators_line" \
    "keystone"

  assert_contains \
    "ALL_OPERATORS lists barbican" \
    "$all_operators_line" \
    " barbican"
}

test_barbican_filter_env_var() {
  echo "Test: ci.yaml passes FILTER_barbican env var to the resolve step"

  assert_file_contains \
    "FILTER_barbican env var is wired from steps.filter.outputs.barbican" \
    "$CI_YAML" \
    'FILTER_barbican: ${{ steps.filter.outputs.barbican }}'
}

test_barbican_test_matrices() {
  echo "Test: unit and integration test matrices include barbican"

  local matrix_count
  matrix_count=$(grep -c "target: \[common, keystone, c5c3, horizon, glance, placement, barbican\]" "$CI_YAML")

  assert_eq \
    "both test and test-integration matrices list barbican" \
    "2" \
    "$matrix_count"
}

test_barbican_helm_validate_loops() {
  echo "Test: helm-validate loops include the barbican-operator chart"

  local loop_count
  loop_count=$(grep -c " operators/barbican/helm/barbican-operator" "$CI_YAML")

  assert_eq \
    "all three helm-validate loops list the barbican-operator chart" \
    "3" \
    "$loop_count"
}

test_cleanup_matrices_include_barbican() {
  echo "Test: cleanup matrices include barbican-operator and barbican"

  local cleanup_section
  cleanup_section=$(extract_yaml_job_section "$CI_YAML" "cleanup-e2e-tags")

  assert_contains \
    "cleanup-e2e-tags package matrix lists barbican-operator and the barbican service image" \
    "$cleanup_section" \
    "barbican-operator, barbican,"

  local operator_images_section stale_tags_section
  operator_images_section=$(extract_yaml_job_section "$CLEANUP_YAML" "cleanup-operator-images")
  stale_tags_section=$(extract_yaml_job_section "$CLEANUP_YAML" "cleanup-e2e-stale-tags")

  assert_contains \
    "cleanup-operator-images matrix lists barbican-operator" \
    "$operator_images_section" \
    "barbican-operator"

  assert_contains \
    "cleanup-e2e-stale-tags matrix lists barbican-operator and the barbican service image" \
    "$stale_tags_section" \
    "barbican-operator, barbican,"
}

test_barbican_codecov_flags() {
  echo "Test: .codecov.yml carries the barbican unit and integration flags"

  local unit_block integration_block
  unit_block=$(extract_codecov_flag_block "unit-barbican")
  integration_block=$(extract_codecov_flag_block "integration-barbican")

  assert_contains \
    "the unit-barbican flag scopes to operators/barbican/" \
    "$unit_block" \
    "- operators/barbican/"

  assert_contains \
    "the unit-barbican flag carries coverage forward" \
    "$unit_block" \
    "carryforward: true"

  assert_contains \
    "the integration-barbican flag scopes to operators/barbican/" \
    "$integration_block" \
    "- operators/barbican/"

  assert_contains \
    "the integration-barbican flag carries coverage forward" \
    "$integration_block" \
    "carryforward: true"
}

# ── e2e-operator wiring ─────────────────────────────────────────────────────

test_barbican_secret_store_grants() {
  echo "Test: the e2e-operator deploy step passes BARBICAN_SECRET_STORE_GRANTS"

  # The barbican e2e suites create their managed BarbicanSecretStore in the
  # openstack Namespace. Without a grant for that Namespace the chart renders no
  # TokenRequest Role there, the operator cannot mint the OpenBaoCluster
  # provisioner token, and every store stalls short of Ready. The account half of
  # the pair is what keeps that Role from covering every ServiceAccount in the
  # shared openstack Namespace; the chart refuses to render without it.
  local operator_section deploy_step
  operator_section=$(extract_yaml_job_section "$CI_YAML" "e2e-operator")
  deploy_step=$(extract_yaml_step "$operator_section" "Deploy operator")

  assert_contains \
    "e2e-operator grants openstack the proving instance's provisioner account" \
    "$deploy_step" \
    "BARBICAN_SECRET_STORE_GRANTS: openstack=openbao-instance-provisioner"
}

# ── e2e-chaos wiring ────────────────────────────────────────────────────────

test_barbican_chaos_suites() {
  echo "Test: both e2e-chaos legs run a barbican suite"

  local chaos_section pod_leg network_leg
  chaos_section=$(extract_yaml_job_section "$CI_YAML" "e2e-chaos")
  pod_leg=$(extract_chaos_leg "$chaos_section" "pod")
  network_leg=$(extract_chaos_leg "$chaos_section" "network")

  assert_contains \
    "pod-leg test_dirs include the barbican operator-kill suite" \
    "$pod_leg" \
    "tests/e2e-chaos/barbican-operator-pod-kill"

  assert_contains \
    "network-leg test_dirs include the barbican OpenBao-outage suite" \
    "$network_leg" \
    "tests/e2e-chaos/barbican-openbao-outage"
}

test_barbican_chaos_wiring() {
  echo "Test: e2e-chaos loads the barbican images and deploys the operator on both legs"

  # Scope every needle to the e2e-chaos job: the image names also occur in the
  # build and tempest jobs, so a whole-file grep would still pass with the
  # e2e-chaos image-load and deploy steps deleted — leaving both barbican
  # suites to run against a cluster with no barbican-operator and an empty pod
  # selector.
  local chaos_section pull_step kind_load_step deploy_step
  chaos_section=$(extract_yaml_job_section "$CI_YAML" "e2e-chaos")
  pull_step=$(extract_yaml_step "$chaos_section" "Load E2E images")

  assert_contains \
    "the GHCR pull step lists the barbican-operator image" \
    "$pull_step" \
    "IMAGE_PREFIX }}/barbican-operator:dev"

  assert_contains \
    "the GHCR pull step lists the barbican service image" \
    "$pull_step" \
    "IMAGE_PREFIX }}/barbican:2025.2"

  # Unlike placement, barbican has a suite on each leg, so neither image may be
  # wrapped in a `matrix.suite == 'pod'` format() expression — the network leg
  # would then pull nothing and barbican-openbao-outage would fail on a missing
  # image.
  assert_not_contains \
    "the barbican images are pulled on both legs, not through a leg gate" \
    "$pull_step" \
    "format('{0}/barbican"

  kind_load_step=$(extract_yaml_step "$chaos_section" "Load images into kind")

  assert_contains \
    "the kind-load step loads the barbican-operator image" \
    "$kind_load_step" \
    "IMAGE_PREFIX }}/barbican-operator:dev"

  assert_contains \
    "the kind-load step loads the barbican service image" \
    "$kind_load_step" \
    "IMAGE_PREFIX }}/barbican:2025.2"

  deploy_step=$(extract_yaml_step "$chaos_section" "Deploy barbican operator")

  assert_contains \
    "e2e-chaos deploys the barbican operator" \
    "$deploy_step" \
    "OPERATOR: barbican"

  assert_contains \
    "e2e-chaos deploys the barbican operator into barbican-system" \
    "$deploy_step" \
    "NAMESPACE: barbican-system"

  assert_contains \
    "the chaos deploy grants openstack the provisioner account" \
    "$deploy_step" \
    "BARBICAN_SECRET_STORE_GRANTS: openstack=openbao-instance-provisioner"

  # barbican-operator-pod-kill runs on the pod leg and barbican-openbao-outage
  # on the network leg, so the deploy stays ungated. A leg gate copied from the
  # placement deploy would leave one of the two suites without an operator.
  assert_not_contains \
    "the barbican operator deploy step carries no chaos-leg gate" \
    "$deploy_step" \
    "if:"
}

# ── build-e2e-images wiring ─────────────────────────────────────────────────

test_barbican_build_is_unconditional() {
  echo "Test: build-e2e-images always builds barbican"

  local build_section resolve_step
  build_section=$(extract_yaml_job_section "$CI_YAML" "build-e2e-images")
  resolve_step=$(extract_yaml_step "$build_section" "Resolve build operators")

  # Both e2e-chaos legs consume barbican-operator:dev and barbican:2025.2
  # unconditionally, so barbican joins keystone, glance and placement in the
  # fixed union. Gating it on a hand-copied duplicate of the e2e-chaos `if:`
  # would let the two conditions drift and fail the chaos legs on
  # `manifest unknown` an hour into the run.
  assert_contains \
    "the unconditional build union lists keystone, glance, placement and barbican" \
    "$resolve_step" \
    "for base in keystone glance placement barbican; do"
}

# ── tempest service-dimension leg ───────────────────────────────────────────

test_barbican_tempest_wiring() {
  echo "Test: tempest job carries the barbican service leg"

  assert_file_contains \
    "the tempest matrix generator emits a barbican leg per release" \
    "$TEMPEST_MATRIX_SCRIPT" \
    "for service in keystone glance barbican; do"

  assert_file_contains \
    "tempest bootstraps the key-manager catalog" \
    "$CI_YAML" \
    "job/barbican-tempest-catalog-setup"

  assert_file_contains \
    "tempest waits on the Barbican CR for the barbican leg" \
    "$CI_YAML" \
    "kubectl wait barbican/\${{ matrix.barbican-cr-name }}"

  assert_file_contains \
    "tempest passes BARBICAN_K8S_NAME to the Tempest wrapper" \
    "$CI_YAML" \
    "BARBICAN_K8S_NAME: \${{ matrix.barbican-cr-name }}"
}

# ── ci-resolve-changes.sh documentation ─────────────────────────────────────

test_resolve_script_documents_filter() {
  echo "Test: ci-resolve-changes.sh documents FILTER_barbican"

  assert_file_contains \
    "resolve script documents FILTER_barbican" \
    "$RESOLVE_SCRIPT" \
    "FILTER_barbican"
}

# ── ci-resolve-changes.sh behavioural tests ─────────────────────────────────

test_resolve_emits_barbican_on_operator_change() {
  echo "Test: ci-resolve-changes.sh emits barbican in e2e-operators on a barbican-only change"

  local resolved operators has
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican" \
    GITHUB_REF="refs/heads/main" \
    FILTER_keystone="false" \
    FILTER_c5c3="false" \
    FILTER_horizon="false" \
    FILTER_glance="false" \
    FILTER_placement="false" \
    FILTER_barbican="true" \
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
    "barbican-only change emits barbican in the e2e-operators matrix" \
    "$operators" \
    '"barbican"' # JSON array entry

  assert_eq \
    "barbican-only change emits barbican alone" \
    '{"operator":["barbican"]}' \
    "$operators"

  assert_eq \
    "barbican-only change sets has-e2e-operators=true" \
    "true" \
    "$has"
}

test_resolve_excludes_barbican_on_keystone_only_change() {
  echo "Test: ci-resolve-changes.sh excludes barbican on a keystone-only change"

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
    "keystone-only change excludes barbican" \
    "$operators" \
    '"barbican"'
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
test_barbican_filter_block
echo ""
test_barbican_helm_filter
echo ""
test_barbican_all_operators
echo ""
test_barbican_filter_env_var
echo ""
test_barbican_test_matrices
echo ""
test_barbican_helm_validate_loops
echo ""
test_cleanup_matrices_include_barbican
echo ""
test_barbican_codecov_flags
echo ""
test_barbican_secret_store_grants
echo ""
test_barbican_chaos_suites
echo ""
test_barbican_chaos_wiring
echo ""
test_barbican_build_is_unconditional
echo ""
test_barbican_tempest_wiring
echo ""
test_resolve_script_documents_filter
echo ""
test_resolve_emits_barbican_on_operator_change
echo ""
test_resolve_excludes_barbican_on_keystone_only_change
echo ""
test_resolve_emits_all_on_go_common_change
echo ""
test_resolve_emits_all_on_tag_push
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
