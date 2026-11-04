#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-generate-tempest-matrix.sh emits one matrix entry per
# (service, release) pair, narrowed by TEMPEST_SERVICES, and that narrowing the
# selection never narrows the missing-directory check.
#
# That last part is the point of the file. The generator hard-fails when a
# service has no tests/tempest/<service>-<slug>/ directory for a release, which
# is what stops a release from being onboarded halfway. Filtering the emitted
# entries by the services a pull request touched must not turn that into a check
# that only runs for the selected ones, or a deleted config directory would sit
# undetected until some later pull request happened to select that service.
#
# The keystone-upgrade entry is checked here as well: the generator appends it
# beside a selected keystone when the tree holds two or more releases, it names
# the newest release and the one before it, TEMPEST_SERVICES cannot select it by
# name, and it adds no config directory requirement of its own.
#
# The generator resolves its repository root from its own location, so each case
# runs against a throwaway tree with the script copied into it.
#
# Usage: bash tests/unit/hack/ci_generate_tempest_matrix_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
MATRIX_SH="$PROJECT_ROOT/hack/ci-generate-tempest-matrix.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_tree <name> <release>... — build a throwaway repo with the generator, the
# named releases, and a full set of Tempest config directories for each.
make_tree() {
  local tree="$TMP_DIR/$1"
  shift
  mkdir -p "$tree/hack"
  cp "$MATRIX_SH" "$tree/hack/"
  local release slug service
  for release in "$@"; do
    slug="${release//./-}"
    mkdir -p "$tree/releases/$release"
    for service in keystone glance barbican neutron cinder nova; do
      mkdir -p "$tree/tests/tempest/${service}-${slug}"
    done
  done
  echo "$tree"
}

# run_matrix <tree> [TEMPEST_SERVICES value] — run the generator, capturing the
# emitted matrix in MATRIX, the messages in OUTPUT and the status in RC.
run_matrix() {
  local tree="$1" out
  out="$(mktemp)"
  RC=0
  if [ "$#" -ge 2 ]; then
    OUTPUT="$(GITHUB_OUTPUT="$out" TEMPEST_SERVICES="$2" \
      bash "$tree/hack/ci-generate-tempest-matrix.sh" 2>&1)" || RC=$?
  else
    OUTPUT="$(GITHUB_OUTPUT="$out" env -u TEMPEST_SERVICES \
      bash "$tree/hack/ci-generate-tempest-matrix.sh" 2>&1)" || RC=$?
  fi
  MATRIX="$(sed -n 's/^tempest-releases=//p' "$out")"
  rm -f "$out"
}

# services_in_matrix — the service field of every emitted entry, sorted.
services_in_matrix() {
  jq -r '.include[].service' <<<"$MATRIX" | sort | tr '\n' ' ' | sed 's/ *$//'
}

# count_service <service> — how many emitted entries carry that service.
count_service() {
  jq --arg s "$1" '[.include[] | select(.service == $s)] | length' <<<"$MATRIX"
}

