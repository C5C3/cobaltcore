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
# Nova stays out of the two-cluster placed-services suite
# (tests/e2e-multicluster/placed-services/), per the author on 2026-09-17. The
# suite proves where the children land, and every grant of the
# target-cluster-access chart a Nova would exercise (Deployments, Jobs,
# CronJobs, HTTPRoutes, NetworkPolicies, HorizontalPodAutoscalers) is exercised
# by the placed Keystone, Barbican, OVNCentral and Neutron already. The placed
# Neutron runs against a deliberately unreachable broker
# (rabbitmq.openstack.invalid in its messaging Secret), and a Nova cannot: its
# scheduler and conductor report ready only off a live broker connection, so
# membership would mean a real broker on the target cluster, a seventh operator
# on the management cluster and an eighth image inside a 90-minute job. Nova's
# remote teardown stays proven by the operator's envtest
# (operators/nova/internal/controller/) and, against a real second cluster, by
# #1013 when compute clusters arrive. Nothing here asserts on that suite, and
# #1019 does not reopen the question.
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

# Echo how many releases the operator ships a service image for. The resolve
# step reads the same script, so a release added under releases/ moves the
# expected ref counts below with it.
release_count() {
  OPERATOR="$1" "$PROJECT_ROOT/hack/ci-service-image-releases.sh" |
    grep -c . || true
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

test_nova_leg_deploys_the_sibling_operators() {
  echo "Test: the nova e2e leg deploys and loads its five sibling operators"

  # A Nova has no identity-free posture: spec.keystoneEndpoint and
  # spec.serviceUser are required on the CRD, so the leg needs a Keystone. The
  # functional suites boot a server, which takes a Placement allocation, a
  # Glance image and a Neutron port, and that Neutron reaches Ready only behind
  # a live OVNCentral. A sibling missing from the job leaves its CRs
  # unreconciled and every suite that reads one waiting out its own timeout.
  local deploy op
  for op in keystone placement glance neutron; do
    deploy=$(job_step e2e-operator "Deploy ${op}-operator")
    assert_not_empty "the ${op}-operator deploy step exists" "$deploy"
    # The whole condition, not a substring of it: an extra `|| always()` would
    # install this operator on all ten legs.
    assert_eq "the ${op} deploy runs on the nova leg alone" \
      "if: matrix.operator == 'nova'" \
      "$(grep -E '^ *if:' <<<"$deploy" | sed 's/^ *//')"
    assert_contains "it goes through the shared deploy script" "$deploy" \
      "run: hack/ci-deploy-operator.sh"
    assert_contains "it deploys the ${op} operator" "$deploy" "OPERATOR: ${op}"
    assert_contains "it uses the run-tagged ${op}-operator image" "$deploy" \
      "IMAGE_PREFIX }}/${op}-operator"
    assert_contains "it lands in the ${op} Namespace" "$deploy" \
      "NAMESPACE: ${op}-system"
  done

  # The keystone install here is a plain one: the CIDR allowlist belongs to the
  # keystone leg's oidc-federation suite, and ci-deploy-operator.sh would
  # render it into this chart too.
  assert_not_contains "the keystone sibling takes no federation override" \
    "$(job_step e2e-operator "Deploy keystone-operator")" \
    "FEDERATION_METADATA_ALLOW_CIDRS"

  # The neutron leg's ovn step is widened rather than copied: job_step returns
  # the first step of a name, so a second "Deploy ovn-operator" would be
  # invisible to every assertion in this directory.
  local ovn
  ovn=$(job_step e2e-operator "Deploy ovn-operator")
  assert_eq "the ovn deploy runs on the neutron and nova legs alone" \
    "if: matrix.operator == 'neutron' || matrix.operator == 'nova'" \
    "$(grep -E '^ *if:' <<<"$ovn" | sed 's/^ *//')"

  # Order is the load-bearing part: the nova-operator's own Nova never resolves
  # a Keystone endpoint, a Placement or a Neutron that nothing reconciles yet.
  local job keystone_at placement_at glance_at ovn_at neutron_at nova_at
  job=$(job_block e2e-operator)
  keystone_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy keystone-operator" | head -1 | cut -d: -f1)
  placement_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy placement-operator" | head -1 | cut -d: -f1)
  glance_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy glance-operator" | head -1 | cut -d: -f1)
  ovn_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy ovn-operator" | head -1 | cut -d: -f1)
  neutron_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy neutron-operator" | head -1 | cut -d: -f1)
  nova_at=$(printf '%s\n' "$job" |
    grep -nF "name: Deploy operator" | head -1 | cut -d: -f1)
  assert_not_empty "the job deploys keystone-operator" "$keystone_at"
  assert_not_empty "the job deploys placement-operator" "$placement_at"
  assert_not_empty "the job deploys glance-operator" "$glance_at"
  assert_not_empty "the job deploys ovn-operator" "$ovn_at"
  assert_not_empty "the job deploys neutron-operator" "$neutron_at"
  assert_not_empty "the job deploys the matrix operator itself" "$nova_at"
  assert_gte "placement-operator comes after keystone-operator" \
    "$placement_at" "$keystone_at"
  assert_gte "glance-operator after placement-operator" \
    "$glance_at" "$placement_at"
  assert_gte "ovn-operator after glance-operator" "$ovn_at" "$glance_at"
  assert_gte "neutron-operator after ovn-operator" "$neutron_at" "$ovn_at"
  assert_gte "and the matrix operator's own deploy last" \
    "$nova_at" "$neutron_at"

  # kind pulls nothing the run did not load, so the load step reads the list the
  # resolve step publishes rather than naming the images a second time: the
  # five operator images and every service image each sibling publishes (ovn
  # publishes none). Pinning 2025.2 alone would leave a 2026.1 Nova talking to
  # 2025.2 siblings.
  local resolve load
  resolve=$(job_step e2e-operator "Resolve E2E images")
  load=$(job_step e2e-operator "Load images into kind")
  assert_contains "the resolve step branches on the nova leg" "$resolve" \
    'if [ "${OPERATOR}" = "nova" ]'
  assert_contains "it resolves the five sibling operator images" "$resolve" \
    'for sibling in keystone placement glance ovn neutron; do'
  assert_contains "at the run-scoped dev tag" "$resolve" \
    '${IMAGE_PREFIX}/${sibling}-operator:dev'
  assert_contains "with their release list read from source-refs.yaml" \
    "$resolve" 'OPERATOR="${sibling}" hack/ci-service-image-releases.sh'
  assert_contains "one service image per release" "$resolve" \
    '${IMAGE_PREFIX}/${sibling}:${release}'
  assert_contains "plus the OVN daemon image at the resolved pin" "$resolve" \
    '${IMAGE_PREFIX}/ovn:${OVN_VERSION}'
  assert_contains "the load step branches on the nova leg too" "$load" \
    'if [ "${OPERATOR}" = "nova" ]'
  assert_contains "it reads the resolved list" "$load" \
    'REFS: ${{ steps.e2e-images.outputs.refs }}'
  assert_contains "and loads every ref in it onto the kind node in one call" \
    "$load" 'kind load docker-image ${REFS} --name "${KIND_CLUSTER}"'

  # Reading the branch proves the shape, not the count. Run the step's own
  # script to see which refs the leg actually pulls and loads: a ref the
  # resolver drops is an ImagePullBackOff halfway through the suites.
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local script output refs
  script=$(mktemp)
  output=$(mktemp)
  yq -r '.jobs.e2e-operator.steps[]
    | select(.name == "Resolve E2E images") | .run' "$CI_YAML" >"$script"

  # The script shells out to hack/ by relative path.
  (cd "$PROJECT_ROOT" &&
    OPERATOR=nova IMAGE_PREFIX=ghcr.io/c5c3 GITHUB_OUTPUT="$output" \
      bash "$script")
  refs=$(awk '/^refs<<EOF$/ { in_b = 1; next }
    in_b && /^EOF$/ { exit }
    in_b { print }' "$output")

  # nova-operator:dev and one nova image per release, then each sibling's
  # operator image and its service images, then ovn:<pin>.
  local expected sibling
  expected=$((1 + $(release_count nova)))
  for sibling in keystone placement glance ovn neutron; do
    expected=$((expected + 1 + $(release_count "$sibling")))
  done
  expected=$((expected + 1))
  assert_eq "the nova leg resolves its own images plus every sibling's" \
    "$expected" "$(printf '%s\n' "$refs" | wc -l | tr -d ' ')"
  assert_contains "its own operator image" "$refs" \
    "ghcr.io/c5c3/nova-operator:dev"
  for op in keystone placement glance ovn neutron; do
    assert_contains "the ${op}-operator image" "$refs" \
      "ghcr.io/c5c3/${op}-operator:dev"
  done
  assert_contains "the 2025.2 keystone service image" "$refs" \
    "ghcr.io/c5c3/keystone:2025.2"
  assert_contains "and the 2026.1 one beside it" "$refs" \
    "ghcr.io/c5c3/keystone:2026.1"
  assert_contains "every release each sibling ships" "$refs" \
    "ghcr.io/c5c3/neutron:2026.1"
  assert_contains "the OVN daemon image at the pin the scripts resolve" \
    "$refs" "ghcr.io/c5c3/ovn:$(cd "$PROJECT_ROOT" &&
      hack/ci-resolve-ovn-version.sh)"

  # And the branch still gates: the neutron leg keeps the refs it had.
  : >"$output"
  (cd "$PROJECT_ROOT" &&
    OPERATOR=neutron IMAGE_PREFIX=ghcr.io/c5c3 GITHUB_OUTPUT="$output" \
      bash "$script")
  refs=$(awk '/^refs<<EOF$/ { in_b = 1; next }
    in_b && /^EOF$/ { exit }
    in_b { print }' "$output")

  # neutron-operator:dev, one neutron image per release, ovn-operator:dev and
  # ovn:<pin>.
  assert_eq "the neutron leg resolves its own images plus the two OVN ones" \
    "$((1 + $(release_count neutron) + 2))" \
    "$(printf '%s\n' "$refs" | wc -l | tr -d ' ')"
  assert_not_contains "the sibling block is nova-only" "$refs" "keystone"

  rm -f "$script" "$output"
}

test_nova_leg_narrows_parallelism_and_budget() {
  echo "Test: the nova e2e leg runs two suites at a time inside a 90-minute wall"

  # A Nova suite of #1039 stands a Keystone, an OVNCentral, a Neutron, a
  # Placement, a Glance and the five Nova workloads on one kind node, so at the
  # shared config's parallel: 4 the Pods stay Pending on "Insufficient cpu" and
  # the CRs never reach Ready. Read the condition together with its body, so a
  # nova arm on a branch that no longer narrows anything does not pass.
  #
  # The wall covers what the leg does before its first suite: the kind broker,
  # fourteen sibling image loads and five sibling deploys. It is an expression
  # on the matrix operator rather than a higher flat number, so only the nova
  # leg spends the extra runner time and no other leg's wall moves with it.
  local narrowing
  narrowing=$(job_step e2e-operator "Run E2E tests" | awk '
    /^ *parallel=\(\)$/ { in_block = 1; next }
    in_block && /^ *fi$/ { exit }
    in_block { print }
  ')

  assert_not_empty "the chainsaw run step narrows the parallelism" "$narrowing"
  assert_contains "the nova leg is one of the narrowed ones" "$narrowing" \
    '[ "${OPERATOR}" = "nova" ]'
  assert_contains "it runs two suites at a time" "$narrowing" \
    "parallel=(--parallel 2)"

  # The arm was added, not swapped in: both legs that were narrowed before it
  # still are.
  assert_contains "the neutron narrowing is kept" "$narrowing" \
    '[ "${OPERATOR}" = "neutron" ]'
  assert_contains "the cinder narrowing is kept" "$narrowing" \
    '[ "${OPERATOR}" = "cinder" ]'

  local job
  job=$(job_block e2e-operator)
  assert_contains "the nova leg gets 90 minutes and the others keep 68" "$job" \
    "timeout-minutes: \${{ matrix.operator == 'nova' && 90 || 68 }}"
  assert_not_contains "no flat wall is left beside the expression" "$job" \
    "timeout-minutes: 68"

  # runs-on on the line above already branches on the same matrix key, which is
  # the precedent this one follows. Asserting it here breaks both together if
  # the axis is ever renamed.
  assert_contains "runs-on branches on the same matrix key" "$job" \
    "runs-on: \${{ matrix.operator == 'keystone'"
}

test_nova_leg_dumps_the_siblings() {
  echo "Test: the nova e2e leg dumps its five sibling Namespaces"

  # The first dump derives its Namespace from the matrix operator and never
  # looks at the siblings', so a failed nova suite would carry no keystone-,
  # placement-, glance-, ovn- or neutron-operator log at all. The dump runs
  # under always(), so it is there when the bring-up itself failed: a sibling
  # deploy that fails at `helm install --wait` skips the chainsaw step but not
  # this one.
  local dump
  dump=$(job_step e2e-operator "Dump diagnostic info (nova siblings)")

  assert_not_empty "the sibling dump step exists" "$dump"
  assert_contains "it dumps on the nova leg even when the suites failed" \
    "$dump" "if: always() && matrix.operator == 'nova'"
  assert_contains "it goes through the shared dump script" "$dump" \
    "hack/ci-dump-diagnostics.sh"
  assert_contains "one call per sibling operator" "$dump" \
    "for op in keystone placement glance ovn neutron; do"
  # The step has no OPERATOR in its env, so a bare call would fall back to
  # OPERATOR="" and print the infrastructure section alone, five times.
  assert_contains "each pass hands its sibling to the dump script" "$dump" \
    'OPERATOR="${op}" hack/ci-dump-diagnostics.sh'

  # The neutron leg's dump was not widened: on the nova leg the loop above is
  # what covers ovn-system, and the first dump still reads the matrix operator.
  assert_contains "the ovn dump stays neutron-only" \
    "$(job_step e2e-operator "Dump diagnostic info (ovn)")" \
    "if: always() && matrix.operator == 'neutron'"
  assert_contains "the first dump still follows the matrix operator" \
    "$(job_step e2e-operator "Dump diagnostic info")" \
    "OPERATOR: \${{ matrix.operator }}"
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
test_nova_leg_deploys_the_sibling_operators
test_nova_leg_narrows_parallelism_and_budget
test_nova_leg_dumps_the_siblings

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
