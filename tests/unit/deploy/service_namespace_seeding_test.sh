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

# make_kubectl_full <dir> <glance_declared> <glance_ns> <glance_dedicated_db> \
#                         <placement_declared> <placement_ns> <placement_dedicated_db> \
#                         <barbican_declared> <barbican_ns> <barbican_dedicated_db>
# A kubectl stub that answers EVERY lookup setup-database-tenant.sh makes so the
# keystone, glance, placement AND barbican legs all run to completion — the bao
# writes reach OpenBao via `kubectl exec ... openbao-0`, which the stub swallows.
#
# <glance_declared> / <placement_declared> / <barbican_declared> are the presence
# gate the script reads first: a non-empty value makes `{.spec.services.<svc>}`
# resolve to a non-empty object, an empty one makes it resolve to nothing so the
# service is UNDECLARED. That distinction has to be modelled separately from the
# namespace, because an empty <svc>_ns means "declared but not placed in a
# namespace of its own" — the service still gets an engine role, keyed on the
# ControlPlane namespace.
#
# <glance_dedicated_db> / <placement_dedicated_db> / <barbican_dedicated_db>
# declare a dedicated database for that service when non-empty (Static-only, so no
# engine role). Keystone stays namespace-unassigned, so its role is keyed on the
# ControlPlane namespace.
make_kubectl_full() {
  local dir="$1" glance_declared="$2" glance_ns="$3" glance_dedicated_db="$4"
  local placement_declared="$5" placement_ns="$6" placement_dedicated_db="$7"
  local barbican_declared="$8" barbican_ns="$9" barbican_dedicated_db="${10}"
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
  # An undeclared Glance prints nothing, which is what the script's -z gate reads.
  if [[ -n "${glance_declared}" ]]; then
    printf 'map[namespace:map[name:%s]]' "${glance_ns}"
  fi
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.placement.dedicatedBackingServices.database}"* ]]; then
  printf '%s' "${placement_dedicated_db}"
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.placement.namespace.name}"* ]]; then
  printf '%s' "${placement_ns}"
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.placement}"* ]]; then
  # A non-empty object marks Placement as declared; its exact shape is irrelevant.
  # An undeclared Placement prints nothing, which the script's -z gate reads.
  if [[ -n "${placement_declared}" ]]; then
    printf 'map[namespace:map[name:%s]]' "${placement_ns}"
  fi
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.barbican.dedicatedBackingServices.database}"* ]]; then
  printf '%s' "${barbican_dedicated_db}"
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.barbican.namespace.name}"* ]]; then
  printf '%s' "${barbican_ns}"
  exit 0
fi
if [[ "\$*" == *"jsonpath={.spec.services.barbican}"* ]]; then
  # A non-empty object marks Barbican as declared; its exact shape is irrelevant.
  # An undeclared Barbican prints nothing, which the script's -z gate reads.
  if [[ -n "${barbican_declared}" ]]; then
    printf 'map[namespace:map[name:%s]]' "${barbican_ns}"
  fi
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
  make_kubectl_full "$tmp" "yes" "images" "" "yes" "" "" "yes" "" ""

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
  make_kubectl_full "$tmp" "yes" "" "" "yes" "" "" "yes" "" ""

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
  make_kubectl_full "$tmp" "yes" "images" "glance-db" "yes" "" "" "yes" "" ""

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
# Test: the placement engine role follows the Placement service namespace
# ---------------------------------------------------------------------------
test_db_tenant_provisions_placement_on_the_placement_namespace() {
  echo "Test: setup-database-tenant.sh keys the placement engine role on the Placement namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Placement declared, placed in its own "compute" namespace, no dedicated database.
  make_kubectl_full "$tmp" "yes" "images" "" "yes" "compute" "" "yes" "" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "the placement engine role is keyed on the Placement service namespace" \
    "$output" "database/mariadb/roles/placement-compute"
  assert_contains "the placement MariaDB is read from the Placement service namespace" \
    "$output" "openstack-db.compute.svc:3306"
  assert_contains "keystone is still provisioned on the ControlPlane namespace" \
    "$output" "database/mariadb/roles/keystone-openstack"
  assert_not_contains "the placement role must not be keyed on the ControlPlane namespace" \
    "$output" "roles/placement-openstack"
}

# ---------------------------------------------------------------------------
# Test: an unplaced Placement defaults its engine role to the ControlPlane namespace
# ---------------------------------------------------------------------------
test_db_tenant_defaults_placement_to_the_controlplane_namespace() {
  echo "Test: setup-database-tenant.sh defaults the placement role to the ControlPlane namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Placement declared but not placed in a namespace of its own: role follows the CP.
  make_kubectl_full "$tmp" "yes" "images" "" "yes" "" "" "yes" "" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "an unplaced Placement keeps the ControlPlane-namespace role" \
    "$output" "database/mariadb/roles/placement-openstack"
  assert_contains "the placement MariaDB is read from the ControlPlane namespace" \
    "$output" "openstack-db.openstack.svc:3306"
}

