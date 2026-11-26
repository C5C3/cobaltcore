#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks bats-core, the runner of make test-shell, as
# one pin in the root Makefile: the tag BATS_VERSION and the commit it names,
# BATS_COMMIT.
#
# Asserts:
#   - exactly one customManager targets bats-core/bats-core over the
#     Makefile with the github-tags datasource and a regex versioning,
#     which the pin needs because it keeps the 'v' prefix of the upstream
#     tag
#   - one matchString captures BATS_VERSION as currentValue and
#     BATS_COMMIT as currentDigest, so Renovate rewrites both lines in the
#     same PR. A split pair would move the version while make install-bats
#     kept fetching the old commit, and its version check would fail every
#     run.
#   - that regex, replayed over the Makefile on disk, captures the two
#     values the file carries. The regex joins the two lines, so their
#     order and adjacency are part of the contract.
#   - a paired packageRule disables majors, the way majors are disabled
#     for every custom-regex manager
#   - a paired packageRule holds minors and patches for 3 days and does
#     NOT automerge them: test-shell downloads and executes the new bats,
#     so a bump must not approve itself into the test-shell job that runs
#     on main
#   - a paired packageRule disables digest updates. A tag moved upstream
#     to another commit is not a release, and the pin stays on the
#     reviewed commit; test 3 below fails on the moved tag instead.
#   - the pinned tag resolves at github.com to the pinned commit. This
#     skips instead of failing when the repository cannot be reached.
#
# Schema validation of renovate.json via `renovate-config-validator` is
# NOT run from this per-feature test, for the reason
# tests/unit/renovate/novnc_pin_custommanager_test.sh gives: it is
# centralised in tests/unit/renovate/fluxoperator_custommanager_test.sh,
# which runs against the same renovate.json file.
#
# Usage: bash tests/unit/renovate/bats_pin_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
MAKEFILE="$PROJECT_ROOT/Makefile"

PKG_NAME="bats-core/bats-core"
PIN_PATH="Makefile"
BATS_UPSTREAM="https://github.com/bats-core/bats-core"

# Reads the pinned tag out of the Makefile. The leading 'v' stays: it is
# part of the value the manager captures and of the ref it looks up.
pinned_version() {
  sed -n 's/^BATS_VERSION ?= //p' "$MAKEFILE"
}

pinned_commit() {
  sed -n 's/^BATS_COMMIT ?= //p' "$MAKEFILE"
}

# ls_remote_tag <refspec-suffix> prints the matching refs of the upstream
# repository, bounded by timeout(1) when it is installed.
ls_remote_tag() {
  if command -v timeout >/dev/null 2>&1; then
    timeout 30 git ls-remote --tags "$BATS_UPSTREAM" "refs/tags/$1" 2>/dev/null
  else
    git ls-remote --tags "$BATS_UPSTREAM" "refs/tags/$1" 2>/dev/null
  fi
}

# --- Test 1: the manager exists and its regex matches the Makefile ---
test_manager_exists_and_regex_matches() {
  echo "Test: the bats-core pin has a customManager whose regex matches BATS_VERSION and BATS_COMMIT"

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

  local count
  count="$(jq --arg pkg "$PKG_NAME" '[.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("Makefile")))] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets ${PKG_NAME} over the Makefile" "1" "$count"

  local entry
  entry="$(jq -c --arg pkg "$PKG_NAME" '.customManagers[]
    | select(.packageNameTemplate == $pkg)
    | select(any(.managerFilePatterns[]; test("Makefile")))' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "manager targets the root ${PIN_PATH} only" \
    '/^Makefile$/' "$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")"
  assert_eq "manager uses the github-tags datasource (it resolves the tag to its commit)" \
    "github-tags" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "manager names the dependency ${PKG_NAME}" \
    "$PKG_NAME" "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_starts_with "manager uses a regex versioning (the pin keeps the upstream 'v')" \
    "$(jq -r '.versioningTemplate' <<<"$entry")" "regex:"

  # One matchString has to carry BOTH groups: Renovate rewrites the whole
  # matched span, so a split pair would bump the version and leave the
  # commit install-bats fetches behind.
  assert_eq "exactly one matchString captures both currentValue and currentDigest" \
    "1" \
    "$(jq '[.matchStrings[]
           | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))]
          | length' <<<"$entry")"

  local match_string captured
  match_string="$(jq -r '.matchStrings[]
    | select(contains("(?<currentValue>") and contains("(?<currentDigest>"))' <<<"$entry" | head -1)"

  # Replaying the regex over the file on disk is what ties the manager to
  # the Makefile. A line between BATS_VERSION and BATS_COMMIT installs the
  # same bats and captures nothing here, because the regex joins the two.
  captured="$(REGEX="$match_string" FILE="$MAKEFILE" perl -e '
    my $re = $ENV{REGEX};
    local $/;
    open my $fh, "<", $ENV{FILE} or exit 1;
    my $content = <$fh>;
    if ($content =~ /$re/) {
      printf "%s|%s", $+{currentValue} // "", $+{currentDigest} // "";
    }
  ')"

  # The expected values come from the Makefile, so a Renovate bump keeps
  # this test green. Never hard-code the version or the commit.
  assert_not_empty "the Makefile carries a BATS_VERSION pin" "$(pinned_version)"
  assert_eq "regex captures BATS_VERSION as currentValue" \
    "$(pinned_version)" "${captured%%|*}"
  assert_eq "regex captures BATS_COMMIT as currentDigest" \
    "$(pinned_commit)" "${captured##*|}"
}

