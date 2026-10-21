#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the release-floor layer (L8) of the check-release-wiring audit, the
# L3 exemption of the floor fixtures, the L4 checks of the kind fake compute
# and the ci-run-tempest.sh slug fallbacks, and the upgrade-path layer (L6):
# the release-upgrade and upgrade-flow transition read by tag, the
# ControlPlane-hop arm, which reads a tests/e2e/c5c3/ suite with an
# NN-patch-upgrade.yaml by openStackRelease, the skip-level target and the
# upgrade-abort wedge.
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
RELEASE_UPGRADE="tests/e2e/keystone/release-upgrade"
UPGRADE_FLOW="tests/e2e/keystone/upgrade-flow"
UPGRADE_ABORT="tests/e2e/keystone/upgrade-abort"

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
  for f in hack/ci-generate-tempest-matrix.sh hack/deploy-infra.sh hack/run-tempest.sh \
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

# write_image_fixture <dir> <path> <repository> <tag> — An upgrade-abort
# fixture at <dir>/<path> (the start CR or a patch) that holds only
# spec.image.repository and spec.image.tag; write_fixture writes no repository.
write_image_fixture() {
  local f="$1/$2"
  mkdir -p "$(dirname "$f")"
  printf 'spec:\n  image:\n    repository: %s\n    tag: "%s"\n' "$3" "$4" >"$f"
}

# run_audit <dir> — Runs the staged audit; echoes its combined output.
run_audit() {
  bash "$1/$AUDIT" 2>&1
}

# l8_section <output> — The L8 block of the audit output.
l8_section() {
  echo "$1" | sed -n '/^=== L8:/,/^=== Summary/p'
}

