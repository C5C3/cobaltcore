#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify placement operator CI pipeline wiring meets requirements.
# Validates: placement paths-filter block, the helm filter entry,
# FILTER_placement env, ALL_OPERATORS membership, test/helm-validate/cleanup
# matrices, the e2e-chaos pod-kill leg and its pod-leg-only image load/deploy,
# the placement images reaching that leg through the build-e2e-images image map,
# and that ci-resolve-changes.sh emits placement in the e2e-operators matrix once
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

  # The shared Dockerfile builds every operator binary, so it belongs to
  # go_common; listing it per operator made a change to it look like a change to
  # each of them in turn.
  assert_not_contains \
    "placement filter does not carry the shared operator Dockerfile" \
    "$filter_block" \
    "operators/Dockerfile"

  assert_file_contains \
    "ci.yaml declares an image_placement filter" \
    "$CI_YAML" \
    "^            image_placement:"

  # The service image has a filter of its own: rebuilding it runs the placement
  # e2e leg without pulling the operator's Go gates in with it.
  local image_block
  image_block=$(extract_paths_filter_block "$CI_YAML" "image_placement")

  assert_contains \
    "image_placement filter includes images/placement/**" \
    "$image_block" \
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
    FILTER_placement="true" \
    run_resolve
  )

  assert_contains \
    "a placement change puts placement in the test matrix" \
    "$(output_value "$resolved" "test-targets")" \
    '"placement"'
}

test_placement_helm_validate_loops() {
  echo "Test: helm-validate loops include the placement-operator chart"

  local loop_count
  # helm-validate iterates the operators/*/helm/*-operator glob in its three
  # loops; the chart is covered by living in that layout.
  loop_count=$(grep -cF 'for chart in operators/*/helm/*-operator' "$CI_YAML")
  [ -d "operators/placement/helm/placement-operator" ] || loop_count=0

  if [ "$loop_count" -ge 3 ]; then
    echo "  PASS: helm-validate references the placement-operator chart in $loop_count loops"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: expected >=3 helm-validate references to the placement-operator chart, found $loop_count"
    FAIL=$((FAIL + 1))
  fi
}

test_cleanup_matrices_include_placement() {
  echo "Test: the derived cleanup package lists cover placement"

  # Both cleanup-images.yaml and ci.yaml's cleanup-e2e-tags build their package
  # matrix from this script, so coverage is a property of its output rather than
  # of a list someone has to remember to extend.
  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(echo "$matrix" | sed -n 's/^cleanup-packages=//p')
  e2e_packages=$(echo "$matrix" | sed -n 's/^cleanup-e2e-packages=//p')

  assert_contains \
    "the nightly cleanup covers placement-operator" \
    "$all_packages" \
    '"placement-operator"'

  assert_contains \
    "the per-run e2e cleanup covers placement-operator" \
    "$e2e_packages" \
    '"placement-operator"'

  assert_contains \
    "the nightly cleanup covers placement" \
    "$all_packages" \
    '"placement"'

  assert_contains \
    "the per-run e2e cleanup covers placement" \
    "$e2e_packages" \
    '"placement"'

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

test_placement_build_uses_the_image_map() {
  echo "Test: the placement images reach the chaos pod leg through the image map"

  local build_section resolve_step
  build_section=$(extract_yaml_job_section "$CI_YAML" "build-e2e-images")
  resolve_step=$(extract_yaml_step "$build_section" "Resolve images")

  # The build set used to be a union spelled out in this step, then one the
  # resolver computed from the jobs a run scheduled. The job now builds only
  # what changed and resolves the rest to the digests main published, so the
  # step runs the script that decides both.
  assert_contains \
    "the build job resolves its images through the script" \
    "$resolve_step" \
    "hack/ci-resolve-e2e-images.sh"

  assert_not_contains \
    "the step no longer carries a hardcoded union" \
    "$resolve_step" \
    "for base in"

  # The e2e-chaos pod leg consumes placement-operator:dev and placement:2025.2.
  # The job builds neither unless placement changed, so the leg reads them from
  # the map. Missing the map input, it falls back to a run-scoped tag nothing
  # pushed and the blocking leg fails on `manifest unknown` an hour into the run.
  local chaos_section
  chaos_section=$(extract_yaml_job_section "$CI_YAML" "e2e-chaos")

  assert_contains \
    "the chaos image load takes the map from the build job" \
    "$(extract_yaml_step "$chaos_section" "Load E2E images")" \
    'image-map: ${{ needs.build-e2e-images.outputs.image-map }}'
}

# ── ci-resolve-changes.sh documentation ─────────────────────────────────────

test_resolve_script_documents_filter() {
  echo "Test: FILTER_placement reaches the resolve script"

  # The resolver reads the per-operator filters through ALL_OPERATORS rather
  # than naming each one, so asserting the literal FILTER_placement against its
  # source would pass on any incidental mention. What has to hold is the wiring
  # in ci.yaml and the onboarding procedure in the resolver's header.
  assert_file_contains \
    "ci.yaml passes FILTER_placement to the resolve step" \
    "$CI_YAML" \
    "FILTER_placement: \${{ steps.filter.outputs.placement }}"

  assert_file_contains \
    "ci.yaml lists placement in ALL_OPERATORS" \
    "$CI_YAML" \
    "ALL_OPERATORS: .*placement"

  assert_file_contains \
    "the resolve script documents how an operator is added" \
    "$RESOLVE_SCRIPT" \
    "To add a new operator"
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
test_placement_build_uses_the_image_map
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
