#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify images/ovn/Dockerfile fetches both halves of its source tree — OVN and
# Open vSwitch — by pinned commit rather than by mutable ref.
#
# images/ovn/Dockerfile is the only Dockerfile in the repo that fetches source
# over the network at build time, and the result is roughly a million lines of
# C that ends up in every chassis pod. Both refs it starts from are mutable:
# the OVN tag can be moved by anyone who can push to ovn-org/ovn, and a
# `git clone --recurse-submodules` would let the gitlink at that tag decide
# both which OVS commit is compiled and which host it comes from. Nothing
# downstream notices either substitution: verify_ovn.sh compares two binaries
# built from the same tree, and the Grype scan does not fail the build. So the
# pins have to be asserted statically — no runtime check on the built image
# can see them.
#
# Usage: bash tests/unit/ci/ovn_ovs_source_pin_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DOCKERFILE="$PROJECT_ROOT/images/ovn/Dockerfile"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

test_ovn_source_is_content_pinned() {
  echo "Test: images/ovn/Dockerfile pins the OVN source by commit"

  if [ ! -f "$DOCKERFILE" ]; then
    echo "  FAIL: $DOCKERFILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  # ARG OVN_VERSION names a git tag, which upstream can repoint at any commit.
  # ARG OVN_COMMIT is what makes the OVN half of the tree content-addressed,
  # exactly as the sha256 digest does for the ubuntu:noble base image.
  local pin
  pin="$(sed -nE 's/^ARG OVN_COMMIT=//p' "$DOCKERFILE")"
  assert_not_empty "Dockerfile carries an 'ARG OVN_COMMIT=' pin" "$pin"

  local well_formed="no"
  if [[ "$pin" =~ ^[0-9a-f]{40}$ ]]; then
    well_formed="yes"
  fi
  assert_eq "OVN_COMMIT is a full 40-hex SHA-1 (got '$pin')" "yes" "$well_formed"

  # OVN is fetched by that commit rather than by the tag, so neither a moved
  # tag (a different tree) nor a deleted one (an unbuildable image) reaches
  # the build. Verifying the tag would leave both failure modes open.
  assert_file_contains "OVN is fetched from ovn-org/ovn" \
    "$DOCKERFILE" 'remote add origin https://github.com/ovn-org/ovn.git'
  assert_file_contains "OVN is fetched by the pinned commit" \
    "$DOCKERFILE" 'fetch --depth 1 origin "${OVN_COMMIT}"'
  # No git command may reach the source through OVN_VERSION at all — not the
  # original 'clone --branch', not a fetch of the tag either.
  local tag_refs
  tag_refs="$(grep -n 'git .*\${OVN_VERSION}' "$DOCKERFILE" || true)"
  assert_eq "no git command resolves the source through the mutable OVN tag" \
    "" "$tag_refs"
}

test_ovs_source_is_content_pinned() {
  echo "Test: images/ovn/Dockerfile pins the Open vSwitch source by commit"

  if [ ! -f "$DOCKERFILE" ]; then
    echo "  FAIL: $DOCKERFILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  # A single 'ARG OVS_COMMIT=' line holding a full 40-hex SHA-1. A short or
  # abbreviated value would still fetch, but would not be the gitlink that
  # `git rev-parse HEAD:ovs` prints, so the comparison below could never match.
  local pin
  pin="$(sed -nE 's/^ARG OVS_COMMIT=//p' "$DOCKERFILE")"
  assert_not_empty "Dockerfile carries an 'ARG OVS_COMMIT=' pin" "$pin"

  local well_formed="no"
  if [[ "$pin" =~ ^[0-9a-f]{40}$ ]]; then
    well_formed="yes"
  fi
  assert_eq "OVS_COMMIT is a full 40-hex SHA-1 (got '$pin')" "yes" "$well_formed"

  # The gitlink at the pinned OVN commit is compared against OVS_COMMIT, so a
  # changed submodule aborts the build.
  assert_file_contains "the build reads the 'ovs' gitlink" \
    "$DOCKERFILE" 'rev-parse "HEAD:ovs"'
  assert_file_contains "the build compares the gitlink against OVS_COMMIT" \
    "$DOCKERFILE" '"${gitlink}" != "${OVS_COMMIT}"'

  # OVS comes from openvswitch/ovs by that commit, not from whatever URL
  # upstream's .gitmodules names at the tag.
  assert_file_contains "OVS is fetched from openvswitch/ovs" \
    "$DOCKERFILE" 'remote add origin https://github.com/openvswitch/ovs.git'
  assert_file_contains "OVS is fetched by the pinned commit" \
    "$DOCKERFILE" 'fetch --depth 1 origin "${OVS_COMMIT}"'
  assert_file_not_contains "the OVN checkout does not recurse into submodules" \
    "$DOCKERFILE" "recurse-submodules"

  # OVN builds against that checkout, not against the empty submodule dir.
  assert_file_contains "OVN builds against the pinned OVS checkout" \
    "$DOCKERFILE" "with-ovs-source=/src/ovs"
}

test_ovn_source_is_content_pinned
test_ovs_source_is_content_pinned

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
