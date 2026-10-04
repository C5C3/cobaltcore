#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json declares a customManagers entry that targets the
# ghcr.io/c5c3/libvirt pins of the metal-stack lab, plus the paired
# packageRules:
#   - the docker-datasource matchStrings regex captures the depName, the whole
#     keeper tag (currentValue) and the whole digest (currentDigest) of all
#     eight image: lines across the five manifests (2, 1, 1, 2, 2), and the
#     versioning is deb, which compares the -r<N> suffix and the Ubuntu
#     revision numerically;
#   - allowedVersions accepts the keeper shape <libvirt-package-version>-r<N>
#     that hack/ci-tag-libvirt-keeper.sh mints and rejects latest, a commit
#     SHA and a bare package version, which deb would also parse;
#   - majors are disabled, and minor, patch and digest bumps are grouped and
#     never automerged, without a minimumReleaseAge: this repository's own
#     main builds the image, and the bump is the review the pin exists for;
#   - the manager and all three rules name all five manifests, so one group
#     moves all eight lines.
#
# This is the regression test the check-renovate-coverage skill requires for
# every customManager; that skill's audit does not scan deploy/lab/, so this
# test is what keeps the manager honest. It does NOT run
# renovate-config-validator itself: the sibling
# fluxoperator_custommanager_test.sh already exercises that authoritative gate
# over the whole file.
#
# Usage: bash tests/unit/renovate/metal_stack_libvirt_image_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

LIBVIRT_PACKAGE="ghcr.io/c5c3/libvirt"

# Each manifest and the number of libvirt image: lines it carries.
MANIFESTS="deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml 2
deploy/lab/metal-stack/probe/nfs-module-load.yaml 1
deploy/lab/metal-stack/nfs/kustomization.yaml 1
deploy/lab/metal-stack/nfs/client-modules-daemonset.yaml 2
deploy/lab/metal-stack/chaos-mesh/modules-daemonset.yaml 2"

