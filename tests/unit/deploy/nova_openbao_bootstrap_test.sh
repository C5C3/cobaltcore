#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the OpenBao bootstrap legs a standalone Nova depends on.
#
#   - setup-auth.sh must write BOTH kubernetes/management roles, nova-api-db and
#     nova-cell-db, each bound to its own ServiceAccount name and its own
#     policy. The two roles are told apart by the SA name alone, so a role that
#     binds the other half's policy hands a Nova API generator the cell creds
#     path.
#   - each dynamic policy must grant exactly one path, its own per-tenant creds
#     path. A second path in either file widens what one leaked token reaches.
#   - the role prefix inside that path (nova-api, nova-cell) must be a shipped
#     SERVICE_TENANTS row: setup-database-tenant.sh derives the engine role name
#     from that column, so a prefix no row writes is a policy granting a path
#     that never exists.
#   - eso-tenant.hcl must grant the nova subtree, or the two kind-only shims
#     authenticate through the per-tenant store and read nothing.
#   - write-bootstrap-secrets.sh must seed TWO nova paths, one per database
#     block of a Nova, with two different SQL users, and must never rewrite an
#     existing one. A rewritten password leaves the materialized Secret naming a
#     credential the database no longer accepts.
#
# The scripts are driven with a recording kubectl stub, so nothing touches a
# cluster or an OpenBao. The stub answers the lookups each script makes on the
# way to the behavior under test and appends every invocation to a log file, so
# the assertions can check the exact arguments that reached the pod.
#
# Usage: bash tests/unit/deploy/nova_openbao_bootstrap_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
AUTH_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-auth.sh"
SEED_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/write-bootstrap-secrets.sh"
API_POLICY="$PROJECT_ROOT/deploy/openbao/policies/nova-api-db-dynamic.hcl"
CELL_POLICY="$PROJECT_ROOT/deploy/openbao/policies/nova-cell-db-dynamic.hcl"
ESO_TENANT_POLICY="$PROJECT_ROOT/deploy/openbao/policies/eso-tenant.hcl"

# The two standalone seeds: one per database block of a Nova. spec.database
# selects the cell credential, spec.apiDatabase the API one.
NOVA_DB_PATH="kv-v2/openstack/nova/openstack/standalone/db"
NOVA_API_DB_PATH="kv-v2/openstack/nova/openstack/standalone/api-db"

# The ACL template every per-tenant policy path is scoped by.
NS_TEMPLATE='{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}'

# A typical engine map of an already-bootstrapped instance.
ENGINES='{"kv-v2/":{},"pki/":{},"database/mariadb/":{}}'

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/service_tenants.sh
source "$PROJECT_ROOT/tests/lib/service_tenants.sh"

# make_kubectl <dir> <log> <secrets_json> <readable_kv_path>
# Installs a recording kubectl stub in <dir>. Every invocation's argument string
# lands in <log>; the canned answers are the ones the scripts branch on:
#
#   - `bao secrets list` returns <secrets_json>, the engine map.
#   - `bao auth list` returns an empty map, so setup-auth.sh enables its mounts
#     (the enables themselves are recorded and swallowed).
#   - `bao kv get` succeeds only for <readable_kv_path>, the existence probe
#     write_secret_if_missing reads. Every other path reports a miss and is
#     therefore seeded.
#
# Everything else (mount enables, role writes, kv puts, metadata puts) is
# recorded and answered with exit 0, so each script runs to completion.
make_kubectl() {
  local dir="$1" log="$2" secrets_json="$3" readable_kv_path="$4"
  mkdir -p "$dir"
  : >"$log"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"${log}"

case "\$*" in
  *"bao secrets list"*)
    printf '%s' '${secrets_json}'
    ;;
  *"bao auth list"*)
    printf '%s' '{}'
    ;;
  *"bao kv get"*)
    if [[ -n "${readable_kv_path}" && "\$*" == *"${readable_kv_path}"* ]]; then
      printf '%s' '{"data":{"data":{}}}'
      exit 0
    fi
    exit 1
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# make_failing_kubectl <dir> <log>
# A recording kubectl stub whose every invocation fails: the OpenBao pod is
# unreachable.
make_failing_kubectl() {
  local dir="$1" log="$2"
  mkdir -p "$dir"
  : >"$log"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"${log}"
exit 1
STUB
  chmod +x "$dir/kubectl"
}

# run_bootstrap <stub_dir> <script>
# Runs a bootstrap script with the kubectl stub prepended to PATH. Echoes the
# combined stdout/stderr; returns the script's exit code. KORC_CONTROLPLANES is
# pinned so a change to the script's default identity cannot move these tests.
run_bootstrap() {
  local stub_dir="$1" script="$2"
  (
    PATH="$stub_dir:$PATH"
    BAO_TOKEN="dummy-token"
    KORC_CONTROLPLANES="openstack/controlplane"
    export PATH BAO_TOKEN KORC_CONTROLPLANES
    bash "$script"
  ) 2>&1
}

