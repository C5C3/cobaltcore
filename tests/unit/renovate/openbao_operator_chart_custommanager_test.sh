#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the openbao-operator chart is delivered under a content pin, and that
# the pin is Renovate-tracked.
#
# The chart is unsigned, so no Flux spec.verify policy can gate it, and the
# operator holds cluster-wide RBAC over the namespace that stores the instance
# seal key. An exact chart.spec.version on a HelmRepository does NOT gate that:
# `0.4.2` on oci://ghcr.io/dc-tec/charts is a mutable tag, Flux tracks the
# resolved artifact digest rather than the tag string, and a re-pushed tag
# therefore upgrades the release on its own within one interval — no Git change,
# no reviewer. Only an OCIRepository ref.digest closes it.
#
# The second half is what keeps the pin from becoming a freeze: Renovate's
# native flux manager does not cover this repo (its default file pattern matches
# only gotk-components.yaml), so without an explicit customManager the operator
# would sit on 0.4.2 forever with no update signal. The manager must capture the
# tag and the digest in ONE matchString so Renovate rewrites both in the same
# reviewed PR; capturing the tag alone would leave the digest behind and Flux
# would keep pulling the old artifact.
#
# A hand-edit can still split the pair, and no file-local assertion can see it:
# the tag and the digest only disagree relative to the registry. The last test
# therefore resolves the pinned tag upstream and compares.
#
# Usage: bash tests/unit/renovate/openbao_operator_chart_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
SOURCE_FILE="$PROJECT_ROOT/deploy/flux-system/sources/openbao-operator.yaml"
RELEASE_FILE="$PROJECT_ROOT/deploy/flux-system/releases/openbao-operator.yaml"

CHART_PACKAGE="ghcr.io/dc-tec/charts/openbao-operator"
SOURCE_PATH="deploy/flux-system/sources/openbao-operator.yaml"

# Read the customManager entry once; every test that needs it re-reads through
# this helper so a missing entry is reported per test instead of aborting.
chart_manager_entry() {
  jq -c --arg path "sources/openbao-operator" '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains($path))' \
    "$RENOVATE_FILE" | head -1
}

# --- Test 1: the source pins chart content, not a chart version ---
test_source_is_digest_pinned_ocirepository() {
  echo "Test: the chart source is a digest-pinned OCIRepository"

  assert_file_contains "source declares kind OCIRepository" \
    "$SOURCE_FILE" "kind: OCIRepository"
  assert_file_contains "source url addresses the chart artifact itself" \
    "$SOURCE_FILE" "url: oci://ghcr.io/dc-tec/charts/openbao-operator"
  assert_file_contains "source selects the Helm chart layer unaltered" \
    "$SOURCE_FILE" "operation: copy"

  # ref.digest takes precedence over every other ref field, so this line is the
  # control: without it the mutable tag decides what runs.
  local digest_line
  digest_line="$(grep -E '^[[:space:]]*digest:[[:space:]]*"sha256:[a-f0-9]{64}"' "$SOURCE_FILE" | head -1)"
  assert_not_empty "source pins ref.digest to a full sha256 digest" "$digest_line"

  assert_file_not_contains "source is no longer a HelmRepository" \
    "$SOURCE_FILE" "kind: HelmRepository"
}

# --- Test 2: the release consumes that source, not a resolvable version ---
test_release_consumes_the_pinned_source() {
  echo "Test: the HelmRelease consumes the pinned source via chartRef"

  assert_file_contains "release references the OCIRepository by chartRef" \
    "$RELEASE_FILE" "kind: OCIRepository"

  # A chart.spec.version would reintroduce Flux-side tag resolution alongside
  # the digest pin, so neither the version key nor a sourceRef may come back.
  assert_file_not_contains "release declares no chart.spec version constraint" \
    "$RELEASE_FILE" "version:"
  assert_file_not_contains "release declares no HelmRepository sourceRef" \
    "$RELEASE_FILE" "sourceRef:"
}

# --- Test 3: a customManager tracks the pin with the docker datasource ---
test_custom_manager_tracks_the_chart() {
  echo "Test: a customManager tracks the openbao-operator chart pin"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local entry
  entry="$(chart_manager_entry)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting $SOURCE_PATH"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "chart customManager.datasourceTemplate is docker" \
    "docker" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "chart customManager.depNameTemplate is the chart artifact" \
    "$CHART_PACKAGE" \
    "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_eq "chart customManager.packageNameTemplate is the chart artifact" \
    "$CHART_PACKAGE" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")"
  assert_eq "chart customManager.versioningTemplate is docker" \
    "docker" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"
}

