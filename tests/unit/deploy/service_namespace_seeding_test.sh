#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the two OpenBao bootstrap scripts key their per-tenant handles on the
# KEYSTONE SERVICE NAMESPACE, not on the ControlPlane's namespace.
#
# The distinction only exists once spec.services.keystone.namespace places the
# Keystone service in a namespace of its own. Then:
#
#   - setup-database-tenant.sh must provision the database-engine role as
#     keystone-<keystone-ns> and read the MariaDB from that namespace. The
#     generator's ServiceAccount authenticates from there, and the templated
#     keystone-db-dynamic policy grants exactly the caller's own namespace — a
#     role keyed on the ControlPlane's namespace is outside its reach, so the
#     credential can never be issued.
#   - write-bootstrap-secrets.sh must seed the admin password at
#     bootstrap/<keystone-ns>/<cp>-keystone/admin, matching
#     adminPasswordRemoteKeyFor in the c5c3 operator and the keystone-operator's
#     rotation PushSecret. Seeding under the ControlPlane's namespace would write
#     a path nothing reads.
#
# Both scripts are driven with stubs, so nothing touches a cluster or an OpenBao.
#
# Usage: bash tests/unit/deploy/service_namespace_seeding_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DB_TENANT_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/setup-database-tenant.sh"
SEED_SCRIPT="$PROJECT_ROOT/deploy/openbao/bootstrap/write-bootstrap-secrets.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# make_kubectl <dir> <keystone_ns>
# Installs a kubectl stub that answers the lookups setup-database-tenant.sh makes.
# The ControlPlane resolves spec.services.keystone.namespace.name to <keystone_ns>
# (empty for an unassigned Keystone); every MariaDB / Secret lookup fails, so the
# script exits before touching OpenBao — by then it has already logged which
# namespace it resolved, which is what this test asserts.
make_kubectl() {
  local dir="$1" keystone_ns="$2"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# Existence check: the ControlPlane is there.
if [[ "\$*" == *"get controlplane"* && "\$*" != *"jsonpath"* ]]; then
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.keystone.namespace.name}"* ]]; then
  printf '%s' "${keystone_ns}"
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.infrastructure.database.clusterRef.name}"* ]]; then
  printf 'openstack-db'
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.infrastructure.database.database}"* ]]; then
  printf 'keystone'
  exit 0
fi
# Every MariaDB / Secret lookup fails: the script exits before reaching OpenBao.
exit 1
STUB
  chmod +x "$dir/kubectl"
}

# run_db_tenant <stub_dir> <namespace> <controlplane>
run_db_tenant() {
  local stub_dir="$1" ns="$2" cp="$3"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    BAO_TOKEN="dummy-token"
    export BAO_TOKEN
    bash "$DB_TENANT_SCRIPT" "$ns" "$cp"
  ) 2>&1
}

# ---------------------------------------------------------------------------
# Test: the engine role follows the Keystone service namespace
# ---------------------------------------------------------------------------
test_db_tenant_uses_the_service_namespace() {
  echo "Test: setup-database-tenant.sh keys the engine role on the Keystone namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_kubectl "$tmp" "identity"

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "the engine role is keyed on the Keystone service namespace" \
    "$output" "database/mariadb/roles/keystone-identity"
  assert_contains "the resolved service namespace is reported" \
    "$output" "Service NS: identity"
  assert_contains "the MariaDB is read from the Keystone service namespace" \
    "$output" "openstack-db.identity.svc:3306"
  assert_not_contains "the role must not be keyed on the ControlPlane namespace" \
    "$output" "roles/keystone-openstack"
}

# ---------------------------------------------------------------------------
# Test: an unassigned Keystone keeps today's derivation
# ---------------------------------------------------------------------------
test_db_tenant_defaults_to_the_controlplane_namespace() {
  echo "Test: setup-database-tenant.sh defaults to the ControlPlane namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # No namespace assignment: the jsonpath resolves empty.
  make_kubectl "$tmp" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "an unassigned Keystone keeps the ControlPlane-namespace role" \
    "$output" "database/mariadb/roles/keystone-openstack"
  assert_contains "the MariaDB is read from the ControlPlane namespace" \
    "$output" "openstack-db.openstack.svc:3306"
}

# ---------------------------------------------------------------------------
# Test: the seeder parses the optional Keystone-namespace segment
# ---------------------------------------------------------------------------
# write-bootstrap-secrets.sh reaches OpenBao almost immediately, so it is driven
# only as far as the identity parsing: a malformed entry must fail loudly, and a
# well-formed three-segment entry must get past the parse. The path derivation the
# parse feeds is asserted against the operator's own adminPasswordRemoteKeyFor in
# the Go unit tests (TestAdminPasswordRemoteKey_FollowsTheKeystoneNamespace).
run_seeder() {
  local stub_dir="$1" identities="$2"
  (
    PATH="$stub_dir:$PATH"
    export PATH
    BAO_TOKEN="dummy-token"
    KORC_CONTROLPLANES="$identities"
    export BAO_TOKEN KORC_CONTROLPLANES
    bash "$SEED_SCRIPT"
  ) 2>&1
}

test_seeder_rejects_a_malformed_identity() {
  echo "Test: write-bootstrap-secrets.sh rejects a malformed KORC_CONTROLPLANES entry"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # A permissive stub: every `kubectl exec` into the OpenBao pod "succeeds" and
  # prints nothing, so the script walks past the infrastructure writes and reaches
  # the KORC_CONTROLPLANES loop — the identity parse is what this exercises.
  cat >"$tmp/kubectl" <<'STUB'
#!/bin/bash
exit 0
STUB
  chmod +x "$tmp/kubectl"

  local output exit_code
  output="$(run_seeder "$tmp" "no-slash-here")"
  exit_code=$?

  assert_nonzero_exit "a slashless identity is rejected" "$exit_code"
  assert_contains "the error names the accepted form, including the optional segment" \
    "$output" "<namespace>/<controlplane>[/<keystone-namespace>]"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_db_tenant_uses_the_service_namespace
test_db_tenant_defaults_to_the_controlplane_namespace
test_seeder_rejects_a_malformed_identity

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
