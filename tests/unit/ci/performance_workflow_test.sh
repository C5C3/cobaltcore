#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the shape of .github/workflows/performance.yaml, the nightly that
# measures token and reconcile latency against tests/perf/baseline.json:
#   - the triggers (a 01:30 UTC schedule and workflow_dispatch), read-only
#     permissions, a concurrency group that never cancels a night, and a
#     KIND_VERSION pin;
#   - one self-hosted job, baseline, with a 75-minute wall;
#   - the steps: the release resolved from the keystone service images, the
#     kind cluster, the three images pulled and loaded, the operator at
#     :latest with one replica, both measurements with their RESULT_JSON, the
#     comparison with a 20 % tolerance after both measurements and without an
#     if:, the artifact and the cluster deletion under always(), the
#     diagnostics under failure();
#   - a copy whose comparison step runs before a measurement step fails the
#     order check;
#   - once committed, tests/perf/baseline.json records its release, commit,
#     run and date.
#
# Usage: bash tests/unit/ci/performance_workflow_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

WORKFLOW="$PROJECT_ROOT/.github/workflows/performance.yaml"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$WORKFLOW"
BASELINE="$PROJECT_ROOT/tests/perf/baseline.json"

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMPARE_STEP="Compare against the baseline"
TOKEN_STEP="Measure token issue and validate"
RECONCILE_STEP="Measure reconcile latency"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# step_names <file> — echo the named steps of the baseline job, in order.
step_names() {
  CI_YAML="$1" job_block baseline | sed -n 's/^      - name: //p'
}

# step_index <file> <name> — echo the 1-based position of the named step, or
# nothing when the job has no such step.
step_index() {
  step_names "$1" | grep -nxF -- "$2" | head -1 | cut -d: -f1
}

