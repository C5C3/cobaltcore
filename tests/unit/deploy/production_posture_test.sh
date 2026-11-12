#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Keep reference resource data independent of development overlays. File moves
# and canonical path comments may change; the Flux runtime and historically
# stable controller releases must retain their Kubernetes fields. The explicit
# exceptions below have dedicated version, sizing or integration contracts.
# Support both the canonical and historical paths in origin/main while this
# branch is being integrated. This fallback reads Git history only.
#
# Independently render the source catalog and require only Flux source kinds.
# Historical comparisons skip without a Git checkout/origin/main; source checks
# also run in source archives when kustomize, yq and jq are available.
#
# Usage: bash tests/unit/deploy/production_posture_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Preconditions — skip gracefully when git context is unavailable.
# ---------------------------------------------------------------------------
preflight() {
  if ! command -v git >/dev/null 2>&1; then
    return 1
  fi
  if ! git -C "$PROJECT_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    return 1
  fi
  # Need a local ref for origin/main; CI clones should have it.
  if ! git -C "$PROJECT_ROOT" rev-parse --verify --quiet origin/main >/dev/null 2>&1; then
    return 1
  fi
  return 0
}

# --- Test 1: tracked production-overlay files are unchanged vs origin/main
#             ---
test_production_overlay_unchanged() {
  echo "Test: reference Flux runtime and controller releases unchanged vs origin/main"

  if ! preflight; then
    echo "  SKIP: git / origin/main unavailable (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local original current name expected actual baseline_flux baseline_releases
  baseline_flux=deploy/bases/flux/fluxinstance.yaml
  baseline_releases=deploy/bases/operators/releases
  if ! git -C "$PROJECT_ROOT" cat-file -e "origin/main:$baseline_flux" 2>/dev/null; then
    baseline_flux=deploy/flux-system/fluxinstance.yaml
    baseline_releases=deploy/flux-system/releases
  fi
  for tool in yq jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "  SKIP: $tool required for moved resource comparison"
      SKIP=$((SKIP + 1))
      return
    fi
  done
  expected="$(git -C "$PROJECT_ROOT" show "origin/main:$baseline_flux" | yq -o=json -I0 '.' | jq -S -s '.')"
  actual="$(yq -o=json -I0 '.' "$PROJECT_ROOT/deploy/bases/flux/fluxinstance.yaml" | jq -S -s '.')"
  assert_eq "Flux runtime resource data stays unchanged across the move" "$expected" "$actual"
  while IFS= read -r original; do
    name="${original##*/}"
    case "$name" in
      kustomization.yaml|chaos-mesh.yaml|openbao.yaml|c5c3-operator.yaml|helmrelease.yaml|external-secrets.yaml|cert-manager.yaml|mariadb-operator.yaml|mariadb-operator-crds.yaml|memcached-operator.yaml|k-orc.yaml|garage-operator.yaml|keystone-operator.yaml|horizon-operator.yaml|glance-operator.yaml|placement-operator.yaml|openbao-operator.yaml|barbican-operator.yaml|ovn-operator.yaml|neutron-operator.yaml|cinder-operator.yaml|nova-operator.yaml|rabbitmq-cluster-operator.yaml|messaging-topology-operator.yaml) continue ;;
    esac
    current="$PROJECT_ROOT/deploy/bases/operators/releases/$name"
    expected="$(git -C "$PROJECT_ROOT" show "origin/main:$original" | yq -o=json -I0 '.' | jq -S -s '.')"
    actual="$(yq -o=json -I0 '.' "$current" | jq -S -s '.')"
    assert_eq "$name retains its historical resource data" "$expected" "$actual"
  done < <(git -C "$PROJECT_ROOT" ls-tree -r --name-only origin/main -- "$baseline_releases")

}

# --- Test 2: the source catalog renders only Flux source objects ---
test_source_catalog_is_flux_sources_only() {
  echo "Test: deploy/bases/operators/sources renders only Flux source objects"

  local tool
  for tool in kustomize yq jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "  SKIP: $tool unavailable (1 check skipped)"
      SKIP=$((SKIP + 1))
      return
    fi
  done

  local rendered sources
  if ! rendered="$(kustomize build "$PROJECT_ROOT/deploy/bases/operators/sources" 2>&1)"; then
    echo "  FAIL: source catalog does not render"
    printf '%s\n' "$rendered" >&2
    FAIL=$((FAIL + 1))
    return
  fi
  if ! sources="$(printf '%s\n' "$rendered" | yq -o=json -I0 '.' - | jq -s 'map(select(. != null))')"; then
    echo "  FAIL: source catalog output is not valid YAML objects"
    FAIL=$((FAIL + 1))
    return
  fi
  if ! printf '%s\n' "$sources" | jq -e 'length > 0 and all(.[];
    (.apiVersion | startswith("source.toolkit.fluxcd.io/")) and
    (.kind == "HelmRepository" or .kind == "OCIRepository" or .kind == "GitRepository"))' >/dev/null; then
    echo "  FAIL: source catalog is empty or contains non-source objects"
    printf '%s\n' "$sources" | jq -r '.[] | "    \(.apiVersion) \(.kind)/\(.metadata.name)"'
    FAIL=$((FAIL + 1))
    return
  fi

  echo "  PASS: source catalog renders only Flux source objects"
  PASS=$((PASS + 1))
}

# --- Run ---
test_production_overlay_unchanged
test_source_catalog_is_flux_sources_only

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
