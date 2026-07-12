#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Enforce the guide↔suite coupling contract per guide under docs/guides/:
#
#   (a) a "## Prerequisites" heading exists;
#   (b) its section contains a "::: info Devstack" container;
#   (c) that container names exactly one Getting-Started tutorial
#       ("](../quick-start...") and holds at least one ```bash fence with the
#       devstack bring-up command;
#   (d) a "## Tested by" heading exists;
#   (e) the "## Tested by" section names at least one suite via
#       "chainsaw test --test-dir tests/...", and every referenced suite
#       directory resolves to a real chainsaw-test.yaml;
#   (f) every VitePress code-import ("<<< @/../...") in the guide resolves to a
#       file that exists, and when a "#region" is named the fixture carries the
#       matching "# region <name>" / "# endregion <name>" markers.
#
# VitePress does not fail the build on a dead snippet path, so (e) and (f) are
# the enforcing gate for the tested-by references and the code-import wiring.
# This check is purely structural (headings, containers, paths) — it makes no
# assertions on the guides' prose.
#
# Usage: bash tests/unit/docs/guide_devstack_and_tested_by_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

GUIDES_DIR="$PROJECT_ROOT/docs/guides"

# Body of the "## <title>" section: lines after the heading up to (but not
# including) the next level-2 heading or EOF.
section_body() {
  local file="$1" heading="$2"
  awk -v h="^## ${heading}[[:space:]]*\$" '
    $0 ~ h { f = 1; next }
    f && /^## / { exit }
    f { print }
  ' "$file"
}

# Body of the first "::: info Devstack" container, including the opening and
# closing fence lines, up to the first line that is exactly ":::".
devstack_container() {
  local file="$1"
  awk '
    /^::: info Devstack/ { f = 1; print; next }
    f && /^:::[[:space:]]*$/ { print; exit }
    f { print }
  ' "$file"
}

check_guide() {
  local file="$1"
  local name
  name="$(basename "$file")"
  echo "Guide: $name"

  # (a) Prerequisites heading.
  if ! grep -qE '^## Prerequisites[[:space:]]*$' "$file"; then
    echo "  FAIL: $name has no '## Prerequisites' heading"
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS: $name has a '## Prerequisites' heading"
  PASS=$((PASS + 1))

  local prereq devstack
  prereq="$(section_body "$file" "Prerequisites")"

  # (b) Devstack container inside Prerequisites.
  if [[ "$prereq" != *"::: info Devstack"* ]]; then
    echo "  FAIL: $name Prerequisites section has no '::: info Devstack' container"
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS: $name Prerequisites has a '::: info Devstack' container"
  PASS=$((PASS + 1))

  devstack="$(devstack_container "$file")"

  # (c1) Exactly one Getting-Started link in the devstack container. Count
  # occurrences (grep -o, one match per line) rather than matching lines, so a
  # second link on the same line cannot slip past.
  local link_count
  link_count="$(printf '%s\n' "$devstack" | grep -oE '\]\(\.\./quick-start' | wc -l | tr -d '[:space:]')"
  assert_eq "$name devstack names exactly one Getting-Started tutorial" \
    "1" "$link_count"

  # (c2) At least one bash bring-up fence in the devstack container.
  local bash_count
  bash_count="$(printf '%s\n' "$devstack" | grep -cE '^```bash')"
  assert_gte "$name devstack holds a bash bring-up fence" "$bash_count" "1"

  # (d) Tested by heading.
  if ! grep -qE '^## Tested by[[:space:]]*$' "$file"; then
    echo "  FAIL: $name has no '## Tested by' heading"
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS: $name has a '## Tested by' heading"
  PASS=$((PASS + 1))

  # (e) Tested by names at least one resolvable suite.
  local tested paths
  tested="$(section_body "$file" "Tested by")"
  paths="$(printf '%s\n' "$tested" \
    | grep -oE 'chainsaw test --test-dir tests/[^ )]+' \
    | awk '{print $4}' | sed 's:/*$::')"
  if [[ -z "$paths" ]]; then
    echo "  FAIL: $name '## Tested by' has no 'chainsaw test --test-dir tests/...' invocation"
    FAIL=$((FAIL + 1))
  else
    local p
    while IFS= read -r p; do
      [[ -z "$p" ]] && continue
      if [[ -f "$PROJECT_ROOT/$p/chainsaw-test.yaml" ]]; then
        echo "  PASS: $name tested-by suite $p resolves"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: $name tested-by suite $p has no chainsaw-test.yaml"
        FAIL=$((FAIL + 1))
      fi
    done <<< "$paths"
  fi

  # (f) Every code-import resolves (file + region markers).
  local spec importfile region
  while IFS= read -r spec; do
    [[ -z "$spec" ]] && continue
    spec="${spec#<<< @/../}"
    importfile="${spec%%#*}"
    if [[ ! -f "$PROJECT_ROOT/$importfile" ]]; then
      echo "  FAIL: $name code-import target $importfile does not exist"
      FAIL=$((FAIL + 1))
      continue
    fi
    if [[ "$spec" == *"#"* ]]; then
      region="${spec#*#}"
      if grep -qE "^# region ${region}\$" "$PROJECT_ROOT/$importfile" \
        && grep -qE "^# endregion ${region}\$" "$PROJECT_ROOT/$importfile"; then
        echo "  PASS: $name code-import $importfile#$region resolves with balanced markers"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: $name code-import $importfile#$region is missing '# region'/'# endregion $region' markers"
        FAIL=$((FAIL + 1))
      fi
    else
      echo "  PASS: $name code-import $importfile resolves (whole file)"
      PASS=$((PASS + 1))
    fi
  done < <(grep -E '^<<< @/\.\./' "$file")
}

# --- Run ---
if [[ ! -d "$GUIDES_DIR" ]]; then
  echo "FAIL: guides directory $GUIDES_DIR does not exist"
  exit 1
fi

shopt -s nullglob
guide_files=("$GUIDES_DIR"/*.md)
shopt -u nullglob

if [[ "${#guide_files[@]}" -eq 0 ]]; then
  echo "FAIL: no guides found under $GUIDES_DIR"
  exit 1
fi

for guide in "${guide_files[@]}"; do
  check_guide "$guide"
done

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
