#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the mod_auth_openidc of the
# keystone-federation-proxy image as one pin in
# images/keystone-federation-proxy/source.json: the tag and the commit it
# names.
#
# Asserts:
#   - exactly one customManager targets OpenIDC/mod_auth_openidc over
#     images/keystone-federation-proxy/source.json with the github-tags
#     datasource and a regex versioning, which the pin needs because it
#     keeps the 'v' prefix of the upstream tag and upstream releases carry
#     a fourth version component (v2.4.20.3)
#   - one matchString captures "version" as currentValue and "rev" as
#     currentDigest, so Renovate rewrites both keys in the same PR. A split
#     pair would move the version while the build kept fetching the old
#     commit, and the AC_INIT guard of
#     images/keystone-federation-proxy/mod_auth_openidc.nix would abort that
#     build.
#   - that regex, replayed over the file on disk, captures the two values
#     the file carries, the captured version matches the versioning, and
#     the file carries the SRI hash of the fetched tree. The key order is
#     part of the contract: hack/nix-lock.sh writes version, rev and hash
#     in that order, and the regex joins the first two. Renovate leaves the
#     hash stale, so nix-lock-check fails its PR until `make nix-lock`
#     writes the new one.
#   - a paired packageRule disables majors, the way majors are disabled
#     for every custom-regex manager
#   - a paired packageRule holds minors and patches for 3 days and does
#     NOT automerge them: the module authenticates every federated login in
#     front of Keystone, and upstream releases change its behaviour inside
#     a patch (2.4.20 changed the key derivation of the session cookies), so
#     a reviewer reads upstream's ChangeLog
#   - a paired packageRule disables digest updates. The manager captures
#     currentDigest, so a tag moved upstream to another commit would
#     otherwise come in as a digest PR that no other rule matches: no
#     cooldown, no group, and a commit the build guard accepts because it
#     still carries the version. Disabled, the pin stays on the reviewed
#     commit and test 3 below fails on the moved tag instead.
#   - the pinned tag resolves at github.com to the pinned commit. This
#     skips instead of failing when the repository cannot be reached.
#
# Schema validation of renovate.json via `renovate-config-validator` is not
# run from this per-feature test; the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh runs it against the
# same renovate.json file.
#
# Usage: bash tests/unit/renovate/mod_auth_openidc_pin_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
PIN="$PROJECT_ROOT/images/keystone-federation-proxy/source.json"

PKG_NAME="OpenIDC/mod_auth_openidc"
PIN_PATH="images/keystone-federation-proxy/source.json"
REPO="https://github.com/OpenIDC/mod_auth_openidc"

# Reads the pinned tag out of source.json. The leading 'v' stays: it is part
# of the value the manager captures and of the ref it looks up.
pinned_version() {
  jq -r '.version // empty' "$PIN"
}

pinned_commit() {
  jq -r '.rev // empty' "$PIN"
}

# ls_remote_tag <refspec-suffix> prints the matching refs of the upstream
# repository, bounded by timeout(1) when it is installed.
ls_remote_tag() {
  if command -v timeout >/dev/null 2>&1; then
    timeout 30 git ls-remote --tags "$REPO" "refs/tags/$1" 2>/dev/null
  else
    git ls-remote --tags "$REPO" "refs/tags/$1" 2>/dev/null
  fi
}

# --- Test 1: the manager exists and its regex matches source.json ---
test_manager_exists_and_regex_matches() {
  echo "Test: the mod_auth_openidc pin has a customManager whose regex matches version and rev"

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
  if [[ ! -f "$PIN" ]]; then
    echo "  FAIL: $PIN missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local count
  count="$(jq '[.customManagers[]
    | select(any(.managerFilePatterns[]; test("images/keystone-federation-proxy/source")))] | length' \
    "$RENOVATE_FILE")"
  assert_eq "exactly one customManager matches ${PIN_PATH}" "1" "$count"

  local entry
  entry="$(jq -c '.customManagers[]
    | select(any(.managerFilePatterns[]; test("images/keystone-federation-proxy/source")))' \
    "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PIN_PATH}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "manager targets ${PIN_PATH}" \
    '/images/keystone-federation-proxy/source\.json$/' "$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")"
  assert_eq "manager names the package ${PKG_NAME}" \
    "$PKG_NAME" "$(jq -r '.packageNameTemplate' <<<"$entry")"
  assert_eq "manager uses the github-tags datasource (it resolves the tag to its commit)" \
    "github-tags" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_starts_with "manager uses a regex versioning (the pin keeps the upstream 'v')" \
    "$(jq -r '.versioningTemplate' <<<"$entry")" "regex:"

  # One matchString has to carry BOTH groups: Renovate rewrites the whole
  # matched span, so a split pair would bump the version and leave the
  # commit the build fetches behind.
  assert_eq "exactly one matchString captures both currentValue and currentDigest" \
    "1" \
    "$(jq '[.matchStrings[]
           | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))]
          | length' <<<"$entry")"

  local match_string versioning captured version
  match_string="$(jq -r '.matchStrings[]
    | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))' <<<"$entry" | head -1)"
  versioning="$(jq -r '.versioningTemplate | ltrimstr("regex:")' <<<"$entry")"

  # Replaying the regex over the file on disk is what ties the manager to
  # source.json. A key between "version" and "rev" builds the same image
  # and captures nothing here, because the regex joins the two keys.
  captured="$(REGEX="$match_string" FILE="$PIN" perl -e '
    my $re = $ENV{REGEX};
    local $/;
    open my $fh, "<", $ENV{FILE} or exit 1;
    my $content = <$fh>;
    if ($content =~ /$re/) {
      printf "%s|%s", $+{currentValue} // "", $+{currentDigest} // "";
    }
  ')"
  version="${captured%%|*}"

  # The expected values come from source.json, so a Renovate bump keeps
  # this test green. Never hard-code the version or the commit.
  assert_eq "regex captures version as currentValue" \
    "$(pinned_version)" "$version"
  assert_eq "regex captures rev as currentDigest" \
    "$(pinned_commit)" "${captured##*|}"

  # A currentValue the versioning cannot parse is a dependency Renovate
  # skips without a word.
  assert_eq "the versioning parses the captured version, the fourth component included" \
    "true" \
    "$(VERSIONING="$versioning" VALUE="$version" perl -e '
      print(($ENV{VALUE} ne "" && $ENV{VALUE} =~ /$ENV{VERSIONING}/) ? "true" : "false")')"
  assert_eq "source.json carries the SRI hash make nix-lock writes" "true" \
    "$(jq -r '.hash // "" | test("^sha256-[A-Za-z0-9+/]{43}=$")' "$PIN")"
}

