#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# GATEWAY_API_VERSION pin in hack/deploy-infra.sh, plus the paired packageRule.
#   - exactly one customManager tracks kubernetes-sigs/gateway-api over
#     hack/deploy-infra.sh via the github-releases datasource
#   - the matchStrings regex captures the current GATEWAY_API_VERSION value out
#     of the `${GATEWAY_API_VERSION:-vX.Y.Z}` default, and that capture equals
#     the literal pin in the script
#   - packageRules disable major bumps: the CRD bundle must stay in lockstep
#     with the sigs.k8s.io/gateway-api module the operators compile against, so
#     a major belongs in the same change as the go.mod bump, not on its own
#
# Renovate's own schema validation of renovate.json is covered by the sibling
# fluxoperator_custommanager_test.sh, so it is not repeated here.
#
# Usage: bash tests/unit/renovate/gateway_api_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
DEPLOY_INFRA_FILE="$PROJECT_ROOT/hack/deploy-infra.sh"

PKG_NAME="kubernetes-sigs/gateway-api"

# --- Test 1: the customManager exists and targets the right file/datasource ---
test_custom_manager_targets_deploy_infra() {
  echo "Test: a customManager tracks ${PKG_NAME} over hack/deploy-infra.sh"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
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
    echo "  FAIL: no customManagers entry with packageNameTemplate=${PKG_NAME}"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "customManagers.depNameTemplate is ${PKG_NAME}" \
    "$PKG_NAME" \
    "$(jq -r '.depNameTemplate' <<<"$entry")"

  assert_eq "customManagers.datasourceTemplate is github-releases" \
    "github-releases" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"

  assert_eq "customManagers.versioningTemplate is semver" \
    "semver" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"

  # managerFilePatterns must target hack/deploy-infra.sh (regex form uses
  # leading/trailing slashes per Renovate's convention).
  local patterns
  patterns="$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")"
  assert_contains "managerFilePatterns targets hack/deploy-infra.sh" \
    "$patterns" "deploy-infra"
}

# --- Test 2: the matchStrings regex captures the live pin ---
# The pin is written as a parameter-expansion default
# (GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-vX.Y.Z}"), so the regex has to
# escape `$`, `{` and `}` — a plain FLUX_OPERATOR_VERSION-style pattern would
# silently match nothing and the pin would stop being bumped.
test_regex_captures_pinned_version() {
  echo "Test: customManagers regex matches GATEWAY_API_VERSION in hack/deploy-infra.sh"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi
  if [[ ! -f "$DEPLOY_INFRA_FILE" ]]; then
    echo "  FAIL: $DEPLOY_INFRA_FILE missing"
    FAIL=$((FAIL + 3))
    return
  fi

  # Derive the expected pin straight from the script so a Renovate bump keeps
  # this test green — never hard-code the version.
  local expected_value
  expected_value="$(grep -oE 'GATEWAY_API_VERSION:-v[0-9]+\.[0-9]+\.[0-9]+' \
    "$DEPLOY_INFRA_FILE" | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  assert_not_empty "GATEWAY_API_VERSION pin present in hack/deploy-infra.sh" \
    "$expected_value"

  local entry match_string
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.packageNameTemplate == $pkg)' "$RENOVATE_FILE" | head -1)"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local line
  line="$(grep -E '^GATEWAY_API_VERSION=' "$DEPLOY_INFRA_FILE" | head -1)"

  local captured
  captured="$(REGEX="$match_string" LINE="$line" perl -e '
    my $re = $ENV{REGEX};
    my $line = $ENV{LINE};
    if ($line =~ /$re/) {
      print $+{currentValue} // "";
    }
  ')"

  assert_not_empty "matchStrings regex matches the GATEWAY_API_VERSION line" \
    "$captured"
  assert_eq "matchStrings regex captures the GATEWAY_API_VERSION value" \
    "$expected_value" "$captured"
}

# --- Test 3: packageRules disable majors for the CRD bundle ---
test_package_rules_disable_majors() {
  echo "Test: packageRules disable major Gateway API bumps"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local major_rule
  major_rule="$(jq -c --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("hack/deploy-infra.sh")) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "major ${PKG_NAME} updates are disabled" \
    "false" \
    "$(jq -r '.enabled' <<<"$major_rule")"
}

# --- Run ---
test_custom_manager_targets_deploy_infra
test_regex_captures_pinned_version
test_package_rules_disable_majors

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
