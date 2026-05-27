#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has a customManager for the headlamp Helm chart
# version range in deploy/kind/base/headlamp.yaml. The manager must:
#   - target the file, not the broader kind/base/ dir
#   - use datasource=helm with the kubernetes-sigs/headlamp registry URL
#   - capture the lower bound from `version: ">=x.y.z <X.Y.Z"`
#   - be paired with a packageRule that disables majors and automerges
#     minor/patch with minimumReleaseAge=3 days
#
# Usage: bash tests/unit/renovate/headlamp_chart_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
CHART_FILE="$PROJECT_ROOT/deploy/kind/base/headlamp.yaml"

test_custom_manager_uses_helm_datasource() {
  echo "Test: headlamp customManager uses datasource=helm with the kubernetes-sigs registry"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("headlamp"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting deploy/kind/base/headlamp.yaml"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "headlamp customManager.datasourceTemplate is helm" \
    "helm" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "headlamp customManager.depNameTemplate is headlamp" \
    "headlamp" \
    "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_contains "headlamp customManager.registryUrlTemplate points at kubernetes-sigs.github.io" \
    "$(jq -r '.registryUrlTemplate' <<<"$entry")" \
    "kubernetes-sigs.github.io/headlamp"
}

test_regex_captures_chart_lower_bound() {
  echo "Test: headlamp customManager regex captures the chart-range lower bound"

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
  if [[ ! -f "$CHART_FILE" ]]; then
    echo "  SKIP: $CHART_FILE missing"
    SKIP=$((SKIP + 1))
    return
  fi

  local entry match_string version_line captured expected_value
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("headlamp"))' \
    "$RENOVATE_FILE")"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  version_line="$(grep -E '^[[:space:]]*version:[[:space:]]*"' "$CHART_FILE" | head -1)"
  assert_not_empty "chart version line present in deploy/kind/base/headlamp.yaml" \
    "$version_line"

  captured="$(REGEX="$match_string" LINE="$version_line" perl -e '
    my $re = $ENV{REGEX};
    my $line = $ENV{LINE};
    if ($line =~ /$re/) { print $+{currentValue} // ""; }
  ')"

  expected_value="$(printf '%s' "$version_line" \
    | sed -E 's/.*">=([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"

  assert_eq "matchStrings regex captures the headlamp chart lower-bound version" \
    "$expected_value" "$captured"
}

test_package_rules_for_headlamp() {
  echo "Test: packageRules disable major headlamp bumps, automerge minor/patch"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("headlamp")) != null
         or ((.matchFileNames   // []) | index("deploy/kind/base/headlamp.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("headlamp")) != null
         or ((.matchFileNames   // []) | index("deploy/kind/base/headlamp.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for headlamp"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major headlamp updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for headlamp"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "minor/patch headlamp updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch headlamp rule waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "minor/patch headlamp rule groupName is headlamp" \
    "headlamp" \
    "$(jq -r '.groupName' <<<"$minor_rule")"
}

test_custom_manager_uses_helm_datasource
test_regex_captures_chart_lower_bound
test_package_rules_for_headlamp

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
