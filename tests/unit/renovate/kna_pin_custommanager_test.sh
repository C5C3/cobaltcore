#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the single ARG KNA_COMMIT pin in
# images/kvm-node-agent/Dockerfile as a digest of the upstream main branch,
# the way the K-ORC GitRepository commit is tracked.
#
# Asserts:
#   - exactly one customManager targets the Dockerfile, with datasource
#     git-refs, currentValue main and the upstream repository URL as its dep
#     and package name
#   - its matchStrings regex matches the checked-in Dockerfile exactly once
#     (the bare 'ARG KNA_COMMIT' re-declarations in the build stages carry no
#     value) and captures the commit hack/ci-resolve-kna-commit.sh resolves
#   - a paired digest packageRule does NOT automerge: a pin move can leave the
#     patch under images/kvm-node-agent/patches/ unappliable, and the lab's
#     chart reference in deploy/lab/metal-stack/hypervisor/sources.yaml has to
#     move with it. Every upstream main commit is a new digest, so the rule
#     batches them weekly behind a 3-day cooldown
#
# Schema validation of renovate.json via `renovate-config-validator`
# (which transitively pulls Renovate via npx) is intentionally NOT run
# from this per-feature test to keep local / CI loops fast. The validation
# is centralised in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh, which runs
# against the same renovate.json file.
#
# Usage: bash tests/unit/renovate/kna_pin_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
KNA_DOCKERFILE="$PROJECT_ROOT/images/kvm-node-agent/Dockerfile"
RESOLVER="$PROJECT_ROOT/hack/ci-resolve-kna-commit.sh"

PKG_NAME="https://github.com/cobaltcore-dev/kvm-node-agent"
DOCKERFILE_PATH="images/kvm-node-agent/Dockerfile"

test_manager_uses_git_refs() {
  echo "Test: the ARG KNA_COMMIT pin has a git-refs customManager on main"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local count
  count="$(jq --arg path "$DOCKERFILE_PATH" '[.customManagers[]
    | select(any((.managerFilePatterns // [])[]; contains($path)))] | length' "$RENOVATE_FILE")"
  assert_eq "exactly one customManager targets ${DOCKERFILE_PATH}" "1" "$count"

  local entry
  entry="$(jq -c --arg path "$DOCKERFILE_PATH" '.customManagers[]
    | select(any((.managerFilePatterns // [])[]; contains($path)))' "$RENOVATE_FILE" | head -1)"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManager for ${DOCKERFILE_PATH}"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "manager uses the git-refs datasource" \
    "git-refs" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "manager tracks the main branch" \
    "main" "$(jq -r '.currentValueTemplate' <<<"$entry")"
  assert_eq "manager depNameTemplate is the upstream repository URL" \
    "$PKG_NAME" "$(jq -r '.depNameTemplate' <<<"$entry")"
  assert_eq "manager packageNameTemplate is the upstream repository URL" \
    "$PKG_NAME" "$(jq -r '.packageNameTemplate' <<<"$entry")"
}

test_regex_matches_the_pin_once() {
  echo "Test: the manager's regex matches the checked-in pin exactly once"

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
  if [[ ! -f "$KNA_DOCKERFILE" ]]; then
    echo "  FAIL: $KNA_DOCKERFILE missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local match_string
  match_string="$(jq -r --arg path "$DOCKERFILE_PATH" '.customManagers[]
    | select(any((.managerFilePatterns // [])[]; contains($path)))
    | .matchStrings[0]' "$RENOVATE_FILE" | head -1)"

  if [ -z "$match_string" ] || [ "$match_string" = "null" ]; then
    echo "  FAIL: no matchStrings for ${DOCKERFILE_PATH}"
    FAIL=$((FAIL + 1))
    return
  fi

  # Renovate applies a matchString with its default matchStringsStrategy
  # "any", which takes every match, so a second match would be a second
  # dependency. The output is "<number of matches> <first currentDigest>".
  local result matches captured
  result="$(REGEX="$match_string" FILE="$KNA_DOCKERFILE" perl -e '
    my $re = $ENV{REGEX};
    local $/; open my $fh, "<", $ENV{FILE} or die $!;
    my $content = <$fh>;
    my ($n, $first) = (0, "");
    while ($content =~ /$re/g) { $n++; $first = $+{currentDigest} // "" if $n == 1; }
    print "$n $first";
  ')"
  matches="${result%% *}"
  captured="${result#* }"

  assert_eq "matchStrings regex matches the Dockerfile exactly once" "1" "$matches"

  if [[ ! -x "$RESOLVER" ]]; then
    echo "  FAIL: $RESOLVER missing or not executable"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "captured digest equals the commit hack/ci-resolve-kna-commit.sh resolves" \
    "$("$RESOLVER")" "$captured"
}

test_package_rule() {
  echo "Test: the paired digest packageRule batches pin moves weekly without automerge"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local rule
  rule="$(jq -c --arg file "$DOCKERFILE_PATH" --arg pkg "$PKG_NAME" '.packageRules[]
    | select(
        ((.matchFileNames // []) | index($file)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("digest")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$rule" ]; then
    echo "  FAIL: no digest packageRule for ${DOCKERFILE_PATH}"
    FAIL=$((FAIL + 4))
    return
  fi

  # Compared as a string against the literal "false": an absent key prints
  # "null" and a flipped one "true", and both fail.
  assert_eq "digest updates are not automerged (the patch and chart need a human)" \
    "false" "$(jq -r '.automerge' <<<"$rule")"
  assert_eq "digest rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$rule")"
  assert_eq "digest rule runs on the weekly schedule" \
    "before 6am on monday" "$(jq -r '.schedule[0]' <<<"$rule")"
  assert_eq "digest rule groupName is kvm-node-agent" \
    "kvm-node-agent" "$(jq -r '.groupName' <<<"$rule")"
}

test_manager_uses_git_refs
test_regex_matches_the_pin_once
test_package_rule

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
