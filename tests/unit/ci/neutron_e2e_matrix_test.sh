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
# do with the change under test. The last tests cover jobs the e2e-operators
# matrix never produces: the e2e-chaos network leg, whose test_dirs are
# enumerated by hand because chainsaw's regex filters are no-ops, and the
# tempest neutron legs, which run the neutron-tempest-plugin against a Neutron
# the job has to bring up itself, together with the flat external network the
# router and floating-IP suites need.
#
# Usage: bash tests/unit/ci/neutron_e2e_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
BASE_PATH="$PATH"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

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

  local all_operators="keystone c5c3 horizon glance placement barbican ovn neutron cinder nova"
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
  deploy=$(job_step e2e-operator "Deploy ovn-operator")

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
  dump=$(job_step e2e-operator "Dump diagnostic info (ovn)")
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
  resolve=$(job_step e2e-operator "Resolve E2E images")
  load=$(job_step e2e-operator "Load images into kind")
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

test_go_matrices_list_neutron() {
  echo "Test: the unit and integration test matrices include neutron"

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

  local all_operators="keystone c5c3 horizon glance placement barbican ovn neutron cinder nova"
  assert_contains "a neutron change puts neutron in the test matrix" \
    "$(resolve_output test-targets refs/heads/main "$all_operators" FILTER_neutron=true)" \
    '"neutron"'
}

test_cleanup_matrices_cover_the_neutron_images() {
  echo "Test: the derived cleanup package lists cover the neutron images"

  # cleanup-images.yaml and ci.yaml's cleanup-e2e-tags both build their package
  # matrix from this generator, so coverage is a property of its output rather
  # than of a list someone has to remember to extend. An uncovered package
  # leaks its run-scoped GHCR tags on every pull request.
  local matrix all_packages e2e_packages
  matrix=$(cd "$PROJECT_ROOT" && bash hack/ci-generate-cleanup-matrix.sh)
  all_packages=$(echo "$matrix" | sed -n 's/^cleanup-packages=//p')
  e2e_packages=$(echo "$matrix" | sed -n 's/^cleanup-e2e-packages=//p')

  assert_contains "the nightly sweep covers neutron-operator" \
    "$all_packages" '"neutron-operator"'
  assert_contains "the per-run sweep covers neutron-operator" \
    "$e2e_packages" '"neutron-operator"'
  assert_contains "the nightly sweep covers neutron" \
    "$all_packages" '"neutron"'
  assert_contains "the per-run sweep covers neutron" \
    "$e2e_packages" '"neutron"'
}

