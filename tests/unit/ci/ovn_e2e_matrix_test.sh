#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the ovn operator reaches the e2e-operators matrix and the helm filter
# in .github/workflows/ci.yaml, that build-e2e-images builds both of its
# images, and that the e2e-operator leg loads the OVN daemon image and asks for
# the OVN kernel modules.
#
# Every signal here fails silently when it is missing. ALL_OPERATORS is the sole
# source of the e2e-operators matrix, so an operator absent from it never
# produces a leg and its Chainsaw suites under tests/e2e/ovn/ are lint-checked
# and never applied to a cluster. The helm filter is the sole gate on
# helm-validate, so a PR touching only operators/ovn/helm/ renders, lints and
# unit-tests nothing. build-e2e-images is the sole producer of the run-tagged
# images the E2E jobs pull, so an image it skips fails those jobs an hour into
# the run. The daemon image and the kernel modules are what the leg needs
# beyond the shared shape: without either one the chassis Pods never start and
# every suite fails at once, for a reason unrelated to the change under test.
#
# The last two tests cover jobs the e2e-operators matrix never produces:
# e2e-ovn-overlay, which needs the two-worker cluster of
# hack/kind-config-multinode.yaml instead of the single-node config every matrix
# leg creates, and the ovn leg of e2e-chaos, whose test_dirs are enumerated by
# hand because chainsaw's regex filters are no-ops.
#
# Usage: bash tests/unit/ci/ovn_e2e_matrix_test.sh

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
# both carry a "Setup E2E infrastructure" and a "Dump diagnostic info" step.
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

test_all_operators_lists_ovn() {
  echo "Test: ci.yaml ALL_OPERATORS includes ovn"

  local line
  line=$(grep "ALL_OPERATORS:" "$CI_YAML" | head -1)
  assert_contains "ALL_OPERATORS lists ovn" "$line" "ovn"
  assert_contains "ALL_OPERATORS still lists keystone" "$line" "keystone"
}

test_ovn_filter_is_wired() {
  echo "Test: the ovn paths filter reaches the resolve step"

  assert_file_contains "the paths filter exists" "$CI_YAML" "^ *ovn:$"
  assert_file_contains "the filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_ovn: \${{ steps.filter.outputs.ovn }}"

  local block
  block=$(filter_block ovn)
  assert_contains "the operator source path is covered" "$block" "operators/ovn/**"

  # The images live in a filter of their own: rebuilding the OVS/OVN daemons or
  # the backup shifter runs the ovn e2e leg without pulling the operator's Go
  # gates in with them.
  assert_file_contains "the image filter exists" "$CI_YAML" "^ *image_ovn:$"
  assert_file_contains "the image filter is passed to the resolve step" "$CI_YAML" \
    "FILTER_image_ovn: \${{ steps.filter.outputs.image_ovn }}"
  block=$(filter_block image_ovn)
  assert_contains "the OVN image is covered" "$block" "images/ovn/**"
  assert_contains "the backup-shifter image is covered" "$block" "images/backup-shifter/**"
}

test_ovn_change_produces_an_e2e_leg() {
  echo "Test: an operators/ovn change puts ovn in the e2e-operators matrix"

  local all_operators="keystone c5c3 horizon glance placement barbican ovn"
  local matrix
  matrix=$(resolve_output e2e-operators refs/heads/main "$all_operators" FILTER_ovn=true)

  assert_contains "the matrix carries the ovn leg" "$matrix" '"ovn"'
  assert_contains "the matrix keeps the operator axis" "$matrix" '"operator"'
  assert_not_contains "the sentinel is gone once an operator changed" \
    "$matrix" "__none__"
}

test_helm_filter_covers_the_ovn_chart() {
  echo "Test: an ovn chart change re-runs helm-validate"

  # The helm filter is the operators/*/helm/** glob; the chart directory
  # under it is what makes the glob cover this operator.
  assert_contains "the helm filter covers every operators/<op>/helm/ tree" \
    "$(filter_block helm)" "operators/*/helm/**"
  assert_eq "the ovn chart lives under that glob" "yes" \
    "$([ -d "$PROJECT_ROOT/operators/ovn/helm/ovn-operator" ] && echo yes || echo no)"
}

