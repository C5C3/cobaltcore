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
# takes the mode and every scenario has to render. The e2e leg also has to ask
# for what the suites need: without WITH_NFS and WITH_MESSAGING in the setup
# step's env block they find no NFS export and no broker, and without the cinder
# arm of the chainsaw narrowing they run four at a time on a node that fits two.
#
# The e2e-chaos network leg carries the same kind of silence one job over. It
# enumerates its test directories by hand, so a cinder chaos suite absent from
# that list is lint-checked and never applied to a cluster, and the images, the
# operator deploy and the same two infrastructure flags are the rest of what
# those suites need: kind pulls nothing the run did not load, a Cinder whose
# operator never deployed sits without status until the suite times out, and a
# leg without the NFS export and the broker cannot bring one up at all.
#
# Three more filters decide what a pull request that leaves operators/cinder/
# alone still runs. image_cinder is what puts the service image into
# changed-services, so an edit under images/cinder/ or patches/cinder/ is
# built and tested instead of pulled from main. tests_e2e_cinder is what
# schedules the leg for an edit that touches only the suites. tempest_cinder
# is declared and handed to the resolver but narrows nothing yet:
# hack/ci-resolve-changes.sh keeps cinder out of TEMPEST_ALL_SERVICES until
# the tests/tempest/cinder-* directories exist, and
# tests/unit/ci/change_classes_wiring_test.sh exempts it from the steering
# check for that reason.
#
# The last signal sits in .github/workflows/build-images.yaml: the option
# catalog check re-derives operators/cinder/api/v1alpha1/catalogs/<release>.json
# from the image it just built, and it runs only where the step's `if:` names
# cinder and where the workflow's trigger paths and its svc_cinder filter carry
# the catalog directory, so a catalog that no longer matches the image reaches
# main unchecked as soon as one of those four places drops the service.
#
# Usage: bash tests/unit/ci/cinder_e2e_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
BUILD_IMAGES_YAML="$PROJECT_ROOT/.github/workflows/build-images.yaml"

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

