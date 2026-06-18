#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the WITH_CHAOS_MESH opt-in flag is correctly threaded from the
# e2e-chaos workflow job through the setup-e2e-infra composite action into
# `make deploy-infra`. Pinning this contract guards
# against regressions where:
#   - the composite action drops the env-passthrough so the flag silently
#     evaporates and Chaos Mesh is never installed even when requested;
#   - the e2e-infra job (default Quick Start contract) starts setting the
#     flag and accidentally re-installs Chaos Mesh on every run;
#   - the e2e-chaos job loses its explicit opt-in and the chaos suite then
#     races a missing chaos-controller-manager.
#
# Implementation: bash + tests/lib/assertions.sh, placed under tests/ci/ next
# to the sibling verify_chaos_ci_pipeline.sh (which inspects the same workflow
# file) and verify_composite_actions.sh (which inspects the same composite-
# action directory). The repository has zero .bats files and no bats binary
# in CI, so introducing one would add an undeclared dependency.
#
# Usage: bash tests/ci/verify_with_chaos_mesh_threading.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

ACTION_YAML="$PROJECT_ROOT/.github/actions/setup-e2e-infra/action.yaml"
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

echo "=== WITH_CHAOS_MESH threading verification ==="
echo ""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Extract a YAML job section from a workflow file by job name.
# Mirrors the helper in verify_chaos_ci_pipeline.sh so both files can evolve
# independently without cross-importing.
extract_yaml_job_section() {
  local file="$1" job_name="$2"
  sed -n "/^  ${job_name}:/,/^  [a-zA-Z]/p" "$file"
}

# Extract the body of the named step within a job section, stopping at the
# next step boundary (`- name:` or `- uses:` at the same indentation) or end
# of section. This lets us scope env-block assertions to a single step.
extract_step_block() {
  local section="$1" step_name="$2"
  echo "$section" | awk -v target="$step_name" '
    BEGIN { in_step = 0 }
    # Step-start marker: "      - name: <something>"
    /^      - (name|uses):/ {
      if (in_step) { exit }
      # Match either "- name: target" or, when the step has no name, the
      # following uses: line will not match this branch and will be skipped.
      if ($0 ~ ("- name: " target "$") || $0 ~ ("- name: " target "[[:space:]]*$")) {
        in_step = 1
        print
        next
      }
    }
    in_step { print }
  '
}

# ---------------------------------------------------------------------------
# Test 1: composite action declares WITH_CHAOS_MESH in the deploy step env
#
# ---------------------------------------------------------------------------
test_action_threads_with_chaos_mesh_env() {
  echo "Test: setup-e2e-infra threads WITH_CHAOS_MESH into the deploy step"

  assert_file_contains \
    "action.yaml passes WITH_CHAOS_MESH from env context to deploy step" \
    "$ACTION_YAML" \
    'WITH_CHAOS_MESH: ${{ env.WITH_CHAOS_MESH }}'

  # The existing SKIP_KIND_CREATE entry must still be present alongside the
  # new WITH_CHAOS_MESH entry — they share the same env: block.
  assert_file_contains \
    "action.yaml still hard-codes SKIP_KIND_CREATE alongside the new entry" \
    "$ACTION_YAML" \
    'SKIP_KIND_CREATE: "true"'
}

# ---------------------------------------------------------------------------
# Test 2: composite action references so future readers can trace
# the WITH_CHAOS_MESH plumbing back to the originating feature (FEATURE_ID_REQUIRED rule)
# ---------------------------------------------------------------------------
test_action_references_cc_0097() {
  echo "Test: setup-e2e-infra references"

  assert_file_contains \
    "action.yaml documents the origin of WITH_CHAOS_MESH plumbing" \
    "$ACTION_YAML" \
    ""
}

# ---------------------------------------------------------------------------
# Test 3: e2e-chaos job opts in via env: WITH_CHAOS_MESH: "true" on the
# Setup E2E infrastructure step
# ---------------------------------------------------------------------------
test_e2e_chaos_sets_with_chaos_mesh_true() {
  echo "Test: e2e-chaos job sets WITH_CHAOS_MESH=true on Setup E2E infrastructure"

  local section step_block
  section="$(extract_yaml_job_section "$CI_YAML" "e2e-chaos")"
  step_block="$(extract_step_block "$section" "Setup E2E infrastructure")"

  assert_not_empty \
    "e2e-chaos has a Setup E2E infrastructure step" \
    "$step_block"

  assert_contains \
    "e2e-chaos Setup E2E infrastructure step sets WITH_CHAOS_MESH: \"true\"" \
    "$step_block" \
    'WITH_CHAOS_MESH: "true"'

  # Sanity-check: the same step still uses the composite action under test.
  assert_contains \
    "e2e-chaos Setup E2E infrastructure step uses ./.github/actions/setup-e2e-infra" \
    "$step_block" \
    "./.github/actions/setup-e2e-infra"
}

# ---------------------------------------------------------------------------
# Test 4: e2e-infra (default Quick Start contract) does NOT set
# WITH_CHAOS_MESH so it falls back to the script's `false` default and
# stays minimal
# ---------------------------------------------------------------------------
test_e2e_infra_does_not_opt_in() {
  echo "Test: e2e-infra job does NOT set WITH_CHAOS_MESH"

  local section step_block
  section="$(extract_yaml_job_section "$CI_YAML" "e2e-infra")"
  step_block="$(extract_step_block "$section" "Setup E2E infrastructure")"

  assert_not_empty \
    "e2e-infra has a Setup E2E infrastructure step" \
    "$step_block"

  assert_not_contains \
    "e2e-infra Setup E2E infrastructure step does NOT set WITH_CHAOS_MESH" \
    "$step_block" \
    "WITH_CHAOS_MESH"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_action_threads_with_chaos_mesh_env
echo ""
test_action_references_cc_0097
echo ""
test_e2e_chaos_sets_with_chaos_mesh_true
echo ""
test_e2e_infra_does_not_opt_in
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
