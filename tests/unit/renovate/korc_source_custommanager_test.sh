#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has a customManager for the K-ORC GitRepository commit pin
# in deploy/flux-system/sources/k-orc.yaml. The source is pinned to an upstream
# main commit (a released K-ORC does not yet ship the RoleAssignment kind the
# c5c3-operator now Owns()), so the manager must:
#   - target the file, not the broader deploy/flux-system/ dir
#   - use datasource=git-refs with the k-orc repo URL as the package name
#   - track the branch (currentValue=main) as a digest
#   - capture the ref.commit value from `commit: <40 hex>`
#   - be paired with a packageRule that automerges the digest bump with
#     minimumReleaseAge=3 days
# This closes the coverage gap the un-tracked Flux commit left: it and the
# Renovate-tracked go.mod k-orc module can no longer drift apart.
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

KORC_PACKAGE="https://github.com/k-orc/openstack-resource-controller"

test_custom_manager_uses_git_refs() {
  echo "Test: k-orc customManager uses datasource=git-refs tracking the main branch"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("k-orc"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting deploy/flux-system/sources/k-orc.yaml"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "k-orc customManager.datasourceTemplate is git-refs" \
    "git-refs" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "k-orc customManager.depNameTemplate is the k-orc repo URL" \
    "$KORC_PACKAGE" \
    "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_eq "k-orc customManager.packageNameTemplate is the k-orc repo URL" \
    "$KORC_PACKAGE" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")"
  assert_eq "k-orc customManager.currentValueTemplate tracks the main branch" \
    "main" \
    "$(jq -r '.currentValueTemplate' <<<"$entry")"
}

test_regex_captures_ref_commit() {
  echo "Test: k-orc customManager regex captures the GitRepository ref.commit digest"

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

  local entry match_string commit_line captured expected_value
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("k-orc"))' \
    "$RENOVATE_FILE")"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  commit_line="$(grep -E '^[[:space:]]*commit:[[:space:]]*[a-f0-9]{40}' "$SOURCE_FILE" | head -1)"
  assert_not_empty "ref.commit line present in deploy/flux-system/sources/k-orc.yaml" \
    "$commit_line"

  captured="$(REGEX="$match_string" LINE="$commit_line" perl -e '
    my $re = $ENV{REGEX};
    my $line = $ENV{LINE};
    if ($line =~ /$re/) { print $+{currentDigest} // ""; }
  ')"

  expected_value="$(printf '%s' "$commit_line" \
    | sed -E 's/.*commit:[[:space:]]*([a-f0-9]{40}).*/\1/')"

  assert_eq "matchStrings regex captures the k-orc ref.commit digest" \
    "$expected_value" "$captured"
}

test_package_rules_for_korc() {
  echo "Test: packageRules automerge the k-orc digest bump with a 3-day age gate"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local digest_rule
  digest_rule="$(jq -c --arg pkg "$KORC_PACKAGE" '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index($pkg)) != null
         or ((.matchFileNames   // []) | index("deploy/flux-system/sources/k-orc.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("digest")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$digest_rule" ]; then
    echo "  FAIL: no packageRule scoping digest updates for k-orc"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "digest k-orc updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$digest_rule")"
  assert_eq "digest k-orc rule waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$digest_rule")"
  assert_eq "digest k-orc rule groupName is k-orc" \
    "k-orc" \
    "$(jq -r '.groupName' <<<"$digest_rule")"
}

test_custom_manager_uses_git_refs
test_regex_captures_ref_commit
test_package_rules_for_korc

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
