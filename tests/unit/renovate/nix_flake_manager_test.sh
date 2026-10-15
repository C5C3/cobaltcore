#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json keeps both Nix flake inputs fresh: nixpkgs (nixos-unstable)
# feeds the dev shell, nixpkgs-images (nixos-26.05) feeds the Nix-built container
# images. The `nix` manager is enabled with lockFileMaintenance (scoped to nix so
# npm's package-lock.json is untouched), and a packageRule groups the nix updates
# on the Monday schedule of the ubuntu digest bump. Without lockFileMaintenance
# the committed flake.lock would become the one pinned artifact Renovate stops
# bumping — a silent regression. flake.lock must name both inputs as github
# branches, the shape the manager re-locks.
#
# The weekly re-lock is opened for human review (automerge off): both refs are
# rolling channel branches, so the re-lock is not a version bump and
# minimumReleaseAge cannot gate it — a reviewer confirms the base-runtime bump the
# devshell and the images inherit instead.
#
# The spike under nix/spike/ is pinned by hand. The manager's default pattern
# matches its flake.nix, and a managerFilePatterns entry only adds to the
# defaults, so a packageRule disables every update there instead: the weekly
# re-lock of its flake.lock and the pep621 updates of its generated locks.
# The locks under nix/locks/ are derived by hack/nix-lock.sh and checked by
# the nix-lock-check job, so a second rule disables every update there too.
#
# Usage: bash tests/unit/renovate/nix_flake_manager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

test_nix_manager_enabled_with_lock_maintenance() {
  echo "Test: the nix manager is enabled with scoped lockFileMaintenance"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  assert_eq "nix.enabled is true" \
    "true" "$(jq -r '.nix.enabled' "$RENOVATE_FILE")"
  assert_eq "nix.lockFileMaintenance.enabled is true" \
    "true" "$(jq -r '.nix.lockFileMaintenance.enabled' "$RENOVATE_FILE")"
  assert_eq "nix.lockFileMaintenance.automerge is false (human-reviewed re-lock)" \
    "false" "$(jq -r '.nix.lockFileMaintenance.automerge' "$RENOVATE_FILE")"
}

test_nix_package_rule_present() {
  echo "Test: a packageRule groups the nix manager updates for a review-gated re-lock"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local rule
  rule="$(jq -c '.packageRules[]
    | select(((.matchManagers // []) | index("nix")) != null)' \
    "$RENOVATE_FILE" | head -1)"

  if [ -z "$rule" ]; then
    echo "  FAIL: no packageRule matches the nix manager"
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS: found a packageRule for matchManagers=[nix]"
  PASS=$((PASS + 1))

  assert_eq "nix packageRule leaves automerge off (human-reviewed re-lock)" \
    "false" "$(jq -r '.automerge' <<<"$rule")"
  assert_eq "nix packageRule groups updates" \
    "nix flake inputs" "$(jq -r '.groupName' <<<"$rule")"
  assert_eq "nix packageRule gates automerge on release age" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$rule")"
  assert_eq "nix packageRule runs on the Monday schedule" \
    '["before 6am on monday"]' "$(jq -c '.schedule' <<<"$rule")"
}

test_lock_maintenance_scoped_to_nix_only() {
  echo "Test: lockFileMaintenance is scoped to the nix manager, not global"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  # A top-level lockFileMaintenance would also churn npm's package-lock.json.
  assert_eq "no top-level lockFileMaintenance" \
    "null" "$(jq -r '.lockFileMaintenance // "null"' "$RENOVATE_FILE")"
}

test_spike_disabled_for_every_manager() {
  echo "Test: a packageRule disables every update under nix/spike/"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local rule
  rule="$(jq -c '.packageRules[] | select(.matchFileNames == ["nix/spike/**"])' \
    "$RENOVATE_FILE" | head -1)"

  if [ -z "$rule" ]; then
    echo "  FAIL: no packageRule matches nix/spike/**"
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS: found a packageRule for matchFileNames=[nix/spike/**]"
  PASS=$((PASS + 1))

  assert_eq "the nix/spike/** packageRule sets enabled to false" \
    "false" "$(jq -r '.enabled' <<<"$rule")"
  # A manager filter would leave the other managers, pep621 among them, on.
  assert_eq "the nix/spike/** packageRule names no manager" \
    "null" "$(jq -r '.matchManagers // "null"' <<<"$rule")"
}

test_nix_locks_disabled_for_every_manager() {
  echo "Test: a packageRule disables every update under nix/locks/"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local rule
  rule="$(jq -c '.packageRules[] | select(.matchFileNames == ["nix/locks/**"])' \
    "$RENOVATE_FILE" | head -1)"

  if [ -z "$rule" ]; then
    echo "  FAIL: no packageRule matches nix/locks/**"
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS: found a packageRule for matchFileNames=[nix/locks/**]"
  PASS=$((PASS + 1))

  assert_eq "the nix/locks/** packageRule sets enabled to false" \
    "false" "$(jq -r '.enabled' <<<"$rule")"
  # A manager filter would leave the pep621 and uv managers on.
  assert_eq "the nix/locks/** packageRule names no manager" \
    "null" "$(jq -r '.matchManagers // "null"' <<<"$rule")"
}

test_flake_lock_carries_both_inputs() {
  echo "Test: flake.lock carries both inputs as github channel branches"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local lock="$PROJECT_ROOT/flake.lock"
  assert_eq "the nixpkgs input follows nixos-unstable" \
    "nixos-unstable" "$(jq -r '.nodes.nixpkgs.original.ref' "$lock")"
  assert_eq "the nixpkgs input is a github input" \
    "github" "$(jq -r '.nodes.nixpkgs.original.type' "$lock")"
  assert_eq "the nixpkgs-images input follows nixos-26.05" \
    "nixos-26.05" "$(jq -r '.nodes["nixpkgs-images"].original.ref' "$lock")"
  assert_eq "the nixpkgs-images input is a github input" \
    "github" "$(jq -r '.nodes["nixpkgs-images"].original.type' "$lock")"
}

test_nix_manager_enabled_with_lock_maintenance
test_nix_package_rule_present
test_lock_maintenance_scoped_to_nix_only
test_spike_disabled_for_every_manager
test_nix_locks_disabled_for_every_manager
test_flake_lock_carries_both_inputs

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