test_cinder_image_filter_is_wired() {
  echo "Test: an images/cinder change reaches changed-services"

  # changed-services is the list build-e2e-images rebuilds from source. An
  # image edit missing from it leaves the cinder leg loading whatever main
  # last published, so the suites pass against the old image and the change
  # under review is never run. patches/cinder/ belongs in the same filter:
  # hack/ci-build-service-image.sh applies patches/<op>/<release>/*.patch into
  # the source it builds, so a patch edited on its own changes that image.
  assert_filter_is_wired image_cinder changed-services

  local block
  block=$(filter_block image_cinder)
  assert_contains "the image build context is covered" "$block" "images/cinder/**"
  assert_contains "the source patches are covered" "$block" "patches/cinder/**"

  assert_eq "an image change rebuilds cinder and nothing else" \
    'changed-services=["cinder"]' \
    "$(resolve_output changed-services refs/heads/main "$ALL_OPS" \
      FILTER_image_cinder=true)"
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

test_cinder_e2e_filter_is_wired() {
  echo "Test: an edit to the cinder suites alone runs the cinder leg"

  # The suites sit in two directories and the e2e-operator job runs both:
  # tests/e2e/cinder/ holds the per-CR tests, tests/e2e/cinder-operator/ the
  # operator-level ones. A filter naming only the first lets an edit under the
  # second schedule no leg at all, and the suite it changed goes unrun.
  assert_filter_is_wired tests_e2e_cinder e2e-operators

  local block
  block=$(filter_block tests_e2e_cinder)
  assert_contains "the per-CR suites are covered" "$block" "tests/e2e/cinder/**"
  assert_contains "the operator-level suites are covered" "$block" \
    "tests/e2e/cinder-operator/**"

  # And that leg only: editing one operator's suites is no reason to spend a
  # runner on the other eight.
  assert_eq "a suite edit runs the cinder leg alone" \
    'e2e-operators={"operator":["cinder"]}' \
    "$(resolve_output e2e-operators refs/heads/main "$ALL_OPS" \
      FILTER_tests_e2e_cinder=true)"
}

test_cinder_leg_opts_into_nfs_and_messaging() {
  echo "Test: the cinder e2e leg asks for the NFS export and the broker"

  # Every cinder suite mounts its volumes as inline CSI volumes from the kind
  # NFS export and takes its own vhost on the shared broker. deploy-infra.sh
  # installs neither by default and setup-e2e-infra reads both flags from env,
  # so the values have to sit in this step's own env block; anywhere else the
  # suites come up against a missing StorageClass and an unreachable transport.
  local setup
  setup=$(job_step e2e-operator "Setup E2E infrastructure")

  assert_contains "the step still uses the shared composite action" "$setup" \
    "uses: ./.github/actions/setup-e2e-infra"
  assert_contains "the cinder leg opts into the NFS stack" "$setup" \
    "WITH_NFS: \${{ matrix.operator == 'cinder' && 'true' || '' }}"
  assert_contains "and into the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ matrix.operator == 'cinder' && 'true' || '' }}"

  # Sixteen suites of three or four Deployments, a db-sync Job and a probe pod
  # each do not run four at a time on one kind node: at the shared config's
  # parallel: 4 the Pods stay Pending on "Insufficient cpu". Read the condition
  # together with its body, so a cinder arm on a branch that no longer narrows
  # anything does not pass. The neutron leg keeps its own narrowing.
  local narrowing
  narrowing=$(job_step e2e-operator "Run E2E tests" | awk '
    /^ *parallel=\(\)$/ { in_block = 1; next }
    in_block && /^ *fi$/ { exit }
    in_block { print }
  ')

  assert_not_empty "the chainsaw run step narrows the parallelism" "$narrowing"
  assert_contains "the cinder leg is one of the narrowed ones" "$narrowing" \
    '[ "${OPERATOR}" = "cinder" ]'
  assert_contains "it runs two suites at a time" "$narrowing" \
    "parallel=(--parallel 2)"
  assert_contains "the neutron narrowing is kept" "$narrowing" \
    '[ "${OPERATOR}" = "neutron" ]'
}

test_chaos_network_leg_runs_the_cinder_suites() {
  echo "Test: the e2e-chaos network leg runs both cinder chaos suites"

  # e2e-chaos enumerates test_dirs per leg (chainsaw's include/exclude-regex
  # flags are no-ops in v0.2.14), so a suite missing from the list is
  # lint-checked and never applied to a cluster.
  local entry
  entry=$(e2e_chaos_matrix_entry network)

  assert_not_empty "the network leg exists" "$entry"
  assert_contains "it runs the operator pod-kill suite" "$entry" \
    "tests/e2e-chaos/cinder-operator-pod-kill"
  assert_contains "it runs the broker outage suite" "$entry" \
    "tests/e2e-chaos/cinder-broker-outage"

  local load
  load=$(job_step e2e-chaos "Load E2E images")
  assert_contains "the leg pulls the cinder-operator image" "$load" \
    "matrix.suite == 'network' && format('{0}/cinder-operator:dev', env.IMAGE_PREFIX)"
  assert_contains "the leg pulls the cinder service image" "$load" \
    "matrix.suite == 'network' && format('{0}/cinder:2025.2', env.IMAGE_PREFIX)"

  local kind_load
  kind_load=$(job_step e2e-chaos "Load cinder images into kind")
  assert_not_empty "both images reach the node" "$kind_load"
  assert_contains "the load runs on the network leg alone" "$kind_load" \
    "if: matrix.suite == 'network'"
  assert_contains "the operator image is loaded" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/cinder-operator:dev"
  assert_contains "the service image is loaded" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/cinder:2025.2"

  # Both suites attach an NFS backend and cinder-operator-pod-kill takes a
  # vhost on the shared broker, so this leg needs the same two opt-ins the
  # e2e-operator cinder leg does. deploy-infra.sh installs neither by default.
  local setup
  setup=$(job_step e2e-chaos "Setup E2E infrastructure")
  assert_contains "the chaos leg opts into the NFS stack" "$setup" \
    "WITH_NFS: \${{ matrix.suite == 'network' && 'true' || '' }}"
  assert_contains "and into the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ matrix.suite == 'network' && 'true' || '' }}"

  local deploy
  deploy=$(job_step e2e-chaos "Deploy cinder operator")
  assert_not_empty "the cinder-operator is deployed" "$deploy"
  assert_contains "the deploy runs on the network leg alone" "$deploy" \
    "if: matrix.suite == 'network'"
  assert_contains "it goes through the shared deploy script" "$deploy" \
    "run: hack/ci-deploy-operator.sh"
  assert_contains "it deploys the cinder operator" "$deploy" "OPERATOR: cinder"
  assert_contains "it uses the run-tagged cinder-operator image" "$deploy" \
    "IMAGE_PREFIX }}/cinder-operator"
  # cinder-operator-pod-kill selects and kills the operator pod by
  # `-n cinder-system` and its PodChaos targets that namespace, so the script's
  # keystone-system default would leave the selector finding nothing.
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: cinder-system"

  # And the blocking pod leg stays out of it: neither suite runs there, so it
  # gains no NFS export, no broker and no cinder-operator.
  local pod_entry
  pod_entry=$(e2e_chaos_matrix_entry pod)
  assert_not_empty "the pod leg exists" "$pod_entry"
  assert_not_contains "the pod leg runs no cinder suite" "$pod_entry" \
    "tests/e2e-chaos/cinder-"
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

test_build_images_verifies_the_cinder_catalog() {
  echo "Test: build-images.yaml checks the cinder option catalog"

  # Two `Verify option catalog` steps run the check, one on the pull-request
  # build and one on the push build, and both list the services by name.
  local gates
  gates=$(grep -A1 -F "name: Verify option catalog" "$BUILD_IMAGES_YAML" |
    grep -F "if:")
  assert_eq "both catalog gates run for cinder" "2" \
    "$(printf '%s\n' "$gates" | grep -cF "matrix.service == 'cinder'")"

  # A catalog edited on its own has to start the workflow at all: the two
  # trigger lists decide that, and the svc_cinder filter decides whether the
  # cinder image is among the ones the run builds and checks.
  assert_eq "the push and pull_request triggers list the cinder catalogs" "2" \
    "$(grep -cF -- "- operators/cinder/api/v1alpha1/catalogs/**" \
      "$BUILD_IMAGES_YAML")"

  local filter
  filter=$(awk '
    $0 == "            svc_cinder:" { in_block = 1; next }
    in_block && /^            [a-z0-9_]+:$/ { exit }
    in_block { print }
  ' "$BUILD_IMAGES_YAML")
  assert_contains "the svc_cinder filter covers the cinder catalogs" \
    "$filter" "operators/cinder/api/v1alpha1/catalogs/**"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_cinder
test_service_operators_lists_cinder
test_cinder_filter_is_wired
test_cinder_image_filter_is_wired
test_cinder_change_produces_an_e2e_leg
test_cinder_e2e_filter_is_wired
test_cinder_leg_opts_into_nfs_and_messaging
test_chaos_network_leg_runs_the_cinder_suites
test_helm_filter_covers_the_cinder_chart
test_helm_validate_renders_the_cinder_chart
test_go_matrices_list_cinder
test_cleanup_matrices_cover_the_cinder_images
test_a_keystone_only_change_produces_no_cinder_leg
test_build_images_verifies_the_cinder_catalog

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
