#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has a customManager covering the GOFUMPT_VERSION pin
# in the root Makefile (which must stay in sync with the workflow env var of
# the same name). The manager is shared with workflow files via a multi-entry
# managerFilePatterns array.
#
# Usage: bash tests/unit/renovate/makefile_pin_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
MAKEFILE="$PROJECT_ROOT/Makefile"

test_makefile_in_a_manager_file_pattern() {
  echo "Test: at least one customManager.managerFilePatterns covers the root Makefile"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local hit
  hit="$(jq -r '.customManagers[].managerFilePatterns[]?
    | select(test("/Makefile\\$/"))' "$RENOVATE_FILE" | head -1)"

  if [ -z "$hit" ]; then
    echo "  FAIL: no customManager.managerFilePatterns matches /Makefile\$/"
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS: customManager covers Makefile via $hit"
  PASS=$((PASS + 1))
}

test_gofumpt_regex_matches_makefile_pin() {
  echo "Test: GOFUMPT_VERSION regex captures the literal in Makefile"

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
  if [[ ! -f "$MAKEFILE" ]]; then
    echo "  SKIP: $MAKEFILE missing"
    SKIP=$((SKIP + 1))
    return
  fi

  local entry match_string captured expected
  entry="$(jq -c '.customManagers[]
    | select(.packageNameTemplate == "mvdan/gofumpt")' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager has packageNameTemplate=mvdan/gofumpt"
    FAIL=$((FAIL + 1))
    return
  fi

  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captured="$(REGEX="$match_string" FILE="$MAKEFILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    if ($content =~ /$re/) { print $+{currentValue} // ""; }
  ')"
  expected="$(grep -E '^GOFUMPT_VERSION' "$MAKEFILE" | head -1 \
    | sed -E 's/.*(v[0-9]+\.[0-9]+\.[0-9]+).*/\1/')"

  assert_eq "GOFUMPT_VERSION regex captures the Makefile pin" \
    "$expected" "$captured"
}

test_gofumpt_package_rule_disables_majors() {
  echo "Test: packageRule disables majors for mvdan/gofumpt across Makefile + workflows"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchPackageNames // []) | index("mvdan/gofumpt")) != null
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchPackageNames // []) | index("mvdan/gofumpt")) != null
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule disables majors for mvdan/gofumpt"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major mvdan/gofumpt updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor/patch mvdan/gofumpt updates"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "minor/patch mvdan/gofumpt updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
}

test_makefile_in_a_manager_file_pattern
test_gofumpt_regex_matches_makefile_pin
test_gofumpt_package_rule_disables_majors

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
