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
# narrows the tempest matrix to the cinder legs for an edit under
# tests/tempest/cinder-*/, which it can do now that cinder is one of
# hack/ci-resolve-changes.sh's TEMPEST_ALL_SERVICES.
#
# The two tempest legs are the last piece, and they are silent in the same way
# the e2e leg is: the images the run tagged, the six operator deploys, the
# catalog Job that registers the block-storage, image, compute, placement and
# network services, the Glance the volume tests create their volumes from, the
# three Cinder CRs, the seed image the suite boots from, and the K8S_NAME
# variables that gate the port-forwards. On top of those the leg carries a
# compute stack, for the compute-tagged tests that attach a volume to a server
# and boot from one: an OVNCentral, a Neutron, a Placement and a Nova with one
# fake compute, deployed by four more operators. Any one of them missing leaves
# the leg burning its wait on a resource that never comes up, or every volume
# test failing to reach an API.
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
# shellcheck source=tests/lib/nova_api_budget.sh
source "$PROJECT_ROOT/tests/lib/nova_api_budget.sh"

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

# conf_section <tempest.conf> <section>
# Echo the key = value lines of one tempest.conf section, comments and blank
# lines dropped. The section ends at the next [header] or at end of file.
# Reading the section rather than the whole file keeps a key that moved into
# another group from answering for this one.
conf_section() {
  awk -v want="[$2]" '
    $0 == want { in_s = 1; next }
    in_s && /^\[/ { exit }
    in_s && /^[[:space:]]*#/ { next }
    in_s && /^[[:space:]]*$/ { next }
    in_s { print }
  ' "$1"
}

# conf_value <tempest.conf> <section> <key>
# Echo the value of one key of one section, or nothing when the section does
# not set it.
conf_value() {
  conf_section "$1" "$2" | sed -n "s/^$3 *= *//p" | head -1
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
  # The broker line is shared with the nova leg, whose Nova processes dial the
  # same bus.
  local setup
  setup=$(job_step e2e-operator "Setup E2E infrastructure")

  assert_contains "the step still uses the shared composite action" "$setup" \
    "uses: ./.github/actions/setup-e2e-infra"
  assert_contains "the cinder leg opts into the NFS stack" "$setup" \
    "WITH_NFS: \${{ matrix.operator == 'cinder' && 'true' || '' }}"
  assert_contains "and into the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ (matrix.operator == 'cinder' || matrix.operator == 'nova') && 'true' || '' }}"

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
  echo "Test: the e2e-chaos network leg runs the three cinder chaos suites"

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
  assert_contains "it runs the NFS outage suite" "$entry" \
    "tests/e2e-chaos/cinder-nfs-outage"

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

  # All three suites attach an NFS backend, cinder-nfs-outage scales that
  # export away and back, and two of them take a vhost on the shared broker,
  # so this leg needs the same two opt-ins the e2e-operator cinder leg does.
  # deploy-infra.sh installs neither by default. The broker line is shared with
  # the nova leg, whose suites take vhosts of their own, so the whole
  # expression is read here: an arm dropped from it leaves this leg's two
  # brokered suites waiting out their timeouts.
  local setup
  setup=$(job_step e2e-chaos "Setup E2E infrastructure")
  assert_contains "the chaos leg opts into the NFS stack" "$setup" \
    "WITH_NFS: \${{ matrix.suite == 'network' && 'true' || '' }}"
  assert_contains "and into the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ (matrix.suite == 'network' || matrix.suite == 'nova') && 'true' || '' }}"

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

  # And the blocking pod leg stays out of it: no cinder suite runs there, so
  # it gains no NFS export, no broker and no cinder-operator.
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

test_cinder_tempest_filter_is_wired() {
  echo "Test: an edit to the cinder tempest configs runs the cinder legs alone"

  # tempest_src switches the tempest job on and the per-service filter narrows
  # the matrix; an edit under tests/tempest/cinder-*/ matches both. Without the
  # narrowing the two cinder legs would only ever run as part of the full
  # five-service matrix, and an edit to their config would spend a runner on
  # keystone, glance, barbican and neutron as well.
  assert_file_contains "the paths filter exists" "$CI_YAML" \
    "^ *tempest_cinder:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_tempest_cinder: \${{ steps.filter.outputs.tempest_cinder }}"
  # tempest-services is consumed inside the changes job rather than exported:
  # it is what the matrix generator step narrows its output by.
  assert_file_contains "the resolver's selection reaches the generator" \
    "$CI_YAML" \
    "TEMPEST_SERVICES: \${{ join(fromJson(steps.result.outputs.tempest-services), ' ') }}"

  local block
  block=$(filter_block tempest_cinder)
  assert_contains "both release directories are covered" "$block" \
    "tests/tempest/cinder-*/**"

  assert_contains "the pair narrows the matrix to cinder" \
    "$(resolve_outputs refs/heads/main "$ALL_OPS" \
      FILTER_tempest_src=true FILTER_tempest_cinder=true)" \
    'tempest-services=["cinder"]'

  # And cinder is in the unnarrowed set: a change to the runner or the tempest
  # image has to reach the cinder legs too.
  assert_contains "a shared tempest change runs the cinder legs" \
    "$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_tempest_src=true)" \
    '"cinder"'
}

test_tempest_cinder_leg_is_wired() {
  echo "Test: the tempest cinder leg brings up the stack its suites need"

  # The four image refs are what the run tag makes available: an entry missing
  # here leaves the leg pulling the published tag instead of this PR's build.
  local load glance_load
  load=$(job_step tempest "Load E2E images")
  assert_contains "the leg pulls the cinder-operator image" "$load" \
    "matrix.service == 'cinder' && format('{0}/cinder-operator:dev', env.IMAGE_PREFIX)"
  assert_contains "the leg pulls the cinder service image" "$load" \
    "matrix.service == 'cinder' && format('{0}/cinder:{1}', env.IMAGE_PREFIX, matrix.release)"

  glance_load=$(job_step tempest "Load Glance E2E images")
  assert_contains "the glance pull covers the cinder leg" "$glance_load" \
    "if: matrix.service == 'glance' || matrix.service == 'cinder' || matrix.service == 'nova'"

  # The catalog Job and the image-seed Job run in-cluster, so the tempest image
  # has to be on the node, and so do the four workload images.
  local kind_load
  kind_load=$(job_step tempest "Load cinder images into kind")
  assert_not_empty "the images reach the node" "$kind_load"
  assert_contains "the load runs on the cinder leg alone" "$kind_load" \
    "if: matrix.service == 'cinder'"
  local image
  for image in cinder-operator:dev "cinder:\${{ matrix.release }}" \
    glance-operator:dev "glance:\${{ matrix.release }}" \
    "tempest:\${{ matrix.release }}"; do
    assert_contains "the node gets $image" "$kind_load" \
      "kind load docker-image \${{ env.IMAGE_PREFIX }}/$image"
  done

  # Both operators: the Cinder CRs need the cinder-operator, and the Glance the
  # volume tests create images from needs the glance-operator.
  local cinder_deploy glance_deploy
  cinder_deploy=$(job_step tempest "Deploy cinder operator")
  assert_not_empty "the cinder-operator is deployed" "$cinder_deploy"
  assert_contains "the cinder deploy runs on the cinder leg alone" \
    "$cinder_deploy" "if: matrix.service == 'cinder'"
  assert_contains "it deploys the cinder operator" "$cinder_deploy" \
    "OPERATOR: cinder"
  assert_contains "it lands in its own Namespace" "$cinder_deploy" \
    "NAMESPACE: cinder-system"

  glance_deploy=$(job_step tempest "Deploy glance operator")
  assert_contains "the glance deploy covers the cinder leg" "$glance_deploy" \
    "if: matrix.service == 'glance' || matrix.service == 'cinder' || matrix.service == 'nova'"

  # deploy-infra.sh installs neither the NFS stack nor the broker by default,
  # and setup-e2e-infra reads both flags from env, so the values have to sit in
  # this step's own env block: without them the CinderBackend finds no export
  # and the Cinder waits out its 600s on an unreachable transport.
  local setup
  setup=$(job_step tempest "Setup E2E infrastructure")
  assert_contains "the cinder leg opts into the NFS stack" "$setup" \
    "WITH_NFS: \${{ matrix.service == 'cinder' && 'true' || '' }}"
  assert_contains "and into the shared broker" "$setup" \
    "WITH_MESSAGING: \${{ (matrix.service == 'cinder' || matrix.service == 'nova') && 'true' || '' }}"

  local catalog glance_cr cinder_cr seed
  catalog=$(job_step tempest "Bootstrap block-storage catalog")
  assert_contains "the catalog Job is applied" "$catalog" \
    "01-catalog-setup-job.yaml"
  assert_contains "the leg waits for it to complete" "$catalog" \
    "job/cinder-tempest-catalog-setup"

  glance_cr=$(job_step tempest "Deploy Glance CR for the cinder leg")
  assert_contains "the Glance CR is applied" "$glance_cr" "02-glance-cr.yaml"
  assert_contains "its default store is applied" "$glance_cr" \
    "03-glancebackend-cr.yaml"
  assert_contains "the leg waits on the Glance the matrix names" "$glance_cr" \
    "kubectl wait glance/\${{ matrix.glance-cr-name }}"

  cinder_cr=$(job_step tempest "Deploy Cinder CR for Tempest")
  assert_contains "the volume backend is applied" "$cinder_cr" \
    "04-cinderbackend-cr.yaml"
  assert_contains "the backup target is applied" "$cinder_cr" \
    "05-cinderbackupbackend-cr.yaml"
  assert_contains "the Cinder CR is applied" "$cinder_cr" "06-cinder-cr.yaml"
  assert_contains "the leg waits on the CR the matrix names" "$cinder_cr" \
    "kubectl wait cinder/\${{ matrix.cinder-cr-name }}"
  assert_contains "the db-sync and the four Deployments fit inside the wait" \
    "$cinder_cr" "--timeout=600s"

  seed=$(job_step tempest "Seed the image the volume tests boot from")
  assert_contains "the seed Job is applied" "$seed" "07-image-seed-job.yaml"
  assert_contains "the leg waits for it to complete" "$seed" \
    "job/cinder-tempest-image-seed"

  # Both port-forwards: CINDER_K8S_NAME gates the 8776 one, without which every
  # volume test fails to reach the API, and GLANCE_K8S_NAME the 9292 one the
  # create-from-image cases need.
  local run_step
  run_step=$(job_step tempest "Run Tempest API tests")
  assert_contains "the runner learns the Cinder Service name" "$run_step" \
    "CINDER_K8S_NAME: \${{ matrix.cinder-cr-name }}"
  assert_contains "and the Glance Service name" "$run_step" \
    "GLANCE_K8S_NAME: \${{ matrix.glance-cr-name }}"

  # The generator's narrowed worker count only reaches stestr through this
  # env line. Dropped, matrix.tempest-concurrency resolves to "" and
  # ci-run-tempest.sh falls back to its default of four workers against a node
  # that already carries the broker, the NFS server, four Cinder workloads and
  # a Glance — the starvation the neutron leg recorded before it narrowed.
  assert_contains "the runner gets the narrowed worker count" "$run_step" \
    "TEMPEST_CONCURRENCY: \${{ matrix.tempest-concurrency }}"

  # The order is the dependency chain: the catalog entry has to exist before
  # the Glance and the Cinder reconcile against it, the Glance before the seed
  # image is uploaded to it, and the seed image before the suite starts.
  local job catalog_at glance_at cinder_at seed_at run_at
  job=$(job_block tempest)
  step_line() {
    printf '%s\n' "$job" | grep -nF "name: $1" | head -1 | cut -d: -f1
  }
  catalog_at=$(step_line "Bootstrap block-storage catalog")
  glance_at=$(step_line "Deploy Glance CR for the cinder leg")
  cinder_at=$(step_line "Deploy Cinder CR for Tempest")
  seed_at=$(step_line "Seed the image the volume tests boot from")
  run_at=$(step_line "Run Tempest API tests")
  assert_eq "catalog, then Glance, then Cinder, then seed, then run" "yes" \
    "$([ -n "$catalog_at" ] && [ -n "$glance_at" ] && [ -n "$cinder_at" ] &&
       [ -n "$seed_at" ] && [ -n "$run_at" ] &&
       [ "$catalog_at" -lt "$glance_at" ] && [ "$glance_at" -lt "$cinder_at" ] &&
       [ "$cinder_at" -lt "$seed_at" ] && [ "$seed_at" -lt "$run_at" ] &&
       echo yes || echo no)"
}

# Each CR name the tempest cinder leg waits on is generated, and each names a
# static fixture in the same config directory. Every assertion above pins one of
# those literals against another copy of itself, so a renamed metadata.name in
# any fixture leaves them green while the leg burns its wait on a resource that
# does not exist. This test reads the names out of the fixtures and requires the
# generator to agree with them.
test_matrix_cr_names_match_the_cinder_fixtures() {
  echo "Test: the matrix names the CRs the cinder tempest fixtures create"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local out matrix legs
  out=$(mktemp)
  # The generator writes the matrix to GITHUB_OUTPUT and prints nothing else,
  # so its ::error:: line for a missing config directory reaches this output.
  GITHUB_OUTPUT="$out" bash "$PROJECT_ROOT/hack/ci-generate-tempest-matrix.sh"
  matrix=$(sed -n 's/^tempest-releases=//p' "$out")
  rm -f "$out"

  legs=$(printf '%s' "$matrix" | jq -r '.include[]
    | select(.service == "cinder")
    | [."config-dir", ."cinder-cr-name", ."glance-cr-name", ."cr-name",
       ."tempest-concurrency", ."nova-cr-name", ."ovn-cr-name",
       ."neutron-cr-name", ."placement-cr-name"] | @tsv')
  assert_not_empty "the generator emits at least one cinder leg" "$legs"

  local config_dir cinder_emitted glance_emitted keystone_emitted concurrency fixture_name
  local nova_emitted ovn_emitted neutron_emitted placement_emitted
  while IFS=$'\t' read -r config_dir cinder_emitted glance_emitted keystone_emitted \
    concurrency nova_emitted ovn_emitted neutron_emitted placement_emitted; do
    [ -n "$config_dir" ] || continue
    fixture_name=$(yq -r '.metadata.name' \
      "$PROJECT_ROOT/$config_dir/06-cinder-cr.yaml")
    assert_eq "$config_dir waits on the Cinder its fixture creates" \
      "$fixture_name" "$cinder_emitted"
    assert_eq "$config_dir attaches its volume backend to that Cinder" \
      "$fixture_name" \
      "$(yq -r '.spec.cinderRef.name' \
        "$PROJECT_ROOT/$config_dir/04-cinderbackend-cr.yaml")"
    assert_eq "$config_dir attaches its backup target to that Cinder" \
      "$fixture_name" \
      "$(yq -r '.spec.cinderRef.name' \
        "$PROJECT_ROOT/$config_dir/05-cinderbackupbackend-cr.yaml")"
    assert_eq "$config_dir waits on the Glance its fixture creates" \
      "$(yq -r '.metadata.name' "$PROJECT_ROOT/$config_dir/02-glance-cr.yaml")" \
      "$glance_emitted"
    assert_eq "$config_dir waits on the Keystone its fixture creates" \
      "$(yq -r '.metadata.name' "$PROJECT_ROOT/$config_dir/00-keystone-cr.yaml")" \
      "$keystone_emitted"
    # The key the run step reads. Nothing else holds this value, and its
    # absence is silent: the leg would run at ci-run-tempest.sh's default of
    # four workers and fail on node starvation rather than on the change under
    # test.
    assert_eq "$config_dir runs at two stestr workers" "2" "$concurrency"

    # The compute stack the compute-tagged volume tests boot a server on. Its
    # four names reach the workflow the same way the three above do, and fail
    # the same way: a rename in any of these fixtures leaves the leg waiting out
    # its 900s on a CR that does not exist.
    assert_eq "$config_dir waits on the Nova its fixture creates" \
      "$(yq -r '.metadata.name' "$PROJECT_ROOT/$config_dir/13-nova-cr.yaml")" \
      "$nova_emitted"
    assert_eq "$config_dir waits on the OVNCentral its fixture creates" \
      "$(yq -r '.metadata.name' \
        "$PROJECT_ROOT/$config_dir/09-ovncentral-cr.yaml")" "$ovn_emitted"
    assert_eq "$config_dir waits on the Neutron its fixture creates" \
      "$(yq -r '.metadata.name' \
        "$PROJECT_ROOT/$config_dir/10-neutron-cr.yaml")" "$neutron_emitted"
    assert_eq "$config_dir waits on the Placement its fixture creates" \
      "$(yq -r '.metadata.name' \
        "$PROJECT_ROOT/$config_dir/11-placement-cr.yaml")" "$placement_emitted"

    # And the fixtures on each other: a Neutron pointed at another OVNCentral
    # renders no ml2_conf.ini and never reaches Ready, and a messaging Secret
    # under another name leaves the same CR waiting on a transport URL nothing
    # wrote.
    assert_eq "$config_dir points its Neutron at that OVNCentral" \
      "$ovn_emitted" \
      "$(yq -r '.spec.ovn.centralRef.name' \
        "$PROJECT_ROOT/$config_dir/10-neutron-cr.yaml")"
    assert_eq "$config_dir points its Neutron at the messaging Secret beside it" \
      "$(yq -r '.metadata.name' \
        "$PROJECT_ROOT/$config_dir/08-messaging-secret.yaml")" \
      "$(yq -r '.spec.messaging.secretRef.name' \
        "$PROJECT_ROOT/$config_dir/10-neutron-cr.yaml")"
    assert_eq "$config_dir points its Nova at the metadata Secret beside it" \
      "$(yq -r '.metadata.name' \
        "$PROJECT_ROOT/$config_dir/12-metadata-secret.yaml")" \
      "$(yq -r '.spec.metadata.sharedSecretRef.name' \
        "$PROJECT_ROOT/$config_dir/13-nova-cr.yaml")"

    assert_eq "$config_dir names the Deployment the workflow rolls out" \
      "${nova_emitted}-fake-compute" \
      "$(yq -r 'select(.kind == "Deployment") | .metadata.name' \
        "$PROJECT_ROOT/$config_dir/14-fake-compute.yaml")"
    # fake-1 as a literal, which is the host the workflow hands
    # discover-hosts.sh on this leg. The nova legs take the node's name from a
    # fieldRef instead, because their chassis registers under it; this leg
    # deploys no chassis, so a fieldRef here would only make the two sides
    # disagree.
    assert_eq "$config_dir registers its compute as fake-1" "fake-1" \
      "$(yq -r 'select(.kind == "Deployment")
        | .spec.template.spec.containers[].env[]
        | select(.name == "OS_DEFAULT__HOST") | .value // ""' \
        "$PROJECT_ROOT/$config_dir/14-fake-compute.yaml")"
    assert_eq "$config_dir reads that host from no field of the pod" "" \
      "$(yq -r 'select(.kind == "Deployment")
        | .spec.template.spec.containers[].env[]
        | select(.name == "OS_DEFAULT__HOST") | .valueFrom.fieldRef.fieldPath // ""' \
        "$PROJECT_ROOT/$config_dir/14-fake-compute.yaml")"

    # The workflow waits on job/${{ matrix.service }}-tempest-flavor-seed, so
    # this leg's Job carries the service name rather than nova's.
    assert_eq "$config_dir names the flavor-seed Job the workflow waits on" \
      "cinder-tempest-flavor-seed" \
      "$(yq -r '.metadata.name' \
        "$PROJECT_ROOT/$config_dir/15-flavor-seed-job.yaml")"

    # No chassis and no metadata agent, by decision: this leg's servers carry no
    # NIC, so nothing binds a port and nothing reads 169.254.169.254. A fixture
    # for either would be applied by no step and reconciled by nobody.
    assert_eq "$config_dir brings up no OVN chassis" "" \
      "$(grep -rl '^kind: OVNChassis$' "$PROJECT_ROOT/$config_dir")"
    assert_eq "$config_dir brings up no metadata agent" "" \
      "$(grep -rl '^kind: NeutronMetadataAgent$' "$PROJECT_ROOT/$config_dir")"
  done <<< "$legs"
}

