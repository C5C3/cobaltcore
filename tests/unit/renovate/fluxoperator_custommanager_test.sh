#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# FLUX_OPERATOR_VERSION constant in hack/deploy-infra.sh, plus the paired
# packageRules that mirror the OpenStack tags rule (CC-0085, REQ-007):
#   - renovate.json validates via `renovate-config-validator`
#   - the matchStrings regex captures the current FLUX_OPERATOR_VERSION value
#   - packageRules disable major bumps and automerge minor/patch with a
#     3-day minimumReleaseAge
# Usage: bash tests/unit/renovate/fluxoperator_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
DEPLOY_INFRA_FILE="$PROJECT_ROOT/hack/deploy-infra.sh"

# --- Test 1: renovate.json passes renovate-config-validator (CC-0085, REQ-007) ---
test_renovate_config_valid() {
  echo "Test: renovate.json validates via renovate-config-validator (CC-0085)"

  if ! command -v npx >/dev/null 2>&1; then
    echo "  SKIP: npx not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local output status=0
  output="$(cd "$PROJECT_ROOT" && npx --yes --package renovate -- \
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

# --- Test 2: customManager targets hack/deploy-infra.sh and captures the
#             current FLUX_OPERATOR_VERSION value (CC-0085, REQ-007) ---
test_custom_manager_regex_captures_version() {
  echo "Test: customManagers regex matches FLUX_OPERATOR_VERSION in hack/deploy-infra.sh (CC-0085)"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # Locate the flux-operator customManagers entry — identified by the
  # hack/deploy-infra.sh managerFilePatterns so the assertions don't
  # collide with the deploy/kind/base/flux-web.yaml entry (CC-0086) that
  # shares the controlplaneio-fluxcd/flux-operator packageNameTemplate.
  local entry
  entry="$(jq -c '.customManagers[]
    | select(.packageNameTemplate == "controlplaneio-fluxcd/flux-operator")
    | select((.managerFilePatterns // []) | join(",") | contains("hack/deploy-infra"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry with packageNameTemplate=controlplaneio-fluxcd/flux-operator"
    FAIL=$((FAIL + 5))
    return
  fi

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

  # Extract the FLUX_OPERATOR_VERSION line verbatim, strip the quotes, and
  # confirm the matchStrings regex captures the same value via Perl (which
  # speaks the same PCRE-style named-group syntax Renovate uses).
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local expected_value
  expected_value="$(grep -E '^FLUX_OPERATOR_VERSION=' "$DEPLOY_INFRA_FILE" \
    | head -1 | sed -E 's/^FLUX_OPERATOR_VERSION="([^"]+)".*/\1/')"
  assert_not_empty "FLUX_OPERATOR_VERSION line present in hack/deploy-infra.sh" \
    "$expected_value"

  local match_string
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local line
  line="$(grep -E '^FLUX_OPERATOR_VERSION=' "$DEPLOY_INFRA_FILE" | head -1)"

  local captured
  captured="$(REGEX="$match_string" LINE="$line" perl -e '
    my $re = $ENV{REGEX};
    my $line = $ENV{LINE};
    if ($line =~ /$re/) {
      print $+{currentValue} // "";
    }
  ')"

  assert_eq "matchStrings regex captures the FLUX_OPERATOR_VERSION value" \
    "$expected_value" "$captured"
}

# --- Test 3: packageRules mirror the OpenStack pattern — disable major,
#             automerge minor/patch with minimumReleaseAge=3 days
#             (CC-0085, REQ-007) ---
test_package_rules_mirror_openstack_pattern() {
  echo "Test: packageRules disable major bumps and automerge minor/patch (CC-0085)"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  # A packageRule matches this custom manager if it scopes via matchPackageNames
  # or matchDepNames containing the flux-operator, or via matchFileNames on
  # hack/deploy-infra.sh. Select rules that reference the flux-operator
  # explicitly so we don't accidentally pick up the OpenStack rule.
  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("controlplaneio-fluxcd/flux-operator")) != null
         or ((.matchDepNames    // []) | index("controlplaneio-fluxcd/flux-operator")) != null
         or ((.matchFileNames   // []) | index("hack/deploy-infra.sh")) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("controlplaneio-fluxcd/flux-operator")) != null
         or ((.matchDepNames    // []) | index("controlplaneio-fluxcd/flux-operator")) != null
         or ((.matchFileNames   // []) | index("hack/deploy-infra.sh")) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for flux-operator"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major flux-operator updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for flux-operator"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "minor/patch flux-operator updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch flux-operator updates carry matchUpdateTypes=patch" \
    "true" \
    "$(jq -r '(.matchUpdateTypes // []) | index("patch") != null' <<<"$minor_rule")"
  assert_eq "minor/patch flux-operator rule waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
}

# --- Run ---
test_renovate_config_valid
test_custom_manager_regex_captures_version
test_package_rules_mirror_openstack_pattern

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
