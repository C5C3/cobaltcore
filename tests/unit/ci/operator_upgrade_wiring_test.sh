#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the wiring of the per-operator helm upgrade suites:
#   - every operator chart under operators/*/helm/*-operator has a suite under
#     tests/e2e-operator-upgrade/<op>-helm-upgrade/ with a baseline.yaml whose
#     pin is X.Y.Z and, compared field by field, not above the chart's version
#     (a pin above it names a chart main has not published);
#   - every operator has a tests_operator_upgrade_<op> paths filter listing its
#     suite, and the shared tests_operator_upgrade filter lists the inputs every
#     suite shares;
#   - merge-operator-images reads the chart version before it generates the
#     image metadata, and tags the index with it on main;
#   - the e2e-operator-upgrade job of ci.yaml calls the reusable workflow with
#     both inputs and keeps its own gate, and cleanup-e2e-tags still waits for
#     it;
#   - the reusable workflow takes the three inputs and runs the leg table, the
#     fetch script and the pinned baseline, and its c5c3 CRD list is the one
#     of the e2e-operator and e2e-orders-multicluster jobs;
#   - make e2e-operator-upgrade deploys the image tag and chart directory the
#     fetch script wrote, with the barbican grants of the workflow.
#
# A pin that drifts above its chart, or an operator that gains a chart without
# a suite, would fail only on a pull request that happens to run that job;
# this test fails on every one.
#
# Usage: bash tests/unit/ci/operator_upgrade_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
UPGRADE_YAML="$PROJECT_ROOT/.github/workflows/e2e-operator-upgrade.yaml"
SUITES_DIR="$PROJECT_ROOT/tests/e2e-operator-upgrade"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# yaml_version <file> — the chart version a YAML file names, read by the
# hack/ci-read-chart-version.sh that hack/ci-fetch-released-operator.sh calls.
yaml_version() {
  bash "$PROJECT_ROOT/hack/ci-read-chart-version.sh" "$1"
}

