#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the glance operator reaches the e2e-operators matrix, the two Go test
# matrices, the helm filter, the cleanup matrices, the two e2e-chaos legs, the
# ControlPlane leg and the tempest leg in .github/workflows/ci.yaml.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/glance/ are lint-checked
# and never applied to a cluster. SERVICE_OPERATORS is what tells the resolver
# that glance ships an OpenStack service image per release, so an image rebuild
# reaches the glance leg without the operator's Go gates. The image_glance
# filter puts the service image into changed-services, so an edit under
# images/glance/ or patches/glance/ is built instead of pulled from main.
# tests_e2e_glance schedules the leg for an edit that touches only the suites.
# The helm filter is the sole gate on helm-validate, so a pull request touching
# only operators/glance/helm/ renders, lints and unit-tests nothing. An image
# package missing from the cleanup matrices leaks its run-scoped GHCR tags on
# every pull request.
#
# The e2e-chaos legs enumerate their test directories by hand, so a glance chaos
# suite absent from them is lint-checked and never applied to a cluster. Both
# the pod and the network leg run glance suites, so the image loads and the
# operator deploy are gated on everything but the ovn leg; kind pulls nothing
# the run did not load, and a Glance whose operator never deployed sits without
# status until the suite times out. The ControlPlane leg needs the same images
# and its own operator deploy.
#
# The tempest leg is silent in the same way, one job over. The glance images
# are pulled for the cinder and nova legs as well, which create their volumes
# and servers from a Glance image, and loaded into kind for the nova leg too.
# The image catalog Job, the Glance the leg waits on, the K8S_NAME variable that
# gates the port-forward and the service-qualified results artifact are the
# rest: any one of them missing leaves the leg burning its wait on a resource
# that never comes up, or two legs overwriting each other's results.
#
# Usage: bash tests/unit/ci/glance_e2e_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# The real list from the ci.yaml resolve step env block. A shorter one would
# make the matrix scenarios below assert nothing.
ALL_OPS=$(ci_all_operators)

# One resolver run for an operators/glance/ change. The e2e-operators
# and the test-targets checks below both read their line from it.
GLANCE_CHANGE=$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_glance=true)

# The release the e2e-chaos and ControlPlane legs load the glance image at.
DEFAULT_RELEASE="2026.1"

CHART="$PROJECT_ROOT/operators/glance/helm/glance-operator"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


