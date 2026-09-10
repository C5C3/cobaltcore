#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the cinder operator reaches the e2e-operators matrix, the two Go test
# matrices and the helm filter in .github/workflows/ci.yaml.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/cinder/ are lint-checked
# and never applied to a cluster. SERVICE_OPERATORS is what tells the resolver
# that cinder ships an OpenStack service image per release, so an image rebuild
# reaches the cinder leg without the operator's Go gates. The helm filter is the
# sole gate on helm-validate, so a pull request touching only
# operators/cinder/helm/ renders, lints and unit-tests nothing. The chart render
# is where cinder departs from the ovn and neutron shape: those two refuse
# namespace-scoped RBAC and the job excuses the refusal, while the cinder chart
# takes the mode and every scenario has to render.
#
# Usage: bash tests/unit/ci/cinder_e2e_matrix_test.sh

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
ALL_OPS="keystone c5c3 horizon glance placement barbican ovn neutron cinder"

CHART="$PROJECT_ROOT/operators/cinder/helm/cinder-operator"

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

test_all_operators_lists_cinder() {
  echo "Test: ci.yaml ALL_OPERATORS includes cinder"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists cinder" "$line" "cinder"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_service_operators_lists_cinder() {
  echo "Test: ci.yaml SERVICE_OPERATORS includes cinder"

  # cinder ships an OpenStack service image for every release that carries a
  # cinder key in source-refs.yaml. Missing from this list, the resolver treats
  # it like the orchestration operators: the image is never rebuilt for a pull
  # request that changes it, and the e2e leg loads whatever main last published.
  local line
  line=$(grep "SERVICE_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "SERVICE_OPERATORS lists cinder" "$line" "cinder"
  assert_not_empty "cinder ships a service image per release" \
    "$(OPERATOR=cinder "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
}

test_cinder_filter_is_wired() {
  echo "Test: the cinder paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *cinder:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_cinder: \${{ steps.filter.outputs.cinder }}"

  local block
  block=$(filter_block cinder)
  assert_contains "the operator source path is covered" "$block" "operators/cinder/**"
}

test_cinder_change_produces_an_e2e_leg() {
  echo "Test: an operators/cinder change puts cinder in the e2e-operators matrix"

  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" FILTER_cinder=true)

  assert_contains "the matrix carries the cinder leg" "$matrix" '"cinder"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_helm_filter_covers_the_cinder_chart() {
  echo "Test: a cinder chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory under
  # it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the cinder chart lives under that glob" "yes" \
    "$([ -d "$CHART" ] && echo yes || echo no)"
}

test_helm_validate_renders_the_cinder_chart() {
  echo "Test: the cinder chart renders under every helm-validate scenario"

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
  # that documented refusal as a pass. The cinder chart carries no such
  # override, so a failed render there is a real one.
  assert_eq "every scenario renders" "" "$failed"

  # The refusal is not the only way to lose the mode: a chart can also render
  # nothing at all under it. Assert the namespaced Role is what comes out.
  out=$(helm template test "$CHART" --set rbac.namespaceScoped=true \
    --set webhook.enabled=false 2>&1)
  assert_contains "namespace-scoped RBAC produces a Role" "$out" "kind: Role"
}

test_go_matrices_list_cinder() {
  echo "Test: the unit and integration test matrices include cinder"

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

  assert_contains "a cinder change puts cinder in the test matrix" \
    "$(resolve_output test-targets refs/heads/main "$ALL_OPS" FILTER_cinder=true)" \
    '"cinder"'
}

test_cleanup_matrices_cover_the_cinder_images() {
  echo "Test: the derived cleanup package lists cover the cinder images"

  # cleanup-images.yaml and ci.yaml's cleanup-e2e-tags both build their package
  # matrix from this generator, so coverage is a property of its output rather
  # than of a list someone has to remember to extend. An uncovered package
  # leaks its run-scoped GHCR tags on every pull request.
  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(echo "$matrix" | sed -n 's/^cleanup-packages=//p')
  e2e_packages=$(echo "$matrix" | sed -n 's/^cleanup-e2e-packages=//p')

  assert_contains "the nightly sweep covers cinder-operator" \
    "$all_packages" '"cinder-operator"'
  assert_contains "the per-run sweep covers cinder-operator" \
    "$e2e_packages" '"cinder-operator"'
  assert_contains "the nightly sweep covers cinder" \
    "$all_packages" '"cinder"'
  assert_contains "the per-run sweep covers cinder" \
    "$e2e_packages" '"cinder"'
}

test_a_keystone_only_change_produces_no_cinder_leg() {
  echo "Test: a keystone-only change keeps cinder out of the e2e-operators matrix"

  # The positive case above proves the filter reaches the matrix; this one
  # proves it still gates. A filter wired to a constant would satisfy the
  # positive assertion and put every operator on every pull request.
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
    FILTER_keystone=true)

  assert_contains "the matrix carries the keystone leg" "$matrix" '"keystone"'
  assert_not_contains "and no cinder leg" "$matrix" '"cinder"'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_cinder
test_service_operators_lists_cinder
test_cinder_filter_is_wired
test_cinder_change_produces_an_e2e_leg
test_helm_filter_covers_the_cinder_chart
test_helm_validate_renders_the_cinder_chart
test_go_matrices_list_cinder
test_cleanup_matrices_cover_the_cinder_images
test_a_keystone_only_change_produces_no_cinder_leg

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
