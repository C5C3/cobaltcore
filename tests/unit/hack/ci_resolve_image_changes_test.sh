#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-resolve-image-changes.sh turns the build-images paths filters
# into a service list and the four release-independent image flags:
#   - a push, a workflow_dispatch, and a plumbing change all build everything,
#     so the publish path keeps the behaviour it has today;
#   - a base or release-config change builds every service and the Tempest
#     image, but not the three images built FROM ubuntu:noble;
#   - a service filter builds that service alone, in ALL_SERVICES order;
#   - an image filter builds that image with an empty service list, which is
#     what has-services exists to gate;
#   - a filter that is set but empty (what the skipped filter step yields)
#     counts as false, and a missing required env var exits 1.
#
# Every mistake here is silent in CI: an output that resolves to false leaves a
# job skipped on every pull request while the pipeline stays green.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_generate_cleanup_matrix_test.sh.
#
# Usage: bash tests/unit/hack/ci_resolve_image_changes_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RESOLVER="$PROJECT_ROOT/hack/ci-resolve-image-changes.sh"

# The real list from the changes job of .github/workflows/build-images.yaml.
# tests/unit/ci/build_images_services_lockstep_test.sh pins it to the keys of
# releases/*/source-refs.yaml.
ALL="keystone horizon glance placement barbican neutron"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# run_resolver [ENV=value ...]
# Runs the resolver with GITHUB_OUTPUT unset so it only prints to stdout.
# Stores the combined stdout/stderr in OUTPUT and the exit status in RC.
run_resolver() {
  RC=0
  OUTPUT="$(env -u GITHUB_OUTPUT -u EVENT_NAME -u ALL_SERVICES "$@" bash "$RESOLVER" 2>&1)" || RC=$?
}

# assert_output <description> <key> <expected-value>
# Compares one whole output line, so `services=` never matches `has-services=`.
assert_output() {
  local description="$1" key="$2" expected="$3" actual
  actual="$(grep -E "^${key}=" <<<"$OUTPUT")"
  assert_eq "$description" "${key}=${expected}" "$actual"
}

# assert_all_flags <description> <expected-value>
# The four release-independent image flags at once.
assert_all_flags() {
  local description="$1" expected="$2" flag
  for flag in build-tempest build-ovn build-proxy build-shifter; do
    assert_output "$description ($flag)" "$flag" "$expected"
  done
}

# ---------------------------------------------------------------------------
# Test 1: everything that is not a pull request builds everything
# ---------------------------------------------------------------------------
test_non_pull_request_builds_everything() {
  echo "Test: push and workflow_dispatch resolve to every service and every image"

  local event
  for event in push workflow_dispatch; do
    run_resolver EVENT_NAME="$event" ALL_SERVICES="$ALL"

    assert_eq "resolver exits 0 on $event" "0" "$RC"
    assert_output "$event builds every service" services "all"
    assert_output "$event has services" has-services "true"
    assert_all_flags "$event builds every image" "true"
  done
}

# ---------------------------------------------------------------------------
# Test 2: a plumbing change builds everything too
# ---------------------------------------------------------------------------
test_plumbing_builds_everything() {
  echo "Test: a plumbing change builds every service and every image"

  run_resolver EVENT_NAME=pull_request ALL_SERVICES="$ALL" FILTER_plumbing=true

  assert_eq "resolver exits 0" "0" "$RC"
  assert_output "plumbing builds every service" services "all"
  assert_output "plumbing has services" has-services "true"
  assert_all_flags "plumbing builds every image" "true"
}

# ---------------------------------------------------------------------------
# Test 3: a base change builds every service and Tempest, nothing else
# ---------------------------------------------------------------------------
test_base_builds_services_and_tempest() {
  echo "Test: a base change builds every service and the Tempest image"

  run_resolver EVENT_NAME=pull_request ALL_SERVICES="$ALL" FILTER_base=true

  assert_eq "resolver exits 0" "0" "$RC"
  assert_output "base builds every service" services "all"
  assert_output "base has services" has-services "true"
  assert_output "base builds Tempest" build-tempest "true"
  assert_output "base does not build OVN" build-ovn "false"
  assert_output "base does not build the federation proxy" build-proxy "false"
  assert_output "base does not build the backup shifter" build-shifter "false"
}

# ---------------------------------------------------------------------------
# Test 4: the image flags union with the base class
# ---------------------------------------------------------------------------
test_base_and_own_filter_union() {
  echo "Test: an image filter adds to the base class rather than replacing it"

  run_resolver EVENT_NAME=pull_request ALL_SERVICES="$ALL" \
    FILTER_base=true FILTER_ovn=true

  assert_output "base still builds every service" services "all"
  assert_output "the OVN filter builds OVN" build-ovn "true"
  assert_output "base still builds Tempest" build-tempest "true"
}