test_all_operators_lists_glance() {
  echo "Test: ci.yaml ALL_OPERATORS includes glance"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists glance" "$line" "glance"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_service_operators_lists_glance() {
  echo "Test: ci.yaml SERVICE_OPERATORS includes glance"

  # glance ships an OpenStack service image for every release that carries a
  # glance key in source-refs.yaml. Missing from this list, the resolver
  # treats it like the orchestration operators: the image is never rebuilt for
  # a pull request that changes it, and the e2e leg loads whatever main last
  # published.
  local line
  line=$(grep "SERVICE_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "SERVICE_OPERATORS lists glance" "$line" "glance"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  assert_not_empty "glance ships a service image per release" \
    "$(OPERATOR=glance "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
}

test_glance_filter_is_wired() {
  echo "Test: the glance paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *glance:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_glance: \${{ steps.filter.outputs.glance }}"

  local block
  block=$(filter_block glance)
  assert_contains "the operator source path is covered" "$block" "operators/glance/**"
}

test_glance_image_filter_is_wired() {
  echo "Test: an images/glance change reaches changed-services"

  # changed-services is the list build-e2e-images rebuilds from source. The
  # lock under nix/locks/ and the patches under patches/glance/ change the
  # image as much as its build context does, so all three belong here.
  assert_filter_is_wired image_glance changed-services

  local block
  block=$(filter_block image_glance)
  assert_contains "the image build context is covered" "$block" "images/glance/**"
  assert_contains "the Nix locks are covered" "$block" "nix/locks/*/glance/**"
  assert_contains "the source patches are covered" "$block" "patches/glance/**"

  assert_eq "an image change rebuilds glance and nothing else" \
    'changed-services=["glance"]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS" \
      FILTER_image_glance=true)"
}

test_glance_change_produces_an_e2e_leg() {
  echo "Test: an operators/glance change puts glance in the e2e-operators matrix"

  local matrix
  matrix=$(grep "^e2e-operators=" <<<"$GLANCE_CHANGE")

  assert_contains "the matrix carries the glance leg" "$matrix" '"glance"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_glance_e2e_filter_is_wired() {
  echo "Test: an edit to the glance suites alone runs the glance leg"

  # The suites sit in two directories and the e2e-operator job runs both, on
  # top of the shared helpers under tests/e2e/lib/. A filter naming fewer lets
  # an edit under the others schedule no leg at all.
  assert_filter_is_wired tests_e2e_glance e2e-operators

  local block
  block=$(filter_block tests_e2e_glance)
  assert_contains "the per-CR suites are covered" "$block" "tests/e2e/glance/**"
  assert_contains "the operator-level suites are covered" "$block" \
    "tests/e2e/glance-operator/**"
  assert_contains "the shared suite helpers are covered" "$block" "tests/e2e/lib/**"

  assert_eq "a suite edit runs the glance leg alone" \
    'e2e-operators={"operator":["glance"]}' \
    "$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
      FILTER_tests_e2e_glance=true)"
}

test_helm_filter_covers_the_glance_chart() {
  echo "Test: a glance chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory under
  # it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the glance chart lives under that glob" "yes" \
    "$([ -d "$CHART" ] && echo yes || echo no)"
}

test_go_matrices_list_glance() {
  echo "Test: the unit and integration test matrices include glance"

  # Both matrices read the resolver's list, and the resolver puts the operator
  # in it when its own code changes. An operator missing from either half
  # compiles and ships with its `go test` leg never run.
  local matrix_count
  matrix_count=$(grep -c \
    'matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}' "$CI_YAML") || true

  assert_eq "both the test and the test-integration matrix read test-targets" \
    "2" "$matrix_count"

  assert_contains "a glance change puts glance in the test matrix" \
    "$(grep "^test-targets=" <<<"$GLANCE_CHANGE")" \
    '"glance"'
}

test_cleanup_matrices_cover_the_glance_images() {
  echo "Test: the derived cleanup package lists cover the glance images"

  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(sed -n 's/^cleanup-packages=//p' <<<"$matrix")
  e2e_packages=$(sed -n 's/^cleanup-e2e-packages=//p' <<<"$matrix")

  assert_contains "the nightly sweep covers glance-operator" \
    "$all_packages" '"glance-operator"'
  assert_contains "the per-run sweep covers glance-operator" \
    "$e2e_packages" '"glance-operator"'
  assert_contains "the nightly sweep covers glance" \
    "$all_packages" '"glance"'
  assert_contains "the per-run sweep covers glance" \
    "$e2e_packages" '"glance"'
}

test_a_keystone_only_change_produces_no_glance_leg() {
  echo "Test: a keystone-only change keeps glance out of the e2e-operators matrix"

  # The positive case above proves the filter reaches the matrix; this one
  # proves it still gates. A filter wired to a constant would satisfy the
  # positive assertion and put every operator on every pull request.
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
    FILTER_keystone=true)

  assert_contains "the matrix carries the keystone leg" "$matrix" '"keystone"'
  assert_not_contains "and no glance leg" "$matrix" '"glance"'
}

test_chaos_legs_run_the_glance_suites() {
  echo "Test: the e2e-chaos pod and network legs run the glance suites"

  local pod network
  pod=$(e2e_chaos_matrix_entry pod)
  network=$(e2e_chaos_matrix_entry network)
  assert_not_empty "the pod leg exists" "$pod"
  assert_contains "the pod leg runs the operator pod-kill suite" "$pod" \
    "tests/e2e-chaos/glance-operator-pod-kill"
  assert_contains "the pod leg runs the Keystone outage suite" "$pod" \
    "tests/e2e-chaos/glance-keystone-outage"
  assert_not_empty "the network leg exists" "$network"
  assert_contains "the network leg runs the Garage outage suite" "$network" \
    "tests/e2e-chaos/glance-garage-outage"
  assert_contains "the network leg runs the MariaDB outage suite" "$network" \
    "tests/e2e-chaos/glance-mariadb-outage"

  local pull kind_load
  pull=$(job_step e2e-chaos "Load E2E images (keystone stack)")
  assert_contains "the keystone-stack pull pulls the glance-operator image" \
    "$pull" "glance-operator:dev"
  assert_contains "it pulls the glance service image" "$pull" "glance:${DEFAULT_RELEASE}"
  kind_load=$(job_step e2e-chaos "Load images into kind")
  assert_contains "the operator image reaches the node" "$kind_load" \
    "glance-operator:dev"
  assert_contains "the service image reaches the node" "$kind_load" \
    "glance:${DEFAULT_RELEASE}"

  local deploy
  deploy=$(job_step e2e-chaos "Deploy glance operator")
  assert_not_empty "the glance-operator is deployed" "$deploy"
  assert_contains "the deploy skips the ovn leg alone" "$deploy" \
    "if: matrix.suite != 'ovn'"
  assert_contains "it deploys the glance operator" "$deploy" "OPERATOR: glance"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: glance-system"
}

