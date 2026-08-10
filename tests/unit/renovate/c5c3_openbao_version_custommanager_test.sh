#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the defaultOpenBaoVersion pin in
# operators/c5c3/internal/controller/reconcile_barbican_openbao.go and pairs it
# with the expected packageRules.
#
# The constant is the image tag every OpenBao instance the ControlPlane projects
# for Barbican runs. It is a Go string literal, so no native manager sees it,
# and left untracked it drifts away from the proving instance in
# deploy/kind/infrastructure/openbao-instance.yaml until the two run different
# OpenBao lines.
#
# Asserts:
#   - exactly one customManager targets openbao/openbao over the controller
#     source with the github-releases datasource and the extractVersionTemplate
#     that strips the v prefix of the upstream release tags
#   - its matchStrings regex captures the current pin from the real source file
#     exactly once, and that capture equals the literal of the const assignment
#   - a paired packageRule disables majors
#   - a paired packageRule proposes minor/patch after a 3-day cooldown, never
#     automerges, and groups with the instance manifest so both pins move in one
#     PR
#
# Usage: bash tests/unit/renovate/c5c3_openbao_version_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
CONTROLLER_FILE="$PROJECT_ROOT/operators/c5c3/internal/controller/reconcile_barbican_openbao.go"

PKG_NAME="openbao/openbao"
CONTROLLER_PATH="operators/c5c3/internal/controller/reconcile_barbican_openbao.go"
INSTANCE_PATH="deploy/kind/infrastructure/openbao-instance.yaml"

test_manager_exists_and_regex_matches() {
  echo "Test: the defaultOpenBaoVersion pin has a customManager whose regex matches"

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
  if [[ ! -f "$CONTROLLER_FILE" ]]; then
    echo "  FAIL: $CONTROLLER_FILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local count
  count="$(jq --arg pkg "$PKG_NAME" '[.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("reconcile_barbican_openbao")))] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets ${PKG_NAME} over the controller source" \
    "1" "$count"

  local entry
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("reconcile_barbican_openbao")))' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PKG_NAME} over ${CONTROLLER_PATH}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_contains "manager targets the c5c3 barbican OpenBao controller" \
    "$(jq -r '.managerFilePatterns[0]' <<<"$entry")" "reconcile_barbican_openbao"
  assert_eq "manager uses the github-releases datasource" \
    "github-releases" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "manager strips the v prefix of the upstream release tags" \
    '^v(?<version>.+)$' "$(jq -r '.extractVersionTemplate' <<<"$entry")"
  assert_eq "manager parses the captured pin as semver" \
    "semver" "$(jq -r '.versioningTemplate' <<<"$entry")"

  local match_string captures captured match_count expected_pin
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captures="$(REGEX="$match_string" FILE="$CONTROLLER_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    while ($content =~ /$re/g) {
      print "$+{currentValue}\n" if defined $+{currentValue};
    }
  ')"
  captured="$(printf '%s\n' "$captures" | head -1)"
  match_count="$(printf '%s\n' "$captures" | grep -c . || true)"

  assert_not_empty "matchStrings regex captures the controller version pin" "$captured"
  # The file also names the const in its doc comment and reads it back where the
  # instance spec is built. A regex loose enough to reach either would hand
  # Renovate a second, bogus dep to bump.
  assert_eq "matchStrings regex captures exactly one value in the source" \
    "1" "$match_count"

  # Derive the expected pin straight from the source so a Renovate bump keeps
  # this test green — never hard-code the version.
  expected_pin="$(grep -oE 'defaultOpenBaoVersion = "[0-9]+\.[0-9]+\.[0-9]+"' "$CONTROLLER_FILE" \
    | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  assert_eq "captured pin equals the literal const assignment" \
    "$expected_pin" "$captured"
}

test_package_rules() {
  echo "Test: paired packageRules gate the controller pin's major vs minor/patch updates"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c --arg path "$CONTROLLER_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c --arg path "$CONTROLLER_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for $CONTROLLER_PATH"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major controller-pin updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor/patch controller-pin updates"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "minor/patch controller-pin updates are not automerged (operator support window)" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch controller-pin rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"

  # The projected instances and the proving one have to stay on the same OpenBao
  # line, so the two pins must arrive in a single PR rather than race each other.
  assert_contains "the minor/patch rule also covers the instance manifest" \
    "$(jq -r '.matchFileNames | join(" ")' <<<"$minor_rule")" "$INSTANCE_PATH"
}

test_manager_exists_and_regex_matches
test_package_rules

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