# l6_section <output> — The L6 block of the audit output.
l6_section() {
  echo "$1" | sed -n '/^=== L6:/,/^=== L7:/p'
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
# Test J: a ControlPlane hop on the newest transition
# ---------------------------------------------------------------------------
test_controlplane_hop_on_newest_transition_passes() {
  echo "Test: a tests/e2e/c5c3/ suite with a patch is a ControlPlane hop, and one on the newest transition passes L6"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fixture "$tmp" tests/e2e/c5c3/hop/00-controlplane-cr.yaml 2026.1 ""
  write_fixture "$tmp" tests/e2e/c5c3/hop/08-patch-upgrade.yaml 2026.2 ""
  write_fixture "$tmp" tests/e2e/c5c3/plain/00-controlplane-cr.yaml 2026.1 ""

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "the hop is read by openStackRelease" "$l6" \
    "[PASS] tests/e2e/c5c3/hop tests 2026.1 -> 2026.2 (newest transition)"
  assert_not_contains "a suite without a patch is no hop" "$l6" "tests/e2e/c5c3/plain"
  assert_not_contains "L6 reports no failure" "$l6" "[FAIL]"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test K: a ControlPlane hop on a stale pair, with a stray pin, or unreadable
# ---------------------------------------------------------------------------
test_controlplane_hop_stale_or_unreadable_fails() {
  echo "Test: a ControlPlane hop on a stale pair, with a pin outside it, or without a readable transition fails L6"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2 2027.1
  write_fixture "$tmp" tests/e2e/c5c3/hop/00-controlplane-cr.yaml 2026.1 ""
  write_fixture "$tmp" tests/e2e/c5c3/hop/08-patch-upgrade.yaml 2026.2 ""
  write_fixture "$tmp" tests/e2e/c5c3/hop/05-agent-cr.yaml 2025.2 ""
  write_fixture "$tmp" tests/e2e/c5c3/hop-b/08-patch-upgrade.yaml "" 2027.1

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "a hop on the previous transition fails" "$l6" \
    "[FAIL] tests/e2e/c5c3/hop tests 2026.1 -> 2026.2 but the newest transition is 2026.2 -> 2027.1"
  assert_contains "a pin outside the hop fails" "$l6" \
    "[FAIL] tests/e2e/c5c3/hop: release pins outside 2026.1 -> 2026.2: openStackRelease:2025.2"
  assert_contains "a hop without a readable transition fails" "$l6" \
    "[FAIL] tests/e2e/c5c3/hop-b: cannot read the ControlPlane transition (NN-controlplane-cr.yaml openStackRelease -> NN-patch-upgrade.yaml openStackRelease) — shape changed, update this audit"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test L: every upgrade-path suite on the newest transition
# ---------------------------------------------------------------------------
test_upgrade_suites_on_newest_transition_pass() {
  echo "Test: release-upgrade, upgrade-flow, skip-level and upgrade-abort suites on the newest transition pass L6"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fixture "$tmp" "$RELEASE_UPGRADE/00-keystone-cr.yaml" 2026.1 2026.1
  write_fixture "$tmp" "$RELEASE_UPGRADE/01-patch-upgrade.yaml" 2026.2 2026.2
  write_fixture "$tmp" "$UPGRADE_FLOW/00-keystone-cr.yaml" 2026.1 2026.1
  write_fixture "$tmp" "$UPGRADE_FLOW/01-patch-upgrade.yaml" 2026.2 2026.2
  write_fixture "$tmp" "$UPGRADE_FLOW/02-patch-skip-level.yaml" "" 2027.2
  write_image_fixture "$tmp" "$UPGRADE_ABORT/00-keystone-cr.yaml" ghcr.io/c5c3/keystone 2026.1
  write_image_fixture "$tmp" "$UPGRADE_ABORT/01-patch-stuck-upgrade.yaml" registry.invalid/c5c3/keystone 2026.2
  write_image_fixture "$tmp" "$UPGRADE_ABORT/02-patch-abort.yaml" ghcr.io/c5c3/keystone 2026.1

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "the newest transition is the last two releases" "$l6" \
    "[INFO] newest transition: 2026.1 -> 2026.2"
  assert_contains "a release-upgrade suite on the newest transition passes" "$l6" \
    "[PASS] $RELEASE_UPGRADE tests 2026.1 -> 2026.2 (newest transition)"
  assert_contains "an upgrade-flow suite on the newest transition passes" "$l6" \
    "[PASS] $UPGRADE_FLOW tests 2026.1 -> 2026.2 (newest transition)"
  assert_contains "a skip target neither wired nor sequential passes" "$l6" \
    "[PASS] $UPGRADE_FLOW/02-patch-skip-level.yaml targets 2027.2 (not wired, not sequential from 2026.2)"
  assert_contains "a wedge on a .invalid registry that aborts back passes" "$l6" \
    "[PASS] $UPGRADE_ABORT wedges 2026.1 -> 2026.2 on registry.invalid (independent of releases/) and aborts back to 2026.1"
  assert_not_contains "L6 reports no failure" "$l6" "[FAIL]"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test M: a release was added, one suite stayed on the previous transition
# ---------------------------------------------------------------------------
test_stale_transition_fails() {
  echo "Test: a suite on the previous transition, a pin outside its pair and a sequential skip target fail L6"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2 2027.1
  write_fixture "$tmp" "$RELEASE_UPGRADE/00-keystone-cr.yaml" 2026.1 2026.1
  write_fixture "$tmp" "$RELEASE_UPGRADE/01-patch-upgrade.yaml" 2026.2 2026.2
  write_fixture "$tmp" "$RELEASE_UPGRADE/01-keystone-user.yaml" "" 2025.2
  write_fixture "$tmp" "$UPGRADE_FLOW/00-keystone-cr.yaml" 2026.2 2026.2
  write_fixture "$tmp" "$UPGRADE_FLOW/01-patch-upgrade.yaml" 2027.1 2027.1
  write_fixture "$tmp" "$UPGRADE_FLOW/02-patch-skip-level.yaml" "" 2027.2

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "the newest transition moves with the new release" "$l6" \
    "[INFO] newest transition: 2026.2 -> 2027.1"
  assert_contains "a suite on the previous transition fails" "$l6" \
    "[FAIL] $RELEASE_UPGRADE tests 2026.1 -> 2026.2 but the newest transition is 2026.2 -> 2027.1"
  assert_contains "a helper CR pinned outside the pair fails" "$l6" \
    "[FAIL] $RELEASE_UPGRADE: release pins outside 2026.1 -> 2026.2: tag:2025.2"
  assert_contains "a moved suite beside it passes" "$l6" \
    "[PASS] $UPGRADE_FLOW tests 2026.2 -> 2027.1 (newest transition)"
  assert_contains "a skip target that is the sequential successor fails" "$l6" \
    "[FAIL] $UPGRADE_FLOW/02-patch-skip-level.yaml targets 2027.2, the sequential successor of 2027.1 — the webhook accepts it; move the target past 2027.2"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test N: a suite without a patch, a patch whose fields disagree
# ---------------------------------------------------------------------------
test_unreadable_suite_and_release_disagreement_fail() {
  echo "Test: a suite without a patch and a patch whose openStackRelease disagrees with its tag fail L6"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fixture "$tmp" "$RELEASE_UPGRADE/00-keystone-cr.yaml" 2026.1 2026.1
  write_fixture "$tmp" "$UPGRADE_FLOW/00-keystone-cr.yaml" 2026.1 2026.1
  write_fixture "$tmp" "$UPGRADE_FLOW/01-patch-upgrade.yaml" 2026.1 2026.2

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "a suite without NN-patch-upgrade.yaml fails" "$l6" \
    "[FAIL] $RELEASE_UPGRADE: cannot read the transition (NN-keystone-cr.yaml tag -> NN-patch-upgrade.yaml tag) — shape changed, update this audit"
  assert_contains "a patch whose openStackRelease stayed behind fails" "$l6" \
    "[FAIL] $UPGRADE_FLOW/01-patch-upgrade.yaml: tag \"2026.2\" but openStackRelease \"2026.1\""
  assert_contains "the transition is read by tag alone" "$l6" \
    "[PASS] $UPGRADE_FLOW tests 2026.1 -> 2026.2 (newest transition)"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test O: a wired skip target, an abort that keeps the .invalid registry
# ---------------------------------------------------------------------------
test_wired_skip_target_and_unrestored_abort_fail() {
  echo "Test: a wired skip target and an abort patch that keeps the .invalid registry fail L6"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fixture "$tmp" "$UPGRADE_FLOW/00-keystone-cr.yaml" 2026.1 2026.1
  write_fixture "$tmp" "$UPGRADE_FLOW/01-patch-upgrade.yaml" 2026.2 2026.2
  write_fixture "$tmp" "$UPGRADE_FLOW/02-patch-skip-level.yaml" "" 2026.2
  write_image_fixture "$tmp" "$UPGRADE_ABORT/00-keystone-cr.yaml" ghcr.io/c5c3/keystone 2026.1
  write_image_fixture "$tmp" "$UPGRADE_ABORT/01-patch-stuck-upgrade.yaml" registry.invalid/c5c3/keystone 2026.2
  write_image_fixture "$tmp" "$UPGRADE_ABORT/02-patch-abort.yaml" registry.invalid/c5c3/keystone 2026.1

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "a skip target under releases/ fails" "$l6" \
    "[FAIL] $UPGRADE_FLOW/02-patch-skip-level.yaml targets 2026.2, which now EXISTS under releases/ — the rejection test no longer tests a skip"
  assert_contains "an abort patch on the .invalid registry fails" "$l6" \
    "[FAIL] $UPGRADE_ABORT/02-patch-abort.yaml must restore ghcr.io/c5c3/keystone:2026.1, found registry.invalid/c5c3/keystone:2026.1 — the abort would pull from registry.invalid too"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test O2: an abort that keeps the stuck tag, a suite without an abort patch
# ---------------------------------------------------------------------------
test_abort_on_stuck_tag_or_missing_fails() {
  echo "Test: an abort patch that keeps the stuck tag and a suite without an abort patch fail L6"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_image_fixture "$tmp" "$UPGRADE_ABORT/00-keystone-cr.yaml" ghcr.io/c5c3/keystone 2026.1
  write_image_fixture "$tmp" "$UPGRADE_ABORT/01-patch-stuck-upgrade.yaml" registry.invalid/c5c3/keystone 2026.2
  write_image_fixture "$tmp" "$UPGRADE_ABORT/02-patch-abort.yaml" ghcr.io/c5c3/keystone 2026.2
  write_image_fixture "$tmp" tests/e2e/glance/upgrade-abort/00-glance-cr.yaml ghcr.io/c5c3/glance 2026.1
  write_image_fixture "$tmp" tests/e2e/glance/upgrade-abort/01-patch-stuck-upgrade.yaml registry.invalid/c5c3/glance 2026.2

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "an abort patch on the stuck tag fails" "$l6" \
    "[FAIL] $UPGRADE_ABORT/02-patch-abort.yaml must restore ghcr.io/c5c3/keystone:2026.1, found ghcr.io/c5c3/keystone:2026.2 — the abort would pull from registry.invalid too"
  assert_contains "a suite without an abort patch fails" "$l6" \
    "[FAIL] tests/e2e/glance/upgrade-abort/NN-patch-abort.yaml must restore ghcr.io/c5c3/glance:2026.1, found <unchanged>:<none> — the abort would pull from registry.invalid too"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test O3: a skip fixture without a tag, upgrade-abort suites that cannot wedge
# ---------------------------------------------------------------------------
test_unreadable_skip_and_unwedged_abort_fail() {
  echo "Test: a skip fixture without a tag and upgrade-abort suites that do not wedge a sequential upgrade fail L6"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1 2026.2
  write_fixture "$tmp" "$UPGRADE_FLOW/02-patch-skip-level.yaml" 2027.2 ""
  write_image_fixture "$tmp" tests/e2e/cinder/upgrade-abort/00-cinder-cr.yaml ghcr.io/c5c3/cinder 2026.1
  write_image_fixture "$tmp" tests/e2e/glance/upgrade-abort/00-glance-cr.yaml ghcr.io/c5c3/glance 2025.2
  write_image_fixture "$tmp" tests/e2e/glance/upgrade-abort/01-patch-stuck-upgrade.yaml registry.invalid/c5c3/glance 2026.1
  write_image_fixture "$tmp" tests/e2e/nova/upgrade-abort/00-nova-cr.yaml ghcr.io/c5c3/nova 2026.1
  write_image_fixture "$tmp" tests/e2e/nova/upgrade-abort/01-patch-stuck-upgrade.yaml registry.invalid/c5c3/nova 2027.1
  write_image_fixture "$tmp" tests/e2e/placement/upgrade-abort/00-placement-cr.yaml ghcr.io/c5c3/placement 2026.1
  write_image_fixture "$tmp" tests/e2e/placement/upgrade-abort/01-patch-stuck-upgrade.yaml ghcr.io/c5c3/placement 2026.2

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "a skip fixture without a tag fails" "$l6" \
    "[FAIL] $UPGRADE_FLOW/02-patch-skip-level.yaml: no tag — shape changed, update this audit"
  assert_contains "a suite without a stuck patch fails" "$l6" \
    "[FAIL] tests/e2e/cinder/upgrade-abort: cannot read the stuck transition (NN-cinder-cr.yaml -> NN-patch-stuck-upgrade.yaml) — shape changed, update this audit"
  assert_contains "a start release outside releases/ fails" "$l6" \
    "[FAIL] tests/e2e/glance/upgrade-abort starts at 2025.2, which has no releases/2025.2/ — the start image is never loaded"
  assert_contains "a stuck upgrade that is not sequential fails" "$l6" \
    "[FAIL] tests/e2e/nova/upgrade-abort upgrades 2026.1 -> 2027.1, not a sequential transition — the webhook rejects it instead of wedging"
  assert_contains "a stuck patch on the real repository of a wired release fails" "$l6" \
    "[FAIL] tests/e2e/placement/upgrade-abort wedges on 2026.2, which is now wired — its image is loaded and the upgrade completes; point the stuck patch's repository at a .invalid registry"
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Test P: a single release has no transition
# ---------------------------------------------------------------------------
test_single_release_reports_no_transition() {
  echo "Test: with one release L6 reports no transition and fails no suite"

  local tmp output l6
  tmp="$(mktemp -d)"
  stage_tree "$tmp" 2026.1 2026.1
  write_fixture "$tmp" "$RELEASE_UPGRADE/00-keystone-cr.yaml" 2026.1 2026.1
  write_fixture "$tmp" "$RELEASE_UPGRADE/01-patch-upgrade.yaml" 2026.2 2026.2
  write_image_fixture "$tmp" "$UPGRADE_ABORT/00-keystone-cr.yaml" ghcr.io/c5c3/keystone 2026.1
  write_image_fixture "$tmp" "$UPGRADE_ABORT/01-patch-stuck-upgrade.yaml" ghcr.io/c5c3/keystone 2026.2

  output="$(run_audit "$tmp")"
  l6="$(l6_section "$output")"

  assert_contains "no transition is derived" "$l6" \
    "[INFO] fewer than two releases — no upgrade transition to cover"
  assert_contains "the suite's transition is reported" "$l6" \
    "[INFO] $RELEASE_UPGRADE tests 2026.1 -> 2026.2"
  assert_contains "a stuck patch on the real repository wedges while its target is not wired" "$l6" \
    "[PASS] $UPGRADE_ABORT wedges 2026.1 -> 2026.2 (sequential, not wired; a .invalid repository in the stuck patch would make it independent of releases/)"
  # L3 reports the unwired 2026.2 pin, outside the L6 block.
  assert_not_contains "L6 reports no failure" "$l6" "[FAIL]"
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
test_controlplane_hop_on_newest_transition_passes
test_controlplane_hop_stale_or_unreadable_fails
test_upgrade_suites_on_newest_transition_pass
test_stale_transition_fails
test_unreadable_suite_and_release_disagreement_fail
test_wired_skip_target_and_unrestored_abort_fail
test_abort_on_stuck_tag_or_missing_fails
test_unreadable_skip_and_unwedged_abort_fail
test_single_release_reports_no_transition

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
