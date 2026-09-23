#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json tracks the envtest Kubernetes minor pinned by
# ENVTEST_K8S_VERSION in the root Makefile.
#
# The pin is single-sourced: ci.yaml awk-reads it for the integration-test
# asset cache key, and hack/nix-devshell-hook.sh reads it for the devshell.
# setup-envtest resolves the assets from the envtest-vX.Y.Z releases of
# kubernetes-sigs/controller-tools, so that release list is the datasource: a
# Kubernetes minor becomes a candidate only once its envtest assets exist.
#
# Asserts:
#   - exactly one customManager targets ENVTEST_K8S_VERSION in the root
#     Makefile, on github-releases of kubernetes-sigs/controller-tools
#   - its matchString, replayed over the Makefile on disk, captures the pin
#   - its extractVersionTemplate turns envtest-v1.37.0 into 1.37 and drops
#     pre-releases and the controller-gen v0.x releases of the same repository
#   - its versioning accepts only <major>.<minor>, the shape of the pin
#   - the Go build tooling packageRules cover it: majors disabled, minor
#     updates grouped and automerged after the three-day soak
#
# Usage: bash tests/unit/renovate/envtest_k8s_version_custommanager_test.sh

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
PKG="kubernetes-sigs/controller-tools"

# envtest_manager — the customManager entry for the envtest pin, compact JSON.
envtest_manager() {
  jq -c '[.customManagers[]
    | select(.depNameTemplate == "envtest")
    | select(any(.managerFilePatterns[]; test("Makefile")))]' "$RENOVATE_FILE"
}

# js_regex_capture <regex> <group> <text> — replays a Renovate (JavaScript)
# regex with named groups through perl and prints the named group, or nothing.
js_regex_capture() {
  REGEX="$1" GROUP="$2" TEXT="$3" perl -e '
    my $re = $ENV{REGEX};
    if ($ENV{TEXT} =~ /$re/) { print $+{$ENV{GROUP}} // ""; }
  '
}

test_manager_shape() {
  echo "Test: one customManager tracks ENVTEST_K8S_VERSION in the root Makefile"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local managers entry
  managers="$(envtest_manager)"
  assert_eq "exactly one envtest customManager over the Makefile" \
    "1" "$(jq 'length' <<<"$managers")"
  entry="$(jq -c '.[0] // {}' <<<"$managers")"

  assert_eq "the manager reads github-releases" \
    "github-releases" "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "the manager looks up $PKG" \
    "$PKG" "$(jq -r '.packageNameTemplate' <<<"$entry")"
  assert_eq "the manager is scoped to the root Makefile" \
    "/^Makefile$/" "$(jq -r '.managerFilePatterns | join(",")' <<<"$entry")"
}

test_match_string_captures_the_pin() {
  echo "Test: the matchString captures the ENVTEST_K8S_VERSION literal in the Makefile"

  if ! command -v jq >/dev/null 2>&1 || ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: jq or perl not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local match_string captured expected
  match_string="$(jq -r '.[0].matchStrings[0] // ""' <<<"$(envtest_manager)")"
  captured="$(js_regex_capture "$match_string" currentValue "$(cat "$MAKEFILE")")"
  expected="$(grep -E '^ENVTEST_K8S_VERSION' "$MAKEFILE" | head -1 \
    | sed -E 's/.*\?=[[:space:]]*([0-9]+\.[0-9]+).*/\1/')"

  assert_not_empty "the Makefile carries an ENVTEST_K8S_VERSION pin" "$expected"
  assert_eq "the matchString captures the Makefile pin" "$expected" "$captured"
}

test_extract_version() {
  echo "Test: extractVersionTemplate maps envtest release tags to <major>.<minor>"

  if ! command -v jq >/dev/null 2>&1 || ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: jq or perl not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local extract versioning
  extract="$(jq -r '.[0].extractVersionTemplate // ""' <<<"$(envtest_manager)")"
  versioning="$(jq -r '.[0].versioningTemplate // ""' <<<"$(envtest_manager)")"

  assert_eq "envtest-v1.37.0 becomes 1.37" \
    "1.37" "$(js_regex_capture "$extract" version "envtest-v1.37.0")"
  assert_eq "envtest-v1.36.2 becomes 1.36" \
    "1.36" "$(js_regex_capture "$extract" version "envtest-v1.36.2")"
  assert_eq "a pre-release tag yields no version" \
    "" "$(js_regex_capture "$extract" version "envtest-v1.35.0-alpha.3")"
  assert_eq "a controller-gen release of the same repository yields no version" \
    "" "$(js_regex_capture "$extract" version "v0.21.0")"
  assert_eq "versioning accepts only <major>.<minor>" \
    'regex:^(?<major>\d+)\.(?<minor>\d+)$' "$versioning"
}

test_package_rules_cover_it() {
  echo "Test: the Go build tooling packageRules cover the envtest pin"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  # A rule covers the pin when it names the Makefile and the manager's
  # packageName (matchPackageNames matches packageName, not depName).
  local covering major_rule minor_rule
  covering='.packageRules[]
    | select(((.matchFileNames // []) | index("Makefile")) != null)
    | select(((.matchPackageNames // []) | index($pkg)) != null)'
  major_rule="$(jq -c --arg pkg "$PKG" "$covering
    | select(((.matchUpdateTypes // []) | index(\"major\")) != null)" "$RENOVATE_FILE" | head -1)"
  minor_rule="$(jq -c --arg pkg "$PKG" "$covering
    | select(((.matchUpdateTypes // []) | index(\"minor\")) != null)" "$RENOVATE_FILE" | head -1)"

  # tostring, not //: jq's alternative operator treats false as absent.
  assert_eq "major updates are disabled" "false" "$(jq -r '.enabled | tostring' <<<"${major_rule:-{\}}")"
  assert_eq "minor updates are automerged" "true" "$(jq -r '.automerge // "unset"' <<<"${minor_rule:-{\}}")"
  assert_eq "minor updates wait the three-day soak" "3 days" \
    "$(jq -r '.minimumReleaseAge // "unset"' <<<"${minor_rule:-{\}}")"
}

test_manager_shape
test_match_string_captures_the_pin
test_extract_version
test_package_rules_cover_it

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
