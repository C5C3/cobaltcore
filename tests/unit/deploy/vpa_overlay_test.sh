#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the VPA recommender opt-in overlay:
#   - deploy/kind/vpa/{kustomization,source,release}.yaml exist with SPDX
#     headers, the kustomization references the local source/release files
#     (no parent-directory paths), and the overlay does NOT create a Namespace
#     (the release installs into the pre-existing kube-system Namespace).
#   - kustomize build of the overlay renders exactly two documents under the
#     default LoadRestrictionsRootOnly security check (no --load-restrictor
#     flag): HelmRepository/autoscaler in flux-system at the upstream chart
#     index, and HelmRelease/vertical-pod-autoscaler in kube-system whose
#     chart version stays within minor 0.13, whose values disable the
#     admission controller and the updater, and whose recommender carries the
#     two per-pod floor flags.
#   - kustomize build of deploy/flux-system and deploy/kind/base renders ZERO
#     vertical-pod-autoscaler resources (production / default posture).
# Usage: bash tests/unit/deploy/vpa_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
KIND_VPA_DIR="$PROJECT_ROOT/deploy/kind/vpa"
KIND_VPA_KUSTOMIZATION="$KIND_VPA_DIR/kustomization.yaml"
KIND_VPA_SOURCE="$KIND_VPA_DIR/source.yaml"
KIND_VPA_RELEASE="$KIND_VPA_DIR/release.yaml"

# --- Test 1: overlay files exist with SPDX headers ---
test_overlay_files_exist_with_spdx() {
  echo "Test: deploy/kind/vpa/{kustomization,source,release}.yaml exist with SPDX headers"

  local f
  for f in "$KIND_VPA_KUSTOMIZATION" "$KIND_VPA_SOURCE" "$KIND_VPA_RELEASE"; do
    if [[ ! -f "$f" ]]; then
      echo "  FAIL: $f does not exist"
      FAIL=$((FAIL + 1))
      continue
    fi
    assert_file_contains "$(basename "$f") has SPDX FileCopyrightText header" \
      "$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$(basename "$f") has SPDX-License-Identifier: Apache-2.0" \
      "$f" "SPDX-License-Identifier: Apache-2.0"
  done
}

# --- Test 2: kustomization references local source/release, no parent dirs,
#             and creates no Namespace ---
test_kustomization_is_self_contained() {
  echo "Test: kustomization references local source/release and is self-contained"

  if [[ ! -f "$KIND_VPA_KUSTOMIZATION" ]]; then
    echo "  FAIL: $KIND_VPA_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "kustomization.yaml references the local source.yaml" \
    "$KIND_VPA_KUSTOMIZATION" "source.yaml$"
  assert_file_contains "kustomization.yaml references the local release.yaml" \
    "$KIND_VPA_KUSTOMIZATION" "release.yaml$"

  # A `../` reference would re-introduce the kubectl#948 load-restrictor
  # failure under kubectl's embedded kustomize.
  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./' "$KIND_VPA_KUSTOMIZATION" || true; } | wc -l)"
  assert_eq "kustomization.yaml has no '../' parent-directory resource entries" \
    "0" "${parent_refs// /}"

  local ns_entry
  ns_entry="$( { grep -E '^[[:space:]]*-[[:space:]]+namespace\.yaml[[:space:]]*$' "$KIND_VPA_KUSTOMIZATION" || true; } | wc -l)"
  assert_eq "kustomization.yaml does not list a namespace.yaml resource" \
    "0" "${ns_entry// /}"
}

