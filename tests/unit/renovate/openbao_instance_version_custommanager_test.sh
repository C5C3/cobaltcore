#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the OpenBaoCluster version pin in
# deploy/kind/infrastructure/openbao-instance.yaml and pairs it with the
# expected packageRules.
#
# The pin rides no Flux chart: the openbao-operator resolves it into the
# instance image itself, so no native manager sees it. Left untracked it goes
# stale silently, and the proving instance stops following the OpenBao line the
# Barbican onboarding targets.
#
# The same package is pinned a second time in the c5c3 controller, under its
# own manager and covered by tests/unit/renovate/
# c5c3_openbao_version_custommanager_test.sh, so every selector here is scoped
# to the instance manifest.
#
# Asserts:
#   - exactly one customManager targets openbao/openbao over the instance
#     manifest with the github-releases datasource and the extractVersionTemplate
#     that strips the v prefix of the upstream release tags
#   - its matchStrings regex captures the current pin from the real manifest
#     exactly once — the manifest carries five apiVersion lines and a kv engine
#     `version: "2"` the regex must not reach — and that capture equals the
#     literal version of the OpenBaoCluster spec
#   - a paired packageRule disables majors
#   - a paired packageRule proposes minor/patch after a 3-day cooldown but never
#     automerges — the bump has to stay inside the openbao-operator's supported
#     version window, which only a human can confirm
#
# Usage: bash tests/unit/renovate/openbao_instance_version_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
INSTANCE_FILE="$PROJECT_ROOT/deploy/kind/infrastructure/openbao-instance.yaml"

PKG_NAME="openbao/openbao"
INSTANCE_PATH="deploy/kind/infrastructure/openbao-instance.yaml"

test_manager_exists_and_regex_matches() {
  echo "Test: the OpenBaoCluster version pin has a customManager whose regex matches"

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
  if [[ ! -f "$INSTANCE_FILE" ]]; then
    echo "  FAIL: $INSTANCE_FILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local count
  count="$(jq --arg pkg "$PKG_NAME" '[.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("openbao-instance")))] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets ${PKG_NAME} over the instance manifest" \
    "1" "$count"

  local entry
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("openbao-instance")))' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_contains "manager targets the OpenBao instance manifest" \
    "$(jq -r '.managerFilePatterns[0]' <<<"$entry")" "openbao-instance"
  assert_eq "manager uses the github-releases datasource" \
    "github-releases" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "manager strips the v prefix of the upstream release tags" \
    '^v(?<version>.+)$' "$(jq -r '.extractVersionTemplate' <<<"$entry")"

  local match_string captures captured match_count expected_pin
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captures="$(REGEX="$match_string" FILE="$INSTANCE_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    while ($content =~ /$re/g) {
      print "$+{currentValue}\n" if defined $+{currentValue};
    }
  ')"
  captured="$(printf '%s\n' "$captures" | head -1)"
  match_count="$(printf '%s\n' "$captures" | grep -c . || true)"

  assert_not_empty "matchStrings regex captures the instance version pin" "$captured"
  # A regex loose enough to reach an apiVersion line or the kv engine's
  # version: "2" would hand Renovate a second, bogus dep to bump.
  assert_eq "matchStrings regex captures exactly one value in the manifest" \
    "1" "$match_count"

  # Derive the expected pin straight from the manifest so a Renovate bump keeps
  # this test green — never hard-code the version.
  expected_pin="$(grep -oE '^  version: "[0-9]+\.[0-9]+\.[0-9]+"' "$INSTANCE_FILE" \
    | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  assert_eq "captured pin equals the literal OpenBaoCluster spec.version" \
    "$expected_pin" "$captured"
}

test_package_rules() {
  echo "Test: paired packageRules gate OpenBao instance major vs minor/patch updates"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c --arg path "$INSTANCE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c --arg path "$INSTANCE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for $INSTANCE_PATH"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major OpenBao instance updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor/patch OpenBao instance updates"
    FAIL=$((FAIL + 2))
    return
  fi

  assert_eq "minor/patch instance updates are not automerged (operator support window)" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch instance rule waits minimumReleaseAge=3 days" \
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
