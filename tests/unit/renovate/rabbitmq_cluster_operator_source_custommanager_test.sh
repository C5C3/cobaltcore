#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the RabbitMQ Cluster Operator source is delivered under a content pin,
# and that the pin is Renovate-tracked.
#
# Upstream publishes no cosign signature, so no Flux spec.verify policy can gate
# this GitRepository, and the operator it installs holds cluster-wide RBAC. The
# tag does not gate it either: a Git tag is a mutable ref, and re-pushing it
# would hand different bytes to that controller within one interval, with no Git
# change and no reviewer. Only ref.commit closes that. The tag rides alongside as
# the human-readable version and as Renovate's lookup handle.
#
# The second half is what keeps the pin from becoming a freeze: Renovate's native
# flux manager does not cover this repo (its default file pattern matches only
# gotk-components.yaml), so without an explicit customManager the operator would
# sit on the pinned commit forever with no update signal. The manager must
# capture the tag and the commit in ONE matchString so Renovate rewrites both in
# the same reviewed PR; capturing the tag alone would leave the commit behind and
# Flux would keep checking out the old tree.
#
# A hand-edit can still split the pair, and no file-local assertion can see it:
# the tag and the commit only disagree relative to the upstream repository. The
# last test therefore resolves the pinned tag at github.com and compares.
#
# Usage: bash tests/unit/renovate/rabbitmq_cluster_operator_source_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
SOURCE_FILE="$PROJECT_ROOT/deploy/flux-system/sources/rabbitmq-cluster-operator.yaml"

OPERATOR_PACKAGE="rabbitmq/cluster-operator"
OPERATOR_REPO="https://github.com/rabbitmq/cluster-operator"
SOURCE_PATH="deploy/flux-system/sources/rabbitmq-cluster-operator.yaml"

# Read the customManager entry once; every test that needs it re-reads through
# this helper so a missing entry is reported per test instead of aborting.
source_manager_entry() {
  jq -c --arg path "sources/rabbitmq-cluster-operator" '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains($path))' \
    "$RENOVATE_FILE" | head -1
}

# Reads the pinned ref.tag out of the source file. The comment block names the
# tag in prose, so the anchor is the key at the start of a line.
pinned_tag() {
  grep -E '^[[:space:]]*tag:' "$SOURCE_FILE" | head -1 \
    | sed -E 's/.*tag:[[:space:]]*"([^"]+)".*/\1/'
}

pinned_commit() {
  grep -E '^[[:space:]]*commit:' "$SOURCE_FILE" | head -1 \
    | sed -E 's/.*commit:[[:space:]]*"([^"]+)".*/\1/'
}

# ls_remote_tag <refspec-suffix> prints the matching refs of the upstream
# repository, bounded by timeout(1) when it is installed.
ls_remote_tag() {
  if command -v timeout >/dev/null 2>&1; then
    timeout 30 git ls-remote --tags "$OPERATOR_REPO" "refs/tags/$1" 2>/dev/null
  else
    git ls-remote --tags "$OPERATOR_REPO" "refs/tags/$1" 2>/dev/null
  fi
}

# --- Test 1: the source pins repository content, not a movable tag ---
test_source_is_commit_pinned_gitrepository() {
  echo "Test: the operator source is a commit-pinned GitRepository"

  assert_file_contains "source declares kind GitRepository" \
    "$SOURCE_FILE" "kind: GitRepository"
  assert_file_contains "source url addresses the upstream operator repository" \
    "$SOURCE_FILE" "url: $OPERATOR_REPO"

  # Flux's gogit client evaluates ref.commit before every other ref field, so
  # the commit line is the control: without it the mutable tag decides what
  # gets checked out.
  local commit_line
  commit_line="$(grep -E '^[[:space:]]*commit:[[:space:]]*"[a-f0-9]{40}"' "$SOURCE_FILE" | head -1)"
  assert_not_empty "source pins ref.commit to a full 40-hex commit" "$commit_line"

  # The tag is what Renovate looks up, so its shape is part of the contract:
  # a semver tag with the upstream v prefix.
  local tag_line
  tag_line="$(grep -E '^[[:space:]]*tag:[[:space:]]*"v[0-9]+\.[0-9]+\.[0-9]+"' "$SOURCE_FILE" | head -1)"
  assert_not_empty "source carries a quoted vX.Y.Z ref.tag beside the commit" "$tag_line"

  # The installer reads nothing outside config/, so the clone stays scoped and
  # the source artifact small.
  assert_file_contains "source scopes the clone to the config kustomize base" \
    "$SOURCE_FILE" '!/config'
}

# --- Test 2: a customManager tracks the pin against the upstream tags ---
test_custom_manager_tracks_the_source() {
  echo "Test: a customManager tracks the rabbitmq-cluster-operator source pin"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local entry
  entry="$(source_manager_entry)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting $SOURCE_PATH"
    FAIL=$((FAIL + 4))
    return
  fi

  assert_eq "source customManager.datasourceTemplate is github-tags" \
    "github-tags" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "source customManager.depNameTemplate is the upstream repository" \
    "$OPERATOR_PACKAGE" \
    "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_eq "source customManager.packageNameTemplate is the upstream repository" \
    "$OPERATOR_PACKAGE" \
    "$(jq -r '.packageNameTemplate' <<<"$entry")"
  assert_eq "source customManager.versioningTemplate is semver" \
    "semver" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"
}

