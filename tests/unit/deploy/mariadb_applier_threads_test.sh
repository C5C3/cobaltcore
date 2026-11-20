#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the production MariaDB runs the Galera applier thread count of
# the Standard sizing profile, and that the kind overlay drops it with Galera.
# No CI job deploys the production base, so this render is CI's only check of
# the value. kustomize build deploy/bases/infrastructure runs once, and the
# test checks:
#
#   1. MariaDB/openstack-db renders spec.galera.replicaThreads as 4.
#   2. That value is a YAML integer: mariadb-operator types the field as an
#      int, and a quoted "4" is a string the API server rejects.
#   3. It equals StandardDatabaseApplierThreads in
#      operators/c5c3/api/v1alpha1/sizing_profiles.go, so the base and the
#      Standard profile stay equal.
#
# kustomize build deploy/overlays/kind/infrastructure runs second, and the
# test checks:
#
#   4. MariaDB/openstack-db renders spec.galera as exactly {"enabled":false}:
#      the single kind replica runs no Galera and carries no thread count.
#
# render (tests/lib/kustomize_render.sh) counts each render's checks as SKIP
# without kustomize or yq, and as FAIL when the build fails or renders
# nothing.
#
# Usage: bash tests/unit/deploy/mariadb_applier_threads_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SIZING_PROFILES="$PROJECT_ROOT/operators/c5c3/api/v1alpha1/sizing_profiles.go"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/kustomize_render.sh
source "$PROJECT_ROOT/tests/lib/kustomize_render.sh"

RENDERED=""

# standard_applier_threads prints <n> from the line
# `StandardDatabaseApplierThreads int32 = <n>` in sizing_profiles.go, or
# nothing when the line is missing.
standard_applier_threads() {
  sed -nE 's/^[[:space:]]*(const[[:space:]]+)?StandardDatabaseApplierThreads int32 = ([0-9]+)$/\2/p' \
    "$SIZING_PROFILES" | head -n 1
}

# --- Checks 1 to 3: the applier threads of the production MariaDB ---
test_production_applier_threads() {
  echo "Test: MariaDB openstack/openstack-db spec.galera.replicaThreads"

  local threads='.spec.galera.replicaThreads'
  local standard
  standard="$(standard_applier_threads)"
  assert_eq "spec.galera.replicaThreads is 4" "4" "$(val MariaDB openstack-db "$threads")"
  assert_eq "spec.galera.replicaThreads is an integer" '!!int' \
    "$(val MariaDB openstack-db "$threads | tag")"
  # An empty $standard (the Go line moved or was reworded) fails here too,
  # because the rendered value is never empty once check 1 passes.
  assert_eq "spec.galera.replicaThreads equals StandardDatabaseApplierThreads (${standard:-missing})" \
    "${standard:-missing}" "$(val MariaDB openstack-db "$threads")"
}

# --- Check 4: the kind overlay runs no Galera ---
test_kind_overlay_galera() {
  echo "Test: the kind overlay's MariaDB openstack/openstack-db spec.galera"
  assert_eq "deploy/overlays/kind/infrastructure renders spec.galera as {\"enabled\":false}" \
    '{"enabled":false}' "$(val MariaDB openstack-db '.spec.galera | to_json(0)')"
}

# --- Run ---
if render "$PROJECT_ROOT/deploy/bases/infrastructure" 3; then
  test_production_applier_threads
fi
if render "$PROJECT_ROOT/deploy/overlays/kind/infrastructure" 1; then
  test_kind_overlay_galera
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
