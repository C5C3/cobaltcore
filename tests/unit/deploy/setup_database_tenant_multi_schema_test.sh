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
# fails before any bao write. main writes each SERVICE_TENANTS row under the
# row's role and schema list, gated on the row's spec-service, so two rows on
# one spec.services block are both written or both skipped.
#
# The last block of tests runs main on the SHIPPED table instead of an injected
# one: the two nova rows are the first pair that shares a spec-service, so their
# roles, schemas, namespace and dedicated-database skip are pinned here, as is
# the rule that keeps the flattened <role>-<namespace> handles unambiguous: no
# role name may be a hyphen-prefix of another.
#
# The script is sourced (its source guard keeps main from running until a test
# calls it), its bao_exec / bao_exec_stdin are redefined to record their
# arguments in a log file, and kubectl is a stub, so nothing touches a cluster
# or an OpenBao.
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

# kubectl stub answering the three lookups provision_service_tenant makes (the
# MariaDB root-secret name and key, then the root Secret payload) and the
# ControlPlane lookups main makes. The ControlPlane exists and declares
# spec.services.nova, placed in the namespace STUB_NOVA_NS names and with a
# dedicated nova database only when STUB_NOVA_DEDICATED_DB is set; every other
# field is unset. STUB_ROOT_SECRET_MISSING makes the root Secret lookup fail,
# the way it does in a namespace that has no MariaDB of its own yet.
cat >"$tmp/kubectl" <<'STUB'
#!/bin/bash
if [[ "$*" == *"get controlplane"* ]]; then
  case "$*" in
    *"jsonpath={.spec.services.nova.namespace.name}") printf '%s' "${STUB_NOVA_NS:-}" ;;
    *"jsonpath={.spec.services.nova}") printf 'map[]' ;;
    *"jsonpath={.spec.services.nova.dedicatedBackingServices.database}") printf '%s' "${STUB_NOVA_DEDICATED_DB:-}" ;;
  esac
  exit 0
fi
if [[ "$*" == *"get mariadb"* && "$*" == *"rootPasswordSecretKeyRef.name"* ]]; then
  printf 'openstack-db-root'
  exit 0
fi
if [[ "$*" == *"get mariadb"* && "$*" == *"rootPasswordSecretKeyRef.key"* ]]; then
  printf 'password'
  exit 0
fi
if [[ "$*" == *"get secret"* ]]; then
  if [[ -n "${STUB_ROOT_SECRET_MISSING:-}" ]]; then
    exit 1
  fi
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
# Test: main writes each SERVICE_TENANTS row under its own role and schemas
# ---------------------------------------------------------------------------
# Every shipped row repeats one name in all three fields, so only rows that
# split them catch a swapped field: here two roles share spec.services.nova.
test_service_tenants_rows_split_role_and_spec_service() {
  echo "Test: main writes every row of a spec-service under the row's role and schemas"

  : >"$BAO_LOG"

  local exit_code written
  (
    SERVICE_TENANTS=("cell nova nova,nova_cell0" "api nova nova_api")
    main
  ) >/dev/null 2>&1
  exit_code=$?
  written="$(cat "$BAO_LOG")"

  assert_eq "main succeeds" "0" "$exit_code"
  assert_contains "the cell role grants the cell row's schemas" "$written" \
    "roles/cell-openstack db_name=cell-openstack creation_statements=CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT ALL PRIVILEGES ON \`nova\`.* TO '{{name}}'@'%'; GRANT ALL PRIVILEGES ON \`nova\\_cell0\`.* TO '{{name}}'@'%'; revocation_statements="
  assert_contains "the api role grants the api row's schema" "$written" \
    "roles/api-openstack db_name=api-openstack creation_statements=CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT ALL PRIVILEGES ON \`nova\\_api\`.* TO '{{name}}'@'%'; revocation_statements="
}

# ---------------------------------------------------------------------------
# Test: a dedicated database skips every row of its spec-service
# ---------------------------------------------------------------------------
test_dedicated_database_skips_every_row() {
  echo "Test: main skips every row of a spec-service that declares a dedicated database"

  : >"$BAO_LOG"

  local exit_code written
  (
    export STUB_NOVA_DEDICATED_DB=nova-db
    SERVICE_TENANTS=("cell nova nova,nova_cell0" "api nova nova_api")
    main
  ) >/dev/null 2>&1
  exit_code=$?
  written="$(cat "$BAO_LOG")"

  assert_eq "main succeeds" "0" "$exit_code"
  assert_contains "the Keystone leg still runs" "$written" "roles/keystone-openstack"
  assert_not_contains "the cell row is skipped" "$written" "cell-openstack"
  assert_not_contains "the api row is skipped" "$written" "api-openstack"
}

# ---------------------------------------------------------------------------
# Test: the shipped nova rows are provisioned under their own roles
# ---------------------------------------------------------------------------
# Nova is the first service with two engine roles. Both are written from the
# SHIPPED table here, so a row edited in the script is caught even when no test
# injects it: the API role grants on nova_api alone, the cell role on nova and
# nova_cell0, in that order.
test_shipped_nova_rows_are_provisioned() {
  echo "Test: main provisions both shipped nova rows"

  : >"$BAO_LOG"

  local exit_code written
  ( main ) >/dev/null 2>&1
  exit_code=$?
  written="$(cat "$BAO_LOG")"

  assert_eq "main succeeds" "0" "$exit_code"
  assert_contains "the nova-api role grants on nova_api alone" "$written" \
    "roles/nova-api-openstack db_name=nova-api-openstack creation_statements=CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT ALL PRIVILEGES ON \`nova\\_api\`.* TO '{{name}}'@'%'; revocation_statements="
  assert_contains "the nova-cell role grants on nova and nova_cell0, in that order" "$written" \
    "roles/nova-cell-openstack db_name=nova-cell-openstack creation_statements=CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}'; GRANT ALL PRIVILEGES ON \`nova\`.* TO '{{name}}'@'%'; GRANT ALL PRIVILEGES ON \`nova\\_cell0\`.* TO '{{name}}'@'%'; revocation_statements="
}

