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
# The matchStrings regex is replayed with Perl, which speaks the same
# PCRE-style syntax Renovate uses.
#
# Schema validation of renovate.json via `renovate-config-validator`
# (which transitively pulls Renovate via npx) is intentionally NOT run
# from this per-feature test to keep local / CI loops fast: the validator
# fetches the Renovate package on every invocation and taking that hit
# once per feature touching renovate.json multiplies the cost linearly.
# The validation is centralised in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh, which runs
# against the same renovate.json file.
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

# NOTE renovate-config-validator schema check is run by the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh over the same
# renovate.json — see the header of this file.

# --- Test 1: the envoy-gateway customManager entry captures the chart
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

# --- Test 2: the ENVOY_GATEWAY_VERSION customManager entry captures the CRD
#             asset pin from hack/deploy-infra.sh
#             ---
test_deploy_infra_pin_manager_captures_version() {
  echo "Test: customManagers regex extracts the ENVOY_GATEWAY_VERSION pin from hack/deploy-infra.sh"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select((.matchStrings // []) | join(",") | contains("ENVOY_GATEWAY_VERSION"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry capturing ENVOY_GATEWAY_VERSION"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "pin manager datasourceTemplate is github-releases" \
    "github-releases" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"

  assert_eq "pin manager packageNameTemplate is envoyproxy/gateway" \
    "envoyproxy/gateway" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local pin_line
  pin_line="$(grep -E '^ENVOY_GATEWAY_VERSION=' "$PROJECT_ROOT/hack/deploy-infra.sh" | head -1)"
  assert_not_empty "ENVOY_GATEWAY_VERSION pin present in hack/deploy-infra.sh" \
    "$pin_line"

  local match_string
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local captured
  captured="$(REGEX="$match_string" LINE="$pin_line" perl -e '
    my $re = $ENV{REGEX};
    my $line = $ENV{LINE};
    if ($line =~ /$re/) {
      print $+{currentValue} // "";
    }
  ')"

  local expected_value
  expected_value="$(printf '%s' "$pin_line" \
    | sed -E 's/.*:-(v[0-9]+\.[0-9]+\.[0-9]+).*/\1/')"

  assert_eq "matchStrings regex captures the pinned CRD asset version" \
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

  # The ENVOY_GATEWAY_VERSION CRD asset pin in hack/deploy-infra.sh must ride
  # the same rules, so a chart bump and the CRD pin land in one grouped PR.
  assert_eq "major rule also covers the hack/deploy-infra.sh pin" \
    "true" \
    "$(jq -r '(.matchFileNames // []) | index("hack/deploy-infra.sh") != null' <<<"$major_rule")"
  assert_eq "minor/patch rule also covers the hack/deploy-infra.sh pin" \
    "true" \
    "$(jq -r '(.matchFileNames // []) | index("hack/deploy-infra.sh") != null' <<<"$minor_rule")"
}

# --- Run ---
test_custom_manager_regex_captures_version
test_deploy_infra_pin_manager_captures_version
test_package_rules_disable_majors_and_group

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
