#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the release-floor layer (L8) of the check-release-wiring audit, the
# L3 exemption of the floor fixtures, and the L4 checks of the kind fake
# compute and the ci-run-tempest.sh slug fallbacks.
#
# A retirement removes releases/<old>/ one pull request before it moves
# release.MinimumSupported (docs/contributing/adding-a-new-release.md). A floor
# above or below the oldest wired release fails; only a floor one release below
# it under ALLOW_FLOOR_LAG=1, set by that step-2 pull request, is an [INFO].
# A *-openstackrelease-below-floor.yaml or *-image-tag-below-floor.yaml
# rejection fixture must pin the field it is named for below the floor, and
# every other pin it carries must name a wired release.
#
# The audit cds to the directory four levels above itself, so each case copies
# it into a temporary tree that carries only the files the audit reads. The
# layers a case does not target report findings on such a tree; the cases
# assert on the lines they target.
#
# Usage: bash tests/unit/ci/check_release_wiring_audit_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
AUDIT=".claude/skills/check-release-wiring/scripts/audit-release-wiring.sh"

# The step-2 gate of a retirement exports ALLOW_FLOOR_LAG=1 beside
# make test-shell; a case that needs the variable sets it as a prefix.
unset ALLOW_FLOOR_LAG

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

NOVA_FIXTURE="tests/e2e/nova/invalid-cr/42-openstackrelease-below-floor.yaml"
KEYSTONE_FIXTURE="tests/e2e/c5c3/invalid-cr/143-keystone-image-tag-below-floor.yaml"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# stage_tree <dir> <floor> <release>... — Writes a tree with a releases/<release>/
# directory per <release> and release.MinimumSupported at <floor>. The kind
# ControlPlane, the fake compute and the ci-run-tempest.sh fallbacks name the
# first <release>; every other file the audit reads is an empty stand-in.
stage_tree() {
  local t="$1" floor="$2" r f slug
  shift 2
  mkdir -p "$t/$(dirname "$AUDIT")" "$t/internal/common/release" \
    "$t/deploy/kind/controlplane" "$t/deploy/kind/fake-compute" "$t/hack" \
    "$t/tests/e2e" "$t/.github/workflows" "$t/operators/c5c3/api/v1alpha1"
  cp "$PROJECT_ROOT/$AUDIT" "$t/$AUDIT"
  for r in "$@"; do
    mkdir -p "$t/releases/$r"
  done
  echo "var MinimumSupported = Release{Year: ${floor%%.*}, Minor: ${floor#*.}, Raw: \"$floor\"}" \
    >"$t/internal/common/release/release.go"
  printf 'spec:\n  openStackRelease: "%s"\n' "$1" >"$t/deploy/kind/controlplane/controlplane.yaml"
  write_fake_compute "$t" "$1"
  slug="${1/./-}"
  write_tempest_fallbacks "$t" "$slug"
  for f in hack/ci-generate-tempest-matrix.sh hack/deploy-infra.sh \
    hack/ci-build-service-image.sh hack/ci-build-tempest-image.sh hack/run-tempest.sh \
    renovate.json .github/workflows/ci.yaml operators/c5c3/api/v1alpha1/controlplane_types.go; do
    : >"$t/$f"
  done
  cat >"$t/operators/c5c3/api/v1alpha1/controlplane_webhook.go" <<'EOF'
var controlPlaneReleaseRegexp = regexp.MustCompile(`^[0-9]{4}\.[12]$`)
EOF
}

# write_fake_compute <dir> <tag> — The two nova image refs of the kind fake
# compute, both at <tag>.
write_fake_compute() {
  cat >"$1/deploy/kind/fake-compute/fake-compute.yaml" <<EOF
          image: ghcr.io/c5c3/nova:$2
          image: ghcr.io/c5c3/nova:$2
EOF
}

# write_tempest_fallbacks <dir> <slug> — The CONFIG_DIR and SERVICE_K8S_NAME
# fallbacks of hack/ci-run-tempest.sh, both at <slug>.
write_tempest_fallbacks() {
  cat >"$1/hack/ci-run-tempest.sh" <<EOF
CONFIG_DIR="\${CONFIG_DIR:-tests/tempest/\${SERVICE}-$2}"
SERVICE_K8S_NAME="\${SERVICE_K8S_NAME:-\${SERVICE}-tempest-$2}"
EOF
}

# write_fixture <dir> <path> <openStackRelease> <tag> — A CR at <dir>/<path>
# pinning spec.openStackRelease and spec.image.tag; an empty value leaves its
# field out.
write_fixture() {
  local f="$1/$2"
  mkdir -p "$(dirname "$f")"
  {
    echo "apiVersion: nova.openstack.c5c3.io/v1alpha1"
    echo "kind: Nova"
    echo "spec:"
    if [ -n "$3" ]; then echo "  openStackRelease: \"$3\""; fi
    if [ -n "$4" ]; then printf '  image:\n    tag: "%s"\n' "$4"; fi
  } >"$f"
}