# --- Test 3: the regex captures tag and commit together ---
test_regex_captures_tag_and_commit_together() {
  echo "Test: the customManager regex captures ref.tag and ref.commit in one match"

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
  entry="$(source_manager_entry)"
  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting $SOURCE_PATH"
    FAIL=$((FAIL + 3))
    return
  fi

  # One matchString must carry BOTH groups: Renovate rewrites the whole matched
  # span, so a split pair would update the tag and leave the commit stale.
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
  captured="$(REGEX="$match_string" FILE="$SOURCE_FILE" perl -e '
    my $re = $ENV{REGEX};
    local $/;
    open my $fh, "<", $ENV{FILE} or exit 1;
    my $content = <$fh>;
    if ($content =~ /$re/) {
      printf "%s|%s", $+{currentValue} // "", $+{currentDigest} // "";
    }
  ')"

  assert_eq "regex captures the ref.tag as currentValue" \
    "$(pinned_tag)" "${captured%%|*}"
  assert_eq "regex captures the ref.commit as currentDigest" \
    "$(pinned_commit)" "${captured##*|}"
}

# --- Test 4: the paired packageRule never automerges the source bump ---
test_package_rule_never_automerges() {
  echo "Test: the paired packageRule reviews source bumps instead of automerging"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local rules
  rules="$(jq -c --arg pkg "$OPERATOR_PACKAGE" --arg path "$SOURCE_PATH" '[.packageRules[]
    | select(
        (((.matchPackageNames // []) | index($pkg)) != null)
        or (((.matchFileNames // []) | index($path)) != null)
      )]' "$RENOVATE_FILE")"

  if [ "$(jq 'length' <<<"$rules")" = "0" ]; then
    echo "  FAIL: no packageRule scoping updates for $OPERATOR_PACKAGE"
    FAIL=$((FAIL + 3))
    return
  fi

  # An operator with cluster-wide RBAC never merges itself: every bump is a
  # human decision, so no matching rule may set automerge.
  assert_eq "no rule automerges the rabbitmq-cluster-operator source" \
    "0" \
    "$(jq '[.[] | select(.automerge == true)] | length' <<<"$rules")"
  assert_eq "a rule holds new operator releases for 3 days" \
    "1" \
    "$(jq '[.[] | select(.minimumReleaseAge == "3 days")] | length' <<<"$rules")"
  # The image rule in releases/ carries the same groupName, which is what puts
  # the tag, the commit, the image tag and the image digest in one PR.
  assert_eq "the source rule groupName is rabbitmq cluster-operator" \
    "rabbitmq cluster-operator" \
    "$(jq -r '[.[] | select(.groupName != null)][0].groupName' <<<"$rules")"
}

# --- Test 5: tag and commit describe the same upstream revision ---
#
# Tests 1 and 3 are self-consistent by construction: they compare the file
# against itself, so they stay green when a hand-edit (a merge-conflict
# resolution on a Renovate PR, a bump made ahead of Renovate) advances ref.tag
# and leaves ref.commit behind. Flux would not notice either, because ref.commit
# wins, so the tag is decoration and the cluster keeps running the old revision
# while every local signal reads as bumped. Only the upstream repository can
# settle whether the two agree, so this test resolves the tag and compares.
#
# Skips rather than fails when github.com cannot be reached: an offline
# workstation or a rate-limited runner must not turn the suite red.
test_tag_and_commit_agree_upstream() {
  echo "Test: ref.tag and ref.commit resolve to the same upstream revision"

  if ! command -v git >/dev/null 2>&1; then
    echo "  SKIP: git not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local tag commit
  tag="$(pinned_tag)"
  commit="$(pinned_commit)"

  # git has no --max-time, so the wall-clock bound comes from timeout(1) where
  # it exists (it is not in the macOS base system).
  local refs rc=0
  refs="$(ls_remote_tag "${tag}^{}")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "  SKIP: $OPERATOR_REPO unreachable, cannot resolve $tag (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # An annotated tag answers under the peeled ^{} form and points the ref at a
  # tag object; a lightweight tag answers only under the plain form, where the
  # ref already is the commit. Upstream uses lightweight tags today, so the
  # fallback is the path normally taken.
  if [ -z "$refs" ]; then
    rc=0
    refs="$(ls_remote_tag "$tag")" || rc=$?
    if [ "$rc" -ne 0 ]; then
      echo "  SKIP: $OPERATOR_REPO unreachable, cannot resolve $tag (1 check skipped)"
      SKIP=$((SKIP + 1))
      return
    fi
  fi

  if [ -z "$refs" ]; then
    # The remote answered and has no such tag. That is the pin pointing at a
    # revision nobody can fetch, not a transport problem.
    echo "  FAIL: $OPERATOR_REPO has no tag $tag"
    FAIL=$((FAIL + 1))
    return
  fi

  local resolved
  resolved="$(printf '%s\n' "$refs" | awk '{ print $1 }' | head -1)"

  assert_eq "ref.tag ${tag} resolves to the pinned ref.commit" \
    "$commit" "$resolved"
}

test_source_is_commit_pinned_gitrepository
test_custom_manager_tracks_the_source
test_regex_captures_tag_and_commit_together
test_package_rule_never_automerges
test_tag_and_commit_agree_upstream

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
