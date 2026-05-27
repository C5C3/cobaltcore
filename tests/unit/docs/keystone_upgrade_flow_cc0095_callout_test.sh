#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/keystone-upgrade-flow.md "Sub-Resource Rename"
# callout shipped with CC-0095:
#   1. The section heading "## Sub-Resource Rename (CC-0095)" exists.
#   2. The callout enumerates the affected sub-resource kinds.
#   3. The callout describes catalog self-heal via `keystone-manage bootstrap`.
#   4. The callout documents the two operator workflows (Generation bump and
#      manual `openstack endpoint set`).
#   5. The callout cross-links to the CRD reference's naming-convention
#      section.
#
# Usage: bash tests/unit/docs/keystone_upgrade_flow_cc0095_callout_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

UPGRADE_DOC="$PROJECT_ROOT/docs/reference/keystone/keystone-upgrade-flow.md"

# --- Test 1: callout heading exists (CC-0095, REQ-007) ---
test_callout_heading_exists() {
  echo "Test: '## Sub-Resource Rename (CC-0095)' heading exists (CC-0095, REQ-007)"

  if [[ ! -f "$UPGRADE_DOC" ]]; then
    echo "  FAIL: $UPGRADE_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "rename callout heading present" \
    "$UPGRADE_DOC" \
    '## Sub-Resource Rename'
}

# --- Test 2: affected sub-resources enumerated (CC-0095, REQ-007) ---
test_affected_sub_resources_listed() {
  echo "Test: callout enumerates the affected sub-resource kinds (CC-0095, REQ-007)"

  assert_file_contains "Deployment listed as affected"           "$UPGRADE_DOC" 'Deployment'
  assert_file_contains "Service (ClusterIP) listed as affected"  "$UPGRADE_DOC" 'Service'
  assert_file_contains "PodDisruptionBudget listed as affected"  "$UPGRADE_DOC" 'PodDisruptionBudget'
  assert_file_contains "HorizontalPodAutoscaler listed"          "$UPGRADE_DOC" 'HorizontalPodAutoscaler'
  assert_file_contains "NetworkPolicy listed as affected"        "$UPGRADE_DOC" 'NetworkPolicy'
  assert_file_contains "HTTPRoute listed as affected"            "$UPGRADE_DOC" 'HTTPRoute'
}

# --- Test 3: catalog self-heal via keystone-manage bootstrap (CC-0095, REQ-007) ---
test_catalog_self_heal_described() {
  echo "Test: callout describes catalog self-heal via keystone-manage bootstrap (CC-0095, REQ-007)"

  assert_file_contains "catalog self-heal mentions keystone-manage bootstrap" \
    "$UPGRADE_DOC" \
    'keystone-manage bootstrap'
  assert_file_contains "callout mentions catalog self-heal" \
    "$UPGRADE_DOC" \
    'self-heal'
}

# --- Test 4: two operator workflows documented (CC-0095, REQ-007) ---
test_two_operator_workflows_documented() {
  echo "Test: callout documents Generation bump and manual openstack endpoint set workflows (CC-0095, REQ-007)"

  assert_file_contains "workflow 1: Generation bump" \
    "$UPGRADE_DOC" \
    'Generation bump'
  assert_file_contains "workflow 2: openstack endpoint set" \
    "$UPGRADE_DOC" \
    'openstack endpoint set'
}

# --- Test 5: cross-link to CRD naming-convention section (CC-0095, REQ-007) ---
test_cross_link_to_crd_naming_convention() {
  echo "Test: callout cross-links to keystone-crd.md naming-convention section (CC-0095, REQ-007)"

  assert_file_contains "cross-link to CRD naming-convention anchor" \
    "$UPGRADE_DOC" \
    './keystone-crd.md#sub-resource-naming-convention'
}

# --- Run ---
test_callout_heading_exists
test_affected_sub_resources_listed
test_catalog_self_heal_described
test_two_operator_workflows_documented
test_cross_link_to_crd_naming_convention

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
