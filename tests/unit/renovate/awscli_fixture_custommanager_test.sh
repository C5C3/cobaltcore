#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# public.ecr.aws/aws-cli/aws-cli image pin in the garage-health e2e fixture,
# plus the paired packageRules:
#   - the docker-datasource matchStrings regex captures the image's depName,
#     tag (currentValue) AND digest (currentDigest)
#   - packageRules disable major bumps and automerge minor/patch/digest with a
#     3-day minimumReleaseAge
#
# This is the regression test the check-renovate-coverage skill requires for
# every customManager. It does NOT run renovate-config-validator itself — the
# sibling fluxoperator_custommanager_test.sh already exercises that
# authoritative gate over the whole file.
#
# Usage: bash tests/unit/renovate/awscli_fixture_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
FIXTURE_FILE="$PROJECT_ROOT/tests/e2e/infrastructure/garage-health/chainsaw-test.yaml"

AWSCLI_PACKAGE="public.ecr.aws/aws-cli/aws-cli"
FIXTURE_PATH="tests/e2e/infrastructure/garage-health/chainsaw-test.yaml"

# --- Test 1: customManager targets the fixture and captures depName/tag/digest ---
test_custom_manager_captures_image() {
  echo "Test: customManagers regex captures the aws-cli fixture depName/tag/digest"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  # The aws-cli-fixture manager is the docker-datasource customManager on the
  # garage-health fixture file.
  local entry
  entry="$(jq -c '.customManagers[]
    | select(.datasourceTemplate == "docker")
    | select((.managerFilePatterns // []) | join(",") | contains("garage-health"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no docker-datasource customManagers entry for $FIXTURE_PATH"
    FAIL=$((FAIL + 6))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is docker" \
    "docker" "$(jq -r '.datasourceTemplate' <<<"$entry")"

  local patterns
  patterns="$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")"
  assert_contains "managerFilePatterns targets the fixture file" "$patterns" "garage-health"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local line match_string
  line="$(grep -E 'image=public\.ecr\.aws/aws-cli/aws-cli' "$FIXTURE_FILE" | head -1)"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  assert_not_empty "aws-cli image line present in the fixture" "$line"

  # Confirm the matchStrings regex captures depName/currentValue/currentDigest
  # from the actual line (perl speaks the same PCRE named-group syntax Renovate
  # uses for the regex customManager).
  local captured
  captured="$(REGEX="$match_string" LINE="$line" perl -e '
    my $re = $ENV{REGEX}; my $l = $ENV{LINE};
    if ($l =~ /$re/) {
      print "depName=$+{depName}\n";
      print "currentValue=$+{currentValue}\n";
      print "currentDigest=$+{currentDigest}\n";
    }
  ')"

  assert_contains "regex captures the depName" "$captured" "depName=${AWSCLI_PACKAGE}"
  assert_contains "regex captures the tag as currentValue" "$captured" "currentValue="
  assert_contains "regex captures the sha256 digest" "$captured" "currentDigest=sha256:"
}

# --- Test 2: packageRules disable major, automerge minor/patch/digest ---
test_package_rules() {
  echo "Test: packageRules disable major bumps and automerge minor/patch/digest"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c --arg p "$AWSCLI_PACKAGE" --arg f "$FIXTURE_PATH" '.packageRules[]
    | select(((.matchPackageNames // []) | index($p)) != null
             and ((.matchFileNames // []) | index($f)) != null
             and ((.matchUpdateTypes // []) | index("major")) != null)' "$RENOVATE_FILE" | head -1)"
  minor_rule="$(jq -c --arg p "$AWSCLI_PACKAGE" --arg f "$FIXTURE_PATH" '.packageRules[]
    | select(((.matchPackageNames // []) | index($p)) != null
             and ((.matchFileNames // []) | index($f)) != null
             and ((.matchUpdateTypes // []) | index("minor")) != null)' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for the aws-cli fixture image"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major aws-cli fixture image updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for the aws-cli fixture image"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "minor/patch aws-cli fixture image updates are automerged" \
    "true" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "the rule also covers digest refreshes" \
    "true" "$(jq -r '(.matchUpdateTypes // []) | index("digest") != null' <<<"$minor_rule")"
  assert_eq "minor/patch aws-cli fixture image rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
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