# --- Test 3: kustomize build renders exactly the HelmRepository and the
#             HelmRelease under the default LoadRestrictionsRootOnly check ---
test_kustomize_build_renders_bundle() {
  echo "Test: kustomize build deploy/kind/vpa renders HelmRepository + HelmRelease, recommender only"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (11 checks skipped)"
    SKIP=$((SKIP + 11))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (11 checks skipped)"
    SKIP=$((SKIP + 11))
    return
  fi

  # Mirror the production invocation: NO --load-restrictor flag. kubectl's
  # embedded kustomize (used by hack/deploy-infra.sh) does not expose one.
  local rendered
  if ! rendered="$(kustomize build "$KIND_VPA_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_VPA_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 11))
    return
  fi

  local kinds
  kinds="$(printf '%s\n' "$rendered" | yq -r '.kind + "/" + .metadata.name' 2>/dev/null \
    | grep -v '^---$' | sort | tr '\n' ' ')"
  assert_eq "the overlay renders exactly the HelmRelease and the HelmRepository" \
    "HelmRelease/vertical-pod-autoscaler HelmRepository/autoscaler " "$kinds"

  local repo_ns repo_url
  repo_ns="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRepository" and .metadata.name == "autoscaler") | .metadata.namespace // ""' \
    2>/dev/null | head -n1)"
  repo_url="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRepository" and .metadata.name == "autoscaler") | .spec.url // ""' \
    2>/dev/null | head -n1)"
  assert_eq "HelmRepository/autoscaler lives in flux-system" "flux-system" "$repo_ns"
  assert_eq "HelmRepository/autoscaler points at the upstream chart index" \
    "https://kubernetes.github.io/autoscaler" "$repo_url"

  local sel='select(.kind == "HelmRelease" and .metadata.name == "vertical-pod-autoscaler")'
  local rel_ns version source admission updater replicas cpu_floor mem_floor
  rel_ns="$(printf '%s\n' "$rendered" | yq -r "$sel | .metadata.namespace // \"\"" 2>/dev/null | head -n1)"
  version="$(printf '%s\n' "$rendered" | yq -r "$sel | .spec.chart.spec.version // \"\"" 2>/dev/null | head -n1)"
  source="$(printf '%s\n' "$rendered" | yq -r \
    "$sel | .spec.chart.spec.sourceRef.kind + \"/\" + .spec.chart.spec.sourceRef.namespace + \"/\" + .spec.chart.spec.sourceRef.name" \
    2>/dev/null | head -n1)"
  admission="$(printf '%s\n' "$rendered" | yq -r "$sel | .spec.values.admissionController.enabled" 2>/dev/null | head -n1)"
  updater="$(printf '%s\n' "$rendered" | yq -r "$sel | .spec.values.updater.enabled" 2>/dev/null | head -n1)"
  replicas="$(printf '%s\n' "$rendered" | yq -r "$sel | .spec.values.recommender.replicas" 2>/dev/null | head -n1)"
  cpu_floor="$(printf '%s\n' "$rendered" | yq -r \
    "$sel | .spec.values.recommender.extraArgs[] | select(. == \"--pod-recommendation-min-cpu-millicores=1\")" \
    2>/dev/null | head -n1)"
  mem_floor="$(printf '%s\n' "$rendered" | yq -r \
    "$sel | .spec.values.recommender.extraArgs[] | select(. == \"--pod-recommendation-min-memory-mb=1\")" \
    2>/dev/null | head -n1)"

  assert_eq "HelmRelease/vertical-pod-autoscaler lives in kube-system" "kube-system" "$rel_ns"
  assert_eq "HelmRelease/vertical-pod-autoscaler pins the chart to minor 0.13" \
    ">=0.13.0 <0.14.0" "$version"
  assert_eq "HelmRelease/vertical-pod-autoscaler takes the chart from HelmRepository/autoscaler" \
    "HelmRepository/flux-system/autoscaler" "$source"
  assert_eq "the admission controller is disabled" "false" "$admission"
  assert_eq "the updater is disabled" "false" "$updater"
  assert_eq "the recommender runs one replica" "1" "$replicas"
  assert_eq "the recommender lowers the per-pod CPU floor to 1 millicore" \
    "--pod-recommendation-min-cpu-millicores=1" "$cpu_floor"
  assert_eq "the recommender lowers the per-pod memory floor to 1 MB" \
    "--pod-recommendation-min-memory-mb=1" "$mem_floor"
}

# --- Test 4/5: production flux-system and kind/base render no
#               vertical-pod-autoscaler resource ---
test_default_overlays_render_no_vpa() {
  local dir
  for dir in "$FLUX_SYSTEM_DIR" "$KIND_BASE_DIR"; do
    echo "Test: kustomize build ${dir#"$PROJECT_ROOT"/} renders no vertical-pod-autoscaler resource"

    if ! command -v kustomize >/dev/null 2>&1; then
      echo "  SKIP: kustomize not installed (2 checks skipped)"
      SKIP=$((SKIP + 2))
      continue
    fi

    local rendered
    if ! rendered="$(kustomize build "$dir" 2>&1)"; then
      echo "  FAIL: kustomize build $dir failed:"
      echo "$rendered" | head -20
      FAIL=$((FAIL + 2))
      continue
    fi

    local named sourced
    named="$(printf '%s\n' "$rendered" \
      | grep -cE '^[[:space:]]*name:[[:space:]]+vertical-pod-autoscaler[[:space:]]*$' || true)"
    sourced="$(printf '%s\n' "$rendered" \
      | grep -cF 'https://kubernetes.github.io/autoscaler' || true)"
    assert_eq "${dir#"$PROJECT_ROOT"/} renders no resource named vertical-pod-autoscaler" "0" "${named// /}"
    assert_eq "${dir#"$PROJECT_ROOT"/} references no autoscaler chart index" "0" "${sourced// /}"
  done
}

# --- Run ---
test_overlay_files_exist_with_spdx
test_kustomization_is_self_contained
test_kustomize_build_renders_bundle
test_default_overlays_render_no_vpa

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
