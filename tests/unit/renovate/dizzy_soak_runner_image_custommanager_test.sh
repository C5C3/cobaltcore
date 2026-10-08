#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# docker.io/alpine/k8s image pin of the dizzy soak's runner
# (deploy/lab/metal-stack/dizzy-soak/job.yaml), plus the paired
# packageRules:
#   - the docker-datasource matchStrings regex captures the image's depName,
#     tag (currentValue) AND digest (currentDigest) from the manifest's real
#     image: line, and captures nothing from the dizzy image's line, whose
#     tag is the token start replaces;
#   - packageRules disable major and minor bumps (the tag is the kubectl
#     release, held on the lab's Kubernetes minor) and automerge patch and
#     digest updates with a 3-day minimumReleaseAge in the group
#     `dizzy soak runner image`.
#
# This is the regression test the check-renovate-coverage skill requires for
# every customManager; that skill's audit does not scan deploy/lab/. It does
# NOT run renovate-config-validator itself: the sibling
# fluxoperator_custommanager_test.sh exercises that gate over the whole file.
#
# Usage: bash tests/unit/renovate/dizzy_soak_runner_image_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
MANIFEST_PATH="deploy/lab/metal-stack/dizzy-soak/job.yaml"
MANIFEST_FILE="$PROJECT_ROOT/$MANIFEST_PATH"
RUNNER_PACKAGE="docker.io/alpine/k8s"

# --- Test 1: customManager targets job.yaml and captures depName/tag/digest ---
test_custom_manager_captures_image() {
  echo "Test: customManagers regex captures the soak runner image depName/tag/digest"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select(.datasourceTemplate == "docker")
    | select((.managerFilePatterns // []) | join(",") | contains("deploy/lab/metal-stack/dizzy-soak/job"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no docker-datasource customManagers entry for $MANIFEST_PATH"
    FAIL=$((FAIL + 8))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is docker" \
    "docker" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "customManagers.versioningTemplate is docker" \
    "docker" "$(jq -r '.versioningTemplate' <<<"$entry")"
  assert_eq "managerFilePatterns targets job.yaml alone" \
    '["/deploy/lab/metal-stack/dizzy-soak/job\\.yaml$/"]' "$(jq -c '.managerFilePatterns' <<<"$entry")"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # Anchored on the key at the start of a line, so a header comment that names
  # the image can never stand in for the real line.
  local line match_string
  line="$( { grep -E '^[[:space:]]*image: docker\.io/alpine/k8s' "$MANIFEST_FILE" || true; } | head -1)"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  assert_not_empty "the runner image line is present in job.yaml" "$line"

  # perl speaks the same PCRE named-group syntax Renovate uses for the regex
  # customManager.
  local captured
  captured="$(REGEX="$match_string" LINE="$line" perl -e '
    my $re = $ENV{REGEX}; my $l = $ENV{LINE};
    if ($l =~ /$re/) {
      print "depName=$+{depName}\n";
      print "currentValue=$+{currentValue}\n";
      print "currentDigest=$+{currentDigest}\n";
    }
  ')"

  # The whole tag and digest of the line: Renovate rewrites the captured span,
  # so a capture cut short would corrupt the pin.
  local pinned_tag pinned_digest
  pinned_tag="$(sed -E 's/.*k8s:([^@]+)@.*/\1/' <<<"$line")"
  pinned_digest="$(sed -E 's/.*@(sha256:[a-f0-9]+).*/\1/' <<<"$line")"
  assert_contains "regex captures the depName" "$captured" "depName=${RUNNER_PACKAGE}"
  assert_eq "regex captures the whole tag as currentValue" "$pinned_tag" \
    "$(sed -n 's/^currentValue=//p' <<<"$captured")"
  assert_eq "regex captures the whole digest as currentDigest" "$pinned_digest" \
    "$(sed -n 's/^currentDigest=//p' <<<"$captured")"

  # Every image: line of the file: only the runner's matches, never the dizzy
  # image with its DIZZY_VERSION token.
  local matches
  matches="$(REGEX="$match_string" FILE="$MANIFEST_FILE" perl -ne '
    BEGIN { $re = $ENV{REGEX} } print if /^\s*image: / && /$re/' "$MANIFEST_FILE" | grep -c . || true)"
  assert_eq "the regex matches one image: line of job.yaml" "1" "$matches"
}

# --- Test 2: packageRules disable major and minor, automerge patch and digest ---
test_package_rules() {
  echo "Test: packageRules disable major and minor bumps and automerge patch and digest updates"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi

  local disabled_rule patch_rule
  disabled_rule="$(jq -c --arg p "$RUNNER_PACKAGE" --arg f "$MANIFEST_PATH" '.packageRules[]
    | select(((.matchPackageNames // []) | index($p)) != null
             and ((.matchFileNames // []) | index($f)) != null
             and ((.matchUpdateTypes // []) | index("major")) != null)' "$RENOVATE_FILE" | head -1)"
  patch_rule="$(jq -c --arg p "$RUNNER_PACKAGE" --arg f "$MANIFEST_PATH" '.packageRules[]
    | select(((.matchPackageNames // []) | index($p)) != null
             and ((.matchFileNames // []) | index($f)) != null
             and ((.matchUpdateTypes // []) | index("patch")) != null)' "$RENOVATE_FILE" | head -1)"

  if [ -z "$disabled_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for the soak runner image"
    FAIL=$((FAIL + 2))
  else
    assert_eq "the rule covers major and minor updates" '["major","minor"]' \
      "$(jq -c '.matchUpdateTypes' <<<"$disabled_rule")"
    assert_eq "major and minor updates of the runner image are disabled" \
      "false" "$(jq -r '.enabled' <<<"$disabled_rule")"
  fi

  if [ -z "$patch_rule" ]; then
    echo "  FAIL: no packageRule scoping patch and digest updates for the soak runner image"
    FAIL=$((FAIL + 5))
    return
  fi

  assert_eq "the rule covers patch and digest updates" '["patch","digest"]' \
    "$(jq -c '.matchUpdateTypes' <<<"$patch_rule")"
  assert_eq "the rule scopes the custom regex manager" '["custom.regex"]' \
    "$(jq -c '.matchManagers' <<<"$patch_rule")"
  assert_eq "patch and digest updates of the runner image are automerged" \
    "true" "$(jq -r '.automerge' <<<"$patch_rule")"
  assert_eq "they wait minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$patch_rule")"
  assert_eq "they are grouped" \
    "dizzy soak runner image" "$(jq -r '.groupName' <<<"$patch_rule")"
}

# --- Run ---
test_custom_manager_captures_image
test_package_rules

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
