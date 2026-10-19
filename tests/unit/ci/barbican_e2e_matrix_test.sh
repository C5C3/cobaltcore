#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the barbican operator reaches the e2e-operators matrix, the two Go test
# matrices, the helm filter, the cleanup matrices, the two e2e-chaos legs, the
# ControlPlane leg and the tempest leg in .github/workflows/ci.yaml.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/barbican/ are
# lint-checked and never applied to a cluster. SERVICE_OPERATORS is what tells
# the resolver that barbican ships an OpenStack service image per release, so an
# image rebuild reaches the barbican leg without the operator's Go gates. The
# image_barbican filter puts the service image into changed-services, so an edit
# under images/barbican/ or patches/barbican/ is built instead of pulled from
# main. tests_e2e_barbican schedules the leg for an edit that touches only the
# suites. The helm filter is the sole gate on helm-validate, so a pull request
# touching only operators/barbican/helm/ renders, lints and unit-tests nothing.
# An image package missing from the cleanup matrices leaks its run-scoped GHCR
# tags on every pull request.
#
# The barbican suites need one thing the other operators do not: a managed
# BarbicanSecretStore in the openstack Namespace gets its OpenBaoCluster
# provisioner token only where the chart granted the operator a TokenRequest
# Role for that account. hack/ci-deploy-operator.sh applies the grant from
# BARBICAN_SECRET_STORE_GRANTS, so a deploy step without the line installs an
# operator whose suites wait out their timeouts on a store that never turns
# Ready. tests/unit/hack/ci_deploy_operator_secret_store_grants_test.sh pins the
# script side; this file pins the steps that hand the value over. The
# ControlPlane leg deploys barbican without the grant: its store hangs off the
# instance the ControlPlane creates, and the ControlPlane projects the matching
# TokenRequest Role at runtime.
#
# The e2e-chaos legs enumerate their test directories by hand, so a barbican
# chaos suite absent from them is lint-checked and never applied to a cluster.
# Both the pod and the network leg run barbican suites, so the image loads and
# the operator deploy are gated on everything but the ovn leg; a pod-only or a
# network-only gate leaves the other leg's suites without an operator.
#
# The tempest leg is silent in the same way, one job over: the images the run
# tagged, the operator deploy, the key-manager catalog Job, the Barbican and
# BarbicanSecretStore the leg waits on, and the K8S_NAME variable that gates the
# port-forward. Any one of them missing leaves the leg burning its wait on a
# resource that never comes up.
#
# Usage: bash tests/unit/ci/barbican_e2e_matrix_test.sh

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

# One resolver run for an operators/barbican/ change. The e2e-operators
# and the test-targets checks below both read their line from it.
BARBICAN_CHANGE=$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_barbican=true)

# The release the e2e-chaos and ControlPlane legs load the barbican image at.
DEFAULT_RELEASE="2026.1"

