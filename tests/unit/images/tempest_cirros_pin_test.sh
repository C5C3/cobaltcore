#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the cirros pin of the tempest image stays in lockstep with the one of
# the kind fixture:
#   1. The URL and the sha256 the fetchurl of images/tempest/image.nix names
#      equal url and hash.value of deploy/overlays/kind/hypervisor-operator-fixtures/
#      image.yaml. Neither pin has a Renovate rule, so a bump of one has to
#      move the other by hand.
#   2. A copy of the two files whose image.nix carries another sha256 fails,
#      and the message names both files.
#   3. The rootLinks path of image.nix ends in the file name of the URL, and
#      the guest suite's seed Job and verify_tempest.sh read that path.
#
# Usage: bash tests/unit/images/tempest_cirros_pin_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

NIX_FILE="images/tempest/image.nix"
YAML_FILE="deploy/bases/hypervisor-fixtures/image.yaml"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# nix_value <root> <attribute>: the value of `<attribute> = "...";` in the
# image.nix below root.
nix_value() {
  sed -n "s/^ *$2 = \"\\(.*\\)\";\$/\\1/p" "$1/$NIX_FILE"
}

# yaml_value <root> <key>: the value of `<key>: ...` in the image.yaml below
# root.
yaml_value() {
  sed -n "s/^ *$2: *//p" "$1/$YAML_FILE"
}

# pin_mismatches <root>: one FAIL line per pin that differs between the two
# files below root, nothing when both agree. Returns 1 on a mismatch.
pin_mismatches() {
  local root="$1" rc=0 nix yaml
  nix="$(nix_value "$root" url)"
  yaml="$(yaml_value "$root" url)"
  if [ -z "$nix" ] || [ "$nix" != "$yaml" ]; then
    echo "FAIL: the cirros url of $NIX_FILE ($nix) differs from $YAML_FILE ($yaml); bump both pins together"
    rc=1
  fi
  nix="$(nix_value "$root" sha256)"
  yaml="$(yaml_value "$root" value)"
  if [ -z "$nix" ] || [ "$nix" != "$yaml" ]; then
    echo "FAIL: the cirros sha256 of $NIX_FILE ($nix) differs from $YAML_FILE ($yaml); bump both pins together"
    rc=1
  fi
  return "$rc"
}

# --- Test 1: the two pins agree ---
test_pins_agree() {
  echo "Test: the cirros URL and sha256 of the tempest image equal the kind fixture's"

  local out rc=0
  out="$(pin_mismatches "$PROJECT_ROOT")" || rc=$?
  assert_eq "the comparison passes" "0" "$rc"
  assert_eq "it prints no mismatch" "" "$out"
  assert_eq "image.nix names one URL" "1" "$(nix_value "$PROJECT_ROOT" url | grep -c .)"
  assert_eq "image.nix names one sha256" "1" "$(nix_value "$PROJECT_ROOT" sha256 | grep -c .)"
}

# --- Test 2: a bump of one pin alone fails ---
test_one_pin_alone_fails() {
  echo "Test: a sha256 changed in image.nix alone fails and names both files"

  # The copy's sha256 differs from the pin in its first digit, whatever the
  # pin is.
  local cur bad
  cur="$(nix_value "$PROJECT_ROOT" sha256)"
  bad="$([ "${cur:0:1}" = 0 ] && echo 1 || echo 0)${cur:1}"
  mkdir -p "$TMP_DIR/root/$(dirname "$NIX_FILE")" "$TMP_DIR/root/$(dirname "$YAML_FILE")"
  sed "s/sha256 = \"$cur\"/sha256 = \"$bad\"/" "$PROJECT_ROOT/$NIX_FILE" >"$TMP_DIR/root/$NIX_FILE"
  cp "$PROJECT_ROOT/$YAML_FILE" "$TMP_DIR/root/$YAML_FILE"

  local out rc=0
  out="$(pin_mismatches "$TMP_DIR/root")" || rc=$?
  assert_eq "the comparison fails" "1" "$rc"
  assert_contains "it names both files and both values" "$out" \
    "FAIL: the cirros sha256 of $NIX_FILE ($bad) differs from $YAML_FILE ($cur); bump both pins together"
  assert_not_contains "the unchanged URL is not reported" "$out" "cirros url"
}

# --- Test 3: the path the image places the file at, and its readers ---
test_path_and_readers() {
  echo "Test: the rootLinks path carries the URL's file name and its readers name it"

  local rel url path reader
  rel="$(sed -n 's/^ *rootLinks\."\(usr\/share\/cirros\/[^"]*\)" = .*/\1/p' "$PROJECT_ROOT/$NIX_FILE")"
  url="$(nix_value "$PROJECT_ROOT" url)"
  path="/$rel"
  assert_not_empty "image.nix places the file below /usr/share/cirros/" "$rel"
  assert_eq "the path ends in the URL's file name" "${url##*/}" "${rel##*/}"
  for reader in tests/e2e-nova-libvirt/guest-dataplane/04-image-seed-job.yaml \
    tests/container-images/verify_tempest.sh; do
    assert_file_contains_fixed "$reader reads $path" "$PROJECT_ROOT/$reader" "$path"
  done
}

# --- Run ---
test_pins_agree
test_one_pin_alone_fails
test_path_and_readers

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
