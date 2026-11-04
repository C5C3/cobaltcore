#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the nightly chaos run in .github/workflows/ci.yaml (#1388):
#   - the triggers: a 22:00 UTC schedule and workflow_dispatch beside push and
#     pull_request;
#   - the concurrency group names the event, so a scheduled or dispatched run
#     never shares the push group, and is never cancelled by a newer run;
#   - the changes job skips its paths filter on the night;
#   - build-e2e-images and e2e-chaos accept the night, and the image build
#     keeps its fork guard;
#   - job e2e-chaos-random (#1395): on a pull request and on the night when
#     the resolver turns it on, after the image build and the Go gates, on
#     self-hosted with a 120-minute wall and contents: read, packages: read;
#     up within 40 minutes and run within 65 with the run id as the seed; the
#     JUnit report uploaded as e2e-chaos-junit-report-random, which the report
#     job downloads, and the kind cluster deleted always; cleanup-e2e-tags
#     waits for it;
#   - job report-nightly-chaos: after changes, build-e2e-images, e2e-chaos and
#     e2e-chaos-random, only on a scheduled run, on ubuntu-latest with a
#     10-minute wall, contents: read, issues: write and actions: read, the
#     JUnit reports downloaded without failing the job, and the seven inputs
#     of hack/ci-report-nightly-chaos.sh with a budget of 720 minutes;
#   - no self-hosted job of ci.yaml holds issues: write, and the report job
#     is the only holder;
#   - a copy whose concurrency group drops the event, a copy whose e2e-chaos job
#     grants itself issues: write, and a copy whose report job runs on
#     self-hosted fail the checks.
#
# The resolver's night outputs are pinned by resolve_changes_scenarios_test.sh,
# the --fail-fast line by e2e_chaos_job_test.sh, and the reporter by
# tests/unit/hack/ci_report_nightly_chaos_test.sh.
#
# Usage: bash tests/unit/ci/nightly_chaos_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

WORKFLOW="$PROJECT_ROOT/.github/workflows/ci.yaml"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$WORKFLOW"

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

