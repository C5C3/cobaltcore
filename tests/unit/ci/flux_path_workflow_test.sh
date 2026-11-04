#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the shape of .github/workflows/flux-path.yaml, the nightly that brings
# up the quick start ControlPlane on deploy-infra's default operator path:
#   - the triggers (a 02:45 UTC schedule, workflow_dispatch, and pull_request
#     on the workflow and its two scripts), top-level permissions of contents:
#     read alone, a concurrency group per event and ref that cancels only pull
#     request runs, KIND_CLUSTER and a KIND_VERSION pin, and no operator path
#     or GHCR login of its own;
#   - job controlplane: the fork guard, self-hosted, a 90-minute wall, no
#     permissions of its own and its eight steps in order, the two artifact
#     actions SHA-pinned, with WITH_CONTROLPLANE as the only env of the
#     infrastructure step, the failure steps under failure() and the cluster
#     deletion last under always();
#   - job report: after controlplane, only for a red scheduled run, on
#     ubuntu-latest with a 5-minute wall, the only holder of issues: write,
#     and the seven inputs of hack/ci-report-red-run.sh;
#   - a copy whose top-level permissions add issues: write, a copy whose
#     controlplane job grants itself a write, and a copy whose infrastructure
#     step picks the external operators fail the checks.
#
# Usage: bash tests/unit/ci/flux_path_workflow_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

WORKFLOW="$PROJECT_ROOT/.github/workflows/flux-path.yaml"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$WORKFLOW"

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

SETUP_STEP="Setup E2E infrastructure"
REPORT_STEP="Report the red night"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# step_heads <file> <job> — Echo each step of the job in order: the name of a
# named step, the action without its pin of an unnamed `- uses:` step.
step_heads() {
  CI_YAML="$1" job_block "$2" | sed -n -e 's/^      - name: //p' -e 's/^      - uses: \([^@]*\)@.*/\1/p'
}

# step_env <file> <job> <step> — Echo the entries of the step's env block.
step_env() {
  CI_YAML="$1" job_step "$2" "$3" | awk '
    /^        env:$/ { in_env = 1; next }
    in_env && /^          / { print; next }
    in_env { exit }
  '
}

# pull_request_paths <file> — Echo the paths filter of the pull_request trigger.
pull_request_paths() {
  awk '
    /^  pull_request:$/ { in_pr = 1; next }
    in_pr && /^    paths:$/ { in_paths = 1; next }
    in_paths && /^      - / { print; next }
    in_pr && /^  [^ ]/ { exit }
    in_paths { exit }
  ' "$1"
}

# top_level_permissions <file> — Echo the entries of the workflow's
# permissions block.
top_level_permissions() {
  awk '/^permissions:$/ { p = 1; next } p && /^[^ ]/ { exit } p' "$1"
}

# fails_on <check> <file> — Run the check against <file> on counters of its
# own, quietly, and echo how many of its assertions failed.
fails_on() {
  (
    PASS=0
    FAIL=0
    "$1" "$2" >/dev/null
    echo "$FAIL"
  )
}

# ---------------------------------------------------------------------------
# Checks: each takes the workflow file, so it runs on the real file and on
# the scratch copies below.
# ---------------------------------------------------------------------------

check_triggers() {
  local wf="$1"
  assert_file_contains "the workflow is named Flux path" "$wf" '^name: Flux path$'
  assert_file_contains_fixed "it runs nightly at 02:45 UTC" "$wf" 'cron: "45 2 * * *"'
  assert_file_contains "it can be dispatched" "$wf" '^  workflow_dispatch:'
  assert_eq "pull_request runs on the workflow and its two scripts" \
    "      - '.github/workflows/flux-path.yaml'
      - 'hack/ci-quick-start-controlplane.sh'
      - 'hack/ci-report-red-run.sh'" "$(pull_request_paths "$wf")"
}

check_top_level_permissions() {
  local wf="$1"
  assert_eq "the top-level permissions are contents: read alone" \
    "  contents: read" "$(top_level_permissions "$wf")"
  assert_eq "issues: write appears once, in the report job" "1" \
    "$(grep -c 'issues: write' "$wf" | tr -d ' ')"
}

