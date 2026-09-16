#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has the test-refs.yaml (PyPI) customManager:
#   - a customManagers entry targeting releases/<release>/test-refs.yaml
#     whose matchStrings regex extracts (depName, x.y.z) per PyPI pin
#   - datasource=pypi, versioning=pep440 (distinct from source-refs.yaml)
#   - a paired packageRule set that disables majors and automerges minor/patch
#     with minimumReleaseAge=3 days
#
# Usage: bash tests/unit/renovate/test_refs_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
TEST_REFS_FILE="$PROJECT_ROOT/releases/2026.1/test-refs.yaml"

test_custom_manager_uses_pypi_datasource() {
  echo "Test: test-refs.yaml customManager uses datasource=pypi, versioning=pep440"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("test-refs"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting releases/*/test-refs.yaml"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "test-refs customManager.datasourceTemplate is pypi" \
    "pypi" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "test-refs customManager.versioningTemplate is pep440" \
    "pep440" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"
  assert_eq "test-refs customManager has a depName capture (no fixed depNameTemplate)" \
    "null" \
    "$(jq -r '.depNameTemplate // "null"' <<<"$entry")"
}

test_regex_captures_tempest_and_plugin() {
  echo "Test: test-refs.yaml regex captures tempest and keystone-tempest-plugin"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if [[ ! -f "$TEST_REFS_FILE" ]]; then
    echo "  SKIP: $TEST_REFS_FILE missing (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local entry match_string
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("test-refs"))' \
    "$RENOVATE_FILE")"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local captured
  captured="$(REGEX="$match_string" FILE="$TEST_REFS_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    my @hits;
    while ($content =~ /$re/gm) { push @hits, $+{depName} . "=" . $+{currentValue}; }
    print join("\n", @hits);
  ')"

  assert_contains "regex captures tempest from test-refs.yaml" \
    "$captured" "tempest="
  assert_contains "regex captures keystone-tempest-plugin from test-refs.yaml" \
    "$captured" "keystone-tempest-plugin="
}

test_package_rules_for_test_refs() {
  echo "Test: packageRules disable major test-refs bumps, automerge minor/patch"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("releases/**/test-refs.yaml")) != null
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("releases/**/test-refs.yaml")) != null
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for releases/**/test-refs.yaml"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major test-refs updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for test-refs.yaml"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "minor/patch test-refs updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch test-refs rule waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_contains "minor/patch test-refs rule groupName mentions PyPI" \
    "$(jq -r '.groupName' <<<"$minor_rule")" \
    "PyPI"
}

# The 2025.2 upper-constraints.txt pins testtools===2.7.2, and
# neutron-tempest-plugin 3.1.0 and later require testtools>=2.8.4, so the
# 2025.2 tempest image cannot resolve them. Without a hold Renovate keeps
# proposing the newest plugin for 2025.2 and the build-tempest leg fails.
test_neutron_tempest_plugin_hold_for_2025_2() {
  echo "Test: neutron-tempest-plugin stays below 3.1.0 for 2025.2"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local hold_rule pin
  hold_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("releases/2025.2/test-refs.yaml")) != null
        and (((.matchPackageNames // []) | index("neutron-tempest-plugin")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$hold_rule" ]; then
    echo "  FAIL: no packageRule holds neutron-tempest-plugin for releases/2025.2/test-refs.yaml"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "the 2025.2 neutron-tempest-plugin rule allows only versions below 3.1.0" \
    "<3.1.0" \
    "$(jq -r '.allowedVersions' <<<"$hold_rule")"
  assert_eq "the hold does not disable the pin" \
    "null" \
    "$(jq -r '.enabled // "null"' <<<"$hold_rule")"

  pin="$(awk -F'"' '/^neutron-tempest-plugin:/ {print $2; exit}' \
    "$PROJECT_ROOT/releases/2025.2/test-refs.yaml")"
  if [[ "$pin" =~ ^3\.0\.[0-9]+$ ]]; then
    echo "  PASS: releases/2025.2/test-refs.yaml pins neutron-tempest-plugin on the 3.0 line ($pin)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: releases/2025.2/test-refs.yaml pins neutron-tempest-plugin '$pin', outside the 3.0 line the hold allows"
    FAIL=$((FAIL + 1))
  fi
}

test_custom_manager_uses_pypi_datasource
test_regex_captures_tempest_and_plugin
test_package_rules_for_test_refs
test_neutron_tempest_plugin_hold_for_2025_2

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
