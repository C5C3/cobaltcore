#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# docker.io/library/debian image pin of the metal-stack node probe
# (deploy/lab/metal-stack/probe/node-probe.yaml), plus the paired
# packageRules:
#   - the docker-datasource matchStrings regex captures the image's depName,
#     tag (currentValue) AND digest (currentDigest) from the manifest's real
#     image: line
#   - packageRules disable major bumps and automerge minor/patch/digest with a
#     3-day minimumReleaseAge, the shape of the aws-cli fixture's rules.
#     bookworm-slim is a codename tag that docker versioning never reads as a
#     minor or major, so only the digest moves under it.
#   - the manager and both rules also name the migration port reservation
#     (deploy/lab/metal-stack/migration-ports/reservation-daemonset.yaml),
#     whose two containers run the probe's image, and the regex captures the
#     whole pin of both image: lines, so one group moves all three pins.
#
# This is the regression test the check-renovate-coverage skill requires for
# every customManager; that skill's audit does not scan deploy/lab/, so this
# test is what keeps the manager honest. It does NOT run
# renovate-config-validator itself: the sibling
# fluxoperator_custommanager_test.sh already exercises that authoritative gate
# over the whole file.
#
# Usage: bash tests/unit/renovate/metal_stack_probe_image_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
MANIFEST_FILE="$PROJECT_ROOT/deploy/lab/metal-stack/probe/node-probe.yaml"

PROBE_PACKAGE="docker.io/library/debian"
MANIFEST_PATH="deploy/lab/metal-stack/probe/node-probe.yaml"
RESERVATION_PATH="deploy/lab/metal-stack/migration-ports/reservation-daemonset.yaml"
RESERVATION_FILE="$PROJECT_ROOT/$RESERVATION_PATH"

