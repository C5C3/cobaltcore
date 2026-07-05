#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the kind-only Flux Web UI overlay:
#   - deploy/kind/base/flux-web.yaml declares a ResourceSet/flux-web in
#     flux-system that renders the flux-operator chart with the Web UI
#     sub-chart (web.serverOnly=true, installCRDs=false, fullnameOverride).
#   - deploy/kind/base/kustomization.yaml lists flux-web.yaml immediately
#     after headlamp.yaml, keeping the kind-only overlay the single
#     entry point for the Web UI demo addon.
#   - deploy/flux-system/ (production overlay) is NOT affected: no
#     ResourceSet is rendered and the flux-web string never appears.
# Usage: bash tests/unit/deploy/kind_base_flux_web_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FLUX_WEB_FILE="$PROJECT_ROOT/deploy/kind/base/flux-web.yaml"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
KIND_KUSTOMIZATION="$KIND_BASE_DIR/kustomization.yaml"
FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
FLUX_SYSTEM_KUSTOMIZATION="$FLUX_SYSTEM_DIR/kustomization.yaml"
FLUX_SYSTEM_FLUXINSTANCE="$FLUX_SYSTEM_DIR/fluxinstance.yaml"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

# --- Test 1: flux-web.yaml has SPDX header + Feature marker ---
test_spdx_header() {
  echo "Test: deploy/kind/base/flux-web.yaml has SPDX header and Feature marker"

  if [[ ! -f "$FLUX_WEB_FILE" ]]; then
    echo "  FAIL: $FLUX_WEB_FILE does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local line1 line2 line3
  line1="$(sed -n '1p' "$FLUX_WEB_FILE")"
  line2="$(sed -n '2p' "$FLUX_WEB_FILE")"
  line3="$(sed -n '3p' "$FLUX_WEB_FILE")"

  assert_eq "line 1 is SPDX-FileCopyrightText" \
    "# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company" \
    "$line1"
  assert_eq "line 2 is blank comment" "#" "$line2"
  assert_eq "line 3 is SPDX-License-Identifier Apache-2.0" \
    "# SPDX-License-Identifier: Apache-2.0" \
    "$line3"
}

# --- Test 2: kustomize build of deploy/kind/base/ emits a ResourceSet with
#             the required nested OCIRepository and HelmRelease ---
test_kustomize_build_emits_resourceset() {
  echo "Test: kustomize build deploy/kind/base/ emits ResourceSet/flux-web with required nested resources"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$KIND_BASE_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_BASE_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 7))
    return
  fi

  # Count ResourceSet/flux-web occurrences — must be exactly one.
  local rs_count
  rs_count="$(printf '%s\n' "$rendered" | yq eval-all \
    '[select(.kind == "ResourceSet" and .metadata.name == "flux-web" and .metadata.namespace == "flux-system")] | length' - \
    | awk '{s+=$1} END {print s+0}')"
  assert_eq "exactly one ResourceSet/flux-web in flux-system" "1" "$rs_count"

  # Pull the single ResourceSet out for per-field assertions.
  local rs
  rs="$(printf '%s\n' "$rendered" | yq eval-all \
    'select(.kind == "ResourceSet" and .metadata.name == "flux-web" and .metadata.namespace == "flux-system")' -)"

  if [[ -z "$rs" ]]; then
    echo "  FAIL: ResourceSet/flux-web not found in kustomize build output"
    FAIL=$((FAIL + 6))
    return
  fi

  assert_eq "ResourceSet apiVersion is fluxcd.controlplane.io/v1" \
    "fluxcd.controlplane.io/v1" \
    "$(printf '%s\n' "$rs" | yq -r '.apiVersion')"

  # Nested OCIRepository inside spec.resources.
  local oci_kind oci_url
  oci_kind="$(printf '%s\n' "$rs" | yq -r \
    '.spec.resources[] | select(.kind == "OCIRepository") | .kind' | head -n1)"
  oci_url="$(printf '%s\n' "$rs" | yq -r \
    '.spec.resources[] | select(.kind == "OCIRepository") | .spec.url' | head -n1)"

  assert_eq "spec.resources contains an OCIRepository" "OCIRepository" "$oci_kind"
  assert_eq "OCIRepository spec.url points at the flux-operator chart" \
    "oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator" \
    "$oci_url"

  # Nested HelmRelease inside spec.resources.
  local hr_web_serveronly hr_install_crds hr_fullname_override
  hr_web_serveronly="$(printf '%s\n' "$rs" | yq -r \
    '.spec.resources[] | select(.kind == "HelmRelease") | .spec.values.web.serverOnly' | head -n1)"
  hr_install_crds="$(printf '%s\n' "$rs" | yq -r \
    '.spec.resources[] | select(.kind == "HelmRelease") | .spec.values.installCRDs' | head -n1)"
  hr_fullname_override="$(printf '%s\n' "$rs" | yq -r \
    '.spec.resources[] | select(.kind == "HelmRelease") | .spec.values.fullnameOverride' | head -n1)"

  assert_eq "HelmRelease spec.values.web.serverOnly is true" "true" "$hr_web_serveronly"
  assert_eq "HelmRelease spec.values.installCRDs is false" "false" "$hr_install_crds"
  assert_eq "HelmRelease spec.values.fullnameOverride is flux-web" "flux-web" "$hr_fullname_override"
}

