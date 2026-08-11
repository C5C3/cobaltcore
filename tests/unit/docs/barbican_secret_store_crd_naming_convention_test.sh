#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/barbican/barbican-secret-store-crd.md reference page
# documents the CRD's conditions and its retained-artefact naming convention.
# Like GlanceBackend, a BarbicanSecretStore projects no Service of its own — it
# renders one store section into the referenced Barbican's config — so the
# vocabulary it must pin down is the condition set (per-store plus the
# Barbican-side aggregate), the AppRole credentials Secret named after the
# store, and the fixed credentials data-key contract.
#
#   1. The "### Conditions" section exists and documents every condition type:
#      the per-store CredentialsReady / ProvisioningReady / ConfigProjected, the
#      aggregate Ready reason AllReady, the Barbican-side NoDefaultSecretStore
#      reason, and the BarbicanSecretStoreSkipped fault-isolation event.
#   2. The "## Retained Artefacts" section documents the <store>-approle Secret
#      and the fixed data keys it carries (role-id, secret-id, and the ca.crt
#      trust bundle a brownfield store references).
#
# Usage: bash tests/unit/docs/barbican_secret_store_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/barbican/barbican-secret-store-crd.md"
SECTION_HEADING="Retained Artefacts"

# section_body prints the lines of $CRD_DOC between the "## $SECTION_HEADING"
# heading and the next "## " heading. Test 2 asserts against that body rather
# than the whole file: role-id, secret-id, and ca.crt all occur in the spec and
# condition tables further up, so an unscoped grep would still pass with the
# section's data-key contract deleted.
#
# A missing $CRD_DOC yields an empty body instead of an awk failure: test 1
# already reports the missing file, and under `set -e` a failing awk inside the
# `body="$(section_body)"` assignment would abort the script before it prints its
# results line.
section_body() {
  [[ -f "$CRD_DOC" ]] || return 0

  awk -v h="^## ${SECTION_HEADING}[[:space:]]*\$" '
    $0 ~ h { f = 1; next }
    f && /^## / { exit }
    f { print }
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

  assert_file_contains "conditions heading present" \
    "$CRD_DOC" \
    '^### Conditions'
  assert_file_contains "documents CredentialsReady" \
    "$CRD_DOC" \
    'CredentialsReady'
  assert_file_contains "documents ProvisioningReady" \
    "$CRD_DOC" \
    'ProvisioningReady'
  assert_file_contains "documents ConfigProjected" \
    "$CRD_DOC" \
    'ConfigProjected'
  assert_file_contains "documents the aggregate Ready reason AllReady" \
    "$CRD_DOC" \
    'AllReady'
  assert_file_contains "documents the NoDefaultSecretStore reason" \
    "$CRD_DOC" \
    'NoDefaultSecretStore'
  assert_file_contains "documents the BarbicanSecretStoreSkipped fault-isolation event" \
    "$CRD_DOC" \
    'BarbicanSecretStoreSkipped'
}

# --- Test 2: retained-artefact Secret naming and data-key contract ---
test_retained_artefact_naming() {
  echo "Test: '## Retained Artefacts' documents the AppRole Secret and its keys"

  assert_file_contains "retained-artefacts heading present" \
    "$CRD_DOC" \
    '^## Retained Artefacts'

  local body
  body="$(section_body)"

  assert_contains "documents the <store>-approle Secret name" \
    "$body" \
    '<store>-approle'
  assert_contains "documents the role-id data key" \
    "$body" \
    'role-id'
  assert_contains "documents the secret-id data key" \
    "$body" \
    'secret-id'
  assert_contains "documents the ca.crt trust-bundle data key" \
    "$body" \
    'ca.crt'
}

# --- Run ---
echo "=== BarbicanSecretStore CRD naming-convention doc tests ==="
echo ""
test_conditions_documented
echo ""
test_retained_artefact_naming
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
