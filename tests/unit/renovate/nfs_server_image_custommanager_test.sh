#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# ghcr.io/kubernetes-sigs/nfs-ganesha image pin in the kind NFS overlay and in
# the nfs-health e2e suite, plus the paired packageRules:
#   - the docker-datasource matchStrings regex captures the image's depName,
#     tag (currentValue) AND digest (currentDigest) in both files
#   - the versioning is a regex that reads the package's tags, V3.5, V4.0.8
#     and V6.5, and the tag the pins carry: Renovate's docker versioning
#     strips a lowercase v alone, so it reads none of them as a version
#   - packageRules disable major bumps and gate minor/patch/digest behind a
#     3-day minimumReleaseAge WITHOUT automerge
#
# Why no automerge, unlike the aws-cli and keycloak e2e fixtures it is
# otherwise modelled on: the image is a third-party build of a userspace NFS
# server, and CI and the metal-stack lab run it with `privileged: true`. A new
# tag or a repointed one is a signal to read rather than routine maintenance,
# and the digest pin is the only control on it.
#
# The suite reuses the overlay's reference so the node has the image cached, so
# both pins must stay identical and Renovate must bump them together.
#
# This is the regression test the check-renovate-coverage skill requires for
# every customManager. It does NOT run renovate-config-validator itself. The
# sibling fluxoperator_custommanager_test.sh already exercises that
# authoritative gate over the whole file.
#
# Usage: bash tests/unit/renovate/nfs_server_image_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

NFS_PACKAGE="ghcr.io/kubernetes-sigs/nfs-ganesha"
OVERLAY_PATH="deploy/kind/nfs/nfs-server.yaml"
SUITE_PATH="tests/e2e/infrastructure/nfs-health/chainsaw-test.yaml"