test_controlplane_leg_deploys_glance() {
  echo "Test: the e2e-controlplane leg loads and deploys glance"

  local pull kind_load deploy
  pull=$(job_step e2e-controlplane "Load E2E images")
  assert_contains "the leg pulls the glance-operator image" "$pull" \
    "glance-operator:dev"
  assert_contains "the leg pulls the glance service image" "$pull" \
    "glance:${DEFAULT_RELEASE}"

  kind_load=$(job_step e2e-controlplane "Load images into kind")
  assert_contains "the operator image reaches the node" "$kind_load" \
    "glance-operator:dev"
  assert_contains "the service image reaches the node" "$kind_load" \
    "glance:${DEFAULT_RELEASE}"

  deploy=$(job_step e2e-controlplane "Deploy glance-operator")
  assert_not_empty "the glance-operator is deployed" "$deploy"
  assert_contains "it deploys the glance operator" "$deploy" "OPERATOR: glance"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: glance-system"
}

test_tempest_glance_leg_is_wired() {
  echo "Test: the tempest glance leg is brought up"

  # The cinder and nova legs create volumes and servers from a Glance image, so
  # they pull the glance images and deploy the operator too. The cinder leg
  # loads the images into kind in a step of its own.
  local pull_gate="if: matrix.service == 'glance' || matrix.service == 'cinder' || matrix.service == 'nova'"
  local step

  step=$(job_step tempest "Load Glance E2E images")
  assert_contains "the pull runs on the glance, cinder and nova legs" "$step" \
    "$pull_gate"
  assert_contains "it pulls the glance-operator image" "$step" "glance-operator:dev"
  assert_contains "it pulls the glance service image of the leg's release" "$step" \
    "glance:\${{ matrix.release }}"

  step=$(job_step tempest "Load Glance images into kind")
  assert_contains "the kind load runs on the glance and nova legs" "$step" \
    "if: matrix.service == 'glance' || matrix.service == 'nova'"
  assert_contains "the operator image reaches the node" "$step" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/glance-operator:dev"
  assert_contains "the service image reaches the node" "$step" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/glance:\${{ matrix.release }}"
  assert_contains "the tempest image reaches the node" "$step" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/tempest:\${{ matrix.release }}"

  assert_contains "the operator is deployed on the glance, cinder and nova legs" \
    "$(job_step tempest "Deploy glance operator")" "$pull_gate"

  step=$(job_step tempest "Bootstrap image catalog")
  assert_contains "the image catalog runs on the glance leg" "$step" \
    "if: matrix.service == 'glance'"
  assert_contains "the image catalog Job is awaited" "$step" \
    "job/glance-tempest-catalog-setup"

  assert_contains "the leg waits on the Glance" \
    "$(job_step tempest "Deploy Glance CR for Tempest")" \
    "kubectl wait glance/\${{ matrix.glance-cr-name }}"

  assert_contains "the run step names the Glance to port-forward" \
    "$(job_step tempest "Run Tempest API tests")" \
    "GLANCE_K8S_NAME: \${{ matrix.glance-cr-name }}"

  assert_contains "the results artifact is named per service and release" \
    "$(job_step tempest "Upload Tempest results")" \
    "name: tempest-\${{ matrix.service }}-\${{ matrix.release }}-results"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_glance
test_service_operators_lists_glance
test_glance_filter_is_wired
test_glance_image_filter_is_wired
test_glance_change_produces_an_e2e_leg
test_glance_e2e_filter_is_wired
test_helm_filter_covers_the_glance_chart
test_go_matrices_list_glance
test_cleanup_matrices_cover_the_glance_images
test_a_keystone_only_change_produces_no_glance_leg
test_chaos_legs_run_the_glance_suites
test_controlplane_leg_deploys_glance
test_tempest_glance_leg_is_wired

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
