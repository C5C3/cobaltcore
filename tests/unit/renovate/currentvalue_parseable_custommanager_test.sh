#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify every customManagers entry in renovate.json captures a
# currentValue that its own versioningTemplate can parse.
#
# A versioningTemplate that cannot parse the value the entry's regex
# captures makes Renovate silently never offer updates for that pin: the
# manager still matches, the extraction still succeeds, and the version
# comparison is then dropped without an error. The flux-web chart entry
# declared `semver` while capturing the range `0.56.x` and went stale
# unnoticed until the pin was audited by hand.
#
# The guard replays each entry's matchStrings over the git-tracked files
# its managerFilePatterns match and validates every captured value
# against a per-versioning allow-table. Captures are aggregated per
# entry, not per file: several workflow-pin entries target all
# .github/workflows/*.yaml files while their version constant lives in
# only two of them.
#
# Schema validation of renovate.json via `renovate-config-validator`
# (which transitively pulls Renovate via npx) is intentionally NOT run
# from this per-feature test to keep local / CI loops fast: the
# validator fetches the Renovate package on every invocation and taking
# that hit once per feature touching renovate.json multiplies the cost
# linearly. The validation is centralised in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh,
# which runs against the same renovate.json file.
#
# Usage: bash tests/unit/renovate/currentvalue_parseable_custommanager_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

# NOTE renovate-config-validator schema check is run by the
# sibling tests/unit/renovate/fluxoperator_custommanager_test.sh.
# See the header of this file for the rationale behind not duplicating
# the npx-driven validation here.

# value_matches_shape <versioningTemplate> <value>
#
# Returns 0 when the value has a shape the versioning parses, 1 when it
# does not, and 2 when the versioning is not in the table. The shapes are
# deliberately narrower than the full versioning grammars: they only need
# to catch pins whose captured value the versioning drops.
value_matches_shape() {
  local template="$1" value="$2"
  local semver_shape='^v?[0-9]+\.[0-9]+\.[0-9]+$'

  case "$template" in
  semver | helm)
    if [[ "$value" =~ $semver_shape ]]; then return 0; fi
    return 1
    ;;
  npm)
    # npm additionally accepts x-ranges such as 0.56.x, which semver
    # rejects.
    if [[ "$value" =~ $semver_shape ]]; then return 0; fi
    if [[ "$value" =~ ^[0-9]+\.[0-9]+\.x$ ]]; then return 0; fi
    return 1
    ;;
  pep440)
    # A release identifier: no range operators, no wildcards.
    if [[ "$value" =~ ^[0-9][A-Za-z0-9.!+]*$ ]]; then return 0; fi
    return 1
    ;;
  docker)
    # Any non-empty tag without whitespace is a valid image reference.
    if [ -z "$value" ]; then return 1; fi
    if [[ "$value" =~ [[:space:]] ]]; then return 1; fi
    return 0
    ;;
  regex:*)
    # Regex versioning parses the values its declared pattern matches,
    # so the shape is the pattern itself. bash ERE cannot parse the
    # (?<major>...) named groups Renovate requires, hence perl.
    if RE="${template#regex:}" VALUE="$value" \
      perl -e 'exit(($ENV{VALUE} =~ /$ENV{RE}/) ? 0 : 1)'; then
      return 0
    fi
    return 1
    ;;
  *)
    # An unknown versioning is a failure at the call site, not a pass:
    # the table only grows when someone deliberately states which value
    # shape the new versioning accepts.
    return 2
    ;;
  esac
}

