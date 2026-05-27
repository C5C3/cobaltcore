#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# chart `version` input inside deploy/kind/base/flux-web.yaml, plus the
# paired packageRules that mirror the hack/deploy-infra.sh rule
# (CC-0086, REQ-004):
#   - the matchStrings regex captures the chart pin currently declared in
#     deploy/kind/base/flux-web.yaml
#   - packageRules disable major bumps and automerge minor/patch with a
#     3-day minimumReleaseAge
#
# Schema validation of renovate.json via `renovate-config-validator`
# (which transitively pulls Renovate via npx) is intentionally NOT run
# from this per-feature test to keep local / CI loops fast: the
# validator fetches the Renovate package on every invocation and taking
# that hit once per feature touching renovate.json multiplies the cost
# linearly. The validation is centralised in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh (CC-0085),
# which runs against the same renovate.json file — a schema regression
# in the flux-web customManager entry fails that test too, so coverage
# is preserved without duplicating the npx download.
#
# Usage: bash tests/unit/renovate/flux_web_chart_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
FLUX_WEB_FILE="$PROJECT_ROOT/deploy/kind/base/flux-web.yaml"

# NOTE (CC-0086): renovate-config-validator schema check is run by the
# sibling tests/unit/renovate/fluxoperator_custommanager_test.sh (CC-0085)
# — see the header of this file for the rationale behind not duplicating
# the npx-driven validation here.

# --- Test 1: customManager targets deploy/kind/base/flux-web.yaml and
#             captures the chart pin currently declared in that file
#             (CC-0086, REQ-004) ---
test_custom_manager_regex_captures_chart_pin() {
  echo "Test: customManagers regex matches chart version input in deploy/kind/base/flux-web.yaml (CC-0086)"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # Locate the flux-web customManagers entry — identified by the
  # deploy/kind/base/flux-web.yaml managerFilePatterns so the assertions
  # don't collide with the existing hack/deploy-infra.sh entry that also
  # targets controlplaneio-fluxcd/flux-operator.
  local entry
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("deploy/kind/base/flux-web"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry with managerFilePatterns targeting deploy/kind/base/flux-web.yaml"
    FAIL=$((FAIL + 5))
    return
  fi

  assert_eq "customManagers.packageNameTemplate is controlplaneio-fluxcd/flux-operator" \
    "controlplaneio-fluxcd/flux-operator" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")"

  assert_eq "customManagers.datasourceTemplate is github-releases" \
    "github-releases" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"

  assert_eq "customManagers.versioningTemplate is semver" \
    "semver" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"

  # Extract the chart `version` input from flux-web.yaml verbatim, then
  # confirm the matchStrings regex captures the same value via Perl (which
  # speaks the same PCRE-style named-group syntax Renovate uses).
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local expected_value
  expected_value="$(grep -E '^[[:space:]]+- version:' "$FLUX_WEB_FILE" \
    | head -1 | sed -E 's/^[[:space:]]+- version:[[:space:]]*"([^"]+)".*/\1/')"
  assert_not_empty "chart version input present in deploy/kind/base/flux-web.yaml" \
    "$expected_value"

  local match_string
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local line
  line="$(grep -E '^[[:space:]]+- version:' "$FLUX_WEB_FILE" | head -1)"

  local captured
  captured="$(REGEX="$match_string" LINE="$line" perl -e '
    my $re = $ENV{REGEX};
    my $line = $ENV{LINE};
    if ($line =~ /$re/) {
      print $+{currentValue} // "";
    }
  ')"

  assert_eq "matchStrings regex captures the chart version input value" \
    "$expected_value" "$captured"
}

# --- Test 2: packageRules cover the new file with the same flux-operator
#             rules — disable major, automerge minor/patch with
#             minimumReleaseAge=3 days (CC-0086, REQ-004) ---
test_package_rules_cover_flux_web() {
  echo "Test: packageRules disable major bumps and automerge minor/patch for deploy/kind/base/flux-web.yaml (CC-0086)"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # A packageRule covers the new file if it scopes flux-operator via
  # matchPackageNames AND lists deploy/kind/base/flux-web.yaml in
  # matchFileNames. Select by file so the assertions don't accidentally
  # match rules scoped only to hack/deploy-infra.sh.
  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("controlplaneio-fluxcd/flux-operator")) != null)
        and (((.matchFileNames   // []) | index("deploy/kind/base/flux-web.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchPackageNames // []) | index("controlplaneio-fluxcd/flux-operator")) != null)
        and (((.matchFileNames   // []) | index("deploy/kind/base/flux-web.yaml")) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major flux-operator updates to deploy/kind/base/flux-web.yaml"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major flux-operator updates are disabled for flux-web.yaml" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch flux-operator updates to deploy/kind/base/flux-web.yaml"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "minor/patch flux-operator updates are automerged for flux-web.yaml" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch flux-operator rule for flux-web.yaml carries matchUpdateTypes=patch" \
    "true" \
    "$(jq -r '(.matchUpdateTypes // []) | index("patch") != null' <<<"$minor_rule")"
  assert_eq "minor/patch flux-operator rule for flux-web.yaml waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
}

# --- Run ---
test_custom_manager_regex_captures_chart_pin
test_package_rules_cover_flux_web

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
