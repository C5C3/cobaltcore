#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the kvm-node-agent chart of the lab hypervisors is delivered under a
# content pin, and that the pin is Renovate-tracked.
#
# deploy/lab/metal-stack/hypervisor/sources.yaml pins the chart with an
# OCIRepository ref.digest, which takes precedence over ref.tag, so a re-pushed
# 0.2.0 tag changes nothing that runs. Renovate's native flux manager does not
# cover this repo, so a customManager tracks the pin, and it must capture the
# tag and the digest in ONE matchString: Renovate rewrites the whole matched
# span, and a tag bumped without its digest would leave Flux on the old chart.
#
# The same file pins the openstack-hypervisor-operator chart under a tag of the
# form 1.2.3_sha-<commit>. That ref follows the hvo image pin, not Renovate
# (tests/unit/deploy/metal_stack_hypervisor_test.sh checks the lockstep), so
# the regex must match the kvm-node-agent pair alone. Upstream also publishes
# a 0.2.0_sha-<commit> chart for every main commit, and the paired packageRule
# allows only plain releases so Renovate never proposes one.
#
# The last test resolves the pinned tag upstream: a hand-edit can split tag and
# digest in a way no file-local assertion sees.
#
# Usage: bash tests/unit/renovate/kvm_node_agent_chart_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
SOURCE_FILE="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/sources.yaml"
RELEASE_FILE="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/kna-release.yaml"

CHART_PACKAGE="ghcr.io/cobaltcore-dev/charts/kvm-node-agent"
SOURCE_PATH="deploy/lab/metal-stack/hypervisor/sources.yaml"

# Read the customManager entry once; every test that needs it re-reads through
# this helper so a missing entry is reported per test instead of aborting.
chart_manager_entry() {
  jq -c --arg path "hypervisor/sources" '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains($path))' \
    "$RENOVATE_FILE" | head -1
}

# kna_ref <key>
# The value of ref.<key> in the OCIRepository whose url is the kvm-node-agent
# chart: the first <key> line after that url.
kna_ref() {
  awk -v key="$1" '
    $0 ~ "url: oci://ghcr.io/cobaltcore-dev/charts/kvm-node-agent$" { found = 1; next }
    found && $0 ~ "^[[:space:]]*" key ":" {
      sub("^[[:space:]]*" key ":[[:space:]]*", ""); gsub(/"/, ""); print; exit
    }
  ' "$SOURCE_FILE"
}

# --- Test 1: the source pins chart content, not a chart version ---
test_source_is_digest_pinned_ocirepository() {
  echo "Test: the kvm-node-agent source is a digest-pinned OCIRepository"

  assert_file_contains "source declares kind OCIRepository" \
    "$SOURCE_FILE" "kind: OCIRepository"
  assert_file_contains "source url addresses the chart artifact itself" \
    "$SOURCE_FILE" "url: oci://ghcr.io/cobaltcore-dev/charts/kvm-node-agent"
  assert_file_contains "source selects the Helm chart layer unaltered" \
    "$SOURCE_FILE" "operation: copy"
  # ref.digest takes precedence over every other ref field, so this value is
  # the control: without it the mutable tag decides what runs.
  assert_eq "the kvm-node-agent ref pins a full sha256 digest" "true" \
    "$([[ "$(kna_ref digest)" =~ ^sha256:[a-f0-9]{64}$ ]] && echo true || echo false)"
}

# --- Test 2: the release consumes that source, not a resolvable version ---
test_release_consumes_the_pinned_source() {
  echo "Test: the kvm-node-agent HelmRelease consumes the pinned source via chartRef"

  assert_file_contains "release references the OCIRepository by chartRef" \
    "$RELEASE_FILE" "kind: OCIRepository"
  assert_file_not_contains "release declares no chart.spec version constraint" \
    "$RELEASE_FILE" "version:"
  assert_file_not_contains "release declares no HelmRepository sourceRef" \
    "$RELEASE_FILE" "sourceRef:"
}

# --- Test 3: a customManager tracks the pin with the docker datasource ---
test_custom_manager_tracks_the_chart() {
  echo "Test: a customManager tracks the kvm-node-agent chart pin"

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
    "docker" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "chart customManager.depNameTemplate is the chart artifact" \
    "$CHART_PACKAGE" "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_eq "chart customManager.packageNameTemplate is the chart artifact" \
    "$CHART_PACKAGE" "$(jq -r '.packageNameTemplate' <<<"$entry")"
  assert_eq "chart customManager.versioningTemplate is docker" \
    "docker" "$(jq -r '.versioningTemplate' <<<"$entry")"
}

# --- Test 4: the regex captures the kvm-node-agent tag and digest together ---
test_regex_captures_tag_and_digest_together() {
  echo "Test: the customManager regex captures the kvm-node-agent ref.tag and ref.digest in one match"

  if ! command -v jq >/dev/null 2>&1 || ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: jq or perl not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local entry match_string
  entry="$(chart_manager_entry)"
  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting $SOURCE_PATH"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "exactly one matchString captures both currentValue and currentDigest" \
    "1" \
    "$(jq '[.matchStrings[]
           | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))]
          | length' <<<"$entry")"

  match_string="$(jq -r '.matchStrings[]
    | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))' <<<"$entry" | head -1)"

  # Every match in the file, one "<tag>|<digest>" per line: the hvo ref must
  # not be one, and a kna tag and digest that a line separates are none.
  local captured
  captured="$(REGEX="$match_string" FILE="$SOURCE_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/;
    open my $fh, "<", $ENV{FILE} or exit 1;
    my $content = <$fh>;
    while ($content =~ /$re/g) {
      printf "%s|%s\n", $+{currentValue} // "", $+{currentDigest} // "";
    }
  ')"

  assert_eq "the regex matches ${SOURCE_PATH} once" "1" "$(grep -c . <<<"$captured")"
  assert_eq "it captures the kvm-node-agent ref.tag as currentValue" \
    "$(kna_ref tag)" "$(head -n 1 <<<"$captured" | cut -d'|' -f1)"
  assert_eq "it captures the kvm-node-agent ref.digest as currentDigest" \
    "$(kna_ref digest)" "$(head -n 1 <<<"$captured" | cut -d'|' -f2)"
}

