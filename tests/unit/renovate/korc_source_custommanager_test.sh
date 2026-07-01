#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has a customManager for the K-ORC GitRepository tag in
# deploy/flux-system/sources/k-orc.yaml. The manager must:
#   - target the file, not the broader deploy/flux-system/ dir
#   - use datasource=github-releases with the k-orc repo package name
#   - capture the ref.tag value from `tag: vX.Y.Z`
#   - be paired with a packageRule that disables majors and automerges
#     minor/patch with minimumReleaseAge=3 days
# This closes the coverage gap the un-tracked Flux tag left: it and the
# Renovate-tracked go.mod k-orc module can no longer drift a minor version apart.
#
# Usage: bash tests/unit/renovate/korc_source_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
SOURCE_FILE="$PROJECT_ROOT/deploy/flux-system/sources/k-orc.yaml"

test_custom_manager_uses_github_releases() {
  echo "Test: k-orc customManager uses datasource=github-releases with the k-orc repo"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("k-orc"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting deploy/flux-system/sources/k-orc.yaml"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "k-orc customManager.datasourceTemplate is github-releases" \
    "github-releases" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "k-orc customManager.depNameTemplate is the k-orc repo" \
    "k-orc/openstack-resource-controller" \
    "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_eq "k-orc customManager.packageNameTemplate is the k-orc repo" \
    "k-orc/openstack-resource-controller" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")"
}

test_regex_captures_ref_tag() {
  echo "Test: k-orc customManager regex captures the GitRepository ref.tag"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed"
    SKIP=$((SKIP + 1))
    return
  fi
  if [[ ! -f "$SOURCE_FILE" ]]; then
    echo "  SKIP: $SOURCE_FILE missing"
    SKIP=$((SKIP + 1))
    return
  fi

  local entry match_string tag_line captured expected_value
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("k-orc"))' \
    "$RENOVATE_FILE")"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  tag_line="$(grep -E '^[[:space:]]*tag:[[:space:]]*v' "$SOURCE_FILE" | head -1)"
  assert_not_empty "ref.tag line present in deploy/flux-system/sources/k-orc.yaml" \
    "$tag_line"

  captured="$(REGEX="$match_string" LINE="$tag_line" perl -e '
    my $re = $ENV{REGEX};
    my $line = $ENV{LINE};
    if ($line =~ /$re/) { print $+{currentValue} // ""; }
  ')"

  expected_value="$(printf '%s' "$tag_line" \
    | sed -E 's/.*tag:[[:space:]]*(v[0-9]+\.[0-9]+\.[0-9]+).*/\1/')"

  assert_eq "matchStrings regex captures the k-orc ref.tag version" \
    "$expected_value" "$captured"
}

test_package_rules_for_korc() {
  echo "Test: packageRules disable major k-orc bumps, automerge minor/patch"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("k-orc/openstack-resource-controller")) != null
         or ((.matchFileNames   // []) | index("deploy/flux-system/sources/k-orc.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("k-orc/openstack-resource-controller")) != null
         or ((.matchFileNames   // []) | index("deploy/flux-system/sources/k-orc.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for k-orc"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major k-orc updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for k-orc"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "minor/patch k-orc updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch k-orc rule waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "minor/patch k-orc rule groupName is k-orc" \
    "k-orc" \
    "$(jq -r '.groupName' <<<"$minor_rule")"
}

test_custom_manager_uses_github_releases
test_regex_captures_ref_tag
test_package_rules_for_korc

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