# --- Test 2: the paired packageRules gate the bump ---
test_package_rules() {
  echo "Test: paired packageRules disable majors and digests and review minor and patch bumps"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local major_rule minor_rule digest_rule
  major_rule="$(jq -c --arg file "$PIN_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c --arg file "$PIN_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("patch")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  digest_rule="$(jq -c --arg file "$PIN_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("digest")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for ${PIN_PATH}"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major mod_auth_openidc updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  # A retag of the pinned version is not a release. Left enabled, it would
  # arrive as a digest update outside the rules below.
  if [ -z "$digest_rule" ]; then
    echo "  FAIL: no packageRule disabling digest updates for ${PIN_PATH}"
    FAIL=$((FAIL + 1))
  else
    assert_eq "digest-only mod_auth_openidc updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$digest_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor and patch mod_auth_openidc updates"
    FAIL=$((FAIL + 4))
    return
  fi

  # Not automerged on purpose: the module is the authentication in front of
  # Keystone, and a reviewer reads upstream's ChangeLog before it moves.
  assert_eq "minor and patch mod_auth_openidc updates are not automerged" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "the same rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "the same rule groups the bump as mod_auth_openidc releases" \
    "mod_auth_openidc releases" "$(jq -r '.groupName' <<<"$minor_rule")"
  assert_eq "the same rule covers minors next to patches" \
    "true" "$(jq '(.matchUpdateTypes | index("minor")) != null' <<<"$minor_rule")"
}

# --- Test 3: tag and commit describe the same upstream revision ---
#
# Test 1 is self-consistent by construction: it compares the file against
# itself, so it stays green when a hand-edit (a merge-conflict resolution
# on a Renovate PR, a bump made ahead of Renovate) advances the version
# and leaves the commit behind. The AC_INIT guard of the build would catch
# that pair, but only after fetching the tree, so this test settles it
# against the upstream repository first.
#
# Skips rather than fails when github.com cannot be reached: an offline
# workstation or a rate-limited runner must not turn the suite red.
test_tag_and_commit_agree_upstream() {
  echo "Test: the pinned tag resolves upstream to the pinned commit"

  if ! command -v git >/dev/null 2>&1; then
    echo "  SKIP: git not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi
  if [[ ! -f "$PIN" ]]; then
    echo "  FAIL: $PIN missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local tag commit
  tag="$(pinned_version)"
  commit="$(pinned_commit)"

  if [ -z "$tag" ] || [ -z "$commit" ]; then
    echo "  FAIL: $PIN_PATH carries no version / rev pair"
    FAIL=$((FAIL + 1))
    return
  fi

  # An annotated tag answers with the tag object on the plain
  # refs/tags/<tag> line, and only the peeled ^{} form names the commit the
  # build fetches. The plain form below is the fallback for a lightweight
  # tag, where the ref already is the commit; upstream's release tags are
  # lightweight today.
  local refs rc=0
  refs="$(ls_remote_tag "${tag}^{}")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "  SKIP: $REPO unreachable, cannot resolve $tag (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  if [ -z "$refs" ]; then
    rc=0
    refs="$(ls_remote_tag "$tag")" || rc=$?
    if [ "$rc" -ne 0 ]; then
      echo "  SKIP: $REPO unreachable, cannot resolve $tag (1 check skipped)"
      SKIP=$((SKIP + 1))
      return
    fi
  fi

  if [ -z "$refs" ]; then
    # The remote answered and has no such tag. That is the pin naming a
    # revision nobody can fetch, not a transport problem.
    echo "  FAIL: $REPO has no tag $tag"
    FAIL=$((FAIL + 1))
    return
  fi

  local resolved
  resolved="$(printf '%s\n' "$refs" | awk '{ print $1 }' | head -1)"

  assert_eq "tag ${tag} resolves to the pinned rev" \
    "$commit" "$resolved"
}

# --- Run ---
test_manager_exists_and_regex_matches
test_package_rules
test_tag_and_commit_agree_upstream

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
