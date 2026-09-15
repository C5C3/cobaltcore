#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify provision_service_tenant in
# deploy/openbao/bootstrap/setup-database-tenant.sh renders one GRANT per schema
# of its comma-separated <database_names> argument, inside the single
# creation_statements value of the database-engine role, so one role can cover
# every schema of a service that owns more than one. A one-element list renders
# the single-schema statement unchanged, and an empty list or an empty element
# fails before any bao write.
#
# The script is sourced (its source guard keeps main from running) and its
# bao_exec / bao_exec_stdin are redefined to record their arguments in a log
# file, so nothing touches a cluster or an OpenBao.
#
# Usage: bash tests/unit/deploy/setup_database_tenant_multi_schema_test.sh

set -uo pipefail

# Resolve the paths BEFORE sourcing the script under test: sourcing it
# overwrites SCRIPT_DIR with its own bootstrap directory.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT_UNDER_TEST="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-database-tenant.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
BAO_LOG="$tmp/bao.log"

# kubectl stub answering the three lookups provision_service_tenant makes: the
# MariaDB root-secret name and key, then the root Secret payload.
cat >"$tmp/kubectl" <<'STUB'
#!/bin/bash
if [[ "$*" == *"get mariadb"* && "$*" == *"rootPasswordSecretKeyRef.name"* ]]; then
  printf 'openstack-db-root'
  exit 0
fi
if [[ "$*" == *"get mariadb"* && "$*" == *"rootPasswordSecretKeyRef.key"* ]]; then
  printf 'password'
  exit 0
fi
if [[ "$*" == *"get secret"* ]]; then
  # base64("root"), decoded by the script before the recorded bao write.
  printf 'cm9vdA=='
  exit 0
fi
exit 1
STUB
chmod +x "$tmp/kubectl"
PATH="$tmp:$PATH"
export PATH

# The two positional arguments satisfy the script's <namespace> <controlplane>
# guards; the source guard keeps main from running.
export BAO_TOKEN="dummy-token"
# shellcheck source=deploy/openbao/bootstrap/setup-database-tenant.sh
source "$SCRIPT_UNDER_TEST" openstack cp
# The sourced script turns on errexit, which would end this harness on the first
# non-zero command under test.
set +e

# Record the bao writes instead of executing them. bao_exec_stdin drains its
# stdin so the script's `printf ... | bao_exec_stdin` never writes into a closed
# pipe.
bao_exec() {
  printf '%s\n' "$*" >>"$BAO_LOG"
}

bao_exec_stdin() {
  cat >/dev/null
  printf '%s\n' "$*" >>"$BAO_LOG"
}

# ---------------------------------------------------------------------------
# Test: every schema of the list gets its own GRANT
# ---------------------------------------------------------------------------
test_multi_schema_grants_each_schema() {
  echo "Test: provision_service_tenant grants on every schema of the list"

  : >"$BAO_LOG"

  # The function's exit 1 must end the subshell, not this harness.
  local output exit_code written
  output="$( (provision_service_tenant cell openstack openstack-db nova,nova_cell0) 2>&1 )"
  exit_code=$?
  written="$(cat "$BAO_LOG")"

  assert_eq "the tenant is provisioned" "0" "$exit_code"
  # The $* join puts the next argument right after the last semicolon and a
  # space, so this needle pins both GRANTs, their order, and that the
  # creation_statements value ends there. The underscore is escaped: a bare _
  # in a database-level GRANT is a wildcard.
  assert_contains "both schemas are granted, in list order" "$written" \
    "creation_statements=CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT ALL PRIVILEGES ON \`nova\`.* TO '{{name}}'@'%'; GRANT ALL PRIVILEGES ON \`nova\\_cell0\`.* TO '{{name}}'@'%'; revocation_statements="
  assert_contains "the role is keyed on the service and its namespace" \
    "$written" "database/mariadb/roles/cell-openstack"
  assert_contains "the whole schema list is reported" \
    "$output" "Database  : nova,nova_cell0"
}

# ---------------------------------------------------------------------------
# Test: a one-element list renders the single-schema statement
# ---------------------------------------------------------------------------
test_single_schema_is_unchanged() {
  echo "Test: provision_service_tenant renders one GRANT for a one-element list"

  : >"$BAO_LOG"

  local output exit_code written
  output="$( (provision_service_tenant keystone openstack openstack-db keystone) 2>&1 )"
  exit_code=$?
  written="$(cat "$BAO_LOG")"

  assert_eq "the tenant is provisioned" "0" "$exit_code"
  assert_contains "exactly one GRANT is rendered" "$written" \
    "creation_statements=CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT ALL PRIVILEGES ON \`keystone\`.* TO '{{name}}'@'%'; revocation_statements="
  assert_contains "the single schema is reported" \
    "$output" "Database  : keystone"
}

# ---------------------------------------------------------------------------
# Test: an empty list is rejected before any bao write
# ---------------------------------------------------------------------------
test_empty_schema_list_fails() {
  echo "Test: provision_service_tenant rejects an empty schema list"

  : >"$BAO_LOG"

  local output exit_code
  output="$( (provision_service_tenant keystone openstack openstack-db "") 2>&1 )"
  exit_code=$?

  assert_nonzero_exit "the empty list exits non-zero" "$exit_code"
  assert_contains "the error names the missing schema list" \
    "$output" "database name list required"
  assert_eq "no bao write is recorded" "" "$(cat "$BAO_LOG")"
}

# ---------------------------------------------------------------------------
# Test: an empty element is rejected before any bao write
# ---------------------------------------------------------------------------
# A leading, trailing or doubled comma would otherwise render a GRANT on an
# unnamed schema, which MySQL rejects at issuance time, long after this script
# reported success.
test_empty_schema_element_fails() {
  echo "Test: provision_service_tenant rejects an empty element in the list"

  local list output exit_code
  for list in ",nova" "nova," "nova,,nova_cell0"; do
    : >"$BAO_LOG"

    output="$( (provision_service_tenant keystone openstack openstack-db "$list") 2>&1 )"
    exit_code=$?

    assert_nonzero_exit "'$list' exits non-zero" "$exit_code"
    assert_contains "the error names '$list'" \
      "$output" "ERROR: empty schema in list '$list'"
    assert_eq "'$list' records no bao write" "" "$(cat "$BAO_LOG")"
  done
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_multi_schema_grants_each_schema
test_single_schema_is_unchanged
test_empty_schema_list_fails
test_empty_schema_element_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