# ---------------------------------------------------------------------------
# Test: both shipped nova rows follow a placed Nova
# ---------------------------------------------------------------------------
# The role is keyed on the SERVICE namespace, which the templated policy matches
# against the caller's own. A row keyed on the ControlPlane's namespace instead
# would be outside the reach of a Nova placed elsewhere.
test_shipped_nova_rows_follow_a_placed_namespace() {
  echo "Test: both shipped nova rows are keyed on the Nova service namespace"

  : >"$BAO_LOG"
  (
    export STUB_NOVA_NS=placed
    main
  ) >/dev/null 2>&1
  local written
  written="$(cat "$BAO_LOG")"

  assert_contains "the nova-api role follows the placed namespace" \
    "$written" "roles/nova-api-placed"
  assert_contains "the nova-cell role follows the placed namespace" \
    "$written" "roles/nova-cell-placed"

  # An absent namespace field falls back to the ControlPlane's own namespace.
  : >"$BAO_LOG"
  ( main ) >/dev/null 2>&1
  written="$(cat "$BAO_LOG")"

  assert_contains "an unplaced Nova keys the api role on the ControlPlane namespace" \
    "$written" "roles/nova-api-openstack"
  assert_contains "an unplaced Nova keys the cell role on the ControlPlane namespace" \
    "$written" "roles/nova-cell-openstack"
}

# ---------------------------------------------------------------------------
# Test: a dedicated nova database skips both shipped rows
# ---------------------------------------------------------------------------
# A dedicated database is Static-only, so neither engine role exists for it.
# Writing one of the two would leave half a tenant behind.
test_shipped_nova_rows_skip_on_a_dedicated_database() {
  echo "Test: main skips both shipped nova rows for a dedicated nova database"

  : >"$BAO_LOG"

  local exit_code written
  (
    export STUB_NOVA_DEDICATED_DB=nova-db
    main
  ) >/dev/null 2>&1
  exit_code=$?
  written="$(cat "$BAO_LOG")"

  assert_eq "main succeeds" "0" "$exit_code"
  assert_contains "the Keystone leg still runs" "$written" "roles/keystone-openstack"
  assert_not_contains "the nova-api row is skipped" "$written" "nova-api-"
  assert_not_contains "the nova-cell row is skipped" "$written" "nova-cell-"
}

# ---------------------------------------------------------------------------
# Test: no shipped role name is a hyphen-prefix of another
# ---------------------------------------------------------------------------
# The engine handles flatten to <role>-<namespace>. If one role name were a
# hyphen-prefix of another, two tenants could flatten to the same handle: a role
# nova in namespace api-x and a role nova-api in namespace x both give
# nova-api-x, so the second onboarding overwrites the first one's connection
# config and a token from either namespace reads the other's credentials.
test_shipped_role_names_are_prefix_free() {
  echo "Test: no shipped role name is a hyphen-prefix of another"

  local names=("keystone") entry role
  for entry in "${SERVICE_TENANTS[@]}"; do
    read -r role _ <<<"$entry"
    names+=("$role")
  done

  local a b violations=""
  for a in "${names[@]}"; do
    for b in "${names[@]}"; do
      if [[ "$a" != "$b" && "$b" == "$a-"* ]]; then
        violations+="role $a is a hyphen-prefix of $b: $a-<ns> is ambiguous"$'\n'
      fi
    done
  done

  assert_eq "no shipped role name is a hyphen-prefix of another" "" "$violations"
}

# ---------------------------------------------------------------------------
# Test: a nova row fails loudly when the root Secret is missing
# ---------------------------------------------------------------------------
# The wait set in hack/deploy-infra.sh exists because of this exit: a nova row
# whose service namespace has no MariaDB root Secret yet stops the onboarding
# instead of writing a role against an unresolvable root credential.
test_nova_row_without_root_secret_fails() {
  echo "Test: a nova row without a root Secret fails before any bao write"

  : >"$BAO_LOG"

  local output exit_code
  output="$( (
    export STUB_ROOT_SECRET_MISSING=true
    provision_service_tenant nova-api openstack openstack-db nova_api
  ) 2>&1 )"
  exit_code=$?

  assert_nonzero_exit "the missing root Secret exits non-zero" "$exit_code"
  assert_contains "the error names the Secret, its key and the namespace" "$output" \
    "ERROR: MariaDB root Secret 'openstack-db-root' (key 'password') not found or empty in namespace 'openstack'."
  assert_eq "no bao write is recorded" "" "$(cat "$BAO_LOG")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_multi_schema_grants_each_schema
test_single_schema_is_unchanged
test_empty_schema_list_fails
test_empty_schema_element_fails
test_service_tenants_rows_split_role_and_spec_service
test_dedicated_database_skips_every_row
test_shipped_nova_rows_are_provisioned
test_shipped_nova_rows_follow_a_placed_namespace
test_shipped_nova_rows_skip_on_a_dedicated_database
test_shipped_role_names_are_prefix_free
test_nova_row_without_root_secret_fails

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
