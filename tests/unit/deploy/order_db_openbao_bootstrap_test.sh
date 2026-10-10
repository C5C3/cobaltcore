#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the OpenBao bootstrap legs the MariaDBDatabase orders depend on.
#
#   - setup-auth.sh must write the order-db role the order generators log in
#     with, bound to the order-db-creds ServiceAccount in any namespace and to
#     the order-db-dynamic policy, with 72h tokens that outlive the credential
#     lease.
#   - setup-auth.sh must write the c5c3-operator role, bound to the operator's
#     one ServiceAccount in its one namespace (c5c3-operator in c5c3-system, or
#     the two environment overrides) and to the c5c3-operator policy, with
#     15-minute tokens.
#   - order-db-dynamic.hcl must grant exactly one path, the order creds paths of
#     the caller's own namespace with the dot separators, read-only.
#   - c5c3-operator.hcl must grant nothing under database/mariadb/config: the
#     connections and the root password stay with the onboarding script.
#   - setup-database-tenant.sh must widen allowed_roles on the Keystone
#     connection by the order glob of the ControlPlane namespace, and on no
#     other connection.
#
# setup-auth.sh runs against a recording kubectl stub; setup-database-tenant.sh
# is sourced and its bao_exec / bao_exec_stdin record their arguments, so
# nothing touches a cluster or an OpenBao.
#
# Usage: bash tests/unit/deploy/order_db_openbao_bootstrap_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
AUTH_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-auth.sh"
TENANT_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-database-tenant.sh"
ORDER_POLICY="$PROJECT_ROOT/deploy/openbao/policies/order-db-dynamic.hcl"
OPERATOR_POLICY="$PROJECT_ROOT/deploy/openbao/policies/c5c3-operator.hcl"

