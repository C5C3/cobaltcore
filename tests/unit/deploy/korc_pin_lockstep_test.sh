#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the committed main-line source and image keep immutable pin shapes.
# The installer separately verifies their agreement through build provenance;
# ci_deploy_korc_main_image_test.sh checks a source-only Renovate bump fails.
#
# Usage: bash tests/unit/deploy/korc_pin_lockstep_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_KORC_SH="$PROJECT_ROOT/hack/ci-deploy-korc.sh"
KORC_SOURCE_YAML="$PROJECT_ROOT/deploy/bases/operators/sources/k-orc.yaml"
KORC_RELEASE_YAML="$PROJECT_ROOT/deploy/bases/operators/releases/k-orc.yaml"

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
# Test 2: replacing the immutable commit with a moving branch fails
# ---------------------------------------------------------------------------
test_mutable_source_fails() {
  echo "Test: replacing ref.commit with a branch fails the gates"

  local tmp output exit_code current bumped
  tmp="$(mktemp -d)"
  current="$(awk '/^[[:space:]]*commit:[[:space:]]*/{print $2; exit}' "$KORC_SOURCE_YAML")"
  bumped="main"
  sed "s/${current}/${bumped}/" "$KORC_SOURCE_YAML" >"$tmp/source.yaml"
  cp "$KORC_RELEASE_YAML" "$tmp/release.yaml"

  output="$(verify_pin "$tmp/source.yaml" "$tmp/release.yaml")"
  exit_code=$?
  rm -rf "$tmp"

  assert_nonzero_exit "a mutable source is rejected" "$exit_code"
  assert_contains "the rejection requires an immutable source" \
    "$output" "MUST be pinned by a full commit SHA"
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
test_mutable_source_fails
test_missing_digest_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