# --- Test 3: chart version pin is a SemVer range locked to the minor
#             track shipped by hack/deploy-infra.sh ---
test_chart_version_pin_is_semver_range() {
  echo "Test: ResourceSet spec.inputs[0].version is 0.53.x and matches FLUX_OPERATOR_VERSION minor track"

  if [[ ! -f "$FLUX_WEB_FILE" ]]; then
    echo "  FAIL: $FLUX_WEB_FILE does not exist"
    FAIL=$((FAIL + 2))
    return
  fi

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
  else
    local version
    version="$(yq -r '.spec.inputs[0].version' "$FLUX_WEB_FILE" 2>/dev/null | head -n1)"
    assert_eq "spec.inputs[0].version pins the 0.53.x SemVer range" "0.53.x" "$version"
  fi

  assert_file_contains "hack/deploy-infra.sh still declares FLUX_OPERATOR_VERSION=\"v0.53.0\"" \
    "$DEPLOY_INFRA_SH" \
    'FLUX_OPERATOR_VERSION="v0.53.0"'
}

# --- Test 4: kind kustomization lists flux-web.yaml after headlamp.yaml ---
test_kustomization_lists_flux_web() {
  echo "Test: deploy/kind/base/kustomization.yaml resources list contains flux-web.yaml after headlamp.yaml"

  if [[ ! -f "$KIND_KUSTOMIZATION" ]]; then
    echo "  FAIL: $KIND_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 3))
    return
  fi

  assert_file_contains "kustomization.yaml references headlamp.yaml" \
    "$KIND_KUSTOMIZATION" \
    "headlamp.yaml"
  assert_file_contains "kustomization.yaml references flux-web.yaml" \
    "$KIND_KUSTOMIZATION" \
    "flux-web.yaml"

  # Adjacency / ordering: flux-web.yaml must appear AFTER headlamp.yaml.
  # Guard with || true so a missing line doesn't abort the script under pipefail.
  local headlamp_line flux_web_line
  headlamp_line="$( { grep -n '^[[:space:]]*-[[:space:]]*headlamp\.yaml[[:space:]]*$' "$KIND_KUSTOMIZATION" || true; } | head -n1 | cut -d: -f1)"
  flux_web_line="$( { grep -n '^[[:space:]]*-[[:space:]]*flux-web\.yaml[[:space:]]*$' "$KIND_KUSTOMIZATION" || true; } | head -n1 | cut -d: -f1)"

  if [[ -z "$headlamp_line" || -z "$flux_web_line" ]]; then
    echo "  FAIL: could not locate both - headlamp.yaml and - flux-web.yaml in resources list"
    echo "    headlamp line: '${headlamp_line:-<none>}' flux-web line: '${flux_web_line:-<none>}'"
    FAIL=$((FAIL + 1))
    return
  fi

  if (( flux_web_line > headlamp_line )); then
    echo "  PASS: flux-web.yaml (line $flux_web_line) is listed after headlamp.yaml (line $headlamp_line)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: flux-web.yaml (line $flux_web_line) must be listed after headlamp.yaml (line $headlamp_line)"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 5: production overlay has NO flux-web / ResourceSet ---
test_production_overlay_has_no_flux_web() {
  echo "Test: kustomize build deploy/flux-system/ renders no ResourceSet and no flux-web string"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$FLUX_SYSTEM_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $FLUX_SYSTEM_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 2))
    return
  fi

  local rs_count fw_count
  rs_count="$(printf '%s\n' "$rendered" | grep -c 'kind: ResourceSet' || true)"
  fw_count="$(printf '%s\n' "$rendered" | grep -c 'flux-web' || true)"

  assert_eq "production overlay renders zero ResourceSet documents" "0" "$rs_count"
  assert_eq "production overlay renders no flux-web string" "0" "$fw_count"
}

# --- Test 6: production kustomization + fluxinstance are unchanged vs. origin/main
#             ---
test_production_kustomization_unchanged() {
  echo "Test: deploy/flux-system/{kustomization.yaml,fluxinstance.yaml} have no diff vs. origin/main"

  if ! git -C "$PROJECT_ROOT" rev-parse --verify origin/main >/dev/null 2>&1; then
    echo "  SKIP: origin/main ref is not available in this checkout (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local diff_rc=0
  git -C "$PROJECT_ROOT" diff --quiet origin/main -- \
    deploy/flux-system/kustomization.yaml \
    deploy/flux-system/fluxinstance.yaml \
    || diff_rc=$?

  assert_eq "git diff against origin/main is empty" "0" "$diff_rc"
}

# --- Run ---
test_spdx_header
test_kustomize_build_emits_resourceset
test_chart_version_pin_is_semver_range
test_kustomization_lists_flux_web
test_production_overlay_has_no_flux_web
test_production_kustomization_unchanged

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
