#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has customManagers for the four tool pins in
# hack/install-test-deps.sh CHAINSAW_VERSION, FLUX_VERSION,
# KIND_VERSION, KUBECTL_VERSION.
#
# For each, assert:
#   - a customManager exists with packageNameTemplate matching the expected
#     GitHub org/repo
#   - its matchStrings regex captures the current literal in the script
#   - a paired packageRule disables majors
#
# Usage: bash tests/unit/renovate/install_test_deps_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
INSTALL_FILE="$PROJECT_ROOT/hack/install-test-deps.sh"

# Map: tool_var → expected packageName
TOOLS=(
  "CHAINSAW_VERSION:kyverno/chainsaw"
  "FLUX_VERSION:fluxcd/flux2"
  "KIND_VERSION:kubernetes-sigs/kind"
  "KUBECTL_VERSION:kubernetes/kubernetes"
)

test_each_tool_has_manager_and_regex_matches() {
  echo "Test: every install-test-deps.sh pin has a customManager whose regex matches"

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
  if [[ ! -f "$INSTALL_FILE" ]]; then
    echo "  SKIP: $INSTALL_FILE missing"
    SKIP=$((SKIP + 1))
    return
  fi

  local pair tool_var pkg_name entry match_string captured
  for pair in "${TOOLS[@]}"; do
    tool_var="${pair%%:*}"
    pkg_name="${pair##*:}"

    entry="$(jq -c --arg pkg "$pkg_name" '.customManagers[]
      | select(.packageNameTemplate == $pkg
               and ((.managerFilePatterns // []) | join(",") | contains("install-test-deps")))' \
      "$RENOVATE_FILE")"

    if [ -z "$entry" ]; then
      echo "  FAIL: no customManager for ${tool_var} → ${pkg_name}"
      FAIL=$((FAIL + 1))
      continue
    fi

    match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"
    captured="$(REGEX="$match_string" FILE="$INSTALL_FILE" perl -e '
      my $re = $ENV{REGEX};
      local $/; open my $fh, "<", $ENV{FILE} or die $!;
      my $content = <$fh>;
      if ($content =~ /$re/) { print $+{currentValue} // ""; }
    ')"

    if [ -n "$captured" ]; then
      echo "  PASS: ${tool_var} → ${pkg_name} regex captures ${captured}"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: ${tool_var} → ${pkg_name} regex did not match install-test-deps.sh"
      FAIL=$((FAIL + 1))
    fi
  done
}

test_package_rule_disables_majors() {
  echo "Test: packageRule disables major updates for install-test-deps tools"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("hack/install-test-deps.sh")) != null
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("hack/install-test-deps.sh")) != null
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule disabling majors for install-test-deps.sh"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major install-test-deps updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule for minor/patch install-test-deps updates"
    FAIL=$((FAIL + 2))
    return
  fi

  assert_eq "minor/patch install-test-deps updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch install-test-deps rule waits minimumReleaseAge=3 days" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
}

test_each_tool_has_manager_and_regex_matches
test_package_rule_disables_majors

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
