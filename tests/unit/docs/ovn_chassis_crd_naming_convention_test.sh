#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/ovn/ovn-chassis-crd.md reference page documents the
# CRD's conditions and the names its children take. An OVNChassis projects two
# DaemonSets, two ConfigMaps and three per-node maintenance Jobs onto the nodes
# it selects, so the vocabulary it has to pin down is the five sub-conditions
# plus the aggregate, and the component suffix each child carries.
#
#   1. The "### Conditions" section exists and documents every condition type:
#      CentralReady, NodesReady, the two DaemonSet conditions OVSReady and
#      ControllerReady, MaintenanceReady, and the aggregate Ready reason
#      AllReady.
#   2. The "## Sub-Resource Naming Convention" section documents the suffix of
#      every child: the two DaemonSets, the nodes and scripts ConfigMaps, and
#      the hashed apply, evacuate and chassis-del Jobs.
#
# CRD_DOC overrides the page under test, so the script can also be pointed at a
# scratch copy or at a path that does not exist.
#
# Usage: bash tests/unit/docs/ovn_chassis_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="${CRD_DOC:-$PROJECT_ROOT/docs/reference/ovn/ovn-chassis-crd.md}"
SECTION_HEADING="Sub-Resource Naming Convention"

# section_body prints the lines of $CRD_DOC between the "## $SECTION_HEADING"
# heading and the next "## " heading. Test 2 asserts against that body rather
# than the whole file: the nodes ConfigMap and the DaemonSet suffixes also occur
# in the condition table and the node contract, so an unscoped grep would still
# pass with the naming table deleted.
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
  assert_file_contains "documents CentralReady" \
    "$CRD_DOC" \
    'CentralReady'
  assert_file_contains "documents NodesReady" \
    "$CRD_DOC" \
    'NodesReady'
  assert_file_contains "documents OVSReady" \
    "$CRD_DOC" \
    'OVSReady'
  assert_file_contains "documents ControllerReady" \
    "$CRD_DOC" \
    'ControllerReady'
  assert_file_contains "documents MaintenanceReady" \
    "$CRD_DOC" \
    'MaintenanceReady'
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

  assert_contains "documents the {name}-ovs DaemonSet" \
    "$body" \
    '-ovs'
  assert_contains "documents the {name}-ovn-controller DaemonSet" \
    "$body" \
    '-ovn-controller'
  assert_contains "documents the {name}-nodes ConfigMap" \
    "$body" \
    '-nodes'
  assert_contains "documents the {name}-chassis-scripts ConfigMap" \
    "$body" \
    '-chassis-scripts'
  assert_contains "documents the {name}-apply-<hash> Job" \
    "$body" \
    '-apply-'
  assert_contains "documents the {name}-evacuate-<hash> Job" \
    "$body" \
    '-evacuate-'
  assert_contains "documents the {name}-chassis-del-<hash> Job" \
    "$body" \
    '-chassis-del-'
}

# --- Run ---
echo "=== OVNChassis CRD naming-convention doc tests ==="
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
