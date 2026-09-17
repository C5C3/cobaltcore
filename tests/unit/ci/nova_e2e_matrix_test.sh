#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the nova operator reaches the e2e-operators matrix, the two Go test
# matrices and the helm filter in .github/workflows/ci.yaml.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/nova/ are lint-checked
# and never applied to a cluster. SERVICE_OPERATORS is what tells the resolver
# that nova ships an OpenStack service image per release, so an image rebuild
# reaches the nova leg without the operator's Go gates. The helm filter is the
# sole gate on helm-validate, so a pull request touching only
# operators/nova/helm/ renders, lints and unit-tests nothing. The nova chart
# takes namespace-scoped RBAC instead of refusing it the way the ovn and neutron
# charts do, so every helm-validate scenario has to render.
#
# The assertions this file does not carry yet are the ones whose wiring is not
# in ci.yaml yet: the tempest_nova paths filter and the tempest legs (#1040),
# the e2e suites beyond the invalid-cr rejection corpus and the chaos leg
# (#1039), and the ControlPlane leg (#1019). They belong here once that wiring
# exists.
#
# Usage: bash tests/unit/ci/nova_e2e_matrix_test.sh

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
# make the two matrix scenarios below assert nothing.
ALL_OPS="keystone c5c3 horizon glance placement barbican ovn neutron cinder nova"

