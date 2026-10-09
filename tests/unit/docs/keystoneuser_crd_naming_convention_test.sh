#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/c5c3/keystoneuser-crd.md reference page documents
# the CRD's conditions and the two naming contracts a KeystoneUser order
# projects: the Secret delivered beside the order and the prefix every child in
# the ControlPlane's namespace carries.
#
#   1. The "### Conditions" section exists and documents the two condition
#      types the reconciler owns (UserReady, DeliveryReady), the aggregate
#      Ready, and the reasons a reader diagnoses a stuck order by:
#      NamespaceNotAssigned, ServiceAccountCollision and KeystoneNotPublished.
#   2. The "## Delivered Secret contract" section documents the stable
#      <metadata.name>-credentials name, both data keys (clouds.yaml and
#      password), and the OpenBao path the password is backed up to.
#   3. The projected-child naming convention (the -user- prefix) and the
#      teardown finalizer are documented.
#
# Usage: bash tests/unit/docs/keystoneuser_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/c5c3/keystoneuser-crd.md"

# section prints the body of the "<heading>" section: everything up to the next
# heading of the same or a higher level.
section() { # <heading line, e.g. "### Conditions">
  awk -v heading="$1" '
    BEGIN { level = index(heading, " ") - 1 }
    $0 == heading { in_section = 1; next }
    in_section && /^#+ / { n = index($0, " ") - 1; if (n <= level) exit }
    in_section { print }
  ' "$CRD_DOC"
}

# --- Test 1: conditions section documents the full condition set ---
test_conditions_documented() {
  echo "Test: '### Conditions' section documents the reconciler's condition set"

  if [[ ! -f "$CRD_DOC" ]]; then
    echo "  FAIL: $CRD_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "conditions heading present" "$CRD_DOC" '^### Conditions'

  local body
  body="$(section '### Conditions')"
  assert_not_empty "the Conditions section has a body" "$body"
  local term
  for term in UserReady DeliveryReady '`Ready`' AllReady NamespaceNotAssigned \
    ServiceAccountCollision KeystoneNotPublished; do
    assert_contains "the Conditions section documents ${term}" "$body" "$term"
  done
}

# --- Test 2: delivered Secret contract ---
test_delivered_secret_contract() {
  echo "Test: the delivered Secret contract is documented"

  assert_file_contains "delivered-secret heading present" \
    "$CRD_DOC" '^## Delivered Secret contract'

  local body
  body="$(section '## Delivered Secret contract')"
  assert_contains "documents the <metadata.name>-credentials Secret name" \
    "$body" '<metadata.name>-credentials'
  assert_contains "documents the clouds.yaml data key" "$body" 'clouds.yaml'
  assert_contains "documents the password data key" "$body" '`password`'
  assert_contains "documents the OpenBao backup path" \
    "$body" 'openstack/keystone/<controlplane namespace>/<name>-<hash>-user/service-accounts/credentials'
}

# --- Test 3: projected-child naming convention ---
test_projected_child_naming() {
  echo "Test: the projected-child naming convention is documented"

  assert_file_contains "documents the -user- child prefix" \
    "$CRD_DOC" '<metadata.name>-<8 hex>-user-<discriminator>'
  assert_file_contains "documents the cluster label" \
    "$CRD_DOC" 'c5c3.io/keystoneuser-cluster'
  assert_file_contains "documents the teardown finalizer name" \
    "$CRD_DOC" 'c5c3.io/keystoneuser-teardown'
}

# --- Run ---
echo "=== KeystoneUser CRD naming-convention doc tests ==="
echo ""
test_conditions_documented
echo ""
test_delivered_secret_contract
echo ""
test_projected_child_naming
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