# Both cinder tempest.conf files are written around the compute stack the leg
# deploys: the compute-tagged volume tests (attach, boot-from-volume) run rather
# than skip, and so does test_incremental_backup, which boots a server of its
# own. Each of those switches is a line in a file nothing else reads, and a flag
# that flips turns the tests it gates into skips. A skipped test is reported as
# a pass.
test_cinder_tempest_conf_runs_against_a_nova() {
  echo "Test: each cinder tempest.conf drives the compute stack the leg deploys"

  local dir name conf flavor_id flavor_alt_id slug excl patterns
  for dir in "$PROJECT_ROOT"/tests/tempest/cinder-*/; do
    name=$(basename "$dir")
    conf="${dir}tempest.conf"
    slug="${name#cinder-}"

    # The three services the compute-tagged cases reach. nova is what takes them
    # out of skip, neutron is what lets tempest's dynamic credentials delete
    # each project's default security group, and placement is where that Nova
    # claims its inventory.
    assert_eq "$name runs against a nova" "true" \
      "$(conf_value "$conf" service_available nova)"
    assert_eq "$name runs against a neutron" "true" \
      "$(conf_value "$conf" service_available neutron)"
    assert_eq "$name runs against a placement" "true" \
      "$(conf_value "$conf" service_available placement)"

    # A fake-driver server has no guest to ssh into, and
    # test_incremental_backup asks create_server for SSHABLE: with validation
    # off tempest waits for ACTIVE alone, which is how the Phase-0 lab of #1015
    # ran that test.
    assert_eq "$name does not validate a server over ssh" "false" \
      "$(conf_value "$conf" validation run_validation)"

    # Two flags that stay off. The volume suites bind no port, so tempest passes
    # no network and nova boots the server without a NIC; switching the networks
    # on would make every boot wait on a port this leg has no chassis to bind.
    # Extending an attached volume is unproven against the NFS driver here.
    assert_eq "$name boots its servers without a NIC" "false" \
      "$(conf_value "$conf" auth create_isolated_networks)"
    assert_eq "$name leaves extend-while-attached off" "false" \
      "$(conf_value "$conf" volume-feature-enabled extend_attached_volume)"

    # One fake compute, so the multi-node classes skip on this value.
    assert_eq "$name runs one compute" "1" \
      "$(conf_value "$conf" compute min_compute_nodes)"

    # image_ref is held against 07-image-seed-job.yaml by
    # test_cinder_fixtures_agree_on_the_seed_image above. The two flavors are
    # the pair 15-flavor-seed-job.yaml creates, and they fail the same way: the
    # Job ends on a listing, so `kubectl wait --for=condition=complete` goes
    # green while every boot 404s on a flavor nobody registered.
    flavor_id=$(sed -n 's/^ *FLAVOR_ID="\(.*\)"/\1/p' \
      "${dir}15-flavor-seed-job.yaml" | head -1)
    flavor_alt_id=$(sed -n 's/^ *FLAVOR_ALT_ID="\(.*\)"/\1/p' \
      "${dir}15-flavor-seed-job.yaml" | head -1)
    assert_not_empty "$name declares a FLAVOR_ID" "$flavor_id"
    assert_eq "$name seeds the flavor its tempest.conf boots on" \
      "$flavor_id" "$(conf_value "$conf" compute flavor_ref)"
    assert_eq "$name seeds the larger flavor beside it" \
      "$flavor_alt_id" "$(conf_value "$conf" compute flavor_ref_alt)"

    # test_incremental_backup boots a server to write into its volume, and this
    # leg has a compute for it. A line for it here would drop it into the skip
    # column without anyone noticing.
    excl="${dir}exclude-tests.txt"
    assert_eq "$name runs test_incremental_backup" "0" \
      "$(grep -c test_incremental_backup "$excl")"
    patterns=$(grep -vE '^[[:space:]]*(#|$)' "$excl")
    assert_eq "$name excludes eight tests" "8" "$(grep -c . <<<"$patterns")"
    assert_eq "$name excludes six plugin snapshot tests" "6" \
      "$(grep -c '^cinder_tempest_plugin\\\.' <<<"$patterns")"
    # The other two back up a volume while it is attached, which the NFS
    # backend serves from a clone that needs an online snapshot through Nova,
    # and the fake driver has no volume_snapshot_create. Anchored on the
    # opening bracket so the plugin's test_incremental_backup stays in.
    assert_eq "$name excludes the two attached-volume backup tests" "2" \
      "$(grep -c '^tempest\\\.api\\\.volume\\\.test_volumes_backup\\\.VolumesBackupsTest\\\.test_\(backup_create_attached_volume\|volume_backup_incremental\)\\\[$' <<<"$patterns")"

    # The three catalog rows the compute stack is reached through. nova's own
    # clients read the internal interface and tempest the public one, and the
    # Job writes both off one URL per service.
    assert_file_contains "$name registers the compute service" \
      "${dir}01-catalog-setup-job.yaml" "create_service nova compute"
    assert_file_contains "$name registers the placement service" \
      "${dir}01-catalog-setup-job.yaml" "create_service placement placement"
    assert_file_contains "$name registers the network service" \
      "${dir}01-catalog-setup-job.yaml" "create_service neutron network"
    assert_file_contains "$name points the compute entry at its own Nova" \
      "${dir}01-catalog-setup-job.yaml" \
      "http://nova-cinder-tempest-${slug}.openstack.svc:8774/v2.1"
    assert_file_contains "$name points the placement entry at its own Placement" \
      "${dir}01-catalog-setup-job.yaml" \
      "http://placement-cinder-tempest-${slug}.openstack.svc:8778"
    assert_file_contains "$name points the network entry at its own Neutron" \
      "${dir}01-catalog-setup-job.yaml" \
      "http://neutron-cinder-tempest-${slug}.openstack.svc:9696"
    # cinder accepts nova-compute's attachment_delete only from a service
    # token carrying one of its service_token_roles ("service" by default).
    # Every CR of the leg names the bootstrap admin as its service user, and
    # bootstrap assigns that role to nobody, so the Job grants it; without it
    # every server delete leaves its volume in-use (409
    # ConflictNovaUsingAttachment in the nova-compute log).
    assert_file_contains "$name grants the service role to the service user" \
      "${dir}01-catalog-setup-job.yaml" \
      "openstack role add --user admin --user-domain Default"
    assert_file_contains "$name grants it on the admin project" \
      "${dir}01-catalog-setup-job.yaml" \
      "project admin --project-domain Default service"
  done
}