# --- Test 5: the paired packageRule reviews plain releases only ---
test_package_rule() {
  echo "Test: the paired packageRule reviews plain chart releases instead of automerging"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
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
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "no rule automerges the kvm-node-agent chart" \
    "0" "$(jq '[.[] | select(.automerge == true)] | length' <<<"$rules")"
  assert_eq "a rule holds new chart releases for 3 days" \
    "1" "$(jq '[.[] | select(.minimumReleaseAge == "3 days")] | length' <<<"$rules")"
  assert_eq "a rule allows only plain x.y.z chart versions, never a _sha- build" \
    '/^\d+\.\d+\.\d+$/' "$(jq -r '[.[] | select(.allowedVersions != null)][0].allowedVersions' <<<"$rules")"
  assert_eq "the chart rule groupName is kvm-node-agent chart" \
    "kvm-node-agent chart" "$(jq -r '[.[] | select(.groupName != null)][0].groupName' <<<"$rules")"
}

# --- Test 6: tag and digest describe the same upstream artifact ---
#
# Skips rather than fails when the registry cannot be reached: an offline
# workstation or a rate-limited runner must not turn the suite red.
test_tag_and_digest_agree_upstream() {
  echo "Test: the kvm-node-agent ref.tag and ref.digest resolve to the same upstream artifact"

  if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: curl or jq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local tag digest registry repository
  tag="$(kna_ref tag)"
  digest="$(kna_ref digest)"
  registry="${CHART_PACKAGE%%/*}"
  repository="${CHART_PACKAGE#*/}"

  # GHCR serves public packages to an anonymous pull token.
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
    "https://${registry}/v2/${repository}/manifests/${tag}" 2>/dev/null |
    tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" { print $2 }' | head -1)"
  if [ -z "$resolved" ]; then
    echo "  SKIP: ${registry} did not resolve tag ${tag} (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  assert_eq "ref.tag ${tag} resolves to the pinned ref.digest" "$digest" "$resolved"
}

test_source_is_digest_pinned_ocirepository
test_release_consumes_the_pinned_source
test_custom_manager_tracks_the_chart
test_regex_captures_tag_and_digest_together
test_package_rule
test_tag_and_digest_agree_upstream

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
