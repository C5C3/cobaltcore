#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the csi-driver-nfs chart version pin in
# deploy/kind/nfs/release.yaml and pairs it with the expected packageRules.
#
# Unlike the other kind HelmReleases (chaos-mesh, metrics-server, dizzy), this
# one carries an exact version rather than a `>=x <y` range: a range lets Flux
# adopt a new chart on its next reconcile with no repo diff, and what this
# release installs is a privileged hostNetwork DaemonSet whose relied-on chart
# defaults (driver.name, attachRequired, fsGroupPolicy, kubeletDir) the release
# does not override. An exact pin makes every upgrade a reviewed diff -- and an
# untracked pin goes stale silently, so it needs a manager.
#
# Asserts:
#   - exactly one customManager targets the csi-driver-nfs chart over the
#     release manifest, and it resolves the same chart index Flux does: the
#     helm datasource with a registryUrlTemplate equal to the HelmRepository
#     url in deploy/kind/nfs/source.yaml. The driver's GitHub releases are a
#     different version stream from the chart versions in that index, so a
#     github-releases datasource would propose tags the index does not carry
#     (Flux then fails with "no chart version found") and miss chart releases
#     that have no matching tag
#   - its matchStrings regex captures the current pin from the real manifest
#     exactly once -- the file also carries an `interval:` and a nested
#     `chart:` block the regex must not reach -- and that capture equals the
#     literal spec.chart.spec.version
#   - a paired packageRule disables majors
#   - a paired packageRule proposes minor/patch after a 3-day cooldown but
#     never automerges, because the upgrade changes a privileged workload
#
# Usage: bash tests/unit/renovate/csi_driver_nfs_chart_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
RELEASE_FILE="$PROJECT_ROOT/deploy/kind/nfs/release.yaml"

PKG_NAME="csi-driver-nfs"
RELEASE_PATH="deploy/kind/nfs/release.yaml"
SOURCE_FILE="$PROJECT_ROOT/deploy/kind/nfs/source.yaml"

test_manager_exists_and_regex_matches() {
  echo "Test: the csi-driver-nfs chart pin has a customManager whose regex matches"

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
  if [[ ! -f "$RELEASE_FILE" ]]; then
    echo "  FAIL: $RELEASE_FILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local count
  count="$(jq --arg pkg "$PKG_NAME" '[.customManagers[]
    | select(.depNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("deploy/kind/nfs/release")))] | length' \
    "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets ${PKG_NAME} over the release manifest" \
    "1" "$count"

  local entry
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.depNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("deploy/kind/nfs/release")))' \
    "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "manager uses the helm datasource" \
    "helm" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "manager resolves helm versioning, like the sibling chart managers" \
    "helm" "$(jq -r '.versioningTemplate' <<<"$entry")"

  # Derived from the manifest, never hard-coded: the datasource must resolve
  # the very index Flux downloads the chart from. A registryUrlTemplate that
  # drifts from the HelmRepository url would track a different version stream.
  local source_url
  source_url="$(grep -oE '^  url: [^ ]+$' "$SOURCE_FILE" | awk '{print $2}' | head -1)"
  assert_not_empty "the HelmRepository declares a chart index url" "$source_url"
  assert_eq "manager resolves the same chart index the HelmRepository points at" \
    "$source_url" "$(jq -r '.registryUrlTemplate' <<<"$entry")"

  local match_string captures captured match_count expected_pin
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  captures="$(REGEX="$match_string" FILE="$RELEASE_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    while ($content =~ /$re/g) {
      print "$+{currentValue}\n" if defined $+{currentValue};
    }
  ')"
  captured="$(printf '%s\n' "$captures" | head -1)"
  match_count="$(printf '%s\n' "$captures" | grep -c . || true)"

  assert_not_empty "matchStrings regex captures the chart version pin" "$captured"
  # A looser regex would also reach the HelmRelease `interval:` neighbours or a
  # future second version key, handing Renovate a bogus dep to bump.
  assert_eq "matchStrings regex captures exactly one value in the manifest" \
    "1" "$match_count"

  # Derive the expected pin straight from the manifest so a Renovate bump keeps
  # this test green -- never hard-code the version.
  expected_pin="$(grep -oE '^      version: "[0-9]+\.[0-9]+\.[0-9]+"' "$RELEASE_FILE" \
    | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  assert_not_empty "the release pins an exact x.y.z chart version" "$expected_pin"
  assert_eq "captured pin equals the literal spec.chart.spec.version" \
    "$expected_pin" "$captured"
}

test_package_rules() {
  echo "Test: paired packageRules gate csi-driver-nfs major vs minor/patch updates"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c --arg path "$RELEASE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c --arg path "$RELEASE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for $RELEASE_PATH"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major csi-driver-nfs chart updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor/patch csi-driver-nfs chart updates"
    FAIL=$((FAIL + 2))
    return
  fi

  assert_eq "minor/patch chart updates are not automerged (privileged workload)" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch chart rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
}

test_manager_exists_and_regex_matches
test_package_rules

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
