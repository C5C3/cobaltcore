#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the production MaxScale CR takes its client listener and admin
# REST API certificates from the OpenStack DB trust domain. No CI job or lab
# run deploys MaxScale: the kind overlay deletes the CR, and the metal-stack
# lab builds on that overlay. This render is CI's only check of the refs.
# kustomize build deploy/bases/infrastructure runs once, and the test
# checks its output:
#
#   1. MaxScale/openstack-db-maxscale renders exactly once.
#   2. It renders in namespace openstack.
#   3. spec.tls.enabled is true.
#   4. spec.tls.listenerCertIssuerRef names the ClusterIssuer
#      openstack-db-ca-issuer (group cert-manager.io).
#   5. spec.tls.adminCertIssuerRef names the same ClusterIssuer.
#   6. spec.tls.listenerCertIssuerRef equals spec.tls.serverCertIssuerRef of
#      MariaDB/openstack-db, so renaming one issuer ref without the other
#      fails.
#   7. to 10. spec.tls.listenerCertSecretRef, adminCertSecretRef,
#      listenerCASecretRef and adminCASecretRef are unset. The webhook
#      rejects a cert Secret ref beside an issuer ref, and a CA ref would
#      replace the ca.crt that cert-manager writes into each certificate
#      Secret.
#   11. ClusterIssuer/openstack-db-ca-issuer renders exactly once.
#
# A field of a MaxScale that does not render reads as empty, not as null, so
# checks 3 to 10 fail together with check 1. render (tests/lib/
# kustomize_render.sh) counts all 11 checks as SKIP without kustomize or yq,
# and as FAIL when the build fails or renders nothing.
#
# Usage: bash tests/unit/deploy/maxscale_tls_issuer_test.sh

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

INFRA_DIR="deploy/bases/infrastructure"
RENDERED=""

# The yq suffix that prints a value as compact JSON with sorted keys, and the
# issuer ref both MaxScale certificates use, in that form.
AS_JSON='sort_keys(..) | @json'
ISSUER_REF='{"group":"cert-manager.io","kind":"ClusterIssuer","name":"openstack-db-ca-issuer"}'

# --- Checks 1 to 10: the MaxScale and its TLS block ---
test_maxscale_tls() {
  echo "Test: MaxScale openstack/openstack-db-maxscale spec.tls"

  local mxs=(MaxScale openstack-db-maxscale) field
  assert_eq "MaxScale/openstack-db-maxscale renders once" "1" "$(count_named "${mxs[@]}")"
  assert_eq "MaxScale/openstack-db-maxscale renders in namespace openstack" "openstack" \
    "$(val "${mxs[@]}" '.metadata.namespace')"
  assert_eq "spec.tls.enabled" "true" "$(val "${mxs[@]}" '.spec.tls.enabled')"
  assert_eq "spec.tls.listenerCertIssuerRef names ClusterIssuer openstack-db-ca-issuer" \
    "$ISSUER_REF" "$(val "${mxs[@]}" ".spec.tls.listenerCertIssuerRef | $AS_JSON")"
  assert_eq "spec.tls.adminCertIssuerRef names ClusterIssuer openstack-db-ca-issuer" \
    "$ISSUER_REF" "$(val "${mxs[@]}" ".spec.tls.adminCertIssuerRef | $AS_JSON")"
  assert_eq "spec.tls.listenerCertIssuerRef equals the MariaDB's spec.tls.serverCertIssuerRef" \
    "$(val MariaDB openstack-db ".spec.tls.serverCertIssuerRef | $AS_JSON")" \
    "$(val "${mxs[@]}" ".spec.tls.listenerCertIssuerRef | $AS_JSON")"
  for field in listenerCertSecretRef adminCertSecretRef listenerCASecretRef adminCASecretRef; do
    assert_eq "spec.tls.$field is unset" "null" "$(val "${mxs[@]}" ".spec.tls.$field")"
  done
}

# --- Check 11: the issuer both refs name ---
test_db_ca_issuer() {
  echo "Test: the ClusterIssuer the refs name"
  assert_eq "ClusterIssuer/openstack-db-ca-issuer renders once" "1" \
    "$(count_named ClusterIssuer openstack-db-ca-issuer)"
}

# --- Run ---
if render "$PROJECT_ROOT/$INFRA_DIR" 11; then
  test_maxscale_tls
  test_db_ca_issuer
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
