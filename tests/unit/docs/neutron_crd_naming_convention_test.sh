#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/neutron/neutron-crd.md "Sub-Resource Naming
# Convention" section shipped with:
#   1. The section heading "## Sub-Resource Naming Convention" exists.
#   2. The section asserts the bare-CR-name convention — checks for the bare
#      `neutron.openstack.svc.cluster.local:9696` Service DNS example.
#   3. The section names every derived and content-addressed child: the config
#      ConfigMap, the db-connection, transport-url and ovn-client Secrets, the
#      db-sync Job, the ovn-db-sync CronJob and the two worker Deployments.
#
# CRD_DOC overrides the page under test, so the script can also be pointed at a
# scratch copy or at a path that does not exist.
#
# Usage: bash tests/unit/docs/neutron_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="${CRD_DOC:-$PROJECT_ROOT/docs/reference/neutron/neutron-crd.md}"
SECTION_HEADING="Sub-Resource Naming Convention"

# section_body prints the lines of $CRD_DOC between the "## $SECTION_HEADING"
# heading and the next "## " heading. Tests 2 and 3 assert against that body
# rather than the whole file, so deleting the section fails the gate even when
# the same strings still appear elsewhere in the doc — the db-sync and
# ovn-db-sync names are all over the condition table and the ovnDBSync section.
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
    "neutron-crd.md carries the naming-convention heading" \
    "$CRD_DOC" \
    "^## Sub-Resource Naming Convention"
}

# --- Test 2: bare-name Service DNS example ---
test_bare_name_dns_example() {
  echo "Test: section shows the bare-CR-name Service DNS example"

  assert_contains \
    "the naming-convention section shows the bare Service DNS name" \
    "$(section_body)" \
    "neutron.openstack.svc.cluster.local:9696"
}

# --- Test 3: the derived-resource exceptions are documented ---
test_derived_names_documented() {
  echo "Test: section documents the derived-resource naming exceptions"

  local body
  body="$(section_body)"

  assert_contains \
    "the section documents the {name}-config-<hash> ConfigMap" \
    "$body" \
    "config-<hash>"
  assert_contains \
    "the section documents the {name}-db-connection Secret" \
    "$body" \
    "-db-connection"
  assert_contains \
    "the section documents the {name}-transport-url Secret" \
    "$body" \
    "-transport-url"
  assert_contains \
    "the section documents the {name}-ovn-client Secret" \
    "$body" \
    "-ovn-client"
  assert_contains \
    "the section documents the {name}-db-sync Job" \
    "$body" \
    "-db-sync"
  # Asserted separately from -db-sync, of which it is a superstring: dropping
  # the CronJob row alone leaves the -db-sync assertion passing, and only this
  # one names -ovn-db-sync in the failure line.
  assert_contains \
    "the section documents the {name}-ovn-db-sync CronJob" \
    "$body" \
    "-ovn-db-sync"
  assert_contains \
    "the section documents the {name}-periodic-workers Deployment" \
    "$body" \
    "-periodic-workers"
  assert_contains \
    "the section documents the {name}-ovn-maintenance-worker Deployment" \
    "$body" \
    "-ovn-maintenance-worker"
}

# --- Run all tests ---
echo "=== Neutron CRD naming-convention doc tests ==="
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
