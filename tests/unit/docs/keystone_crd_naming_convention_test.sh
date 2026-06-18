#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/keystone-crd.md "Sub-Resource Naming Convention"
# section shipped with:
#   1. The section heading "## Sub-Resource Naming Convention"
#      exists.
#   2. The section asserts the new convention (no `-api` suffix) — checks
#      for the bare `keystone.openstack.svc.cluster.local` Service DNS
#      example.
#   3. The section cross-links to the upgrade-flow reference for migration
#      semantics.
#
# Usage: bash tests/unit/docs/keystone_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/keystone/keystone-crd.md"

# --- Test 1: heading exists ---
test_heading_exists() {
  echo "Test: '## Sub-Resource Naming Convention' heading exists"

  if [[ ! -f "$CRD_DOC" ]]; then
    echo "  FAIL: $CRD_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "naming-convention heading present" \
    "$CRD_DOC" \
    '## Sub-Resource Naming Convention'
}

# --- Test 2: section asserts the bare-name convention ---
test_bare_name_convention_described() {
  echo "Test: section describes the bare CR-name convention (no '-api' suffix)"

  assert_file_contains "section calls out 'no \`-api\` suffix'" \
    "$CRD_DOC" \
    'no `-api` suffix'
  assert_file_contains "section shows bare-name Service DNS example" \
    "$CRD_DOC" \
    'keystone.openstack.svc.cluster.local'
}

# --- Test 3: cross-link to the upgrade-flow reference ---
test_cross_link_to_upgrade_flow() {
  echo "Test: section links to keystone-upgrade-flow.md for migration semantics"

  assert_file_contains "cross-link to upgrade-flow reference" \
    "$CRD_DOC" \
    './keystone-upgrade-flow.md'
}

# --- Run ---
test_heading_exists
test_bare_name_convention_described
test_cross_link_to_upgrade_flow

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
