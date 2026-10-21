#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-changes.sh emits the keystone-pressure change signal
# and that .github/workflows/ci.yaml wires it up to the keystone-pressure job.
#
# The signal crosses three places and a mismatch in any of them is silent:
# GitHub Actions resolves an unknown `needs.<job>.outputs.<name>` to the empty
# string rather than failing, so a renamed output or an unwired FILTER_ env var
# leaves the job permanently skipped, and a Keystone patch merges without the
# token-path comparison its reviewer reads.
#
# The resolve script is executed for real; the ci.yaml sides are asserted
# against the workflow file. The last tests follow the signal to the job that
# reads it, to the release matrix the job runs over, and to the Makefile
# target the job calls, which is also how a developer reproduces it locally.
#
# Modelled on the sibling ci_resolve_changes_nova_libvirt_test.sh, with the
# shared scaffolding in tests/lib/ci_resolve.sh and tests/lib/ci_yaml.sh.
#
# Usage: bash tests/unit/hack/ci_resolve_changes_keystone_pressure_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC2034 # read by tests/lib/ci_resolve.sh and tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
MAKEFILE="$PROJECT_ROOT/Makefile"

ALL_OPERATORS_FIXTURE="keystone glance"

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
# Helpers
# ---------------------------------------------------------------------------

