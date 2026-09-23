#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the K-ORC pin in the committed Flux manifests is internally consistent:
# the image tag in deploy/flux-system/releases/k-orc.yaml names the commit that
# deploy/flux-system/sources/k-orc.yaml pins, and both carry their canonical
# shapes (40-char commit, sha256 digest).
#
# hack/ci-deploy-korc.sh enforces exactly this before it clones, but it runs only
# in the jobs that deploy K-ORC (e2e-operator for c5c3 and the three ControlPlane
# jobs). A change to deploy/** alone selects the canary instead, so a Renovate
# PR that moves only ref.commit (the digest is not Renovate-tracked) could pass
# CI with the image left behind, and the k-orc rule automerges. make test-shell
# runs on every pull request; this test runs the script's offline gates
# (KORC_VERIFY_ONLY=true) against the real manifests there.
#
# Usage: bash tests/unit/deploy/korc_pin_lockstep_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_KORC_SH="$PROJECT_ROOT/hack/ci-deploy-korc.sh"
KORC_SOURCE_YAML="$PROJECT_ROOT/deploy/flux-system/sources/k-orc.yaml"
KORC_RELEASE_YAML="$PROJECT_ROOT/deploy/flux-system/releases/k-orc.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# verify_pin <source.yaml> <release.yaml> — runs the offline pin gates against
# the given manifests. Echoes combined output; returns the script's status.
verify_pin() {
  (
    KORC_SOURCE="$1"
    KORC_RELEASE="$2"
    KORC_VERIFY_ONLY=true
    export KORC_SOURCE KORC_RELEASE KORC_VERIFY_ONLY
    bash "$DEPLOY_KORC_SH"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test 1: the committed manifests pass the pin gates
# ---------------------------------------------------------------------------
test_committed_pin_is_consistent() {
  echo "Test: the committed K-ORC source commit and image tag/digest are in lockstep"

  local output exit_code
  output="$(verify_pin "$KORC_SOURCE_YAML" "$KORC_RELEASE_YAML")"
  exit_code=$?

  assert_eq "the committed pin passes the offline gates" "0" "$exit_code"
  assert_contains "the gates report the verified pin" "$output" "K-ORC pin verified"
  assert_not_contains "verify-only mode stops before the clone" "$output" "Cloning K-ORC"
}

# ---------------------------------------------------------------------------
# Test 2: a commit-only bump (the Renovate shape) fails
# ---------------------------------------------------------------------------
test_commit_only_bump_fails() {
  echo "Test: moving ref.commit without the image tag fails the gates"

  local tmp output exit_code current bumped
  tmp="$(mktemp -d)"
  current="$(awk '/^[[:space:]]*commit:[[:space:]]*/{print $2; exit}' "$KORC_SOURCE_YAML")"
  # Any other valid 40-char SHA: flip the first hex digit.
  case "${current:0:1}" in
    0) bumped="1${current:1}" ;;
    *) bumped="0${current:1}" ;;
  esac
  sed "s/${current}/${bumped}/" "$KORC_SOURCE_YAML" >"$tmp/source.yaml"
  cp "$KORC_RELEASE_YAML" "$tmp/release.yaml"

  output="$(verify_pin "$tmp/source.yaml" "$tmp/release.yaml")"
  exit_code=$?
  rm -rf "$tmp"

  assert_nonzero_exit "a commit-only bump is rejected" "$exit_code"
  assert_contains "the rejection names the tag/commit drift" \
    "$output" "does not match the pinned commit"
}

# ---------------------------------------------------------------------------
# Test 3: a dropped digest fails (the tag alone pins nothing)
# ---------------------------------------------------------------------------
test_missing_digest_fails() {
  echo "Test: a release manifest without the image digest fails the gates"

  local tmp output exit_code
  tmp="$(mktemp -d)"
  grep -v '^[[:space:]]*digest:' "$KORC_RELEASE_YAML" >"$tmp/release.yaml"

  output="$(verify_pin "$KORC_SOURCE_YAML" "$tmp/release.yaml")"
  exit_code=$?
  rm -rf "$tmp"

  assert_nonzero_exit "a missing digest is rejected" "$exit_code"
  assert_contains "the rejection names the digest requirement" \
    "$output" "MUST be pinned by digest"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_committed_pin_is_consistent
test_commit_only_bump_fails
test_missing_digest_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