# compare_runs_last <file> — succeed when the comparison step comes after
# both measurement steps.
compare_runs_last() {
  local compare token reconcile
  compare="$(step_index "$1" "$COMPARE_STEP")"
  token="$(step_index "$1" "$TOKEN_STEP")"
  reconcile="$(step_index "$1" "$RECONCILE_STEP")"
  [ -n "$compare" ] && [ -n "$token" ] && [ -n "$reconcile" ] &&
    [ "$compare" -gt "$token" ] && [ "$compare" -gt "$reconcile" ]
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_triggers_permissions_and_env() {
  echo "Test: triggers, permissions, concurrency and the kind pin"

  if [ ! -f "$WORKFLOW" ]; then
    echo "  FAIL: $WORKFLOW is missing"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_file_contains "the workflow is named Performance" "$WORKFLOW" '^name: Performance$'
  assert_file_contains_fixed "it runs nightly at 01:30 UTC" "$WORKFLOW" 'cron: "30 1 * * *"'
  assert_file_contains "it can be dispatched" "$WORKFLOW" '^  workflow_dispatch:'
  assert_eq "the permissions are contents: read and packages: read" \
    "  contents: read
  packages: read" \
    "$(awk '/^permissions:$/ { p = 1; next } p && /^[^ ]/ { exit } p' "$WORKFLOW")"
  assert_file_not_contains "nothing is granted write" "$WORKFLOW" ': write'
  assert_file_contains_fixed "one night per ref" "$WORKFLOW" 'group: performance-${{ github.ref }}'
  assert_file_contains_fixed "a dispatch never cancels a night" "$WORKFLOW" 'cancel-in-progress: false'
  assert_file_contains_fixed "IMAGE_PREFIX is ghcr.io/c5c3" "$WORKFLOW" 'IMAGE_PREFIX: ghcr.io/c5c3'
  assert_file_contains_fixed "KIND_CLUSTER is cobaltcore" "$WORKFLOW" 'KIND_CLUSTER: cobaltcore'

  # tests/unit/renovate/workflow_pins_custommanager_test.sh asserts the pin
  # agrees with ci.yaml's and hack/install-test-deps.sh's.
  assert_not_empty "performance.yaml pins KIND_VERSION" "$(workflow_kind_pin "$WORKFLOW")"
}

test_one_self_hosted_job() {
  echo "Test: one self-hosted job with a 75-minute wall"

  assert_eq "the only job is baseline" "baseline" "$(job_names | paste -sd' ' -)"
  local job
  job="$(job_block baseline)"
  assert_contains "it runs on self-hosted" "$job" "    runs-on: self-hosted"
  assert_contains "its wall is 75 minutes" "$job" "    timeout-minutes: 75"
}

test_the_steps() {
  echo "Test: the release, the cluster, the images and the operator"

  local step
  step="$(job_step baseline "Resolve the release")"
  assert_contains "the release is the first keystone service image release" "$step" \
    'OPERATOR=keystone hack/ci-service-image-releases.sh | head -n1'
  assert_contains "the release lands in GITHUB_ENV" "$step" '"RELEASE=${release}" >> "$GITHUB_ENV"'

  step="$(job_step baseline "Create kind cluster")"
  assert_contains "the cluster comes from create-kind-cluster" "$step" "uses: ./.github/actions/create-kind-cluster"
  assert_contains "with hack/kind-config.yaml" "$step" "config: hack/kind-config.yaml"
  assert_contains "at KIND_VERSION" "$step" 'version: ${{ env.KIND_VERSION }}'

  step="$(job_step baseline "Log in to GHCR")"
  assert_contains "the pulls log in first" "$step" "uses: ./.github/actions/registry-login"

  step="$(job_step baseline "Pull and load the images")"
  assert_contains "the operator image is :latest" "$step" '"${IMAGE_PREFIX}/keystone-operator:latest"'
  assert_contains "the keystone image is of the release" "$step" '"${IMAGE_PREFIX}/keystone:${RELEASE}"'
  assert_contains "the tempest image is of the release" "$step" '"${IMAGE_PREFIX}/tempest:${RELEASE}"'
  assert_contains "each image is pulled" "$step" 'docker pull "${image}"'
  assert_contains "each image is loaded into the cluster" "$step" 'kind load docker-image "${image}" --name "${KIND_CLUSTER}"'

  step="$(job_step baseline "Setup E2E infrastructure")"
  assert_contains "the stack comes from setup-e2e-infra" "$step" "uses: ./.github/actions/setup-e2e-infra"

  step="$(job_step baseline "Deploy keystone operator")"
  assert_contains "the operator is deployed by ci-deploy-operator.sh" "$step" "run: hack/ci-deploy-operator.sh"
  assert_contains "OPERATOR is keystone" "$step" "OPERATOR: keystone"
  assert_contains "IMAGE_REPO is the keystone-operator" "$step" 'IMAGE_REPO: ${{ env.IMAGE_PREFIX }}/keystone-operator'
  assert_contains "IMAGE_TAG is latest" "$step" "IMAGE_TAG: latest"
  assert_contains "NAMESPACE is keystone-system" "$step" "NAMESPACE: keystone-system"
  assert_contains "one replica, so the metrics port-forward reaches the leader" "$step" 'OPERATOR_REPLICAS: "1"'
}

test_the_measurements_and_the_comparison() {
  echo "Test: both measurements, the comparison and the closing steps"

  local step
  step="$(job_step baseline "$TOKEN_STEP")"
  assert_contains "the token step runs perf-token-baseline.sh" "$step" "run: hack/perf-token-baseline.sh"
  assert_contains "with IMAGE_TAG the release" "$step" 'IMAGE_TAG: ${{ env.RELEASE }}'
  assert_contains "with the tempest image of the release" "$step" \
    'CHAOS_WORKLOAD_IMAGE: ${{ env.IMAGE_PREFIX }}/tempest:${{ env.RELEASE }}'
  assert_contains "writing token.json" "$step" "RESULT_JSON: _output/perf/token.json"

  step="$(job_step baseline "$RECONCILE_STEP")"
  assert_contains "the reconcile step runs the benchmark" "$step" "run: hack/perf-reconcile-benchmark.sh"
  assert_contains "for 1 and 3 CRs" "$step" 'CR_COUNTS: "1 3"'
  assert_contains "with a 120 s settle" "$step" 'SETTLE_SECONDS: "120"'
  assert_contains "gated at a 2 s p95" "$step" 'GATE_P95_SECONDS: "2"'
  assert_contains "with IMAGE_TAG the release" "$step" 'IMAGE_TAG: ${{ env.RELEASE }}'
  assert_contains "writing reconcile.json" "$step" "RESULT_JSON: _output/perf/reconcile.json"

  step="$(job_step baseline "$COMPARE_STEP")"
  assert_contains "the comparison runs perf-baseline.py compare" "$step" "python3 hack/perf-baseline.py compare"
  assert_contains "against tests/perf/baseline.json" "$step" "--baseline tests/perf/baseline.json"
  assert_contains "with a 20 % tolerance" "$step" "--tolerance-percent 20"
  assert_contains "on both measured files" "$step" "--measured _output/perf/token.json _output/perf/reconcile.json"
  assert_not_contains "it has no if:, so a failed measurement leaves no comparison" "$step" "if:"
  if compare_runs_last "$WORKFLOW"; then
    echo "  PASS: the comparison comes after both measurements"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the comparison does not come after both measurements"
    FAIL=$((FAIL + 1))
  fi

  step="$(job_step baseline "Upload performance results")"
  assert_contains "the artifact is uploaded always" "$step" "if: always()"
  assert_contains "with a SHA-pinned upload-artifact" "$step" \
    "uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2"
  assert_contains "named performance-results" "$step" "name: performance-results"
  assert_contains "holding _output/perf/" "$step" "path: _output/perf/"
  assert_contains "kept 90 days" "$step" "retention-days: 90"

  step="$(job_step baseline "Dump diagnostic info")"
  assert_contains "the diagnostics run on failure" "$step" "if: failure()"
  assert_contains "for the keystone operator" "$step" "OPERATOR: keystone"

  step="$(job_step baseline "Delete kind cluster")"
  assert_contains "the cluster is deleted always" "$step" "if: always()"
  assert_contains "by ci-delete-kind-cluster.sh" "$step" "run: hack/ci-delete-kind-cluster.sh"
  assert_eq "the deletion is the last step" "Delete kind cluster" "$(step_names "$WORKFLOW" | tail -1)"
}

test_a_comparison_before_a_measurement_fails_the_check() {
  echo "Test: a copy with the comparison before a measurement fails the order check"

  local copy="$TMP_DIR/performance.yaml"
  sed -e "s/- name: ${COMPARE_STEP}\$/- name: SWAP/" \
    -e "s/- name: ${TOKEN_STEP}\$/- name: ${COMPARE_STEP}/" \
    -e "s/- name: SWAP\$/- name: ${TOKEN_STEP}/" "$WORKFLOW" >"$copy"
  assert_eq "the copy moves the comparison before the reconcile measurement" \
    "$TOKEN_STEP" "$(step_names "$copy" | grep -A1 -xF "$RECONCILE_STEP" | tail -1)"
  if compare_runs_last "$copy"; then
    echo "  FAIL: the order check passes on a copy that compares before measuring"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: the order check fails on the copy"
    PASS=$((PASS + 1))
  fi

  grep -vF -- "- name: ${COMPARE_STEP}" "$WORKFLOW" >"$TMP_DIR/no-compare.yaml"
  if compare_runs_last "$TMP_DIR/no-compare.yaml"; then
    echo "  FAIL: the order check passes on a copy without a comparison step"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: the order check fails on a copy without a comparison step"
    PASS=$((PASS + 1))
  fi
}

test_the_committed_baseline() {
  echo "Test: tests/perf/baseline.json records its origin"

  # The baseline is written from the artifact of the first Performance run;
  # until it is committed, the nightly's comparison exits 2.
  if [ ! -f "$BASELINE" ]; then
    echo "  SKIP: $BASELINE is not committed yet"
    SKIP=$((SKIP + 1))
    return
  fi
  assert_eq "it has recorded release, commit, run and date and a metrics object" "ok" \
    "$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
r = d.get("recorded", {})
ok = all(isinstance(r.get(k), str) and r[k] for k in ("release", "commit", "run", "date"))
print("ok" if ok and isinstance(d.get("metrics"), dict) and d["metrics"] else "incomplete")
' "$BASELINE" 2>&1)"
  assert_contains "the run is a workflow run of this repository" \
    "$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["recorded"]["run"])' "$BASELINE" 2>&1)" \
    "https://github.com/C5C3/cobaltcore/actions/runs/"
}

test_triggers_permissions_and_env
test_one_self_hosted_job
test_the_steps
test_the_measurements_and_the_comparison
test_a_comparison_before_a_measurement_fails_the_check
test_the_committed_baseline

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