test_helm_validate_renders_the_ovn_chart() {
  echo "Test: helm-validate lints, templates and unit-tests the ovn chart"

  # The three `for chart in ...` loops of helm-validate — lint, template and
  # unittest — iterate the operators/*/helm/*-operator glob, so the chart is
  # covered by living in that layout (asserted above).
  local loops
  loops=$(grep -cF 'for chart in operators/*/helm/*-operator' "$CI_YAML")
  assert_eq "the lint, template and unittest loops iterate the chart glob" "3" "$loops"
}

test_build_e2e_images_builds_the_ovn_images() {
  echo "Test: build-e2e-images reaches both ovn images on every run"

  # The ovn-operator image and the OVN daemon image are consumed by jobs whose
  # triggers have nothing to do with an operators/ovn change, so neither may
  # sit behind a copy of another job's `if:`.
  local job
  job=$(job_block build-e2e-images)

  # The operator image is one of the keys hack/ci-resolve-e2e-images.sh derives
  # from the tree — every operators/<op>/ with a go.mod gets a map entry, built
  # when its own sources changed and reused from main's digest otherwise — so a
  # consumer always has an ovn-operator:dev to pull.
  assert_contains "the job resolves its images through the shared script" "$job" \
    "hack/ci-resolve-e2e-images.sh"
  assert_eq "the ovn operator is one of the tree-derived image keys" "yes" \
    "$([ -f "$PROJECT_ROOT/operators/ovn/go.mod" ] && echo yes || echo no)"

  # The daemon image carries no OpenStack release and no go.mod, so nothing
  # derives it and the map never names it. It keeps a build step of its own,
  # which runs on every run of this job; load-e2e-images falls back to the
  # run-scoped tag for a reference the map does not carry.
  assert_contains "the daemon image has its own build step" "$job" \
    "name: Build OVN image"
  assert_contains "the step resolves the pin instead of repeating it" "$job" \
    "hack/ci-resolve-ovn-version.sh"
  assert_contains "the step builds through the shared script" "$job" \
    "hack/ci-build-ovn-image.sh"
  assert_contains "the daemon image is pushed under the run tag" "$job" \
    'push_image "${IMAGE_PREFIX}/ovn:${OVN_VERSION}"'

  # images/ovn/Dockerfile holds the pin. A copy in the workflow would keep
  # pointing at the old tag the next time the Dockerfile is bumped.
  local pin
  pin=$("$PROJECT_ROOT/hack/ci-resolve-ovn-version.sh")
  assert_eq "the workflow never spells the pinned version" "0" \
    "$(grep -cF "$pin" "$CI_YAML")"
}

test_e2e_leg_loads_the_ovn_image() {
  echo "Test: the ovn e2e leg pulls and loads the OVN daemon image"

  # ovn ships no per-release service image, so the generic per-release loop
  # leaves this leg with the operator image alone. Every OVNCentral and
  # OVNChassis Pod runs ghcr.io/c5c3/ovn:<pin>, the default effectiveImage()
  # resolves, and kubelet only skips the registry for a tag already on the
  # node. A leg that never loads it waits on a pull instead of running.
  local resolve load
  resolve=$(e2e_operator_step "Resolve E2E images")
  load=$(e2e_operator_step "Load images into kind")

  assert_not_empty "the resolve step is still named that" "$resolve"
  assert_contains "the resolve step branches on the ovn leg" "$resolve" \
    '[ "${OPERATOR}" = "ovn" ]'
  assert_contains "it appends the daemon image" "$resolve" \
    '${IMAGE_PREFIX}/ovn:${OVN_VERSION}'
  assert_contains "the pin comes from the shared resolver" "$resolve" \
    'OVN_VERSION="$(hack/ci-resolve-ovn-version.sh)"'
  assert_contains "the pin is published for the load step" "$resolve" \
    'echo "ovn-version=${OVN_VERSION}"'

  assert_not_empty "the load step is still named that" "$load"
  assert_contains "the load step reads the published pin" "$load" \
    "OVN_VERSION: \${{ steps.e2e-images.outputs.ovn-version }}"
  assert_contains "the load step branches on the ovn leg too" "$load" \
    '[ "${OPERATOR}" = "ovn" ]'
  assert_contains "it loads the same ref into kind" "$load" \
    'kind load docker-image "${IMAGE_PREFIX}/ovn:${OVN_VERSION}"'
}

