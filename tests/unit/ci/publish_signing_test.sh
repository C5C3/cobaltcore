#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the publish jobs of .github/workflows/ci.yaml sign what they push:
#   - merge-operator-images, helm-push and helm-push-target-cluster, and no
#     other job and not the workflow-level permissions, get id-token: write
#     for keyless signing;
#   - each of the three sets up the registry through setup-docker-registry,
#     which installs cosign by default and runs the docker login cosign reads,
#     and merge-operator-images has no buildx or login step of its own;
#   - the two chart jobs, which build no image, pass setup-buildx: 'false',
#     and the action skips its Buildx step for that input;
#   - the step that pushes has an id, and a sign step after it passes the
#     digest that step wrote to hack/ci-cosign-sign.sh;
#   - ci.yaml runs no cosign command itself, the three jobs still run on a
#     push only, and the target_cluster_chart filter lists the chart and the
#     two scripts that push and sign it.
#
# The three jobs run on a push only, so no pull request run shows a dropped
# permission or a sign step that names an empty output before the artifacts
# are public. hack/ci-cosign-sign.sh itself is covered by
# tests/unit/hack/ci_cosign_sign_test.sh.
#
# Usage: bash tests/unit/ci/publish_signing_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
SETUP_ACTION="$PROJECT_ROOT/.github/actions/setup-docker-registry/action.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

SIGNING_JOBS="merge-operator-images helm-push helm-push-target-cluster"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# step_line <job> <step-name>
# Echoes the line number of the step within the job body, or nothing.
step_line() {
  job_block "$1" | grep -nxF "      - name: $2" | head -1 | cut -d: -f1
}

# assert_step_before <job> <first step> <second step>
assert_step_before() {
  local first second
  first="$(step_line "$1" "$2")"
  second="$(step_line "$1" "$3")"
  if [ -n "$first" ] && [ -n "$second" ] && [ "$first" -lt "$second" ]; then
    echo "  PASS: $1: '$3' runs after '$2'"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1: '$3' runs after '$2' (lines: '${first}' and '${second}')"
    FAIL=$((FAIL + 1))
  fi
}

# uncommented <text>
# Echoes the lines of <text> that are not YAML comments.
uncommented() {
  grep -v '^[[:space:]]*#' <<<"$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_only_the_signing_jobs_get_an_oidc_token() {
  echo "Test: only artifact publishing and stable promotion have id-token: write"
  local signing_jobs="$SIGNING_JOBS github-release"

  local job
  for job in $signing_jobs; do
    assert_contains "$job has id-token: write" "$(job_block "$job")" "      id-token: write"
  done

  local jobs others=""
  jobs="$(job_names)"
  assert_contains "job_names lists build-and-push" "$jobs" "build-and-push"
  for job in $jobs; do
    case " $signing_jobs " in
      *" $job "*) continue ;;
    esac
    if grep -qE '^[[:space:]]*id-token:' <<<"$(job_block "$job")"; then
      others="$others $job"
    fi
  done
  assert_eq "no other job has an id-token line" "" "$others"

  # A job without a permissions block of its own inherits the workflow-level
  # one, so an id-token there would reach every such job.
  local top
  top="$(awk '/^permissions:/ { p = 1; next } p && /^[^ ]/ { exit } p' "$CI_YAML")"
  assert_contains "the workflow-level permissions block is found" "$top" "contents: read"
  assert_not_contains "the workflow-level permissions grant no id-token" "$top" "id-token"
}

test_the_jobs_set_up_the_registry_through_the_action() {
  echo "Test: the three jobs set up the docker login and cosign through setup-docker-registry, the chart jobs without Buildx"

  local job step
  for job in $SIGNING_JOBS; do
    step="$(job_step "$job" "Setup Docker registry")"
    assert_contains "$job uses setup-docker-registry" "$step" "uses: ./.github/actions/setup-docker-registry"
    assert_contains "$job logs in to the workflow's registry" "$step" 'registry: ${{ env.REGISTRY }}'
    assert_not_contains "$job leaves install-cosign at its default" "$step" "install-cosign"
  done

  local merge
  merge="$(uncommented "$(job_block merge-operator-images)")"
  assert_not_contains "merge-operator-images sets up no buildx of its own" "$merge" "docker/setup-buildx-action"
  assert_not_contains "merge-operator-images runs no registry-login of its own" "$merge" "./.github/actions/registry-login"

  local chart
  for chart in helm-push helm-push-target-cluster; do
    assert_step_before "$chart" "Setup Docker registry" "Package and push Helm chart"
    assert_contains "$chart builds no image and starts no Buildx builder" \
      "$(job_step "$chart" "Setup Docker registry")" "          setup-buildx: 'false'"
  done

  assert_contains "setup-docker-registry skips Buildx when setup-buildx is 'false'" \
    "$(grep -A1 -xF "    - name: Set up Docker Buildx" "$SETUP_ACTION")" \
    "      if: inputs.setup-buildx != 'false'"
}