CHART="$PROJECT_ROOT/operators/barbican/helm/barbican-operator"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_all_operators_lists_barbican() {
  echo "Test: ci.yaml ALL_OPERATORS includes barbican"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists barbican" "$line" "barbican"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_service_operators_lists_barbican() {
  echo "Test: ci.yaml SERVICE_OPERATORS includes barbican"

  # barbican ships an OpenStack service image for every release that carries a
  # barbican key in source-refs.yaml. Missing from this list, the resolver
  # treats it like the orchestration operators: the image is never rebuilt for
  # a pull request that changes it, and the e2e leg loads whatever main last
  # published.
  local line
  line=$(grep "SERVICE_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "SERVICE_OPERATORS lists barbican" "$line" "barbican"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  assert_not_empty "barbican ships a service image per release" \
    "$(OPERATOR=barbican "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
}

test_barbican_filter_is_wired() {
  echo "Test: the barbican paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *barbican:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_barbican: \${{ steps.filter.outputs.barbican }}"

  local block
  block=$(filter_block barbican)
  assert_contains "the operator source path is covered" "$block" "operators/barbican/**"
}

test_barbican_image_filter_is_wired() {
  echo "Test: an images/barbican change reaches changed-services"

  # changed-services is the list build-e2e-images rebuilds from source. The
  # lock under nix/locks/ and the patches under patches/barbican/ change the
  # image as much as its build context does, so all three belong here.
  assert_filter_is_wired image_barbican changed-services

  local block
  block=$(filter_block image_barbican)
  assert_contains "the image build context is covered" "$block" "images/barbican/**"
  assert_contains "the Nix locks are covered" "$block" "nix/locks/*/barbican/**"
  assert_contains "the source patches are covered" "$block" "patches/barbican/**"

  assert_eq "an image change rebuilds barbican and nothing else" \
    'changed-services=["barbican"]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS" \
      FILTER_image_barbican=true)"
}

test_barbican_change_produces_an_e2e_leg() {
  echo "Test: an operators/barbican change puts barbican in the e2e-operators matrix"

  local matrix
  matrix=$(grep "^e2e-operators=" <<<"$BARBICAN_CHANGE")

  assert_contains "the matrix carries the barbican leg" "$matrix" '"barbican"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_barbican_e2e_filter_is_wired() {
  echo "Test: an edit to the barbican suites alone runs the barbican leg"

  # The suites sit in two directories and the e2e-operator job runs both, on
  # top of the shared helpers under tests/e2e/lib/. A filter naming fewer lets
  # an edit under the others schedule no leg at all.
  assert_filter_is_wired tests_e2e_barbican e2e-operators

  local block
  block=$(filter_block tests_e2e_barbican)
  assert_contains "the per-CR suites are covered" "$block" "tests/e2e/barbican/**"
  assert_contains "the operator-level suites are covered" "$block" \
    "tests/e2e/barbican-operator/**"
  assert_contains "the shared suite helpers are covered" "$block" "tests/e2e/lib/**"

  assert_eq "a suite edit runs the barbican leg alone" \
    'e2e-operators={"operator":["barbican"]}' \
    "$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
      FILTER_tests_e2e_barbican=true)"
}

test_helm_filter_covers_the_barbican_chart() {
  echo "Test: a barbican chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory under
  # it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the barbican chart lives under that glob" "yes" \
    "$([ -d "$CHART" ] && echo yes || echo no)"
}

test_go_matrices_list_barbican() {
  echo "Test: the unit and integration test matrices include barbican"

  # Both matrices read the resolver's list, and the resolver puts the operator
  # in it when its own code changes. An operator missing from either half
  # compiles and ships with its `go test` leg never run.
  local matrix_count
  matrix_count=$(grep -c \
    'matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}' "$CI_YAML") || true

  assert_eq "both the test and the test-integration matrix read test-targets" \
    "2" "$matrix_count"

  assert_contains "a barbican change puts barbican in the test matrix" \
    "$(grep "^test-targets=" <<<"$BARBICAN_CHANGE")" \
    '"barbican"'
}

test_cleanup_matrices_cover_the_barbican_images() {
  echo "Test: the derived cleanup package lists cover the barbican images"

  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(sed -n 's/^cleanup-packages=//p' <<<"$matrix")
  e2e_packages=$(sed -n 's/^cleanup-e2e-packages=//p' <<<"$matrix")

  assert_contains "the nightly sweep covers barbican-operator" \
    "$all_packages" '"barbican-operator"'
  assert_contains "the per-run sweep covers barbican-operator" \
    "$e2e_packages" '"barbican-operator"'
  assert_contains "the nightly sweep covers barbican" \
    "$all_packages" '"barbican"'
  assert_contains "the per-run sweep covers barbican" \
    "$e2e_packages" '"barbican"'
}

test_a_keystone_only_change_produces_no_barbican_leg() {
  echo "Test: a keystone-only change keeps barbican out of the e2e-operators matrix"

  # The positive case above proves the filter reaches the matrix; this one
  # proves it still gates. A filter wired to a constant would satisfy the
  # positive assertion and put every operator on every pull request.
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
    FILTER_keystone=true)

  assert_contains "the matrix carries the keystone leg" "$matrix" '"keystone"'
  assert_not_contains "and no barbican leg" "$matrix" '"barbican"'
}