# ---------------------------------------------------------------------------
# Test 5: a service filter builds that service alone
# ---------------------------------------------------------------------------
test_service_filter_selects_one_service() {
  echo "Test: a service filter builds that service and no image"

  run_resolver EVENT_NAME=pull_request ALL_SERVICES="$ALL" FILTER_svc_glance=true

  assert_eq "resolver exits 0" "0" "$RC"
  assert_output "only glance is built" services "glance"
  assert_output "glance has services" has-services "true"
  assert_all_flags "a service change builds no release-independent image" "false"
}

# ---------------------------------------------------------------------------
# Test 6: several service filters keep ALL_SERVICES order
# ---------------------------------------------------------------------------
test_service_list_keeps_all_services_order() {
  echo "Test: the service list is emitted in ALL_SERVICES order"

  run_resolver EVENT_NAME=pull_request ALL_SERVICES="$ALL" \
    FILTER_svc_glance=true FILTER_svc_keystone=true

  assert_output "keystone precedes glance, as ALL_SERVICES does" \
    services "keystone glance"
}

# ---------------------------------------------------------------------------
# Test 7: an image filter alone leaves the service list empty
# ---------------------------------------------------------------------------
test_image_filter_leaves_services_empty() {
  echo "Test: an OVN change builds OVN with an empty service list"

  run_resolver EVENT_NAME=pull_request ALL_SERVICES="$ALL" FILTER_ovn=true

  assert_eq "resolver exits 0" "0" "$RC"
  assert_output "no service is built" services ""
  assert_output "has-services gates the empty matrix" has-services "false"
  assert_output "OVN is built" build-ovn "true"
  assert_output "Tempest is not built" build-tempest "false"
}

# ---------------------------------------------------------------------------
# Test 8: no filter at all resolves to nothing, successfully
# ---------------------------------------------------------------------------
test_no_filter_resolves_to_nothing() {
  echo "Test: a pull request matching no filter builds nothing and exits 0"

  run_resolver EVENT_NAME=pull_request ALL_SERVICES="$ALL"

  assert_eq "resolver exits 0" "0" "$RC"
  assert_output "no service is built" services ""
  assert_output "has-services is false" has-services "false"
  assert_all_flags "no image is built" "false"
}

# ---------------------------------------------------------------------------
# Test 9: a filter that is set but empty counts as false
# ---------------------------------------------------------------------------
test_empty_filter_counts_as_false() {
  echo "Test: an empty filter value is false, as the skipped filter step yields"

  run_resolver EVENT_NAME=pull_request ALL_SERVICES="$ALL" FILTER_svc_glance=

  assert_eq "resolver exits 0" "0" "$RC"
  assert_output "an empty filter builds no service" services ""
  assert_output "has-services is false" has-services "false"
}

# ---------------------------------------------------------------------------
# Test 10: a missing required env var fails loudly
# ---------------------------------------------------------------------------
test_missing_env_vars_fail() {
  echo "Test: a missing required env var exits 1 with an ::error:: annotation"

  run_resolver EVENT_NAME=pull_request
  assert_eq "resolver exits 1 without ALL_SERVICES" "1" "$RC"
  assert_contains "the annotation names ALL_SERVICES" \
    "$OUTPUT" "::error::ALL_SERVICES must be set"

  run_resolver ALL_SERVICES="$ALL"
  assert_eq "resolver exits 1 without EVENT_NAME" "1" "$RC"
  assert_contains "the annotation names EVENT_NAME" \
    "$OUTPUT" "::error::EVENT_NAME must be set"
}

# ---------------------------------------------------------------------------
# Test 11: GITHUB_OUTPUT receives the same six lines stdout does
# ---------------------------------------------------------------------------
test_github_output_receives_every_line() {
  echo "Test: every output line is appended to GITHUB_OUTPUT and echoed"

  local out
  out="$(mktemp)"

  OUTPUT="$(env -u EVENT_NAME -u ALL_SERVICES \
    EVENT_NAME=pull_request ALL_SERVICES="$ALL" FILTER_svc_glance=true \
    GITHUB_OUTPUT="$out" bash "$RESOLVER" 2>&1)"
  RC=$?

  assert_eq "resolver exits 0" "0" "$RC"
  assert_eq "GITHUB_OUTPUT holds the same six lines stdout does" \
    "$OUTPUT" "$(cat "$out")"
  assert_contains "GITHUB_OUTPUT carries the service list" \
    "$(cat "$out")" "services=glance"

  rm -f "$out"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_non_pull_request_builds_everything
test_plumbing_builds_everything
test_base_builds_services_and_tempest
test_base_and_own_filter_union
test_service_filter_selects_one_service
test_service_list_keeps_all_services_order
test_image_filter_leaves_services_empty
test_no_filter_resolves_to_nothing
test_empty_filter_counts_as_false
test_missing_env_vars_fail
test_github_output_receives_every_line

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
