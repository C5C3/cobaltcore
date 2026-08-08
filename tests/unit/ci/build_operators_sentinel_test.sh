#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the `Resolve build operators` step of the build-e2e-images job in
# .github/workflows/ci.yaml drops the `__none__` sentinel that
# hack/ci-resolve-changes.sh emits when no operator changed.
#
# build-e2e-images is the only consumer that reads the e2e-operators matrix
# without gating on has-e2e-operators — it also runs for the e2e-chaos,
# e2e-prometheus and e2e-controlplane triggers. On a PR that reaches it through
# one of those paths alone the matrix is {"operator":["__none__"]}, and an
# unfiltered sentinel lands in BUILD_OPERATORS, where the next step turns it
# into `make docker-build OPERATOR=__none__` against a Dockerfile path that does
# not exist.
#
# The step's shell body is extracted from ci.yaml and executed with the two
# `${{ needs.changes.outputs.* }}` expressions substituted the way Actions
# substitutes them, so this exercises the real snippet rather than a copy.
#
# Usage: bash tests/unit/ci/build_operators_sentinel_test.sh

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

# Run the extracted step with the supplied matrix JSON and e2e-controlplane
# flag, and echo the BUILD_OPERATORS value the step appends to GITHUB_ENV.
# The GHA expressions are replaced together with their surrounding quotes, so
# the substituted values are shell-expanded exactly as Actions inlines them.
run_resolve_step() {
  local matrix="$1" controlplane="$2"
  local tmp step env_file
  tmp=$(mktemp -d)
  step="$tmp/step.sh"
  env_file="$tmp/github_env"

  extract_resolve_step "$step"
  sed -i.bak \
    -e "s|'\${{ needs.changes.outputs.e2e-operators }}'|\"\$E2E_OPERATORS\"|" \
    -e "s|'\${{ needs.changes.outputs.e2e-controlplane }}'|\"\$E2E_CONTROLPLANE\"|" \
    "$step"

  E2E_OPERATORS="$matrix" E2E_CONTROLPLANE="$controlplane" GITHUB_ENV="$env_file" \
    bash "$step" >/dev/null 2>&1

  # BUILD_OPERATORS is written as a heredoc block; collapse it to one line.
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

test_sentinel_matrix_yields_only_the_fixed_union() {
  echo "Test: the __none__ sentinel never reaches BUILD_OPERATORS"

  # What ci-resolve-changes.sh emits when no operator changed and the job is
  # reached through the e2e-chaos / e2e-prometheus / e2e-controlplane triggers.
  local ops
  ops=$(run_resolve_step '{"operator":["__none__"]}' 'false')

  assert_not_contains "sentinel is filtered out of the build union" "$ops" "__none__"
  assert_eq "only the fixed keystone/glance/placement union is built" \
    "keystone glance placement barbican" "$ops"
}

test_changed_operators_still_union_with_the_fixed_set() {
  echo "Test: a real operator matrix still unions with keystone/glance/placement"

  local ops
  ops=$(run_resolve_step '{"operator":["keystone","horizon"]}' 'false')

  assert_eq "changed operators are preserved and deduplicated" \
    "keystone horizon glance placement barbican" "$ops"
}

test_controlplane_extras_are_appended() {
  echo "Test: e2e-controlplane adds c5c3 and horizon on the sentinel path"

  local ops
  ops=$(run_resolve_step '{"operator":["__none__"]}' 'true')

  assert_not_contains "sentinel is filtered on the controlplane path too" "$ops" "__none__"
  assert_eq "controlplane extras join the fixed union" \
    "keystone glance placement barbican c5c3 horizon" "$ops"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_extraction_finds_the_step
test_sentinel_matrix_yields_only_the_fixed_union
test_changed_operators_still_union_with_the_fixed_set
test_controlplane_extras_are_appended

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
