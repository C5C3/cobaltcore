#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Pin hack/ci-resolve-changes.sh to the change classes it implements: one
# scenario per class, each asserting the outputs that class is supposed to move
# AND the expensive ones it must leave alone.
#
# The second half is what this file is really for. Every failure mode of the
# gating is silent: an over-broad class burns runner hours nobody notices, and a
# too-narrow one leaves a job unscheduled, which reads as a green pull request.
# Neither shows up in a workflow run you would think to look at, so each scenario
# states the full expected shape rather than only the flag it is named after.
#
# The script is executed for real through tests/lib/ci_resolve.sh; nothing here
# reimplements its logic.
#
# Usage: bash tests/unit/ci/resolve_changes_scenarios_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# The real lists from the ci.yaml resolve step. A shorter ALL_OPERATORS would
# make the per-operator scenarios assert nothing.
ALL_OPS="keystone c5c3 horizon glance placement barbican ovn neutron"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_resolve.sh
source "$PROJECT_ROOT/tests/lib/ci_resolve.sh"

# ---------------------------------------------------------------------------
# Harness
# ---------------------------------------------------------------------------

CURRENT=""
CURRENT_NAME=""

# scenario <name> <ref> [ENV=value ...]
scenario() {
  CURRENT_NAME="$1"
  local ref="$2"
  shift 2
  echo "Scenario: $CURRENT_NAME"
  CURRENT=$(resolve_outputs "$ref" "$ALL_OPS" "$@")
}

# expect <output-key> <value>
expect() {
  local key="$1" want="$2" got
  got=$(printf '%s\n' "$CURRENT" | grep "^${key}=" | head -1)
  assert_eq "$CURRENT_NAME: ${key}=${want}" "${key}=${want}" "$got"
}

# expect_all <value> <output-key>... — the same value for several outputs.
expect_all() {
  local want="$1"
  shift
  local key
  for key in "$@"; do expect "$key" "$want"; done
}

# The jobs a cheap change must never schedule.
EXPENSIVE="e2e-chaos e2e-prometheus e2e-controlplane e2e-controlplane-sso e2e-external-keystone e2e-multicluster tempest"

SENTINEL_TARGETS='{"target":["__none__"]}'
SENTINEL_OPERATORS='{"operator":["__none__"]}'

# ---------------------------------------------------------------------------
# Per-operator and shared Go
# ---------------------------------------------------------------------------

test_operator_change() {
  scenario "a glance operator change" refs/heads/main FILTER_glance=true
  expect go true
  expect test-targets '{"target":["glance"]}'
  expect e2e-operators '{"operator":["glance"]}'
  expect changed-operators '["glance"]'
  expect changed-services '[]'
  expect build-operators '["glance"]'
  expect build-e2e-images true
  expect tempest-services '[]'
  # shellcheck disable=SC2086 # deliberate: expand the job list
  expect_all false $EXPENSIVE e2e-infra e2e-operator-upgrade actionlint noop
}

test_c5c3_change_runs_the_controlplane_trio() {
  # The c5c3 e2e leg installs only the sibling CRDs, so the ControlPlane jobs
  # are where that operator is actually exercised.
  scenario "a c5c3 operator change" refs/heads/main FILTER_c5c3=true
  expect e2e-operators '{"operator":["c5c3"]}'
  expect_all true e2e-controlplane e2e-controlplane-sso e2e-external-keystone
  expect build-operators '["keystone","c5c3","horizon","glance","placement","barbican"]'
  expect_all false e2e-chaos tempest e2e-multicluster e2e-prometheus
}

test_keystone_change_runs_the_upgrade_suite() {
  scenario "a keystone operator change" refs/heads/main FILTER_keystone=true
  expect e2e-operators '{"operator":["keystone"]}'
  expect e2e-operator-upgrade true
  expect build-operators '["keystone"]'
  expect_all false e2e-controlplane tempest e2e-chaos
}