# Run the resolve script for the given ref and echo the keystone-pressure line
# it emits. Extra FILTER_ and label assignments are passed through.
run_resolve() {
  local ref="$1"
  shift
  resolve_output keystone-pressure "$ref" "$ALL_OPERATORS_FIXTURE" "$@"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_each_input_schedules_the_job() {
  echo "Test: the Keystone image, the harness and ci:pressure each schedule the job"

  assert_eq "a Keystone image change (a patch under patches/keystone/) schedules the job" \
    "keystone-pressure=true" "$(run_resolve refs/heads/main FILTER_image_keystone=true)"
  assert_eq "a harness change schedules the job" \
    "keystone-pressure=true" "$(run_resolve refs/heads/main FILTER_keystone_pressure=true)"
  assert_eq "ci:pressure schedules the job" \
    "keystone-pressure=true" "$(run_resolve refs/heads/main PR_LABELS='["ci:pressure"]')"
}

test_other_changes_do_not_schedule_the_job() {
  echo "Test: nothing else schedules the job"

  assert_eq "no input leaves the job off" \
    "keystone-pressure=false" "$(run_resolve refs/heads/main)"
  # The base images rebuild every service, but the job measures Keystone's
  # own code and spends a self-hosted runner per release on it.
  assert_eq "a base image change does not schedule the job" \
    "keystone-pressure=false" "$(run_resolve refs/heads/main FILTER_images_base=true)"
  assert_eq "a keystone-operator change does not schedule the job" \
    "keystone-pressure=false" "$(run_resolve refs/heads/main FILTER_keystone=true)"
  assert_eq "a shared Go change does not schedule the job" \
    "keystone-pressure=false" "$(run_resolve refs/heads/main FILTER_go_common=true)"
  assert_eq "another service image change does not schedule the job" \
    "keystone-pressure=false" "$(run_resolve refs/heads/main FILTER_image_glance=true)"
  assert_eq "a keystone suite edit does not schedule the job" \
    "keystone-pressure=false" "$(run_resolve refs/heads/main FILTER_tests_e2e_keystone=true)"
  assert_eq "a substrate change does not schedule the job" \
    "keystone-pressure=false" "$(run_resolve refs/heads/main FILTER_e2e_shared=true)"
  assert_eq "another ci: label does not schedule the job" \
    "keystone-pressure=false" "$(run_resolve refs/heads/main PR_LABELS='["ci:tempest"]')"
}

test_unset_filter_defaults_to_false() {
  echo "Test: an unwired filter defaults to false rather than tripping set -u"

  assert_eq "the output is emitted even with no FILTER_ env var" \
    "keystone-pressure=false" \
    "$(resolve_output keystone-pressure refs/heads/main "$ALL_OPERATORS_FIXTURE")"
}

test_noop_run_still_emits_the_output() {
  echo "Test: the labeled no-op path emits an explicit false"

  assert_eq "the no-op path emits the output" \
    "keystone-pressure=false" \
    "$(run_resolve refs/heads/main FILTER_image_keystone=true EVENT_ACTION=labeled EVENT_LABEL=bug)"
  assert_eq "a ci:pressure label event is no no-op" \
    "keystone-pressure=true" \
    "$(run_resolve refs/heads/main EVENT_ACTION=labeled EVENT_LABEL=ci:pressure PR_LABELS='["ci:pressure"]')"
}

test_forced_runs_schedule_the_job() {
  echo "Test: ci:full and a v* tag set the output whatever the filters say"

  assert_eq "ci:full schedules the job" \
    "keystone-pressure=true" "$(run_resolve refs/heads/main PR_LABELS='["ci:full"]')"
  assert_eq "a v* tag sets the output" \
    "keystone-pressure=true" "$(run_resolve refs/tags/v1.2.3)"
}

test_the_signal_reaches_the_image_build() {
  echo "Test: scheduling the job also schedules the images it loads"

  # The job gates on `needs.build-e2e-images.result == 'success'`, so a
  # harness edit or the label alone must switch the build on as well.
  assert_contains "a harness-only change still builds the E2E images" \
    "$(resolve_outputs refs/heads/main "$ALL_OPERATORS_FIXTURE" FILTER_keystone_pressure=true)" \
    "build-e2e-images=true"
  assert_contains "ci:pressure alone still builds the E2E images" \
    "$(resolve_outputs refs/heads/main "$ALL_OPERATORS_FIXTURE" PR_LABELS='["ci:pressure"]')" \
    "build-e2e-images=true"
  # A Keystone image change marks keystone as a changed service, which is
  # what has build-e2e-images build the pull request's image.
  assert_contains "a Keystone image change rebuilds the Keystone image" \
    "$(resolve_outputs refs/heads/main "$ALL_OPERATORS_FIXTURE" FILTER_image_keystone=true)" \
    'changed-services=["keystone"]'
}

test_ci_yaml_wires_the_signal() {
  echo "Test: ci.yaml declares the filter, passes it in and exports the output"

  assert_filter_is_wired keystone_pressure keystone-pressure
  assert_filter_is_wired image_keystone changed-services

  local block glob
  block=$(filter_block keystone_pressure)
  for glob in 'hack/keystone-pressure-ab.sh' 'hack/dizzy.sh' 'tests/load/keystone-pressure/**'; do
    assert_contains "the keystone_pressure filter lists $glob" "$block" "$glob"
  done
  # The Keystone image reaches the job through image_keystone, which lists
  # the patches the job exists to measure.
  assert_contains "the image_keystone filter lists the Keystone patches" \
    "$(filter_block image_keystone)" "patches/keystone/**"
}

test_the_release_matrix() {
  echo "Test: the changes job derives one leg per Keystone release"

  assert_file_contains "the changes job exports the release matrix" "$CI_YAML" \
    'keystone-pressure-releases: \${{ steps.keystone-pressure-matrix.outputs.keystone-pressure-releases }}'
  assert_file_contains_fixed "the matrix comes from the releases that ship a Keystone image" \
    "$CI_YAML" 'releases="$(OPERATOR=keystone hack/ci-service-image-releases.sh)"'

  # The step's jq, run on what the script prints for this tree, yields one
  # leg per release.
  local releases matrix count
  releases="$(OPERATOR=keystone bash "$PROJECT_ROOT/hack/ci-service-image-releases.sh")"
  matrix="$(jq -Rnc '{release: [inputs | select(length > 0)]}' <<<"${releases}")"
  count="$(printf '%s\n' "${releases}" | grep -c .)"
  assert_eq "the matrix has one leg per Keystone release" \
    "${count}" "$(jq '.release | length' <<<"${matrix}")"
  assert_gte "Keystone ships at least one release" "${count}" 1
}

test_the_job_reads_the_signal() {
  echo "Test: the job gates on the output and compares main's image with the pull request's"

  local job pull load run
  job=$(job_block keystone-pressure)
  pull=$(job_step keystone-pressure "Pull main's keystone image")
  load=$(job_step keystone-pressure "Load E2E images")
  run=$(job_step keystone-pressure "Run keystone pressure")

  assert_not_empty "the keystone-pressure job exists" "$job"
  assert_contains "the job reads the output the resolve script wrote" "$job" \
    "needs.changes.outputs.keystone-pressure == 'true'"
  assert_contains "the job runs on pull requests only" "$job" \
    "github.event_name == 'pull_request'"
  assert_contains "the job runs on the self-hosted runners" "$job" \
    "runs-on: self-hosted"
  assert_contains "the job does not block the pull request" "$job" \
    "continue-on-error: true"
  assert_contains "the job runs one leg per release" "$job" \
    'matrix: ${{ fromJson(needs.changes.outputs.keystone-pressure-releases) }}'
  assert_contains "the job creates the single-node cluster" "$job" \
    "config: hack/kind-config.yaml"

  # The base has to be pulled before the map's image takes the canonical
  # tag, or both sides would run the pull request's build.
  assert_contains "the base is main's published image" "$pull" \
    'ref="${IMAGE_PREFIX}/keystone:${RELEASE}"'
  assert_contains "the base keeps a tag of its own" "$pull" \
    'docker tag "${ref}" "${ref}-main"'
  local pull_line load_line
  pull_line=$(grep -n "      - name: Pull main's keystone image" <<<"$job" | cut -d: -f1)
  load_line=$(grep -n "      - name: Load E2E images" <<<"$job" | cut -d: -f1)
  assert_not_empty "the pull step exists" "$pull_line"
  assert_not_empty "the load step exists" "$load_line"
  if [[ -n "$pull_line" && -n "$load_line" ]]; then
    assert_eq "main's image is pulled before the map re-tags the canonical reference" \
      "true" "$([[ "$pull_line" -lt "$load_line" ]] && echo true || echo false)"
  fi
  assert_contains "the patched side comes from the map" "$load" \
    '${{ env.IMAGE_PREFIX }}/keystone:${{ matrix.release }}'
  assert_contains "the operator comes from the map" "$load" \
    '${{ env.IMAGE_PREFIX }}/keystone-operator:dev'

  assert_contains "the job runs the harness through the Makefile" "$run" \
    "make keystone-pressure-ab"
  assert_contains "the base is main's image" "$run" \
    'KEYSTONE_PRESSURE_BASE_IMAGE: ${{ env.IMAGE_PREFIX }}/keystone:${{ matrix.release }}-main'
  assert_contains "the patched side is the map's image" "$run" \
    'KEYSTONE_PRESSURE_PATCHED_IMAGE: ${{ env.IMAGE_PREFIX }}/keystone:${{ matrix.release }}'
  # Shorter, and the last stages of effective-fan get no window for their
  # grants and record nothing.
  assert_contains "effective-fan gets the six minutes its stages need" "$run" \
    'KEYSTONE_PRESSURE_PROFILE_DURATIONS: effective-fan=6m'
  assert_contains "the leg names its release" "$run" \
    'KEYSTONE_PRESSURE_RELEASE: ${{ matrix.release }}'
  assert_contains "the results land where the upload looks" "$run" \
    'KEYSTONE_PRESSURE_OUTPUT: _output/keystone-pressure/${{ matrix.release }}'
  assert_contains "the job uploads the results" "$job" \
    "name: keystone-pressure-\${{ matrix.release }}"
  assert_contains "the upload runs on failure too" \
    "$(job_step keystone-pressure "Upload keystone pressure results")" "if: always()"
  assert_contains "the job deletes its cluster" "$job" \
    "run: hack/ci-delete-kind-cluster.sh"

  # cleanup-e2e-tags prunes the run-scoped image tags this job pulls, so it
  # has to wait for the job.
  assert_contains "cleanup-e2e-tags waits for the job" \
    "$(job_block cleanup-e2e-tags)" "keystone-pressure"
}

test_makefile_target() {
  echo "Test: the Makefile target exists and names each missing precondition"

  assert_file_contains "the Makefile declares the target" "$MAKEFILE" \
    "^keystone-pressure-ab:$"
  assert_file_contains "the target is phony" "$MAKEFILE" \
    "^\\.PHONY: keystone-pressure-ab$"
  assert_file_contains_fixed "the target runs the harness" "$MAKEFILE" \
    "	hack/keystone-pressure-ab.sh"
  assert_file_contains_fixed "the second preflight names the missing operator" "$MAKEFILE" \
    "the keystone-operator is not installed; run \`OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator NAMESPACE=keystone-system hack/ci-deploy-operator.sh\` first"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_each_input_schedules_the_job
test_other_changes_do_not_schedule_the_job
test_unset_filter_defaults_to_false
test_noop_run_still_emits_the_output
test_forced_runs_schedule_the_job
test_the_signal_reaches_the_image_build
test_ci_yaml_wires_the_signal
test_the_release_matrix
test_the_job_reads_the_signal
test_makefile_target

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
