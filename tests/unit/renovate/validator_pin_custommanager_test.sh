#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the RENOVATE_VALIDATOR_VERSION pin the
# shell test suite feeds to `npx renovate-config-validator`, and pairs it
# with a packageRule that keeps a bump under human review.
#
# Asserts:
#   - exactly one customManager targets the npm package `renovate` over
#     tests/unit/renovate/*_test.sh
#   - its matchStrings regex captures the pin from the real test file and
#     that capture equals the literal constant
#   - exactly one test file carries the constant, so the three-way drift
#     that a hand edit or a partial revert would cause cannot happen
#   - that file still invokes the validator, so the suite's single schema
#     check cannot be deleted unnoticed
#   - exactly one packageRule scopes the pin and no rule scoped to the
#     same glob automerges: test-shell downloads and executes that
#     release on `main` and on `v*` tag pushes, and the validator run is
#     the only check that exercises it — a bump must not be able to
#     approve itself
#   - majors are NOT disabled: the validator has to follow the major the
#     hosted bot runs, or it silently stops checking renovate.json
#     against the schema that bot enforces
#
# Schema validation of renovate.json via `renovate-config-validator` is
# centralised in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh — the file that
# carries the pin asserted here.
#
# Usage: bash tests/unit/renovate/validator_pin_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
RENOVATE_TEST_DIR="$PROJECT_ROOT/tests/unit/renovate"
PIN_FILE="$RENOVATE_TEST_DIR/fluxoperator_custommanager_test.sh"

PKG_NAME="renovate"
FILE_GLOB="tests/unit/renovate/**.sh"

test_manager_exists_and_regex_matches() {
  echo "Test: the RENOVATE_VALIDATOR_VERSION pin has a customManager whose regex matches"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local count
  count="$(jq --arg pkg "$PKG_NAME" '[.customManagers[]
    | select(.packageNameTemplate == $pkg)] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets the ${PKG_NAME} npm package" "1" "$count"

  local entry
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.packageNameTemplate == $pkg)' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PKG_NAME}"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_contains "manager targets tests/unit/renovate/*_test.sh" \
    "$(jq -r '.managerFilePatterns[0]' <<<"$entry")" "tests/unit/renovate/"
  assert_eq "manager uses the npm datasource" \
    "npm" "$(jq -r '.datasourceTemplate' <<<"$entry")"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if [[ ! -f "$PIN_FILE" ]]; then
    echo "  FAIL: $PIN_FILE missing"
    FAIL=$((FAIL + 2))
    return
  fi

  local match_string captured expected_pin
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captured="$(REGEX="$match_string" FILE="$PIN_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    if ($content =~ /$re/) { print $+{currentValue} // ""; }
  ')"

  assert_not_empty "matchStrings regex captures the validator pin" "$captured"

  # Derive the expected pin straight from the test file so a Renovate bump
  # keeps this test green — never hard-code the version.
  expected_pin="$(grep -oE '^RENOVATE_VALIDATOR_VERSION="[0-9]+\.[0-9]+\.[0-9]+"' "$PIN_FILE" \
    | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  assert_eq "captured pin equals the literal RENOVATE_VALIDATOR_VERSION constant" \
    "$expected_pin" "$captured"
}

test_pin_is_declared_once() {
  echo "Test: exactly one test file declares the validator pin and runs the validator"

  # Renovate bumps every occurrence in one PR, so drift only comes from a
  # hand edit or a partial revert — and then two files run one validator
  # version while a third runs another, with nothing naming the cause.
  local pin_files
  pin_files="$( { grep -rlE '^RENOVATE_VALIDATOR_VERSION="' "$RENOVATE_TEST_DIR" || true; } | wc -l | tr -d ' ')"
  assert_eq "exactly one tests/unit/renovate file declares RENOVATE_VALIDATOR_VERSION" \
    "1" "$pin_files"

  # The sibling tests delegate schema validation to this file. Drop the
  # call and renovate.json stops being checked against the schema the
  # hosted bot enforces — the pin above still lines up, so nothing else
  # in the suite goes red.
  assert_file_contains "the pin file actually invokes renovate-config-validator" \
    "$PIN_FILE" "renovate-config-validator renovate.json"
}

test_package_rule_keeps_bumps_under_review() {
  echo "Test: the paired packageRule proposes every update type but never automerges"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi

  # Exactly one rule: a second one could re-disable majors or flip
  # automerge back on without this test noticing.
  local rule_count
  rule_count="$(jq --arg pkg "$PKG_NAME" --arg glob "$FILE_GLOB" '[.packageRules[]
    | select(
        (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchFileNames // []) | index($glob)) != null)
      )] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one packageRule scopes the validator pin" "1" "$rule_count"

  # The count above only sees rules that name the package. A later rule
  # scoped to the same glob but without matchPackageNames matches the pin
  # too, and Renovate lets the later match win — automerge would be back
  # on with the count still at 1.
  local automerge_rules
  automerge_rules="$(jq --arg glob "$FILE_GLOB" '[.packageRules[]
    | select(((.matchFileNames // []) | index($glob)) != null)
    | select(.automerge == true)] | length' "$RENOVATE_FILE")"
  assert_eq "no packageRule automerges anything scoped to ${FILE_GLOB}" \
    "0" "$automerge_rules"

  local rule
  rule="$(jq -c --arg pkg "$PKG_NAME" --arg glob "$FILE_GLOB" '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchFileNames // []) | index($glob)) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$rule" ]; then
    echo "  FAIL: no packageRule scoping ${PKG_NAME} to ${FILE_GLOB}"
    FAIL=$((FAIL + 5))
    return
  fi

  assert_eq "validator bumps are never automerged" \
    "false" "$(jq -r '.automerge' <<<"$rule")"
  assert_eq "validator bumps wait minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$rule")"
  assert_eq "validator bumps are grouped as renovate-config-validator" \
    "renovate-config-validator" "$(jq -r '.groupName' <<<"$rule")"

  # An `enabled: false` here (or a missing `major` update type) would
  # freeze the validator on its current major with no PR and no dashboard
  # entry, while the hosted bot keeps moving.
  assert_eq "the validator pin is not disabled" \
    "null" "$(jq -r '.enabled' <<<"$rule")"
  assert_eq "major validator updates still open a PR" \
    "true" \
    "$(jq -r '((.matchUpdateTypes // []) | index("major")) != null' <<<"$rule")"
}

test_manager_exists_and_regex_matches
test_pin_is_declared_once
test_package_rule_keeps_bumps_under_review

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