test_the_manifest_list_is_signed_by_digest() {
  echo "Test: merge-operator-images signs the index digest its manifest step wrote"

  local step
  assert_contains "the manifest step has id manifest" \
    "$(job_step merge-operator-images "Create and push manifest list")" "        id: manifest"
  step="$(job_step merge-operator-images "Sign the manifest list")"
  assert_not_empty "merge-operator-images has a Sign the manifest list step" "$step"
  assert_contains "the sign step names the operator image" "$step" \
    'IMAGE: ${{ env.IMAGE_PREFIX }}/${{ matrix.operator }}-operator'
  assert_contains "the sign step reads the manifest step's digest" "$step" \
    'DIGEST: ${{ steps.manifest.outputs.digest }}'
  assert_contains "the sign step runs the script on the digest reference" "$step" \
    'run: hack/ci-cosign-sign.sh "${IMAGE}@${DIGEST}"'
  assert_step_before merge-operator-images "Setup Docker registry" "Create and push manifest list"
  assert_step_before merge-operator-images "Create and push manifest list" "Sign the manifest list"
}

test_the_charts_are_signed_by_digest() {
  echo "Test: helm-push and helm-push-target-cluster sign the chart digest their push step wrote"

  local job step
  for job in helm-push helm-push-target-cluster; do
    assert_contains "$job: the push step has id push" \
      "$(job_step "$job" "Package and push Helm chart")" "        id: push"
    step="$(job_step "$job" "Sign the chart")"
    assert_not_empty "$job has a Sign the chart step" "$step"
    assert_contains "$job: the sign step reads the push step's ref" "$step" \
      'CHART_REF: ${{ steps.push.outputs.ref }}'
    assert_contains "$job: the sign step runs the script on it" "$step" \
      'run: hack/ci-cosign-sign.sh "${CHART_REF}"'
    assert_step_before "$job" "Setup Docker registry" "Sign the chart"
    assert_step_before "$job" "Package and push Helm chart" "Sign the chart"
  done
}

test_signing_stays_in_the_script_and_on_push() {
  echo "Test: ci.yaml runs no cosign itself, and the signing jobs run on a push only"

  assert_eq "no uncommented line of ci.yaml runs cosign sign or cosign verify" "" \
    "$(grep -nE '^[^#]*cosign (sign|verify)' "$CI_YAML")"

  local job
  for job in $SIGNING_JOBS; do
    assert_contains "$job still runs on a push only" \
      "$(job_block "$job" | grep -m1 '^    if: ')" "github.event_name == 'push'"
  done

  assert_file_contains_fixed "hack/ci-cosign-sign.sh exists" \
    "$PROJECT_ROOT/hack/ci-cosign-sign.sh" "set -euo pipefail"
  if [ -x "$PROJECT_ROOT/hack/ci-cosign-sign.sh" ]; then
    echo "  PASS: hack/ci-cosign-sign.sh is executable, as the sign steps run it directly"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: hack/ci-cosign-sign.sh is not executable, and the sign steps run it directly"
    FAIL=$((FAIL + 1))
  fi
}

test_the_target_cluster_chart_filter_lists_the_publish_scripts() {
  echo "Test: the target_cluster_chart filter lists the chart and the scripts that push and sign it"

  assert_eq "the filter lists exactly the chart and the two scripts" \
    "deploy/bases/target-cluster-access/**
hack/ci-helm-push.sh
hack/ci-cosign-sign.sh" \
    "$(filter_block target_cluster_chart | sed -n "s/^ *- '\(.*\)'\$/\1/p")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_only_the_signing_jobs_get_an_oidc_token
test_the_jobs_set_up_the_registry_through_the_action
test_the_manifest_list_is_signed_by_digest
test_the_charts_are_signed_by_digest
test_signing_stays_in_the_script_and_on_push
test_the_target_cluster_chart_filter_lists_the_publish_scripts

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
