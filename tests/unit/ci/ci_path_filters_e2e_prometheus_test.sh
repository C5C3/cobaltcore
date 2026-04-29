#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Path-filter completeness lint for the e2e-prometheus CI job (CC-0100, REQ-010).
#
# Why this test exists:
#   The dorny/paths-filter `e2e_prometheus` block in
#   .github/workflows/ci.yaml gates the e2e-prometheus job. The list is
#   human-maintained: a contributor who adds a new file under
#   operators/keystone/dashboards/, or renames the operator-chart
#   ServiceMonitor template, must remember to also update the filter, or
#   the CI lane silently stops running for that path. This test pins the
#   canonical set so a drift between the documented inputs to the
#   prometheus opt-in (deploy overlay, dashboard JSON, deploy script,
#   chart values surface, composite action, workflow file, Makefile
#   target) and the dorny filter list fails fast at the unit-test stage,
#   long before a pull request hits the e2e lane.
#
# What this test asserts:
#   1. The e2e_prometheus filter block exists in ci.yaml.
#   2. Every REQ-010-mandated path is present in the filter.
#   3. Every path the filter references resolves to an actual file or
#      directory in the repo (catches typos and stale entries).
#   4. The filter wiring is end-to-end: the FILTER_e2e_prometheus env
#      bridge in the Resolve effective changes step and the
#      e2e-prometheus output declaration are both present, and
#      hack/ci-resolve-changes.sh consumes the variable so a future
#      cleanup that removes one half cannot silently disable the lane.
#
# Implementation notes:
#   bash + tests/lib/assertions.sh, matching every other unit test under
#   tests/unit/hack/ and tests/unit/docs/. yq is intentionally NOT used:
#   the repo's existing unit-test harness does not assume yq is
#   installed, and a regex over a stable, hand-edited filter block is
#   sufficient to catch drift without adding a host-dependency.
#
# Usage: bash tests/unit/ci/ci_path_filters_e2e_prometheus_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"
RESOLVE_SH="$PROJECT_ROOT/hack/ci-resolve-changes.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# REQ-010 canonical path list (CC-0100).
#
# Every entry below MUST be in the e2e_prometheus filter block of
# .github/workflows/ci.yaml. Adding a new input to the prometheus opt-in
# (e.g. a sibling kind overlay file, a new dashboard JSON, a helper
# script that participates in the deploy flow) requires both updating
# this array AND the filter block — the test fails until they agree.
# ---------------------------------------------------------------------------
EXPECTED_PATHS=(
  'deploy/kind/prometheus/**'
  'tests/e2e/keystone/prometheus-stack/**'
  'hack/deploy-infra.sh'
  'operators/keystone/helm/keystone-operator/templates/servicemonitor.yaml'
  'operators/keystone/helm/keystone-operator/values.yaml'
  'operators/keystone/dashboards/keystone-operator.json'
  '.github/actions/setup-e2e-infra/action.yaml'
  '.github/workflows/ci.yaml'
  'Makefile'
)

# extract_filter_block — print the YAML lines belonging to the
# e2e_prometheus filter block (between the `e2e_prometheus:` key and the
# next sibling key at the same indentation level).
#
# The filter block lives under spec.steps[…].with.filters in
# .github/workflows/ci.yaml at column 12 (`            e2e_prometheus:`).
# We extract the block by line-number arithmetic to avoid taking on a
# yq host dependency for a CI-pinned, hand-edited list.
extract_filter_block() {
  local start_line end_line
  # Match the exact `e2e_prometheus:` key with its expected indentation.
  start_line="$(grep -n '^            e2e_prometheus:$' "$CI_YAML" | head -1 | cut -d: -f1)"
  if [[ -z "${start_line}" ]]; then
    return 1
  fi
  # Find the next non-list, non-comment line at the same indentation
  # (12 spaces) that is NOT a continuation of the current block.
  # The list items use indentation 14+ (`              - 'path'`), so any
  # line at indentation ≤12 that is non-blank ends the block.
  end_line="$(awk -v start="${start_line}" '
    NR <= start { next }
    # Match a line that starts with up to 12 spaces, not 14+.
    /^            [^ ]/ { print NR; exit }
    /^[a-zA-Z]/ { print NR; exit }
  ' "$CI_YAML")"
  if [[ -z "${end_line}" ]]; then
    end_line=$(wc -l < "$CI_YAML")
  fi
  sed -n "${start_line},${end_line}p" "$CI_YAML"
}

# ---------------------------------------------------------------------------
# Test 1: filter block exists (CC-0100, REQ-010)
# ---------------------------------------------------------------------------
test_filter_block_exists() {
  echo "Test: e2e_prometheus filter block exists in ci.yaml (CC-0100, REQ-010)"

  local block
  block="$(extract_filter_block || true)"
  assert_not_empty "ci.yaml has an 'e2e_prometheus:' filter block" "$block"
}

