#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/glance/glance-crd.md "Sub-Resource Naming
# Convention" section shipped with:
#   1. The section heading "## Sub-Resource Naming Convention" exists.
#   2. The section asserts the bare-CR-name convention — checks for the bare
#      `glance.openstack.svc.cluster.local:9292` Service DNS example.
#   3. The section names the derived-resource exceptions: the content-hashed
#      config ConfigMap and backends Secret, the derived db-connection Secret,
#      and the db-sync Job.
#
# Usage: bash tests/unit/docs/glance_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/glance/glance-crd.md"

# assert_file_contains_fixed greps a FIXED (non-regex) pattern with `--` so a
# pattern that begins with a dash (e.g. "-db-sync") is not parsed as a grep
# option. The shared assert_file_contains uses `grep -q "$pattern"`, which
# cannot express a leading-dash literal.
assert_file_contains_fixed() {
  local description="$1" file="$2" pattern="$3"
  if grep -qF -- "$pattern" "$file"; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description (fixed pattern '$pattern' not found in $file)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 1: heading exists ---
test_heading_exists() {
  echo "Test: '## Sub-Resource Naming Convention' heading exists"

  if [[ ! -f "$CRD_DOC" ]]; then
    echo "  FAIL: $CRD_DOC does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains \
    "glance-crd.md carries the naming-convention heading" \
    "$CRD_DOC" \
    "^## Sub-Resource Naming Convention"
}

# --- Test 2: bare-name Service DNS example ---
test_bare_name_dns_example() {
  echo "Test: section shows the bare-CR-name Service DNS example"

  assert_file_contains \
    "glance-crd.md shows the bare Service DNS name" \
    "$CRD_DOC" \
    "glance.openstack.svc.cluster.local:9292"
}

# --- Test 3: the derived-resource exceptions are documented ---
test_derived_names_documented() {
  echo "Test: section documents the derived-resource naming exceptions"

  assert_file_contains \
    "glance-crd.md documents the {name}-config-<hash> ConfigMap" \
    "$CRD_DOC" \
    "config-<hash>"
  assert_file_contains \
    "glance-crd.md documents the {name}-backends-<hash> Secret" \
    "$CRD_DOC" \
    "backends-<hash>"
  assert_file_contains_fixed \
    "glance-crd.md documents the {name}-db-connection Secret" \
    "$CRD_DOC" \
    "-db-connection"
  assert_file_contains_fixed \
    "glance-crd.md documents the {name}-db-sync Job" \
    "$CRD_DOC" \
    "-db-sync"
}

# --- Run all tests ---
echo "=== glance CRD naming-convention doc tests ==="
echo ""
test_heading_exists
echo ""
test_bare_name_dns_example
echo ""
test_derived_names_documented
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