check_concurrency_and_env() {
  local wf="$1"
  assert_file_contains_fixed "a night has a group of its own, so no dispatch replaces a pending one" "$wf" \
    'group: flux-path-${{ github.event_name }}-${{ github.ref }}'
  assert_file_contains_fixed "only a pull request run is cancelled by a newer one" "$wf" \
    "cancel-in-progress: \${{ github.event_name == 'pull_request' }}"
  assert_file_contains_fixed "KIND_CLUSTER is cobaltcore" "$wf" 'KIND_CLUSTER: cobaltcore'
  # tests/unit/renovate/workflow_pins_custommanager_test.sh asserts the pin
  # agrees with the other four.
  assert_not_empty "flux-path.yaml pins KIND_VERSION" "$(workflow_kind_pin "$wf")"
  assert_file_not_contains "deploy-infra's operator path is its default, named nowhere" \
    "$wf" 'CONTROLPLANE_OPERATORS'
  assert_file_not_contains "the night pulls anonymously, as a user does" "$wf" 'registry-login'
}

check_controlplane_job() {
  local wf="$1" job step
  assert_eq "the jobs are controlplane and report" "controlplane report" \
    "$(CI_YAML="$wf" job_names | paste -sd' ' -)"
  job="$(CI_YAML="$wf" job_block controlplane)"
  assert_contains "a fork's pull request does not run it" "$job" \
    "    if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.fork != true"
  assert_contains "it runs on self-hosted" "$job" "    runs-on: self-hosted"
  assert_contains "its wall is 90 minutes" "$job" "    timeout-minutes: 90"
  assert_not_contains "it widens no permission of the top level" "$job" "    permissions:"
  assert_eq "its eight steps, in order" \
    "actions/checkout
Create kind cluster
Setup E2E infrastructure
Bring up the quick start ControlPlane
Summarize the failure
Dump diagnostic info
Upload the summary
Delete kind cluster" "$(step_heads "$wf" controlplane)"

  step="$(CI_YAML="$wf" job_step controlplane "Create kind cluster")"
  assert_contains "the cluster comes from create-kind-cluster" "$step" "uses: ./.github/actions/create-kind-cluster"
  assert_contains "at KIND_VERSION" "$step" 'version: ${{ env.KIND_VERSION }}'
  assert_contains "named KIND_CLUSTER" "$step" 'cluster-name: ${{ env.KIND_CLUSTER }}'
  assert_contains "with hack/kind-config.yaml" "$step" "config: hack/kind-config.yaml"

  step="$(CI_YAML="$wf" job_step controlplane "$SETUP_STEP")"
  assert_contains "the stack comes from setup-e2e-infra" "$step" "uses: ./.github/actions/setup-e2e-infra"

  step="$(CI_YAML="$wf" job_step controlplane "Bring up the quick start ControlPlane")"
  assert_contains "the bring-up runs up" "$step" "run: hack/ci-quick-start-controlplane.sh up"
  assert_not_contains "and runs under no if:" "$step" "if:"

  step="$(CI_YAML="$wf" job_step controlplane "Summarize the failure")"
  assert_contains "the summary runs on failure" "$step" "if: failure()"
  assert_contains "it writes _output/flux-path/summary.md" "$step" \
    "hack/ci-quick-start-controlplane.sh summary _output/flux-path/summary.md"
  assert_contains "and appends it to the job summary" "$step" \
    'cat _output/flux-path/summary.md >> "$GITHUB_STEP_SUMMARY"'

  step="$(CI_YAML="$wf" job_step controlplane "Dump diagnostic info")"
  assert_contains "the diagnostics run on failure" "$step" "if: failure()"
  assert_contains "through ci-dump-diagnostics.sh" "$step" "run: hack/ci-dump-diagnostics.sh"
  assert_contains "for the c5c3 operator" "$step" "OPERATOR: c5c3"

  step="$(CI_YAML="$wf" job_step controlplane "Upload the summary")"
  assert_contains "the summary is uploaded on failure" "$step" "if: failure()"
  assert_not_empty "with a SHA-pinned upload-artifact" \
    "$(grep -E '^        uses: actions/upload-artifact@[0-9a-f]{40} # v[0-9]' <<<"$step")"
  assert_contains "named flux-path-summary" "$step" "name: flux-path-summary"
  assert_contains "holding the summary file" "$step" "path: _output/flux-path/summary.md"
  assert_contains "kept 14 days" "$step" "retention-days: 14"

  step="$(CI_YAML="$wf" job_step controlplane "Delete kind cluster")"
  assert_contains "the cluster is deleted always" "$step" "if: always()"
  assert_contains "by ci-delete-kind-cluster.sh" "$step" "run: hack/ci-delete-kind-cluster.sh"
}

check_setup_infra() {
  local wf="$1"
  assert_eq "WITH_CONTROLPLANE is the only env of the infrastructure step" \
    '          WITH_CONTROLPLANE: "true"' "$(step_env "$wf" controlplane "$SETUP_STEP")"
}

