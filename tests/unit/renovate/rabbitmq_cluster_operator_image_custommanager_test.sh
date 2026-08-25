#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the RabbitMQ Cluster Operator image is pinned by digest, and that the
# pin is Renovate-tracked.
#
# The upstream kustomize base this Flux Kustomization builds rewrites its dev
# placeholder to ghcr.io/rabbitmq/cluster-operator:latest, so the images
# override in releases/rabbitmq-cluster-operator.yaml is the only thing keeping
# a mutable tag out of the cluster. The digest resolves the pull; newTag stays
# as the human-readable record of the tag the digest was resolved from and as
# the offline drift anchor.
#
# The second half is what keeps the pin from becoming a freeze: Renovate's
# native flux manager does not cover this file, so without an explicit
# customManager the operator image would sit on the pinned digest forever with
# no update signal. The manager must capture newTag and digest in ONE
# matchString so Renovate rewrites both in the same reviewed PR; capturing the
# tag alone would leave the digest behind and the cluster would keep pulling the
# old image.
#
# A hand-edit can still split the pair, and no file-local assertion can see it:
# the tag and the digest only disagree relative to the registry. The last test
# therefore resolves the pinned tag at ghcr.io and compares.
#
# Usage: bash tests/unit/renovate/rabbitmq_cluster_operator_image_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
RELEASE_FILE="$PROJECT_ROOT/deploy/flux-system/releases/rabbitmq-cluster-operator.yaml"

IMAGE_PACKAGE="ghcr.io/rabbitmq/cluster-operator"
RELEASE_PATH="deploy/flux-system/releases/rabbitmq-cluster-operator.yaml"
SOURCE_PATH="deploy/flux-system/sources/rabbitmq-cluster-operator.yaml"

# Read the customManager entry once; every test that needs it re-reads through
# this helper so a missing entry is reported per test instead of aborting.
image_manager_entry() {
  jq -c --arg path "releases/rabbitmq-cluster-operator" '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains($path))' \
    "$RENOVATE_FILE" | head -1
}

# Reads the pinned image tag out of the release file. The comment block names
# newTag in prose, so the anchor is the key at the start of a line.
pinned_tag() {
  grep -E '^[[:space:]]*newTag:' "$RELEASE_FILE" | head -1 \
    | sed -E 's/.*newTag:[[:space:]]*"([^"]+)".*/\1/'
}

pinned_digest() {
  grep -E '^[[:space:]]*digest:' "$RELEASE_FILE" | head -1 \
    | sed -E 's/.*digest:[[:space:]]*"([^"]+)".*/\1/'
}

# --- Test 1: the release pins the image by digest ---
test_release_pins_the_image_by_digest() {
  echo "Test: the release is a Kustomization pinning the operator image by digest"

  assert_file_contains "release declares kind Kustomization" \
    "$RELEASE_FILE" "kind: Kustomization"
  assert_file_contains "release builds the upstream installation base" \
    "$RELEASE_FILE" "path: ./config/installation"
  assert_file_contains "release prunes resources it no longer renders" \
    "$RELEASE_FILE" "prune: true"
  assert_file_contains "release waits for the applied resources to become ready" \
    "$RELEASE_FILE" "wait: true"

  # The inner kustomization already resolves the dev placeholder to this name,
  # so the override keys on the resolved name rather than the placeholder.
  assert_file_contains "release overrides the resolved operator image name" \
    "$RELEASE_FILE" "name: $IMAGE_PACKAGE"

  local digest_line
  digest_line="$(grep -E '^[[:space:]]*digest:[[:space:]]*"sha256:[a-f0-9]{64}"' "$RELEASE_FILE" | head -1)"
  assert_not_empty "release pins the image to a full sha256 digest" "$digest_line"

  local tag_line
  tag_line="$(grep -E '^[[:space:]]*newTag:[[:space:]]*"[^"]+"' "$RELEASE_FILE" | head -1)"
  assert_not_empty "release records the resolved tag as a quoted newTag" "$tag_line"

  # Upstream ships no chart, so a HelmRelease here would mean someone
  # reintroduced the retired Bitnami packaging.
  assert_file_not_contains "release is not a HelmRelease" \
    "$RELEASE_FILE" "kind: HelmRelease"
}

# --- Test 2: a customManager tracks the pin against the registry ---
test_custom_manager_tracks_the_image() {
  echo "Test: a customManager tracks the rabbitmq-cluster-operator image pin"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local entry
  entry="$(image_manager_entry)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting $RELEASE_PATH"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "image customManager.datasourceTemplate is docker" \
    "docker" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "image customManager.depNameTemplate is the operator image" \
    "$IMAGE_PACKAGE" \
    "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_eq "image customManager.packageNameTemplate is the operator image" \
    "$IMAGE_PACKAGE" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")"
  assert_eq "image customManager.versioningTemplate is docker" \
    "docker" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"
}

# --- Test 3: the regex captures newTag and digest together ---
test_regex_captures_tag_and_digest_together() {
  echo "Test: the customManager regex captures newTag and digest in one match"

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

  local entry match_string
  entry="$(image_manager_entry)"
  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting $RELEASE_PATH"
    FAIL=$((FAIL + 3))
    return
  fi

  # One matchString must carry BOTH groups: Renovate rewrites the whole matched
  # span, so a split pair would update the tag and leave the digest stale.
  assert_eq "exactly one matchString captures both currentValue and currentDigest" \
    "1" \
    "$(jq '[.matchStrings[]
           | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))]
          | length' <<<"$entry")"

  match_string="$(jq -r '.matchStrings[]
    | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))' <<<"$entry" | head -1)"

  # Replaying the regex over the file on disk is what ties the two together:
  # a reordered or unquoted pair still parses as YAML and still deploys, but it
  # stops matching here.
  local captured
  captured="$(REGEX="$match_string" FILE="$RELEASE_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/;
    open my $fh, "<", $ENV{FILE} or exit 1;
    my $content = <$fh>;
    if ($content =~ /$re/) {
      printf "%s|%s", $+{currentValue} // "", $+{currentDigest} // "";
    }
  ')"

  assert_eq "regex captures the images newTag as currentValue" \
    "$(pinned_tag)" "${captured%%|*}"
  assert_eq "regex captures the images digest as currentDigest" \
    "$(pinned_digest)" "${captured##*|}"
}