test_a_keystone_only_change_produces_no_neutron_leg() {
  echo "Test: a keystone-only change keeps neutron out of the e2e-operators matrix"

  # The positive case above proves the filter reaches the matrix; this one
  # proves it still gates. A filter wired to a constant would satisfy the
  # positive assertion and put every operator on every pull request.
  local all_operators="keystone c5c3 horizon glance placement barbican ovn neutron cinder nova"
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$all_operators" \
    FILTER_keystone=true)

  assert_contains "the matrix carries the keystone leg" "$matrix" '"keystone"'
  assert_not_contains "and no neutron leg" "$matrix" '"neutron"'
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
  echo "Test: the e2e-chaos network leg runs the four neutron suites and the ovn-operator kill"

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
  assert_contains "it runs the Keystone outage suite" "$entry" \
    "tests/e2e-chaos/neutron-keystone-outage"
  assert_contains "it runs the operator pod-kill suite" "$entry" \
    "tests/e2e-chaos/neutron-operator-pod-kill"
  # ovn-operator-pod-kill brings a Neutron that programs the OVN control
  # plane, so it runs where the neutron-operator is deployed.
  assert_contains "it runs the ovn-operator pod-kill suite" "$entry" \
    "tests/e2e-chaos/ovn-operator-pod-kill"

  # The first dump reads keystone-system alone on this leg, and three of its
  # suites kill an operator, so the leg dumps every operator Namespace it
  # deployed, one pass per operator with OPERATOR_ONLY, as the nova leg does.
  local dump
  dump=$(job_step e2e-chaos "Dump diagnostic info (network leg)")
  assert_not_empty "the leg dumps the Namespaces the first dump misses" "$dump"
  assert_contains "it dumps even when the suites failed" "$dump" \
    "if: always() && matrix.suite == 'network'"
  assert_contains "one call per operator the leg deploys beyond keystone" "$dump" \
    "for op in neutron ovn cinder placement glance barbican horizon; do"
  assert_contains "each pass hands its operator to the dump script" "$dump" \
    'OPERATOR="${op}" OPERATOR_ONLY=1 hack/ci-dump-diagnostics.sh'

  local load
  load=$(job_step e2e-chaos "Load E2E images")
  assert_contains "the leg pulls the neutron-operator image" "$load" \
    "matrix.suite == 'network' && format('{0}/neutron-operator:dev', env.IMAGE_PREFIX)"
  assert_contains "the leg pulls the neutron service image" "$load" \
    "matrix.suite == 'network' && format('{0}/neutron:2026.1', env.IMAGE_PREFIX)"

  local kind_load
  kind_load=$(job_step e2e-chaos "Load neutron images into kind")
  assert_not_empty "both images reach the node" "$kind_load"
  assert_contains "the load runs on the network leg alone" "$kind_load" \
    "if: matrix.suite == 'network'"
  assert_contains "the operator image is loaded" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/neutron-operator:dev"
  assert_contains "the service image is loaded" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/neutron:2026.1"

  local deploy
  deploy=$(job_step e2e-chaos "Deploy neutron operator")
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

test_tempest_neutron_leg_is_wired() {
  echo "Test: the tempest neutron leg brings up the stack its suites need"

  # Each assertion below guards a silent failure on the two neutron legs. The
  # four image refs are what the run tag makes available: an entry missing here
  # leaves the leg pulling the published tag instead of this PR's build, or
  # waiting on a pull that never resolves. The two operator deploys are what
  # reconciles the OVNCentral and the Neutron; without them the CRs sit without
  # status until the wait times out. The catalog Job registers the network
  # service and its endpoints in Keystone, which the Neutron reconciler
  # authenticates against. NEUTRON_K8S_NAME is the sole gate on the 9696
  # port-forward in ci-run-tempest.sh: unset, every network test fails to reach
  # the API.
  local load
  load=$(job_step tempest "Load E2E images")

  assert_contains "the leg pulls the neutron-operator image" "$load" \
    "matrix.service == 'neutron' && format('{0}/neutron-operator:dev', env.IMAGE_PREFIX)"
  assert_contains "the leg pulls the neutron service image" "$load" \
    "matrix.service == 'neutron' && format('{0}/neutron:{1}', env.IMAGE_PREFIX, matrix.release)"
  assert_contains "the leg pulls the ovn-operator image" "$load" \
    "matrix.service == 'neutron' && format('{0}/ovn-operator:dev', env.IMAGE_PREFIX)"
  assert_contains "the leg pulls the OVN daemon image at the resolved pin" "$load" \
    "matrix.service == 'neutron' && format('{0}/ovn:{1}', env.IMAGE_PREFIX, env.OVN_VERSION)"

  # The catalog and external-network Jobs run in-cluster, so the tempest image
  # has to be on the node.
  local kind_load
  kind_load=$(job_step tempest "Load neutron images into kind")
  assert_contains "the load runs on the neutron leg alone" "$kind_load" \
    "if: matrix.service == 'neutron'"
  assert_contains "the tempest image reaches the node for both Jobs" \
    "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/tempest:\${{ matrix.release }}"

  local ovn_deploy neutron_deploy
  ovn_deploy=$(job_step tempest "Deploy ovn operator")
  neutron_deploy=$(job_step tempest "Deploy neutron operator")

  assert_not_empty "the ovn-operator is deployed" "$ovn_deploy"
  assert_contains "the ovn deploy covers the neutron leg" "$ovn_deploy" \
    "if: matrix.service == 'neutron' || matrix.service == 'nova' || matrix.service == 'cinder'"
  assert_contains "it deploys the ovn operator" "$ovn_deploy" "OPERATOR: ovn"
  assert_contains "it lands in its own Namespace" "$ovn_deploy" \
    "NAMESPACE: ovn-system"

  assert_not_empty "the neutron-operator is deployed" "$neutron_deploy"
  assert_contains "the neutron deploy covers the neutron leg" \
    "$neutron_deploy" \
    "if: matrix.service == 'neutron' || matrix.service == 'nova' || matrix.service == 'cinder'"
  assert_contains "it deploys the neutron operator" "$neutron_deploy" \
    "OPERATOR: neutron"
  assert_contains "it lands in its own Namespace" "$neutron_deploy" \
    "NAMESPACE: neutron-system"

  local catalog ovncentral neutron_cr
  catalog=$(job_step tempest "Bootstrap network catalog")
  assert_contains "the catalog Job is applied" "$catalog" \
    "01-catalog-setup-job.yaml"
  assert_contains "the leg waits for it to complete" "$catalog" \
    "job/neutron-tempest-catalog-setup"

  ovncentral=$(job_step tempest "Deploy OVNCentral for Tempest")
  assert_contains "the messaging Secret is applied" "$ovncentral" \
    "02-messaging-secret.yaml"
  assert_contains "the OVNCentral is applied" "$ovncentral" \
    "03-ovncentral-cr.yaml"
  # The name comes from the matrix, like every other CR this job waits on. It
  # was once rebuilt here by stripping "neutron-" off the config-dir basename,
  # which put the OVNCentral name in the workflow as well as in the fixture, so
  # a rename in tests/tempest/neutron-*/03-ovncentral-cr.yaml left CI waiting
  # out its 300s on a resource that does not exist.
  assert_contains "the leg waits on the OVNCentral the matrix names" "$ovncentral" \
    "kubectl wait ovncentral/\${{ matrix.ovn-cr-name }}"
  assert_not_contains "the name is not rebuilt from the config directory" \
    "$ovncentral" 'slug="${slug#neutron-}"'
  # The `..` stand in for the escaped quotes of the JSON the generator emits,
  # so the needle pins the emitting line rather than the header comment.
  assert_file_contains "the matrix generator emits ovn-cr-name for the neutron legs" \
    "$PROJECT_ROOT/hack/ci-generate-tempest-matrix.sh" \
    'ovn-cr-name..:..ovn-neutron-tempest'

  neutron_cr=$(job_step tempest "Deploy Neutron CR for Tempest")
  assert_contains "the Neutron CR is applied" "$neutron_cr" "04-neutron-cr.yaml"
  assert_contains "the leg waits on the CR the matrix names" "$neutron_cr" \
    "kubectl wait neutron/\${{ matrix.neutron-cr-name }}"
  assert_contains "the db-sync fits inside the wait" "$neutron_cr" \
    "--timeout=600s"

  # The router, floating-IP and external-network suites need the network this
  # Job creates, and the run step resolves its id: without the step, the run
  # stops on "No external network exists".
  local external
  external=$(job_step tempest "Create the external network for Tempest")
  assert_not_empty "the external-network step exists" "$external"
  assert_contains "it runs on the neutron leg alone" "$external" \
    "if: matrix.service == 'neutron'"
  assert_contains "the external-network Job is applied" "$external" \
    "05-external-network-job.yaml"
  assert_contains "the leg waits for it to complete" "$external" \
    "kubectl wait --for=condition=complete job/neutron-tempest-external-network"
  assert_contains "the wait has its own 300s budget" "$external" \
    "--timeout=300s"
  assert_contains "the step prints the Job's log" "$external" \
    "kubectl logs job/neutron-tempest-external-network"

  local run_step
  run_step=$(job_step tempest "Run Tempest API tests")
  assert_contains "the runner learns the Neutron Service name" "$run_step" \
    "NEUTRON_K8S_NAME: \${{ matrix.neutron-cr-name }}"

  # The catalog Job authenticates against Keystone and creates the network
  # service, but the Neutron that consumes it needs an operator watching by
  # then, so both deploys come first.
  local job neutron_at catalog_at
  job=$(job_block tempest)
  neutron_at=$(printf '%s\n' "$job" | grep -nF "name: Deploy neutron operator" | head -1 | cut -d: -f1)
  catalog_at=$(printf '%s\n' "$job" | grep -nF "name: Bootstrap network catalog" | head -1 | cut -d: -f1)
  assert_eq "the operator deploys run before the catalog bootstrap" "yes" \
    "$([ -n "$neutron_at" ] && [ -n "$catalog_at" ] &&
       [ "$neutron_at" -lt "$catalog_at" ] && echo yes || echo no)"

  # The Job creates the network through the Neutron API, so the Neutron has to
  # be Ready first, and the run step looks the network up before any suite.
  local cr_at external_at run_at
  cr_at=$(printf '%s\n' "$job" | grep -nF "name: Deploy Neutron CR for Tempest" | head -1 | cut -d: -f1)
  external_at=$(printf '%s\n' "$job" | grep -nF "name: Create the external network for Tempest" | head -1 | cut -d: -f1)
  run_at=$(printf '%s\n' "$job" | grep -nF "name: Run Tempest API tests" | head -1 | cut -d: -f1)
  assert_eq "the external network is created after the Neutron is Ready and before Tempest runs" "yes" \
    "$([ -n "$cr_at" ] && [ -n "$external_at" ] && [ -n "$run_at" ] &&
       [ "$cr_at" -lt "$external_at" ] && [ "$external_at" -lt "$run_at" ] &&
       echo yes || echo no)"
}

