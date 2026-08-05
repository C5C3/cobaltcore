#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the two contracts deploy/eso/clustersecretstore.yaml carries that no
# other check enforces:
#
#   - The c5c3 operator's openBaoDefaultServer / openBaoDefaultKubernetesMount
#     fallbacks match the manifest. The operator copies the connection from the
#     store at runtime and falls back to these constants when the store cannot be
#     read, so a divergence is invisible until exactly that failure path is taken
#     — and then it points the generator at a server that does not exist. The
#     controller's own unit tests compare the constants to themselves, which says
#     nothing about the manifest.
#   - The namespace allow-list is exactly {openstack, shared-services}. Every
#     entry is a privilege grant: anyone able to create an ExternalSecret in a
#     listed namespace reads all of bootstrap/* as the shared eso-management
#     identity. Pinning the list makes a widening a deliberate, reviewed edit
#     here instead of a one-line addition nobody notices. The whole conditions
#     array is pinned, not just its .namespaces entries: a ClusterSecretStore
#     condition is a union of namespaces, namespaceSelector and namespaceRegexes
#     and the entries are OR'd, so a `namespaceSelector: {}` or a
#     `namespaceRegexes: [".*"]` added alongside the list widens the store to
#     every namespace while the list itself still reads clean.
#
# Usage: bash tests/unit/deploy/eso_cluster_store_contract_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

STORE_FILE="$PROJECT_ROOT/deploy/eso/clustersecretstore.yaml"
CONTROLLER_FILE="$PROJECT_ROOT/operators/c5c3/internal/controller/reconcile_dbcredentials.go"

# The reviewed conditions array, as compact JSON.
EXPECTED_CONDITIONS='[{"namespaces":["openstack","shared-services"]}]'

# go_const <name> — Echo the string literal a Go const in CONTROLLER_FILE is
# assigned. Empty when the const is absent.
go_const() {
  grep -oE "$1[[:space:]]+= \"[^\"]+\"" "$CONTROLLER_FILE" 2>/dev/null \
    | head -n1 | sed 's/.*"\(.*\)"/\1/'
}

# --- Test 1: the operator's fallbacks match the production store -------------
test_operator_fallbacks_match_manifest() {
  echo "Test: the c5c3 OpenBao fallbacks match the production ClusterSecretStore"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local manifest_server manifest_mount const_server const_mount
  manifest_server="$(yq -r \
    'select(.kind == "ClusterSecretStore") | .spec.provider.vault.server' \
    "$STORE_FILE" 2>/dev/null | head -n1)"
  manifest_mount="$(yq -r \
    'select(.kind == "ClusterSecretStore") | .spec.provider.vault.auth.kubernetes.mountPath' \
    "$STORE_FILE" 2>/dev/null | head -n1)"
  const_server="$(go_const openBaoDefaultServer)"
  const_mount="$(go_const openBaoDefaultKubernetesMount)"

  assert_eq "openBaoDefaultServer matches the store's provider.vault.server" \
    "$manifest_server" "$const_server"
  assert_eq "openBaoDefaultKubernetesMount matches the store's Kubernetes auth mountPath" \
    "$manifest_mount" "$const_mount"
}

# --- Test 2: the namespace allow-list is pinned ------------------------------
test_namespace_allow_list_is_pinned() {
  echo "Test: the ClusterSecretStore allow-lists exactly the reviewed namespaces"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # Compare the whole conditions array, not just the .namespaces entries: an
  # added namespaceSelector/namespaceRegexes entry is OR'd with the list and can
  # widen the store to every namespace without touching it.
  local actual
  actual="$(yq -o=json -I=0 \
    'select(.kind == "ClusterSecretStore") | .spec.conditions' \
    "$STORE_FILE" 2>/dev/null)"

  assert_eq "adding a condition here grants those namespaces read access to all of bootstrap/*" \
    "$EXPECTED_CONDITIONS" "$actual"
}

# --- Run ---
test_operator_fallbacks_match_manifest
test_namespace_allow_list_is_pinned

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