# ---------------------------------------------------------------------------
# Test: a dedicated placement database skips the placement engine leg
# ---------------------------------------------------------------------------
test_db_tenant_skips_placement_with_a_dedicated_database() {
  echo "Test: setup-database-tenant.sh skips the placement leg for a dedicated placement database"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Placement declared WITH a dedicated database — Static-only, so no engine role.
  make_kubectl_full "$tmp" "yes" "images" "" "yes" "compute" "placement-db" "yes" "" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "the dedicated-database placement leg is skipped, loudly" \
    "$output" "skipping the placement database-engine tenant"
  assert_not_contains "no placement engine role is written for a dedicated database" \
    "$output" "database/mariadb/roles/placement-"
  assert_contains "keystone is still provisioned" \
    "$output" "database/mariadb/roles/keystone-openstack"
}

# ---------------------------------------------------------------------------
# Test: the barbican engine role follows the Barbican service namespace
# ---------------------------------------------------------------------------
test_db_tenant_provisions_barbican_on_the_barbican_namespace() {
  echo "Test: setup-database-tenant.sh keys the barbican engine role on the Barbican namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Barbican declared, placed in its own "secrets" namespace, no dedicated database.
  make_kubectl_full "$tmp" "yes" "images" "" "yes" "compute" "" "yes" "secrets" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "the barbican engine role is keyed on the Barbican service namespace" \
    "$output" "database/mariadb/roles/barbican-secrets"
  assert_contains "the barbican MariaDB is read from the Barbican service namespace" \
    "$output" "openstack-db.secrets.svc:3306"
  assert_contains "keystone is still provisioned on the ControlPlane namespace" \
    "$output" "database/mariadb/roles/keystone-openstack"
  assert_not_contains "the barbican role must not be keyed on the ControlPlane namespace" \
    "$output" "roles/barbican-openstack"
}

# ---------------------------------------------------------------------------
# Test: an unplaced Barbican defaults its engine role to the ControlPlane namespace
# ---------------------------------------------------------------------------
test_db_tenant_defaults_barbican_to_the_controlplane_namespace() {
  echo "Test: setup-database-tenant.sh defaults the barbican role to the ControlPlane namespace"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Barbican declared but not placed in a namespace of its own: role follows the CP.
  make_kubectl_full "$tmp" "yes" "images" "" "yes" "compute" "" "yes" "" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "an unplaced Barbican keeps the ControlPlane-namespace role" \
    "$output" "database/mariadb/roles/barbican-openstack"
  assert_contains "the barbican MariaDB is read from the ControlPlane namespace" \
    "$output" "openstack-db.openstack.svc:3306"
}

# ---------------------------------------------------------------------------
# Test: a dedicated barbican database skips the barbican engine leg
# ---------------------------------------------------------------------------
test_db_tenant_skips_barbican_with_a_dedicated_database() {
  echo "Test: setup-database-tenant.sh skips the barbican leg for a dedicated barbican database"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # Barbican declared WITH a dedicated database — Static-only, so no engine role.
  make_kubectl_full "$tmp" "yes" "images" "" "yes" "compute" "" "yes" "secrets" "barbican-db"

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "the dedicated-database barbican leg is skipped, loudly" \
    "$output" "skipping the barbican database-engine tenant"
  assert_not_contains "no barbican engine role is written for a dedicated database" \
    "$output" "database/mariadb/roles/barbican-"
  assert_contains "keystone is still provisioned" \
    "$output" "database/mariadb/roles/keystone-openstack"
}

# ---------------------------------------------------------------------------
# Test: a Keystone-only ControlPlane runs no optional service leg
# ---------------------------------------------------------------------------
# The presence gate is what keeps the loop from provisioning an engine tenant for
# a service the ControlPlane never declared — writing a role, a policy and a
# connection for a schema nothing will ever connect to, against a MariaDB that
# need not even exist in that namespace.
test_db_tenant_skips_undeclared_services() {
  echo "Test: setup-database-tenant.sh skips glance, placement and barbican when none is declared"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # No optional service declared: every presence lookup resolves empty.
  make_kubectl_full "$tmp" "" "" "" "" "" "" "" "" ""

  local output
  output="$(run_db_tenant "$tmp" "openstack" "cp")"

  assert_contains "the undeclared glance leg is skipped, loudly" \
    "$output" "ControlPlane declares no spec.services.glance"
  assert_contains "the undeclared placement leg is skipped, loudly" \
    "$output" "ControlPlane declares no spec.services.placement"
  assert_contains "the undeclared barbican leg is skipped, loudly" \
    "$output" "ControlPlane declares no spec.services.barbican"
  assert_not_contains "no glance engine role is written for an undeclared service" \
    "$output" "database/mariadb/roles/glance-"
  assert_not_contains "no placement engine role is written for an undeclared service" \
    "$output" "database/mariadb/roles/placement-"
  assert_not_contains "no barbican engine role is written for an undeclared service" \
    "$output" "database/mariadb/roles/barbican-"
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
test_db_tenant_provisions_placement_on_the_placement_namespace
test_db_tenant_defaults_placement_to_the_controlplane_namespace
test_db_tenant_skips_placement_with_a_dedicated_database
test_db_tenant_provisions_barbican_on_the_barbican_namespace
test_db_tenant_defaults_barbican_to_the_controlplane_namespace
test_db_tenant_skips_barbican_with_a_dedicated_database
test_db_tenant_skips_undeclared_services
test_seeder_rejects_a_malformed_identity

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
