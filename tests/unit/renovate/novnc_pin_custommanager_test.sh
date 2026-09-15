#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the bundled noVNC assets of
# images/nova/Dockerfile as one pin: the tag and the commit it names.
#
# Asserts:
#   - exactly one customManager targets novnc/noVNC over
#     images/nova/Dockerfile with the github-tags datasource and a regex
#     versioning, which the pin needs because it keeps the 'v' prefix of
#     the upstream tag
#   - one matchString captures ARG NOVNC_VERSION as currentValue and
#     ARG NOVNC_COMMIT as currentDigest, so Renovate rewrites both lines
#     in the same PR. A split pair would move the version while the build
#     kept fetching the old commit, and the package.json check in the
#     Dockerfile would abort that build.
#   - that regex, replayed over the Dockerfile on disk, captures the two
#     values the file carries. The adjacency of the two ARG lines is part
#     of the contract: the regex joins them with [ \t]*\n, so a blank line
#     between them stops the match and Renovate offers no bump at all.
#     With \s*\n the engine would match across such a blank line, and the
#     pin would look tracked while the file had drifted out of the shape
#     Renovate rewrites.
#   - a paired packageRule disables majors, the way majors are disabled
#     for every custom-regex manager
#   - a paired packageRule holds minors and patches for 3 days and does
#     NOT automerge them: the console page is user-facing and no e2e suite
#     loads it before #1018, so a reviewer looks at the bumped assets
#   - a paired packageRule disables digest updates. The manager captures
#     currentDigest, so a tag moved upstream to another commit would
#     otherwise come in as a digest PR that no other rule matches: no
#     cooldown, no group, and a commit the build guard accepts because it
#     still carries the version. Disabled, the pin stays on the reviewed
#     commit and test 3 below fails on the moved tag instead.
#   - the pinned tag resolves at github.com to the pinned commit. This
#     skips instead of failing when the repository cannot be reached.
#
# The commit is Renovate-tracked here, unlike the ARG OVN_COMMIT and
# ARG OVS_COMMIT pins beside the OVN tag: the github-tags datasource
# resolves a tag to the commit it names and writes it as currentDigest,
# and a plain tag has no second gitlink (the ovs submodule) that only a
# reviewer can read out of a clone.
#
# Schema validation of renovate.json via `renovate-config-validator`
# (which transitively pulls Renovate via npx) is intentionally NOT run
# from this per-feature test to keep local / CI loops fast: the validator
# fetches the Renovate package on every invocation and taking that hit
# once per feature touching renovate.json multiplies the cost linearly.
# The validation is centralised in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh, which runs
# against the same renovate.json file.
#
# Usage: bash tests/unit/renovate/novnc_pin_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
NOVA_DOCKERFILE="$PROJECT_ROOT/images/nova/Dockerfile"

PKG_NAME="novnc/noVNC"
DOCKERFILE_PATH="images/nova/Dockerfile"
NOVNC_REPO="https://github.com/novnc/noVNC"

# Reads the pinned tag out of the Dockerfile. The leading 'v' stays: it is
# part of the value the manager captures and of the ref it looks up.
pinned_version() {
  grep '^ARG NOVNC_VERSION=' "$NOVA_DOCKERFILE" | head -1 | sed 's/^ARG NOVNC_VERSION=//'
}

pinned_commit() {
  grep '^ARG NOVNC_COMMIT=' "$NOVA_DOCKERFILE" | head -1 | sed 's/^ARG NOVNC_COMMIT=//'
}

# ls_remote_tag <refspec-suffix> prints the matching refs of the upstream
# repository, bounded by timeout(1) when it is installed.
ls_remote_tag() {
  if command -v timeout >/dev/null 2>&1; then
    timeout 30 git ls-remote --tags "$NOVNC_REPO" "refs/tags/$1" 2>/dev/null
  else
    git ls-remote --tags "$NOVNC_REPO" "refs/tags/$1" 2>/dev/null
  fi
}

