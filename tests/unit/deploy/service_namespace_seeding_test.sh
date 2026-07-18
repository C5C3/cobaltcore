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

# make_kubectl_full <dir> <glance_ns> <glance_dedicated_db>
# A kubectl stub that answers EVERY lookup setup-database-tenant.sh makes so both
# the keystone AND the glance legs run to completion — the bao writes reach
# OpenBao via `kubectl exec ... openbao-0`, which the stub swallows. The
# ControlPlane declares Glance (spec.services.glance non-empty); its service
# namespace resolves to <glance_ns> (empty defaults to the ControlPlane
# namespace) and it declares a dedicated glance database only when
# <glance_dedicated_db> is non-empty. Keystone stays namespace-unassigned, so its
# role is keyed on the ControlPlane namespace.
make_kubectl_full() {
  local dir="$1" glance_ns="$2" glance_dedicated_db="$3"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<STUB
#!/bin/bash
# The bao writes reach OpenBao via 'kubectl exec ... openbao-0'; swallow them.
if [[ "\$1" == "exec" ]]; then
  exit 0
fi
# Existence check: the ControlPlane is there.
if [[ "\$*" == *"get controlplane"* && "\$*" != *"jsonpath"* ]]; then
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.keystone.namespace.name}"* ]]; then
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.glance.dedicatedBackingServices.database}"* ]]; then
  printf '%s' "${glance_dedicated_db}"
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.glance.namespace.name}"* ]]; then
  printf '%s' "${glance_ns}"
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.glance}"* ]]; then
  # A non-empty object marks Glance as declared; its exact shape is irrelevant.
  printf 'map[namespace:map[name:%s]]' "${glance_ns}"
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
# MariaDB root-secret resolution (per service), then the root Secret payload.
if [[ "\$*" == *"get mariadb"* && "\$*" == *"rootPasswordSecretKeyRef.name"* ]]; then
  printf 'openstack-db-root'
  exit 0
fi
if [[ "\$*" == *"get mariadb"* && "\$*" == *"rootPasswordSecretKeyRef.key"* ]]; then
  printf 'password'
  exit 0
fi
if [[ "\$*" == *"get secret"* ]]; then
  # base64("root") — decoded by the script before the (swallowed) bao write.
  printf 'cm9vdA=='
  exit 0
fi
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
# Test: the glance engine role follows the Glance service namespace
# ---------------------------------------------------------------------------
test_db_tenant_provisions_glance_on_the_glance_namespace() {
  echo "Test: setup-database-tenant.sh keys the glance engine role on the Glance namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Glance declared, placed in its own "images" namespace, no dedicated database.
  make_kubectl_full "$tmp" "images" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "the glance engine role is keyed on the Glance service namespace" \
    "$output" "database/mariadb/roles/glance-images"
  assert_contains "the glance MariaDB is read from the Glance service namespace" \
    "$output" "openstack-db.images.svc:3306"
  assert_contains "keystone is still provisioned on the ControlPlane namespace" \
    "$output" "database/mariadb/roles/keystone-openstack"
  assert_not_contains "the glance role must not be keyed on the ControlPlane namespace" \
    "$output" "roles/glance-openstack"
}

# ---------------------------------------------------------------------------
# Test: an unplaced Glance defaults its engine role to the ControlPlane namespace
# ---------------------------------------------------------------------------
test_db_tenant_defaults_glance_to_the_controlplane_namespace() {
  echo "Test: setup-database-tenant.sh defaults the glance role to the ControlPlane namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Glance declared but not placed in a namespace of its own: role follows the CP.
  make_kubectl_full "$tmp" "" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "an unplaced Glance keeps the ControlPlane-namespace role" \
    "$output" "database/mariadb/roles/glance-openstack"
  assert_contains "the glance MariaDB is read from the ControlPlane namespace" \
    "$output" "openstack-db.openstack.svc:3306"
}

# ---------------------------------------------------------------------------
# Test: a dedicated glance database skips the glance engine leg
# ---------------------------------------------------------------------------
test_db_tenant_skips_glance_with_a_dedicated_database() {
  echo "Test: setup-database-tenant.sh skips the glance leg for a dedicated glance database"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Glance declared WITH a dedicated database — Static-only, so no engine role.
  make_kubectl_full "$tmp" "images" "glance-db"

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "the dedicated-database glance leg is skipped, loudly" \
    "$output" "skipping the glance database-engine tenant"
  assert_not_contains "no glance engine role is written for a dedicated database" \
    "$output" "database/mariadb/roles/glance-"
  assert_contains "keystone is still provisioned" \
    "$output" "database/mariadb/roles/keystone-openstack"
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
test_db_tenant_provisions_glance_on_the_glance_namespace
test_db_tenant_defaults_glance_to_the_controlplane_namespace
test_db_tenant_skips_glance_with_a_dedicated_database
test_seeder_rejects_a_malformed_identity

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
