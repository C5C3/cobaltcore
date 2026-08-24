#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/c5c3/keystoneservice-crd.md reference page documents
# the CRD's conditions and the two naming contracts a KeystoneService projects.
# The CR provisions no Service and renders no config, so the vocabulary it must
# pin down is the condition set the dedicated reconciler writes, the consumer
# Secret a registered account is read through, and the prefix every projected
# child carries.
#
#   1. The "### Conditions" section exists and documents every condition type
#      the reconciler owns (CatalogReady, AccountReady, the aggregate Ready)
#      plus the reasons a reader diagnoses a stuck registration by:
#      NamespaceNotAllowed, ServiceCollision, and ServiceAccountCollision.
#   2. The "## Consumer Secret contract" section documents the stable
#      <metadata.name>-credentials name, both data keys (clouds.yaml and
#      password), and the OpenBao path the ExternalSecret materializes it from.
#   3. The projected-child naming convention (the -registration- prefix) is
#      documented, together with the consumer Secret being the one child that
#      deliberately does not carry it.
#
# Usage: bash tests/unit/docs/keystoneservice_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/c5c3/keystoneservice-crd.md"

# --- Test 1: conditions section documents the full condition set ---
test_conditions_documented() {
  echo "Test: '### Conditions' section documents the reconciler's condition set"

  if [[ ! -f "$CRD_DOC" ]]; then
    echo "  FAIL: $CRD_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "conditions heading present" \
    "$CRD_DOC" \
    '^### Conditions'
  assert_file_contains "documents CatalogReady" \
    "$CRD_DOC" \
    'CatalogReady'
  assert_file_contains "documents AccountReady" \
    "$CRD_DOC" \
    'AccountReady'
  assert_file_contains "documents the aggregate Ready reason AllReady" \
    "$CRD_DOC" \
    'AllReady'
  assert_file_contains "documents the namespace-consent gate reason" \
    "$CRD_DOC" \
    'NamespaceNotAllowed'
  assert_file_contains "documents the catalog collision reason" \
    "$CRD_DOC" \
    'ServiceCollision'
  assert_file_contains "documents the account collision reason" \
    "$CRD_DOC" \
    'ServiceAccountCollision'
}

# --- Test 2: consumer Secret contract ---
test_consumer_secret_contract() {
  echo "Test: the consumer Secret contract is documented"

  assert_file_contains "consumer-secret heading present" \
    "$CRD_DOC" \
    '^## Consumer Secret contract'
  assert_file_contains "documents the <metadata.name>-credentials Secret name" \
    "$CRD_DOC" \
    '<metadata.name>-credentials'
  assert_file_contains "documents the clouds.yaml data key" \
    "$CRD_DOC" \
    'clouds.yaml'
  assert_file_contains "documents the password data key" \
    "$CRD_DOC" \
    'password'
  assert_file_contains "documents the per-CR OpenBao path" \
    "$CRD_DOC" \
    'openstack/keystone/<namespace>/<name>/service-accounts/credentials'
}

# --- Test 3: projected-child naming convention ---
test_projected_child_naming() {
  echo "Test: the projected-child naming convention is documented"

  assert_file_contains "documents the -registration- child prefix" \
    "$CRD_DOC" \
    '\-registration\-'
  assert_file_contains "documents the teardown finalizer name" \
    "$CRD_DOC" \
    'c5c3.io/keystoneservice-teardown'
}

# --- Run ---
echo "=== KeystoneService CRD naming-convention doc tests ==="
echo ""
test_conditions_documented
echo ""
test_consumer_secret_contract
echo ""
test_projected_child_naming
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