CHART="$PROJECT_ROOT/operators/nova/helm/nova-operator"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the flags of every `helm template` scenario the helm-validate job runs,
# one scenario per line; the default-values scenario comes out as an empty line.
# Reading them out of the job keeps this test rendering what CI renders: a
# scenario added there alone would leave the chart unproven under it.
helm_template_scenarios() {
  job_step helm-validate "Helm template" |
    grep -oE 'helm template test "\$chart".*' |
    sed -e 's/^helm template test "\$chart" *//' -e 's/ 2>&1).*$//'
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_all_operators_lists_nova() {
  echo "Test: ci.yaml ALL_OPERATORS includes nova"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists nova" "$line" "nova"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_service_operators_lists_nova() {
  echo "Test: ci.yaml SERVICE_OPERATORS includes nova"

  # nova ships an OpenStack service image for every release that carries a nova
  # key in source-refs.yaml. Missing from this list, the resolver treats it like
  # the orchestration operators: the image is never rebuilt for a pull request
  # that changes it, and the e2e leg loads whatever main last published.
  local line
  line=$(grep "SERVICE_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "SERVICE_OPERATORS lists nova" "$line" "nova"
  assert_not_empty "nova ships a service image per release" \
    "$(OPERATOR=nova "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
}

test_nova_filter_is_wired() {
  echo "Test: the nova paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *nova:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_nova: \${{ steps.filter.outputs.nova }}"

  local block
  block=$(filter_block nova)
  assert_contains "the operator source path is covered" "$block" "operators/nova/**"
}

test_nova_change_produces_an_e2e_leg() {
  echo "Test: an operators/nova change puts nova in the e2e-operators matrix"

  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" FILTER_nova=true)

  assert_contains "the matrix carries the nova leg" "$matrix" '"nova"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_helm_filter_covers_the_nova_chart() {
  echo "Test: a nova chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory under
  # it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the nova chart lives under that glob" "yes" \
    "$([ -d "$CHART" ] && echo yes || echo no)"
}

test_helm_validate_renders_the_nova_chart() {
  echo "Test: the nova chart renders under every helm-validate scenario"

  if ! command -v helm >/dev/null 2>&1; then
    echo "  SKIP: helm not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  # helm-validate vendors the operator-library subchart with `make helm-deps`
  # before it renders anything. charts/ is gitignored, so do the same here
  # instead of skipping on a working copy that has not run the target yet.
  if ! compgen -G "$CHART/charts/operator-library-*.tgz" >/dev/null; then
    helm dependency build --skip-refresh "$CHART" >/dev/null 2>&1
  fi

  local scenarios
  scenarios=$(helm_template_scenarios)
  assert_not_empty "the job's template scenarios are readable" "$scenarios"
  assert_contains "the namespace-scoped scenario is one of them" "$scenarios" \
    "--set rbac.namespaceScoped=true"

  local flags out failed=""
  while IFS= read -r flags; do
    # shellcheck disable=SC2086 # deliberate: split the scenario's flags
    if ! out=$(helm template test "$CHART" $flags 2>&1); then
      failed="${failed}${flags:-default values}: ${out}"$'\n'
    fi
  done <<<"$scenarios"

  # ovn-operator and neutron-operator refuse Scenario 5 by overriding the
  # operator-library.chart.namespaceScopedUnsupported hook, and the job reads
  # that documented refusal as a pass. The nova chart carries no such override,
  # so a failed render there is a real one.
  assert_eq "every scenario renders" "" "$failed"

  # The refusal is not the only way to lose the mode: a chart can also render
  # nothing at all under it. Assert the namespaced Role is what comes out.
  out=$(helm template test "$CHART" --set rbac.namespaceScoped=true \
    --set webhook.enabled=false 2>&1)
  assert_contains "namespace-scoped RBAC produces a Role" "$out" "kind: Role"
}

test_go_matrices_list_nova() {
  echo "Test: the unit and integration test matrices include nova"

  # Both matrices are resolved per pull request rather than hand-written, so an
  # operator reaches them in two steps: the jobs read the resolver's list, and
  # the resolver puts the operator in it when its own code changes. An operator
  # missing from either half compiles and ships with its `go test` leg never
  # run, and the pipeline stays green because no job failed.
  local matrix_count
  matrix_count=$(grep -c \
    'matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}' "$CI_YAML") || true

  assert_eq "both the test and the test-integration matrix read test-targets" \
    "2" "$matrix_count"

  assert_contains "a nova change puts nova in the test matrix" \
    "$(resolve_output test-targets refs/heads/main "$ALL_OPS" FILTER_nova=true)" \
    '"nova"'
}

test_cleanup_matrices_cover_the_nova_images() {
  echo "Test: the derived cleanup package lists cover the nova images"

  # cleanup-images.yaml and ci.yaml's cleanup-e2e-tags both build their package
  # matrix from this generator, so coverage is a property of its output rather
  # than of a list someone has to remember to extend. An uncovered package
  # leaks its run-scoped GHCR tags on every pull request.
  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(echo "$matrix" | sed -n 's/^cleanup-packages=//p')
  e2e_packages=$(echo "$matrix" | sed -n 's/^cleanup-e2e-packages=//p')

  assert_contains "the nightly sweep covers nova-operator" \
    "$all_packages" '"nova-operator"'
  assert_contains "the per-run sweep covers nova-operator" \
    "$e2e_packages" '"nova-operator"'
  assert_contains "the nightly sweep covers nova" \
    "$all_packages" '"nova"'
  assert_contains "the per-run sweep covers nova" \
    "$e2e_packages" '"nova"'
}

test_a_keystone_only_change_produces_no_nova_leg() {
  echo "Test: a keystone-only change keeps nova out of the e2e-operators matrix"

  # The positive case above proves the filter reaches the matrix; this one
  # proves it still gates. A filter wired to a constant would satisfy the
  # positive assertion and put every operator on every pull request.
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
    FILTER_keystone=true)

  assert_contains "the matrix carries the keystone leg" "$matrix" '"keystone"'
  assert_not_contains "and no nova leg" "$matrix" '"nova"'
}

test_nova_image_filter_is_wired() {
  echo "Test: an images/nova change reaches changed-services"

  # changed-services is the list build-e2e-images rebuilds from source. An
  # image edit missing from it leaves the nova leg loading whatever main last
  # published, so the suites pass against the old image and the change under
  # review is never run. patches/nova/ belongs in the same filter:
  # hack/ci-build-service-image.sh applies patches/<op>/<release>/*.patch into
  # the source it builds, so a patch edited on its own changes that image.
  assert_filter_is_wired image_nova changed-services

  local block
  block=$(filter_block image_nova)
  assert_contains "the image build context is covered" "$block" "images/nova/**"
  assert_contains "the source patches are covered" "$block" "patches/nova/**"

  assert_eq "an image change rebuilds nova and nothing else" \
    'changed-services=["nova"]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS" \
      FILTER_image_nova=true)"

  # The rebuild is what schedules the leg here: the operator's own Go gates stay
  # shut, so the image is the only thing that puts nova on the runner.
  local outputs
  outputs=$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_image_nova=true)
  assert_contains "the rebuilt image schedules the nova leg" "$outputs" \
    'e2e-operators={"operator":["nova"]}'
  assert_contains "no Go job is asked for" "$outputs" "go=false"
  assert_contains "and no operator counts as changed" "$outputs" \
    "changed-operators=[]"

  # And it still gates: a run that matched no filter rebuilds nothing.
  assert_eq "an untouched image is not rebuilt" 'changed-services=[]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS")"
}