check_report_job() {
  local wf="$1" job step
  job="$(CI_YAML="$wf" job_block report)"
  assert_contains "it runs after controlplane" "$job" "    needs: controlplane"
  assert_contains "only for a red scheduled run" "$job" \
    "    if: always() && github.event_name == 'schedule' && needs.controlplane.result != 'success'"
  assert_contains "on ubuntu-latest" "$job" "    runs-on: ubuntu-latest"
  assert_contains "with a 5-minute wall" "$job" "    timeout-minutes: 5"
  assert_eq "it holds contents: read and issues: write" \
    "      contents: read
      issues: write" "$(awk '/^    permissions:$/ { p = 1; next } p && /^      [a-z]/ { print; next } p { exit }' <<<"$job")"
  assert_eq "its steps, in order" \
    "actions/checkout
Download the summary
$REPORT_STEP" "$(step_heads "$wf" report)"

  step="$(CI_YAML="$wf" job_step report "Download the summary")"
  assert_not_empty "with a SHA-pinned download-artifact" \
    "$(grep -E '^        uses: actions/download-artifact@[0-9a-f]{40} # v[0-9]' <<<"$step")"
  assert_contains "of flux-path-summary" "$step" "name: flux-path-summary"
  assert_contains "into _output/flux-path" "$step" "path: _output/flux-path"
  assert_contains "a missing artifact does not stop the report" "$step" "continue-on-error: true"

  step="$(CI_YAML="$wf" job_step report "$REPORT_STEP")"
  assert_contains "the report runs ci-report-red-run.sh" "$step" "run: hack/ci-report-red-run.sh"
  assert_eq "with the seven inputs of the script" \
    '          GH_TOKEN: ${{ github.token }}
          GH_REPO: ${{ github.repository }}
          REPORT_TITLE: Scheduled Flux path run is red
          REPORT_RESULT: ${{ needs.controlplane.result }}
          RUN_URL: ${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}
          REPORT_SHA: ${{ github.sha }}
          SUMMARY_FILE: _output/flux-path/summary.md' "$(step_env "$wf" report "$REPORT_STEP")"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_workflow() {
  echo "Test: triggers, permissions, concurrency and env"
  check_triggers "$WORKFLOW"
  check_top_level_permissions "$WORKFLOW"
  check_concurrency_and_env "$WORKFLOW"

  echo "Test: job controlplane"
  check_controlplane_job "$WORKFLOW"
  check_setup_infra "$WORKFLOW"

  echo "Test: job report"
  check_report_job "$WORKFLOW"
}

test_a_copy_granting_issues_write_at_the_top_fails() {
  echo "Test: a copy whose top-level permissions add issues: write fails"

  local copy="$TMP_DIR/top-issues-write.yaml"
  awk '{ print } /^permissions:$/ { print "  issues: write" }' "$WORKFLOW" >"$copy"
  assert_contains "the copy grants issues: write at the top" \
    "$(top_level_permissions "$copy")" "  issues: write"
  assert_gte "the permission check fails on it" "$(fails_on check_top_level_permissions "$copy")" 1
}

test_a_copy_granting_the_self_hosted_job_a_write_fails() {
  echo "Test: a copy whose controlplane job grants itself a write fails"

  local copy="$TMP_DIR/job-write.yaml"
  awk '{ print } /^  controlplane:$/ { print "    permissions:"; print "      contents: write" }' \
    "$WORKFLOW" >"$copy"
  assert_contains "the copy grants the job contents: write" \
    "$(CI_YAML="$copy" job_block controlplane)" "      contents: write"
  assert_gte "the job check fails on it" "$(fails_on check_controlplane_job "$copy")" 1
}

test_a_copy_choosing_external_operators_fails() {
  echo "Test: a copy whose infrastructure step picks the external operators fails"

  local copy="$TMP_DIR/external-operators.yaml"
  awk '{ print } /^          WITH_CONTROLPLANE: "true"$/ { print "          CONTROLPLANE_OPERATORS: external" }' \
    "$WORKFLOW" >"$copy"
  assert_contains "the copy sets the external operators" \
    "$(step_env "$copy" controlplane "$SETUP_STEP")" "CONTROLPLANE_OPERATORS: external"
  assert_gte "the env check fails on it" "$(fails_on check_setup_infra "$copy")" 1
  assert_gte "and so does the check for the name" "$(fails_on check_concurrency_and_env "$copy")" 1
}

if [ ! -f "$WORKFLOW" ]; then
  echo "  FAIL: $WORKFLOW is missing"
  FAIL=$((FAIL + 1))
else
  test_the_workflow
  test_a_copy_granting_issues_write_at_the_top_fails
  test_a_copy_granting_the_self_hosted_job_a_write_fails
  test_a_copy_choosing_external_operators_fails
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
