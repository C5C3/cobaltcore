#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the placement operator reaches the e2e-operators matrix, the two Go
# test matrices, the helm filter, the cleanup matrices and the e2e-chaos pod leg
# in .github/workflows/ci.yaml.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/placement/ are
# lint-checked and never applied to a cluster. SERVICE_OPERATORS is what tells
# the resolver that placement ships an OpenStack service image per release, so
# an image rebuild reaches the placement leg without the operator's Go gates.
# The image_placement filter puts the service image into changed-services, so
# an edit under images/placement/ or patches/placement/ is built instead of
# pulled from main. tests_e2e_placement schedules the leg for an edit that
# touches only the suites. The helm filter is the sole gate on helm-validate, so
# a pull request touching only operators/placement/helm/ renders, lints and
# unit-tests nothing. An image package missing from the cleanup matrices leaks
# its run-scoped GHCR tags on every pull request.
#
# The e2e-chaos legs enumerate their test directories by hand, so a placement
# chaos suite absent from the pod entry is lint-checked and never applied to a
# cluster. The image loads, the operator deploy and the condition that runs them
# on the pod and network legs are pinned by
# tests/unit/ci/chaos_workload.bats, which reads the real if: line, so this
# file asserts the pod entry's suites alone. Placement has no tempest leg
# (hack/ci-generate-tempest-matrix.sh emits none), so nothing is asserted there.
#
# Usage: bin/bats tests/unit/ci/placement_e2e_matrix.bats

load '../../lib/bats/common'

CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

setup_file() {
  # The real list from the ci.yaml resolve step env block. A shorter one would
  # make the matrix scenarios below assert nothing.
  ALL_OPS=$(ci_all_operators)

  # One resolver run for an operators/placement/ change. The e2e-operators
  # and the test-targets checks below both read their line from it.
  PLACEMENT_CHANGE=$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_placement=true)
  export ALL_OPS PLACEMENT_CHANGE
}

CHART="$PROJECT_ROOT/operators/placement/helm/placement-operator"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


@test "ci.yaml ALL_OPERATORS includes placement" {
  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists placement" "$line" "placement"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

@test "ci.yaml SERVICE_OPERATORS includes placement" {
  # placement ships an OpenStack service image for every release that carries a
  # placement key in source-refs.yaml. Missing from this list, the resolver
  # treats it like the orchestration operators: the image is never rebuilt for
  # a pull request that changes it, and the e2e leg loads whatever main last
  # published.
  local line
  line=$(grep "SERVICE_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "SERVICE_OPERATORS lists placement" "$line" "placement"

  require_tool yq
  assert_not_empty "placement ships a service image per release" \
    "$(OPERATOR=placement "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
}

@test "the placement paths filter reaches the resolve step" {
  assert_file_matches "the paths filter exists" "$CI_YAML" "^ *placement:$"
  assert_file_matches "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_placement: \${{ steps.filter.outputs.placement }}"

  local block
  block=$(filter_block placement)
  assert_contains "the operator source path is covered" "$block" "operators/placement/**"
}

@test "an images/placement change reaches changed-services" {
  # changed-services is the list build-e2e-images rebuilds from source. The
  # lock under nix/locks/ and the patches under patches/placement/ change the
  # image as much as its build context does, so all three belong here.
  assert_filter_is_wired image_placement changed-services

  local block
  block=$(filter_block image_placement)
  assert_contains "the image build context is covered" "$block" "images/placement/**"
  assert_contains "the Nix locks are covered" "$block" "nix/locks/*/placement/**"
  assert_contains "the source patches are covered" "$block" "patches/placement/**"

  assert_eq "an image change rebuilds placement and nothing else" \
    'changed-services=["placement"]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS" \
      FILTER_image_placement=true)"
}

@test "an operators/placement change puts placement in the e2e-operators matrix" {
  local matrix
  matrix=$(grep "^e2e-operators=" <<<"$PLACEMENT_CHANGE")

  assert_contains "the matrix carries the placement leg" "$matrix" '"placement"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

@test "an edit to the placement suites alone runs the placement leg" {
  # The suites sit in two directories and the e2e-operator job runs both, on
  # top of the shared helpers under tests/e2e/lib/. A filter naming fewer lets
  # an edit under the others schedule no leg at all.
  assert_filter_is_wired tests_e2e_placement e2e-operators

  local block
  block=$(filter_block tests_e2e_placement)
  assert_contains "the per-CR suites are covered" "$block" "tests/e2e/placement/**"
  assert_contains "the operator-level suites are covered" "$block" \
    "tests/e2e/placement-operator/**"
  assert_contains "the shared suite helpers are covered" "$block" "tests/e2e/lib/**"

  assert_eq "a suite edit runs the placement leg alone" \
    'e2e-operators={"operator":["placement"]}' \
    "$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
      FILTER_tests_e2e_placement=true)"
}

@test "a placement chart change re-runs helm-validate" {
  # The helm filter is the operators/*/helm/** glob; the chart directory under
  # it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the placement chart lives under that glob" "yes" \
    "$([ -d "$CHART" ] && echo yes || echo no)"
}

@test "the unit and integration test matrices include placement" {
  # Both matrices read the resolver's list, and the resolver puts the operator
  # in it when its own code changes. An operator missing from either half
  # compiles and ships with its `go test` leg never run.
  local matrix_count
  matrix_count=$(grep -c \
    'matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}' "$CI_YAML") || true

  assert_eq "both the test and the test-integration matrix read test-targets" \
    "2" "$matrix_count"

  assert_contains "a placement change puts placement in the test matrix" \
    "$(grep "^test-targets=" <<<"$PLACEMENT_CHANGE")" \
    '"placement"'
}

@test "the derived cleanup package lists cover the placement images" {
  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(sed -n 's/^cleanup-packages=//p' <<<"$matrix")
  e2e_packages=$(sed -n 's/^cleanup-e2e-packages=//p' <<<"$matrix")

  assert_contains "the nightly sweep covers placement-operator" \
    "$all_packages" '"placement-operator"'
  assert_contains "the per-run sweep covers placement-operator" \
    "$e2e_packages" '"placement-operator"'
  assert_contains "the nightly sweep covers placement" \
    "$all_packages" '"placement"'
  assert_contains "the per-run sweep covers placement" \
    "$e2e_packages" '"placement"'
}

@test "a keystone-only change keeps placement out of the e2e-operators matrix" {
  # The positive case above proves the filter reaches the matrix; this one
  # proves it still gates. A filter wired to a constant would satisfy the
  # positive assertion and put every operator on every pull request.
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
    FILTER_keystone=true)

  assert_contains "the matrix carries the keystone leg" "$matrix" '"keystone"'
  assert_not_contains "and no placement leg" "$matrix" '"placement"'
}

@test "the e2e-chaos pod leg runs the placement suites" {
  local entry
  entry=$(e2e_chaos_matrix_entry pod)
  assert_not_empty "the pod leg exists" "$entry"
  assert_contains "it runs the operator pod-kill suite" "$entry" \
    "tests/e2e-chaos/placement-operator-pod-kill"
  assert_contains "it runs the Keystone outage suite" "$entry" \
    "tests/e2e-chaos/placement-keystone-outage"
}