# The ACL template every per-tenant policy path is scoped by.
NS_TEMPLATE='{{identity.entity.aliases.KUBERNETES_MANAGEMENT_ACCESSOR.metadata.service_account_namespace}}'

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# make_kubectl <dir> <log>
# Installs a recording kubectl stub in <dir>. Every invocation's argument string
# lands in <log>. `bao auth list` answers an empty map, so setup-auth.sh enables
# its mounts; every other call is recorded and answered with exit 0.
make_kubectl() {
  local dir="$1" log="$2"
  mkdir -p "$dir"
  : >"$log"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
printf '%s\n' "\$*" >>"${log}"
case "\$*" in
  *"bao auth list"*)
    printf '%s' '{}'
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# run_auth <stub_dir> [VAR=value ...]
# Runs setup-auth.sh with the kubectl stub prepended to PATH and the given
# environment. Returns the script's exit code.
run_auth() {
  local stub_dir="$1"
  shift
  (
    PATH="$stub_dir:$PATH"
    BAO_TOKEN="dummy-token"
    export PATH BAO_TOKEN
    local assignment
    for assignment in "$@"; do
      export "${assignment?}"
    done
    bash "$AUTH_SCRIPT"
  ) >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# Test: the order-db role
# ---------------------------------------------------------------------------
test_writes_the_order_db_role() {
  echo "Test: setup-auth.sh writes the order-db role"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_kubectl "$tmp" "$tmp/calls.log"

  run_auth "$tmp"
  assert_eq "the run succeeds" "0" "$?"

  local role_calls role_call
  role_calls="$(grep -c 'auth/kubernetes/management/role/order-db ' "$tmp/calls.log")"
  role_call="$(grep 'auth/kubernetes/management/role/order-db ' "$tmp/calls.log")"

  assert_eq "the order-db role is written exactly once" "1" "$role_calls"
  assert_contains "the order-db role binds the order-db-creds ServiceAccount" \
    "$role_call" "bound_service_account_names=order-db-creds"
  assert_contains "the order-db role admits any ControlPlane namespace" \
    "$role_call" "bound_service_account_namespaces=*"
  assert_contains "the order-db role binds the order-db-dynamic policy" \
    "$role_call" "token_policies=order-db-dynamic"
  # 72h on both TTLs: the token has to outlive the credential lease it mints.
  assert_contains "the order-db token lives for 72h" "$role_call" "token_ttl=72h"
  assert_contains "the order-db token cannot be renewed past 72h" "$role_call" "token_max_ttl=72h"
}

# ---------------------------------------------------------------------------
# Test: the c5c3-operator role and its overrides
# ---------------------------------------------------------------------------
test_writes_the_c5c3_operator_role() {
  echo "Test: setup-auth.sh writes the c5c3-operator role"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "$tmp/calls.log"
  run_auth "$tmp"
  assert_eq "the run succeeds" "0" "$?"

  local role_calls role_call
  role_calls="$(grep -c 'auth/kubernetes/management/role/c5c3-operator ' "$tmp/calls.log")"
  role_call="$(grep 'auth/kubernetes/management/role/c5c3-operator ' "$tmp/calls.log")"

  assert_eq "the c5c3-operator role is written exactly once" "1" "$role_calls"
  assert_contains "the role binds the operator's ServiceAccount by default" \
    "$role_call" "bound_service_account_names=c5c3-operator "
  assert_contains "the role binds the operator's namespace by default" \
    "$role_call" "bound_service_account_namespaces=c5c3-system "
  assert_not_contains "the role admits no other namespace" \
    "$role_call" "bound_service_account_namespaces=*"
  assert_contains "the role binds the c5c3-operator policy" \
    "$role_call" "token_policies=c5c3-operator"
  assert_contains "the operator token lives for 15m" "$role_call" "token_ttl=15m"
  assert_contains "the operator token cannot be renewed past 15m" "$role_call" "token_max_ttl=15m"

  # A deployment that runs the operator under another identity names it.
  make_kubectl "$tmp" "$tmp/calls.log"
  run_auth "$tmp" C5C3_OPERATOR_SERVICE_ACCOUNT=op C5C3_OPERATOR_NAMESPACE=ops
  assert_eq "the overridden run succeeds" "0" "$?"
  role_call="$(grep 'auth/kubernetes/management/role/c5c3-operator ' "$tmp/calls.log")"
  assert_contains "the ServiceAccount override reaches the role" \
    "$role_call" "bound_service_account_names=op "
  assert_contains "the namespace override reaches the role" \
    "$role_call" "bound_service_account_namespaces=ops "
}

# ---------------------------------------------------------------------------
# Test: order-db-dynamic grants exactly the caller's own order creds paths
# ---------------------------------------------------------------------------
test_order_policy_grants_one_namespaced_path() {
  echo "Test: order-db-dynamic.hcl grants exactly one path"

  if [[ ! -f "$ORDER_POLICY" ]]; then
    echo "  FAIL: $ORDER_POLICY does not exist, so the order-db role binds a policy that is never written"
    FAIL=$((FAIL + 1))
    return
  fi

  local path_count path_line path
  path_count="$(grep -c '^path ' "$ORDER_POLICY")"
  path_line="$(grep '^path ' "$ORDER_POLICY")"
  path="$(sed -n 's|^path "\(.*\)" {$|\1|p' <<<"$path_line")"

  assert_eq "order-db-dynamic grants a single path" "1" "$path_count"
  assert_starts_with "the path is under the order creds paths" "$path" "database/mariadb/creds/order."
  # The namespace segment is closed by a dot before the glob: a namespace
  # cannot contain one, so the glob cannot reach a namespace that merely starts
  # with the caller's.
  assert_eq "the path is the caller's namespace between two dots, then the glob" \
    "database/mariadb/creds/order.${NS_TEMPLATE}.*" "$path"
  assert_file_contains "order-db-dynamic grants read and nothing more" \
    "$ORDER_POLICY" 'capabilities = \["read"\]'
}

# ---------------------------------------------------------------------------
# Test: the operator's policy stays off the engine connections
# ---------------------------------------------------------------------------
test_operator_policy_reaches_roles_and_leases_only() {
  echo "Test: c5c3-operator.hcl grants the order roles and leases, nothing under config"

  if [[ ! -f "$OPERATOR_POLICY" ]]; then
    echo "  FAIL: $OPERATOR_POLICY does not exist, so the c5c3-operator role binds a policy that is never written"
    FAIL=$((FAIL + 1))
    return
  fi

  local paths
  paths="$(sed -n 's|^path "\(.*\)" {$|\1|p' "$OPERATOR_POLICY")"

  assert_not_contains "no path under database/mariadb/config" "$paths" "database/mariadb/config"
  assert_eq "exactly the order roles and the order lease revocation" \
    "database/mariadb/roles/order.*"$'\n'"sys/leases/revoke-prefix/database/mariadb/creds/order.*" "$paths"
  assert_file_contains "revoke-prefix carries sudo" "$OPERATOR_POLICY" 'capabilities = \["update", "sudo"\]'
}

# ---------------------------------------------------------------------------
# Test: setup-database-tenant.sh widens allowed_roles on the Keystone connection
# ---------------------------------------------------------------------------
test_tenant_widens_the_keystone_connection_alone() {
  echo "Test: setup-database-tenant.sh admits the order roles on the Keystone connection alone"

  local tmp
  tmp="$(mktemp -d)"
  local bao_log="$tmp/bao.log"
  : >"$bao_log"

  # The ControlPlane lives in "openstack" and declares spec.services.glance; its
  # Keystone is placed in "identity", so the connection is keyed on that
  # namespace while the order glob names the ControlPlane's.
  cat >"$tmp/kubectl" <<'STUB'
#!/bin/bash
if [[ "$*" == *"get controlplane"* ]]; then
  case "$*" in
    *"jsonpath={.spec.services.keystone.namespace.name}") printf 'identity' ;;
    *"jsonpath={.spec.services.glance}") printf 'map[]' ;;
  esac
  exit 0
