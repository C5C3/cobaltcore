#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has the envoy-gateway customManagers + packageRules:
#   - a customManagers entry targeting deploy/kind/base/envoy-gateway.yaml
#     whose matchStrings regex extracts the pinned chart version lower bound
#   - a paired packageRule set that disables majors and automerges minor/patch
#     with minimumReleaseAge=3 days, groupName=envoy-gateway
#
# Prefers `renovate-config-validator` when available (via npx); otherwise
# performs a regex-only validation using Perl (which speaks the same PCRE
# syntax Renovate uses).
#
# Usage: bash tests/unit/renovate/envoy_gateway_manager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
CHART_FILE="$PROJECT_ROOT/deploy/kind/base/envoy-gateway.yaml"

# --- Test 1: renovate.json passes renovate-config-validator when available
#             ---
test_renovate_config_valid() {
  echo "Test: renovate.json validates via renovate-config-validator"

  if ! command -v npx >/dev/null 2>&1; then
    echo "  SKIP: npx not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local output status=0
  output="$(cd "$PROJECT_ROOT" && npx --yes --package renovate@latest -- \
    renovate-config-validator renovate.json 2>&1)" || status=$?

  if [ "$status" -ne 0 ]; then
    echo "  FAIL: renovate-config-validator rejected renovate.json"
    echo "$output" | head -40
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS: renovate-config-validator accepted renovate.json"
  PASS=$((PASS + 1))
}

# --- Test 2: the envoy-gateway customManager entry captures the chart
#             version lower bound from deploy/kind/base/envoy-gateway.yaml
#             ---
test_custom_manager_regex_captures_version() {
  echo "Test: customManagers regex extracts envoy-gateway chart version from fixture"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # Locate the envoy-gateway customManagers entry by its managerFilePatterns
  # so it doesn't collide with the flux-operator or flux-web entries.
  local entry
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("envoy-gateway"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting deploy/kind/base/envoy-gateway.yaml"
    FAIL=$((FAIL + 5))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is github-releases" \
    "github-releases" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"

  assert_eq "customManagers.versioningTemplate is semver" \
    "semver" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"

  assert_eq "customManagers.packageNameTemplate is envoyproxy/gateway" \
    "envoyproxy/gateway" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if [[ ! -f "$CHART_FILE" ]]; then
    echo "  SKIP: $CHART_FILE missing (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  # Extract the version line from the manifest (e.g., '      version: ">=1.3.0 <2.0.0"').
  local version_line
  version_line="$(grep -E '^[[:space:]]*version:[[:space:]]*"' "$CHART_FILE" | head -1)"
  assert_not_empty "chart version line present in deploy/kind/base/envoy-gateway.yaml" \
    "$version_line"

  local match_string
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local captured
  captured="$(REGEX="$match_string" LINE="$version_line" perl -e '
    my $re = $ENV{REGEX};
    my $line = $ENV{LINE};
    if ($line =~ /$re/) {
      print $+{currentValue} // "";
    }
  ')"

  # Compare against the parsed lower bound (the part after ">=" up to the
  # first whitespace) so changes to the upper bound do not break the test.
  local expected_value
  expected_value="$(printf '%s' "$version_line" \
    | sed -E 's/.*">=([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"

  assert_eq "matchStrings regex captures the chart lower-bound version" \
    "$expected_value" "$captured"
}

# --- Test 3: packageRules for envoy-gateway disable majors and automerge
#             minor/patch with minimumReleaseAge=3 days, groupName=envoy-gateway
#             ---
test_package_rules_disable_majors_and_group() {
  echo "Test: packageRules disable major envoy-gateway bumps, automerge minor/patch with 3-day cooldown"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # Select packageRules scoped to the envoy-gateway chart — via matchPackageNames
  # on envoyproxy/gateway OR matchFileNames on deploy/kind/base/envoy-gateway.yaml.
  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("envoyproxy/gateway")) != null
         or ((.matchFileNames   // []) | index("deploy/kind/base/envoy-gateway.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("envoyproxy/gateway")) != null
         or ((.matchFileNames   // []) | index("deploy/kind/base/envoy-gateway.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for envoyproxy/gateway"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major envoy-gateway updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for envoyproxy/gateway"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "minor/patch envoy-gateway updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch envoy-gateway updates carry matchUpdateTypes=patch" \
    "true" \
    "$(jq -r '(.matchUpdateTypes // []) | index("patch") != null' <<<"$minor_rule")"
  assert_eq "minor/patch envoy-gateway rule waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "minor/patch envoy-gateway rule groupName is envoy-gateway" \
    "envoy-gateway" \
    "$(jq -r '.groupName' <<<"$minor_rule")"
}

# --- Run ---
test_renovate_config_valid
test_custom_manager_regex_captures_version
test_package_rules_disable_majors_and_group

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
