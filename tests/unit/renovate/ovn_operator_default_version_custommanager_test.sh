#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the defaultOVNVersion pin in
# operators/ovn/internal/controller/image.go and moves it together with the
# ARG OVN_VERSION pin in images/ovn/Dockerfile.
#
# The constant is the tag every OVNCentral and OVNChassis CR that leaves
# spec.image unset runs. It is a Go string literal, so no native manager sees
# it, and left untracked it drifts away from the image images/ovn/Dockerfile
# builds until the operator asks for a tag the registry never received.
#
# Asserts:
#   - exactly one customManager targets ovn-org/ovn over the controller source
#     with the github-tags datasource, because ovn-org/ovn publishes tags but
#     no GitHub releases
#   - it strips the leading 'v' of those tags, and its versioningTemplate regex
#     carries no 'v' either: the Go constant holds the bare version, where the
#     Dockerfile pin keeps the prefix of the upstream tag
#   - that versioning regex parses the pin the source currently holds into
#     major, minor and patch
#   - its matchStrings regex captures that pin from the real source file
#     exactly once, and the capture equals the literal of the const assignment
#   - the paired packageRule that disables majors disables minors too and
#     covers both pins: 26.09 is a minor bump and 27.03 a major one, and both
#     leave the 26.03 LTS line
#   - the paired patch rule waits a 3-day cooldown, never automerges, and
#     groups both pins under one name so they arrive in a single PR.
#     TestDefaultOVNVersionMatchesDockerfilePin in
#     operators/ovn/internal/controller/ fails on a PR that moves only one
#
# Usage: bash tests/unit/renovate/ovn_operator_default_version_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
IMAGE_FILE="$PROJECT_ROOT/operators/ovn/internal/controller/image.go"

PKG_NAME="ovn-org/ovn"
IMAGE_PATH="operators/ovn/internal/controller/image.go"
DOCKERFILE_PATH="images/ovn/Dockerfile"

test_manager_exists_and_regex_matches() {
  echo "Test: the defaultOVNVersion pin has a customManager whose regex matches"

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
  if [[ ! -f "$IMAGE_FILE" ]]; then
    echo "  FAIL: $IMAGE_FILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local count
  count="$(jq --arg pkg "$PKG_NAME" '[.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("controller/image")))] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets ${PKG_NAME} over the controller source" \
    "1" "$count"

  local entry
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("controller/image")))' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PKG_NAME} over ${IMAGE_PATH}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_contains "manager targets ${IMAGE_PATH}" \
    "$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")" "operators/ovn/internal/controller/image"
  assert_eq "manager uses the github-tags datasource (ovn-org/ovn cuts no releases)" \
    "github-tags" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "manager strips the v prefix of the upstream tags" \
    '^v(?<version>.+)$' "$(jq -r '.extractVersionTemplate' <<<"$entry")"

  local match_string captures captured match_count expected_pin
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captures="$(REGEX="$match_string" FILE="$IMAGE_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    while ($content =~ /$re/g) {
      print "$+{currentValue}\n" if defined $+{currentValue};
    }
  ')"
  captured="$(printf '%s\n' "$captures" | head -1)"
  match_count="$(printf '%s\n' "$captures" | grep -c . || true)"

  assert_not_empty "matchStrings regex captures the controller version pin" "$captured"
  # The file names the const again where effectiveImage reads it back. A regex
  # loose enough to reach that line would hand Renovate a second, bogus dep.
  assert_eq "matchStrings regex captures exactly one value in the source" \
    "1" "$match_count"

  # Derive the expected pin straight from the source so a Renovate bump keeps
  # this test green — never hard-code the version.
  expected_pin="$(grep -oE 'defaultOVNVersion = "[0-9]+\.[0-9]+\.[0-9]+"' "$IMAGE_FILE" \
    | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  assert_eq "captured pin equals the literal const assignment" \
    "$expected_pin" "$captured"

  local versioning versioning_re parsed
  versioning="$(jq -r '.versioningTemplate' <<<"$entry")"
  # 26.03 carries a leading zero strict semver rejects, so the pin needs a
  # regex versioning — one without the 'v' the Dockerfile manager matches.
  assert_starts_with "manager parses the captured pin with a regex versioning" \
    "$versioning" "regex:"
  versioning_re="${versioning#regex:}"
  parsed="$(REGEX="$versioning_re" VALUE="$captured" perl -e '
    my $re = $ENV{REGEX};
    if ($ENV{VALUE} =~ /$re/) {
      print join(".", $+{major} // "", $+{minor} // "", $+{patch} // "");
    }
  ')"
  assert_eq "the versioning regex parses the pin into major.minor.patch" \
    "$captured" "$parsed"
}

test_package_rules() {
  echo "Test: paired packageRules move the controller pin with the Dockerfile pin"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local held_rule patch_rule
  held_rule="$(jq -c --arg path "$IMAGE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  patch_rule="$(jq -c --arg path "$IMAGE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("patch")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$held_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for $IMAGE_PATH"
    FAIL=$((FAIL + 3))
  else
    assert_eq "major controller-pin updates are disabled (27.03 leaves the LTS line)" \
      "false" "$(jq -r '.enabled' <<<"$held_rule")"
    assert_eq "the same rule also disables minors (26.09 leaves the LTS line)" \
      "true" "$(jq '(.matchUpdateTypes | index("minor")) != null' <<<"$held_rule")"
    assert_contains "the held rule also covers the Dockerfile pin" \
      "$(jq -r '.matchFileNames | join(" ")' <<<"$held_rule")" "$DOCKERFILE_PATH"
  fi

  if [ -z "$patch_rule" ]; then
    echo "  FAIL: no packageRule for patch controller-pin updates"
    FAIL=$((FAIL + 3))
    return
  fi

  # Not automerged on purpose: the same bump has to carry the ARG OVN_COMMIT
  # and ARG OVS_COMMIT content pins in the Dockerfile, which no Renovate
  # datasource can rewrite.
  assert_eq "patch controller-pin updates are not automerged (the content pins need a human)" \
    "false" "$(jq -r '.automerge' <<<"$patch_rule")"
  assert_eq "patch controller-pin rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$patch_rule")"
  # Both pins have to arrive in one PR: an operator default the built image
  # never received resolves to a tag no registry serves.
  assert_eq "the patch rule groups both pins under one PR" \
    "OVN LTS patch releases" "$(jq -r '.groupName' <<<"$patch_rule")"
  assert_contains "the patch rule also covers the Dockerfile pin" \
    "$(jq -r '.matchFileNames | join(" ")' <<<"$patch_rule")" "$DOCKERFILE_PATH"
}

test_manager_exists_and_regex_matches
test_package_rules

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