test_shared_go_change() {
  # Every operator's code is affected, so every unit, envtest and e2e leg runs
  # and the upgrade suite with them. The ControlPlane trio, chaos, multicluster,
  # prometheus and tempest stay off: they are the most expensive jobs in the
  # pipeline and ci:full is how you ask for them.
  scenario "a shared Go change" refs/heads/main FILTER_go_common=true
  expect test-targets '{"target":["common","keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'
  expect e2e-operators '{"operator":["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'
  expect changed-operators '["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]'
  expect changed-services '[]'
  expect e2e-operator-upgrade true
  expect e2e-infra false
  # shellcheck disable=SC2086 # deliberate: expand the job list
  expect_all false $EXPENSIVE
}

# ---------------------------------------------------------------------------
# Suite and image classes
# ---------------------------------------------------------------------------

test_suite_edit_runs_one_leg() {
  scenario "an edit to one operator's e2e suite" refs/heads/main FILTER_tests_e2e_glance=true
  expect go false
  expect test-targets "$SENTINEL_TARGETS"
  expect e2e-operators '{"operator":["glance"]}'
  expect changed-operators '[]'
  expect build-operators '["glance"]'
}

test_service_image_change() {
  scenario "a glance service image change" refs/heads/main FILTER_image_glance=true
  expect changed-services '["glance"]'
  expect changed-operators '[]'
  expect e2e-operators '{"operator":["glance"]}'
  expect go false
}

test_base_image_change_covers_every_service() {
  scenario "a base image change" refs/heads/main FILTER_images_base=true
  expect changed-services '["keystone","horizon","glance","placement","barbican","neutron"]'
  expect changed-tempest true
  expect e2e-operators '{"operator":["keystone","horizon","glance","placement","barbican","neutron"]}'
  expect changed-operators '[]'
}

test_federation_proxy_change() {
  scenario "a federation proxy image change" refs/heads/main FILTER_image_proxy=true
  expect changed-proxy true
  expect e2e-operators '{"operator":["keystone"]}'
}

test_special_suites_run_only_their_own_job() {
  scenario "a chaos suite edit" refs/heads/main FILTER_tests_chaos=true
  expect e2e-chaos true
  expect_all false e2e-controlplane e2e-multicluster e2e-prometheus tempest

  scenario "a multicluster suite edit" refs/heads/main FILTER_tests_multicluster=true
  expect e2e-multicluster true
  expect_all false e2e-chaos e2e-controlplane tempest

  scenario "a prometheus suite edit" refs/heads/main FILTER_tests_prometheus=true
  expect e2e-prometheus true
  expect_all false e2e-chaos e2e-controlplane tempest

  scenario "an operator-upgrade suite edit" refs/heads/main FILTER_tests_operator_upgrade=true
  expect e2e-operator-upgrade true
  expect_all false e2e-chaos e2e-controlplane tempest

  scenario "a ControlPlane SSO suite edit" refs/heads/main FILTER_tests_controlplane_sso=true
  expect e2e-controlplane-sso true
  expect_all false e2e-controlplane e2e-external-keystone
}

# ---------------------------------------------------------------------------
# The canary
# ---------------------------------------------------------------------------

test_shared_substrate_runs_the_canary() {
  scenario "a change to the shared e2e substrate" refs/heads/main FILTER_e2e_shared=true
  expect go false
  expect e2e-infra true
  expect e2e-operators '{"operator":["keystone"]}'
  expect changed-operators '[]'
  expect build-operators '["keystone"]'
  expect actionlint false
  # shellcheck disable=SC2086 # deliberate: expand the job list
  expect_all false $EXPENSIVE
}

test_openbao_change_adds_barbican() {
  scenario "an OpenBao deploy change" refs/heads/main FILTER_e2e_shared=true FILTER_e2e_openbao=true
  expect e2e-infra true
  expect e2e-operators '{"operator":["keystone","barbican"]}'
  expect build-operators '["keystone","barbican"]'
}

test_workflow_plumbing_runs_the_canary_and_actionlint() {
  scenario "a workflow plumbing change" refs/heads/main FILTER_ci_plumbing=true FILTER_actionlint=true
  expect e2e-infra true
  expect e2e-operators '{"operator":["keystone"]}'
  expect actionlint true
}

