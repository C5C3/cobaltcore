#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the horizon operator reaches the e2e-operators matrix, the two Go test
# matrices, the helm filter, the cleanup matrices, the e2e-chaos legs, the
# ControlPlane leg and the e2e-prometheus leg in .github/workflows/ci.yaml.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/horizon/ are
# lint-checked and never applied to a cluster. SERVICE_OPERATORS is what tells
# the resolver that horizon ships an OpenStack service image per release, so an
# image rebuild reaches the horizon leg without the operator's Go gates. The
# image_horizon filter puts the service image into changed-services, so an edit
# under images/horizon/ or patches/horizon/ is built instead of pulled from
# main. tests_e2e_horizon schedules the leg for an edit that touches only the
# suites. The helm filter is the sole gate on helm-validate, so a pull request
# touching only operators/horizon/helm/ renders, lints and unit-tests nothing.
# An image package missing from the cleanup matrices leaks its run-scoped GHCR
# tags on every pull request.
#
# Horizon has no tempest leg. tests/unit/docs/chaos_suite_inventory_test.sh
# keeps each of its three chaos suites in exactly one e2e-chaos entry, and this
# file pins what those suites run on: the horizon images on the node and an
# operator in horizon-system. horizon-operator-pod-kill selects the operator pod
# in that Namespace, so an operator deployed into the keystone-system default
# leaves the PodChaos without a target. The ControlPlane leg needs the same
# images and its own operator deploy. The e2e-prometheus leg deploys the
# horizon-operator with its ServiceMonitor into horizon-system, where
# tests/e2e/horizon/prometheus-stack reads it.
#
# Usage: bash tests/unit/ci/horizon_e2e_matrix_test.sh

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

# One resolver run for an operators/horizon/ change. The e2e-operators
# and the test-targets checks below both read their line from it.
HORIZON_CHANGE=$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_horizon=true)

# The release the e2e-chaos and ControlPlane legs load the horizon image at.
DEFAULT_RELEASE="2026.1"

CHART="$PROJECT_ROOT/operators/horizon/helm/horizon-operator"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