# libvirt_manager — the docker-datasource customManagers entry that targets
# the libvirt DaemonSet, compact JSON, or nothing.
libvirt_manager() {
  jq -c '.customManagers[]
    | select(.datasourceTemplate == "docker")
    | select((.managerFilePatterns // []) | join(",") | contains("deploy/lab/metal-stack/hypervisor/libvirt-daemonset"))' \
    "$RENOVATE_FILE" | head -1
}

# libvirt_rule <selector> — the first packageRule for the libvirt image that
# also matches the jq <selector>, compact JSON, or nothing.
libvirt_rule() {
  jq -c --arg p "$LIBVIRT_PACKAGE" ".packageRules[]
    | select(((.matchPackageNames // []) | index(\$p)) != null)
    | select($1)" "$RENOVATE_FILE" | head -1
}

# --- Test 1: the manager captures the whole pin of all eight lines ---
test_custom_manager_captures_every_pin() {
  echo "Test: customManagers regex captures depName, tag and digest of all eight libvirt image lines"

  if ! command -v jq >/dev/null 2>&1 || ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: jq or perl not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi

  local entry
  entry="$(libvirt_manager)"
  if [ -z "$entry" ]; then
    echo "  FAIL: no docker-datasource customManagers entry for the libvirt DaemonSet"
    FAIL=$((FAIL + 8))
    return
  fi

  assert_eq "customManagers.datasourceTemplate is docker" \
    "docker" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "customManagers.versioningTemplate is deb" \
    "deb" "$(jq -r '.versioningTemplate' <<<"$entry")"

  # Replayed over the whole file, comments included, as Renovate does: a
  # comment that named the pin would be captured too and fail the equality.
  # The captures are rebuilt into image: lines and compared with the file's
  # own lines, anchored on the key, so a capture cut short fails as well.
  local match_string path count lines captured
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
  while read -r path count; do
    lines="$( { grep -E '^[[:space:]]*image: ghcr\.io/c5c3/libvirt' "$PROJECT_ROOT/$path" || true; } |
      sed -E 's/^[[:space:]]*//')"
    captured="$(REGEX="$match_string" FILE="$PROJECT_ROOT/$path" perl -e '
      my $re = $ENV{REGEX};
      local $/;
      open my $fh, "<", $ENV{FILE} or exit 1;
      my $content = <$fh>;
      while ($content =~ /$re/g) {
        print "image: $+{depName}:$+{currentValue}\@$+{currentDigest}\n";
      }
    ')"
    if [ -n "$lines" ] && [ "$(grep -c . <<<"$lines")" -eq "$count" ] && [ "$captured" = "$lines" ]; then
      echo "  PASS: the regex captures the whole pin of the $count image line(s) of $path"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $path: expected $count image line(s) captured whole"
      echo "    lines:    $lines"
      echo "    captured: $captured"
      FAIL=$((FAIL + 1))
    fi
  done <<<"$MANIFESTS"

  # The eight lines name one reference, so one group bump moves them together.
  local all_lines
  all_lines="$(while read -r path count; do
    grep -hE '^[[:space:]]*image: ghcr\.io/c5c3/libvirt' "$PROJECT_ROOT/$path" || true
  done <<<"$MANIFESTS" | sed -E 's/^[[:space:]]*//' | sort -u)"
  assert_eq "the eight lines name one reference" "1" "$(grep -c . <<<"$all_lines")"
}

# --- Test 2: allowedVersions admits the keeper shape alone ---
test_allowed_versions() {
  echo "Test: allowedVersions accepts the keeper tag shape and rejects what deb would also offer"

  if ! command -v jq >/dev/null 2>&1 || ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: jq or perl not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi

  local rule allowed pinned_tag
  rule="$(libvirt_rule 'has("allowedVersions")')"
  if [ -z "$rule" ]; then
    echo "  FAIL: no packageRule sets allowedVersions for $LIBVIRT_PACKAGE"
    FAIL=$((FAIL + 7))
    return
  fi
  assert_eq "the allowedVersions rule applies to every update type" "false" \
    "$(jq -r 'has("matchUpdateTypes")' <<<"$rule")"

  # allowedVersions is a slash-delimited regex.
  allowed="$(jq -r '.allowedVersions' <<<"$rule")"
  allowed="${allowed#/}"
  allowed="${allowed%/}"
  pinned_tag="$(grep -hoE 'image: ghcr\.io/c5c3/libvirt:[^@[:space:]]+' \
    "$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml" | head -1 | sed 's/.*libvirt://')"

  local version want got
  for version in "$pinned_tag|0" "10.0.0-2ubuntu8.20-r12|0" "latest|1" \
    "$(printf '%.0sa' {1..40})|1" "10.0.0-2ubuntu8.19|1"; do
    want="${version##*|}"
    version="${version%|*}"
    got=0
    RE="$allowed" VALUE="$version" perl -e 'exit(($ENV{VALUE} =~ /$ENV{RE}/) ? 0 : 1)' || got=$?
    if [ "$want" = 0 ]; then
      assert_eq "allowedVersions accepts '$version'" "0" "$got"
    else
      assert_eq "allowedVersions rejects '$version'" "1" "$got"
    fi
  done
  assert_not_empty "the DaemonSet carries a pinned tag to test" "$pinned_tag"
}

# --- Test 3: majors off, the rest grouped and reviewed ---
test_update_rules() {
  echo "Test: packageRules disable major bumps and group minor/patch/digest without automerge"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local major_rule minor_rule
  major_rule="$(libvirt_rule '((.matchUpdateTypes // []) | index("major")) != null')"
  minor_rule="$(libvirt_rule '((.matchUpdateTypes // []) | index("minor")) != null')"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for $LIBVIRT_PACKAGE"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major libvirt image updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch/digest updates for $LIBVIRT_PACKAGE"
    FAIL=$((FAIL + 4))
    return
  fi
  assert_eq "the rule covers minor, patch and digest updates" \
    '["minor","patch","digest"]' "$(jq -c '.matchUpdateTypes' <<<"$minor_rule")"
  assert_eq "libvirt image updates are never automerged" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "the libvirt image updates are grouped" \
    "metal-stack lab libvirt image" "$(jq -r '.groupName' <<<"$minor_rule")"
  assert_eq "the rule sets no minimumReleaseAge" \
    "false" "$(jq -r 'has("minimumReleaseAge")' <<<"$minor_rule")"
}

# --- Test 4: every file in the manager and in every rule ---
test_every_file_is_covered() {
  echo "Test: the manager and all three packageRules name all five manifests"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (20 checks skipped)"
    SKIP=$((SKIP + 20))
    return
  fi

  local entry patterns rule_allowed rule_major rule_minor
  entry="$(libvirt_manager)"
  patterns="$(jq -r '(.managerFilePatterns // [])[]' <<<"${entry:-{\}}")"
  rule_allowed="$(libvirt_rule 'has("allowedVersions")')"
  rule_major="$(libvirt_rule '((.matchUpdateTypes // []) | index("major")) != null')"
  rule_minor="$(libvirt_rule '((.matchUpdateTypes // []) | index("minor")) != null')"

  local path count pattern matched name rule
  while read -r path count; do
    # managerFilePatterns are slash-delimited regexes.
    matched=0
    while IFS= read -r pattern; do
      [ -n "$pattern" ] || continue
      pattern="${pattern#/}"
      pattern="${pattern%/}"
      if grep -qE -- "$pattern" <<<"$path"; then
        matched=1
      fi
    done <<<"$patterns"
    assert_eq "managerFilePatterns matches $path" "1" "$matched"

    for name in allowed major minor; do
      case "$name" in
        allowed) rule="$rule_allowed" ;;
        major) rule="$rule_major" ;;
        minor) rule="$rule_minor" ;;
      esac
      assert_eq "the $name rule names $path" "true" \
        "$(jq -r --arg f "$path" '((.matchFileNames // []) | index($f)) != null' <<<"${rule:-{\}}")"
    done
  done <<<"$MANIFESTS"
}

# --- Run ---
test_custom_manager_captures_every_pin
test_allowed_versions
test_update_rules
test_every_file_is_covered

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
