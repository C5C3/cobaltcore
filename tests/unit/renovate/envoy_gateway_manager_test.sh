#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the Envoy Gateway CRD pin:
#   - a customManagers entry whose matchStrings regex extracts the
#     ENVOY_GATEWAY_VERSION pin from hack/deploy-infra.sh
#   - a paired custom.regex packageRule set, scoped to hack/deploy-infra.sh,
#     that disables majors and automerges minor/patch with
#     minimumReleaseAge=3 days, groupName=envoy-gateway
#   - the ENVOY_GATEWAY_VERSION pin inside the gateway-helm chart range of
#     deploy/kind/base/envoy-gateway.yaml
#
# The gateway-helm chart range in the kind base manifest belongs to
# Renovate's native flux manager and is pinned by
# tests/unit/renovate/flux_helmrelease_manager_test.sh. The shared groupName
# envoy-gateway puts a chart bump and the CRD pin in one PR. The chart comes
# from Docker Hub and the pin from GitHub releases, so a chart PR can open
# before the pin joins it, and a chart major opens alone because the pin's
# majors are disabled. Test 3 fails such a PR until the pin is inside the new
# range, so Flux never runs a controller ahead of the CRDs this script owns.
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

# NOTE renovate-config-validator schema check is run by the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh over the same
# renovate.json — see the header of this file.

# --- Test 1: the ENVOY_GATEWAY_VERSION customManager entry captures the CRD
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

# --- Test 2: packageRules for envoy-gateway disable majors and automerge
#             minor/patch with minimumReleaseAge=3 days, groupName=envoy-gateway
#             ---
test_package_rules_disable_majors_and_group() {
  echo "Test: packageRules disable major envoy-gateway bumps, automerge minor/patch with 3-day cooldown"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # Select the custom.regex packageRules for the envoyproxy/gateway CRD pin.
  # The flux rules for the gateway-helm chart name another package.
  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchManagers // []) | index("custom.regex")) != null)
        and (((.matchPackageNames // []) | index("envoyproxy/gateway")) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        (((.matchManagers // []) | index("custom.regex")) != null)
        and (((.matchPackageNames // []) | index("envoyproxy/gateway")) != null)
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

  # No customManager reads the kind manifest any more, so both rules are
  # scoped to the one file that carries the ENVOY_GATEWAY_VERSION pin.
  assert_eq "major rule is scoped to hack/deploy-infra.sh" \
    '["hack/deploy-infra.sh"]' \
    "$(jq -c '.matchFileNames' <<<"$major_rule")"
  assert_eq "minor/patch rule is scoped to hack/deploy-infra.sh" \
    '["hack/deploy-infra.sh"]' \
    "$(jq -c '.matchFileNames' <<<"$minor_rule")"
}

# semver_lt <a> <b> — exit 0 when the x.y.z version a sorts before b.
semver_lt() {
  local a1 a2 a3 b1 b2 b3
  IFS=. read -r a1 a2 a3 <<<"$1"
  IFS=. read -r b1 b2 b3 <<<"$2"
  if [ "$a1" -ne "$b1" ]; then [ "$a1" -lt "$b1" ]; return; fi
  if [ "$a2" -ne "$b2" ]; then [ "$a2" -lt "$b2" ]; return; fi
  [ "$a3" -lt "$b3" ]
}

# --- Test 3: the ENVOY_GATEWAY_VERSION CRD pin lies inside the gateway-helm
#             chart range, so no chart PR moves the controller past its CRDs
#             ---
test_crd_pin_inside_chart_range() {
  echo "Test: the ENVOY_GATEWAY_VERSION CRD pin lies inside the gateway-helm chart range"

  local xyz='[0-9]+\.[0-9]+\.[0-9]+' pin range floor ceiling
  pin="$(sed -nE "s/^ENVOY_GATEWAY_VERSION=.*:-v(${xyz})\}.*/\1/p" \
    "$PROJECT_ROOT/hack/deploy-infra.sh")"
  range="$(sed -nE "s/^ +version: \"(>=${xyz} <${xyz})\"\$/\1/p" \
    "$PROJECT_ROOT/deploy/kind/base/envoy-gateway.yaml")"
  floor="$(sed -E "s/^>=(${xyz}) .*/\1/" <<<"$range")"
  ceiling="$(sed -E "s/.* <(${xyz})\$/\1/" <<<"$range")"

  if [ -z "$pin" ] || [ -z "$range" ]; then
    echo "  FAIL: cannot read the vX.Y.Z pin from hack/deploy-infra.sh (\"$pin\") or the \">=X <Y\" chart range from deploy/kind/base/envoy-gateway.yaml (\"$range\")"
    FAIL=$((FAIL + 1))
    return
  fi

  if ! semver_lt "$pin" "$floor" && semver_lt "$pin" "$ceiling"; then
    echo "  PASS: ENVOY_GATEWAY_VERSION $pin satisfies the chart range \"$range\""
    PASS=$((PASS + 1))
  else
    echo "  FAIL: ENVOY_GATEWAY_VERSION $pin lies outside the chart range \"$range\"; move the pin in hack/deploy-infra.sh on the same branch as the chart"
    FAIL=$((FAIL + 1))
  fi
}

# --- Run ---
test_deploy_infra_pin_manager_captures_version
test_package_rules_disable_majors_and_group
test_crd_pin_inside_chart_range

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