test_all_operators_lists_horizon() {
  echo "Test: ci.yaml ALL_OPERATORS includes horizon"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists horizon" "$line" "horizon"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_service_operators_lists_horizon() {
  echo "Test: ci.yaml SERVICE_OPERATORS includes horizon"

  # horizon ships an OpenStack service image for every release that carries a
  # horizon key in source-refs.yaml. Missing from this list, the resolver
  # treats it like the orchestration operators: the image is never rebuilt for
  # a pull request that changes it, and the e2e leg loads whatever main last
  # published.
  local line
  line=$(grep "SERVICE_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "SERVICE_OPERATORS lists horizon" "$line" "horizon"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  assert_not_empty "horizon ships a service image per release" \
    "$(OPERATOR=horizon "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
}

test_horizon_filter_is_wired() {
  echo "Test: the horizon paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *horizon:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_horizon: \${{ steps.filter.outputs.horizon }}"

  local block
  block=$(filter_block horizon)
  assert_contains "the operator source path is covered" "$block" "operators/horizon/**"
}

test_horizon_image_filter_is_wired() {
  echo "Test: an images/horizon change reaches changed-services"

  # changed-services is the list build-e2e-images rebuilds from source. The
  # lock under nix/locks/ and the patches under patches/horizon/ change the
  # image as much as its build context does, so all three belong here.
  assert_filter_is_wired image_horizon changed-services

  local block
  block=$(filter_block image_horizon)
  assert_contains "the image build context is covered" "$block" "images/horizon/**"
  assert_contains "the Nix locks are covered" "$block" "nix/locks/*/horizon/**"
  assert_contains "the source patches are covered" "$block" "patches/horizon/**"

  assert_eq "an image change rebuilds horizon and nothing else" \
    'changed-services=["horizon"]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS" \
      FILTER_image_horizon=true)"
}

test_horizon_change_produces_an_e2e_leg() {
  echo "Test: an operators/horizon change puts horizon in the e2e-operators matrix"

  local matrix
  matrix=$(grep "^e2e-operators=" <<<"$HORIZON_CHANGE")

  assert_contains "the matrix carries the horizon leg" "$matrix" '"horizon"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_horizon_e2e_filter_is_wired() {
  echo "Test: an edit to the horizon suites alone runs the horizon leg"

  # The suites sit in two directories and the e2e-operator job runs both, on
  # top of the shared helpers under tests/e2e/lib/. A filter naming fewer lets
  # an edit under the others schedule no leg at all.
  assert_filter_is_wired tests_e2e_horizon e2e-operators

  local block
  block=$(filter_block tests_e2e_horizon)
  assert_contains "the per-CR suites are covered" "$block" "tests/e2e/horizon/**"
  assert_contains "the operator-level suites are covered" "$block" \
    "tests/e2e/horizon-operator/**"
  assert_contains "the shared suite helpers are covered" "$block" "tests/e2e/lib/**"

  assert_eq "a suite edit runs the horizon leg alone" \
    'e2e-operators={"operator":["horizon"]}' \
    "$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
      FILTER_tests_e2e_horizon=true)"
}

test_helm_filter_covers_the_horizon_chart() {
  echo "Test: a horizon chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory under
  # it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the horizon chart lives under that glob" "yes" \
    "$([ -d "$CHART" ] && echo yes || echo no)"
}

test_go_matrices_list_horizon() {
  echo "Test: the unit and integration test matrices include horizon"

  # Both matrices read the resolver's list, and the resolver puts the operator
  # in it when its own code changes. An operator missing from either half
  # compiles and ships with its `go test` leg never run.
  local matrix_count
  matrix_count=$(grep -c \
    'matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}' "$CI_YAML") || true

  assert_eq "both the test and the test-integration matrix read test-targets" \
    "2" "$matrix_count"

  assert_contains "a horizon change puts horizon in the test matrix" \
    "$(grep "^test-targets=" <<<"$HORIZON_CHANGE")" \
    '"horizon"'
}

test_cleanup_matrices_cover_the_horizon_images() {
  echo "Test: the derived cleanup package lists cover the horizon images"

  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(sed -n 's/^cleanup-packages=//p' <<<"$matrix")
  e2e_packages=$(sed -n 's/^cleanup-e2e-packages=//p' <<<"$matrix")

  assert_contains "the nightly sweep covers horizon-operator" \
    "$all_packages" '"horizon-operator"'
  assert_contains "the per-run sweep covers horizon-operator" \
    "$e2e_packages" '"horizon-operator"'
  assert_contains "the nightly sweep covers horizon" \
    "$all_packages" '"horizon"'
  assert_contains "the per-run sweep covers horizon" \
    "$e2e_packages" '"horizon"'
}

test_a_keystone_only_change_produces_no_horizon_leg() {
  echo "Test: a keystone-only change keeps horizon out of the e2e-operators matrix"

  # The positive case above proves the filter reaches the matrix; this one
  # proves it still gates. A filter wired to a constant would satisfy the
  # positive assertion and put every operator on every pull request.
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
    FILTER_keystone=true)

  assert_contains "the matrix carries the keystone leg" "$matrix" '"keystone"'
  assert_not_contains "and no horizon leg" "$matrix" '"horizon"'
}

test_chaos_legs_deploy_horizon() {
  echo "Test: the e2e-chaos legs load and deploy horizon"

  local pull kind_load deploy
  pull=$(job_step e2e-chaos "Load E2E images (keystone stack)")
  assert_contains "the keystone-stack pull skips the ovn leg alone" "$pull" \
    "if: matrix.suite != 'ovn'"
  assert_contains "it pulls the horizon-operator image" "$pull" \
    "\${{ env.IMAGE_PREFIX }}/horizon-operator:dev"
  assert_contains "it pulls the horizon service image" "$pull" \
    "\${{ env.IMAGE_PREFIX }}/horizon:${DEFAULT_RELEASE}"

  kind_load=$(job_step e2e-chaos "Load images into kind")
  assert_contains "the operator image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/horizon-operator:dev"
  assert_contains "the service image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/horizon:${DEFAULT_RELEASE}"

  deploy=$(job_step e2e-chaos "Deploy horizon operator")
  assert_not_empty "the horizon-operator is deployed" "$deploy"
  assert_contains "the deploy skips the ovn leg alone" "$deploy" \
    "if: matrix.suite != 'ovn'"
  assert_contains "it deploys the horizon operator" "$deploy" "OPERATOR: horizon"
  assert_contains "it lands where the PodChaos looks" "$deploy" \
    "NAMESPACE: horizon-system"
}

