#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify barbican operator CI pipeline wiring meets requirements.
# Validates: barbican paths-filter block, the helm filter entry,
# FILTER_barbican env, ALL_OPERATORS membership, test/helm-validate/cleanup
# matrices, the codecov flags, the BARBICAN_SECRET_STORE_GRANTS passthrough
# on the e2e-operator deploy step, both e2e-chaos legs with their ungated image
# load and operator deploy, the e2e-controlplane filter entry with its image
# load and grant-free operator deploy, the barbican entry in the
# build-e2e-images operator union, the tempest service leg, and that
# ci-resolve-changes.sh emits barbican in the e2e-operators matrix once
# barbican is a known operator.
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
  # SERVICE_OPERATORS and CANARY_OPERATOR are required by the resolve script and
  # supplied here as defaults, so a caller that only sets ALL_OPERATORS and its
  # FILTER_ vars keeps working. A caller that sets either one wins.
  SERVICE_OPERATORS="${SERVICE_OPERATORS:-keystone horizon glance placement barbican neutron}" \
    CANARY_OPERATOR="${CANARY_OPERATOR:-keystone}" \
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

  # The shared Dockerfile builds every operator binary, so it belongs to
  # go_common; listing it per operator made a change to it look like a change to
  # each of them in turn.
  assert_not_contains \
    "barbican filter does not carry the shared operator Dockerfile" \
    "$filter_block" \
    "operators/Dockerfile"

  assert_file_contains \
    "ci.yaml declares an image_barbican filter" \
    "$CI_YAML" \
    "^            image_barbican:"

  # The service image has a filter of its own: rebuilding it runs the barbican
  # e2e leg without pulling the operator's Go gates in with it.
  local image_block
  image_block=$(extract_paths_filter_block "$CI_YAML" "image_barbican")

  assert_contains \
    "image_barbican filter includes images/barbican/**" \
    "$image_block" \
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

  # Both matrices are resolved per pull request rather than hardcoded, so the
  # assertion is in two parts: the jobs read the resolver's list, and the
  # resolver puts this operator in it when its own code changes.
  local matrix_count
  matrix_count=$(grep -c 'matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}' "$CI_YAML") || true

  assert_eq \
    "both test and test-integration matrices read test-targets" \
    "2" \
    "$matrix_count"

  local resolved
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican ovn neutron" \
    GITHUB_REF="refs/heads/main" \
    FILTER_barbican="true" \
    run_resolve
  )

  assert_contains \
    "a barbican change puts barbican in the test matrix" \
    "$(output_value "$resolved" "test-targets")" \
    '"barbican"'
}

test_barbican_helm_validate_loops() {
  echo "Test: helm-validate loops include the barbican-operator chart"

  local loop_count
  # `|| true`: grep exits 1 on no match, which under `set -e` killed the whole
  # run here and hid every assertion below this point.
  loop_count=$(grep -c " operators/barbican/helm/barbican-operator" "$CI_YAML") || true

  assert_eq \
    "all three helm-validate loops list the barbican-operator chart" \
    "3" \
    "$loop_count"
}

test_cleanup_matrices_include_barbican() {
  echo "Test: the derived cleanup package lists cover barbican"

  # Both cleanup-images.yaml and ci.yaml's cleanup-e2e-tags build their package
  # matrix from this script, so coverage is a property of its output rather than
  # of a list someone has to remember to extend.
  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(echo "$matrix" | sed -n 's/^cleanup-packages=//p')
  e2e_packages=$(echo "$matrix" | sed -n 's/^cleanup-e2e-packages=//p')

  assert_contains \
    "the nightly cleanup covers barbican-operator" \
    "$all_packages" \
    '"barbican-operator"'

  assert_contains \
    "the per-run e2e cleanup covers barbican-operator" \
    "$e2e_packages" \
    '"barbican-operator"'

  assert_contains \
    "the nightly cleanup covers barbican" \
    "$all_packages" \
    '"barbican"'

  assert_contains \
    "the per-run e2e cleanup covers barbican" \
    "$e2e_packages" \
    '"barbican"'

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

# ── e2e-controlplane wiring ─────────────────────────────────────────────────

test_barbican_controlplane_filter() {
  echo "Test: a barbican change reaches the ControlPlane jobs by label, not by default"

  # The full-chain suite deploys the barbican-operator, but the three
  # ControlPlane jobs take up to 195 minutes each and running them for every
  # service-operator change is what the change classes exist to stop. A barbican
  # change proves itself on the barbican e2e leg; ci:controlplane and ci:full
  # ask for the chain.
  local resolved
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican ovn neutron" \
    GITHUB_REF="refs/heads/main" \
    FILTER_barbican="true" \
    run_resolve
  )

  assert_eq \
    "a barbican-only change does not schedule the ControlPlane chain" \
    "false" \
    "$(output_value "$resolved" "e2e-controlplane")"

  assert_contains \
    "a barbican-only change still runs the barbican e2e leg" \
    "$(output_value "$resolved" "e2e-operators")" \
    '"barbican"'

  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican ovn neutron" \
    GITHUB_REF="refs/heads/main" \
    FILTER_barbican="true" \
    PR_LABELS='["ci:controlplane"]' \
    run_resolve
  )

  assert_eq \
    "the ci:controlplane label schedules the chain" \
    "true" \
    "$(output_value "$resolved" "e2e-controlplane")"

  # And when it runs, the barbican operator image has to be in the build set or
  # the chain fails at Load E2E images with a tag that was never pushed.
  assert_contains \
    "the chain builds the barbican operator image" \
    "$(output_value "$resolved" "build-operators")" \
    '"barbican"'
}

