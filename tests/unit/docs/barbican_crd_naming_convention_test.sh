#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/barbican/barbican-crd.md "Sub-Resource Naming
# Convention" section shipped with:
#   1. The section heading "## Sub-Resource Naming Convention" exists.
#   2. The section asserts the bare-CR-name convention — checks for the bare
#      `barbican.openstack.svc.cluster.local:9311` Service DNS example.
#   3. The section names the four derived-resource exceptions: the
#      content-hashed config Secret, the derived db-connection Secret, the
#      db-sync Job, and the db-clean CronJob.
#
# Usage: bash tests/unit/docs/barbican_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="$PROJECT_ROOT/docs/reference/barbican/barbican-crd.md"
SECTION_HEADING="Sub-Resource Naming Convention"

# section_body prints the lines of $CRD_DOC between the "## $SECTION_HEADING"
# heading and the next "## " heading. Tests 2 and 3 assert against that body
# rather than the whole file, so deleting the section fails the gate even when
# the same strings still appear elsewhere in the doc.
#
# A missing $CRD_DOC yields an empty body instead of an awk failure: test 1
# already reports the missing file, and under `set -e` a failing awk inside the
# `body="$(section_body)"` assignment of test 3 would abort the script before it
# prints its results line.
section_body() {
  [[ -f "$CRD_DOC" ]] || return 0

  awk -v h="^## ${SECTION_HEADING}[[:space:]]*\$" '
    $0 ~ h { f = 1; next }
    f && /^## / { exit }
    f { print }
  ' "$CRD_DOC"
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
    "barbican-crd.md carries the naming-convention heading" \
    "$CRD_DOC" \
    "^## Sub-Resource Naming Convention"
}

# --- Test 2: bare-name Service DNS example ---
test_bare_name_dns_example() {
  echo "Test: section shows the bare-CR-name Service DNS example"

  assert_contains \
    "the naming-convention section shows the bare Service DNS name" \
    "$(section_body)" \
    "barbican.openstack.svc.cluster.local:9311"
}

# --- Test 3: the derived-resource exceptions are documented ---
test_derived_names_documented() {
  echo "Test: section documents the four derived-resource naming exceptions"

  local body
  body="$(section_body)"

  assert_contains \
    "the section documents the {name}-config-<hash> Secret" \
    "$body" \
    "config-<hash>"
  assert_contains \
    "the section documents the {name}-db-connection Secret" \
    "$body" \
    "-db-connection"
  assert_contains \
    "the section documents the {name}-db-sync Job" \
    "$body" \
    "-db-sync"
  assert_contains \
    "the section documents the {name}-db-clean CronJob" \
    "$body" \
    "-db-clean"
}

# --- Run all tests ---
echo "=== barbican CRD naming-convention doc tests ==="
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
