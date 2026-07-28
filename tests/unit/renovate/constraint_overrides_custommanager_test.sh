#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json has the overrides/<release>/constraints.txt (PyPI)
# customManager:
#   - a customManagers entry targeting overrides/<release>/constraints.txt whose
#     matchStrings regex extracts (depName, version) per `package===x.y.z` pin
#     and leaves the `-package` removal lines alone
#   - datasource=pypi, versioning=pep440
#   - a paired packageRule set that disables majors and raises minor/patch as a
#     reviewed PR rather than automerging them
#
# The file is where a pin lands for a package upper-constraints.txt does not
# carry (lhafile, the LHA reader the image_decompression import plugin needs).
# Those pins reach the runtime service image, so an untracked one silently rots
# and a CVE against it cannot be answered from the tree.
#
# Usage: bash tests/unit/renovate/constraint_overrides_custommanager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

test_custom_manager_uses_pypi_datasource() {
  echo "Test: constraints.txt customManager uses datasource=pypi, versioning=pep440"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local entry
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("constraints"))' \
    "$RENOVATE_FILE")"

  if [ -z "$entry" ]; then
    echo "  FAIL: no customManagers entry targeting overrides/*/constraints.txt"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_eq "constraints customManager.datasourceTemplate is pypi" \
    "pypi" \
    "$(jq -r '.datasourceTemplate' <<<"$entry")"
  assert_eq "constraints customManager.versioningTemplate is pep440" \
    "pep440" \
    "$(jq -r '.versioningTemplate' <<<"$entry")"
  assert_eq "constraints customManager has a depName capture (no fixed depNameTemplate)" \
    "null" \
    "$(jq -r '.depNameTemplate // "null"' <<<"$entry")"
}

test_regex_captures_pins_and_skips_removals() {
  echo "Test: constraints.txt regex captures every === pin and skips -package lines"

  # EVERY release's overrides file is checked, not just the default release's:
  # overrides/<release>/constraints.txt exists precisely so releases can diverge,
  # so a pin shape the regex misses (a PEP 440 .postN, rcN or four-segment
  # version) could otherwise land in one release and go untracked while this test
  # stays green against another.
  local candidate files=0
  for candidate in "$PROJECT_ROOT"/overrides/*/constraints.txt; do
    [ -f "$candidate" ] || continue
    files=$((files + 1))
  done
  # Two per-file checks plus the two file-independent ones below.
  local checks=$((2 * files + 2))

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed ($checks checks skipped)"
    SKIP=$((SKIP + checks))
    return
  fi
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed ($checks checks skipped)"
    SKIP=$((SKIP + checks))
    return
  fi

  local entry match_string
  entry="$(jq -c '.customManagers[]
    | select((.managerFilePatterns // []) | join(",") | contains("constraints"))' \
    "$RENOVATE_FILE")"
  match_string="$(jq -r '.matchStrings[0]' <<<"$entry")"

  local overrides all_captured=""
  for overrides in "$PROJECT_ROOT"/overrides/*/constraints.txt; do
    [ -f "$overrides" ] || continue
    local rel captured pinned captured_count
    rel="${overrides#"$PROJECT_ROOT"/}"

    captured="$(REGEX="$match_string" FILE="$overrides" perl -e '
      my $re = $ENV{REGEX};
      local $/; open my $fh, "<", $ENV{FILE} or die $!;
      my $content = <$fh>;
      my @hits;
      while ($content =~ /$re/gm) { push @hits, $+{depName} . "=" . $+{currentValue}; }
      print join("\n", @hits);
    ')"
    all_captured="${all_captured}${captured}
"

    assert_not_contains "regex ignores the -package removal lines in $rel" \
      "$captured" "horizon="

    # Every pinned line in the file must be captured, so a future pin cannot be
    # added past the manager's reach without this test noticing.
    pinned="$(grep -cE '^[A-Za-z0-9._-]+===' "$overrides" || true)"
    captured_count="$(grep -c . <<<"$captured" || true)"
    assert_eq "every === pin in $rel is captured" \
      "$pinned" "$captured_count"
  done

  assert_gte "at least one overrides/*/constraints.txt was checked" "$files" 1
  assert_contains "regex captures the lhafile pin the service image installs" \
    "$all_captured" "lhafile="
}

test_package_rules_for_constraint_overrides() {
  echo "Test: packageRules disable major constraint bumps, review minor/patch"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local major_rule minor_rule
  major_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("overrides/**/constraints.txt")) != null
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c '.packageRules[]
    | select(
        ((.matchFileNames // []) | index("overrides/**/constraints.txt")) != null
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no packageRule scoping major updates for overrides/**/constraints.txt"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major constraint-override updates are disabled" \
      "false" \
      "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no packageRule scoping minor/patch updates for constraints.txt"
    FAIL=$((FAIL + 3))
    return
  fi

  # These pins land in the runtime service image and parse attacker-supplied
  # archive bytes, so the bump is a reviewed PR rather than an automerge.
  assert_eq "minor/patch constraint-override updates are not automerged" \
    "false" \
    "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch constraint-override rule waits minimumReleaseAge=7 days" \
    "7 days" \
    "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_contains "minor/patch constraint-override rule groupName mentions PyPI" \
    "$(jq -r '.groupName' <<<"$minor_rule")" \
    "PyPI"
}

test_custom_manager_uses_pypi_datasource
test_regex_captures_pins_and_skips_removals
test_package_rules_for_constraint_overrides

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