test_e2e_leg_opts_into_kernel_modules() {
  echo "Test: the ovn and neutron e2e legs ask for the OVN kernel modules"

  # The chassis DaemonSet opens a Geneve tunnel from the kind node, which needs
  # openvswitch and geneve on the host. deploy-infra.sh defaults the flag to
  # false and setup-e2e-infra reads it from env, so the value has to sit in
  # this step's own env block; anywhere else it never reaches modprobe.
  local setup
  setup=$(e2e_operator_step "Setup E2E infrastructure")

  assert_contains "the step still uses the shared composite action" "$setup" \
    "uses: ./.github/actions/setup-e2e-infra"
  assert_contains "both OVN legs opt in, the others keep the default" "$setup" \
    "WITH_OVN_KERNEL_MODULES: \${{ (matrix.operator == 'ovn' || matrix.operator == 'neutron') && 'true' || '' }}"
}

test_overlay_job_is_wired() {
  echo "Test: the e2e-ovn-overlay job runs the multi-node suite non-blocking"

  # The job is the only consumer of hack/kind-config-multinode.yaml and of the
  # e2e-ovn-overlay change signal. Every assertion below is a silent failure
  # otherwise: a wrong runner label leaves the job queued forever, a missing
  # continue-on-error turns a kernel-module gap on the host into a red PR, a
  # single-node config makes the suite fail its own two-node preflight, and an
  # unset WITH_OVN_KERNEL_MODULES leaves the chassis Pods unable to start.
  local job
  job=$(job_block e2e-ovn-overlay)

  assert_not_empty "the job exists" "$job"
  assert_contains "it runs on the self-hosted runners" "$job" \
    "runs-on: self-hosted"
  assert_contains "it enters CI non-blocking" "$job" \
    "continue-on-error: true"
  assert_contains "it creates the two-worker cluster" "$job" \
    "config: hack/kind-config-multinode.yaml"
  assert_contains "it asks for the OVN kernel modules" "$job" \
    'WITH_OVN_KERNEL_MODULES: "true"'
  assert_contains "it runs the suite through the Makefile target" "$job" \
    "make e2e-ovn-overlay"
  assert_contains "it uploads its own JUnit artifact" "$job" \
    "name: e2e-ovn-overlay-junit-report"
  assert_contains "the diagnostics dump looks in ovn-system" "$job" \
    "OPERATOR: ovn"
  assert_contains "it gates on the overlay change signal" "$job" \
    "needs.changes.outputs.e2e-ovn-overlay == 'true'"

  # Without this the tag prune races the job and deletes the run-scoped ovn
  # images out from under a suite that is still pulling them.
  assert_contains "cleanup-e2e-tags waits for the job" \
    "$(job_block cleanup-e2e-tags)" "e2e-ovn-overlay"
}