# version_above <a> <b> — true when the X.Y.Z version a is above b, compared
# field by field as numbers (0.14.0 is above 0.9.0).
version_above() {
  local a1 a2 a3 b1 b2 b3
  IFS=. read -r a1 a2 a3 <<<"$1"
  IFS=. read -r b1 b2 b3 <<<"$2"
  if ((10#$a1 != 10#$b1)); then ((10#$a1 > 10#$b1)); return; fi
  if ((10#$a2 != 10#$b2)); then ((10#$a2 > 10#$b2)); return; fi
  ((10#$a3 > 10#$b3))
}

# pin_problem <baseline.yaml> <Chart.yaml> — echoes what is wrong with the pin,
# or nothing when it is an X.Y.Z version not above the chart's.
pin_problem() {
  local pin_file="$1" chart_file="$2" pin chart
  if [ ! -f "$pin_file" ]; then
    echo "no pin at ${pin_file#"$PROJECT_ROOT"/}"
    return
  fi
  pin="$(yaml_version "$pin_file")"
  chart="$(yaml_version "$chart_file")"
  if [[ ! "$pin" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "pin '$pin' is not X.Y.Z"
  elif [[ ! "$chart" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "chart version '$chart' is not X.Y.Z"
  elif version_above "$pin" "$chart"; then
    echo "pin $pin is above the chart version $chart"
  fi
}

# chart_operators — every operator with a chart under operators/*/helm/, in
# glob order.
chart_operators() {
  local chart op
  for chart in "$PROJECT_ROOT"/operators/*/helm/*-operator/Chart.yaml; do
    [ -f "$chart" ] || continue
    op="${chart#"$PROJECT_ROOT"/operators/}"
    echo "${op%%/*}"
  done
}

# workflow_job <file> <job> — job_block of another workflow file.
workflow_job() {
  CI_YAML="$1" job_block "$2"
}

# run_block — the lines after the `run: |` line of the step on stdin.
run_block() {
  sed -n '/^        run: |$/,$p' | sed '1d'
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_every_chart_has_a_pinned_suite() {
  echo "Test: every operator chart has a suite and a baseline.yaml pin not above its version"

  local op checked=0
  for op in $(chart_operators); do
    checked=$((checked + 1))
    if [ -f "$SUITES_DIR/${op}-helm-upgrade/chainsaw-test.yaml" ]; then
      assert_eq "$op has tests/e2e-operator-upgrade/${op}-helm-upgrade/chainsaw-test.yaml" "yes" "yes"
    else
      assert_eq "$op has tests/e2e-operator-upgrade/${op}-helm-upgrade/chainsaw-test.yaml" "yes" "no"
    fi
    assert_eq "$op's pin is X.Y.Z and not above its chart version" "" \
      "$(pin_problem "$SUITES_DIR/${op}-helm-upgrade/baseline.yaml" \
        "$PROJECT_ROOT/operators/${op}/helm/${op}-operator/Chart.yaml")"
    assert_contains "$op's suite is the Test ${op}-operator-helm-upgrade" \
      "$(cat "$SUITES_DIR/${op}-helm-upgrade/chainsaw-test.yaml" 2>/dev/null)" \
      "  name: ${op}-operator-helm-upgrade"
  done
  assert_eq "the ten operator charts were checked" "10" "$checked"
}

test_the_pin_check_catches_a_bad_pin() {
  echo "Test: the pin check reports a pin above its chart, compared field by field"

  local keystone_chart="$PROJECT_ROOT/operators/keystone/helm/keystone-operator/Chart.yaml"
  printf 'version: "0.99.0"\n' >"$TMP_DIR/pin.yaml"
  assert_contains "a keystone pin of 0.99.0 is above the keystone chart" \
    "$(pin_problem "$TMP_DIR/pin.yaml" "$keystone_chart")" "pin 0.99.0 is above the chart version"

  printf 'version: 0.9.0\n' >"$TMP_DIR/chart.yaml"
  printf 'version: "0.14.0"\n' >"$TMP_DIR/pin.yaml"
  assert_eq "0.14.0 is above 0.9.0 (numbers, not strings)" \
    "pin 0.14.0 is above the chart version 0.9.0" "$(pin_problem "$TMP_DIR/pin.yaml" "$TMP_DIR/chart.yaml")"
  printf 'version: "0.8.12"\n' >"$TMP_DIR/pin.yaml"
  assert_eq "0.8.12 is below 0.9.0" "" "$(pin_problem "$TMP_DIR/pin.yaml" "$TMP_DIR/chart.yaml")"
  printf 'version: "0.9.0"\n' >"$TMP_DIR/pin.yaml"
  assert_eq "a pin equal to the chart version is allowed" "" \
    "$(pin_problem "$TMP_DIR/pin.yaml" "$TMP_DIR/chart.yaml")"
  printf 'version: latest\n' >"$TMP_DIR/pin.yaml"
  assert_eq "a non-X.Y.Z pin is reported" "pin 'latest' is not X.Y.Z" \
    "$(pin_problem "$TMP_DIR/pin.yaml" "$TMP_DIR/chart.yaml")"
  assert_contains "a missing pin is reported" \
    "$(pin_problem "$TMP_DIR/absent.yaml" "$TMP_DIR/chart.yaml")" "no pin at"
}

test_every_operator_has_an_upgrade_filter() {
  echo "Test: every operator has a tests_operator_upgrade_<op> filter, and the shared one lists the shared inputs"

  local op block
  for op in $(ci_all_operators_from_ci_yaml); do
    block="$(filter_block "tests_operator_upgrade_${op}")"
    assert_contains "tests_operator_upgrade_${op} lists the ${op} suite" "$block" \
      "- 'tests/e2e-operator-upgrade/${op}-helm-upgrade/**'"
    case "$op" in
      cinder | nova)
        assert_contains "tests_operator_upgrade_${op} lists the vhost script its suite calls" \
          "$block" "- 'tests/e2e/cinder/broker-vhost.sh'"
        ;;
      keystone)
        assert_contains "tests_operator_upgrade_keystone lists the CRD window check its suite calls" \
          "$block" "- 'hack/ci-operator-upgrade-crd-window.sh'"
        ;;
    esac
  done

  local shared path
  shared="$(filter_block tests_operator_upgrade)"
  for path in \
    tests/e2e-operator-upgrade/chainsaw-config.yaml \
    hack/ci-fetch-released-operator.sh \
    hack/ci-read-chart-version.sh \
    hack/ci-operator-upgrade-leg.sh \
    hack/ci-deploy-operator.sh \
    .github/workflows/e2e-operator-upgrade.yaml; do
    assert_contains "tests_operator_upgrade lists $path" "$shared" "- '$path'"
  done
  assert_not_contains "tests_operator_upgrade does not list every suite at once" \
    "$shared" "tests/e2e-operator-upgrade/**"
}

# ci_all_operators_from_ci_yaml — the ALL_OPERATORS of the ci.yaml resolve step.
ci_all_operators_from_ci_yaml() {
  sed -n 's/^          ALL_OPERATORS: //p' "$CI_YAML" | head -1
}

test_merge_operator_images_tags_the_chart_version() {
  echo "Test: merge-operator-images reads the chart version and tags the index with it on main"

  local job chart_at meta_at
  job="$(job_block merge-operator-images)"
  chart_at="$(grep -n -m1 '^      - name: Read the chart version$' <<<"$job" | cut -d: -f1)"
  meta_at="$(grep -n -m1 '^      - name: Generate image metadata$' <<<"$job" | cut -d: -f1)"
  assert_not_empty "the job has a Read the chart version step" "$chart_at"
  if [ -n "$chart_at" ] && [ -n "$meta_at" ] && [ "$chart_at" -lt "$meta_at" ]; then
    assert_eq "it runs before Generate image metadata" "yes" "yes"
  else
    assert_eq "it runs before Generate image metadata" "yes" "no"
  fi
  assert_contains "the step's id is chart" \
    "$(job_step merge-operator-images "Read the chart version")" "id: chart"
  assert_contains "the metadata step tags the chart version on the default branch" \
    "$(job_step merge-operator-images "Generate image metadata")" \
    'type=raw,value=${{ steps.chart.outputs.version }},enable={{is_default_branch}}'
}

test_ci_yaml_calls_the_reusable_workflow() {
  echo "Test: the e2e-operator-upgrade job of ci.yaml calls the reusable workflow"

  local job
  job="$(job_block e2e-operator-upgrade)"
  assert_contains "it uses the reusable workflow" "$job" \
    "    uses: ./.github/workflows/e2e-operator-upgrade.yaml"
  assert_contains "it passes the upgrade matrix" "$job" \
    'operators: ${{ needs.changes.outputs.e2e-operator-upgrade-operators }}'
  assert_contains "it passes the image map" "$job" \
    'image-map: ${{ needs.build-e2e-images.outputs.image-map }}'
  assert_contains "it keeps its own gate" "$job" \
    "needs.changes.outputs.e2e-operator-upgrade == 'true'"
  assert_contains "it runs on pull requests only" "$job" "github.event_name == 'pull_request'"
  assert_not_contains "it carries no steps of its own" "$job" "    steps:"
  assert_contains "the changes job exports the matrix" "$(job_block changes)" \
    'e2e-operator-upgrade-operators: ${{ steps.result.outputs.e2e-operator-upgrade-operators }}'
  # The run-scoped e2e images are pruned only after every job that pulls them
  # finished. The scan of tests/unit/ci/cleanup_e2e_tags_needs_test.sh reads
  # ci.yaml, where this job no longer names load-e2e-images.
  assert_contains "cleanup-e2e-tags still waits for the upgrade jobs" \
    "$(job_block cleanup-e2e-tags | sed -n '/^    needs:/,/^    [a-z]/p')" "e2e-operator-upgrade"
}

test_the_reusable_workflow_runs_the_pinned_baseline() {
  echo "Test: the reusable workflow takes its inputs and runs the leg table and the pinned baseline"

  local wf job
  wf="$(cat "$UPGRADE_YAML")"
  assert_contains "it is a called workflow" "$wf" "  workflow_call:"
  local input
  for input in operators image-map ref; do
    assert_contains "it declares the $input input" "$wf" "      ${input}:"
  done
  job="$(workflow_job "$UPGRADE_YAML" upgrade)"
  assert_contains "the matrix comes from the operators input" "$job" \
    'matrix: ${{ fromJson(inputs.operators) }}'
  assert_contains "it names each job after its operator" "$job" \
    'name: e2e-operator-upgrade (${{ matrix.operator }})'
  assert_contains "it checks out the ref input" "$job" 'ref: ${{ inputs.ref }}'
  assert_contains "it loads images through the image-map input" "$job" \
    'image-map: ${{ inputs.image-map }}'
  assert_contains "it loads the refs the leg table names" "$job" \
    'images: ${{ steps.leg.outputs.refs }}'
  assert_contains "it resolves the leg" "$job" "run: hack/ci-operator-upgrade-leg.sh"
  assert_contains "it fetches the baseline" "$job" "run: hack/ci-fetch-released-operator.sh"
  assert_contains "it deploys the pinned image tag" "$job" \
    'IMAGE_TAG: ${{ steps.baseline.outputs.image_tag }}'
  assert_contains "it deploys the pulled chart" "$job" \
    'CHART_DIR: ${{ steps.baseline.outputs.chart_dir }}'
  assert_contains "it runs the operator's suite" "$job" \
    'tests/e2e-operator-upgrade/${OPERATOR}-helm-upgrade/'
  assert_contains "the K-ORC clone gets the token" \
    "$(CI_YAML="$UPGRADE_YAML" job_step upgrade "Install CRDs watched by the c5c3-operator")" \
    'GITHUB_TOKEN: ${{ github.token }}'

  # A kind the c5c3-operator starts watching has to reach every copy of the
  # CRD list, or that job's informers never sync.
  local wf_crds job_crds crd_job
  wf_crds="$(CI_YAML="$UPGRADE_YAML" job_step upgrade "Install CRDs watched by the c5c3-operator" | run_block)"
  assert_contains "the c5c3 leg installs the K-ORC kinds" "$wf_crds" "hack/ci-deploy-korc.sh"
  for crd_job in e2e-operator e2e-orders-multicluster; do
    job_crds="$(job_step "$crd_job" "Install CRDs watched by the c5c3-operator" | run_block)"
    assert_not_empty "$crd_job installs the CRDs watched by the c5c3-operator" "$job_crds"
    assert_eq "the c5c3 leg installs the CRDs $crd_job installs" "$job_crds" "$wf_crds"
  done

  local ci_grants wf_grants
  ci_grants="$(sed -n 's/^ *BARBICAN_SECRET_STORE_GRANTS: //p' "$CI_YAML" | sort -u)"
  wf_grants="$(sed -n 's/^ *BARBICAN_SECRET_STORE_GRANTS: //p' "$UPGRADE_YAML")"
  assert_not_empty "the workflow sets the barbican grants" "$wf_grants"
  assert_eq "the workflow's barbican grants equal ci.yaml's" "$ci_grants" "$wf_grants"
}

test_the_make_target_deploys_what_the_fetch_wrote() {
  echo "Test: make e2e-operator-upgrade deploys the image tag and chart the fetch script wrote"

  if ! command -v make >/dev/null 2>&1; then
    echo "  SKIP: make not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi
  local recipe fetched grants
  recipe="$(make -s -n -C "$PROJECT_ROOT" e2e-operator-upgrade OPERATOR=glance)"
  fetched="_output/operator-upgrade/glance.fetched"
  grants="$(sed -n 's/^ *BARBICAN_SECRET_STORE_GRANTS: //p' "$UPGRADE_YAML")"
  # make's CURDIR is the physical path.
  assert_contains "the fetch writes its outputs to a file" "$recipe" \
    "GITHUB_OUTPUT=$(cd "$PROJECT_ROOT" && pwd -P)/$fetched OPERATOR=glance hack/ci-fetch-released-operator.sh"
  # A second parse of the pin drifts from the fetch script's: a single-quoted
  # pin once reached the deploy with its quotes.
  assert_contains "the deploy reads image_tag back" "$recipe" \
    "IMAGE_TAG=\"\$(sed -n 's/^image_tag=//p' $fetched)\""
  assert_contains "the deploy reads chart_dir back" "$recipe" \
    "CHART_DIR=\"\$(sed -n 's/^chart_dir=//p' $fetched)\""
  assert_not_contains "the recipe does not read the pin itself" "$recipe" "baseline.yaml"
  assert_contains "it deploys into glance-system" "$recipe" "NAMESPACE=glance-system"
  assert_contains "it passes the barbican grants the workflow passes" "$recipe" \
    "BARBICAN_SECRET_STORE_GRANTS=${grants} hack/ci-deploy-operator.sh"
  assert_contains "it runs the glance suite" "$recipe" "tests/e2e-operator-upgrade/glance-helm-upgrade/"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_every_chart_has_a_pinned_suite
test_the_pin_check_catches_a_bad_pin
test_every_operator_has_an_upgrade_filter
test_merge_operator_images_tags_the_chart_version
test_ci_yaml_calls_the_reusable_workflow
test_the_reusable_workflow_runs_the_pinned_baseline
test_the_make_target_deploys_what_the_fetch_wrote

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