test_controlplane_leg_deploys_horizon() {
  echo "Test: the e2e-controlplane leg loads and deploys horizon"

  local pull kind_load deploy
  pull=$(job_step e2e-controlplane "Load E2E images")
  assert_contains "the leg pulls the horizon-operator image" "$pull" \
    "horizon-operator:dev"
  assert_contains "the leg pulls the horizon service image" "$pull" \
    "horizon:${DEFAULT_RELEASE}"

  kind_load=$(job_step e2e-controlplane "Load images into kind")
  assert_contains "the operator image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/horizon-operator:dev"
  assert_contains "the service image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/horizon:${DEFAULT_RELEASE}"

  deploy=$(job_step e2e-controlplane "Deploy horizon-operator")
  assert_not_empty "the horizon-operator is deployed" "$deploy"
  assert_contains "it deploys the horizon operator" "$deploy" "OPERATOR: horizon"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: horizon-system"
}

test_prometheus_leg_scrapes_horizon() {
  echo "Test: the e2e-prometheus leg deploys and scrapes horizon"

  local pull kind_load deploy run dump block
  pull=$(job_step e2e-prometheus "Load E2E images")
  assert_contains "the leg pulls the horizon-operator image" "$pull" \
    "\${{ env.IMAGE_PREFIX }}/horizon-operator:dev"

  kind_load=$(job_step e2e-prometheus "Load images into kind")
  assert_contains "the operator image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/horizon-operator:dev"

  deploy=$(job_step e2e-prometheus "Deploy horizon-operator")
  assert_not_empty "the horizon-operator is deployed" "$deploy"
  assert_contains "it deploys the horizon operator" "$deploy" "OPERATOR: horizon"
  assert_contains "it lands where the suite reads its ServiceMonitor" "$deploy" \
    "NAMESPACE: horizon-system"
  assert_contains "it renders the ServiceMonitor" "$deploy" \
    'WITH_PROMETHEUS: "true"'

  run=$(job_step e2e-prometheus "Run prometheus E2E tests")
  assert_contains "the leg runs the keystone suite" "$run" \
    "tests/e2e/keystone/prometheus-stack/"
  assert_contains "the leg runs the horizon suite" "$run" \
    "tests/e2e/horizon/prometheus-stack/"

  dump=$(job_step e2e-prometheus "Dump diagnostic info")
  assert_contains "the dump reads horizon-system too" "$dump" \
    "OPERATOR=horizon OPERATOR_ONLY=1 hack/ci-dump-diagnostics.sh"

  block=$(filter_block tests_prometheus)
  assert_contains "tests_prometheus names the horizon suite" "$block" \
    "'tests/e2e/horizon/prometheus-stack/**'"
  assert_contains "tests_prometheus names the horizon ServiceMonitor template" "$block" \
    "'operators/horizon/helm/horizon-operator/templates/servicemonitor.yaml'"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_horizon
test_service_operators_lists_horizon
test_horizon_filter_is_wired
test_horizon_image_filter_is_wired
test_horizon_change_produces_an_e2e_leg
test_horizon_e2e_filter_is_wired
test_helm_filter_covers_the_horizon_chart
test_go_matrices_list_horizon
test_cleanup_matrices_cover_the_horizon_images
test_a_keystone_only_change_produces_no_horizon_leg
test_chaos_legs_deploy_horizon
test_controlplane_leg_deploys_horizon
test_prometheus_leg_scrapes_horizon

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