test_tempest_cinder_leg_brings_up_a_compute_stack() {
  echo "Test: the tempest cinder leg brings up the compute stack, minus the chassis"

  # The compute-stack steps are shared with the nova legs, and which of them run
  # here is decided by their `if:` alone. Widen one of the nova-only ones and
  # this leg deploys a chassis with no kernel modules loaded and an agent with
  # no chassis to read from, each waiting out its own 300s; narrow one of the
  # shared ones and the compute-tagged volume tests go back to skipping, which
  # is reported as a pass.
  local job
  job=$(job_block tempest)
  step_line() {
    printf '%s\n' "$job" | grep -nF "name: $1" | head -1 | cut -d: -f1
  }

  local seed_at run_at name at step
  seed_at=$(step_line "Seed the image the volume tests boot from")
  run_at=$(step_line "Run Tempest API tests")
  assert_not_empty "the leg still seeds its volume image" "$seed_at"
  assert_not_empty "and still runs tempest" "$run_at"

  for name in "Deploy OVNCentral for the compute stack" \
    "Deploy Neutron CR for the compute stack" "Deploy Placement CR" \
    "Deploy Nova CR" "Deploy the fake compute" "Seed the flavors"; do
    step=$(job_step tempest "$name")
    assert_eq "'${name}' runs on both compute-stack legs" \
      "if: matrix.service == 'nova' || matrix.service == 'cinder'" \
      "$(grep -E '^ *if:' <<<"$step" | sed 's/^ *//')"
    at=$(step_line "$name")
    assert_gte "'${name}' comes after the volume image is seeded" \
      "$at" "$seed_at"
    assert_gte "and before the suite starts" "$run_at" "$at"
  done

  # The OVNCentral and the Placement are applied before the leg's own chain and
  # waited on after it, so they reconcile through the Glance's 300s, the
  # Cinder's 600s and the seed Job's 300s instead of adding their own 300s each
  # to a wall that is already 120 minutes. Put this step back below the seed and
  # the leg pays for both serially again, with nothing failing to say so.
  local pre_apply pre_apply_at glance_at
  pre_apply=$(job_step tempest "Apply the independent compute-stack CRs")
  assert_not_empty "the leg applies those CRs up front" "$pre_apply"
  pre_apply_at=$(step_line "Apply the independent compute-stack CRs")
  glance_at=$(step_line "Deploy Glance CR for the cinder leg")
  assert_gte "the apply comes before the leg's own Glance" \
    "$glance_at" "$pre_apply_at"
  assert_gte "and after the catalog it authenticates against" \
    "$pre_apply_at" "$(step_line "Bootstrap block-storage catalog")"

  # The nova-only half. The chassis and the agent need a node with the OVS
  # kernel modules loaded, which this leg does not ask for; the Glance, its seed
  # images and the compute catalog are already covered by the leg's own steps.
  for name in "Bootstrap compute catalog" "Deploy Glance CR for the nova leg" \
    "Seed the images the compute tests boot from" "Deploy the OVN chassis" \
    "Deploy the metadata agent"; do
    assert_eq "'${name}' stays off the cinder leg" \
      "if: matrix.service == 'nova'" \
      "$(grep -E '^ *if:' <<<"$(job_step tempest "$name")" | sed 's/^ *//')"
  done

  # Without this the 8774 forward never opens and every compute-tagged volume
  # test fails to reach the API it boots its server through.
  assert_contains "the runner learns the Nova Service name" \
    "$(job_step tempest "Run Tempest API tests")" \
    "NOVA_K8S_NAME: \${{ matrix.nova-cr-name }}"

  # deploy-infra.sh modprobes openvswitch and geneve on the runner host under
  # this switch. This leg places no chassis, so the expression names nova alone
  # and the host's modules are left where they were.
  local modules
  modules=$(grep WITH_OVN_KERNEL_MODULES <<<"$(job_step tempest "Setup E2E infrastructure")")
  assert_not_empty "the kernel-module flag is readable" "$modules"
  assert_not_contains "the chassis modules stay off the cinder leg" \
    "$modules" "cinder"
}