# policy_path_line <policy_file>
# Prints the single `path "..." {` line of a dynamic policy.
policy_path_line() {
  grep '^path ' "$1"
}

# ---------------------------------------------------------------------------
# Test: both nova auth roles are written, each bound to its own half
# ---------------------------------------------------------------------------
test_writes_both_nova_auth_roles() {
  echo "Test: setup-auth.sh writes the nova-api-db and nova-cell-db roles"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES" ""

  local exit_code
  run_bootstrap "$tmp" "$AUTH_SCRIPT" >/dev/null
  exit_code=$?
  assert_eq "the run succeeds" "0" "$exit_code"

  local half role_call role_calls
  for half in api cell; do
    role_calls="$(grep -c "auth/kubernetes/management/role/nova-${half}-db" "$tmp/calls.log")"
    role_call="$(grep "auth/kubernetes/management/role/nova-${half}-db" "$tmp/calls.log")"

    assert_eq "the nova-${half}-db role is written exactly once" "1" "$role_calls"
    assert_contains "the nova-${half}-db role is an upsert on the management mount" \
      "$role_call" "bao write auth/kubernetes/management/role/nova-${half}-db"
    # The SA name is the only thing telling the two roles apart: both bind "*"
    # namespaces, so a swapped name hands one generator the other's creds path.
    assert_contains "the nova-${half}-db role binds its own ServiceAccount name" \
      "$role_call" "bound_service_account_names=nova-${half}-db-creds"
    assert_contains "the nova-${half}-db role admits any tenant namespace" \
      "$role_call" "bound_service_account_namespaces=*"
    assert_contains "the nova-${half}-db role binds its own dynamic policy" \
      "$role_call" "token_policies=nova-${half}-db-dynamic"
    # 72h on both TTLs: the token has to outlive the credential lease it reads.
    assert_contains "the nova-${half}-db token lives for 72h" \
      "$role_call" "token_ttl=72h"
    assert_contains "the nova-${half}-db token cannot be renewed past 72h" \
      "$role_call" "token_max_ttl=72h"
  done
}

# ---------------------------------------------------------------------------
# Test: each nova policy grants exactly its own creds path
# ---------------------------------------------------------------------------
test_nova_policies_grant_exactly_their_creds_path() {
  echo "Test: the nova dynamic policies grant exactly one creds path each"

  local half policy path_count path_line
  for half in api cell; do
    policy="$PROJECT_ROOT/deploy/openbao/policies/nova-${half}-db-dynamic.hcl"
    if [[ ! -f "$policy" ]]; then
      echo "  FAIL: $policy does not exist, so the nova-${half}-db role is bound to a policy that is never written"
      FAIL=$((FAIL + 1))
      continue
    fi

    path_count="$(grep -c '^path ' "$policy")"
    path_line="$(policy_path_line "$policy")"

    assert_eq "nova-${half}-db-dynamic grants a single path" "1" "$path_count"
    assert_eq "nova-${half}-db-dynamic grants its own per-tenant creds path" \
      "path \"database/mariadb/creds/nova-${half}-${NS_TEMPLATE}\" {" "$path_line"
    # A dynamic engine has no static password to push back, so read is all the
    # generator ever needs.
    assert_file_contains "nova-${half}-db-dynamic grants read and nothing more" \
      "$policy" 'capabilities = \["read"\]'
  done
}

