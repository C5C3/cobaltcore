#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify deploy/flux-system/fluxinstance.yaml renders through kustomize with
# the FluxInstance spec mandated by REQ-006 (CC-0085). Covers the three
# test_specifications bound to REQ-006:
#   - kustomize build renders FluxInstance with required spec (CC-0085)
#   - kustomize build of kind overlay renders the same FluxInstance (CC-0085)
#   - fluxinstance.yaml has SPDX header (CC-0085)
# Usage: bash tests/unit/deploy/flux_system_fluxinstance_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FLUXINSTANCE_FILE="$PROJECT_ROOT/deploy/flux-system/fluxinstance.yaml"
FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"

# --- Test 1: fluxinstance.yaml has SPDX header (CC-0085, REQ-006) ---
test_spdx_header() {
  echo "Test: deploy/flux-system/fluxinstance.yaml has SPDX header (CC-0085)"

  if [[ ! -f "$FLUXINSTANCE_FILE" ]]; then
    echo "  FAIL: $FLUXINSTANCE_FILE does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local line1 line2 line3
  line1="$(sed -n '1p' "$FLUXINSTANCE_FILE")"
  line2="$(sed -n '2p' "$FLUXINSTANCE_FILE")"
  line3="$(sed -n '3p' "$FLUXINSTANCE_FILE")"

  assert_eq "line 1 is SPDX-FileCopyrightText" \
    "# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company" \
    "$line1"
  assert_eq "line 2 is blank comment" "#" "$line2"
  assert_eq "line 3 is SPDX-License-Identifier Apache-2.0" \
    "# SPDX-License-Identifier: Apache-2.0" \
    "$line3"
}

# --- Test 2: kustomize build deploy/flux-system/ renders FluxInstance
#             with required spec (CC-0085, REQ-006) ---
test_kustomize_build_flux_system() {
  echo "Test: kustomize build deploy/flux-system/ renders FluxInstance with required spec (CC-0085)"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$FLUX_SYSTEM_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $FLUX_SYSTEM_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 6))
    return
  fi

  # Extract the FluxInstance document by kind+name+namespace.
  local fi
  fi="$(printf '%s\n' "$rendered" | yq eval-all \
    'select(.kind == "FluxInstance" and .metadata.name == "flux" and .metadata.namespace == "flux-system")' -)"

  if [[ -z "$fi" ]]; then
    echo "  FAIL: FluxInstance/flux in flux-system not found in kustomize build output"
    FAIL=$((FAIL + 6))
    return
  fi

  # Count occurrences — must be exactly one.
  local count
  count="$(printf '%s\n' "$rendered" | yq eval-all \
    '[select(.kind == "FluxInstance" and .metadata.name == "flux" and .metadata.namespace == "flux-system")] | length' - \
    | awk '{s+=$1} END {print s+0}')"
  assert_eq "exactly one FluxInstance/flux in flux-system" "1" "$count"

  assert_eq "apiVersion is fluxcd.controlplane.io/v1" \
    "fluxcd.controlplane.io/v1" \
    "$(printf '%s\n' "$fi" | yq -r '.apiVersion')"

  assert_eq "spec.distribution.version is 2.x" \
    "2.x" \
    "$(printf '%s\n' "$fi" | yq -r '.spec.distribution.version')"

  assert_eq "spec.distribution.registry is ghcr.io/fluxcd" \
    "ghcr.io/fluxcd" \
    "$(printf '%s\n' "$fi" | yq -r '.spec.distribution.registry')"

  local components
  components="$(printf '%s\n' "$fi" | yq -r '.spec.components | join(",")')"
  assert_eq "spec.components lists the four controllers" \
    "source-controller,kustomize-controller,helm-controller,notification-controller" \
    "$components"

  local cluster
  cluster="$(printf '%s\n' "$fi" | yq -r \
    '[.spec.cluster.type, (.spec.cluster.size // ""), (.spec.cluster.multitenant|tostring), (.spec.cluster.networkPolicy|tostring)] | join("|")')"
  assert_eq "spec.cluster is {type=kubernetes,size=small,multitenant=false,networkPolicy=false}" \
    "kubernetes|small|false|false" \
    "$cluster"

  local has_sync
  has_sync="$(printf '%s\n' "$fi" | yq -r '.spec | has("sync")')"
  assert_eq "spec.sync is NOT present" "false" "$has_sync"
}

# --- Test 3: kustomize build deploy/kind/base/ includes the FluxInstance
#             unchanged (CC-0085, REQ-006) ---
test_kustomize_build_kind_base() {
  echo "Test: kustomize build deploy/kind/base/ renders the FluxInstance unchanged (CC-0085)"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$KIND_BASE_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_BASE_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 2))
    return
  fi

  local count
  count="$(printf '%s\n' "$rendered" | yq eval-all \
    '[select(.kind == "FluxInstance" and .metadata.name == "flux" and .metadata.namespace == "flux-system")] | length' - \
    | awk '{s+=$1} END {print s+0}')"
  assert_eq "exactly one FluxInstance/flux in kind/base build" "1" "$count"

  local version_from_kind
  version_from_kind="$(printf '%s\n' "$rendered" | yq eval-all \
    'select(.kind == "FluxInstance" and .metadata.name == "flux" and .metadata.namespace == "flux-system") | .spec.distribution.version' -)"
  assert_eq "kind overlay FluxInstance distribution.version unchanged (no patch)" \
    "2.x" \
    "$version_from_kind"
}

# --- Run ---
test_spdx_header
test_kustomize_build_flux_system
test_kustomize_build_kind_base

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