REPORT_JOB="report-nightly-chaos"
NIGHT_EVENTS="(github.event_name == 'pull_request' || github.event_name == 'schedule' || github.event_name == 'workflow_dispatch') &&"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# on_block <file> <event> — Echo the body of the event under the workflow's
# "on": key.
on_block() {
  awk -v key="  $2:" '
    /^"on":$/ { in_on = 1; next }
    in_on && /^[^ #]/ { exit }
    in_on && $0 == key { in_event = 1; next }
    in_event && /^  [a-z#]/ { exit }
    in_event { print }
  ' "$1"
}

# permissions_of <file> <job> — Echo the entries of the job's permissions
# block.
permissions_of() {
  CI_YAML="$1" job_block "$2" |
    awk '/^    permissions:$/ { p = 1; next } p && /^      [a-z]/ { print; next } p { exit }'
}

# step_env <file> <job> <step> — Echo the entries of the step's env block.
step_env() {
  CI_YAML="$1" job_step "$2" "$3" | awk '
    /^        env:$/ { in_env = 1; next }
    in_env && /^          / { print; next }
    in_env { exit }
  '
}

# filter_step <file> — Echo the paths-filter step of the changes job, from
# its uses: line to the next step.
filter_step() {
  CI_YAML="$1" job_block changes | awk '
    /^      - uses: dorny\/paths-filter@/ { in_step = 1; print; next }
    in_step && /^      [-#]/ { exit }
    in_step { print }
  '
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
  assert_eq "the schedule is 22:00 UTC alone" '    - cron: "0 22 * * *"' \
    "$(on_block "$wf" schedule)"
  assert_file_contains "the night can be dispatched" "$wf" '^  workflow_dispatch:$'
  assert_eq "a dispatch takes no inputs" "" "$(on_block "$wf" workflow_dispatch)"
  assert_contains "push still runs main and the tags" "$(on_block "$wf" push)" 'branches: [main]'
  assert_contains "pull_request still runs" "$(on_block "$wf" pull_request)" 'types: [opened, synchronize, reopened, labeled]'
}

check_concurrency() {
  local wf="$1" group
  group="$(grep -A2 '^concurrency:$' "$wf" | grep 'group:')"
  assert_contains "a night gets a group per event" "$group" \
    "|| format('{0}-{1}-{2}', github.ref, github.workflow, github.event_name) }}"
  assert_file_contains_fixed "only a pull request cancels the run in flight" "$wf" \
    "  cancel-in-progress: \${{ github.event_name == 'pull_request' }}"
}

check_changes_job() {
  local wf="$1"
  assert_contains "the paths filter runs on a pull request and a push alone" "$(filter_step "$wf")" \
    "        if: github.event_name == 'pull_request' || github.event_name == 'push'"
}

check_gates() {
  local wf="$1" job
  job="$(CI_YAML="$wf" job_block build-e2e-images)"
  assert_contains "build-e2e-images runs on a pull request and on the night" "$job" "      $NIGHT_EVENTS"
  assert_contains "it keeps the fork guard" "$job" \
    "github.event.pull_request.head.repo.fork != true &&"
  job="$(CI_YAML="$wf" job_block e2e-chaos)"
  assert_contains "e2e-chaos runs on a pull request and on the night" "$job" "      $NIGHT_EVENTS"
}

check_random_job() {
  local wf="$1" job step
  job="$(CI_YAML="$wf" job_block e2e-chaos-random)"
  assert_not_empty "job e2e-chaos-random exists" "$job"
  assert_contains "it runs after the image build and the Go gates" "$job" \
    "    needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]"
  assert_contains "on a pull request and on the night" "$job" "      $NIGHT_EVENTS"
  assert_contains "when the resolver turns it on" "$job" "      needs.changes.outputs.e2e-chaos-random == 'true' &&"
  assert_contains "after a built image map" "$job" "      needs.build-e2e-images.result == 'success' &&"
  assert_contains "on self-hosted" "$job" "    runs-on: self-hosted"
  assert_contains "with a 120-minute wall" "$job" "    timeout-minutes: 120"
  assert_eq "it reads the repository and the packages alone" "      contents: read
      packages: read" "$(permissions_of "$wf" e2e-chaos-random)"
  step="$(CI_YAML="$wf" job_step e2e-chaos-random "Bring up the random chaos plane")"
  assert_contains "up runs within 40 minutes" "$step" "        timeout-minutes: 40"
  assert_contains "with hack/chaos-random.sh up" "$step" "        run: hack/chaos-random.sh up"
  step="$(CI_YAML="$wf" job_step e2e-chaos-random "Run random chaos")"
  assert_contains "the run runs within 65 minutes" "$step" "        timeout-minutes: 65"
  assert_eq "on ci with the run id as the seed" '          CHAOS_RANDOM_ENV: ci
          CHAOS_RANDOM_SEED: ${{ github.run_id }}' "$(step_env "$wf" e2e-chaos-random "Run random chaos")"
  step="$(CI_YAML="$wf" job_step e2e-chaos-random "Setup E2E infrastructure")"
  assert_contains "the stack carries the dizzy stack" "$step" 'WITH_DIZZY: "true"'
  assert_contains "and metrics-server for memory-growth" "$step" 'WITH_METRICS_SERVER: "true"'
  step="$(CI_YAML="$wf" job_step e2e-chaos-random "Upload JUnit report")"
  assert_contains "the JUnit report goes where the report job looks" "$step" "name: e2e-chaos-junit-report-random"
  assert_contains "always" "$step" "if: always()"
  step="$(CI_YAML="$wf" job_step e2e-chaos-random "Delete kind cluster")"
  assert_contains "the kind cluster goes always" "$step" "if: always()"
  assert_contains "cleanup-e2e-tags waits for it" "$(CI_YAML="$wf" job_block cleanup-e2e-tags)" \
    "e2e-chaos, e2e-chaos-random, e2e-prometheus"
}

check_report_job() {
  local wf="$1" job step
  job="$(CI_YAML="$wf" job_block "$REPORT_JOB")"
  assert_not_empty "the job exists" "$job"
  assert_contains "it runs after the image build, the legs and the random run" "$job" \
    "    needs: [changes, build-e2e-images, e2e-chaos, e2e-chaos-random]"
  assert_eq "on every scheduled run, green or red, and on nothing else" \
    "    if: always() && github.event_name == 'schedule'" \
    "$(grep -E '^    if:' <<<"$job")"
  assert_contains "on ubuntu-latest" "$job" "    runs-on: ubuntu-latest"
  assert_contains "with a 10-minute wall" "$job" "    timeout-minutes: 10"
  assert_eq "it holds contents: read, issues: write and actions: read" \
    "      contents: read
      issues: write
      actions: read" "$(permissions_of "$wf" "$REPORT_JOB")"

  step="$(CI_YAML="$wf" job_step "$REPORT_JOB" "Download the JUnit reports")"
  assert_not_empty "with a SHA-pinned download-artifact" \
    "$(grep -E '^        uses: actions/download-artifact@[0-9a-f]{40} # v[0-9]' <<<"$step")"
  assert_contains "of every leg's report" "$step" "pattern: e2e-chaos-junit-report-*"
  assert_contains "into _output/nightly" "$step" "path: _output/nightly"
  assert_contains "a missing report does not stop the report" "$step" "continue-on-error: true"

  step="$(CI_YAML="$wf" job_step "$REPORT_JOB" "Report the night")"
  assert_contains "the report runs ci-report-nightly-chaos.sh" "$step" \
    "run: hack/ci-report-nightly-chaos.sh"
  assert_eq "with the seven inputs of the script" \
    '          GH_TOKEN: ${{ github.token }}
          GH_REPO: ${{ github.repository }}
          RUN_ID: ${{ github.run_id }}
          RUN_URL: ${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}
          REPORT_SHA: ${{ github.sha }}
          REPORTS_DIR: _output/nightly
          BUDGET_MINUTES: "720"' "$(step_env "$wf" "$REPORT_JOB" "Report the night")"
}

check_issue_writers() {
  local wf="$1" job block writers=""
  for job in $(CI_YAML="$wf" job_names); do
    # The block is read whole first: grep -q would close the pipe early, and
    # under pipefail the SIGPIPE of a long job_block reads as no match.
    block="$(CI_YAML="$wf" job_block "$job")"
    if grep -qE '^ +issues: write$' <<<"$block"; then
      writers="${writers:+$writers }$job"
      assert_eq "$job, which holds issues: write, is GitHub-hosted" "" \
        "$(grep -E '^    runs-on: self-hosted' <<<"$block")"
    fi
  done
  assert_eq "the report job alone holds issues: write" "$REPORT_JOB" "$writers"
  assert_eq "issues: write appears once in ci.yaml" "1" "$(grep -cE '^ +issues: write$' "$wf" | tr -d ' ')"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_night() {
  echo "Test: triggers and concurrency"
  check_triggers "$WORKFLOW"
  check_concurrency "$WORKFLOW"

  echo "Test: the changes job and the gates"
  check_changes_job "$WORKFLOW"
  check_gates "$WORKFLOW"

  echo "Test: job e2e-chaos-random"
  check_random_job "$WORKFLOW"

  echo "Test: job $REPORT_JOB"
  check_report_job "$WORKFLOW"

  echo "Test: no self-hosted job writes issues"
  check_issue_writers "$WORKFLOW"
}

test_a_copy_without_the_night_group_fails() {
  echo "Test: a copy that runs the night in the push group fails"

  local copy="$TMP_DIR/no-night-group.yaml"
  sed "s/format('{0}-{1}-{2}', github.ref, github.workflow, github.event_name)/format('{0}-{1}', github.ref, github.workflow)/" \
    "$WORKFLOW" >"$copy"
  assert_file_not_contains "the copy has no night group" "$copy" "format('{0}-{1}-{2}'"
  assert_eq "the concurrency check fails on it" "1" "$(fails_on check_concurrency "$copy")"
}

test_a_self_hosted_issue_writer_fails() {
  echo "Test: a copy whose e2e-chaos job grants issues: write fails"

  local copy="$TMP_DIR/chaos-writes-issues.yaml"
  awk '
    $0 == "  e2e-chaos:" { in_job = 1 }
    in_job && $0 == "      packages: read" { print; print "      issues: write"; in_job = 0; next }
    { print }
  ' "$WORKFLOW" >"$copy"
  assert_contains "the copy grants e2e-chaos issues: write" \
    "$(CI_YAML="$copy" job_block e2e-chaos)" "issues: write"
  assert_eq "the writer check fails on it" "3" "$(fails_on check_issue_writers "$copy")"

  echo "Test: a copy whose report job runs on self-hosted fails"
  copy="$TMP_DIR/report-self-hosted.yaml"
  awk -v key="  $REPORT_JOB:" '
    $0 == key { in_job = 1 }
    in_job && $0 == "    runs-on: ubuntu-latest" { print "    runs-on: self-hosted"; in_job = 0; next }
    { print }
  ' "$WORKFLOW" >"$copy"
  assert_eq "the writer check fails on it" "1" "$(fails_on check_issue_writers "$copy")"
  assert_eq "the report check fails on it" "1" "$(fails_on check_report_job "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_the_night
test_a_copy_without_the_night_group_fails
test_a_self_hosted_issue_writer_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