test_cinder_tempest_crs_fit_their_uwsgi_workers() {
  echo "Test: each cinder leg's Nova CR fits the uWSGI workers it asks for"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local dir cr
  for dir in "$PROJECT_ROOT"/tests/tempest/cinder-*/; do
    for cr in "$dir"*-nova-cr.yaml; do
      assert_nova_api_fits_its_workers "$cr"
    done
  done
}

test_controlplane_leg_deploys_cinder() {
  echo "Test: the e2e-controlplane leg carries the block-storage service"

  # The full-ControlPlane suite drives a seventh service through the chain, and
  # every piece of that is wired in this one job. kind pulls nothing the run did
  # not load, the projected Cinder child needs a cinder-operator to drive it to
  # Ready, and the suite's own gate hard-fails on a leg that carries the CRDs
  # alone (E2E_REQUIRE_CONTROLPLANE_STACK=true).
  local load
  load=$(job_step e2e-controlplane "Load E2E images")
  assert_contains "the leg pulls the cinder-operator image" "$load" \
    "cinder-operator:dev"
  assert_contains "the leg pulls the cinder service image" "$load" \
    "cinder:2025.2"

  local kind_load
  kind_load=$(job_step e2e-controlplane "Load images into kind")
  assert_contains "the operator image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/cinder-operator:dev"
  assert_contains "the service image reaches the node" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/cinder:2025.2"

  # The suite mounts its volume and backup shares from the kind NFS export and
  # takes a vhost on the shared broker. deploy-infra.sh installs neither by
  # default and setup-e2e-infra reads both flags from env, so they have to sit in
  # this step's own env block.
  local setup
  setup=$(job_step e2e-controlplane "Setup E2E infrastructure")
  assert_contains "the leg opts into the NFS stack" "$setup" \
    "WITH_NFS: \"true\""
  assert_contains "and into the shared broker" "$setup" \
    "WITH_MESSAGING: \"true\""

  local deploy
  deploy=$(job_step e2e-controlplane "Deploy cinder-operator")
  assert_not_empty "the cinder-operator is deployed" "$deploy"
  assert_contains "it deploys the cinder operator" "$deploy" "OPERATOR: cinder"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: cinder-system"

  # Order is the load-bearing part: c5c3-operator projects the Cinder child and
  # its two satellite kinds, so the Cinder CRDs have to be served before it
  # starts. A deploy step that drifted below c5c3-operator would leave the first
  # projection passes failing on an unknown kind.
  local body neutron_at cinder_at c5c3_at
  body=$(job_block e2e-controlplane)
  neutron_at=$(grep -n "^      - name: Deploy neutron-operator$" <<< "$body" | cut -d: -f1)
  cinder_at=$(grep -n "^      - name: Deploy cinder-operator$" <<< "$body" | cut -d: -f1)
  c5c3_at=$(grep -n "^      - name: Deploy c5c3-operator$" <<< "$body" | cut -d: -f1)
  assert_not_empty "the job deploys neutron-operator" "$neutron_at"
  assert_not_empty "the job deploys c5c3-operator" "$c5c3_at"
  assert_gte "cinder-operator is deployed after neutron-operator" \
    "$cinder_at" "$neutron_at"
  assert_gte "and before c5c3-operator" "$c5c3_at" "$cinder_at"

  # The c5c3 dump reads cinder-system for nothing, so a failed run would carry no
  # cinder-operator log at all without a second dump of its own.
  local dump
  dump=$(job_step e2e-controlplane "Dump cinder diagnostic info")
  assert_contains "the failed run dumps the cinder-operator" "$dump" \
    "OPERATOR: cinder"
}

