#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-deploy-korc.sh enforces the SHAPE of the K-ORC source pin, not
# just its presence.
#
# The script's integrity model rests on the source tree being pinned by a full
# 40-char commit SHA: `git checkout --detach <sha>` pins an immutable tree, but
# `--detach main` and `--detach v2.6.0` succeed just as happily while following a
# MUTABLE ref. A `commit: main` slipped into the GitRepository manifest while
# debugging would otherwise have CI build and apply whatever upstream main is at
# that moment, under K-ORC's cluster RBAC.
#
# The commit gate runs before the clone, so every case here is offline: a rejected
# pin must fail without reaching the network, and the accepted pin is proven by
# reaching a LATER gate (the image-tag drift guard) rather than by cloning.
#
# Usage: bash tests/unit/hack/ci_deploy_korc_commit_pin_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_KORC_SH="$PROJECT_ROOT/hack/ci-deploy-korc.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# A syntactically valid 40-char SHA and the per-commit image tag upstream derives
# from it, so fixtures that mean to pass the commit gate also pass the drift guard.
VALID_COMMIT="0123456789abcdef0123456789abcdef01234567"
VALID_TAG="commit-0123456"
VALID_DIGEST="sha256:$(printf 'a%.0s' {1..64})"

# run_with_pin <commit> [tag]
# Writes throwaway GitRepository + Kustomization fixtures carrying <commit> and
# (optionally) <tag>, then runs ci-deploy-korc.sh against them. Echoes combined
# stdout/stderr; returns the script's exit status. PATH is emptied of git and
# kubectl so any case that gets past the gates fails loudly rather than reaching
# the network.
run_with_pin() {
  local commit="$1"
  local tag="${2:-$VALID_TAG}"
  local tmp
  tmp="$(mktemp -d)"

  cat >"$tmp/source.yaml" <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: k-orc
spec:
  ref:
    commit: ${commit}
EOF

  cat >"$tmp/release.yaml" <<EOF
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: k-orc
spec:
  images:
    - name: controller
      newTag: ${tag}
      digest: ${VALID_DIGEST}
EOF

  (
    KORC_SOURCE="$tmp/source.yaml"
    KORC_RELEASE="$tmp/release.yaml"
    export KORC_SOURCE KORC_RELEASE
    bash "$DEPLOY_KORC_SH"
  ) 2>&1
  local rc=$?
  rm -rf "$tmp"
  return $rc
}

# ---------------------------------------------------------------------------
# Test 1: a mutable ref in place of a commit SHA is rejected
# ---------------------------------------------------------------------------
test_rejects_mutable_refs() {
  echo "Test: ci-deploy-korc.sh rejects a branch/tag/short-SHA where a commit SHA belongs"

  local ref output exit_code
  for ref in "main" "v2.6.0" "0123456" "not-a-sha-at-all"; do
    output="$(run_with_pin "$ref")"
    exit_code=$?

    assert_nonzero_exit "commit '$ref' is rejected" "$exit_code"
    assert_contains "rejection of '$ref' explains the full-SHA requirement" \
      "$output" "MUST be pinned by a full commit SHA"
    assert_not_contains "commit '$ref' is rejected before the clone" \
      "$output" "Cloning K-ORC"
  done
}

# ---------------------------------------------------------------------------
# Test 2: an uppercase SHA is rejected (the per-commit tag derives lowercase)
# ---------------------------------------------------------------------------
test_rejects_uppercase_sha() {
  echo "Test: ci-deploy-korc.sh rejects an upper-case SHA"

  local output exit_code
  output="$(run_with_pin "0123456789ABCDEF0123456789ABCDEF01234567")"
  exit_code=$?

  assert_nonzero_exit "an upper-case SHA is rejected" "$exit_code"
  assert_contains "the rejection explains the full-SHA requirement" \
    "$output" "MUST be pinned by a full commit SHA"
}

# ---------------------------------------------------------------------------
# Test 3: an absent commit is still reported
# ---------------------------------------------------------------------------
test_rejects_absent_commit() {
  echo "Test: ci-deploy-korc.sh still reports an absent commit"

  local output exit_code
  output="$(run_with_pin "")"
  exit_code=$?

  assert_nonzero_exit "an absent commit is rejected" "$exit_code"
  assert_contains "the rejection names the parse failure" \
    "$output" "Could not parse a 'commit: <40 hex>' value"
}

# ---------------------------------------------------------------------------
# Test 4: a valid 40-char SHA passes the commit gate
# ---------------------------------------------------------------------------
test_accepts_full_sha() {
  echo "Test: ci-deploy-korc.sh accepts a full 40-char commit SHA"

  # Mismatch the image tag on purpose: reaching the DRIFT guard proves the commit
  # gate let the valid SHA through, and stops the run before the clone so the test
  # stays offline.
  local output exit_code
  output="$(run_with_pin "$VALID_COMMIT" "commit-deadbee")"
  exit_code=$?

  assert_nonzero_exit "the mismatched tag still fails the run" "$exit_code"
  assert_not_contains "a full SHA is not rejected as a bad commit pin" \
    "$output" "MUST be pinned by a full commit SHA"
  assert_contains "the run proceeds to the image-tag drift guard" \
    "$output" "does not match the pinned commit"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_rejects_mutable_refs
test_rejects_uppercase_sha
test_rejects_absent_commit
test_accepts_full_sha

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