test_chaos_ovn_leg_is_wired() {
  echo "Test: the e2e-chaos ovn leg runs the southbound-outage suite alone"

  # e2e-chaos enumerates test_dirs per leg (chainsaw's include/exclude-regex
  # flags are no-ops in v0.2.14), so a suite absent from a leg is lint-checked
  # and never applied to a cluster. Every other assertion here is a silent
  # failure too: a wrong runner label leaves the leg queued forever, a missing
  # continue-on-error turns a kernel-module gap on the host into a red PR, an
  # unloaded daemon image leaves the OVNCentral Pods pulling from GHCR, an unset
  # WITH_OVN_KERNEL_MODULES leaves the chassis Pod unable to open its Geneve
  # tunnel, and a diagnostics dump pointed at keystone-system collects nothing
  # about the operator that failed.
  local job entry
  job=$(job_block e2e-chaos)
  entry=$(e2e_chaos_matrix_entry ovn)

  assert_not_empty "the matrix carries an ovn leg" "$entry"
  assert_contains "it runs on the self-hosted runners" "$entry" \
    "runner: self-hosted"
  assert_contains "it runs the southbound-outage suite" "$entry" \
    "tests/e2e-chaos/ovn-southbound-outage"
  assert_contains "it health-checks the chaos-mesh install first" "$entry" \
    "tests/e2e/infrastructure/chaos-mesh-health"
  assert_contains "only the pod leg stays blocking" "$job" \
    "continue-on-error: \${{ matrix.suite != 'pod' }}"

  # images/ovn/Dockerfile holds the pin, so the leg resolves it at runtime
  # rather than repeating the tag here (asserted globally further up).
  local load
  load=$(e2e_chaos_step "Load E2E images")
  assert_contains "the leg resolves the pin into the env" "$job" \
    'OVN_VERSION=$(hack/ci-resolve-ovn-version.sh)'
  assert_contains "it pulls the ovn-operator image" "$load" \
    "format('{0}/ovn-operator:dev', env.IMAGE_PREFIX)"
  assert_contains "it pulls the OVN daemon image at the resolved pin" "$load" \
    "format('{0}/ovn:{1}', env.IMAGE_PREFIX, env.OVN_VERSION)"

  local kind_load
  kind_load=$(e2e_chaos_step "Load OVN images into kind")
  assert_not_empty "both images reach the node" "$kind_load"
  assert_contains "the load runs on every leg with an OVNCentral" "$kind_load" \
    "if: matrix.suite != 'pod'"

  local deploy
  deploy=$(e2e_chaos_step "Deploy ovn operator")
  assert_not_empty "the ovn-operator is deployed" "$deploy"
  assert_contains "the deploy runs on every leg with an OVNCentral" "$deploy" \
    "if: matrix.suite != 'pod'"
  assert_contains "it goes through the shared deploy script" "$deploy" \
    "run: hack/ci-deploy-operator.sh"
  assert_contains "it deploys the ovn operator" "$deploy" "OPERATOR: ovn"
  assert_contains "it lands in its own Namespace" "$deploy" \
    "NAMESPACE: ovn-system"

  local setup dump
  setup=$(e2e_chaos_step "Setup E2E infrastructure")
  assert_contains "the ovn leg alone asks for the OVN kernel modules" "$setup" \
    "WITH_OVN_KERNEL_MODULES: \${{ matrix.suite == 'ovn' && 'true' || '' }}"
  assert_contains "Chaos Mesh stays on for every leg" "$setup" \
    'WITH_CHAOS_MESH: "true"'
  dump=$(e2e_chaos_step "Dump diagnostic info")
  assert_contains "the dump follows the leg's operator" "$dump" \
    "OPERATOR: \${{ matrix.suite == 'ovn' && 'ovn' || 'keystone' }}"

  # The ovn leg brings up no Keystone, so deploying the keystone-operator there
  # would only add a `helm install --wait` for an operator nothing reconciles.
  local keystone_deploy
  keystone_deploy=$(e2e_chaos_step "Deploy operator")
  assert_contains "the keystone deploy is gated off the ovn leg" \
    "$keystone_deploy" "if: matrix.suite != 'ovn'"
  assert_contains "and it is still the keystone one" "$keystone_deploy" \
    "OPERATOR: keystone"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_all_operators_lists_ovn
test_ovn_filter_is_wired
test_ovn_change_produces_an_e2e_leg
test_helm_filter_covers_the_ovn_chart
test_helm_validate_renders_the_ovn_chart
test_build_e2e_images_builds_the_ovn_images
test_e2e_leg_loads_the_ovn_image
test_e2e_leg_opts_into_kernel_modules
test_overlay_job_is_wired
test_chaos_ovn_leg_is_wired

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
