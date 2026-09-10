#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/cinder/cinder-backend-crd.md reference page
# documents the CRD's conditions and its projected-artefact naming convention.
# A CinderBackend projects no Service of its own — it renders one backend
# section and one cinder-volume Deployment under the referenced Cinder — so the
# vocabulary it has to pin down is the condition set (per-backend plus the
# Cinder-side aggregate), the content-hashed projection Secret with its three
# data keys, the detach contract, and the extraOptions denylist.
#
#   1. The "## Conditions" section exists and documents every condition type
#      and reason: CredentialsReady with CredentialsNotRequired and
#      WaitingForParent, ConfigProjected with WaitingForProjection, the
#      aggregate Ready with AllReady / NotAllReady / Detaching, the Cinder-side
#      BackendsReady with AllBackendsProjected / WaitingForBackends /
#      NoBackends, and the CinderBackendSkipped fault-isolation event.
#   2. The "## Retained Artefacts" section documents the content-hashed
#      <cinder>-backend-<name>-<hash> Secret and its three data keys.
#   3. The detach contract names the finalizer, the Job and the tolerated
#      exit code.
#   4. The extraOptions denylist names the keys the projection owns.
#
# Usage: bash tests/unit/docs/cinder_backend_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/cinder/cinder-backend-crd.md"

# --- Test 1: conditions section documents the full condition set ---
test_conditions_documented() {
  echo "Test: '## Conditions' section documents the controller's condition set"

  if [[ ! -f "$CRD_DOC" ]]; then
    echo "  FAIL: $CRD_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "conditions heading present" \
    "$CRD_DOC" '^## Conditions'
  assert_file_contains "documents CredentialsReady" \
    "$CRD_DOC" 'CredentialsReady'
  assert_file_contains "documents the CredentialsNotRequired reason" \
    "$CRD_DOC" 'CredentialsNotRequired'
  assert_file_contains "documents the WaitingForParent reason" \
    "$CRD_DOC" 'WaitingForParent'
  assert_file_contains "documents ConfigProjected" \
    "$CRD_DOC" 'ConfigProjected'
  assert_file_contains "documents the WaitingForProjection reason" \
    "$CRD_DOC" 'WaitingForProjection'
  assert_file_contains "documents the aggregate Ready reason AllReady" \
    "$CRD_DOC" 'AllReady'
  assert_file_contains "documents the Detaching reason" \
    "$CRD_DOC" 'Detaching'
  assert_file_contains "documents the Cinder-side aggregate BackendsReady" \
    "$CRD_DOC" 'BackendsReady'
  assert_file_contains "documents the AllBackendsProjected reason" \
    "$CRD_DOC" 'AllBackendsProjected'
  assert_file_contains "documents the WaitingForBackends reason" \
    "$CRD_DOC" 'WaitingForBackends'
  assert_file_contains "documents the NoBackends reason" \
    "$CRD_DOC" 'NoBackends'
  assert_file_contains "documents the CinderBackendSkipped fault-isolation event" \
    "$CRD_DOC" 'CinderBackendSkipped'
}

# --- Test 2: retained-artefact Secret naming convention ---
test_retained_artefact_naming() {
  echo "Test: '## Retained Artefacts' documents the content-hashed Secret naming"

  assert_file_contains "retained-artefacts heading present" \
    "$CRD_DOC" '^## Retained Artefacts'
  assert_file_contains "documents the <cinder>-backend-<name>-<hash> Secret name" \
    "$CRD_DOC" '<cinder>-backend-<name>-<hash>'
  assert_file_contains "documents the backend.conf data key" \
    "$CRD_DOC" 'backend.conf'
  assert_file_contains "documents the shares data key" \
    "$CRD_DOC" '.shares'
  assert_file_contains "documents the volume.conf data key" \
    "$CRD_DOC" 'volume.conf'
}

# --- Test 3: the detach contract ---
test_detach_contract() {
  echo "Test: the detach contract names the finalizer, the Job and the tolerated exit"

  assert_file_contains "documents the service-remove finalizer" \
    "$CRD_DOC" 'cinder.openstack.c5c3.io/service-remove'
  assert_file_contains "documents the service-remove Job name" \
    "$CRD_DOC" '<cinder>-<name>-service-remove'
  assert_file_contains "documents the tolerated exit code" \
    "$CRD_DOC" 'Exit code 2'
  assert_file_contains "documents the ServiceRemoveJobFailed reason" \
    "$CRD_DOC" 'ServiceRemoveJobFailed'
}

# --- Test 4: the extraOptions denylist ---
test_extra_options_denylist() {
  echo "Test: the extraOptions denylist names the keys the projection owns"

  assert_file_contains "denylist heading present" \
    "$CRD_DOC" '^## extraOptions Denylist'
  assert_file_contains "documents the volume_driver key" \
    "$CRD_DOC" 'volume_driver'
  assert_file_contains "documents the volume_backend_name key" \
    "$CRD_DOC" 'volume_backend_name'
  assert_file_contains "documents the nfs_shares_config key" \
    "$CRD_DOC" 'nfs_shares_config'
  assert_file_contains "documents the image_volume_cache_enabled key" \
    "$CRD_DOC" 'image_volume_cache_enabled'
}

# --- Run ---
echo "=== CinderBackend CRD naming-convention doc tests ==="
echo ""
test_conditions_documented
echo ""
test_retained_artefact_naming
echo ""
test_detach_contract
echo ""
test_extra_options_denylist
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
