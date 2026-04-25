#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/kind-config.yaml exposes the host :443 → container :31443 port
# mapping that bridges `https://keystone.127-0-0-1.nip.io/v3` to the Envoy
# Gateway NodePort data plane (CC-0088, REQ-004).
#
# Uses `yq` to assert the presence and shape of the port mapping so that
# formatting changes (indentation, key order) do not break the test. Skipped
# when `yq` is not installed.
#
# Usage: bash tests/unit/hack/kind_config_port_mapping_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KIND_CONFIG_FILE="$PROJECT_ROOT/hack/kind-config.yaml"

# --- Test 1: kind-config.yaml has SPDX header and feature ID (CC-0088, REQ-004) ---
test_feature_id_comment() {
  echo "Test: hack/kind-config.yaml carries CC-0088 feature ID comment (CC-0088, REQ-004)"

  if [[ ! -f "$KIND_CONFIG_FILE" ]]; then
    echo "  FAIL: $KIND_CONFIG_FILE does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "CC-0088 feature ID appears near the port mapping" \
    "$KIND_CONFIG_FILE" "CC-0088"
}

# --- Test 2: nodes[0] has the hostPort:443 extraPortMapping with the
#             expected shape (CC-0088, REQ-004) ---
test_port_mapping_shape() {
  echo "Test: nodes[0].extraPortMappings[hostPort==443] is containerPort=31443 TCP (CC-0088, REQ-004)"

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  # Select the port mapping under nodes[0] whose hostPort is 443 and assert
  # the three fields that the kind bridge depends on plus the listenAddress
  # which keeps the endpoint local-only.
  local mapping
  mapping="$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 443)' "$KIND_CONFIG_FILE")"

  if [[ -z "$mapping" ]]; then
    echo "  FAIL: no extraPortMappings entry with hostPort=443 under nodes[0]"
    FAIL=$((FAIL + 5))
    return
  fi

  assert_eq "nodes[0].extraPortMappings[hostPort=443].containerPort is 31443" \
    "31443" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 443) | .containerPort' "$KIND_CONFIG_FILE")"

  assert_eq "nodes[0].extraPortMappings[hostPort=443].protocol is TCP" \
    "TCP" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 443) | .protocol' "$KIND_CONFIG_FILE")"

  assert_eq "nodes[0].extraPortMappings[hostPort=443].listenAddress is 127.0.0.1" \
    "127.0.0.1" \
    "$(yq -r '.nodes[0].extraPortMappings[] | select(.hostPort == 443) | .listenAddress' "$KIND_CONFIG_FILE")"

  # There must be exactly one mapping for hostPort=443 — duplicates would
  # make kind fail to create the cluster.
  local count
  count="$(yq -r '[.nodes[0].extraPortMappings[] | select(.hostPort == 443)] | length' "$KIND_CONFIG_FILE")"
  assert_eq "exactly one hostPort=443 entry" "1" "$count"

  # Sanity check: the node this mapping lives under must be the control-plane.
  local role
  role="$(yq -r '.nodes[0].role' "$KIND_CONFIG_FILE")"
  assert_eq "nodes[0].role is control-plane" "control-plane" "$role"
}

# --- Run ---
test_feature_id_comment
test_port_mapping_shape

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
