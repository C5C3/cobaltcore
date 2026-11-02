#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that the OpenBao server config of the kind base overlay, which the
# lab overlay deploy/lab/metal-stack/base builds on, has no Kubernetes service
# registration:
#   - the kind base renders OpenBao in standalone mode, and its standalone
#     config has no service_registration stanza. The chart grants the Role
#     that lets the server label its own pod only in HA mode. Without it every
#     GET of the pod gets a 403, the server retries every 5 seconds, and
#     OpenBao 2.7.1 leaks one goroutine per retry until the pod is OOMKilled
#     and comes back sealed;
#   - deploy/flux-system/ (production) keeps HA mode and its
#     service_registration stanza, for which the chart grants that Role.
#
# Usage: bash tests/unit/deploy/kind_base_openbao_service_registration_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
OPENBAO='.kind == "HelmRelease" and .metadata.name == "openbao"'

# render <dir> <file>
# Renders <dir> into <file>; prints kustomize's stderr and returns non-zero on
# a failed build.
render() {
  local dir="$1" out="$2" err
  if ! err="$(kustomize build "$dir" 2>&1 >"$out")"; then
    echo "  FAIL: kustomize build $dir failed:"
    echo "$err" | head -20
    return 1
  fi
}

# field <file> <path>
# Prints <path> of the OpenBao HelmRelease in <file>.
field() {
  yq eval-all "select(${OPENBAO}) | $2" "$1"
}

# ---------------------------------------------------------------------------
# Test 1: the kind base runs standalone without service registration
# ---------------------------------------------------------------------------
test_kind_base() {
  local file="$1" config
  echo "Test: the kind base's standalone OpenBao has no service registration"

  assert_eq "OpenBao runs in standalone mode" \
    "true" "$(field "$file" '.spec.values.server.standalone.enabled')"
  assert_eq "with HA disabled" \
    "false" "$(field "$file" '.spec.values.server.ha.enabled')"
  config="$(field "$file" '.spec.values.server.standalone.config')"
  assert_contains "the standalone config is the overlay's" "$config" 'storage "raft"'
  assert_not_contains "and has no service_registration stanza" "$config" "service_registration"
}

# ---------------------------------------------------------------------------
# Test 2: production keeps HA mode and its service registration
# ---------------------------------------------------------------------------
test_production() {
  local file="$1"
  echo "Test: deploy/flux-system/ keeps HA mode and its service registration"

  assert_eq "production OpenBao runs in HA mode" \
    "true" "$(field "$file" '.spec.values.server.ha.enabled')"
  assert_contains "with the service_registration stanza" \
    "$(field "$file" '.spec.values.server.ha.raft.config')" 'service_registration "kubernetes" {}'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: kustomize or yq not installed"
  SKIP=$((SKIP + 1))
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  if render "$KIND_BASE_DIR" "$tmp/kind.yaml"; then
    test_kind_base "$tmp/kind.yaml"
  else
    FAIL=$((FAIL + 1))
  fi
  if render "$FLUX_SYSTEM_DIR" "$tmp/prod.yaml"; then
    test_production "$tmp/prod.yaml"
  else
    FAIL=$((FAIL + 1))
  fi
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
