#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `Resolve build operators` step of the build-e2e-images job in
# .github/workflows/ci.yaml turns the resolver's build-operators output into the
# BUILD_OPERATORS variable the build and push steps loop over.
#
# The step used to compute the union itself: it stripped the __none__ sentinel
# out of the e2e-operators matrix and added keystone, glance, placement and
# barbican unconditionally, plus c5c3 and horizon behind an inline read of the
# e2e-controlplane output. That union now lives in hack/ci-resolve-changes.sh,
# where it is derived from the jobs the run actually scheduled and pinned by
# tests/unit/ci/resolve_changes_scenarios_test.sh. What is left here is the
# translation, and the two properties the build steps depend on: an operator
# name per line, and nothing else on the line.
#
# The step's shell body is extracted from ci.yaml and executed with the
# `${{ needs.changes.outputs.* }}` expression supplied through the step's own
# env block, so this exercises the real snippet rather than a copy.
#
# Usage: bash tests/unit/ci/build_operators_wiring_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Extract the `run:` body of the `Resolve build operators` step into a runnable
# script at $1. Step bodies sit at 10-space indent, so the first non-blank line
# with less indentation ends the block; the indent is then stripped.
extract_resolve_step() {
  local out="$1"
  awk '
    /^      - name: Resolve build operators$/ { in_step = 1; next }
    in_step && /^        run: \|$/            { in_run = 1; next }
    in_run {
      if ($0 == "") { print ""; next }
      if ($0 !~ /^          /) { exit }
      print substr($0, 11)
    }
  ' "$CI_YAML" >"$out"
}

# Run the extracted step with the supplied build-operators JSON and echo the
# BUILD_OPERATORS value it appends to GITHUB_ENV, collapsed to one line.
run_resolve_step() {
  local build_operators="$1"
  local tmp step env_file
  tmp=$(mktemp -d)
  step="$tmp/step.sh"
  env_file="$tmp/github_env"

  extract_resolve_step "$step"
  BUILD_OPERATORS_JSON="$build_operators" GITHUB_ENV="$env_file" \
    bash "$step" >/dev/null 2>&1

  sed -n '/^BUILD_OPERATORS<<EOF$/,/^EOF$/p' "$env_file" |
    sed '1d;$d' | tr '\n' ' ' | tr -s ' ' | sed 's/^ *//;s/ *$//'
  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_extraction_finds_the_step() {
  echo "Test: the Resolve build operators body is extractable from ci.yaml"

  local tmp
  tmp=$(mktemp)
  extract_resolve_step "$tmp"
  assert_file_contains "extracted body assembles BUILD_OPERATORS" "$tmp" \
    'BUILD_OPERATORS<<EOF'
  rm -f "$tmp"
}

test_step_reads_the_resolver_output() {
  echo "Test: the step takes its list from needs.changes.outputs.build-operators"

  # An inline `${{ ... }}` in the run body would be substituted by Actions
  # before bash ever sees it, which is how the old union smuggled workflow state
  # into the script. Routing it through env: keeps the body testable.
  local block
  block=$(awk '
    /^      - name: Resolve build operators$/ { in_step = 1 }
    in_step && /^      - name: Build base images$/ { exit }
    in_step { print }
  ' "$CI_YAML")

  assert_contains "the step reads the resolver output through env" \
    "$block" 'BUILD_OPERATORS_JSON: ${{ needs.changes.outputs.build-operators }}'
  assert_not_contains "the fixed keystone/glance/placement/barbican union is gone" \
    "$block" "for base in"
  assert_not_contains "the inline e2e-controlplane read is gone" \
    "$block" "for extra in"
  # The resolver never emits the sentinel in build-operators, so the step has no
  # reason to filter it. If it ever appears here again, the union has moved back.
  assert_not_contains "the step does not filter a sentinel it can no longer receive" \
    "$block" "__none__"
}

test_json_list_becomes_one_operator_per_line() {
  echo "Test: the JSON list is turned into one operator per line"

  assert_eq "several operators are preserved in order" \
    "keystone glance" "$(run_resolve_step '["keystone","glance"]')"
  assert_eq "a single operator works" \
    "glance" "$(run_resolve_step '["glance"]')"
}

test_empty_list_produces_no_build() {
  echo "Test: an empty list leaves BUILD_OPERATORS empty rather than malformed"

  # build-e2e-images is skipped when the resolver has nothing for it to build,
  # so this is a defensive case rather than one CI reaches. It must not put an
  # empty or literal line into the loop, which would run
  # `make docker-build OPERATOR=` against a Dockerfile path that does not exist.
  assert_eq "an empty list yields an empty variable" \
    "" "$(run_resolve_step '[]')"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_extraction_finds_the_step
test_step_reads_the_resolver_output
test_json_list_becomes_one_operator_per_line
test_empty_list_produces_no_build

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