# --- Test 1: the allow-table itself, across every branch it offers ---
test_shape_table_self_check() {
  echo "Test: allow-table accepts, rejects and fails closed as its call sites expect"

  # versioning|value|expected return code of value_matches_shape.
  # 0 = the versioning parses the value, 1 = it does not, 2 = the
  # versioning is not in the table. The first two rows are the pair that
  # produced the original gap: the flux-web chart pin captured the range
  # 0.56.x while declaring semver. The last two rows pin the fail-closed
  # branch, which is the only thing making an untabled versioning a
  # finding instead of a silent pass.
  local -a cases=(
    "semver|0.56.x|1"
    "npm|0.56.x|0"
    "semver|v0.56.0|0"
    "helm|0.56.x|1"
    "pep440|1.0.0|0"
    "pep440|>=1.0|1"
    "docker|3.20|0"
    "docker||1"
    "regex:^v(?<major>\\d+)\\.(?<minor>\\d+)\\.(?<patch>\\d+)$|v26.03.2|0"
    "regex:^v(?<major>\\d+)\\.(?<minor>\\d+)\\.(?<patch>\\d+)$|26.03.2|1"
    "loose|1.0.0|2"
    "|1.0.0|2"
  )

  local row tmpl val want rc
  for row in "${cases[@]}"; do
    IFS='|' read -r tmpl val want <<<"$row"
    rc=0
    value_matches_shape "$tmpl" "$val" || rc=$?
    assert_eq "value_matches_shape('$tmpl', '$val') returns $want" "$want" "$rc"
  done
}

