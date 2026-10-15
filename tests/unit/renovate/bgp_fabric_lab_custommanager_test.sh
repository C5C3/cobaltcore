#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares the two customManagers of the BGP fabric
# containerlab and their paired packageRules:
#   - quay.io/frrouting/frr in tests/lab/bgp-fabric/bgp-fabric.clab.yml, the
#     image every lab node runs
#   - ghcr.io/srl-labs/clab in tests/lab/bgp-fabric/run.sh, the
#     CLAB_IMAGE_DEFAULT the runner deploys and destroys the lab with
# For each: the docker-datasource regex captures depName, tag (currentValue)
# and digest (currentDigest) from the pinned line, the versioning is docker,
# major bumps are disabled, and minor/patch/digest wait 3 days WITHOUT
# automerge in the "bgp-fabric lab" group.
#
# Why no automerge: no CI job runs the lab (the job belongs to the frr-k8s
# substrate work), so a bump stays unverified until run.sh is run by hand and
# the run record on docs/reference/networking/bgp-fabric-contract.md is
# refreshed.
#
# This is the regression test the check-renovate-coverage skill requires for
# every customManager. It does NOT run renovate-config-validator itself; the
# sibling fluxoperator_custommanager_test.sh exercises that gate over the
# whole file.
#
# Usage: bash tests/unit/renovate/bgp_fabric_lab_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

FRR_PACKAGE="quay.io/frrouting/frr"
FRR_PATH="tests/lab/bgp-fabric/bgp-fabric.clab.yml"
CLAB_PACKAGE="ghcr.io/srl-labs/clab"
CLAB_PATH="tests/lab/bgp-fabric/run.sh"

# The pins as package|file|grep pattern of the pinned line.
PINS="$FRR_PACKAGE|$FRR_PATH|image: quay\.io/frrouting/frr:
$CLAB_PACKAGE|$CLAB_PATH|^CLAB_IMAGE_DEFAULT=\"ghcr\.io/srl-labs/clab:"

# manager_for <path>: the customManagers entry whose managerFilePatterns name
# <path> (Renovate regexes, so the backslash escapes are dropped first).
manager_for() {
  jq -c --arg f "$1" '.customManagers[]
    | select((.managerFilePatterns // []) | map(gsub("\\\\"; "")) | join(",") | contains($f))' \
    "$RENOVATE_FILE"
}

# --- Test 1: each customManager captures depName/tag/digest from its pin ---
test_custom_managers_capture_the_pins() {
  echo "Test: each customManager captures depName, tag and digest from its pinned line"

  if ! command -v jq >/dev/null 2>&1 || ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: jq or perl not installed (14 checks skipped)"
    SKIP=$((SKIP + 14))
    return
  fi

  local pkg path pattern entry line captured
  while IFS='|' read -r pkg path pattern; do
    entry="$(manager_for "$path")"
    if [ -z "$entry" ]; then
      echo "  FAIL: no customManagers entry for $path"
      FAIL=$((FAIL + 7))
      continue
    fi
    assert_eq "the manager for $path has one entry" "1" "$(wc -l <<<"$entry" | tr -d '[:space:]')"
    assert_eq "the manager for $path uses the docker datasource" \
      "docker" "$(jq -r '.datasourceTemplate' <<<"$entry")"
    assert_eq "the manager for $path uses docker versioning" \
      "docker" "$(jq -r '.versioningTemplate' <<<"$entry")"

    line="$(grep -E "$pattern" "$PROJECT_ROOT/$path" || true)"
    assert_not_empty "the pinned line is present in $path" "$line"

    # perl speaks the PCRE named-group syntax Renovate's regex manager uses.
    captured="$(REGEX="$(jq -r '.matchStrings[0]' <<<"$entry")" LINE="$line" perl -e '
      my $re = $ENV{REGEX}; my $l = $ENV{LINE};
      if ($l =~ /$re/) {
        print "depName=$+{depName}\n";
        print "currentValue=$+{currentValue}\n";
        print "currentDigest=$+{currentDigest}\n";
      }
    ')"
    assert_contains "the regex captures depName $pkg in $path" "$captured" "depName=$pkg"
    assert_contains "the regex captures a dotted tag as currentValue in $path" \
      "$(grep '^currentValue=' <<<"$captured" || true)" "."
    assert_contains "the regex captures the sha256 digest in $path" "$captured" "currentDigest=sha256:"
  done <<<"$PINS"
}

# --- Test 2: packageRules disable major, gate minor/patch/digest, no automerge ---
test_package_rules() {
  echo "Test: packageRules disable major bumps and never automerge minor/patch/digest"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (12 checks skipped)"
    SKIP=$((SKIP + 12))
    return
  fi

  local pkg path pattern major_rule minor_rule
  while IFS='|' read -r pkg path pattern; do
    major_rule="$(jq -c --arg p "$pkg" --arg f "$path" '.packageRules[]
      | select(((.matchPackageNames // []) | index($p)) != null
               and ((.matchFileNames // []) | index($f)) != null
               and ((.matchUpdateTypes // []) | index("major")) != null)' "$RENOVATE_FILE")"
    minor_rule="$(jq -c --arg p "$pkg" --arg f "$path" '.packageRules[]
      | select(((.matchPackageNames // []) | index($p)) != null
               and ((.matchFileNames // []) | index($f)) != null
               and ((.matchUpdateTypes // []) | index("minor")) != null)' "$RENOVATE_FILE")"

    if [ -z "$major_rule" ]; then
      echo "  FAIL: no packageRule scoping major updates of $pkg in $path"
      FAIL=$((FAIL + 1))
    else
      assert_eq "major updates of $pkg are disabled" \
        "false" "$(jq -r '.enabled' <<<"$major_rule")"
    fi

    if [ -z "$minor_rule" ]; then
      echo "  FAIL: no packageRule scoping minor/patch updates of $pkg in $path"
      FAIL=$((FAIL + 5))
      continue
    fi
    assert_eq "minor/patch updates of $pkg are NOT automerged" \
      "false" "$(jq -r '.automerge' <<<"$minor_rule")"
    assert_eq "the $pkg rule also covers digest refreshes" \
      "true" "$(jq -r '(.matchUpdateTypes // []) | index("digest") != null' <<<"$minor_rule")"
    assert_eq "the $pkg rule waits minimumReleaseAge=3 days" \
      "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
    assert_eq "the $pkg rule is in the bgp-fabric lab group" \
      "bgp-fabric lab" "$(jq -r '.groupName' <<<"$minor_rule")"
    assert_eq "the $pkg rule matches the custom.regex manager" \
      '["custom.regex"]' "$(jq -c '.matchManagers' <<<"$minor_rule")"
  done <<<"$PINS"
}

# --- Test 3: each image is pinned once, so the one manager match is the pin ---
test_each_image_is_pinned_once() {
  echo "Test: each lab image is pinned on exactly one line under tests/lab/"

  # clab-bgp-fabric/ is the working directory containerlab writes during a
  # run (gitignored, root-owned); it is not part of the lab.
  local lab="$PROJECT_ROOT/tests/lab/bgp-fabric" pkg path pattern count
  while IFS='|' read -r pkg path pattern; do
    count="$(find "$lab" -path "$lab/clab-bgp-fabric" -prune -o -type f -print |
      while IFS= read -r f; do grep -hF "$pkg:" "$f" || true; done | wc -l | tr -d '[:space:]')"
    assert_eq "$pkg appears on one line under tests/lab/" "1" "$count"
  done <<<"$PINS"
}

# --- Run ---
test_custom_managers_capture_the_pins
test_package_rules
test_each_image_is_pinned_once

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