# --- Test 4: the regex captures tag and digest together ---
test_regex_captures_tag_and_digest_together() {
  echo "Test: the customManager regex captures ref.tag and ref.digest in one match"

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
  entry="$(chart_manager_entry)"
  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting $SOURCE_PATH"
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

  local captured
  captured="$(REGEX="$match_string" FILE="$SOURCE_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/;
    open my $fh, "<", $ENV{FILE} or exit 1;
    my $content = <$fh>;
    if ($content =~ /$re/) {
      printf "%s|%s", $+{currentValue} // "", $+{currentDigest} // "";
    }
  ')"

  local expected_tag expected_digest
  expected_tag="$(grep -E '^[[:space:]]*tag:' "$SOURCE_FILE" | head -1 | sed -E 's/.*tag:[[:space:]]*"([^"]+)".*/\1/')"
  expected_digest="$(grep -E '^[[:space:]]*digest:' "$SOURCE_FILE" | head -1 | sed -E 's/.*digest:[[:space:]]*"([^"]+)".*/\1/')"

  assert_eq "regex captures the ref.tag as currentValue" \
    "$expected_tag" "${captured%%|*}"
  assert_eq "regex captures the ref.digest as currentDigest" \
    "$expected_digest" "${captured##*|}"
}

# --- Test 5: the paired packageRule never automerges the chart ---
test_package_rule_never_automerges() {
  echo "Test: the paired packageRule reviews chart bumps instead of automerging"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local rules
  rules="$(jq -c --arg pkg "$CHART_PACKAGE" --arg path "$SOURCE_PATH" '[.packageRules[]
    | select(
        (((.matchPackageNames // []) | index($pkg)) != null)
        or (((.matchFileNames // []) | index($path)) != null)
      )]' "$RENOVATE_FILE")"

  if [ "$(jq 'length' <<<"$rules")" = "0" ]; then
    echo "  FAIL: no packageRule scoping updates for $CHART_PACKAGE"
    FAIL=$((FAIL + 3))
    return
  fi

  # An operator in the production data path never merges itself: every bump of
  # this chart is a human decision, so no matching rule may set automerge.
  assert_eq "no rule automerges the openbao-operator chart" \
    "0" \
    "$(jq '[.[] | select(.automerge == true)] | length' <<<"$rules")"
  assert_eq "a rule holds new chart releases for 3 days" \
    "1" \
    "$(jq '[.[] | select(.minimumReleaseAge == "3 days")] | length' <<<"$rules")"
  assert_eq "the chart rule groupName is openbao-operator chart" \
    "openbao-operator chart" \
    "$(jq -r '[.[] | select(.groupName != null)][0].groupName' <<<"$rules")"
}

# --- Test 6: tag and digest describe the same upstream artifact ---
#
# Tests 1 and 4 are self-consistent by construction: they compare the file
# against itself, so they stay green when a hand-edit (a merge-conflict
# resolution on a Renovate PR, a bump made ahead of Renovate) advances ref.tag
# and leaves ref.digest behind. Flux would not notice either — ref.digest takes
# precedence, so the tag is decoration and the cluster keeps running the old
# chart while every local signal, including the docs table, reads as bumped.
# Only the registry can settle whether the two agree, so this test resolves the
# tag and compares.
#
# Skips rather than fails when the registry cannot be reached: an offline
# workstation or a rate-limited runner must not turn the suite red.
test_tag_and_digest_agree_upstream() {
  echo "Test: ref.tag and ref.digest resolve to the same upstream artifact"

  if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: curl or jq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local tag digest registry repository
  tag="$(grep -E '^[[:space:]]*tag:' "$SOURCE_FILE" | head -1 | sed -E 's/.*tag:[[:space:]]*"([^"]+)".*/\1/')"
  digest="$(grep -E '^[[:space:]]*digest:' "$SOURCE_FILE" | head -1 | sed -E 's/.*digest:[[:space:]]*"([^"]+)".*/\1/')"
  registry="${CHART_PACKAGE%%/*}"
  repository="${CHART_PACKAGE#*/}"

  # GHCR serves public packages to an anonymous pull token, so no credential is
  # needed here.
  local token
  token="$(curl -sS --max-time 20 \
    "https://${registry}/token?service=${registry}&scope=repository:${repository}:pull" \
    2>/dev/null | jq -r '.token // empty')"

  if [ -z "$token" ]; then
    echo "  SKIP: ${registry} unreachable — cannot resolve ${tag} (1 check skipped)"
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

  assert_eq "ref.tag ${tag} resolves to the pinned ref.digest" \
    "$digest" "$resolved"
}

test_source_is_digest_pinned_ocirepository
test_release_consumes_the_pinned_source
test_custom_manager_tracks_the_chart
test_regex_captures_tag_and_digest_together
test_package_rule_never_automerges
test_tag_and_digest_agree_upstream

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
