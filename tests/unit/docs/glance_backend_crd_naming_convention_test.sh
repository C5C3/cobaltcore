#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/glance/glance-backend-crd.md reference page
# documents the CRD's conditions and its projected-artefact naming convention.
# Like KeystoneIdentityBackend, a GlanceBackend projects no Service of its own —
# it renders one store section into the referenced Glance's Deployment — so the
# vocabulary it must pin down is the condition set (per-backend plus the
# Glance-side aggregate), the content-hashed backends Secret, and the fixed S3
# credentials data-key contract.
#
#   1. The "### Conditions" section exists and documents every condition type:
#      the per-backend CredentialsReady / ConfigProjected, the aggregate Ready
#      reason AllReady, the Glance-side aggregate BackendsReady with its
#      NoDefaultBackend reason, and the GlanceBackendSkipped fault-isolation
#      event.
#   2. The "## Retained Artefacts" section documents the content-hashed
#      <glance>-backends-<hash> Secret and its backends.conf data key.
#   3. The fixed S3 credentials data-key contract (access-key-id /
#      secret-access-key) is documented.
#
# Usage: bash tests/unit/docs/glance_backend_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/glance/glance-backend-crd.md"

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
  assert_file_contains "documents CredentialsReady" \
    "$CRD_DOC" \
    'CredentialsReady'
  assert_file_contains "documents ConfigProjected" \
    "$CRD_DOC" \
    'ConfigProjected'
  assert_file_contains "documents the aggregate Ready reason AllReady" \
    "$CRD_DOC" \
    'AllReady'
  assert_file_contains "documents the Glance-side aggregate BackendsReady" \
    "$CRD_DOC" \
    'BackendsReady'
  assert_file_contains "documents the NoDefaultBackend reason" \
    "$CRD_DOC" \
    'NoDefaultBackend'
  assert_file_contains "documents the GlanceBackendSkipped fault-isolation event" \
    "$CRD_DOC" \
    'GlanceBackendSkipped'
}

# --- Test 2: retained-artefact Secret naming convention ---
test_retained_artefact_naming() {
  echo "Test: '## Retained Artefacts' documents the content-hashed Secret naming"

  assert_file_contains "retained-artefacts heading present" \
    "$CRD_DOC" \
    '^## Retained Artefacts'
  assert_file_contains "documents the <glance>-backends-<hash> Secret name" \
    "$CRD_DOC" \
    '<glance>-backends-<hash>'
  assert_file_contains "documents the backends.conf data key" \
    "$CRD_DOC" \
    'backends.conf'
}

# --- Test 3: fixed S3 credentials data-key contract ---
test_credentials_data_keys() {
  echo "Test: the fixed S3 credentials data-key contract is documented"

  assert_file_contains "documents the access-key-id data key" \
    "$CRD_DOC" \
    'access-key-id'
  assert_file_contains "documents the secret-access-key data key" \
    "$CRD_DOC" \
    'secret-access-key'
}

# --- Run ---
echo "=== GlanceBackend CRD naming-convention doc tests ==="
echo ""
test_conditions_documented
echo ""
test_retained_artefact_naming
echo ""
test_credentials_data_keys
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
