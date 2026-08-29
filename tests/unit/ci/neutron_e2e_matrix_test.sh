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
# do with the change under test. The last two tests cover jobs the
# e2e-operators matrix never produces: the e2e-chaos network leg, whose
# test_dirs are enumerated by hand because chainsaw's regex filters are no-ops,
# and the tempest neutron legs, which run the neutron-tempest-plugin against a
# Neutron the job has to bring up itself.
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

  local all_operators="keystone c5c3 horizon glance placement barbican ovn neutron"
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
  local all_operators="keystone c5c3 horizon glance placement barbican ovn neutron"
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
  load=$(job_step e2e-chaos "Load E2E images")
  assert_contains "the leg pulls the neutron-operator image" "$load" \
    "matrix.suite == 'network' && format('{0}/neutron-operator:dev', env.IMAGE_PREFIX)"
  assert_contains "the leg pulls the neutron service image" "$load" \
    "matrix.suite == 'network' && format('{0}/neutron:2025.2', env.IMAGE_PREFIX)"

  local kind_load
  kind_load=$(job_step e2e-chaos "Load neutron images into kind")
  assert_not_empty "both images reach the node" "$kind_load"
  assert_contains "the load runs on the network leg alone" "$kind_load" \
    "if: matrix.suite == 'network'"
  assert_contains "the operator image is loaded" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/neutron-operator:dev"
  assert_contains "the service image is loaded" "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/neutron:2025.2"

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

  # The catalog Job runs in-cluster, so the tempest image has to be on the node.
  local kind_load
  kind_load=$(job_step tempest "Load neutron images into kind")
  assert_contains "the load runs on the neutron leg alone" "$kind_load" \
    "if: matrix.service == 'neutron'"
  assert_contains "the tempest image reaches the node for the catalog Job" \
    "$kind_load" \
    "kind load docker-image \${{ env.IMAGE_PREFIX }}/tempest:\${{ matrix.release }}"

  local ovn_deploy neutron_deploy
  ovn_deploy=$(job_step tempest "Deploy ovn operator")
  neutron_deploy=$(job_step tempest "Deploy neutron operator")

  assert_not_empty "the ovn-operator is deployed" "$ovn_deploy"
  assert_contains "the ovn deploy runs on the neutron leg alone" "$ovn_deploy" \
    "if: matrix.service == 'neutron'"
  assert_contains "it deploys the ovn operator" "$ovn_deploy" "OPERATOR: ovn"
  assert_contains "it lands in its own Namespace" "$ovn_deploy" \
    "NAMESPACE: ovn-system"

  assert_not_empty "the neutron-operator is deployed" "$neutron_deploy"
  assert_contains "the neutron deploy runs on the neutron leg alone" \
    "$neutron_deploy" "if: matrix.service == 'neutron'"
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
test_matrix_cr_names_match_the_tempest_fixtures

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