test_e2e_operator_deploy_grants_the_secret_store() {
  echo "Test: the e2e-operator deploy grants the barbican secret store"

  # ci-deploy-operator.sh reads the value for OPERATOR=barbican only, so the
  # line sits on the shared step and the other legs ignore it.
  assert_contains "the e2e-operator deploy grants openstack the provisioner account" \
    "$(job_step e2e-operator "Deploy operator")" \
    "BARBICAN_SECRET_STORE_GRANTS: openstack=openbao-instance-provisioner"
}

test_chaos_legs_run_the_barbican_suites() {
  echo "Test: the e2e-chaos pod and network legs run the barbican suites"

  local pod network
  pod=$(e2e_chaos_matrix_entry pod)
  network=$(e2e_chaos_matrix_entry network)
  assert_not_empty "the pod leg exists" "$pod"
  assert_contains "the pod leg runs the operator pod-kill suite" "$pod" \
    "tests/e2e-chaos/barbican-operator-pod-kill"
  assert_contains "the pod leg runs the Keystone outage suite" "$pod" \
    "tests/e2e-chaos/barbican-keystone-outage"
  assert_not_empty "the network leg exists" "$network"
  assert_contains "the network leg runs the OpenBao outage suite" "$network" \
    "tests/e2e-chaos/barbican-openbao-outage"
  assert_contains "the network leg runs the MariaDB outage suite" "$network" \
    "tests/e2e-chaos/barbican-mariadb-outage"

  # Both legs need the images, so the pull is gated on everything but the ovn
  # leg rather than on one of the two.
  local pull
  pull=$(job_step e2e-chaos "Load E2E images (keystone stack)")
  assert_contains "the keystone-stack pull skips the ovn leg alone" "$pull" \
    "if: matrix.suite != 'ovn'"
  assert_contains "it pulls the barbican-operator image" "$pull" \
    "\${{ env.IMAGE_PREFIX }}/barbican-operator:dev"
  assert_contains "it pulls the barbican service image" "$pull" \
    "\${{ env.IMAGE_PREFIX }}/barbican:${DEFAULT_RELEASE}"
  assert_not_contains "the pull is not gated on the pod leg alone" "$pull" \
    "if: matrix.suite == 'pod'"
  assert_not_contains "the pull is not gated on the network leg alone" "$pull" \
    "if: matrix.suite == 'network'"

  local kind_load
  kind_load=$(job_step e2e-chaos "Load images into kind")
  assert_contains "the operator image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/barbican-operator:dev"
  assert_contains "the service image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/barbican:${DEFAULT_RELEASE}"

  local deploy
  deploy=$(job_step e2e-chaos "Deploy barbican operator")
  assert_not_empty "the barbican-operator is deployed" "$deploy"
  assert_contains "the deploy skips the ovn leg alone" "$deploy" \
    "if: matrix.suite != 'ovn'"
  assert_contains "it deploys the barbican operator" "$deploy" "OPERATOR: barbican"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: barbican-system"
  assert_contains "the chaos deploy grants openstack the provisioner account" \
    "$deploy" "BARBICAN_SECRET_STORE_GRANTS: openstack=openbao-instance-provisioner"
}