test_makefile_change() {
  scenario "a Makefile change" refs/heads/main FILTER_makefile=true FILTER_helm=true
  expect go true
  expect test-targets '{"target":["common","keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'
  expect helm true
  expect e2e-infra true
  expect e2e-operators '{"operator":["keystone"]}'
  expect changed-operators '["keystone"]'
  expect e2e-operator-upgrade false
}

test_docs_only_change() {
  scenario "a docs-only change" refs/heads/main FILTER_docs=true
  expect docs true
  expect go false
  expect has-e2e-operators false
  expect e2e-operators "$SENTINEL_OPERATORS"
  expect build-e2e-images false
  expect e2e-infra false
  expect noop false
}

# ---------------------------------------------------------------------------
# Tempest
# ---------------------------------------------------------------------------

test_tempest_config_narrows_to_its_service() {
  scenario "a glance tempest config edit" refs/heads/main \
    FILTER_tempest_src=true FILTER_tempest_glance=true
  expect go false
  expect tempest true
  expect tempest-services '["glance"]'
  expect has-e2e-operators false
  expect build-operators '["keystone","glance"]'
  expect build-e2e-images true
}

test_shared_tempest_change_covers_every_service() {
  scenario "a tempest runner change" refs/heads/main FILTER_tempest_src=true
  expect tempest true
  expect tempest-services '["keystone","glance","barbican"]'
}

# ---------------------------------------------------------------------------
# Labels
# ---------------------------------------------------------------------------

test_ci_full_runs_everything() {
  scenario "ci:full on a glance pull request" refs/heads/main \
    FILTER_glance=true PR_LABELS='["ci:full"]'
  # shellcheck disable=SC2086 # deliberate: expand the job list
  expect_all true $EXPENSIVE go docs helm target-cluster-chart e2e-infra \
    e2e-operator-upgrade actionlint changed-tempest changed-proxy build-e2e-images \
    has-e2e-operators
  expect noop false
  expect test-targets '{"target":["common","keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'
  expect e2e-operators '{"operator":["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'
  expect changed-operators '["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]'
  expect changed-services '["keystone","horizon","glance","placement","barbican","neutron"]'
  expect tempest-services '["keystone","glance","barbican"]'
}

test_ci_tempest_follows_the_touched_service() {
  scenario "ci:tempest on a glance pull request" refs/heads/main \
    FILTER_glance=true PR_LABELS='["ci:tempest"]'
  expect tempest true
  expect tempest-services '["glance"]'
  expect build-operators '["keystone","glance"]'
  expect_all false e2e-chaos e2e-controlplane

  scenario "ci:tempest with no service touched" refs/heads/main \
    FILTER_docs=true PR_LABELS='["ci:tempest"]'
  expect tempest true
  expect tempest-services '["keystone"]'
  expect build-operators '["keystone"]'
  expect build-e2e-images true
}

test_ci_chaos_and_its_alias() {
  local expected_build='["keystone","horizon","glance","placement","barbican"]'

  scenario "ci:chaos on a glance pull request" refs/heads/main \
    FILTER_glance=true PR_LABELS='["ci:chaos"]'
  expect e2e-chaos true
  expect build-operators "$expected_build"

  # run-chaos predates the ci: prefix and stays an alias, so an existing habit
  # keeps working.
  scenario "the run-chaos alias" refs/heads/main \
    FILTER_glance=true PR_LABELS='["run-chaos"]'
  expect e2e-chaos true
  expect build-operators "$expected_build"
}

test_ci_controlplane_and_multicluster() {
  scenario "ci:controlplane on a glance pull request" refs/heads/main \
    FILTER_glance=true PR_LABELS='["ci:controlplane"]'
  expect_all true e2e-controlplane e2e-controlplane-sso e2e-external-keystone
  expect build-operators '["keystone","c5c3","horizon","glance","placement","barbican"]'

  scenario "ci:multicluster on its own" refs/heads/main PR_LABELS='["ci:multicluster"]'
  expect e2e-multicluster true
  expect build-operators '["keystone","barbican"]'
  expect has-e2e-operators false
}

