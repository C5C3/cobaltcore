#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# quay.io/ceph/ceph image pin of the lab Ceph, plus the paired packageRules:
#   - the manager reads deploy/lab/metal-stack/ceph/cluster/cluster.yaml, the
#     CephCluster, and toolbox.yaml, the toolbox that runs the same image,
#     with docker as datasource and versioning;
#   - its matchStrings regex captures depName, the tag (currentValue) and the
#     digest (currentDigest) from the real image: line of each file, the whole
#     pin, so a bump rewrites both lines alike;
#   - packageRules disable major bumps and hold minor, patch and digest
#     updates for 3 days without automerge, in one group for both files: a
#     Ceph minor is a question of the versions the pinned Rook supports.
#
# This is the regression test the check-renovate-coverage skill requires for
# every customManager; that skill's audit does not scan deploy/lab/. It does
# NOT run renovate-config-validator itself: the sibling
# fluxoperator_custommanager_test.sh already exercises that gate over the
# whole file.
#
# Usage: bash tests/unit/renovate/ceph_image_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

CEPH_PACKAGE="quay.io/ceph/ceph"
CLUSTER_PATH="deploy/lab/metal-stack/ceph/cluster/cluster.yaml"
TOOLBOX_PATH="deploy/lab/metal-stack/ceph/cluster/toolbox.yaml"

# ceph_manager — the docker-datasource customManagers entry for the CephCluster.
ceph_manager() {
  jq -c '.customManagers[]
    | select(.datasourceTemplate == "docker")
    | select((.managerFilePatterns // []) | join(",") | contains("deploy/lab/metal-stack/ceph/cluster/cluster"))' \
    "$RENOVATE_FILE"
}

# --- Test 1: the customManager and what its regex captures ---
test_custom_manager_captures_image() {
  echo "Test: the customManager regex captures the Ceph image's depName, tag and digest in both files"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (12 checks skipped)"
    SKIP=$((SKIP + 12))
    return
  fi

  local entry
  entry="$(ceph_manager)"
  if [ -z "$entry" ]; then
    echo "  FAIL: no docker-datasource customManagers entry for $CLUSTER_PATH"
    FAIL=$((FAIL + 12))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is docker" \
    "docker" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "customManagers.versioningTemplate is docker" \
    "docker" "$(jq -r '.versioningTemplate' <<<"$entry")"
  assert_eq "managerFilePatterns names the CephCluster and the toolbox alone" \
    '["/deploy/lab/metal-stack/ceph/cluster/cluster\\.yaml$/","/deploy/lab/metal-stack/ceph/cluster/toolbox\\.yaml$/"]' \
    "$(jq -c '.managerFilePatterns' <<<"$entry")"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (9 checks skipped)"
    SKIP=$((SKIP + 9))
    return
  fi

  local match_string path lines line captured pins=""
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  for path in "$CLUSTER_PATH" "$TOOLBOX_PATH"; do
    # Anchored on the key at the start of a line, so a header comment that
    # names the image can never stand in for the real line.
    lines="$( { grep -E '^[[:space:]]*image: quay\.io/ceph/ceph' "$PROJECT_ROOT/$path" || true; } | sed -E 's/^[[:space:]]*//')"
    assert_eq "$path has exactly one quay.io/ceph/ceph image: line" "1" "$(grep -c . <<<"$lines")"
    line="$(head -n 1 <<<"$lines")"

    # perl speaks the PCRE named-group syntax Renovate uses for the regex
    # customManager.
    captured="$(REGEX="$match_string" LINE="$line" perl -e '
      my $re = $ENV{REGEX}; my $l = $ENV{LINE};
      if ($l =~ /$re/) {
        print "depName=$+{depName}\n";
        print "currentValue=$+{currentValue}\n";
        print "currentDigest=$+{currentDigest}\n";
      }
    ')"
    assert_eq "the regex captures the depName in $path" "$CEPH_PACKAGE" \
      "$(sed -n 's/^depName=//p' <<<"$captured")"
    assert_eq "the regex captures the whole tag in $path" \
      "$(sed -E 's/.*ceph:([^@]+)@.*/\1/' <<<"$line")" "$(sed -n 's/^currentValue=//p' <<<"$captured")"
    # Renovate rewrites the captured span, so the three parts must rebuild the
    # whole pin, or a bump would corrupt it.
    assert_eq "the three captures rebuild the line of $path" "$line" \
      "image: $(sed -n 's/^depName=//p' <<<"$captured"):$(sed -n 's/^currentValue=//p' <<<"$captured")@$(sed -n 's/^currentDigest=//p' <<<"$captured")"
    pins="${pins}${line}"$'\n'
  done
  assert_eq "the CephCluster and the toolbox pin the same image" "1" \
    "$(grep -v '^$' <<<"$pins" | sort -u | grep -c .)"
}

# --- Test 2: packageRules disable major, hold the rest without automerge ---
test_package_rules() {
  echo "Test: packageRules disable Ceph majors and hold minor/patch/digest for 3 days without automerge"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c --arg p "$CEPH_PACKAGE" --arg f "$CLUSTER_PATH" '.packageRules[]
    | select(((.matchPackageNames // []) | index($p)) != null
             and ((.matchFileNames // []) | index($f)) != null
             and ((.matchUpdateTypes // []) | index("major")) != null)' "$RENOVATE_FILE" | head -1)"
  minor_rule="$(jq -c --arg p "$CEPH_PACKAGE" --arg f "$CLUSTER_PATH" '.packageRules[]
    | select(((.matchPackageNames // []) | index($p)) != null
             and ((.matchFileNames // []) | index($f)) != null
             and ((.matchUpdateTypes // []) | index("minor")) != null)' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for the Ceph image"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major Ceph image updates are disabled" "false" "$(jq -r '.enabled' <<<"$major_rule")"
    assert_eq "the major rule names both files" "$CLUSTER_PATH,$TOOLBOX_PATH" \
      "$(jq -r '.matchFileNames | join(",")' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch/digest updates for the Ceph image"
    FAIL=$((FAIL + 6))
    return
  fi
  assert_eq "the rule covers minor, patch and digest updates" \
    '["minor","patch","digest"]' "$(jq -c '.matchUpdateTypes' <<<"$minor_rule")"
  assert_eq "minor/patch/digest Ceph image updates are not automerged" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "the rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "the CephCluster and the toolbox move in one group" \
    "ceph image" "$(jq -r '.groupName' <<<"$minor_rule")"
  assert_eq "the rule names both files" "$CLUSTER_PATH,$TOOLBOX_PATH" \
    "$(jq -r '.matchFileNames | join(",")' <<<"$minor_rule")"
  assert_eq "the rule is scoped to the regex manager" '["custom.regex"]' \
    "$(jq -c '.matchManagers' <<<"$minor_rule")"
}

# --- Run ---
test_custom_manager_captures_image
test_package_rules

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
