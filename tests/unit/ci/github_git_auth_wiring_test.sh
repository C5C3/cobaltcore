#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify every CI step that reaches github.com with the runner's git hands the
# workflow token to the script it runs, and that the OVN image build carries
# the token into images/ovn/Dockerfile as a BuildKit secret.
#
# hack/ci-build-service-image.sh, hack/ci-deploy-korc.sh and
# hack/ci-build-ovn-image.sh only authenticate when GITHUB_TOKEN is set. A
# step that forgets the env clones anonymously, which the self-hosted
# runners' git (Ubuntu 24.04's 2.43) cannot rely on since 2026-09-02: GitHub
# answers the anonymous upload-pack request with a 401 challenge and the
# clone dies on "could not read Username for 'https://github.com'". The list
# of steps is derived from ci.yaml itself, so a new caller of one of the
# scripts is caught the moment it lands without the env.
#
# Usage: bash tests/unit/ci/github_git_auth_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
BUILD_IMAGES_YAML="$PROJECT_ROOT/.github/workflows/build-images.yaml"
BUILD_PUSH_ACTION="$PROJECT_ROOT/.github/actions/build-push-image/action.yaml"
DOCKERFILE="$PROJECT_ROOT/images/ovn/Dockerfile"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

TOKEN_ENV='GITHUB_TOKEN: ${{ github.token }}'
CLONING_SCRIPTS="hack/ci-build-service-image.sh hack/ci-deploy-korc.sh hack/ci-build-ovn-image.sh"

# steps_running <script>
#
# Echo "<job><TAB><step name>" for every ci.yaml step whose body invokes
# <script>. Comment lines and the paths-filter entries (list items) that name
# the script are not invocations and are skipped.
steps_running() {
  awk -v script="$1" '
    /^  [a-z0-9-]+:$/ { job = $1; sub(/:$/, "", job) }
    /^      - name: / { step = $0; sub(/^      - name: /, "", step) }
    index($0, script) && $0 !~ /^ *#/ && $0 !~ /^ *- / { print job "\t" step }
  ' "$CI_YAML" | sort -u
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_every_cloning_step_carries_the_token() {
  echo "Test: every ci.yaml step running a github.com-cloning script sets GITHUB_TOKEN"

  # A script without a caller in ci.yaml is fine (the OVN image build is
  # only wired here once its e2e leg lands); test_known_callers_are_found
  # keeps the derivation itself honest.
  local script found job step body
  for script in $CLONING_SCRIPTS; do
    found="$(steps_running "$script")"
    while IFS=$'\t' read -r job step; do
      [ -n "$job" ] || continue
      body="$(job_step "$job" "$step")"
      assert_not_empty "$job / '$step' is a step ci_yaml.sh can slice" "$body"
      assert_contains "$job / '$step' hands the workflow token to $script" "$body" "$TOKEN_ENV"
    done <<<"$found"
  done
}

test_known_callers_are_found() {
  echo "Test: the derivation sees the callers that exist today"

  # A regression in steps_running() that finds nothing would pass the loop
  # above vacuously, so pin the callers known at the time of writing.
  local korc_steps
  korc_steps="$(steps_running hack/ci-deploy-korc.sh)"
  assert_contains "e2e-operator installs K-ORC for the c5c3 leg" "$korc_steps" \
    "e2e-operator	Install CRDs watched by the c5c3-operator"
  assert_contains "e2e-controlplane deploys K-ORC" "$korc_steps" "e2e-controlplane	Deploy K-ORC"
  assert_contains "build-e2e-images builds the service images" \
    "$(steps_running hack/ci-build-service-image.sh)" "build-e2e-images	Build service images"
}

test_build_ovn_passes_the_token_as_a_secret() {
  echo "Test: build-images.yaml build-ovn mounts the workflow token as the github_token secret"

  local job
  job="$(awk '/^  build-ovn:$/ { p = 1; next } p && /^  [#a-z0-9-]/ { exit } p' "$BUILD_IMAGES_YAML")"
  assert_not_empty "the build-ovn job exists" "$job"
  assert_contains "build-ovn passes github_token from the workflow token" "$job" \
    'github_token=${{ secrets.GITHUB_TOKEN }}'
  assert_not_contains "the token is no build-arg" "$job" "GITHUB_TOKEN="
  assert_contains "the build-push-image action forwards its secrets input" \
    "$(cat "$BUILD_PUSH_ACTION")" 'secrets: ${{ inputs.secrets }}'
}

test_dockerfile_authenticates_through_the_secret() {
  echo "Test: images/ovn/Dockerfile authenticates its fetches through the mounted secret"

  # The mount and both fetches must share one RUN: a secret mount is scoped
  # to the instruction that declares it.
  local run_block
  run_block="$(awk '/^RUN --mount=type=secret,id=github_token/ { p = 1 } p { print } p && !/\\$/ { exit }' "$DOCKERFILE")"

  assert_not_empty "the source fetch mounts github_token" "$run_block"
  assert_contains "the token becomes a github.com-scoped auth header" "$run_block" \
    "http.https://github.com/.extraheader"
  assert_contains "git never waits for a prompt" "$run_block" "GIT_TERMINAL_PROMPT=0"
  assert_contains "the OVN fetch is inside the mounting RUN" "$run_block" \
    'fetch --depth 1 origin "${OVN_COMMIT}"'
  assert_contains "the OVS fetch is inside the mounting RUN" "$run_block" \
    'fetch --depth 1 origin "${OVS_COMMIT}"'
  assert_file_not_contains "the token is no build ARG" "$DOCKERFILE" '^ARG GITHUB_TOKEN'
  assert_contains "hack/ci-build-ovn-image.sh mounts the secret from the environment" \
    "$(cat "$PROJECT_ROOT/hack/ci-build-ovn-image.sh")" "id=github_token,env=GITHUB_TOKEN"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_every_cloning_step_carries_the_token
test_known_callers_are_found
test_build_ovn_passes_the_token_as_a_secret
test_dockerfile_authenticates_through_the_secret

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