test_barbican_controlplane_wiring() {
  echo "Test: e2e-controlplane loads the barbican images and deploys the operator"

  # Scope every needle to the e2e-controlplane job for the reason the e2e-chaos
  # test gives: the same image names occur in the build, chaos and tempest jobs.
  local cp_section pull_step kind_load_step deploy_step
  cp_section=$(extract_yaml_job_section "$CI_YAML" "e2e-controlplane")
  pull_step=$(extract_yaml_step "$cp_section" "Load E2E images")

  assert_contains \
    "the GHCR pull step lists the barbican-operator image" \
    "$pull_step" \
    "IMAGE_PREFIX }}/barbican-operator:dev"

  assert_contains \
    "the GHCR pull step lists the barbican service image" \
    "$pull_step" \
    "IMAGE_PREFIX }}/barbican:2025.2"

  kind_load_step=$(extract_yaml_step "$cp_section" "Load images into kind")

  assert_contains \
    "the kind-load step loads the barbican-operator image" \
    "$kind_load_step" \
    "IMAGE_PREFIX }}/barbican-operator:dev"

  assert_contains \
    "the kind-load step loads the barbican service image" \
    "$kind_load_step" \
    "IMAGE_PREFIX }}/barbican:2025.2"

  deploy_step=$(extract_yaml_step "$cp_section" "Deploy barbican-operator")

  assert_contains \
    "e2e-controlplane deploys the barbican operator" \
    "$deploy_step" \
    "OPERATOR: barbican"

  assert_contains \
    "e2e-controlplane deploys the barbican operator into barbican-system" \
    "$deploy_step" \
    "NAMESPACE: barbican-system"

  # This job's store hangs off the OpenBaoCluster the ControlPlane creates, and
  # the ControlPlane projects the TokenRequest Role and RoleBinding for that
  # instance's own provisioner account. A chart-level grant copied from the
  # e2e-operator or e2e-chaos deploy would name the proving instance's account
  # instead, so it would neither help the store nor stay scoped.
  assert_not_contains \
    "the controlplane deploy sets no static secret-store grant" \
    "$deploy_step" \
    "BARBICAN_SECRET_STORE_GRANTS"
}

# ── build-e2e-images wiring ─────────────────────────────────────────────────

test_barbican_build_is_resolved() {
  echo "Test: build-e2e-images builds barbican whenever a chaos leg runs"

  local build_section resolve_step
  build_section=$(extract_yaml_job_section "$CI_YAML" "build-e2e-images")
  resolve_step=$(extract_yaml_step "$build_section" "Resolve build operators")

  assert_contains \
    "the step takes its list from the resolver" \
    "$resolve_step" \
    "needs.changes.outputs.build-operators"

  # Both e2e-chaos legs consume barbican-operator:dev and barbican:2025.2, so
  # the resolver has to put barbican in the build set whenever chaos is
  # scheduled, and again for the two-cluster job that places a secret store.
  local resolved
  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican ovn neutron" \
    GITHUB_REF="refs/heads/main" \
    FILTER_tests_chaos="true" \
    run_resolve
  )

  assert_contains \
    "a chaos suite change builds barbican" \
    "$(output_value "$resolved" "build-operators")" \
    '"barbican"'

  resolved=$(
    ALL_OPERATORS="keystone c5c3 horizon glance placement barbican ovn neutron" \
    GITHUB_REF="refs/heads/main" \
    FILTER_tests_multicluster="true" \
    run_resolve
  )

  assert_contains \
    "a two-cluster suite change builds barbican" \
    "$(output_value "$resolved" "build-operators")" \
    '"barbican"'
}

# ── tempest service-dimension leg ───────────────────────────────────────────

test_barbican_tempest_wiring() {
  echo "Test: tempest job carries the barbican service leg"

  assert_file_contains \
    "the tempest matrix generator knows the barbican service" \
    "$TEMPEST_MATRIX_SCRIPT" \
    "ALL_TEMPEST_SERVICES=(keystone glance barbican)"

  # The matrix is narrowed per pull request, so the leg has to survive both the
  # unnarrowed case and a selection that names it.
  local out
  out=$(mktemp)
  GITHUB_OUTPUT="$out" TEMPEST_SERVICES="barbican" \
    bash "$TEMPEST_MATRIX_SCRIPT" >/dev/null 2>&1
  assert_file_contains \
    "selecting barbican emits a barbican leg" \
    "$out" \
    '"service":"barbican"'
  rm -f "$out"

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
  echo "Test: FILTER_barbican reaches the resolve script"

  # The resolver reads the per-operator filters through ALL_OPERATORS rather
  # than naming each one, so asserting the literal FILTER_barbican against its
  # source would pass on any incidental mention. What has to hold is the wiring
  # in ci.yaml and the onboarding procedure in the resolver's header.
  assert_file_contains \
    "ci.yaml passes FILTER_barbican to the resolve step" \
    "$CI_YAML" \
    "FILTER_barbican: \${{ steps.filter.outputs.barbican }}"

  assert_file_contains \
    "ci.yaml lists barbican in ALL_OPERATORS" \
    "$CI_YAML" \
    "ALL_OPERATORS: .*barbican"

  assert_file_contains \
    "the resolve script documents how an operator is added" \
    "$RESOLVE_SCRIPT" \
    "To add a new operator"
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
test_barbican_controlplane_filter
echo ""
test_barbican_controlplane_wiring
echo ""
test_barbican_build_is_resolved
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
