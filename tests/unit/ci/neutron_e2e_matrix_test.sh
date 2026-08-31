#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the neutron operator reaches the e2e-operators matrix and the helm
# filter in .github/workflows/ci.yaml, and that its two exceptions hold.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the
# sole source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/neutron/ are
# lint-checked and never applied to a cluster. The helm filter is the sole gate
# on helm-validate, so a PR touching only operators/neutron/helm/ renders,
# lints and unit-tests nothing. The Scenario-5 skip and the OVN-CRD install
# step are the two places where the neutron leg departs from the shared shape:
# dropping either turns a green pipeline red for a reason that has nothing to
# do with the change under test.
#
# Usage: bash tests/unit/ci/neutron_e2e_matrix_test.sh

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

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo the paths-filter block of the given filter key. Filter keys sit at
# 12-space indent and their entries deeper, so the next key at that indent ends
# the block. Scoping matters for shared entries such as operators/Dockerfile,
# which every operator filter lists.
filter_block() {
  awk -v key="            $1:" '
    $0 == key { in_block = 1; next }
    in_block && /^            [a-z0-9_]+:$/ { exit }
    in_block { print }
  ' "$CI_YAML"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_all_operators_lists_neutron() {
  echo "Test: ci.yaml ALL_OPERATORS includes neutron"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists neutron" "$line" "neutron"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_neutron_filter_is_wired() {
  echo "Test: the neutron paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *neutron:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_neutron: \${{ steps.filter.outputs.neutron }}"

  local block
  block=$(filter_block neutron)
  assert_contains "the operator source path is covered" "$block" "operators/neutron/**"

  # The service image lives in a filter of their own, so rebuilding it runs the
  # neutron e2e leg without the operator's Go gates.
  assert_file_contains "the image filter exists" "$CI_YAML" "^ *image_neutron:$"
  assert_file_contains "the image filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_image_neutron: \${{ steps.filter.outputs.image_neutron }}"
  assert_contains "the Neutron image is covered" \
    "$(filter_block image_neutron)" "images/neutron/**"
}

test_neutron_change_produces_an_e2e_leg() {
  echo "Test: an operators/neutron change puts neutron in the e2e-operators matrix"

  local all_operators="keystone c5c3 horizon glance placement barbican ovn neutron"
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$all_operators" FILTER_neutron=true)

  assert_contains "the matrix carries the neutron leg" "$matrix" '"neutron"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_helm_filter_covers_the_neutron_chart() {
  echo "Test: a neutron chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory
  # under it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the neutron chart lives under that glob" "yes" \
    "$([ -d "$PROJECT_ROOT/operators/neutron/helm/neutron-operator" ] && echo yes || echo no)"
}

test_helm_validate_renders_the_neutron_chart() {
  echo "Test: helm-validate lints, templates and unit-tests the neutron chart"

  # The three `for chart in ...` loops of helm-validate — lint, template and
  # unittest — iterate the operators/*/helm/*-operator glob, so the chart is
  # covered by living in that layout (asserted above).
  local loops
  loops=$(grep -cF 'for chart in operators/*/helm/*-operator' "$CI_YAML")
  assert_eq "the lint, template and unittest loops iterate the chart glob" "3" "$loops"
}

test_scenario_five_accepts_the_neutron_refusal() {
  echo "Test: helm-validate accepts the neutron chart's refusal of Scenario 5"

  # neutron-operator refuses rbac.namespaceScoped=true on purpose: its
  # _helpers.tpl overrides the operator-library.chart.namespaceScopedUnsupported
  # hook, the shared Role template fails the render with the documented
  # "is not supported by <chart>" message (role_test.yaml pins it), and
  # Scenario 5 treats that message as a pass instead of carrying a name list.
  assert_contains "scenario 5 accepts the documented refusal" \
    "$(grep -F 'is not supported by' "$CI_YAML")" \
    'rbac.namespaceScoped=true is not supported by'
  local hook='define "operator-library.chart.namespaceScopedUnsupported"'
  assert_eq "the neutron chart overrides the refusal hook" "1" \
    "$(grep -cF "$hook" "$PROJECT_ROOT/operators/neutron/helm/neutron-operator/templates/_helpers.tpl")"
  assert_eq "the ovn chart still overrides the refusal hook" "1" \
    "$(grep -cF "$hook" "$PROJECT_ROOT/operators/ovn/helm/ovn-operator/templates/_helpers.tpl")"
}

test_e2e_leg_installs_the_ovn_crds() {
  echo "Test: the neutron e2e leg installs the OVN CRDs"

  # Both neutron reconcilers watch an OVN kind. Without those CRDs the manager
  # never finishes its cache sync and the leg fails on every suite at once.
  assert_file_contains "the install step exists" "$CI_YAML" \
    "name: Install CRDs watched by the neutron-operator"
  assert_file_contains "the step runs on the neutron leg only" "$CI_YAML" \
    "if: matrix.operator == 'neutron'"
  assert_file_contains "the step applies the OVN CRDs" "$CI_YAML" \
    "kubectl apply -f operators/ovn/helm/ovn-operator/crds/"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_neutron
test_neutron_filter_is_wired
test_neutron_change_produces_an_e2e_leg
test_helm_filter_covers_the_neutron_chart
test_helm_validate_renders_the_neutron_chart
test_scenario_five_accepts_the_neutron_refusal
test_e2e_leg_installs_the_ovn_crds

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