# ---------------------------------------------------------------------------
# Test 2: every REQ-010 canonical path is present in the filter (CC-0100)
# ---------------------------------------------------------------------------
test_canonical_paths_in_filter() {
  echo "Test: every REQ-010 canonical path is in the filter (CC-0100, REQ-010)"

  local block
  block="$(extract_filter_block || true)"
  if [[ -z "${block}" ]]; then
    echo "  FAIL: cannot test canonical paths — filter block missing"
    FAIL=$((FAIL + 1))
    return
  fi

  local path
  for path in "${EXPECTED_PATHS[@]}"; do
    # The filter list quotes paths with single quotes:
    #   - 'deploy/kind/prometheus/**'
    if printf '%s' "$block" | grep -qF -- "- '${path}'"; then
      echo "  PASS: filter includes '${path}'"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: filter is missing '${path}' — add it to .github/workflows/ci.yaml e2e_prometheus block"
      FAIL=$((FAIL + 1))
    fi
  done
}

# ---------------------------------------------------------------------------
# Test 3: every path referenced in the filter resolves on disk (CC-0100, REQ-010)
# A typo or stale entry in the filter is a silent regression — the job
# would simply never trigger for the misspelled path. Catch it here.
# ---------------------------------------------------------------------------
test_filter_paths_exist_on_disk() {
  echo "Test: every filter path resolves to a real file or directory (CC-0100, REQ-010)"

  local block
  block="$(extract_filter_block || true)"
  if [[ -z "${block}" ]]; then
    echo "  FAIL: cannot test filter paths — block missing"
    FAIL=$((FAIL + 1))
    return
  fi

  # Extract single-quoted list values: lines like `              - 'path'`
  # The path may end in `/**` (directory glob) or be a literal file.
  local entries
  entries="$(printf '%s' "$block" \
    | grep -oE -- "- '[^']+'" \
    | sed -E "s/^- '//; s/'$//")"

  if [[ -z "${entries}" ]]; then
    echo "  FAIL: filter block has no list entries"
    FAIL=$((FAIL + 1))
    return
  fi

  local entry resolved
  while IFS= read -r entry; do
    [[ -z "${entry}" ]] && continue
    # Strip a trailing literal `/**` to a directory path; otherwise
    # treat as a literal file path. The suffix is quoted so bash treats
    # it as a literal — without quoting, `${entry%/**}` would match
    # `/<anything>` because `*` is a wildcard in parameter expansion
    # patterns.
    resolved="${entry%'/**'}"
    if [[ "${resolved}" != "${entry}" ]]; then
      if [[ -d "$PROJECT_ROOT/${resolved}" ]]; then
        echo "  PASS: directory '${resolved}/' exists"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: filter references missing directory '${resolved}/'"
        FAIL=$((FAIL + 1))
      fi
    else
      if [[ -e "$PROJECT_ROOT/${entry}" ]]; then
        echo "  PASS: file '${entry}' exists"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: filter references missing file '${entry}'"
        FAIL=$((FAIL + 1))
      fi
    fi
  done <<< "${entries}"
}

# ---------------------------------------------------------------------------
# Test 4: filter is wired end-to-end through the resolve step and outputs
# (CC-0100, REQ-010). A future cleanup that drops one half (env bridge,
# output declaration, or the resolve script consumer) silently disables
# the gate — pin both halves here.
# ---------------------------------------------------------------------------
test_filter_is_wired_end_to_end() {
  echo "Test: e2e_prometheus filter is wired end-to-end (CC-0100, REQ-010)"

  assert_file_contains \
    "ci.yaml declares e2e-prometheus as a changes-job output" \
    "$CI_YAML" \
    "e2e-prometheus: \${{ steps.result.outputs.e2e-prometheus }}"

  assert_file_contains \
    "ci.yaml threads FILTER_e2e_prometheus into the resolve step env" \
    "$CI_YAML" \
    'FILTER_e2e_prometheus: ${{ steps.filter.outputs.e2e_prometheus }}'

  if [[ ! -f "$RESOLVE_SH" ]]; then
    echo "  FAIL: $RESOLVE_SH does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains \
    "hack/ci-resolve-changes.sh consumes FILTER_e2e_prometheus" \
    "$RESOLVE_SH" \
    'FILTER_e2e_prometheus'

  assert_file_contains \
    "hack/ci-resolve-changes.sh emits e2e-prometheus to GITHUB_OUTPUT" \
    "$RESOLVE_SH" \
    'e2e-prometheus'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_filter_block_exists
test_canonical_paths_in_filter
test_filter_paths_exist_on_disk
test_filter_is_wired_end_to_end

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
