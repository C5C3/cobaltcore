#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has the source-refs.yaml customManager + packageRules
# described by (OpenStack release tag tracking):
#   - a customManagers entry targeting releases/<release>/source-refs.yaml
#     whose matchStrings regex extracts (depName, x.y.z) from every entry
#   - a paired packageRule set that disables majors and automerges minor/patch
#     with minimumReleaseAge=3 days, groupName="OpenStack upstream tags"
#
# The matchStrings regex is replayed with Perl, which speaks the same
# PCRE-style syntax Renovate uses.
#
# Schema validation of renovate.json via `renovate-config-validator`
# (which transitively pulls Renovate via npx) is intentionally NOT run
# from this per-feature test to keep local / CI loops fast: the validator
# fetches the Renovate package on every invocation and taking that hit
# once per feature touching renovate.json multiplies the cost linearly.
# The validation is centralised in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh, which runs
# against the same renovate.json file.
#
# Usage: bash tests/unit/renovate/source_refs_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
SOURCE_REFS_FILE="$PROJECT_ROOT/releases/2026.1/source-refs.yaml"

# NOTE renovate-config-validator schema check is run by the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh over the same
# renovate.json — see the header of this file.

test_custom_manager_regex_captures_depname_and_version() {
  echo "Test: customManagers regex extracts (depName, x.y.z) from source-refs.yaml"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("source-refs"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting releases/*/source-refs.yaml"
    FAIL=$((FAIL + 5))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is git-tags" \
    "git-tags" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"

  assert_eq "customManagers.versioningTemplate is semver" \
    "semver" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"

  assert_contains "customManagers.packageNameTemplate references opendev.org/openstack" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")" \
    "opendev.org/openstack"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if [[ ! -f "$SOURCE_REFS_FILE" ]]; then
    echo "  SKIP: $SOURCE_REFS_FILE missing (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local match_string
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local captured_dep captured_ver
  captured_dep="$(REGEX="$match_string" FILE="$SOURCE_REFS_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    if ($content =~ /$re/m) { print $+{depName} // ""; }
  ')"
  captured_ver="$(REGEX="$match_string" FILE="$SOURCE_REFS_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    if ($content =~ /$re/m) { print $+{currentValue} // ""; }
  ')"

  assert_not_empty "matchStrings regex captures a depName from source-refs.yaml" \
    "$captured_dep"
  assert_not_empty "matchStrings regex captures an x.y.z currentValue" \
    "$captured_ver"
}

test_package_rules_disable_majors_and_group() {
  echo "Test: packageRules disable major source-refs bumps, automerge minor/patch"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("releases/**/source-refs.yaml")) != null
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("releases/**/source-refs.yaml")) != null
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for releases/**/source-refs.yaml"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major source-refs updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for source-refs.yaml"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "minor/patch source-refs updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch source-refs updates carry matchUpdateTypes=patch" \
    "true" \
    "$(jq -r '(.matchUpdateTypes // []) | index("patch") != null' <<<"$minor_rule")"
  assert_eq "minor/patch source-refs rule waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "minor/patch source-refs rule groupName is 'OpenStack upstream tags'" \
    "OpenStack upstream tags" \
    "$(jq -r '.groupName' <<<"$minor_rule")"
}

test_custom_manager_regex_captures_depname_and_version
test_package_rules_disable_majors_and_group

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
