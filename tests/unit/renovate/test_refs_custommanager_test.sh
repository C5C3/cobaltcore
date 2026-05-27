#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has the test-refs.yaml (PyPI) customManager described
# by CC-0051 REQ-001:
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
  echo "Test: test-refs.yaml customManager uses datasource=pypi, versioning=pep440 (CC-0051)"

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
  echo "Test: test-refs.yaml regex captures tempest and keystone-tempest-plugin (CC-0051)"

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
  echo "Test: packageRules disable major test-refs bumps, automerge minor/patch (CC-0051)"

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

test_custom_manager_uses_pypi_datasource
test_regex_captures_tempest_and_plugin
test_package_rules_for_test_refs

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