test_nova_e2e_filter_is_wired() {
  echo "Test: an edit to the nova suites alone runs the nova leg"

  # The suites sit in two directories and the e2e-operator run step probes for
  # the -operator one. tests/e2e/nova-operator/ arrives with #1039's
  # scrape-target suite; the filter names it ahead of that, so the first suite
  # landing there schedules the leg instead of none.
  assert_filter_is_wired tests_e2e_nova e2e-operators

  local block
  block=$(filter_block tests_e2e_nova)
  assert_contains "the per-CR suites are covered" "$block" "tests/e2e/nova/**"
  assert_contains "the operator-level suites are covered" "$block" \
    "tests/e2e/nova-operator/**"

  local outputs
  outputs=$(resolve_outputs refs/heads/main "$ALL_OPS" \
    FILTER_tests_e2e_nova=true)
  assert_contains "a suite edit runs the nova leg" "$outputs" \
    'e2e-operators={"operator":["nova"]}'
  assert_contains "and spends no runner on the Go matrices" "$outputs" \
    'test-targets={"target":["__none__"]}'

  # That leg and no other, in both directions: a sibling's suite edit schedules
  # its own leg alone and leaves nova out.
  assert_eq "a glance suite edit runs the glance leg alone" \
    'e2e-operators={"operator":["glance"]}' \
    "$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
      FILTER_tests_e2e_glance=true)"
}

test_nova_leg_opts_into_the_broker() {
  echo "Test: the nova e2e leg asks for the broker and nothing else"

  # Every Nova process dials the bus, and the scheduler and the conductor
  # report ready off their broker connection. deploy-infra.sh installs the
  # broker only when it is asked to, and setup-e2e-infra reads the flag from
  # this step's env, so a Nova on a leg without it waits out its readiness on
  # an unreachable transport. The NFS export and the host kernel modules stay
  # off: no Nova suite mounts a volume or places a chassis.
  local setup
  setup=$(job_step e2e-operator "Setup E2E infrastructure")

  assert_not_empty "the setup step is readable" "$setup"
  assert_contains "the step uses the shared composite action" "$setup" \
    "uses: ./.github/actions/setup-e2e-infra"
  assert_contains "the nova leg opts into the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ (matrix.operator == 'cinder' || matrix.operator == 'nova') && 'true' || '' }}"
  assert_contains "the NFS export stays cinder-only" "$setup" \
    "WITH_NFS: \${{ matrix.operator == 'cinder' && 'true' || '' }}"

  # The kernel modules are loaded on the runner host itself, so a nova arm on
  # that line would touch the host for suites that place no chassis.
  local modules
  modules=$(grep WITH_OVN_KERNEL_MODULES <<<"$setup")
  assert_not_empty "the kernel-module flag is readable" "$modules"
  assert_not_contains "the chassis modules stay off the nova leg" "$modules" \
    "nova"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_nova
test_service_operators_lists_nova
test_nova_filter_is_wired
test_nova_change_produces_an_e2e_leg
test_helm_filter_covers_the_nova_chart
test_helm_validate_renders_the_nova_chart
test_go_matrices_list_nova
test_cleanup_matrices_cover_the_nova_images
test_a_keystone_only_change_produces_no_nova_leg
test_nova_image_filter_is_wired
test_nova_e2e_filter_is_wired
test_nova_leg_opts_into_the_broker

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
