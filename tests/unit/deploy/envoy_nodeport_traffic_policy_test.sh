#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the kind EnvoyProxy override pins the proxy Service to
# externalTrafficPolicy: Cluster:
#   - deploy/kind/infrastructure/envoy-nodeport.yaml sets
#     spec.provider.kubernetes.envoyService.externalTrafficPolicy: Cluster.
#     EnvoyProxy defaults the field to Local, and with a NodePort Service
#     Envoy Gateway then assigns the Gateway only the addresses of nodes that
#     carry a Ready endpoint of the proxy Service. A status pass that runs
#     before the proxy pod's readiness reaches the EndpointSlice leaves
#     Gateway/openstack-gw at Programmed=False (AddressNotAssigned), and
#     hack/deploy-infra.sh times out on it after 600 s.
#   - The Service stays type: NodePort, which is what the policy qualifies.
# Usage: bash tests/unit/deploy/envoy_nodeport_traffic_policy_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

ENVOY_NODEPORT="$PROJECT_ROOT/deploy/kind/infrastructure/envoy-nodeport.yaml"

# The value of a direct child key of the envoyService block, or empty when the
# key is absent. Comment lines are skipped, so the DECISION notes in the file
# header cannot satisfy the lookup.
envoy_service_field() {
  local key="$1"
  awk -v key="$key" '
    /^[[:space:]]*#/ { next }
    /^      envoyService:[[:space:]]*$/ { inside = 1; next }
    inside && NF && !/^        / { inside = 0 }
    inside && $0 ~ "^        " key ":" {
      sub("^        " key ":[[:space:]]*", ""); print; exit
    }
  ' "$ENVOY_NODEPORT"
}

# --- Test 1: the proxy Service is pinned to externalTrafficPolicy: Cluster ---
test_envoy_service_traffic_policy_is_cluster() {
  echo "Test: envoy-nodeport.yaml sets envoyService.externalTrafficPolicy: Cluster"

  if [[ ! -f "$ENVOY_NODEPORT" ]]; then
    echo "  FAIL: $ENVOY_NODEPORT does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "envoyService.externalTrafficPolicy" "Cluster" "$(envoy_service_field externalTrafficPolicy)"
}

# --- Test 2: the Service the policy qualifies is still a NodePort ---
test_envoy_service_type_is_nodeport() {
  echo "Test: envoy-nodeport.yaml keeps envoyService.type: NodePort"

  if [[ ! -f "$ENVOY_NODEPORT" ]]; then
    echo "  FAIL: $ENVOY_NODEPORT does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_eq "envoyService.type" "NodePort" "$(envoy_service_field type)"
}

# --- Run ---
test_envoy_service_traffic_policy_is_cluster
test_envoy_service_type_is_nodeport

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