# --- Test 1: the manager exists and its regex matches the Dockerfile ---
test_manager_exists_and_regex_matches() {
  echo "Test: the noVNC pin has a customManager whose regex matches both ARG lines"

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
  if [[ ! -f "$NOVA_DOCKERFILE" ]]; then
    echo "  FAIL: $NOVA_DOCKERFILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local count
  count="$(jq --arg pkg "$PKG_NAME" '[.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("images/nova/Dockerfile")))] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets ${PKG_NAME} over the Dockerfile" "1" "$count"

  local entry
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("images/nova/Dockerfile")))' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_contains "manager targets ${DOCKERFILE_PATH}" \
    "$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")" "$DOCKERFILE_PATH"
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

  local match_string captured
  match_string="$(jq -r '.matchStrings[]
    | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))' <<<"$entry" | head -1)"

  # Replaying the regex over the file on disk is what ties the manager to
  # the Dockerfile. A blank line inserted between the two ARG lines builds
  # the same image and captures nothing here, because the regex joins the
  # lines with [ \t]*\n.
  captured="$(REGEX="$match_string" FILE="$NOVA_DOCKERFILE" perl -e '
    my $re = $ENV{REGEX};
    local $/;
    open my $fh, "<", $ENV{FILE} or exit 1;
    my $content = <$fh>;
    if ($content =~ /$re/) {
      printf "%s|%s", $+{currentValue} // "", $+{currentDigest} // "";
    }
  ')"

  # The expected values come from the Dockerfile, so a Renovate bump keeps
  # this test green. Never hard-code the version or the commit.
  assert_eq "regex captures ARG NOVNC_VERSION as currentValue" \
    "$(pinned_version)" "${captured%%|*}"
  assert_eq "regex captures ARG NOVNC_COMMIT as currentDigest" \
    "$(pinned_commit)" "${captured##*|}"
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
  major_rule="$(jq -c --arg file "$DOCKERFILE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c --arg file "$DOCKERFILE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("patch")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  digest_rule="$(jq -c --arg file "$DOCKERFILE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("digest")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for ${DOCKERFILE_PATH}"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major noVNC updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  # A retag of the pinned version is not a release. Left enabled, it would
  # arrive as a digest update outside the rules below.
  if [ -z "$digest_rule" ]; then
    echo "  FAIL: no packageRule disabling digest updates for ${DOCKERFILE_PATH}"
    FAIL=$((FAIL + 1))
  else
    assert_eq "digest-only noVNC updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$digest_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor and patch noVNC updates"
    FAIL=$((FAIL + 4))
    return
  fi

  # Not automerged on purpose: the noVNC pages are what an operator opens
  # for an instance console, and no e2e suite loads them before #1018.
  assert_eq "minor and patch noVNC updates are not automerged" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "the same rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "the same rule groups the bump as noVNC releases" \
    "noVNC releases" "$(jq -r '.groupName' <<<"$minor_rule")"
  assert_eq "the same rule covers minors next to patches" \
    "true" "$(jq '(.matchUpdateTypes | index("minor")) != null' <<<"$minor_rule")"
}

# --- Test 3: tag and commit describe the same upstream revision ---
#
# Test 1 is self-consistent by construction: it compares the file against
# itself, so it stays green when a hand-edit (a merge-conflict resolution
# on a Renovate PR, a bump made ahead of Renovate) advances the version
# and leaves the commit behind. The build would catch that pair, but only
# after fetching the assets, so this test settles it against the upstream
# repository first.
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
  if [[ ! -f "$NOVA_DOCKERFILE" ]]; then
    echo "  FAIL: $NOVA_DOCKERFILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local tag commit
  tag="$(pinned_version)"
  commit="$(pinned_commit)"

  if [ -z "$tag" ] || [ -z "$commit" ]; then
    echo "  FAIL: $DOCKERFILE_PATH carries no ARG NOVNC_VERSION / ARG NOVNC_COMMIT pair"
    FAIL=$((FAIL + 1))
    return
  fi

  # git has no --max-time, so the wall-clock bound comes from timeout(1)
  # where it exists (it is not in the macOS base system).
  #
  # noVNC tags its releases with annotated tags, so the plain
  # refs/tags/<tag> line names the tag object, not the commit, and only
  # the peeled ^{} form answers with the revision the build fetches. That
  # is the path normally taken here; the plain form below is the fallback
  # for a lightweight tag, where the ref already is the commit.
  local refs rc=0
  refs="$(ls_remote_tag "${tag}^{}")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "  SKIP: $NOVNC_REPO unreachable, cannot resolve $tag (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  if [ -z "$refs" ]; then
    rc=0
    refs="$(ls_remote_tag "$tag")" || rc=$?
    if [ "$rc" -ne 0 ]; then
      echo "  SKIP: $NOVNC_REPO unreachable, cannot resolve $tag (1 check skipped)"
      SKIP=$((SKIP + 1))
      return
    fi
  fi

  if [ -z "$refs" ]; then
    # The remote answered and has no such tag. That is the pin naming a
    # revision nobody can fetch, not a transport problem.
    echo "  FAIL: $NOVNC_REPO has no tag $tag"
    FAIL=$((FAIL + 1))
    return
  fi

  local resolved
  resolved="$(printf '%s\n' "$refs" | awk '{ print $1 }' | head -1)"

  assert_eq "tag ${tag} resolves to the pinned ARG NOVNC_COMMIT" \
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