# ---------------------------------------------------------------------------
# The labeled no-op
# ---------------------------------------------------------------------------

test_non_ci_label_resolves_to_nothing() {
  # Adding `bug` to a pull request must not cancel and restart its pipeline.
  # The workflow gives such an event a concurrency group of its own; this
  # resolves it to nothing so every gated job skips.
  scenario "a non-CI label event" refs/heads/main \
    FILTER_glance=true EVENT_ACTION=labeled EVENT_LABEL=bug
  expect noop true
  expect go false
  expect has-e2e-operators false
  expect build-e2e-images false
  expect test-targets "$SENTINEL_TARGETS"
  expect e2e-operators "$SENTINEL_OPERATORS"
  expect changed-operators '[]'
  expect tempest false
}

test_ci_label_event_is_not_a_noop() {
  scenario "a ci: label event" refs/heads/main \
    FILTER_glance=true EVENT_ACTION=labeled EVENT_LABEL=ci:tempest \
    PR_LABELS='["ci:tempest"]'
  expect noop false
  expect tempest true

  scenario "a run-chaos label event" refs/heads/main \
    EVENT_ACTION=labeled EVENT_LABEL=run-chaos PR_LABELS='["run-chaos"]'
  expect noop false
  expect e2e-chaos true

  # An unrecognised ci: label steers nothing, but it must not be treated as a
  # no-op either: the run still has to evaluate the paths.
  scenario "an unknown ci: label event" refs/heads/main \
    EVENT_ACTION=labeled EVENT_LABEL=ci:unknown PR_LABELS='["ci:unknown"]'
  expect noop false
  expect tempest false
  expect e2e-chaos false
  expect has-e2e-operators false

  scenario "a synchronize event carrying a non-CI label" refs/heads/main \
    FILTER_glance=true EVENT_ACTION=synchronize EVENT_LABEL=bug
  expect noop false
  expect e2e-operators '{"operator":["glance"]}'
}

# ---------------------------------------------------------------------------
# Tag and push events
# ---------------------------------------------------------------------------

test_tag_push_forces_everything() {
  scenario "a v* tag push" refs/tags/v1.2.3
  # shellcheck disable=SC2086 # deliberate: expand the job list
  expect_all true $EXPENSIVE go docs helm target-cluster-chart e2e-infra \
    e2e-operator-upgrade has-e2e-operators
  expect noop false
  expect e2e-operators '{"operator":["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'
  expect changed-operators '["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]'
  expect changed-services '["keystone","horizon","glance","placement","barbican","neutron"]'
  expect tempest-services '["keystone","glance","barbican"]'
}

test_push_keeps_the_publish_matrix() {
  # The publish jobs read e2e-operators on push. What they publish must not
  # change, so these four cases pin today's behaviour.
  scenario "a push touching one operator" refs/heads/main \
    EVENT_NAME=push FILTER_glance=true
  expect e2e-operators '{"operator":["glance"]}'
  expect has-e2e-operators true

  scenario "a push touching one service image" refs/heads/main \
    EVENT_NAME=push FILTER_image_glance=true
  expect e2e-operators '{"operator":["glance"]}'

  scenario "a push touching a shared publish path" refs/heads/main \
    EVENT_NAME=push FILTER_publish_legacy=true
  expect e2e-operators '{"operator":["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'

  scenario "a push touching shared Go" refs/heads/main \
    EVENT_NAME=push FILTER_go_common=true
  expect e2e-operators '{"operator":["keystone","c5c3","horizon","glance","placement","barbican","ovn","neutron"]}'

  scenario "a docs-only push" refs/heads/main EVENT_NAME=push FILTER_docs=true
  expect has-e2e-operators false
  expect e2e-operators "$SENTINEL_OPERATORS"
  expect build-e2e-images false
}

# ---------------------------------------------------------------------------
# Shadow paths
# ---------------------------------------------------------------------------

