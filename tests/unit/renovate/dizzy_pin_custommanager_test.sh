#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the single DIZZY_VERSION pin in hack/dizzy.sh
# and pairs it with the expected packageRules.
#
# Asserts:
#   - exactly one customManager targets B42Labs/dizzy over hack/dizzy.sh with
#     the github-releases datasource
#   - its matchStrings regex captures the current pin from the real script and
#     that capture equals the literal DIZZY_VERSION default in the file
#   - a paired packageRule disables majors
#   - a paired packageRule proposes minor/patch after a 3-day cooldown but
#     never automerges — no CI lane exercises the WITH_DIZZY path, so a dizzy
#     release that renames/relocates a staged dashboard must land under human
#     review, not silently on main
#
# Usage: bash tests/unit/renovate/dizzy_pin_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
DIZZY_FILE="$PROJECT_ROOT/hack/dizzy.sh"

PKG_NAME="B42Labs/dizzy"

test_manager_exists_and_regex_matches() {
  echo "Test: the DIZZY_VERSION pin has a customManager whose regex matches"

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
  if [[ ! -f "$DIZZY_FILE" ]]; then
    echo "  FAIL: $DIZZY_FILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local count
  count="$(jq --arg pkg "$PKG_NAME" '[.customManagers[]
    | select(.packageNameTemplate == $pkg)] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets ${PKG_NAME}" "1" "$count"

  local entry
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.packageNameTemplate == $pkg)' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_contains "manager targets hack/dizzy.sh" \
    "$(jq -r '.managerFilePatterns[0]' <<<"$entry")" "dizzy"
  assert_eq "manager uses the github-releases datasource" \
    "github-releases" "$(jq -r '.datasourceTemplate' <<<"$entry")"

  local match_string captured expected_pin
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captured="$(REGEX="$match_string" FILE="$DIZZY_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    if ($content =~ /$re/) { print $+{currentValue} // ""; }
  ')"

  assert_not_empty "matchStrings regex captures the dizzy pin" "$captured"

  # Derive the expected pin straight from the script so a Renovate bump keeps
  # this test green — never hard-code the version.
  expected_pin="$(grep -oE 'DIZZY_VERSION:-v[0-9]+\.[0-9]+\.[0-9]+' "$DIZZY_FILE" \
    | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  assert_eq "captured pin equals the literal DIZZY_VERSION default" \
    "$expected_pin" "$captured"
}

test_package_rules() {
  echo "Test: paired packageRules gate dizzy major vs minor/patch updates"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("hack/dizzy.sh")) != null
        and (((.matchPackageNames // []) | index("B42Labs/dizzy")) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("hack/dizzy.sh")) != null
        and (((.matchPackageNames // []) | index("B42Labs/dizzy")) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for hack/dizzy.sh"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major dizzy updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor/patch dizzy updates"
    FAIL=$((FAIL + 2))
    return
  fi

  assert_eq "minor/patch dizzy updates are not automerged (no WITH_DIZZY CI gate)" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch dizzy rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
}

test_manager_exists_and_regex_matches
test_package_rules

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
