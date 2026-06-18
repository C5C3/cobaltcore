#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify docs/quick-start.md Step 3 contracts:
#   - The default `make deploy-infra` flow does NOT mention chaos-mesh in
#     the `What happens` table or the `What gets deployed` snapshot.
#   - A `::: tip Enabling Chaos Mesh` block documents the
#     `WITH_CHAOS_MESH=true make deploy-infra` opt-in and links to
#     docs/reference/chaos-e2e-tests.md.
# - The opt-in block carries the feature ID for traceability.
#
#
#
# Usage: bash tests/unit/docs/quick_start_no_chaos_default_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

QUICK_START="$PROJECT_ROOT/docs/quick-start-extended.md"

if [[ ! -f "$QUICK_START" ]]; then
  echo "FAIL: $QUICK_START does not exist"
  exit 1
fi

# Extract the section between "## Step 3 — Deploy the infrastructure stack"
# and the next "## Step" so we can scope assertions to Step 3 only. The
# `::: tip Enabling Chaos Mesh` block lives inside Step 3 details.
extract_step3_block() {
  awk '
    /^## Step 3 / { in_block = 1; print; next }
    in_block && /^## Step / { exit }
    in_block { print }
  ' "$QUICK_START"
}

STEP3_BLOCK="$(extract_step3_block)"

# --- Test 1: Default `make deploy-infra` flow drops chaos-mesh ---
test_default_flow_table_has_no_chaos_mesh() {
  echo "Test: Step 3 'What happens' table does not mention chaos-mesh in the default flow"

  # The "What happens" table rows are lines starting with `| <step> |`.
  # Any of those rows mentioning chaos-mesh would mean chaos-mesh is part
  # of the default `make deploy-infra` walkthrough, which contradicts.
  local table_rows
  table_rows="$(printf '%s\n' "$STEP3_BLOCK" | grep -E '^\| [0-9]+[a-z]? \|' || true)"

  if [[ -z "$table_rows" ]]; then
    echo "  FAIL: could not locate any 'What happens' table rows in Step 3"
    FAIL=$((FAIL + 1))
    return
  fi

  if printf '%s\n' "$table_rows" | grep -qiE 'chaos[- ]mesh'; then
    echo "  FAIL: 'What happens' table mentions chaos-mesh in default flow"
    printf '%s\n' "$table_rows" | grep -iE 'chaos[- ]mesh' | sed 's/^/    > /'
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: no chaos-mesh in 'What happens' table"
    PASS=$((PASS + 1))
  fi
}

# --- Test 2: `What gets deployed` snapshot has no chaos-mesh row ---
test_snapshot_has_no_chaos_mesh_namespace() {
  echo "Test: 'What gets deployed' snapshot does not list a chaos-mesh namespace"

  # The snapshot is the indented block inside `::: details What gets deployed`.
  # Look for a row that places `chaos-mesh` in the namespace column. The
  # opt-in tip block below the snapshot legitimately uses the string
  # "chaos-mesh" in prose, so we anchor on the space-delimited row format
  # used by the snapshot ("namespace<spaces>workload<spaces>state").
  local snapshot_block
  snapshot_block="$(printf '%s\n' "$STEP3_BLOCK" | awk '
    /::: details What gets deployed/ { in_block = 1; next }
    in_block && /^:::$/ { exit }
    in_block { print }
  ')"

  if [[ -z "$snapshot_block" ]]; then
    echo "  FAIL: could not locate the 'What gets deployed' details block"
    FAIL=$((FAIL + 1))
    return
  fi

  if printf '%s\n' "$snapshot_block" | grep -qE '^chaos-mesh[[:space:]]'; then
    echo "  FAIL: snapshot lists a chaos-mesh namespace row"
    printf '%s\n' "$snapshot_block" | grep -E '^chaos-mesh[[:space:]]' | sed 's/^/    > /'
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: snapshot has no chaos-mesh namespace row"
    PASS=$((PASS + 1))
  fi
}

# --- Test 3: Opt-in tip block exists ---
test_optin_tip_block_present() {
  echo "Test: Step 3 contains a '::: tip Enabling Chaos Mesh' block"

  # VitePress/Markdown custom container syntax for tip blocks.
  if printf '%s\n' "$STEP3_BLOCK" | grep -qE '^::: tip Enabling Chaos Mesh'; then
    echo "  PASS: '::: tip Enabling Chaos Mesh' block found"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: missing '::: tip Enabling Chaos Mesh' block"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 4: Tip block documents WITH_CHAOS_MESH=true command ---
test_optin_tip_block_documents_command() {
  echo "Test: Tip block documents 'WITH_CHAOS_MESH=true make deploy-infra'"

  assert_file_contains \
    "WITH_CHAOS_MESH=true make deploy-infra is documented in quick-start.md" \
    "$QUICK_START" \
    'WITH_CHAOS_MESH=true make deploy-infra'
}

# --- Test 5: Tip block links to chaos-e2e-tests reference doc ---
test_optin_tip_block_links_to_chaos_e2e_reference() {
  echo "Test: Tip block links to docs/reference/chaos-e2e-tests.md"

  # Markdown link form is `(./reference/chaos-e2e-tests.md)` from quick-start.md
  # (sibling docs/ directory traversal). Accept any link that resolves to the
  # chaos-e2e-tests page so future link-style refactors do not break this
  # test unnecessarily.
  if printf '%s\n' "$STEP3_BLOCK" | grep -qE '\]\([^)]*chaos-e2e-tests\.md[^)]*\)'; then
    echo "  PASS: tip block links to chaos-e2e-tests.md"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: tip block missing link to chaos-e2e-tests.md"
    FAIL=$((FAIL + 1))
  fi
}

# --- Run ---
test_default_flow_table_has_no_chaos_mesh
test_snapshot_has_no_chaos_mesh_namespace
test_optin_tip_block_present
test_optin_tip_block_documents_command
test_optin_tip_block_links_to_chaos_e2e_reference

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
