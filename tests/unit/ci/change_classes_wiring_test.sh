#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the ci.yaml side of the change classes: every filter is declared,
# handed to the resolver and consumed, every job gates on its own flag, and no
# job reads an output the changes job does not export.
#
# Every mistake this file looks for is silent. GitHub Actions resolves an
# unknown `needs.<job>.outputs.<name>` to the empty string rather than failing,
# so a renamed output or a missing FILTER_ env var leaves a job skipped on every
# pull request from then on, and the pipeline stays green while it stops testing
# something. The resolver's own behaviour is pinned separately, in
# tests/unit/ci/resolve_changes_scenarios_test.sh.
#
# Usage: bash tests/unit/ci/change_classes_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
ACTIONLINT_CONFIG="$PROJECT_ROOT/.github/actionlint.yaml"

PASS=0
FAIL=0
SKIP=0

# The real list from the ci.yaml resolve step env block.
ALL_OPS="keystone c5c3 horizon glance placement barbican ovn neutron"

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Echo every filter key declared under the paths-filter step, one per line.
declared_filters() {
  awk '
    /^          filters: \|$/ { in_filters = 1; next }
    in_filters && /^      [a-z-]/ { exit }
    in_filters && /^            [a-z0-9_]+:$/ {
      gsub(/[ :]/, "", $0); print
    }
  ' "$CI_YAML"
}

# Echo every filter name handed to the resolve step as FILTER_<name>.
wired_filters() {
  grep -oE '^          FILTER_[a-z0-9_]+:' "$CI_YAML" | sed 's/^ *FILTER_//;s/:$//'
}

# Echo the body of the top-level ci.yaml job <name>.
job_block() {
  awk -v key="  $1:" '
    $0 == key { in_block = 1; next }
    in_block && /^  [#a-z0-9-]/ { exit }
    in_block { print }
  ' "$CI_YAML"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_every_filter_is_wired_both_ways() {
  echo "Test: every declared filter reaches the resolver, and nothing else does"

  local declared wired
  declared=$(declared_filters | sort)
  wired=$(wired_filters | sort)

  assert_not_empty "the paths-filter step declares filters" "$declared"
  assert_eq "every declared filter is passed as FILTER_<name>, and no others" \
    "$declared" "$wired"

}

test_every_filter_steers_something() {
  echo "Test: every filter moves at least one output"

  # A filter the resolver does not read is not an error anywhere: it defaults to
  # false, the paths it names stop scheduling anything, and the only symptom is
  # a job that quietly stops running. Rather than grep for a name the resolver
  # builds at runtime (filter_on constructs FILTER_<name>), set each filter on
  # its own and require the output to differ from the all-false baseline.
  local baseline
  baseline=$(resolve_outputs refs/heads/main "$ALL_OPS")

  # Two classes cannot move an output on their own, and are checked below
  # instead: publish_legacy is read on push events only, and the per-service
  # Tempest filters narrow a matrix that tempest_src has to switch on first.
  local exempt=" publish_legacy tempest_keystone tempest_glance tempest_barbican tempest_neutron "

  local name inert=""
  for name in $(declared_filters); do
    case "$exempt" in *" $name "*) continue ;; esac
    if [ "$(resolve_outputs refs/heads/main "$ALL_OPS" "FILTER_${name}=true")" = "$baseline" ]; then
      inert="${inert} ${name}"
    fi
  done
  assert_eq "no filter resolves to nothing at all" "" "$inert"

  # publish_legacy exists so a push to main publishes the operator set it
  # publishes today; it is ignored on pull requests by design.
  assert_eq "publish_legacy is inert on a pull request" \
    "$baseline" "$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_publish_legacy=true)"
  assert_contains "publish_legacy fills the publish matrix on a push" \
    "$(resolve_outputs refs/heads/main "$ALL_OPS" EVENT_NAME=push FILTER_publish_legacy=true)" \
    'e2e-operators={"operator":["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'

  # A tests/tempest/<svc>-*/ edit matches tempest_src as well, so the pair is
  # what CI ever sees; what the per-service filter has to do is narrow.
  assert_contains "tempest_src alone runs every service" \
    "$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_tempest_src=true)" \
    'tempest-services=["keystone","glance","barbican","neutron"]'
  local svc
  for svc in keystone glance barbican neutron; do
    assert_contains "tempest_${svc} narrows the matrix to ${svc}" \
      "$(resolve_outputs refs/heads/main "$ALL_OPS" FILTER_tempest_src=true "FILTER_tempest_${svc}=true")" \
      "tempest-services=[\"${svc}\"]"
  done
}