# --- Test 4: the paired packageRule never automerges the image bump ---
test_package_rule_never_automerges() {
  echo "Test: the paired packageRule reviews image bumps instead of automerging"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local rules
  rules="$(jq -c --arg pkg "$IMAGE_PACKAGE" --arg path "$RELEASE_PATH" '[.packageRules[]
    | select(
        (((.matchPackageNames // []) | index($pkg)) != null)
        or (((.matchFileNames // []) | index($path)) != null)
      )]' "$RENOVATE_FILE")"

  if [ "$(jq 'length' <<<"$rules")" = "0" ]; then
    echo "  FAIL: no packageRule scoping updates for $IMAGE_PACKAGE"
    FAIL=$((FAIL + 3))
    return
  fi

  # An operator in the messaging data path never merges itself: every bump of
  # this image is a human decision, so no matching rule may set automerge.
  assert_eq "no rule automerges the rabbitmq-cluster-operator image" \
    "0" \
    "$(jq '[.[] | select(.automerge == true)] | length' <<<"$rules")"
  assert_eq "a rule holds new operator images for 3 days" \
    "1" \
    "$(jq '[.[] | select(.minimumReleaseAge == "3 days")] | length' <<<"$rules")"
  assert_eq "the image rule groupName is rabbitmq cluster-operator" \
    "rabbitmq cluster-operator" \
    "$(jq -r '[.[] | select(.groupName != null)][0].groupName' <<<"$rules")"
}

# --- Test 5: source and image rules share one group ---
#
# The four literals of this operator live in two files: tag and commit in the
# GitRepository, newTag and digest in this Kustomization. Renovate raises one
# PR per group, so only a shared groupName puts all four in front of the same
# reviewer. Two groups would deploy an image built from a revision the source
# does not check out, for as long as one of the two PRs sits unmerged.
test_both_rabbitmq_rules_share_one_group() {
  echo "Test: source and image packageRules share one group so one PR moves both"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local groups
  groups="$(jq -r --arg src "$SOURCE_PATH" --arg rel "$RELEASE_PATH" '[.packageRules[]
    | select(
        (((.matchFileNames // []) | index($src)) != null)
        or (((.matchFileNames // []) | index($rel)) != null)
      )
    | .groupName]' "$RENOVATE_FILE")"

  assert_eq "both rabbitmq packageRules declare a groupName" \
    "2" \
    "$(jq '[.[] | select(. != null)] | length' <<<"$groups")"
  assert_eq "the two rabbitmq packageRules share exactly one groupName" \
    '["rabbitmq cluster-operator"]' \
    "$(jq -c 'unique' <<<"$groups")"
}

# --- Test 6: newTag and digest describe the same upstream image ---
#
# Tests 1 and 3 are self-consistent by construction: they compare the file
# against itself, so they stay green when a hand-edit (a merge-conflict
# resolution on a Renovate PR, a bump made ahead of Renovate) advances newTag
# and leaves the digest behind. Kustomize would not notice either, because the
# digest wins over newTag in the rewritten image reference, so the cluster keeps
# running the old image while every local signal reads as bumped. Only the
# registry can settle whether the two agree, so this test resolves the tag and
# compares.
#
# Skips rather than fails when the registry cannot be reached: an offline
# workstation or a rate-limited runner must not turn the suite red.
test_tag_and_digest_agree_upstream() {
  echo "Test: newTag and digest resolve to the same upstream image"

  if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: curl or jq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local tag digest registry repository
  tag="$(pinned_tag)"
  digest="$(pinned_digest)"
  registry="${IMAGE_PACKAGE%%/*}"
  repository="${IMAGE_PACKAGE#*/}"

  # GHCR serves public packages to an anonymous pull token, so no credential is
  # needed here.
  local token
  token="$(curl -sS --max-time 20 \
    "https://${registry}/token?service=${registry}&scope=repository:${repository}:pull" \
    2>/dev/null | jq -r '.token // empty')"

  if [ -z "$token" ]; then
    echo "  SKIP: ${registry} unreachable, cannot resolve ${tag} (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local resolved
  resolved="$(curl -sS --max-time 20 -I \
    -H "Authorization: Bearer ${token}" \
    -H "Accept: application/vnd.oci.image.manifest.v1+json,application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.v2+json" \
    "https://${registry}/v2/${repository}/manifests/${tag}" 2>/dev/null \
    | tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" { print $2 }' | head -1)"

  if [ -z "$resolved" ]; then
    echo "  SKIP: ${registry} did not resolve tag ${tag} (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  assert_eq "newTag ${tag} resolves to the pinned digest" \
    "$digest" "$resolved"
}

test_release_pins_the_image_by_digest
test_custom_manager_tracks_the_image
test_regex_captures_tag_and_digest_together
test_package_rule_never_automerges
test_both_rabbitmq_rules_share_one_group
test_tag_and_digest_agree_upstream

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