# Two release directories carry the seed image the volume suites boot from, and
# each carries it twice: 07-image-seed-job.yaml registers the UUID and
# tempest.conf points compute.image_ref at it. Nothing holds the two together.
# The seed Job ends in an `image show` on its own SEED_ID, so it succeeds and
# CI's `kubectl wait --for=condition=complete` goes green while every
# create-volume-from-image case 404s on an image nobody registered.
test_cinder_fixtures_agree_on_the_seed_image() {
  echo "Test: each cinder tempest fixture seeds the image its tempest.conf boots from"

  local config_dir seed_id image_ref
  for config_dir in "$PROJECT_ROOT"/tests/tempest/cinder-*/; do
    seed_id=$(sed -n 's/^ *SEED_ID="\(.*\)"/\1/p' \
      "$config_dir/07-image-seed-job.yaml" | head -1)
    image_ref=$(sed -n 's/^image_ref = //p' "$config_dir/tempest.conf" | head -1)

    assert_not_empty "$(basename "$config_dir") declares a SEED_ID" "$seed_id"
    assert_eq "$(basename "$config_dir") seeds the image its tempest.conf boots from" \
      "$seed_id" "$image_ref"
  done
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
test_cinder_tempest_filter_is_wired
test_tempest_cinder_leg_is_wired
test_tempest_cinder_leg_brings_up_a_compute_stack
test_matrix_cr_names_match_the_cinder_fixtures
test_cinder_fixtures_agree_on_the_seed_image
test_cinder_tempest_conf_runs_against_a_nova
test_cinder_tempest_crs_fit_their_uwsgi_workers
test_controlplane_leg_deploys_cinder

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