# --- Test 1: customManager targets both files and captures depName/tag/digest ---
test_custom_manager_captures_image() {
  echo "Test: customManagers regex captures the nfs-server depName/tag/digest"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (18 checks skipped)"
    SKIP=$((SKIP + 18))
    return
  fi

  # The nfs-server manager is the docker-datasource customManager on the kind
  # NFS overlay.
  local entry
  entry="$(jq -c '.customManagers[]
    | select(.datasourceTemplate == "docker")
    | select((.managerFilePatterns // []) | join(",") | contains("deploy/kind/nfs"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no docker-datasource customManagers entry for $OVERLAY_PATH"
    FAIL=$((FAIL + 18))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is docker" \
    "docker" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  # The tags carry a capital V, which docker versioning does not strip.
  assert_eq "customManagers.versioningTemplate reads V<major>.<minor>[.<patch>]" \
    'regex:^V(?<major>\d+)\.(?<minor>\d+)(\.(?<patch>\d+))?$' "$(jq -r '.versioningTemplate' <<<"$entry")"

  local patterns
  patterns="$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")"
  assert_contains "managerFilePatterns targets the kind overlay" \
    "$patterns" "deploy/kind/nfs/nfs-server"
  assert_contains "managerFilePatterns targets the nfs-health suite" \
    "$patterns" "tests/e2e/infrastructure/nfs-health/chainsaw-test"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (14 checks skipped)"
    SKIP=$((SKIP + 14))
    return
  fi

  local match_string
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local path line captured
  for path in "$OVERLAY_PATH" "$SUITE_PATH"; do
    line="$(grep -E 'image: ghcr\.io/kubernetes-sigs/nfs-ganesha' \
      "$PROJECT_ROOT/$path" | head -1)"
    assert_not_empty "nfs-server image line present in $path" "$line"

    # Confirm the matchStrings regex captures depName/currentValue/currentDigest
    # from the actual line (perl speaks the same PCRE named-group syntax
    # Renovate uses for the regex customManager).
    captured="$(REGEX="$match_string" LINE="$line" perl -e '
      my $re = $ENV{REGEX}; my $l = $ENV{LINE};
      if ($l =~ /$re/) {
        print "depName=$+{depName}\n";
        print "currentValue=$+{currentValue}\n";
        print "currentDigest=$+{currentDigest}\n";
      }
    ')"

    assert_contains "regex captures the depName in $path" "$captured" "depName=${NFS_PACKAGE}"
    assert_contains "regex captures the tag as currentValue in $path" "$captured" "currentValue="
    assert_contains "regex captures the sha256 digest in $path" "$captured" "currentDigest=sha256:"
  done

  # A pin to a tag the versioning cannot read stops the updates without an
  # error, so apply the regex to the pinned tag and to the shapes around it.
  local versioning pinned_tag version want got
  versioning="$(jq -r '.versioningTemplate' <<<"$entry")"
  versioning="${versioning#regex:}"
  pinned_tag="$(grep -hoE 'image: ghcr\.io/kubernetes-sigs/nfs-ganesha:[^@[:space:]]+' \
    "$PROJECT_ROOT/$OVERLAY_PATH" | head -1 | sed 's/.*nfs-ganesha://')"
  assert_not_empty "the overlay carries a pinned tag to test" "$pinned_tag"
  for version in "$pinned_tag|0" "V3.5|0" "V4.0.8|0" "latest|1" "v6.5|1"; do
    want="${version##*|}"
    version="${version%|*}"
    got=0
    RE="$versioning" VALUE="$version" perl -e 'exit(($ENV{VALUE} =~ /$ENV{RE}/) ? 0 : 1)' || got=$?
    if [ "$want" = 0 ]; then
      assert_eq "the versioning regex reads '$version'" "0" "$got"
    else
      assert_eq "the versioning regex rejects '$version'" "1" "$got"
    fi
  done
}

# --- Test 2: packageRules disable major, gate minor/patch/digest, no automerge ---
test_package_rules() {
  echo "Test: packageRules disable major bumps and never automerge minor/patch/digest"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (10 checks skipped)"
    SKIP=$((SKIP + 10))
    return
  fi

  local path major_rule minor_rule
  for path in "$OVERLAY_PATH" "$SUITE_PATH"; do
    major_rule="$(jq -c --arg p "$NFS_PACKAGE" --arg f "$path" '.packageRules[]
      | select(((.matchPackageNames // []) | index($p)) != null
               and ((.matchFileNames // []) | index($f)) != null
               and ((.matchUpdateTypes // []) | index("major")) != null)' "$RENOVATE_FILE" | head -1)"
    minor_rule="$(jq -c --arg p "$NFS_PACKAGE" --arg f "$path" '.packageRules[]
      | select(((.matchPackageNames // []) | index($p)) != null
               and ((.matchFileNames // []) | index($f)) != null
               and ((.matchUpdateTypes // []) | index("minor")) != null)' "$RENOVATE_FILE" | head -1)"

    if [ -z "$major_rule" ]; then
      echo "  FAIL: no packageRule scoping major updates for $path"
      FAIL=$((FAIL + 1))
    else
      assert_eq "major nfs-server image updates are disabled for $path" \
        "false" "$(jq -r '.enabled' <<<"$major_rule")"
    fi

    if [ -z "$minor_rule" ]; then
      echo "  FAIL: no packageRule scoping minor/patch updates for $path"
      FAIL=$((FAIL + 4))
      continue
    fi

    assert_eq "minor/patch nfs-server image updates are NOT automerged for $path" \
      "false" "$(jq -r '.automerge' <<<"$minor_rule")"
    assert_eq "the rule also covers digest refreshes for $path" \
      "true" "$(jq -r '(.matchUpdateTypes // []) | index("digest") != null' <<<"$minor_rule")"
    assert_eq "minor/patch nfs-server image rule waits minimumReleaseAge=3 days for $path" \
      "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
    assert_eq "minor/patch nfs-server image rule groups both files for $path" \
      "nfs-server e2e fixture" "$(jq -r '.groupName' <<<"$minor_rule")"
  done
}

# --- Test 3: every nfs-server pin is Renovate-covered and consistent ---
# A third copy of the pin that is not added to the customManager + packageRules
# silently stops receiving digest refreshes, so enumerate every pin here.
test_all_nfs_server_pins_covered() {
  echo "Test: every nfs-server image pin is covered by the customManager and packageRules"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local pinned
  pinned="$(cd "$PROJECT_ROOT" && grep -rl 'image: ghcr\.io/kubernetes-sigs/nfs-ganesha' \
    deploy/kind tests/e2e | sort)"
  assert_not_empty "at least one nfs-server image pin exists" "$pinned"

  # The nfs-server image pin is the only customManager on deploy/kind/nfs; the
  # csi-driver-nfs chart version belongs to the native flux manager. The
  # datasource filter keeps this lookup on the image if another manager joins.
  local entry patterns f
  entry="$(jq -c '.customManagers[]
    | select(.datasourceTemplate == "docker")
    | select((.managerFilePatterns // []) | join(",") | contains("deploy/kind/nfs"))' \
    "$RENOVATE_FILE" | head -1)"
  # managerFilePatterns are Renovate regexes ("/path/to/file\\.yaml$/"), so
  # drop the backslash escapes before matching the plain file paths.
  patterns="$(jq -r '.managerFilePatterns | join(",")' <<<"$entry" | tr -d '\\\\')"

  while IFS= read -r f; do
    [ -n "$f" ] || continue
    assert_contains "customManager covers $f" "$patterns" "$f"
    local rules
    rules="$(jq -r --arg p "$NFS_PACKAGE" --arg f "$f" '[.packageRules[]
      | select(((.matchPackageNames // []) | index($p)) != null
               and ((.matchFileNames // []) | index($f)) != null)] | length' "$RENOVATE_FILE")"
    assert_eq "packageRules cover $f (major + minor/patch)" "2" "$rules"
  done <<<"$pinned"

  # The suite reuses the overlay's reference, so a bump that lands in only one
  # of the files would pull a second image onto the kind node.
  local distinct
  distinct="$(cd "$PROJECT_ROOT" && grep -rh 'image: ghcr\.io/kubernetes-sigs/nfs-ganesha' \
    deploy/kind tests/e2e | sed 's/^[[:space:]]*//' | sort -u | wc -l | tr -d '[:space:]')"
  assert_eq "all nfs-server pins reference the same tag and digest" "1" "$distinct"
}

# --- Run ---
test_custom_manager_captures_image
test_package_rules
test_all_nfs_server_pins_covered

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