# --- Test 2: the paired packageRules gate the bump ---
test_package_rules() {
  echo "Test: paired packageRules disable majors and digests and review minor and patch bumps"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
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
    echo "  FAIL: no packageRule disabling majors for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
  else
    # tostring, not //: jq's alternative operator treats false as absent.
    assert_eq "major bats-core updates are disabled" \
      "false" "$(jq -r '.enabled | tostring' <<<"$major_rule")"
  fi

  if [ -z "$digest_rule" ]; then
    echo "  FAIL: no packageRule disabling digest updates for ${PKG_NAME}"
    FAIL=$((FAIL + 1))
  else
    assert_eq "digest-only bats-core updates are disabled" \
      "false" "$(jq -r '.enabled | tostring' <<<"$digest_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor and patch bats-core updates"
    FAIL=$((FAIL + 4))
  else
    assert_eq "minor and patch bats-core updates are not automerged" \
      "false" "$(jq -r '.automerge | tostring' <<<"$minor_rule")"
    assert_eq "the same rule waits minimumReleaseAge=3 days" \
      "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
    assert_eq "the same rule groups the bump as bats-core releases" \
      "bats-core releases" "$(jq -r '.groupName' <<<"$minor_rule")"
    assert_eq "the same rule covers minors next to patches" \
      "true" "$(jq '(.matchUpdateTypes | index("minor")) != null' <<<"$minor_rule")"
  fi

  # Rules later in the list win, so a rule that automerges this package
  # anywhere would undo the one above.
  assert_eq "no packageRule automerges ${PKG_NAME}" \
    "0" \
    "$(jq --arg pkg "$PKG_NAME" '[.packageRules[]
      | select(((.matchPackageNames // []) | index($pkg)) != null)
      | select(.automerge == true)] | length' "$RENOVATE_FILE")"
}

# --- Test 3: tag and commit describe the same upstream revision ---
#
# Test 1 compares the Makefile against itself, so it stays green when a
# hand-edit advances the version and leaves the commit behind. make
# install-bats would catch that pair, but only after fetching, so this test
# settles it against the upstream repository first.
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

  local tag commit
  tag="$(pinned_version)"
  commit="$(pinned_commit)"

  if [ -z "$tag" ] || [ -z "$commit" ]; then
    echo "  FAIL: $PIN_PATH carries no BATS_VERSION / BATS_COMMIT pair"
    FAIL=$((FAIL + 1))
    return
  fi

  # bats-core tags its releases with annotated tags, so only the peeled
  # ^{} form answers with the commit install-bats fetches. The plain form
  # below is the fallback for a lightweight tag, where the ref already is
  # the commit.
  local refs rc=0
  refs="$(ls_remote_tag "${tag}^{}")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "  SKIP: $BATS_UPSTREAM unreachable, cannot resolve $tag (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  if [ -z "$refs" ]; then
    rc=0
    refs="$(ls_remote_tag "$tag")" || rc=$?
    if [ "$rc" -ne 0 ]; then
      echo "  SKIP: $BATS_UPSTREAM unreachable, cannot resolve $tag (1 check skipped)"
      SKIP=$((SKIP + 1))
      return
    fi
  fi

  if [ -z "$refs" ]; then
    # The remote answered and has no such tag. That is the pin naming a
    # revision nobody can fetch, not a transport problem.
    echo "  FAIL: $BATS_UPSTREAM has no tag $tag"
    FAIL=$((FAIL + 1))
    return
  fi

  local resolved
  resolved="$(printf '%s\n' "$refs" | awk '{ print $1 }' | head -1)"

  assert_eq "tag ${tag} resolves to the pinned BATS_COMMIT" \
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