# ---------------------------------------------------------------------------
# Test: the role prefix each policy grants is a shipped SERVICE_TENANTS row
# ---------------------------------------------------------------------------
# setup-database-tenant.sh builds the engine role name as <role>-<namespace>
# from the first column of SERVICE_TENANTS. A policy whose prefix no row carries
# grants a creds path the engine never provisions, and the generator reading it
# gets a 404 instead of a credential.
test_policy_role_prefixes_are_shipped_rows() {
  echo "Test: the creds-path prefixes of the nova policies are SERVICE_TENANTS rows"

  local roles
  roles="$(service_tenants_column 1)"
  assert_not_empty "the SERVICE_TENANTS role column is readable" "$roles"

  local policy prefix
  for policy in "$API_POLICY" "$CELL_POLICY"; do
    prefix="$(policy_path_line "$policy" \
      | sed -n 's|^path "database/mariadb/creds/\(.*\)-{{.*|\1|p')"
    assert_not_empty "$(basename "$policy") carries a creds-path prefix" "$prefix"
    if grep -qx "$prefix" <<<"$roles"; then
      echo "  PASS: SERVICE_TENANTS ships a '$prefix' row"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: SERVICE_TENANTS ships no '$prefix' row, so $(basename "$policy") grants a path the engine never provisions"
      FAIL=$((FAIL + 1))
    fi
  done
}

# ---------------------------------------------------------------------------
# Test: the per-tenant ESO identity reaches the nova subtree
# ---------------------------------------------------------------------------
# Both kind-only shims read through the namespaced openbao-tenant-store, which
# authenticates as eso-tenant. One wildcard grant covers the cell path and the
# API path, because both hang under openstack/nova/{namespace}.
test_eso_tenant_grants_the_nova_subtree() {
  echo "Test: eso-tenant.hcl grants the nova subtree of the caller's namespace"

  assert_file_contains "the per-tenant ESO identity may read openstack/nova/{ns}/*" \
    "$ESO_TENANT_POLICY" \
    "path \"kv-v2/data/openstack/nova/${NS_TEMPLATE}/\*\""
}

# ---------------------------------------------------------------------------
# Test: both nova paths are seeded when they are missing
# ---------------------------------------------------------------------------
# A Nova has two database blocks and each carries its own SQL user, so one seed
# cannot serve both: a brownfield Nova reads username from the Secret, and the
# API database is owned by nova_api while the cell database is owned by nova.
test_seeds_both_nova_paths_when_missing() {
  echo "Test: write-bootstrap-secrets.sh seeds both nova database paths when they are missing"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # No path is readable, so every probe reports a miss.
  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES" ""

  local exit_code cell_call api_call
  run_bootstrap "$tmp" "$SEED_SCRIPT" >/dev/null
  exit_code=$?
  cell_call="$(grep "bao kv put" "$tmp/calls.log" | grep "BAO_KV_PATH=$NOVA_DB_PATH")"
  api_call="$(grep "bao kv put" "$tmp/calls.log" | grep "BAO_KV_PATH=$NOVA_API_DB_PATH")"

  assert_eq "the run succeeds" "0" "$exit_code"

  assert_not_empty "the cell database credential is written" "$cell_call"
  assert_contains "the cell credential names the nova SQL user" \
    "$cell_call" "username='nova'"
  assert_contains "the cell password is generated inside the pod, never on the host" \
    "$cell_call" "sys/tools/random/32"

  assert_not_empty "the API database credential is written" "$api_call"
  assert_contains "the API credential names the nova_api SQL user" \
    "$api_call" "username='nova_api'"
  assert_contains "the API password is generated inside the pod, never on the host" \
    "$api_call" "sys/tools/random/32"
}

# ---------------------------------------------------------------------------
# Test: an existing API seed is never rewritten
# ---------------------------------------------------------------------------
# The seeded password is the one the SQL user was created with. Rewriting it on
# a re-run leaves the materialized nova-api-db Secret naming a credential the
# database rejects, and the Nova API loses its database until the user is
# recreated by hand.
test_never_rewrites_an_existing_api_db_seed() {
  echo "Test: write-bootstrap-secrets.sh never rewrites an existing nova API seed"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # The API path is readable; every other seed path still reports a miss.
  make_kubectl "$tmp" "$tmp/calls.log" "$ENGINES" "$NOVA_API_DB_PATH"

  local output exit_code puts
  output="$(run_bootstrap "$tmp" "$SEED_SCRIPT")"
  exit_code=$?
  puts="$(grep "bao kv put" "$tmp/calls.log")"

  assert_eq "the re-run succeeds" "0" "$exit_code"
  assert_contains "the skip is logged for the API path" \
    "$output" "Secret '$NOVA_API_DB_PATH' already exists"
  assert_not_contains "the stored API credential is left untouched" \
    "$puts" "BAO_KV_PATH=$NOVA_API_DB_PATH"
  assert_contains "the missing cell credential is still seeded" \
    "$puts" "BAO_KV_PATH=$NOVA_DB_PATH"
}

# ---------------------------------------------------------------------------
# Test: an unreachable OpenBao aborts the seed run
# ---------------------------------------------------------------------------
# Reporting a seeded path that was never written sends the deploy on to the ESO
# shims, which then sit in SecretSyncedError with no seeded source.
test_seed_aborts_when_openbao_is_unreachable() {
  echo "Test: write-bootstrap-secrets.sh aborts when OpenBao is unreachable"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_failing_kubectl "$tmp" "$tmp/calls.log"

  local output exit_code
  output="$(run_bootstrap "$tmp" "$SEED_SCRIPT")"
  exit_code=$?

  assert_nonzero_exit "the script exits non-zero" "$exit_code"
  assert_not_contains "no cell credential is reported as written" \
    "$output" "Secret '$NOVA_DB_PATH' written."
  assert_not_contains "no API credential is reported as written" \
    "$output" "Secret '$NOVA_API_DB_PATH' written."
  assert_not_contains "the run does not report success" "$output" "=== Done ==="
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_writes_both_nova_auth_roles
test_nova_policies_grant_exactly_their_creds_path
test_policy_role_prefixes_are_shipped_rows
test_eso_tenant_grants_the_nova_subtree
test_seeds_both_nova_paths_when_missing
test_never_rewrites_an_existing_api_db_seed
test_seed_aborts_when_openbao_is_unreachable

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
