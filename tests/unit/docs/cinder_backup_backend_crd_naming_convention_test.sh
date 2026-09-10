#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/cinder/cinder-backup-backend-crd.md reference page
# documents the CRD's conditions and its projected-artefact naming convention.
# A CinderBackupBackend describes the one property of the one cinder-backup
# Deployment, so the vocabulary it has to pin down is the condition set
# (per-backend plus the Cinder-side aggregate, including the two states a second
# attachment produces), the content-hashed projection Secret, the typed defaults
# the rendered section carries, and the extraOptions denylist.
#
#   1. The "## Conditions" section exists and documents every condition type
#      and reason: CredentialsReady with CredentialsNotRequired and
#      WaitingForParent, ConfigProjected with WaitingForProjection, the
#      aggregate Ready with AllReady / NotAllReady, and the Cinder-side
#      BackupBackendReady with BackupBackendProjected / NoBackupBackend /
#      WaitingForBackupBackend / MultipleBackupBackends, plus the
#      CinderBackupBackendSkipped fault event.
#   2. The "## Retained Artefacts" section documents the content-hashed
#      <cinder>-backup-<name>-<hash> Secret and its backup.conf data key.
#   3. The typed defaults and bounds of the chunk size and the compression
#      algorithm are documented.
#   4. The extraOptions denylist names the keys the projection owns, and the
#      at-most-one rule names its admission message.
#
# Usage: bash tests/unit/docs/cinder_backup_backend_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/cinder/cinder-backup-backend-crd.md"

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
  assert_file_contains "documents the Cinder-side aggregate BackupBackendReady" \
    "$CRD_DOC" 'BackupBackendReady'
  assert_file_contains "documents the BackupBackendProjected reason" \
    "$CRD_DOC" 'BackupBackendProjected'
  assert_file_contains "documents the NoBackupBackend reason" \
    "$CRD_DOC" 'NoBackupBackend'
  assert_file_contains "documents the WaitingForBackupBackend reason" \
    "$CRD_DOC" 'WaitingForBackupBackend'
  assert_file_contains "documents the MultipleBackupBackends reason" \
    "$CRD_DOC" 'MultipleBackupBackends'
  assert_file_contains "documents the CinderBackupBackendSkipped fault event" \
    "$CRD_DOC" 'CinderBackupBackendSkipped'
}

# --- Test 2: retained-artefact Secret naming convention ---
test_retained_artefact_naming() {
  echo "Test: '## Retained Artefacts' documents the content-hashed Secret naming"

  assert_file_contains "retained-artefacts heading present" \
    "$CRD_DOC" '^## Retained Artefacts'
  assert_file_contains "documents the <cinder>-backup-<name>-<hash> Secret name" \
    "$CRD_DOC" '<cinder>-backup-<name>-<hash>'
  assert_file_contains "documents the backup.conf data key" \
    "$CRD_DOC" 'backup.conf'
}

# --- Test 3: the typed defaults and bounds ---
test_typed_defaults() {
  echo "Test: the chunk-size and compression defaults are documented"

  assert_file_contains "documents the fileSize default" \
    "$CRD_DOC" '52428800'
  assert_file_contains "documents the fileSize minimum" \
    "$CRD_DOC" '1048576'
  assert_file_contains "documents the fileSize multiple" \
    "$CRD_DOC" '32768'
  assert_file_contains "documents the compression default" \
    "$CRD_DOC" 'zlib'
  assert_file_contains "documents that this kind carries no finalizer" \
    "$CRD_DOC" 'carries no finalizer'
}

# --- Test 4: the denylist and the at-most-one rule ---
test_denylist_and_single_attachment() {
  echo "Test: the denylist and the at-most-one rule are documented"

  assert_file_contains "denylist heading present" \
    "$CRD_DOC" '^## extraOptions Denylist'
  assert_file_contains "documents the backup_driver key" \
    "$CRD_DOC" 'backup_driver'
  assert_file_contains "documents the backup_share key" \
    "$CRD_DOC" 'backup_share'
  assert_file_contains "documents the backup_file_size key" \
    "$CRD_DOC" 'backup_file_size'
  assert_file_contains "documents the backup_compression_algorithm key" \
    "$CRD_DOC" 'backup_compression_algorithm'
  assert_file_contains "documents the admission message of the at-most-one rule" \
    "$CRD_DOC" 'already has CinderBackupBackend'
}

# --- Run ---
echo "=== CinderBackupBackend CRD naming-convention doc tests ==="
echo ""
test_conditions_documented
echo ""
test_retained_artefact_naming
echo ""
test_typed_defaults
echo ""
test_denylist_and_single_attachment
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
