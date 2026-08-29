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
# lints and unit-tests nothing. The Scenario-5 skip and the ovn-operator deploy
# are the two places where the neutron leg departs from the shared shape:
# dropping either turns a green pipeline red for a reason that has nothing to
# do with the change under test. The last test covers the e2e-chaos network
# leg, which the e2e-operators matrix never produces and whose test_dirs are
# enumerated by hand because chainsaw's regex filters are no-ops.
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

# Echo the body of the top-level ci.yaml job <name>. The block ends at the next
# 2-space line that is not part of the body — the next job key, or the comment
# header introducing it.
job_block() {
  awk -v key="  $1:" '
    $0 == key { in_block = 1; next }
    in_block && /^  [#a-z0-9-]/ { exit }
    in_block { print }
  ' "$CI_YAML"
}

# Echo the body of the named step of the e2e-operator job. Steps sit at 6-space
# indent, so the next line at that indent ends the block: the following step,
# or the comment introducing it. Scoping to the job keeps a step of the same
# name in another job from satisfying an assertion here.
e2e_operator_step() {
  job_block e2e-operator | awk -v key="      - name: $1" '
    $0 == key { in_block = 1; next }
    in_block && /^      [-#]/ { exit }
    in_block { print }
  '
}

# Echo the body of the named step of the e2e-chaos job. Same shape as
# e2e_operator_step above; scoping matters because e2e-chaos and e2e-operator
# both carry a "Load E2E images" and a "Setup E2E infrastructure" step.
e2e_chaos_step() {
  job_block e2e-chaos | awk -v key="      - name: $1" '
    $0 == key { in_block = 1; next }
    in_block && /^      [-#]/ { exit }
    in_block { print }
  '
}

# Echo the e2e-chaos matrix entry whose suite key is $1. Entries sit at 10-space
# indent and their keys deeper, so the next line at that indent ends the block:
# the following entry, or the comment introducing it.
e2e_chaos_matrix_entry() {
  job_block e2e-chaos | awk -v key="          - suite: $1" '
    $0 == key { in_block = 1; next }
    in_block && /^          [-#]/ { exit }
    in_block { print }
  '
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

test_e2e_leg_deploys_the_ovn_operator() {
  echo "Test: the neutron e2e leg deploys the ovn-operator, not only its CRDs"

  # A Neutron never reaches Ready without a live OVNCentral: the Neutron
  # controller reads its published Northbound and Southbound addresses, and the
  # NeutronMetadataAgent controller resolves an OVNChassis. Applying the OVN
  # CRDs alone keeps the manager's cache sync from timing out and leaves every
  # suite waiting on objects nothing reconciles.
  local deploy
  deploy=$(e2e_operator_step "Deploy ovn-operator")

  assert_not_empty "the deploy step exists" "$deploy"
  assert_contains "it runs on the neutron leg only" "$deploy" \
    "if: matrix.operator == 'neutron'"
  assert_contains "it goes through the shared deploy script" "$deploy" \
    "run: hack/ci-deploy-operator.sh"
  assert_contains "it deploys the ovn operator" "$deploy" "OPERATOR: ovn"
  assert_contains "it uses the run-tagged ovn-operator image" "$deploy" \
    "IMAGE_PREFIX }}/ovn-operator"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: ovn-system"

  assert_file_not_contains "the CRD-only step it replaced is gone" "$CI_YAML" \
    "name: Install CRDs watched by the neutron-operator"

  # The OVNCentral has to be reconcilable before the neutron-operator's own
  # suites start, so the ovn deploy comes first.
  local job ovn_at neutron_at
  job=$(job_block e2e-operator)
  ovn_at=$(printf '%s\n' "$job" | grep -nF "name: Deploy ovn-operator" | head -1 | cut -d: -f1)
  neutron_at=$(printf '%s\n' "$job" | grep -nF "name: Deploy operator" | head -1 | cut -d: -f1)
  assert_eq "the ovn deploy runs before the matrix operator's own" "yes" \
    "$([ -n "$ovn_at" ] && [ -n "$neutron_at" ] && [ "$ovn_at" -lt "$neutron_at" ] && echo yes || echo no)"

  # Its pods live in ovn-system, which the first dump never looks at: that one
  # derives its Namespace from the matrix operator.
  local dump
  dump=$(e2e_operator_step "Dump diagnostic info (ovn)")
  assert_not_empty "the second diagnostics dump exists" "$dump"
  assert_contains "it dumps even when the suites failed" "$dump" \
    "if: always() && matrix.operator == 'neutron'"
  assert_contains "it dumps the ovn Namespace" "$dump" "OPERATOR: ovn"
}

test_e2e_leg_loads_the_ovn_images() {
  echo "Test: the neutron e2e leg loads both images the ovn-operator needs"

  # kind pulls nothing the run did not load: the operator image the deploy runs
  # and the daemon image its OVNCentral rolls out both have to be on the node.
  local resolve load ovn_branch neutron_branch
  resolve=$(e2e_operator_step "Resolve E2E images")
  load=$(e2e_operator_step "Load images into kind")
  neutron_branch=$(printf '%s\n' "$resolve" | awk '
    /= "neutron" \]; then/ { in_b = 1; next }
    in_b && /^ *fi$/ { exit }
    in_b { print }
  ')

  assert_not_empty "the resolve step branches on the neutron leg" "$neutron_branch"
  assert_contains "it pulls the ovn-operator image" "$neutron_branch" \
    '${IMAGE_PREFIX}/ovn-operator:dev'
  assert_contains "it pulls the OVN daemon image at the resolved pin" \
    "$neutron_branch" '${IMAGE_PREFIX}/ovn:${OVN_VERSION}'
  assert_contains "the load step mirrors the operator image" "$load" \
    'kind load docker-image "${IMAGE_PREFIX}/ovn-operator:dev"'
  assert_contains "the load step mirrors the daemon image" "$load" \
    'kind load docker-image "${IMAGE_PREFIX}/ovn:${OVN_VERSION}"'

  # The ovn leg already gets ovn-operator:dev from the generic
  # ${OPERATOR}-operator:dev line; naming it again there would load it twice.
  ovn_branch=$(printf '%s\n' "$resolve" | awk '
    /= "ovn" \]; then/ { in_b = 1; next }
    in_b && /^ *fi$/ { exit }
    in_b { print }
  ')
  assert_not_empty "the resolve step branches on the ovn leg too" "$ovn_branch"
  assert_not_contains "the ovn branch does not repeat the operator image" \
    "$ovn_branch" "ovn-operator:dev"
}

test_build_e2e_images_builds_the_neutron_images() {
  echo "Test: build-e2e-images reaches the neutron images on every run"

  # The tempest neutron legs and the e2e-chaos network leg pull
  # neutron-operator:dev and neutron:<release>, and their triggers are
  # independent of an operators/neutron change. Both are keys
  # hack/ci-resolve-e2e-images.sh derives from the tree — the operator image
  # from operators/neutron/go.mod, the service images from the neutron entries
  # of releases/*/source-refs.yaml — so the job hands those legs a reference
  # whether or not this pull request touched the operator: the run-scoped tag
  # when it built the image, the digest main published when it did not.
  assert_file_contains "the job resolves its images through the shared script" \
    "$CI_YAML" "hack/ci-resolve-e2e-images.sh"
  assert_eq "the neutron operator is one of the tree-derived image keys" "yes" \
    "$([ -f "$PROJECT_ROOT/operators/neutron/go.mod" ] && echo yes || echo no)"
  assert_not_empty "neutron ships a service image per release" \
    "$(OPERATOR=neutron "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
}

test_chaos_network_leg_runs_the_neutron_suites() {
  echo "Test: the e2e-chaos network leg runs both neutron outage suites"

  # e2e-chaos enumerates test_dirs per leg (chainsaw's include/exclude-regex
  # flags are no-ops in v0.2.14), so a suite missing from the list is
  # lint-checked and never applied to a cluster. The images and the operator
  # deploy are the rest of what the two suites need: kind pulls nothing the run
  # did not load, and a Neutron whose operator never deployed sits without
  # status until the suite times out.
  local entry
  entry=$(e2e_chaos_matrix_entry network)

  assert_not_empty "the network leg exists" "$entry"
  assert_contains "it runs the MariaDB outage suite" "$entry" \
    "tests/e2e-chaos/neutron-mariadb-outage"
  assert_contains "it runs the broker outage suite" "$entry" \
    "tests/e2e-chaos/neutron-broker-outage"

  local load
  load=$(e2e_chaos_step "Load E2E images")
  assert_contains "the leg pulls the neutron-operator image" "$load" \
    "matrix.suite == 'network' && format('{0}/neutron-operator:dev', env.IMAGE_PREFIX)"
  assert_contains "the leg pulls the neutron service image" "$load" \
    "matrix.suite == 'network' && format('{0}/neutron:2025.2', env.IMAGE_PREFIX)"

  local kind_load
  kind_load=$(e2e_chaos_step "Load neutron images into kind")
  assert_not_empty "both images reach the node" "$kind_load"
  assert_contains "the load runs on the network leg alone" "$kind_load" \
    "if: matrix.suite == 'network'"
  assert_contains "the operator image is loaded" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/neutron-operator:dev"
  assert_contains "the service image is loaded" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/neutron:2025.2"

  local deploy
  deploy=$(e2e_chaos_step "Deploy neutron operator")
  assert_not_empty "the neutron-operator is deployed" "$deploy"
  assert_contains "the deploy runs on the network leg alone" "$deploy" \
    "if: matrix.suite == 'network'"
  assert_contains "it goes through the shared deploy script" "$deploy" \
    "run: hack/ci-deploy-operator.sh"
  assert_contains "it deploys the neutron operator" "$deploy" "OPERATOR: neutron"
  assert_contains "it uses the run-tagged neutron-operator image" "$deploy" \
    "IMAGE_PREFIX }}/neutron-operator"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: neutron-system"

  # Both suites create their own Keystone and their own OVNCentral, so the
  # neutron-operator deploy has to come after the operators that reconcile
  # those two kinds.
  local job keystone_at ovn_at neutron_at
  job=$(job_block e2e-chaos)
  keystone_at=$(printf '%s\n' "$job" | grep -nF "name: Deploy operator" | head -1 | cut -d: -f1)
  ovn_at=$(printf '%s\n' "$job" | grep -nF "name: Deploy ovn operator" | head -1 | cut -d: -f1)
  neutron_at=$(printf '%s\n' "$job" | grep -nF "name: Deploy neutron operator" | head -1 | cut -d: -f1)
  assert_eq "the keystone and ovn deploys run first" "yes" \
    "$([ -n "$keystone_at" ] && [ -n "$ovn_at" ] && [ -n "$neutron_at" ] &&
       [ "$keystone_at" -lt "$neutron_at" ] && [ "$ovn_at" -lt "$neutron_at" ] &&
       echo yes || echo no)"
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
test_e2e_leg_deploys_the_ovn_operator
test_e2e_leg_loads_the_ovn_images
test_build_e2e_images_builds_the_neutron_images
test_chaos_network_leg_runs_the_neutron_suites

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
