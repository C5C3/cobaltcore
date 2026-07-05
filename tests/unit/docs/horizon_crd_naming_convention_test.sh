#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/horizon/horizon-crd.md "Sub-Resource Naming
# Convention" section shipped with:
#   1. The section heading "## Sub-Resource Naming Convention" exists.
#   2. The section asserts the bare-CR-name convention — checks for the bare
#      `horizon.openstack.svc.cluster.local` Service DNS example.
#   3. The section names the one derived exception (the content-addressed
#      config ConfigMap).
#
# Usage: bash tests/unit/docs/horizon_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/horizon/horizon-crd.md"

# --- Test 1: heading exists ---
test_heading_exists() {
  echo "Test: '## Sub-Resource Naming Convention' heading exists"

  if [[ ! -f "$CRD_DOC" ]]; then
    echo "  FAIL: $CRD_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains \
    "horizon-crd.md carries the naming-convention heading" \
    "$CRD_DOC" \
    "^## Sub-Resource Naming Convention"
}

# --- Test 2: bare-name Service DNS example ---
test_bare_name_dns_example() {
  echo "Test: section shows the bare-CR-name Service DNS example"

  assert_file_contains \
    "horizon-crd.md shows the bare Service DNS name" \
    "$CRD_DOC" \
    "horizon.openstack.svc.cluster.local:8080"
}

# --- Test 3: the derived ConfigMap exception is documented ---
test_configmap_exception_documented() {
  echo "Test: section documents the content-addressed ConfigMap exception"

  assert_file_contains \
    "horizon-crd.md documents the {name}-config-<hash> exception" \
    "$CRD_DOC" \
    "config-<content-hash>"
}

# --- Run all tests ---
echo "=== horizon CRD naming-convention doc tests ==="
echo ""
test_heading_exists
echo ""
test_bare_name_dns_example
echo ""
test_configmap_exception_documented
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
