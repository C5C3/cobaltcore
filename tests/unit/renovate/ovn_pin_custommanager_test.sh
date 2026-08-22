#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the single ARG OVN_VERSION pin in
# images/ovn/Dockerfile and holds it on the 26.03 LTS line.
#
# Asserts:
#   - exactly one customManager targets ovn-org/ovn over
#     images/ovn/Dockerfile with the github-tags datasource, because
#     ovn-org/ovn publishes tags but no GitHub releases
#   - its versioningTemplate is a regex versioning: 26.03 carries a
#     leading zero strict semver rejects, and the pinned value keeps the
#     'v' prefix of the upstream tag
#   - its matchStrings regex captures the current pin from the real
#     Dockerfile and that capture equals the tag
#     hack/ci-resolve-ovn-version.sh resolves
#   - a paired packageRule disables both major and minor bumps: 26.09 is
#     a minor bump and 27.03 a major one, and both leave the LTS line
#   - a paired packageRule waits a 3-day cooldown on patches but does
#     NOT automerge them: a tag bump moves the ARG OVN_COMMIT and
#     ARG OVS_COMMIT content pins next to it, and no datasource can
#     express "the commit / the ovs gitlink at tag X", so Renovate
#     leaves both stale and the build aborts. A reviewer has to carry
#     them across, which an automerged PR would give nobody an owner for
#   - no releases/*/source-refs.yaml carries an 'ovn' key: the image is
#     release-independent and must not be bound to an OpenStack release
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
# Usage: bash tests/unit/renovate/ovn_pin_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
OVN_DOCKERFILE="$PROJECT_ROOT/images/ovn/Dockerfile"
RESOLVER="$PROJECT_ROOT/hack/ci-resolve-ovn-version.sh"

PKG_NAME="ovn-org/ovn"
DOCKERFILE_PATH="images/ovn/Dockerfile"

test_manager_exists_and_regex_matches() {
  echo "Test: the ARG OVN_VERSION pin has a customManager whose regex matches"

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
  if [[ ! -f "$OVN_DOCKERFILE" ]]; then
    echo "  FAIL: $OVN_DOCKERFILE missing"
    FAIL=$((FAIL + 1))
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
    echo "  FAIL: no customManager for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_contains "manager targets ${DOCKERFILE_PATH}" \
    "$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")" "$DOCKERFILE_PATH"
  assert_eq "manager uses the github-tags datasource (ovn-org/ovn cuts no releases)" \
    "github-tags" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_starts_with "manager uses a regex versioning (26.03 is not strict semver)" \
    "$(jq -r '.versioningTemplate' <<<"$entry")" "regex:"

  local match_string captured expected_pin
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captured="$(REGEX="$match_string" FILE="$OVN_DOCKERFILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    if ($content =~ /$re/) { print $+{currentValue} // ""; }
  ')"

  assert_not_empty "matchStrings regex captures the OVN pin" "$captured"

  # Derive the expected tag from the resolver the build lane uses so a
  # Renovate bump keeps this test green. Never hard-code the version.
  # The resolver prints the pin without its leading 'v'.
  if [[ ! -x "$RESOLVER" ]]; then
    echo "  FAIL: $RESOLVER missing or not executable"
    FAIL=$((FAIL + 1))
    return
  fi
  expected_pin="v$("$RESOLVER")"
  assert_eq "captured pin equals the tag hack/ci-resolve-ovn-version.sh resolves" \
    "$expected_pin" "$captured"
}

test_package_rules() {
  echo "Test: paired packageRules hold the OVN pin on the 26.03 LTS line"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local held_rule patch_rule
  held_rule="$(jq -c --arg file "$DOCKERFILE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  patch_rule="$(jq -c --arg file "$DOCKERFILE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("patch")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$held_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for ${DOCKERFILE_PATH}"
    FAIL=$((FAIL + 2))
  else
    assert_eq "major OVN updates are disabled (27.03 leaves the LTS line)" \
      "false" "$(jq -r '.enabled' <<<"$held_rule")"
    assert_eq "the same rule also disables minors (26.09 leaves the LTS line)" \
      "true" "$(jq '(.matchUpdateTypes | index("minor")) != null' <<<"$held_rule")"
  fi

  if [ -z "$patch_rule" ]; then
    echo "  FAIL: no packageRule for patch OVN updates"
    FAIL=$((FAIL + 2))
    return
  fi

  # Not automerged on purpose: the bump has to carry the ARG OVN_COMMIT and
  # ARG OVS_COMMIT content pins, which no Renovate datasource can rewrite.
  assert_eq "patch OVN updates are not automerged (the content pins need a human)" \
    "false" "$(jq -r '.automerge' <<<"$patch_rule")"
  assert_eq "patch OVN rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$patch_rule")"
}

test_no_release_pins_ovn() {
  echo "Test: no releases/*/source-refs.yaml pins OVN"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local checked=0 refs
  for refs in "$PROJECT_ROOT"/releases/*/source-refs.yaml; do
    [ -f "$refs" ] || continue
    checked=$((checked + 1))
    assert_eq "$(basename "$(dirname "$refs")")/source-refs.yaml carries no 'ovn' key" \
      "null" "$(yq '.ovn' "$refs")"
  done

  if [ "$checked" -eq 0 ]; then
    echo "  FAIL: no releases/*/source-refs.yaml found"
    FAIL=$((FAIL + 1))
  fi
}

test_manager_exists_and_regex_matches
test_package_rules
test_no_release_pins_ovn

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
