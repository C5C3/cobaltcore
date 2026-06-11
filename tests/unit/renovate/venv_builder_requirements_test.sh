#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the venv-builder base-package pins are covered by Renovate.
#
# The pins live in images/venv-builder/requirements.txt and are tracked by
# Renovate's native pip_requirements manager (no customManager needed). This
# test guards the coverage the way tests/unit/renovate/*_custommanager_test.sh
# guard the regex managers: the requirements file must pin every base package,
# and packageRules must gate major bumps and automerge minor/patch — matching
# the conservative posture applied to every other tracked pin in the repo.
#
# Usage: bash tests/unit/renovate/venv_builder_requirements_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"
REQUIREMENTS="$PROJECT_ROOT/images/venv-builder/requirements.txt"
REQUIREMENTS_GLOB="images/venv-builder/requirements.txt"

test_requirements_file_pins_base_packages() {
  echo "Test: images/venv-builder/requirements.txt pins every base package"

  if [[ ! -f "$REQUIREMENTS" ]]; then
    echo "  FAIL: $REQUIREMENTS missing"
    FAIL=$((FAIL + 1))
    return
  fi

  # Every non-comment, non-blank line names a base package and must pin it to
  # an exact version (name==x.y.z). Parsing the package names from the file
  # rather than hard-coding them keeps a single source of truth: a new or
  # removed base package only changes requirements.txt, and an unpinned entry
  # (e.g. "requests>=2.0" or a bare "requests") fails here.
  local line name checked=0
  while IFS= read -r line; do
    # Strip inline comments and surrounding whitespace, then skip blanks.
    line="${line%%#*}"
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "$line" ]] && continue

    checked=$((checked + 1))
    if [[ "$line" =~ ^[A-Za-z0-9._-]+==[0-9] ]]; then
      name="${line%%==*}"
      echo "  PASS: ${name} is pinned to an exact version"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: '${line}' is not an exact pin (name==x.y.z) in requirements.txt"
      FAIL=$((FAIL + 1))
    fi
  done < "$REQUIREMENTS"

  if [[ "$checked" -eq 0 ]]; then
    echo "  FAIL: no base packages found in $REQUIREMENTS"
    FAIL=$((FAIL + 1))
  fi
}

test_package_rule_disables_majors() {
  echo "Test: packageRule disables majors for the venv-builder requirements file"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed"
    SKIP=$((SKIP + 1))
    return
  fi

  local major_rule
  major_rule="$(jq -c --arg f "$REQUIREMENTS_GLOB" '.packageRules[]
    | select(
        ((.matchManagers // []) | index("pip_requirements")) != null
        and ((.matchFileNames // []) | index($f)) != null
        and ((.matchUpdateTypes // []) | index("major")) != null
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no pip_requirements packageRule for $REQUIREMENTS_GLOB disables majors"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "major venv-builder base-package updates are disabled" \
    "false" \
    "$(jq -r '.enabled' <<<"$major_rule")"
}

test_package_rule_automerges_minor_patch() {
  echo "Test: packageRule automerges minor/patch for the venv-builder requirements file"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local minor_rule
  minor_rule="$(jq -c --arg f "$REQUIREMENTS_GLOB" '.packageRules[]
    | select(
        ((.matchManagers // []) | index("pip_requirements")) != null
        and ((.matchFileNames // []) | index($f)) != null
        and ((.matchUpdateTypes // []) | index("minor")) != null
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no pip_requirements packageRule for $REQUIREMENTS_GLOB covers minor/patch"
    FAIL=$((FAIL + 2))
    return
  fi

  assert_eq "minor/patch venv-builder base-package updates are automerged" \
    "true" \
    "$(jq -r '.automerge' <<<"$minor_rule")"

  assert_eq "minor/patch venv-builder base-package updates have a release-age gate" \
    "3 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
}

test_requirements_file_pins_base_packages
test_package_rule_disables_majors
test_package_rule_automerges_minor_patch

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
