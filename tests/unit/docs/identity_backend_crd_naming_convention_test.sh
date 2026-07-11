#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/keystone/identity-backend-crd.md reference page
# documents the CRD's conditions and its projected-artefact naming convention.
# Unlike the Keystone and Horizon CRDs, KeystoneIdentityBackend projects no
# Service of its own — it renders config into the referenced Keystone's
# Deployment — so the naming convention it must pin down is the one for the
# per-domain config file and the content-hashed Secrets, plus the condition
# set the dedicated reconciler writes.
#
#   1. The "### Conditions" section exists and documents every condition type
#      the reconciler owns (DomainReady, ConfigProjected, FederationObjectsReady,
#      MappingsReady, the aggregate Ready) plus the aggregated
#      IdentityBackendsReady condition mirrored onto the Keystone CR.
#   2. The "## Retained Artefacts" section documents the content-hashed Secret
#      naming convention (<keystone>-domains-<hash>, <keystone>-federation-<hash>)
#      and the stable-named <keystone>-oidc-crypto-passphrase Secret.
#   3. The per-domain config-file naming convention (keystone.<domain>.conf) is
#      documented.
#
# Usage: bash tests/unit/docs/identity_backend_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/keystone/identity-backend-crd.md"

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
  assert_file_contains "documents DomainReady" \
    "$CRD_DOC" \
    'DomainReady'
  assert_file_contains "documents ConfigProjected" \
    "$CRD_DOC" \
    'ConfigProjected'
  assert_file_contains "documents the federation-only FederationObjectsReady" \
    "$CRD_DOC" \
    'FederationObjectsReady'
  assert_file_contains "documents the federation-only MappingsReady" \
    "$CRD_DOC" \
    'MappingsReady'
  assert_file_contains "documents the aggregate Ready reason AllReady" \
    "$CRD_DOC" \
    'AllReady'
  assert_file_contains "documents the Keystone-side aggregate IdentityBackendsReady" \
    "$CRD_DOC" \
    'IdentityBackendsReady'
}

# --- Test 2: retained-artefact Secret naming convention ---
test_retained_artefact_naming() {
  echo "Test: '## Retained Artefacts' documents the content-hashed Secret naming"

  assert_file_contains "retained-artefacts heading present" \
    "$CRD_DOC" \
    '^## Retained Artefacts'
  assert_file_contains "documents the <keystone>-domains-<hash> Secret name" \
    "$CRD_DOC" \
    '<keystone>-domains-<hash>'
  assert_file_contains "documents the <keystone>-federation-<hash> Secret name" \
    "$CRD_DOC" \
    '<keystone>-federation-<hash>'
  assert_file_contains "documents the stable-named crypto-passphrase Secret" \
    "$CRD_DOC" \
    '<keystone>-oidc-crypto-passphrase'
}

# --- Test 3: per-domain config-file naming convention ---
test_per_domain_config_naming() {
  echo "Test: the per-domain config-file naming convention is documented"

  assert_file_contains "documents the keystone.<domain>.conf per-domain file name" \
    "$CRD_DOC" \
    'keystone.<domain>.conf'
}

# --- Run ---
echo "=== KeystoneIdentityBackend CRD naming-convention doc tests ==="
echo ""
test_conditions_documented
echo ""
test_retained_artefact_naming
echo ""
test_per_domain_config_naming
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
