#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify images/ovn/ovn.nix fetches both halves of its source tree, OVN and
# Open vSwitch, by the commit and hash images/ovn/source.json pins rather than
# by a mutable ref.
#
# The ovn image compiles roughly a million lines of C from two upstream
# trees, and the result ends up in every chassis pod. Both refs it starts
# from are mutable: the OVN tag can be moved by anyone who can push to
# ovn-org/ovn, and a fetch with submodules would let the gitlink at that tag
# decide both which OVS commit is compiled and which host it comes from.
# Nothing downstream notices either substitution: verify_ovn.sh compares two
# binaries built from the same tree, and the Grype scan does not fail the
# build. So the pins have to be asserted statically, no runtime check on the
# built image can see them: each tree is a fixed-output fetch by a full
# commit and an SRI hash, and the OVS commit is the one hack/nix-lock.sh
# reads from the gitlink at the pinned OVN commit.
#
# Usage: bin/bats tests/unit/ci/ovn_ovs_source_pin.bats

load '../../lib/bats/common'

PIN="$PROJECT_ROOT/images/ovn/source.json"
DERIVATION="$PROJECT_ROOT/images/ovn/ovn.nix"
GENERATOR="$PROJECT_ROOT/hack/nix-lock.sh"

# well_formed <value> <regex> prints yes when <value> matches <regex>.
well_formed() {
  if [[ "$1" =~ $2 ]]; then echo yes; else echo no; fi
}

@test "images/ovn/source.json pins both trees by a full commit and an SRI hash" {
  require_tool jq
  if [ ! -f "$PIN" ]; then
    fail "$PIN missing"
  fi

  # The tag names a commit upstream can repoint; rev is what makes the OVN
  # half of the tree content-addressed. A short or abbreviated commit would
  # still fetch, but would not be the gitlink `git ls-tree` prints.
  local key value
  for key in .rev .ovs.rev; do
    value="$(jq -r "$key // empty" "$PIN")"
    soft assert_eq "$key is a full 40-hex SHA-1 (got '$value')" "yes" "$(well_formed "$value" '^[0-9a-f]{40}$')"
  done
  for key in .hash .ovs.hash; do
    value="$(jq -r "$key // empty" "$PIN")"
    soft assert_eq "$key is an SRI sha256 hash (got '$value')" "yes" \
      "$(well_formed "$value" '^sha256-[A-Za-z0-9+/]{43}=$')"
  done
}

@test "images/ovn/ovn.nix fetches both trees by the pin, without submodules" {
  if [ ! -f "$DERIVATION" ]; then
    fail "$DERIVATION missing"
  fi

  assert_file_matches "OVN is fetched from ovn-org/ovn" "$DERIVATION" 'owner = "ovn-org";'
  assert_file_matches "the OVN repository is ovn" "$DERIVATION" 'repo = "ovn";'
  assert_file_matches "OVN is fetched by the pinned commit and hash" "$DERIVATION" \
    'inherit (pin) rev hash;'
  # OVS comes from openvswitch/ovs by that commit, not from whatever URL
  # upstream's .gitmodules names at the tag.
  assert_file_matches "OVS is fetched from openvswitch/ovs" "$DERIVATION" 'owner = "openvswitch";'
  assert_file_matches "the OVS repository is ovs" "$DERIVATION" 'repo = "ovs";'
  assert_file_matches "OVS is fetched by the pinned commit and hash" "$DERIVATION" \
    'inherit (pin.ovs) rev hash;'
  assert_eq "both trees are fetchFromGitHub fetches" "2" \
    "$(grep -c 'fetchFromGitHub {' "$DERIVATION")"
  assert_file_not_matches "no fetch follows the submodule" "$DERIVATION" "fetchSubmodules"
  # OVN builds against that tree, not against the empty submodule dir.
  assert_file_contains_fixed "OVN builds against the pinned OVS tree" "$DERIVATION" \
    'with-ovs-source="$PWD/ovs"'
}

@test "hack/nix-lock.sh derives ovs.rev from the gitlink at the pinned OVN commit" {
  assert_file_matches "the generator reads the 'ovs' gitlink of the fetched commit" "$GENERATOR" \
    'ls-tree FETCH_HEAD ovs'
  assert_file_matches "the generator fetches the pinned commit, not the tag" "$GENERATOR" \
    'fetch -q --depth 1 --filter=blob:none'
}