test_service_operators_match_the_release_configs() {
  echo "Test: SERVICE_OPERATORS is the union of the release source-refs keys"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  # The resolver derives changed-services and the per-service e2e legs from this
  # list. An operator that ships a service image but is missing here never gets
  # its image rebuilt for a pull request that changes it.
  local from_yaml from_ci f
  from_yaml=$(for f in "$PROJECT_ROOT"/releases/*/source-refs.yaml; do
    yq -r 'keys | .[]' "$f"
  done | sort -u | tr '\n' ' ' | sed 's/ *$//')
  from_ci=$(grep -oE '^          SERVICE_OPERATORS: .*$' "$CI_YAML" |
    sed 's/^ *SERVICE_OPERATORS: //' | tr ' ' '\n' | sort -u | tr '\n' ' ' | sed 's/ *$//')

  assert_eq "SERVICE_OPERATORS matches releases/*/source-refs.yaml" \
    "$from_yaml" "$from_ci"
}

test_every_job_gates_on_its_own_flag() {
  echo "Test: each job reads the output that belongs to it"

  local job
  for job in e2e-infra e2e-chaos e2e-prometheus e2e-controlplane \
    e2e-controlplane-sso e2e-external-keystone e2e-multicluster \
    e2e-operator-upgrade tempest actionlint; do
    assert_contains "$job gates on needs.changes.outputs.$job" \
      "$(job_block "$job")" "needs.changes.outputs.${job} == 'true'"
  done

  assert_contains "build-e2e-images gates on its own union flag" \
    "$(job_block build-e2e-images)" "needs.changes.outputs.build-e2e-images == 'true'"
  assert_contains "e2e-operator gates on the operator matrix" \
    "$(job_block e2e-operator)" "needs.changes.outputs.has-e2e-operators == 'true'"
}

test_label_handling_lives_in_the_resolver() {
  echo "Test: no job inspects the label set itself"

  # run-chaos used to be read inline in the e2e-chaos `if:`. With five labels
  # that pattern would put label logic in five places and leave the resolver's
  # own view of them incomplete.
  assert_file_not_contains "no job reads github.event.pull_request.labels" \
    "$CI_YAML" "contains(github.event.pull_request.labels"
  assert_file_contains "the resolver is handed the label set" "$CI_YAML" \
    "PR_LABELS: \${{ toJSON(github.event.pull_request.labels.\*.name) }}"
}

test_always_on_gates_skip_a_noop_run() {
  echo "Test: the always-on gates skip a run that resolved to nothing"

  local job
  for job in shellcheck feature-ids review-markers verify-invalid-cr-fixtures \
    chainsaw-lint; do
    assert_contains "$job skips a no-op run" \
      "$(job_block "$job")" "needs.changes.outputs.noop != 'true'"
  done

  # test-shell is deliberately exempt: it carries no `if:` at all so a breakage
  # that lands on main fails main's own run. tests/unit/ci/test_shell_event_guard_test.sh
  # pins that; asserting the absence here too would duplicate it.
  assert_not_contains "test-shell is not gated on the resolver" \
    "$(job_block test-shell)" "needs.changes.outputs"
}

test_test_matrices_come_from_the_resolver() {
  echo "Test: the unit and envtest matrices follow the changed operators"

  local matrix_line
  matrix_line=$(grep -c 'matrix: ${{ fromJson(needs.changes.outputs.test-targets) }}' "$CI_YAML")
  assert_eq "both test matrices read test-targets" "2" "$matrix_line"
  assert_file_not_contains "the hardcoded nine-target list is gone" \
    "$CI_YAML" "target: \[common, keystone"
}

test_concurrency_isolates_a_noop_label_event() {
  echo "Test: a non-CI label event gets a concurrency group of its own"

  local group
  group=$(grep -A2 '^concurrency:$' "$CI_YAML" | grep 'group:')

  assert_contains "a labeled event is routed by the label name" \
    "$group" "github.event.action == 'labeled'"
  assert_contains "a ci: label stays in the pull request's own group" \
    "$group" "startsWith(github.event.label.name, 'ci:')"
  assert_contains "the run-chaos alias stays in it too" \
    "$group" "github.event.label.name != 'run-chaos'"
  assert_contains "everything else gets a group of its own" \
    "$group" "format('noop-{0}', github.run_id)"
  assert_contains "the normal group is unchanged" \
    "$group" "format('{0}-{1}', github.ref, github.workflow)"
}

test_tempest_matrix_is_narrowed_by_service() {
  echo "Test: the Tempest matrix generator is told which services to emit"

  assert_file_contains "the generator step threads TEMPEST_SERVICES" "$CI_YAML" \
    "TEMPEST_SERVICES: \${{ join(fromJson(steps.result.outputs.tempest-services), ' ') }}"
  assert_file_contains "the generator reads it" \
    "$PROJECT_ROOT/hack/ci-generate-tempest-matrix.sh" "TEMPEST_SERVICES"
}

test_actionlint_job_is_pinned() {
  echo "Test: the actionlint job pins and verifies the binary it installs"

  local block
  block=$(job_block actionlint)
  assert_not_empty "the job exists" "$block"
  assert_contains "the version is pinned" "$block" "ACTIONLINT_VERSION: "
  assert_contains "the download is checksummed" "$block" "sha256sum --check --strict"

  local sha
  sha=$(grep -oE 'ACTIONLINT_SHA256: [0-9a-f]+' "$CI_YAML" | awk '{print $2}')
  assert_eq "the checksum is a full sha256" "64" "${#sha}"

  # actionlint validates runs-on against GitHub's own label list, so every
  # self-hosted pool the workflows use has to be declared or the lint fails on a
  # runner that exists.
  assert_file_contains "the Blacksmith runner label is declared" \
    "$ACTIONLINT_CONFIG" "blacksmith-4vcpu-ubuntu-2404"
}

test_no_job_reads_an_unexported_output() {
  echo "Test: every needs.changes.outputs.<name> a job reads is exported"

  # The mechanical guard: an unexported name is not an error in Actions, it is
  # the empty string, and the job it gates simply never runs again.
  local referenced exported missing=""
  referenced=$(grep -oE 'needs\.changes\.outputs\.[a-z0-9-]+' "$CI_YAML" |
    sed 's/.*outputs\.//' | sort -u)
  exported=$(awk '
    /^    outputs:$/ { in_block = 1; next }
    in_block && /^    [a-z]/ { exit }
    in_block && /:/ { sub(/:.*/, "", $1); print $1 }
  ' "$CI_YAML" | sort -u)

  local name
  for name in $referenced; do
    grep -qx -- "$name" <<<"$exported" || missing="${missing} ${name}"
  done
  assert_eq "no job reads an output the changes job does not export" "" "$missing"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_every_filter_is_wired_both_ways
test_every_filter_steers_something
test_service_operators_match_the_release_configs
test_every_job_gates_on_its_own_flag
test_label_handling_lives_in_the_resolver
test_always_on_gates_skip_a_noop_run
test_test_matrices_come_from_the_resolver
test_concurrency_isolates_a_noop_label_event
test_tempest_matrix_is_narrowed_by_service
test_actionlint_job_is_pinned
test_no_job_reads_an_unexported_output

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
