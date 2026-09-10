#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the docs/reference/cinder/cinder-crd.md "Sub-Resource Naming
# Convention" section shipped with:
#   1. The section heading "## Sub-Resource Naming Convention" exists.
#   2. The section asserts the bare-CR-name convention — checks for the bare
#      `cinder.openstack.svc.cluster.local:8776` Service DNS example.
#   3. The section names every derived and content-addressed child: the config
#      ConfigMap, the two satellite projection Secrets, the db-connection and
#      transport-url Secrets, the db-sync Job, the three upgrade Jobs, the
#      db-purge CronJob, the three non-API Deployments and the service-remove
#      Job.
#   4. The section carries the os-brick mount contract, which is a name the
#      operator does not choose freely: the md5 of "server:path".
#
# CRD_DOC overrides the page under test, so the script can also be pointed at a
# scratch copy or at a path that does not exist.
#
# Usage: bash tests/unit/docs/cinder_crd_naming_convention_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CRD_DOC="${CRD_DOC:-$PROJECT_ROOT/docs/reference/cinder/cinder-crd.md}"
SECTION_HEADING="Sub-Resource Naming Convention"

# section_body prints the lines of $CRD_DOC between the "## $SECTION_HEADING"
# heading and the next "## " heading. Tests 2 to 4 assert against that body
# rather than the whole file, so deleting the section fails the gate even when
# the same strings still appear elsewhere in the doc — the Job and Deployment
# names are all over the owned-resources and rendered-configuration sections.
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
    "cinder-crd.md carries the naming-convention heading" \
    "$CRD_DOC" \
    "^## Sub-Resource Naming Convention"
}

# --- Test 2: bare-name Service DNS example ---
test_bare_name_dns_example() {
  echo "Test: section shows the bare-CR-name Service DNS example"

  assert_contains \
    "the naming-convention section shows the bare Service DNS name" \
    "$(section_body)" \
    "cinder.openstack.svc.cluster.local:8776"
}

# --- Test 3: the derived-resource exceptions are documented ---
test_derived_names_documented() {
  echo "Test: section documents the derived-resource naming exceptions"

  local body
  body="$(section_body)"

  assert_contains \
    "the section documents the {name}-config-<hash> ConfigMap" \
    "$body" \
    "-config-<hash>"
  assert_contains \
    "the section documents the per-backend projection Secret" \
    "$body" \
    "-backend-{backend}-<hash>"
  assert_contains \
    "the section documents the backup projection Secret" \
    "$body" \
    "-backup-{backupBackend}-<hash>"
  assert_contains \
    "the section documents the {name}-db-connection Secret" \
    "$body" \
    "-db-connection"
  assert_contains \
    "the section documents the {name}-transport-url Secret" \
    "$body" \
    "-transport-url"
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
  assert_contains \
    "the section documents the {name}-db-purge CronJob" \
    "$body" \
    "-db-purge"
  assert_contains \
    "the section documents the {name}-scheduler Deployment" \
    "$body" \
    "-scheduler"
  assert_contains \
    "the section documents the {name}-volume-{backend} Deployment" \
    "$body" \
    "-volume-{backend}"
  # Asserted separately from the backup Secret, of which it is a substring:
  # dropping the Deployment row alone would leave that assertion passing.
  assert_contains \
    "the section documents the {name}-backup Deployment" \
    "$body" \
    "\`{name}-backup\`"
  assert_contains \
    "the section documents the service-remove Job" \
    "$body" \
    "-service-remove"
}

# --- Test 4: the os-brick mount contract ---
test_mount_contract_documented() {
  echo "Test: section documents the os-brick mount path contract"

  local body
  body="$(section_body)"

  assert_contains \
    "the section names the mount base the driver derives its directory under" \
    "$body" \
    "/var/lib/cinder/mnt/"
  assert_contains \
    "the section works the md5 example through" \
    "$body" \
    "6f3cb55ed3b423dbb7791aaf3783754f"
  assert_contains \
    "the section names the volume service's host identity" \
    "$body" \
    "{name}@{backend}"
}

# --- Run all tests ---
echo "=== Cinder CRD naming-convention doc tests ==="
echo ""
test_heading_exists
echo ""
test_bare_name_dns_example
echo ""
test_derived_names_documented
echo ""
test_mount_contract_documented
echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
