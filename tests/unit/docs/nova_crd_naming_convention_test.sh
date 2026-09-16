#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/nova/nova-crd.md "Sub-Resource Naming Convention"
# section shipped with:
#   1. The section heading "## Sub-Resource Naming Convention" exists.
#   2. The section asserts the bare-CR-name convention — checks for the bare
#      `nova.openstack.svc.cluster.local:8774` Service DNS example.
#   3. The section names every derived and content-addressed child: the four
#      non-API Deployments, the console HTTPRoute, the config ConfigMap, the two
#      db-connection Secrets, the transport-url and compute-config Secrets, the
#      db-sync Job, the three upgrade Jobs, the db-archive CronJob and the
#      cell0 MariaDB CRs.
#   4. The section states the 41-character metadata.name cap the db-archive
#      CronJob imposes, and the child-name suffixes metadata.name must not end
#      in.
#
# CRD_DOC overrides the page under test, so the script can also be pointed at a
# scratch copy or at a path that does not exist.
#
# Usage: bash tests/unit/docs/nova_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="${CRD_DOC:-$PROJECT_ROOT/docs/reference/nova/nova-crd.md}"
SECTION_HEADING="Sub-Resource Naming Convention"

# section_body prints the lines of $CRD_DOC between the "## $SECTION_HEADING"
# heading and the next "## " heading. Tests 2 to 4 assert against that body
# rather than the whole file, so deleting the section fails the gate even when
# the same strings still appear elsewhere in the doc — the Job, CronJob and
# Deployment names are all over the owned-resources and rendered-configuration
# sections.
#
# A missing $CRD_DOC yields an empty body instead of an awk failure: test 1
# already reports the missing file, and under `set -e` a failing awk inside a
# `body="$(section_body)"` assignment would abort the script before it prints
# its results line.
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
    "nova-crd.md carries the naming-convention heading" \
    "$CRD_DOC" \
    "^## Sub-Resource Naming Convention"
}

# --- Test 2: bare-name Service DNS example ---
test_bare_name_dns_example() {
  echo "Test: section shows the bare-CR-name Service DNS example"

  assert_contains \
    "the naming-convention section shows the bare Service DNS name" \
    "$(section_body)" \
    "nova.openstack.svc.cluster.local:8774"
}

# --- Test 3: the derived-resource exceptions are documented ---
test_derived_names_documented() {
  echo "Test: section documents the derived-resource naming exceptions"

  local body
  body="$(section_body)"

  # The four non-API Deployments. Each is asserted on its own so a dropped row
  # names the workload that went missing.
  # The row is anchored on its resource column: the bare suffix also appears in
  # the Metadata HTTPRoute row and in the suffix rule below the table.
  assert_contains \
    "the section documents the {name}-metadata Deployment and Service" \
    "$body" \
    "| Metadata Deployment / Service | \`{name}-metadata\`"
  assert_contains \
    "the section documents the {name}-scheduler Deployment" \
    "$body" \
    "-scheduler"
  assert_contains \
    "the section documents the {name}-conductor Deployment" \
    "$body" \
    "-conductor"
  assert_contains \
    "the section documents the {name}-novncproxy Deployment and Service" \
    "$body" \
    "-novncproxy"
  assert_contains \
    "the section documents the {name}-console HTTPRoute" \
    "$body" \
    "-console\`"

  assert_contains \
    "the section documents the {name}-config-<hash> ConfigMap" \
    "$body" \
    "-config-<hash>"
  # Asserted before the bare cell DSN Secret, of which the api one is not a
  # substring: dropping either row has to be visible on its own.
  assert_contains \
    "the section documents the {name}-api-db-connection Secret" \
    "$body" \
    "-api-db-connection"
  assert_contains \
    "the section documents the {name}-db-connection Secret" \
    "$body" \
    "\`{name}-db-connection\`"
  assert_contains \
    "the section documents the {name}-transport-url Secret" \
    "$body" \
    "-transport-url"
  assert_contains \
    "the section documents the {name}-compute-config Secret" \
    "$body" \
    "-compute-config"

  assert_contains \
    "the section documents the {name}-db-sync Job" \
    "$body" \
    "-db-sync"
  # The three upgrade Jobs are asserted one by one: they share a prefix with
  # nothing else here, and only a per-name assertion says which row went
  # missing.
  assert_contains \
    "the section documents the {name}-db-expand Job" \
    "$body" \
    "-db-expand"
  assert_contains \
    "the section documents the {name}-db-migrate Job" \
    "$body" \
    "-db-migrate"
  assert_contains \
    "the section documents the {name}-db-contract Job" \
    "$body" \
    "-db-contract"
  # Anchored on its resource column, like the metadata row: the cap sentence
  # below the table names the suffix too.
  assert_contains \
    "the section documents the {name}-db-archive CronJob" \
    "$body" \
    "| DB-archive CronJob | \`{name}-db-archive\`"

  # The MariaDB CRs: the nova_api instance, the cell instance, and cell0, whose
  # object name is derived from the SQL schema rather than chosen.
  assert_contains \
    "the section documents the {name}-api MariaDB CRs" \
    "$body" \
    "\`{name}-api\`"
  assert_contains \
    "the section documents the cell0 MariaDB CRs" \
    "$body" \
    "{name}-{database}-cell0"
  assert_contains \
    "the section works the cell0 object name through" \
    "$body" \
    "nova-nova-cell0"
}

# --- Test 4: the metadata.name rules ---
test_name_cap_documented() {
  echo "Test: section states the metadata.name cap and the forbidden suffixes"

  local body
  body="$(section_body)"

  assert_contains \
    "the section names the 41-character cap" \
    "$body" \
    "41 characters"
  assert_contains \
    "the section names the CronJob cap the 41 characters are derived from" \
    "$body" \
    "52-character"
  assert_contains \
    "the section names the child-name suffixes metadata.name must not end in" \
    "$body" \
    "It must not end in \`-api\`"
}

# --- Run all tests ---
echo "=== Nova CRD naming-convention doc tests ==="
echo ""
test_heading_exists
echo ""
test_bare_name_dns_example
echo ""
test_derived_names_documented
echo ""
test_name_cap_documented
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
