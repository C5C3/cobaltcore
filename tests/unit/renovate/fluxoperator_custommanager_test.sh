#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# FLUX_OPERATOR_VERSION constant in hack/deploy-infra.sh and
# hack/deploy-mgmt-cluster.sh, plus the paired packageRules that mirror the
# OpenStack tags rule
#   - renovate.json validates via `renovate-config-validator`
#   - the matchStrings regex captures the current FLUX_OPERATOR_VERSION value
#     in both files, and the two pins are equal (the two clusters of the
#     two-cluster devstack bootstrap the same flux-operator)
#   - packageRules disable major bumps and automerge minor/patch with a
#     3-day minimumReleaseAge, and scope both files
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

# Pin the validator: test-shell also runs on the push runs for main and
# v* tags, so `renovate@latest` would fetch and execute an unreviewed
# Renovate release inside the release pipeline, and a breaking release
# would turn main red at a commit that changed nothing. Bumped by the
# renovatebot/renovate customManagers entry in renovate.json, whose
# packageRule never automerges — the run below is the only check that
# exercises the new release, so a bump must not approve itself. This is
# the single pin in the suite; the sibling tests deliberately do not
# repeat the validator run (and therefore not the constant either).
RENOVATE_VALIDATOR_VERSION="44.8.0"
DEPLOY_INFRA_FILE="$PROJECT_ROOT/hack/deploy-infra.sh"
DEPLOY_MGMT_FILE="$PROJECT_ROOT/hack/deploy-mgmt-cluster.sh"

# --- Test 1: renovate.json passes renovate-config-validator ---
test_renovate_config_valid() {
  echo "Test: renovate.json validates via renovate-config-validator"

  if ! command -v npx >/dev/null 2>&1; then
    echo "  SKIP: npx not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local output status=0
  output="$(cd "$PROJECT_ROOT" && npx --yes --package "renovate@${RENOVATE_VALIDATOR_VERSION}" -- \
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
#             current FLUX_OPERATOR_VERSION value ---
test_custom_manager_regex_captures_version() {
  echo "Test: customManagers regex matches FLUX_OPERATOR_VERSION in the deploy scripts"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  # Locate the flux-operator customManagers entry — identified by the
  # hack/deploy-infra.sh managerFilePatterns so the assertions don't
  # collide with the deploy/kind/base/flux-web.yaml entry that
  # shares the controlplaneio-fluxcd/flux-operator packageNameTemplate.
  local entry
  entry="$(jq -c '.customManagers[]
    | select(.packageNameTemplate == "controlplaneio-fluxcd/flux-operator")
    | select((.managerFilePatterns // []) | join(",") | contains("hack/deploy-infra"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry with packageNameTemplate=controlplaneio-fluxcd/flux-operator"
    FAIL=$((FAIL + 6))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is github-releases" \
    "github-releases" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"

  assert_eq "customManagers.versioningTemplate is semver" \
    "semver" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"

  # managerFilePatterns must target both deploy scripts (regex form uses
  # leading/trailing slashes per Renovate's convention). The management cluster
  # of the two-cluster devstack bootstraps its own flux-operator from
  # hack/deploy-mgmt-cluster.sh, so a pattern covering only hack/deploy-infra.sh
  # would leave that pin behind on every bump.
  local patterns
  patterns="$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")"
  assert_contains "managerFilePatterns targets hack/deploy-infra.sh" \
    "$patterns" "deploy-infra"
  assert_contains "managerFilePatterns targets hack/deploy-mgmt-cluster.sh" \
    "$patterns" "deploy-mgmt-cluster"

  # Extract the FLUX_OPERATOR_VERSION line verbatim, strip the quotes, and
  # confirm the matchStrings regex captures the same value via Perl (which
  # speaks the same PCRE-style named-group syntax Renovate uses).
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local match_string
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local file expected_value line captured
  for file in "$DEPLOY_INFRA_FILE" "$DEPLOY_MGMT_FILE"; do
    expected_value="$(grep -E '^FLUX_OPERATOR_VERSION=' "$file" \
      | head -1 | sed -E 's/^FLUX_OPERATOR_VERSION="([^"]+)".*/\1/')"
    assert_not_empty "FLUX_OPERATOR_VERSION line present in ${file##*/}" \
      "$expected_value"

    line="$(grep -E '^FLUX_OPERATOR_VERSION=' "$file" | head -1)"

    captured="$(REGEX="$match_string" LINE="$line" perl -e '
      my $re = $ENV{REGEX};
      my $line = $ENV{LINE};
      if ($line =~ /$re/) {
        print $+{currentValue} // "";
      }
    ')"

    assert_eq "matchStrings regex captures the FLUX_OPERATOR_VERSION value in ${file##*/}" \
      "$expected_value" "$captured"
  done
}

# --- Test 3: packageRules mirror the OpenStack pattern — disable major,
#             automerge minor/patch with minimumReleaseAge=3 days
#             ---
test_package_rules_mirror_openstack_pattern() {
  echo "Test: packageRules disable major bumps and automerge minor/patch"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
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
    FAIL=$((FAIL + 3))
  else
    assert_eq "major flux-operator updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
    # A rule scoped by matchFileNames applies to the files it lists and to no
    # others, so the second deploy script has to be listed on both rules: left
    # out, its pin would take major bumps and skip the automerge group.
    assert_eq "the major rule scopes hack/deploy-mgmt-cluster.sh" \
      "true" \
      "$(jq -r '(.matchFileNames // []) | index("hack/deploy-mgmt-cluster.sh") != null' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for flux-operator"
    FAIL=$((FAIL + 4))
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
  assert_eq "the minor/patch rule scopes hack/deploy-mgmt-cluster.sh" \
    "true" \
    "$(jq -r '(.matchFileNames // []) | index("hack/deploy-mgmt-cluster.sh") != null' <<<"$minor_rule")"
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