# make_external_network_stubs <dir>
# Writes the openstack stub the external-network Job resolves off PATH. It
# records every argv in $STUB_OS_LOG and answers the two list guards from
# $STUB_HAS_NETWORK and $STUB_HAS_SUBNET (yes, no or fail), so one script run
# is one state of a cluster the Job meets: nothing created yet, a pod retried
# after the network create, a Job applied again after both creates, or a list
# call that fails the way a Neutron 503 does.
make_external_network_stubs() {
  local dir="$1"

  cat >"$dir/openstack" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$STUB_OS_LOG"
case "$1 $2" in
  "network list") state="$STUB_HAS_NETWORK" name=tempest-public ;;
  "subnet list") state="$STUB_HAS_SUBNET" name=tempest-public-subnet ;;
  *) exit 0 ;;
esac
case "$state" in
  yes) echo "$name" ;;
  fail) echo "Service Unavailable (HTTP 503)" >&2; exit 1 ;;
esac
exit 0
STUB
  chmod +x "$dir/openstack"
}

# run_external_network_script <dir> <job-fixture> <has-network> <has-subnet>
# Extracts the shell script the external-network Job runs and runs it with the
# stub in <dir> first on PATH. Each call starts from an empty log at
# <dir>/openstack.log. Returns the script's exit code and prints nothing.
run_external_network_script() {
  local dir="$1" fixture="$2" has_network="$3" has_subnet="$4"
  : >"$dir/openstack.log"
  yq -r '.spec.template.spec.containers[0].args[0]' "$fixture" >"$dir/external.sh"
  (
    PATH="$dir:$BASE_PATH"
    STUB_OS_LOG="$dir/openstack.log"
    STUB_HAS_NETWORK="$has_network"
    STUB_HAS_SUBNET="$has_subnet"
    export PATH STUB_OS_LOG STUB_HAS_NETWORK STUB_HAS_SUBNET
    bash "$dir/external.sh"
  ) >/dev/null 2>&1
}

