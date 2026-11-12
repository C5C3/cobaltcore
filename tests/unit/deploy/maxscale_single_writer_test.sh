#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the production MaxScale CR sends every statement to the Galera
# primary, the condition under which the OpenStack operators route a service
# through it (database.ResolveEndpoint). No CI job or lab run deploys
# MaxScale, so this render is CI's only check of the service. kustomize build
# deploy/bases/infrastructure runs once, and the test checks:
#
#   1. MaxScale/openstack-db-maxscale renders exactly once.
#   2. spec.services has exactly one entry.
#   3. Its name is rw-router, the name MaxScale.SetDefaults gives the
#      default service.
#   4. Its router is readwritesplit, the router SetDefaults writes; the
#      webhook keeps the router immutable.
#   5. params.max_slave_connections is 0, which keeps every read on the
#      primary.
#   6. That value is a YAML string: params is a map of strings, and an
#      unquoted 0 would be an integer.
#   7. Its listener port is 3306, the database port the services connect to.
#   8. The service is not suspended.
#   9. Its listener is not suspended.
#  10. spec.mariaDbRef names openstack-db,
#  11. in no namespace other than openstack.
#  12. MariaDB/openstack-db names openstack-db-maxscale in spec.maxScaleRef,
#  13. in no namespace other than openstack.
#
# kustomize build deploy/overlays/kind/infrastructure runs second, and the test checks:
#
#  14. The kind overlay renders no MaxScale/openstack-db-maxscale.
#
# render (tests/lib/kustomize_render.sh) counts each render's checks as SKIP
# without kustomize or yq, and as FAIL when the build fails or renders
# nothing.
#
# Usage: bash tests/unit/deploy/maxscale_single_writer_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/kustomize_render.sh
source "$PROJECT_ROOT/tests/lib/kustomize_render.sh"

RENDERED=""

# --- Checks 1 to 13: the single-writer service of the production MaxScale ---
test_maxscale_single_writer() {
  echo "Test: MaxScale openstack/openstack-db-maxscale spec.services"

  local mxs=(MaxScale openstack-db-maxscale) svc='.spec.services[0]'
  assert_eq "MaxScale/openstack-db-maxscale renders once" "1" "$(count_named "${mxs[@]}")"
  assert_eq "spec.services has one entry" "1" "$(val "${mxs[@]}" '.spec.services | length')"
  assert_eq "the service is named rw-router" "rw-router" "$(val "${mxs[@]}" "$svc.name")"
  assert_eq "the service router is readwritesplit" "readwritesplit" "$(val "${mxs[@]}" "$svc.router")"
  assert_eq "params.max_slave_connections is 0" "0" \
    "$(val "${mxs[@]}" "$svc.params.max_slave_connections")"
  assert_eq "params.max_slave_connections is a string" '!!str' \
    "$(val "${mxs[@]}" "$svc.params.max_slave_connections | tag")"
  assert_eq "the listener port is 3306" "3306" "$(val "${mxs[@]}" "$svc.listener.port")"
  # Checks 8 and 9 print the service name only while the flag is unset, null
  # or false. Kubernetes also reads True and TRUE as true, and yq's == compares
  # the scalar text, so `select(. == true)` would miss those spellings.
  # Checks 11 and 13 select the offending value and expect nothing.
  # No check uses `// default`: yq before v4.45.3 evaluates the default on
  # every document val's select drops, so that form would pass on whatever
  # the overlay sets.
  assert_eq "the service is not suspended" "rw-router" \
    "$(val "${mxs[@]}" "$svc | select(.suspend == null or .suspend == false) | .name")"
  assert_eq "the listener is not suspended" "rw-router" \
    "$(val "${mxs[@]}" "$svc | select(.listener.suspend == null or .listener.suspend == false) | .name")"
  assert_eq "spec.mariaDbRef names openstack-db" "openstack-db" "$(val "${mxs[@]}" '.spec.mariaDbRef.name')"
  assert_eq "spec.mariaDbRef names no other namespace" "" \
    "$(val "${mxs[@]}" '.spec.mariaDbRef.namespace | select(. != null and . != "openstack")')"
  assert_eq "MariaDB openstack-db names the MaxScale" "openstack-db-maxscale" \
    "$(val MariaDB openstack-db '.spec.maxScaleRef.name')"
  assert_eq "spec.maxScaleRef names no other namespace" "" \
    "$(val MariaDB openstack-db '.spec.maxScaleRef.namespace | select(. != null and . != "openstack")')"
}

# --- Check 14: the kind overlay deletes the MaxScale ---
test_kind_overlay_has_no_maxscale() {
  echo "Test: the kind overlay renders no MaxScale"
  assert_eq "deploy/overlays/kind/infrastructure renders no MaxScale/openstack-db-maxscale" "0" \
    "$(count_named MaxScale openstack-db-maxscale)"
}

# --- Run ---
if render "$PROJECT_ROOT/deploy/bases/infrastructure" 13; then
  test_maxscale_single_writer
fi
if render "$PROJECT_ROOT/deploy/overlays/kind/infrastructure" 1; then
  test_kind_overlay_has_no_maxscale
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
