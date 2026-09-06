#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/ovn/ovn-central-crd.md reference page documents the
# CRD's conditions and the names its children take. An OVNCentral projects two
# Raft databases, northd, an optional relay and a backup pair into one
# namespace, so the vocabulary it has to pin down is the seven sub-conditions
# plus the aggregate, and the component suffix each child carries.
#
#   1. The "### Conditions" section exists and documents every condition type:
#      TLSReady, the two database conditions NorthboundReady and
#      SouthboundReady, EndpointsReady, NorthdReady, RelayReady, BackupReady,
#      and the aggregate Ready with its AllReady reason.
#   2. The "## Sub-Resource Naming Convention" section documents the suffix of
#      every child: the two database StatefulSets, the per-member pods and
#      Services, northd, the relay, the server and client Secrets, the scripts
#      ConfigMap and the backup claim plus CronJob.
#
# CRD_DOC overrides the page under test, so the script can also be pointed at a
# scratch copy or at a path that does not exist.
#
# Usage: bash tests/unit/docs/ovn_central_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="${CRD_DOC:-$PROJECT_ROOT/docs/reference/ovn/ovn-central-crd.md}"
SECTION_HEADING="Sub-Resource Naming Convention"

# section_body prints the lines of $CRD_DOC between the "## $SECTION_HEADING"
# heading and the next "## " heading. Test 2 asserts against that body rather
# than the whole file: the backup and client suffixes also occur in the Backup
# and Status sections, so an unscoped grep would still pass with the naming
# table deleted.
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
  assert_file_contains "documents TLSReady" \
    "$CRD_DOC" \
    'TLSReady'
  assert_file_contains "documents NorthboundReady" \
    "$CRD_DOC" \
    'NorthboundReady'
  assert_file_contains "documents SouthboundReady" \
    "$CRD_DOC" \
    'SouthboundReady'
  assert_file_contains "documents EndpointsReady" \
    "$CRD_DOC" \
    'EndpointsReady'
  assert_file_contains "documents NorthdReady" \
    "$CRD_DOC" \
    'NorthdReady'
  assert_file_contains "documents RelayReady" \
    "$CRD_DOC" \
    'RelayReady'
  assert_file_contains "documents BackupReady" \
    "$CRD_DOC" \
    'BackupReady'
  # The aggregate is matched with its backticks: a bare 'Ready' is a substring
  # of every sub-condition above and would pass on a page that never names it.
  assert_file_contains "documents the aggregate Ready condition" \
    "$CRD_DOC" \
    "\`Ready\`"
  assert_file_contains "documents the aggregate Ready reason AllReady" \
    "$CRD_DOC" \
    'AllReady'
}

# --- Test 2: every child's naming suffix is documented ---
test_child_naming_documented() {
  echo "Test: '## Sub-Resource Naming Convention' documents every child suffix"

  assert_file_contains "naming-convention heading present" \
    "$CRD_DOC" \
    '^## Sub-Resource Naming Convention'

  local body
  body="$(section_body)"

  assert_contains "documents the {name}-nb StatefulSet and headless Service" \
    "$body" \
    '-nb'
  assert_contains "documents the {name}-sb StatefulSet and headless Service" \
    "$body" \
    '-sb'
  assert_contains "documents the {name}-nb-0 per-member pod and Service" \
    "$body" \
    '-nb-0'
  assert_contains "documents the {name}-northd Deployment" \
    "$body" \
    '-northd'
  assert_contains "documents the {name}-sb-relay Deployment and Service" \
    "$body" \
    '-sb-relay'
  assert_contains "documents the {name}-client Secret" \
    "$body" \
    '-client'
  assert_contains "documents the {name}-nb-server Secret" \
    "$body" \
    '-nb-server'
  assert_contains "documents the {name}-central-scripts ConfigMap" \
    "$body" \
    '-central-scripts'
  assert_contains "documents the {name}-backup claim and CronJob" \
    "$body" \
    '-backup'
}

# --- Run ---
echo "=== OVNCentral CRD naming-convention doc tests ==="
echo ""
test_conditions_documented
echo ""
test_child_naming_documented
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