# --- Test 1: customManager targets the manifest and captures depName/tag/digest ---
test_custom_manager_captures_image() {
  echo "Test: customManagers regex captures the node probe image depName/tag/digest"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select(.datasourceTemplate == "docker")
    | select((.managerFilePatterns // []) | join(",") | contains("deploy/lab/metal-stack/probe/node-probe"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no docker-datasource customManagers entry for $MANIFEST_PATH"
    FAIL=$((FAIL + 7))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is docker" \
    "docker" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "customManagers.versioningTemplate is docker" \
    "docker" "$(jq -r '.versioningTemplate' <<<"$entry")"

  local patterns
  patterns="$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")"
  assert_contains "managerFilePatterns targets the probe manifest" \
    "$patterns" "deploy/lab/metal-stack/probe/node-probe"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  # Anchored on the key at the start of a line, so the manifest's header
  # comment, which names the image too, can never stand in for the real line.
  local line match_string
  line="$( { grep -E '^[[:space:]]*image: docker\.io/library/debian' "$MANIFEST_FILE" || true; } | head -1)"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  assert_not_empty "debian image line present in the probe manifest" "$line"

  # perl speaks the same PCRE named-group syntax Renovate uses for the regex
  # customManager.
  local captured current_value
  captured="$(REGEX="$match_string" LINE="$line" perl -e '
    my $re = $ENV{REGEX}; my $l = $ENV{LINE};
    if ($l =~ /$re/) {
      print "depName=$+{depName}\n";
      print "currentValue=$+{currentValue}\n";
      print "currentDigest=$+{currentDigest}\n";
    }
  ')"
  current_value="$(sed -n 's/^currentValue=//p' <<<"$captured")"

  # The whole tag and digest of the line: Renovate rewrites the captured span,
  # so a capture cut short would corrupt the pin.
  local pinned_tag pinned_digest
  pinned_tag="$(sed -E 's/.*debian:([^@]+)@.*/\1/' <<<"$line")"
  pinned_digest="$(sed -E 's/.*@(sha256:[a-f0-9]+).*/\1/' <<<"$line")"
  assert_contains "regex captures the depName" "$captured" "depName=${PROBE_PACKAGE}"
  assert_eq "regex captures the whole tag as currentValue" "$pinned_tag" "$current_value"
  assert_eq "regex captures the whole digest as currentDigest" \
    "$pinned_digest" "$(sed -n 's/^currentDigest=//p' <<<"$captured")"
}

# --- Test 2: packageRules disable major, automerge minor/patch/digest ---
test_package_rules() {
  echo "Test: packageRules disable major bumps and automerge minor/patch/digest"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c --arg p "$PROBE_PACKAGE" --arg f "$MANIFEST_PATH" '.packageRules[]
    | select(((.matchPackageNames // []) | index($p)) != null
             and ((.matchFileNames // []) | index($f)) != null
             and ((.matchUpdateTypes // []) | index("major")) != null)' "$RENOVATE_FILE" | head -1)"
  minor_rule="$(jq -c --arg p "$PROBE_PACKAGE" --arg f "$MANIFEST_PATH" '.packageRules[]
    | select(((.matchPackageNames // []) | index($p)) != null
             and ((.matchFileNames // []) | index($f)) != null
             and ((.matchUpdateTypes // []) | index("minor")) != null)' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for the node probe image"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major node probe image updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch/digest updates for the node probe image"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "the rule covers minor, patch and digest updates" \
    '["minor","patch","digest"]' "$(jq -c '.matchUpdateTypes' <<<"$minor_rule")"
  assert_eq "minor/patch/digest node probe image updates are automerged" \
    "true" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch/digest node probe image rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "the node probe image updates are grouped" \
    "metal-stack node probe image" "$(jq -r '.groupName' <<<"$minor_rule")"
}

# --- Test 3: the manager and both rules cover the migration port reservation ---
test_reservation_is_covered() {
  echo "Test: the manager and its packageRules cover the migration port reservation's image pins"

  if ! command -v jq >/dev/null 2>&1 || ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: jq or perl not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select(.datasourceTemplate == "docker")
    | select((.managerFilePatterns // []) | join(",") | contains("deploy/lab/metal-stack/probe/node-probe"))' \
    "$RENOVATE_FILE")"
  assert_contains "managerFilePatterns targets the reservation manifest" \
    "$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")" \
    "deploy/lab/metal-stack/migration-ports/reservation-daemonset"

  # Both image: lines, each with its whole pin captured: a line the regex
  # misses would keep its digest while the probe's moves.
  local lines captured
  lines="$( { grep -E '^[[:space:]]*image: ' "$RESERVATION_FILE" || true; } | sed -E 's/^[[:space:]]*//')"
  assert_eq "the reservation manifest has two image: lines" "2" "$(grep -c . <<<"$lines")"
  captured="$(REGEX="$(jq -r '.matchStrings[0]' <<<"$entry")" LINES="$lines" perl -e '
    my $re = $ENV{REGEX};
    for my $l (split /\n/, $ENV{LINES}) {
      print "image: $+{depName}:$+{currentValue}\@$+{currentDigest}\n" if $l =~ /$re/;
    }
  ')"
  assert_eq "the regex captures the whole pin of both lines" "$lines" "$captured"

  local types rule
  for types in major minor; do
    rule="$(jq -c --arg p "$PROBE_PACKAGE" --arg f "$MANIFEST_PATH" --arg t "$types" '.packageRules[]
      | select(((.matchPackageNames // []) | index($p)) != null
               and ((.matchFileNames // []) | index($f)) != null
               and ((.matchUpdateTypes // []) | index($t)) != null)' "$RENOVATE_FILE" | head -1)"
    assert_contains "the $types rule of the probe image names the reservation manifest" \
      "$(jq -r '(.matchFileNames // []) | join(",")' <<<"${rule:-{\}}")" "$RESERVATION_PATH"
  done
}

# --- Run ---
test_custom_manager_captures_image
test_package_rules
test_reservation_is_covered

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