# run_audit <dir> — Runs the staged audit; echoes its combined output.
run_audit() {
  bash "$1/$AUDIT" 2>&1
}

# l8_section <output> — The L8 block of the audit output.
l8_section() {
  echo "$1" | sed -n '/^=== L8:/,/^=== Summary/p'
}

# ---------------------------------------------------------------------------
# Test A: a floor at the oldest wired release, fixtures well-formed
# ---------------------------------------------------------------------------
test_well_formed_floor_passes() {
  echo "Test: a floor at the oldest release and isolating fixtures pass L8, L3 and L4"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fixture "$tmp" "$NOVA_FIXTURE" 2025.2 2026.1
  write_fixture "$tmp" "$KEYSTONE_FIXTURE" 2026.1 2025.2

  output="$(run_audit "$tmp")"

  assert_contains "the floor is the oldest wired release" "$output" \
    "[PASS] release floor 2026.1 (internal/common/release/release.go MinimumSupported) is the oldest wired release"
  assert_contains "the openStackRelease fixture pins its field below the floor" "$output" \
    "[PASS] $NOVA_FIXTURE: openStackRelease 2025.2 is below the floor 2026.1"
  assert_contains "its image tag names a wired release" "$output" \
    "[PASS] $NOVA_FIXTURE: tag 2026.1 exists under releases/"
  assert_contains "the image-tag fixture pins its field below the floor" "$output" \
    "[PASS] $KEYSTONE_FIXTURE: tag 2025.2 is below the floor 2026.1"
  assert_contains "its openStackRelease names a wired release" "$output" \
    "[PASS] $KEYSTONE_FIXTURE: openStackRelease 2026.1 exists under releases/"
  assert_not_contains "L8 reports no failure" "$(l8_section "$output")" "[FAIL]"
  assert_not_contains "L3 leaves the floor fixtures to L8" "$output" "which has no releases/2025.2/"
  assert_contains "the fake compute tags exist" "$output" \
    "[PASS] deploy/kind/fake-compute/fake-compute.yaml: nova image tag \"2026.1\" exists under releases/"
  assert_not_contains "the fake compute tags equal the kind default" "$output" "is not the kind ControlPlane default"
  assert_contains "the CONFIG_DIR fallback parses and exists" "$output" \
    "[PASS] hack/ci-run-tempest.sh: CONFIG_DIR fallback \"2026.1\" exists under releases/"
  assert_contains "the SERVICE_K8S_NAME fallback parses and exists" "$output" \
    "[PASS] hack/ci-run-tempest.sh: SERVICE_K8S_NAME fallback \"2026.1\" exists under releases/"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test B: the release was removed, the floor never moved
# ---------------------------------------------------------------------------
test_lagging_floor_fails() {
  echo "Test: a floor below the oldest wired release fails"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.2 2027.1
  write_fixture "$tmp" "$NOVA_FIXTURE" 2025.2 2026.2

  output="$(run_audit "$tmp")"

  assert_contains "the lag is reported" "$output" \
    "[FAIL] release floor 2026.1 lags the oldest wired release 2026.2"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test B2: the step-2 pull request of a retirement allows the one-release lag
# ---------------------------------------------------------------------------
test_allowed_floor_lag_is_info() {
  echo "Test: ALLOW_FLOOR_LAG=1 turns a floor one release below the oldest into an [INFO]"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.2 2027.1
  write_fixture "$tmp" "$NOVA_FIXTURE" 2025.2 2026.2

  output="$(ALLOW_FLOOR_LAG=1 run_audit "$tmp")"

  assert_contains "the lag is reported" "$output" \
    "[INFO] release floor 2026.1 lags the oldest wired release 2026.2 (ALLOW_FLOOR_LAG=1)"
  assert_not_contains "L8 reports no failure" "$(l8_section "$output")" "[FAIL]"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test B3: no single retirement leaves the floor two releases behind
# ---------------------------------------------------------------------------
test_floor_two_releases_behind_fails() {
  echo "Test: a floor two releases below the oldest fails under ALLOW_FLOOR_LAG=1"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2025.2 2026.2 2027.1

  output="$(ALLOW_FLOOR_LAG=1 run_audit "$tmp")"

  assert_contains "the lag is reported" "$output" \
    "[FAIL] release floor 2025.2 lags the oldest wired release 2026.2 by more than one release"
  assert_not_contains "the variable is not offered as the way out" "$(l8_section "$output")" \
    "rerun with ALLOW_FLOOR_LAG=1"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test C: the floor moved before the release was removed
# ---------------------------------------------------------------------------
test_floor_above_oldest_release_fails() {
  echo "Test: a floor above the oldest wired release fails"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.2 2026.1 2026.2

  output="$(run_audit "$tmp")"

  assert_contains "the floor is reported above the oldest release" "$output" \
    "[FAIL] release floor 2026.2 is above the oldest wired release 2026.1"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test D: the fixture's own field reached the floor, another pin fell below it
# ---------------------------------------------------------------------------
test_own_field_decides() {
  echo "Test: an openStackRelease fixture at the floor fails although its tag is below it"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fixture "$tmp" "$NOVA_FIXTURE" 2026.1 2025.2

  output="$(run_audit "$tmp")"

  assert_contains "the openStackRelease pin is not below the floor" "$output" \
    "[FAIL] $NOVA_FIXTURE: openStackRelease 2026.1 is not below the floor 2026.1"
  assert_contains "the below-floor tag is reported as an unwired release" "$output" \
    "[FAIL] $NOVA_FIXTURE: tag 2025.2 has no releases/2025.2/ directory"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test E: step 3 moved the floor, the fixture's image tag stayed behind
# ---------------------------------------------------------------------------
test_stale_companion_pin_fails() {
  echo "Test: a companion pin on a retired release fails"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.2 2026.2 2027.1
  write_fixture "$tmp" "$NOVA_FIXTURE" 2026.1 2026.1

  output="$(run_audit "$tmp")"

  assert_contains "the openStackRelease pin is below the floor" "$output" \
    "[PASS] $NOVA_FIXTURE: openStackRelease 2026.1 is below the floor 2026.2"
  assert_contains "the stale image tag is reported" "$output" \
    "[FAIL] $NOVA_FIXTURE: tag 2026.1 has no releases/2026.1/ directory"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test F: a floor fixture name outside an invalid-* corpus
# ---------------------------------------------------------------------------
test_floor_name_outside_invalid_corpus_is_swept() {
  echo "Test: L3 sweeps a *-below-floor.yaml file outside tests/e2e/*/invalid-*/"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fixture "$tmp" tests/e2e-chaos/nova/01-openstackrelease-below-floor.yaml 2025.2 ""

  output="$(run_audit "$tmp")"

  assert_contains "the unwired pin is reported by L3" "$output" \
    "which has no releases/2025.2/ (e.g. tests/e2e-chaos/nova/01-openstackrelease-below-floor.yaml"
  assert_contains "L8 does not take the file" "$output" "[INFO] no *-below-floor.yaml fixtures found"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test G: the fake compute runs another release than the kind default
# ---------------------------------------------------------------------------
test_fake_compute_off_default_fails() {
  echo "Test: fake compute nova tags other than the kind default fail"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fake_compute "$tmp" 2026.2

  output="$(run_audit "$tmp")"

  assert_contains "the tag mismatch is reported" "$output" \
    "[FAIL] deploy/kind/fake-compute/fake-compute.yaml: nova image tag \"2026.2\" is not the kind ControlPlane default \"2026.1\""
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test H: the Tempest slug fallbacks name a removed release
# ---------------------------------------------------------------------------
test_tempest_fallbacks_on_removed_release_fail() {
  echo "Test: ci-run-tempest.sh slug fallbacks on a removed release fail"

  local tmp output
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_tempest_fallbacks "$tmp" 2025-2

  output="$(run_audit "$tmp")"

  assert_contains "the CONFIG_DIR fallback is reported" "$output" \
    "[FAIL] hack/ci-run-tempest.sh: CONFIG_DIR fallback \"2025.2\" has no releases/2025.2/ directory"
  assert_contains "the SERVICE_K8S_NAME fallback is reported" "$output" \
    "[FAIL] hack/ci-run-tempest.sh: SERVICE_K8S_NAME fallback \"2025.2\" has no releases/2025.2/ directory"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test I: the audit parses the repository's own references
# ---------------------------------------------------------------------------
test_repository_references_parse() {
  echo "Test: the audit parses the repository's floor, fake compute and Tempest fallbacks"

  local tmp output f d releases=""
  tmp="$(mktemp -d)"
  for d in "$PROJECT_ROOT"/releases/*/; do
    d="${d%/}"
    releases="$releases ${d##*/}"
  done
  # The copies below replace the staged floor and default references.
  # shellcheck disable=SC2086 # one argument per release directory
  stage_tree "$tmp" 2026.1 $releases
  for f in internal/common/release/release.go deploy/kind/controlplane/controlplane.yaml \
    deploy/kind/fake-compute/fake-compute.yaml hack/ci-run-tempest.sh; do
    cp "$PROJECT_ROOT/$f" "$tmp/$f"
  done

  output="$(run_audit "$tmp")"

  assert_not_contains "MinimumSupported parses" "$output" "could not extract MinimumSupported"
  assert_not_contains "the fake compute nova tags parse" "$output" \
    "fake-compute.yaml: could not extract"
  assert_not_contains "the ci-run-tempest.sh fallbacks parse" "$output" \
    "ci-run-tempest.sh: could not extract"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_well_formed_floor_passes
test_lagging_floor_fails
test_allowed_floor_lag_is_info
test_floor_two_releases_behind_fails
test_floor_above_oldest_release_fails
test_own_field_decides
test_stale_companion_pin_fails
test_floor_name_outside_invalid_corpus_is_swept
test_fake_compute_off_default_fails
test_tempest_fallbacks_on_removed_release_fail
test_repository_references_parse

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