require_jq() {
  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed ($1 checks skipped)"
    SKIP=$((SKIP + $1))
    return 1
  fi
  return 0
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_unset_selection_emits_every_service() {
  echo "Test: an unset or empty TEMPEST_SERVICES emits every service"

  require_jq 4 || return
  local tree
  tree="$(make_tree all 2025.2 2026.1)"

  run_matrix "$tree"
  assert_eq "the generator succeeds" "0" "$RC"
  assert_eq "every service is emitted for every release" \
    "barbican barbican cinder cinder glance glance keystone keystone keystone-upgrade neutron neutron nova nova" "$(services_in_matrix)"

  # A skipped job still evaluates its matrix expression, so an empty include
  # would fail the run rather than skip it. Empty has to mean "all", not "none".
  run_matrix "$tree" ""
  assert_eq "an empty selection succeeds" "0" "$RC"
  assert_eq "an empty selection is not an empty matrix" \
    "barbican barbican cinder cinder glance glance keystone keystone keystone-upgrade neutron neutron nova nova" "$(services_in_matrix)"
}

test_selection_narrows_the_matrix() {
  echo "Test: TEMPEST_SERVICES narrows the emitted entries"

  require_jq 19 || return
  local tree key
  tree="$(make_tree narrow 2025.2 2026.1)"

  run_matrix "$tree" "glance"
  assert_eq "a single service succeeds" "0" "$RC"
  assert_eq "only that service is emitted, once per release" \
    "glance glance" "$(services_in_matrix)"
  assert_contains "the glance entry carries its own service CR name" \
    "$MATRIX" '"glance-cr-name":"glance-tempest-2025-2"'

  run_matrix "$tree" "keystone barbican"
  assert_eq "two services succeed" "0" "$RC"
  assert_eq "both are emitted, once per release, with the keystone upgrade leg" \
    "barbican barbican keystone keystone keystone-upgrade" "$(services_in_matrix)"

  # A nova leg brings up a whole compute stack, so its entry names five CRs
  # besides the Nova itself. A key the generator drops reaches the workflow as
  # the empty string, and the job then waits out its timeout on a resource with
  # no name.
  run_matrix "$tree" "nova"
  assert_eq "the nova selection succeeds" "0" "$RC"
  assert_eq "only the nova legs are emitted" "nova nova" "$(services_in_matrix)"
  for key in '"nova-cr-name":"nova-tempest-2025-2"' \
    '"ovn-cr-name":"ovn-nova-tempest-2025-2"' \
    '"neutron-cr-name":"neutron-nova-tempest-2025-2"' \
    '"placement-cr-name":"placement-nova-tempest-2025-2"' \
    '"glance-cr-name":"glance-nova-tempest-2025-2"' \
    '"cr-name":"keystone-nova-tempest-2025-2"' \
    '"tempest-concurrency":"2"'; do
    assert_contains "the nova entry carries ${key}" "$MATRIX" "$key"
  done

  # The cinder legs run a compute stack of their own, under their own slug.
  run_matrix "$tree" "cinder"
  assert_eq "the cinder selection succeeds" "0" "$RC"
  for key in '"nova-cr-name":"nova-cinder-tempest-2025-2"' \
    '"ovn-cr-name":"ovn-cinder-tempest-2025-2"' \
    '"neutron-cr-name":"neutron-cinder-tempest-2025-2"' \
    '"placement-cr-name":"placement-cinder-tempest-2025-2"'; do
    assert_contains "the cinder entry carries ${key}" "$MATRIX" "$key"
  done
}

# The keystone-upgrade leg deploys its Keystone at upgrade-from and upgrades it in
# place to release, so the pair has to be the newest transition under releases/.
# Its key order is the order GitHub lists the leg's values in the check name.
test_keystone_upgrade_leg() {
  echo "Test: the keystone-upgrade entry follows the newest release transition"

  local tree expected missing
  tree="$(make_tree upgrade 2026.1 2026.2)"

  # The entry is emitted, never selected: keystone-upgrade has no config
  # directory of its own, so it stays out of the selection vocabulary.
  run_matrix "$tree" "keystone-upgrade"
  assert_nonzero_exit "selecting keystone-upgrade by name fails the step" "$RC"
  assert_contains "the error names keystone-upgrade as unknown" \
    "$OUTPUT" "::error::Unknown tempest service: keystone-upgrade"
  assert_eq "no matrix is emitted for it" "" "$MATRIX"

  # The leg reads the newest keystone directory, which the per-release loop
  # checks for every selection; the entry adds no requirement of its own.
  missing="$(make_tree upgrade-missing 2026.1 2026.2)"
  rm -rf "$missing/tests/tempest/keystone-2026-2"
  expected="::error::Missing Tempest config directory: tests/tempest/keystone-2026-2 (for service keystone, release 2026.2)"
  run_matrix "$missing"
  assert_nonzero_exit "a missing newest keystone directory fails the run" "$RC"
  assert_contains "the error names the keystone directory of the newest release" \
    "$OUTPUT" "$expected"
  assert_eq "no matrix is emitted without it" "" "$MATRIX"
  run_matrix "$missing" "glance"
  assert_nonzero_exit "narrowing to glance still fails the run" "$RC"
  assert_contains "and reports the same missing keystone directory" \
    "$OUTPUT" "$expected"
  assert_eq "no matrix is emitted for the narrowed run either" "" "$MATRIX"

  require_jq 16 || return

  run_matrix "$tree"
  assert_eq "a two-release tree succeeds" "0" "$RC"
  assert_eq "it emits exactly one keystone-upgrade entry" "1" \
    "$(count_service keystone-upgrade)"
  assert_eq "the entry carries the newest keystone keys plus upgrade-from, in order" \
    '{"service":"keystone-upgrade","release":"2026.2","config-dir":"tests/tempest/keystone-2026-2","cr-name":"keystone-tempest-2026-2","service-k8s-name":"keystone-tempest-2026-2","upgrade-from":"2026.1"}' \
    "$(jq -c '.include[] | select(.service == "keystone-upgrade")' <<<"$MATRIX")"
  # The leg has to run like the fresh keystone leg of its release, so a key the
  # keystone entry gains, such as tempest-concurrency, has to reach it as well.
  assert_eq "the entry mirrors the newest keystone entry apart from service and upgrade-from" \
    "$(jq -c '.include[] | select(.service == "keystone" and .release == "2026.2") | del(.service)' <<<"$MATRIX")" \
    "$(jq -c '.include[] | select(.service == "keystone-upgrade") | del(.service, ."upgrade-from")' <<<"$MATRIX")"
  assert_eq "it comes after every per-release entry" "keystone-upgrade" \
    "$(jq -r '.include[-1].service' <<<"$MATRIX")"

  run_matrix "$(make_tree upgrade-three 2026.1 2026.2 2027.1)"
  assert_eq "a three-release tree succeeds" "0" "$RC"
  assert_eq "its one keystone-upgrade entry is the newest transition" \
    "2027.1 2026.2 tests/tempest/keystone-2027-1" \
    "$(jq -r '[.include[] | select(.service == "keystone-upgrade") | "\(.release) \(."upgrade-from") \(."config-dir")"] | join(",")' <<<"$MATRIX")"

  run_matrix "$(make_tree upgrade-single 2026.1)"
  assert_eq "a single-release tree succeeds" "0" "$RC"
  assert_eq "it still emits its keystone entry" "1" "$(count_service keystone)"
  assert_eq "it has no transition, so no keystone-upgrade entry" "0" \
    "$(count_service keystone-upgrade)"

  run_matrix "$tree" "glance"
  assert_eq "a glance-only selection succeeds" "0" "$RC"
  assert_eq "it emits no keystone-upgrade entry" "0" \
    "$(count_service keystone-upgrade)"
  run_matrix "$tree" "keystone"
  assert_eq "a keystone-only selection succeeds" "0" "$RC"
  assert_eq "it emits the keystone-upgrade entry once" "1" \
    "$(count_service keystone-upgrade)"
  run_matrix "$tree" ""
  assert_eq "an empty selection succeeds" "0" "$RC"
  assert_eq "it emits the keystone-upgrade entry once" "1" \
    "$(count_service keystone-upgrade)"
}

test_unknown_service_is_rejected() {
  echo "Test: an unknown service name fails before anything is emitted"

  local tree
  tree="$(make_tree unknown 2025.2)"

  run_matrix "$tree" "glance compute"
  assert_nonzero_exit "an unknown service fails the step" "$RC"
  assert_contains "the error names the offending value" \
    "$OUTPUT" "Unknown tempest service: compute"
  assert_eq "no matrix is emitted" "" "$MATRIX"
}

test_missing_config_directory_fails_whatever_is_selected() {
  echo "Test: a missing config directory fails even for an unselected service"

  local tree
  tree="$(make_tree missing 2025.2)"
  rm -rf "$tree/tests/tempest/barbican-2025-2"

  run_matrix "$tree" "keystone"
  assert_nonzero_exit "the run fails although barbican was not selected" "$RC"
  assert_contains "the error names the missing directory" \
    "$OUTPUT" "Missing Tempest config directory: tests/tempest/barbican-2025-2"
}

# A half-finished onboarding leaves a service with a config directory for one
# release and none for the other, which is the shape the hard failure exists to
# catch. The error has to name the service and the release beside the path: the
# path alone says nothing about which loop iteration asked for it.
test_missing_nova_directory_fails_for_one_release() {
  echo "Test: a nova config directory missing for one release fails the run"

  local expected tree
  expected="::error::Missing Tempest config directory: tests/tempest/nova-2026-1 (for service nova, release 2026.1)"
  tree="$(make_tree missing-nova 2025.2 2026.1)"
  rm -rf "$tree/tests/tempest/nova-2026-1"

  run_matrix "$tree"
  assert_nonzero_exit "the unnarrowed run fails" "$RC"
  assert_contains "the error names the directory, the service and the release" \
    "$OUTPUT" "$expected"

  run_matrix "$tree" "keystone"
  assert_nonzero_exit "narrowing to keystone still fails the run" "$RC"
  assert_contains "and reports the same missing nova directory" \
    "$OUTPUT" "$expected"
}

test_no_releases_yields_an_empty_matrix() {
  echo "Test: a tree without releases emits an empty include list"

  local tree="$TMP_DIR/empty"
  mkdir -p "$tree/hack"
  cp "$MATRIX_SH" "$tree/hack/"

  run_matrix "$tree"
  assert_eq "the generator still succeeds" "0" "$RC"
  assert_eq "the matrix is empty rather than absent" '{"include":[]}' "$MATRIX"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_unset_selection_emits_every_service
test_selection_narrows_the_matrix
test_keystone_upgrade_leg
test_unknown_service_is_rejected
test_missing_config_directory_fails_whatever_is_selected
test_missing_nova_directory_fails_for_one_release
test_no_releases_yields_an_empty_matrix

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