test_tempest_neutron_leg_has_an_external_network() {
  echo "Test: each tempest neutron leg creates a flat external network and points Tempest at it"

  # The router, floating-IP and external-network suites need a public network.
  # Three files carry it on each leg: the Job creates it, tempest.conf names
  # it through the placeholder hack/ci-run-tempest.sh resolves, and
  # exclude-tests.txt leaves out the classes of the dvr extension the network
  # list shows missing. A shared network would fail
  # test_external_network_visibility, which checks subnet visibility against
  # the shared flag.
  local dirs=() dir
  for dir in "$PROJECT_ROOT"/tests/tempest/neutron-*/; do
    dirs+=("$dir")
  done
  assert_gte "both releases carry a neutron leg" "${#dirs[@]}" 2

  local name job dvr_pattern
  for dir in "${dirs[@]}"; do
    name=$(basename "$dir")
    job="${dir}05-external-network-job.yaml"
    if [ ! -f "$job" ]; then
      echo "  FAIL: $name has no 05-external-network-job.yaml"
      FAIL=$((FAIL + 1))
      continue
    fi
    assert_file_contains_fixed "$name names the Job the ci.yaml step waits on" \
      "$job" "name: neutron-tempest-external-network"
    assert_file_contains_fixed "$name creates an external network" "$job" \
      "--external"
    assert_file_contains_fixed "$name makes it a flat provider network" "$job" \
      "--provider-network-type flat"
    assert_not_contains "$name does not share it" "$(cat "$job")" "--share"
    assert_not_contains "$name does not make it the default external network" \
      "$(cat "$job")" "--default"
    assert_file_contains_fixed "$name points public_network_id at the resolved id" \
      "${dir}tempest.conf" 'public_network_id = ${NEUTRON_PUBLIC_NETWORK_ID}'
    assert_file_contains "$name allocates floating IPs" \
      "${dir}tempest.conf" "^floating_ips = true$"
    for dvr_pattern in \
      'tempest\.api\.network\.test_routers_negative\.DvrRoutersNegativeTest\.' \
      'neutron_tempest_plugin\.api\.test_routers\.DvrRouters' \
      'neutron_tempest_plugin\.api\.test_routers_negative\.DvrRoutersNegative'; do
      assert_file_contains_fixed "$name excludes $dvr_pattern" \
        "${dir}exclude-tests.txt" "$dvr_pattern"
    done
    assert_file_contains_fixed "$name carries the DVR exclusion of #1614" \
      "${dir}exclude-tests.txt" \
      "tracked-by: https://github.com/C5C3/cobaltcore/issues/1614"
    # The plugin's router classes keep every router until the class ends, and
    # the default of ten per project runs out mid-class. The serial retry
    # passes such a test alone, so a dropped override shows up as flakes, not
    # as a red leg.
    assert_file_contains_fixed "$name raises the router quota of the suite's projects" \
      "${dir}04-neutron-cr.yaml" 'quota_router: "100"'
  done

  # The two guards are what make a retried pod converge: the network create
  # and the subnet create each run only when their own list comes back
  # without the name, and a list that fails stops the pod rather than reading
  # as absent. Proven by running the script against a stubbed client.
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local tmp log code keystone release
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_external_network_stubs "$tmp"

  for dir in "${dirs[@]}"; do
    name=$(basename "$dir")
    job="${dir}05-external-network-job.yaml"
    [ -f "$job" ] || continue

    # The Job's two release literals. A Job copied into a new release
    # directory with the old ones authenticates against a Keystone that does
    # not exist, or pulls another release's tempest image instead of the one
    # the step loaded into kind.
    keystone=$(yq -r '.metadata.name' "${dir}00-keystone-cr.yaml")
    release=${name#neutron-}
    release=${release/-/.}
    assert_eq "$name: the Job authenticates against the leg's Keystone" \
      "http://${keystone}.openstack.svc.cluster.local:5000/v3" \
      "$(yq -r '.spec.template.spec.containers[0].env[] | select(.name == "OS_AUTH_URL") | .value' "$job")"
    assert_eq "$name: the Job runs the leg's tempest image" \
      "ghcr.io/c5c3/tempest:${release}" \
      "$(yq -r '.spec.template.spec.containers[0].image' "$job")"

    run_external_network_script "$tmp" "$job" no no
    code=$?
    log=$(cat "$tmp/openstack.log")
    assert_eq "$name: a first run exits 0" "0" "$code"
    assert_eq "$name: it creates the flat external network on physnet1 once" "1" \
      "$(grep -cF "network create --external --provider-network-type flat --provider-physical-network physnet1 tempest-public" <<<"$log")"
    assert_eq "$name: and its subnet once" "1" \
      "$(grep -cF "subnet create --network tempest-public --subnet-range 203.0.113.0/24 --gateway 203.0.113.1 --no-dhcp tempest-public-subnet" <<<"$log")"
    assert_not_contains "$name: the network is not shared" "$log" "--share"
    assert_not_contains "$name: the network is not the default" "$log" "--default"

    run_external_network_script "$tmp" "$job" yes no
    code=$?
    log=$(cat "$tmp/openstack.log")
    assert_eq "$name: a pod retried after the network create exits 0" "0" "$code"
    assert_not_contains "$name: it creates no second network" "$log" \
      "network create"
    assert_eq "$name: it creates the missing subnet" "1" \
      "$(grep -c "subnet create" <<<"$log")"

    run_external_network_script "$tmp" "$job" yes yes
    code=$?
    log=$(cat "$tmp/openstack.log")
    assert_eq "$name: a Job applied again exits 0" "0" "$code"
    assert_not_contains "$name: it creates nothing" "$log" " create "

    # A second network named tempest-public breaks every later subnet list on
    # "More than one Network exists", and the lookup in hack/ci-run-tempest.sh
    # stops on two external networks.
    run_external_network_script "$tmp" "$job" fail no
    code=$?
    log=$(cat "$tmp/openstack.log")
    assert_nonzero_exit "$name: a pod whose network list fails exits non-zero" "$code"
    assert_not_contains "$name: it creates no network on a failed list" "$log" \
      "network create"
    assert_not_contains "$name: nor a subnet" "$log" "subnet create"

    run_external_network_script "$tmp" "$job" yes fail
    code=$?
    log=$(cat "$tmp/openstack.log")
    assert_nonzero_exit "$name: a pod whose subnet list fails exits non-zero" "$code"
    assert_not_contains "$name: it creates no subnet on a failed list" "$log" \
      "subnet create"
  done
}

# Each of the three CR names the tempest neutron leg waits on is generated, and
# each names a static fixture in the same config directory: ovn-cr-name the
# OVNCentral of 03-ovncentral-cr.yaml (300s), neutron-cr-name the Neutron of
# 04-neutron-cr.yaml (600s, and that fixture holds the OVNCentral name a second
# time in its centralRef), cr-name the Keystone of 00-keystone-cr.yaml (300s,
# and it drives the port-forward too). Every assertion above pins one of those
# literals against another copy of itself, so a renamed metadata.name in any of
# the three fixtures leaves them all green while the leg burns its wait on a
# resource that does not exist. This test reads the names out of the fixtures
# and requires the generator to agree with them.
test_matrix_cr_names_match_the_tempest_fixtures() {
  echo "Test: the matrix names the CRs the tempest fixtures create"

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
    | select(.service == "neutron")
    | [."config-dir", ."ovn-cr-name", ."neutron-cr-name", ."cr-name"] | @tsv')
  assert_not_empty "the generator emits at least one neutron leg" "$legs"

  local config_dir emitted neutron_emitted keystone_emitted
  local fixture_name central_ref
  while IFS=$'\t' read -r config_dir emitted neutron_emitted keystone_emitted; do
    [ -n "$config_dir" ] || continue
    fixture_name=$(yq -r '.metadata.name' \
      "$PROJECT_ROOT/$config_dir/03-ovncentral-cr.yaml")
    assert_eq "$config_dir waits on the OVNCentral its fixture creates" \
      "$fixture_name" "$emitted"
    central_ref=$(yq -r '.spec.ovn.centralRef.name' \
      "$PROJECT_ROOT/$config_dir/04-neutron-cr.yaml")
    assert_eq "$config_dir points its Neutron at the same OVNCentral" \
      "$fixture_name" "$central_ref"
    assert_eq "$config_dir waits on the Neutron its fixture creates" \
      "$(yq -r '.metadata.name' "$PROJECT_ROOT/$config_dir/04-neutron-cr.yaml")" \
      "$neutron_emitted"
    assert_eq "$config_dir waits on the Keystone its fixture creates" \
      "$(yq -r '.metadata.name' "$PROJECT_ROOT/$config_dir/00-keystone-cr.yaml")" \
      "$keystone_emitted"
  done <<< "$legs"
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
test_go_matrices_list_neutron
test_cleanup_matrices_cover_the_neutron_images
test_a_keystone_only_change_produces_no_neutron_leg
test_build_e2e_images_builds_the_neutron_images
test_chaos_network_leg_runs_the_neutron_suites
test_tempest_neutron_leg_is_wired
test_tempest_neutron_leg_has_an_external_network
test_matrix_cr_names_match_the_tempest_fixtures

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