fi
if [[ "$*" == *"get mariadb"* && "$*" == *"rootPasswordSecretKeyRef.name"* ]]; then
  printf 'openstack-db-root'
  exit 0
fi
if [[ "$*" == *"get secret"* ]]; then
  printf 'cm9vdA=='
  exit 0
fi
exit 1
STUB
  chmod +x "$tmp/kubectl"

  local written
  (
    PATH="$tmp:$PATH"
    export PATH BAO_TOKEN="dummy-token"
    # shellcheck source=deploy/openbao/bootstrap/setup-database-tenant.sh
    source "$TENANT_SCRIPT" openstack cp
    set +e
    bao_exec() { printf '%s\n' "$*" >>"$bao_log"; }
    bao_exec_stdin() {
      cat >/dev/null
      printf '%s\n' "$*" >>"$bao_log"
    }
    main
  ) >/dev/null 2>&1
  assert_eq "main succeeds" "0" "$?"
  written="$(cat "$bao_log")"

  assert_contains "the Keystone connection admits its own role and the order glob" "$written" \
    "database/mariadb/config/keystone-identity plugin_name=mysql-database-plugin connection_url={{username}}:{{password}}@tcp(openstack-db.identity.svc:3306)/ allowed_roles=keystone-identity,order.openstack.* "
  assert_contains "the Glance connection admits its own role alone" "$written" \
    "database/mariadb/config/glance-openstack plugin_name=mysql-database-plugin connection_url={{username}}:{{password}}@tcp(openstack-db.openstack.svc:3306)/ allowed_roles=glance-openstack "
  assert_eq "only one connection carries the order glob" "1" "$(grep -c 'order\.' "$bao_log")"

  rm -rf "$tmp"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_writes_the_order_db_role
test_writes_the_c5c3_operator_role
test_order_policy_grants_one_namespaced_path
test_operator_policy_reaches_roles_and_leases_only
test_tenant_widens_the_keystone_connection_alone

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