test_controlplane_leg_deploys_barbican() {
  echo "Test: the e2e-controlplane leg loads and deploys barbican without the grant"

  local pull kind_load deploy
  pull=$(job_step e2e-controlplane "Load E2E images")
  assert_contains "the leg pulls the barbican-operator image" "$pull" \
    "barbican-operator:dev"
  assert_contains "the leg pulls the barbican service image" "$pull" \
    "barbican:${DEFAULT_RELEASE}"

  kind_load=$(job_step e2e-controlplane "Load images into kind")
  assert_contains "the operator image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/barbican-operator:dev"
  assert_contains "the service image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/barbican:${DEFAULT_RELEASE}"

  deploy=$(job_step e2e-controlplane "Deploy barbican-operator")
  assert_not_empty "the barbican-operator is deployed" "$deploy"
  assert_contains "it deploys the barbican operator" "$deploy" "OPERATOR: barbican"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: barbican-system"
  assert_not_contains "the ControlPlane leg grants no secret store" "$deploy" \
    "BARBICAN_SECRET_STORE_GRANTS"
}

test_tempest_barbican_leg_is_wired() {
  echo "Test: the tempest barbican leg is generated and brought up"

  assert_file_contains "the matrix generator knows the barbican service" \
    "$PROJECT_ROOT/hack/ci-generate-tempest-matrix.sh" \
    "^ALL_TEMPEST_SERVICES=.*[( ]barbican[ )]"

  local out
  out=$(mktemp)
  GITHUB_OUTPUT="$out" TEMPEST_SERVICES=barbican \
    bash "$PROJECT_ROOT/hack/ci-generate-tempest-matrix.sh" >/dev/null 2>&1
  assert_file_contains_fixed "the generator emits a barbican leg" "$out" \
    '"service":"barbican"'
  rm -f "$out"

  local gate="if: matrix.service == 'barbican'"
  local step

  step=$(job_step tempest "Load Barbican E2E images")
  assert_contains "the pull runs on the barbican leg" "$step" "$gate"
  assert_contains "it pulls the barbican-operator image" "$step" "barbican-operator:dev"
  assert_contains "it pulls the barbican service image of the leg's release" "$step" \
    "barbican:\${{ matrix.release }}"

  step=$(job_step tempest "Load Barbican images into kind")
  assert_contains "the kind load runs on the barbican leg" "$step" "$gate"
  assert_contains "the operator image reaches the node" "$step" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/barbican-operator:dev"
  assert_contains "the service image reaches the node" "$step" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/barbican:\${{ matrix.release }}"
  assert_contains "the tempest image reaches the node" "$step" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/tempest:\${{ matrix.release }}"

  step=$(job_step tempest "Deploy barbican operator")
  assert_contains "the deploy runs on the barbican leg" "$step" "$gate"
  assert_contains "it deploys the barbican operator" "$step" "OPERATOR: barbican"

  assert_contains "the key-manager catalog Job is awaited" \
    "$(job_step tempest "Bootstrap key-manager catalog")" \
    "job/barbican-tempest-catalog-setup"

  step=$(job_step tempest "Deploy Barbican CR for Tempest")
  assert_contains "the leg waits on the secret store" "$step" \
    "kubectl wait barbicansecretstore/\${{ matrix.barbican-cr-name }}-store"
  assert_contains "the leg waits on the Barbican" "$step" \
    "kubectl wait barbican/\${{ matrix.barbican-cr-name }}"

  assert_contains "the run step names the Barbican to port-forward" \
    "$(job_step tempest "Run Tempest API tests")" \
    "BARBICAN_K8S_NAME: \${{ matrix.barbican-cr-name }}"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_barbican
test_service_operators_lists_barbican
test_barbican_filter_is_wired
test_barbican_image_filter_is_wired
test_barbican_change_produces_an_e2e_leg
test_barbican_e2e_filter_is_wired
test_helm_filter_covers_the_barbican_chart
test_go_matrices_list_barbican
test_cleanup_matrices_cover_the_barbican_images
test_a_keystone_only_change_produces_no_barbican_leg
test_e2e_operator_deploy_grants_the_secret_store
test_chaos_legs_run_the_barbican_suites
test_controlplane_leg_deploys_barbican
test_tempest_barbican_leg_is_wired

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