# --- Test 2: replay every entry's matchStrings over its own files and
#             validate each captured currentValue ---
test_every_current_value_parses() {
  echo "Test: every captured currentValue parses as the entry's versioningTemplate"

  local replayed=0 digest_only=0
  local entry

  # The git index does not change while the test runs, so enumerate the
  # tracked files once instead of once per managerFilePatterns entry.
  local all_tracked
  all_tracked="$(git -C "$PROJECT_ROOT" ls-files)"

  while IFS= read -r entry; do
    [ -n "$entry" ] || continue

    local label
    label="$(jq -r '.depNameTemplate // .managerFilePatterns[0]' <<<"$entry")"

    # Entries that pin a digest instead of a version capture no
    # currentValue and carry no versioningTemplate, so this check has to
    # run before the versioning lookup.
    if ! jq -e '[.matchStrings[] | select(contains("(?<currentValue>"))] | length > 0' \
      <<<"$entry" >/dev/null; then
      echo "  SKIP (no currentValue capture group): $label"
      SKIP=$((SKIP + 1))
      digest_only=$((digest_only + 1))
      continue
    fi

    local template
    template="$(jq -r '.versioningTemplate // ""' <<<"$entry")"

    # Probe the table once per entry so an unknown or missing versioning
    # is reported here instead of once per captured value.
    local rc=0
    value_matches_shape "$template" "1.0.0" || rc=$?
    if [ "$rc" -eq 2 ]; then
      echo "  FAIL: $label: unknown versioningTemplate '$template', add its value shape to value_matches_shape()"
      FAIL=$((FAIL + 1))
      continue
    fi

    # Resolve the entry's files: managerFilePatterns are slash-delimited
    # regexes, so strip the delimiters before matching git-tracked paths.
    local files="" missing_pattern="" pattern matched raw_pattern
    while IFS= read -r raw_pattern; do
      [ -n "$raw_pattern" ] || continue
      pattern="${raw_pattern#/}"
      pattern="${pattern%/}"
      matched="$(printf '%s\n' "$all_tracked" | { grep -E "$pattern" || true; })"
      if [ -z "$matched" ]; then
        missing_pattern="$raw_pattern"
        break
      fi
      files="$files$matched"$'\n'
    done < <(jq -r '.managerFilePatterns[]' <<<"$entry")

    if [ -n "$missing_pattern" ]; then
      echo "  FAIL: $label: managerFilePatterns entry $missing_pattern matches no git-tracked file"
      FAIL=$((FAIL + 1))
      continue
    fi

    local matched_files
    matched_files="$(printf '%s' "$files" | sort -u)"

    # Replay every matchStrings regex over every matched file and collect
    # the captures of the whole entry, tagged with their source file.
    # $entry is fixed for the whole file loop, so extract its matchStrings
    # once instead of once per matched file.
    local match_strings
    match_strings="$(jq -r '.matchStrings[]' <<<"$entry")"

    local values="" captured file match_string value count=0 perl_rc
    while IFS= read -r file; do
      [ -n "$file" ] || continue

      # git ls-files reports index entries; a tracked path can be absent
      # from the worktree (interrupted rebase, sparse checkout, git rm
      # --cached). Naming that here beats letting perl die mid-loop.
      if [ ! -r "$PROJECT_ROOT/$file" ]; then
        echo "  FAIL: $label: tracked file $file is not readable in the worktree"
        FAIL=$((FAIL + 1))
        continue
      fi

      while IFS= read -r match_string; do
        [ -n "$match_string" ] || continue
        # Renovate compiles matchStrings with RE2 and falls back to the JS
        # engine; perl rejects a few patterns both accept. Report that as a
        # finding on this entry instead of letting the die abort the run
        # under set -e, which would swallow every remaining entry.
        perl_rc=0
        captured="$(REGEX="$match_string" FILE="$PROJECT_ROOT/$file" perl -e '
          my $re = $ENV{REGEX};
          local $/;
          open my $fh, "<", $ENV{FILE} or exit 1;
          my $content = <$fh>;
          while ($content =~ /$re/g) {
            print "$+{currentValue}\n" if defined $+{currentValue};
          }
        ')" || perl_rc=$?
        if [ "$perl_rc" -ne 0 ]; then
          echo "  FAIL: $label: perl could not apply matchString '$match_string' over $file"
          FAIL=$((FAIL + 1))
          continue
        fi
        while IFS= read -r value; do
          [ -n "$value" ] || continue
          values="$values$file"$'\t'"$value"$'\n'
          count=$((count + 1))
        done <<<"$captured"
      done <<<"$match_strings"
    done <<<"$matched_files"

    if [ "$count" -eq 0 ]; then
      echo "  FAIL: $label: matchStrings captured no currentValue in $(printf '%s' "$matched_files" | tr '\n' ' ')"
      FAIL=$((FAIL + 1))
      continue
    fi

    local bad=0 source_file
    while IFS=$'\t' read -r source_file value; do
      [ -n "$value" ] || continue
      rc=0
      value_matches_shape "$template" "$value" || rc=$?
      if [ "$rc" -ne 0 ]; then
        echo "  FAIL: $label: captured currentValue '$value' in $source_file does not parse as $template"
        FAIL=$((FAIL + 1))
        bad=$((bad + 1))
      fi
    done <<<"$values"

    if [ "$bad" -eq 0 ]; then
      echo "  PASS: $label: $count captured value(s) parse as $template"
      PASS=$((PASS + 1))
    fi

    replayed=$((replayed + 1))
  done < <(jq -c '.customManagers[]' "$RENOVATE_FILE")

  # Exact counts, not a lower bound: the only path that reaches `continue`
  # without a FAIL is the digest-only classifier, so a misfiring probe
  # would otherwise move entries from replayed to skipped in bulk while a
  # single survivor kept the sweep green.
  local entry_count
  entry_count="$(jq '.customManagers | length' "$RENOVATE_FILE")"
  assert_eq "exactly one digest-only customManagers entry is skipped" \
    "1" "$digest_only"
  assert_eq "every other customManagers entry was replayed" \
    "$((entry_count - 1))" "$replayed"
}

# --- Run ---
if ! command -v jq >/dev/null 2>&1; then
  echo "  SKIP: jq not installed (entire test skipped)"
  SKIP=$((SKIP + 1))
  echo ""
  echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
  exit 0
fi

if ! command -v perl >/dev/null 2>&1; then
  echo "  SKIP: perl not installed (entire test skipped)"
  SKIP=$((SKIP + 1))
  echo ""
  echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
  exit 0
fi

test_shape_table_self_check
test_every_current_value_parses

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