test_no_filters_at_all() {
  scenario "no filter matched" refs/heads/main
  # shellcheck disable=SC2086 # deliberate: expand the job list
  expect_all false $EXPENSIVE go docs helm target-cluster-chart e2e-infra \
    e2e-operator-upgrade actionlint changed-tempest changed-proxy \
    build-e2e-images has-e2e-operators noop
  expect test-targets "$SENTINEL_TARGETS"
  expect e2e-operators "$SENTINEL_OPERATORS"
  expect changed-operators '[]'
  expect changed-services '[]'
  expect build-operators '[]'
  expect tempest-services '[]'
}

test_malformed_labels_mean_no_labels() {
  # PR_LABELS is rendered by toJSON(), which yields null on a push event. None
  # of these forms may switch a job on.
  local form
  for form in '' 'null' '"x"' '{"a":1}' 'not json'; do
    scenario "PR_LABELS=${form:-<empty>}" refs/heads/main PR_LABELS="$form"
    expect tempest false
    expect e2e-chaos false
    expect e2e-multicluster false
  done
}

test_missing_required_inputs_fail_loudly() {
  echo "Scenario: the resolve script refuses to guess its operator lists"

  local rc out
  for var in ALL_OPERATORS SERVICE_OPERATORS; do
    out=$(mktemp)
    env ALL_OPERATORS="$ALL_OPS" SERVICE_OPERATORS="keystone" "$var=" \
      GITHUB_OUTPUT="$out" GITHUB_REF=refs/heads/main \
      bash "$PROJECT_ROOT/hack/ci-resolve-changes.sh" >/dev/null 2>&1
    rc=$?
    assert_nonzero_exit "an empty $var fails loudly" "$rc"
    rm -f "$out"
  done
}

test_outputs_are_complete() {
  # Every output ci.yaml reads must be emitted on every path, or the consuming
  # job silently resolves it to the empty string and skips forever.
  scenario "the emitted output set" refs/heads/main FILTER_glance=true
  local expected key
  expected="noop go docs helm target-cluster-chart e2e-infra e2e-chaos
    e2e-prometheus e2e-controlplane e2e-controlplane-sso e2e-external-keystone
    e2e-multicluster e2e-operator-upgrade tempest actionlint changed-tempest
    changed-proxy build-e2e-images has-e2e-operators changed-operators
    changed-services build-operators tempest-services test-targets e2e-operators"
  for key in $expected; do
    assert_not_empty "the resolver emits $key" \
      "$(printf '%s\n' "$CURRENT" | grep "^${key}=")"
  done

  # And the same set on the no-op path, where an omission would leave a job
  # reading an empty string instead of an explicit false.
  scenario "the emitted output set on a no-op run" refs/heads/main \
    EVENT_ACTION=labeled EVENT_LABEL=bug
  for key in $expected; do
    assert_not_empty "the no-op path emits $key" \
      "$(printf '%s\n' "$CURRENT" | grep "^${key}=")"
  done
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_operator_change
test_c5c3_change_runs_the_controlplane_trio
test_keystone_change_runs_the_upgrade_suite
test_shared_go_change
test_suite_edit_runs_one_leg
test_service_image_change
test_base_image_change_covers_every_service
test_federation_proxy_change
test_special_suites_run_only_their_own_job
test_shared_substrate_runs_the_canary
test_openbao_change_adds_barbican
test_workflow_plumbing_runs_the_canary_and_actionlint
test_makefile_change
test_docs_only_change
test_tempest_config_narrows_to_its_service
test_shared_tempest_change_covers_every_service
test_ci_full_runs_everything
test_ci_tempest_follows_the_touched_service
test_ci_chaos_and_its_alias
test_ci_controlplane_and_multicluster
test_non_ci_label_resolves_to_nothing
test_ci_label_event_is_not_a_noop
test_tag_push_forces_everything
test_push_keeps_the_publish_matrix
test_no_filters_at_all
test_malformed_labels_mean_no_labels
test_missing_required_inputs_fail_loudly
test_outputs_are_complete

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
