#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the Chaos Mesh opt-in overlay introduced by CC-0097:
#   - deploy/flux-system/kustomization.yaml no longer references
#     sources/chaos-mesh.yaml or releases/chaos-mesh.yaml in its resources
#     list, and the corresponding files are no longer present under
#     deploy/flux-system/{sources,releases}/ (they were relocated into the
#     opt-in overlay so the overlay is self-contained — CC-0097, REQ-001).
#   - deploy/flux-system/namespaces.yaml no longer creates the chaos-mesh
#     Namespace.
#   - deploy/kind/base/kustomization.yaml no longer carries the chaos-mesh
#     HelmRelease patch (kustomize would otherwise refuse to render with a
#     missing target).
#   - deploy/kind/chaos-mesh/kustomization.yaml exists, references the
#     local source/release/namespace files (no parent-directory paths),
#     and carries the kind-tuning patch.
#   - kustomize build of all three overlays renders the expected document
#     set under the default LoadRestrictionsRootOnly security check
#     (no --load-restrictor flag): production renders no chaos-mesh
#     resources; kind/base succeeds without an orphan patch error;
#     kind/chaos-mesh renders Namespace + HelmRepository + HelmRelease
#     with the kind-tuning values applied and dependsOn: cert-manager
#     preserved.
# Feature: CC-0097, REQ-001, REQ-002, REQ-009
# Usage: bash tests/unit/deploy/chaos_mesh_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
FLUX_SYSTEM_KUSTOMIZATION="$FLUX_SYSTEM_DIR/kustomization.yaml"
FLUX_SYSTEM_NAMESPACES="$FLUX_SYSTEM_DIR/namespaces.yaml"
FLUX_SYSTEM_CHAOS_SOURCE="$FLUX_SYSTEM_DIR/sources/chaos-mesh.yaml"
FLUX_SYSTEM_CHAOS_RELEASE="$FLUX_SYSTEM_DIR/releases/chaos-mesh.yaml"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
KIND_BASE_KUSTOMIZATION="$KIND_BASE_DIR/kustomization.yaml"
KIND_CHAOS_DIR="$PROJECT_ROOT/deploy/kind/chaos-mesh"
KIND_CHAOS_KUSTOMIZATION="$KIND_CHAOS_DIR/kustomization.yaml"
KIND_CHAOS_NAMESPACE="$KIND_CHAOS_DIR/namespace.yaml"
KIND_CHAOS_SOURCE="$KIND_CHAOS_DIR/source.yaml"
KIND_CHAOS_RELEASE="$KIND_CHAOS_DIR/release.yaml"

# --- Test 1: production overlay no longer lists chaos-mesh source/release
#             files in its resources block (CC-0097, REQ-001) ---
test_flux_system_kustomization_drops_chaos_mesh_entries() {
  echo "Test: deploy/flux-system/kustomization.yaml resources list contains no chaos-mesh entries (CC-0097, REQ-001)"

  if [[ ! -f "$FLUX_SYSTEM_KUSTOMIZATION" ]]; then
    echo "  FAIL: $FLUX_SYSTEM_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  # Match a list-item entry exactly (`- sources/chaos-mesh.yaml`); a comment
  # mentioning the path is fine and expected.
  local source_entry release_entry
  source_entry="$( { grep -E '^[[:space:]]*-[[:space:]]+sources/chaos-mesh\.yaml[[:space:]]*$' "$FLUX_SYSTEM_KUSTOMIZATION" || true; } | wc -l)"
  release_entry="$( { grep -E '^[[:space:]]*-[[:space:]]+releases/chaos-mesh\.yaml[[:space:]]*$' "$FLUX_SYSTEM_KUSTOMIZATION" || true; } | wc -l)"

  assert_eq "no '- sources/chaos-mesh.yaml' entry in resources list" "0" "${source_entry// /}"
  assert_eq "no '- releases/chaos-mesh.yaml' entry in resources list" "0" "${release_entry// /}"
}

# --- Test 2: chaos-mesh source/release files live INSIDE the opt-in overlay
#             so the overlay is self-contained — kustomize build does not need
#             --load-restrictor=LoadRestrictionsNone (CC-0097, REQ-001/REQ-003) ---
test_chaos_mesh_files_local_to_overlay() {
  echo "Test: chaos-mesh source/release files live inside the opt-in overlay (CC-0097, REQ-001)"

  if [[ -f "$KIND_CHAOS_SOURCE" ]]; then
    echo "  PASS: $KIND_CHAOS_SOURCE exists"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $KIND_CHAOS_SOURCE missing — overlay must be self-contained"
    FAIL=$((FAIL + 1))
  fi

  if [[ -f "$KIND_CHAOS_RELEASE" ]]; then
    echo "  PASS: $KIND_CHAOS_RELEASE exists"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $KIND_CHAOS_RELEASE missing — overlay must be self-contained"
    FAIL=$((FAIL + 1))
  fi

  # The legacy on-disk locations under deploy/flux-system/ must be gone:
  # leaving stale duplicates would invite drift between the production-omitted
  # files and the overlay-active copies. kubectl apply -k cannot pass
  # --load-restrictor (kubernetes/kubectl#948), so the overlay MUST be
  # self-contained, and the only reason to keep the flux-system copies would
  # be to support a parent-directory reference that no longer exists.
  if [[ -f "$FLUX_SYSTEM_CHAOS_SOURCE" ]]; then
    echo "  FAIL: $FLUX_SYSTEM_CHAOS_SOURCE still exists — should have been moved into the overlay"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: $FLUX_SYSTEM_CHAOS_SOURCE no longer present (relocated)"
    PASS=$((PASS + 1))
  fi

  if [[ -f "$FLUX_SYSTEM_CHAOS_RELEASE" ]]; then
    echo "  FAIL: $FLUX_SYSTEM_CHAOS_RELEASE still exists — should have been moved into the overlay"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: $FLUX_SYSTEM_CHAOS_RELEASE no longer present (relocated)"
    PASS=$((PASS + 1))
  fi
}

# --- Test 3: production namespaces.yaml no longer declares the chaos-mesh
#             Namespace (CC-0097, REQ-001) ---
test_flux_system_namespaces_drops_chaos_mesh() {
  echo "Test: deploy/flux-system/namespaces.yaml no longer creates the chaos-mesh Namespace (CC-0097, REQ-001)"

  if [[ ! -f "$FLUX_SYSTEM_NAMESPACES" ]]; then
    echo "  FAIL: $FLUX_SYSTEM_NAMESPACES does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  # `name: chaos-mesh` would only appear as a metadata.name of a Namespace
  # in this file; comments referencing the opt-in overlay path are fine
  # because they are not `name:` keys.
  local hits
  hits="$( { grep -E '^[[:space:]]*name:[[:space:]]+chaos-mesh[[:space:]]*$' "$FLUX_SYSTEM_NAMESPACES" || true; } | wc -l)"
  assert_eq "no 'name: chaos-mesh' Namespace entry in namespaces.yaml" "0" "${hits// /}"
}

# --- Test 4: deploy/kind/base/kustomization.yaml no longer carries a patch
#             whose target is the chaos-mesh HelmRelease (CC-0097, REQ-009) ---
test_kind_base_kustomization_drops_chaos_mesh_patch() {
  echo "Test: deploy/kind/base/kustomization.yaml no longer patches the chaos-mesh HelmRelease (CC-0097, REQ-009)"

  if [[ ! -f "$KIND_BASE_KUSTOMIZATION" ]]; then
    echo "  FAIL: $KIND_BASE_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  # Walk every patch entry's target.name and assert none equals chaos-mesh.
  # `yq -r` returns "null" for missing keys, which we filter out. Any
  # remaining "chaos-mesh" line is a violation.
  local patch_target_hits
  patch_target_hits="$(yq -r '.patches[]?.target.name // empty' "$KIND_BASE_KUSTOMIZATION" 2>/dev/null \
    | grep -c '^chaos-mesh$' || true)"

  assert_eq "no patch in kind/base/kustomization.yaml targets HelmRelease/chaos-mesh" \
    "0" "${patch_target_hits// /}"
}

# --- Test 5: deploy/kind/chaos-mesh/ overlay exists with the expected
#             on-disk shape (CC-0097, REQ-002) ---
test_kind_chaos_overlay_files_exist() {
  echo "Test: deploy/kind/chaos-mesh/{kustomization,namespace}.yaml exist with SPDX + Feature ID (CC-0097, REQ-002)"

  if [[ ! -f "$KIND_CHAOS_KUSTOMIZATION" ]]; then
    echo "  FAIL: $KIND_CHAOS_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  if [[ ! -f "$KIND_CHAOS_NAMESPACE" ]]; then
    echo "  FAIL: $KIND_CHAOS_NAMESPACE does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "kustomization.yaml has SPDX FileCopyrightText header" \
    "$KIND_CHAOS_KUSTOMIZATION" \
    "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
  assert_file_contains "kustomization.yaml has SPDX-License-Identifier: Apache-2.0" \
    "$KIND_CHAOS_KUSTOMIZATION" \
    "SPDX-License-Identifier: Apache-2.0"
  assert_file_contains "kustomization.yaml cites Feature: CC-0097" \
    "$KIND_CHAOS_KUSTOMIZATION" \
    "CC-0097"
  assert_file_contains "kustomization.yaml references the local source.yaml" \
    "$KIND_CHAOS_KUSTOMIZATION" \
    "source.yaml$"
  assert_file_contains "kustomization.yaml references the local release.yaml" \
    "$KIND_CHAOS_KUSTOMIZATION" \
    "release.yaml$"
  assert_file_contains "kustomization.yaml lists the inline namespace.yaml" \
    "$KIND_CHAOS_KUSTOMIZATION" \
    "namespace.yaml"

  # Pin the no-parent-dir contract: a `../../` reference would re-introduce
  # the kubectl#948 load-restrictor failure documented at the top of this
  # test file (CC-0097, REQ-003).
  local parent_refs
  parent_refs="$( { grep -E '^[[:space:]]*-[[:space:]]+\.\./\.\.' "$KIND_CHAOS_KUSTOMIZATION" || true; } | wc -l)"
  assert_eq "kustomization.yaml has no '../../' parent-directory resource entries" \
    "0" "${parent_refs// /}"

  assert_file_contains "namespace.yaml declares name: chaos-mesh" \
    "$KIND_CHAOS_NAMESPACE" \
    "name: chaos-mesh"
  assert_file_contains "namespace.yaml carries the privileged PodSecurity label" \
    "$KIND_CHAOS_NAMESPACE" \
    "pod-security.kubernetes.io/enforce: privileged"
}

# --- Test 6: kustomize build of production flux-system renders zero
#             chaos-mesh resources (CC-0097, REQ-001) ---
test_kustomize_build_flux_system_renders_no_chaos_mesh() {
  echo "Test: kustomize build deploy/flux-system renders no chaos-mesh resources (CC-0097, REQ-001)"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$FLUX_SYSTEM_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $FLUX_SYSTEM_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 3))
    return
  fi

  local helmrepo_count helmrelease_count namespace_count
  helmrepo_count="$(printf '%s\n' "$rendered" \
    | awk '
        /^---$/ { kind=""; name="" }
        /^kind:[[:space:]]+HelmRepository[[:space:]]*$/ { kind="HelmRepository" }
        /^[[:space:]]*name:[[:space:]]+chaos-mesh[[:space:]]*$/ {
          if (kind == "HelmRepository" && name == "") { name="chaos-mesh"; print }
        }
      ' | wc -l)"
  helmrelease_count="$(printf '%s\n' "$rendered" \
    | awk '
        /^---$/ { kind=""; name="" }
        /^kind:[[:space:]]+HelmRelease[[:space:]]*$/ { kind="HelmRelease" }
        /^[[:space:]]*name:[[:space:]]+chaos-mesh[[:space:]]*$/ {
          if (kind == "HelmRelease" && name == "") { name="chaos-mesh"; print }
        }
      ' | wc -l)"
  namespace_count="$(printf '%s\n' "$rendered" \
    | awk '
        /^---$/ { kind=""; name="" }
        /^kind:[[:space:]]+Namespace[[:space:]]*$/ { kind="Namespace" }
        /^[[:space:]]*name:[[:space:]]+chaos-mesh[[:space:]]*$/ {
          if (kind == "Namespace" && name == "") { name="chaos-mesh"; print }
        }
      ' | wc -l)"

  assert_eq "production overlay renders zero HelmRepository/chaos-mesh" "0" "${helmrepo_count// /}"
  assert_eq "production overlay renders zero HelmRelease/chaos-mesh" "0" "${helmrelease_count// /}"
  assert_eq "production overlay renders zero Namespace/chaos-mesh" "0" "${namespace_count// /}"
}

# --- Test 7: kustomize build of kind/base succeeds (no orphan patch error)
#             and renders no chaos-mesh resources (CC-0097, REQ-009) ---
test_kustomize_build_kind_base_succeeds_without_chaos_mesh() {
  echo "Test: kustomize build deploy/kind/base succeeds and renders no chaos-mesh resources (CC-0097, REQ-009)"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local rendered
  if ! rendered="$(kustomize build "$KIND_BASE_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_BASE_DIR failed (likely orphan chaos-mesh patch):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 2))
    return
  fi

  local chaos_resource_count
  chaos_resource_count="$(printf '%s\n' "$rendered" \
    | grep -cE '^[[:space:]]*name:[[:space:]]+chaos-mesh[[:space:]]*$' || true)"

  assert_eq "kind/base build succeeds without orphan patch error" "0" "0"
  assert_eq "kind/base output contains no resource named chaos-mesh" "0" "${chaos_resource_count// /}"
}

# --- Test 8: kustomize build of the new opt-in overlay renders the full
#             Chaos Mesh bundle with kind tuning + dependsOn cert-manager
#             preserved (CC-0097, REQ-002) ---
#
# The overlay is self-contained — all resource references are local to
# deploy/kind/chaos-mesh/. Kustomize's default security model
# (LoadRestrictionsRootOnly) is therefore satisfied without any extra flag,
# and the same default applies to kubectl's embedded kustomize used by
# hack/deploy-infra.sh (kubectl does not expose --load-restrictor —
# kubernetes/kubectl#948 — so the no-flag build is the production caller).
test_kustomize_build_kind_chaos_overlay_renders_full_bundle() {
  echo "Test: kustomize build deploy/kind/chaos-mesh renders Namespace + HelmRepository + HelmRelease with kind tuning (CC-0097, REQ-002)"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: yq not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi

  # Mirror the production invocation: NO --load-restrictor flag. kubectl's
  # embedded kustomize (used by hack/deploy-infra.sh) does not expose one,
  # so the unit test must pass without it or the test would approve a
  # rendering that fails for the production caller (CC-0097, REQ-003).
  local rendered
  if ! rendered="$(kustomize build "$KIND_CHAOS_DIR" 2>&1)"; then
    echo "  FAIL: kustomize build $KIND_CHAOS_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 8))
    return
  fi

  # Namespace document with the privileged PodSecurity label.
  local ns_label
  ns_label="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "Namespace" and .metadata.name == "chaos-mesh") | .metadata.labels."pod-security.kubernetes.io/enforce" // empty' \
    2>/dev/null | head -n1)"
  assert_eq "Namespace/chaos-mesh carries the privileged PodSecurity label" \
    "privileged" "$ns_label"

  # HelmRepository at the chaos-mesh chart URL.
  local repo_url
  repo_url="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRepository" and .metadata.name == "chaos-mesh") | .spec.url // empty' \
    2>/dev/null | head -n1)"
  assert_eq "HelmRepository/chaos-mesh points at https://charts.chaos-mesh.org" \
    "https://charts.chaos-mesh.org" "$repo_url"

  # HelmRelease with dependsOn cert-manager preserved verbatim.
  local depends_on_name depends_on_namespace
  depends_on_name="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "chaos-mesh") | .spec.dependsOn[0].name // empty' \
    2>/dev/null | head -n1)"
  depends_on_namespace="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "chaos-mesh") | .spec.dependsOn[0].namespace // empty' \
    2>/dev/null | head -n1)"
  assert_eq "HelmRelease.spec.dependsOn[0].name is cert-manager" "cert-manager" "$depends_on_name"
  assert_eq "HelmRelease.spec.dependsOn[0].namespace is cert-manager" "cert-manager" "$depends_on_namespace"

  # Kind-tuning patch applied: containerd runtime, dashboard disabled,
  # reduced controller-manager resource requests.
  local runtime socket_path dashboard_create cm_cpu
  runtime="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "chaos-mesh") | .spec.values.chaosDaemon.runtime // empty' \
    2>/dev/null | head -n1)"
  socket_path="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "chaos-mesh") | .spec.values.chaosDaemon.socketPath // empty' \
    2>/dev/null | head -n1)"
  dashboard_create="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "chaos-mesh") | .spec.values.dashboard.create' \
    2>/dev/null | head -n1)"
  cm_cpu="$(printf '%s\n' "$rendered" | yq -r \
    'select(.kind == "HelmRelease" and .metadata.name == "chaos-mesh") | .spec.values.controllerManager.resources.requests.cpu // empty' \
    2>/dev/null | head -n1)"

  assert_eq "chaosDaemon.runtime is containerd" "containerd" "$runtime"
  assert_eq "chaosDaemon.socketPath is /run/containerd/containerd.sock" \
    "/run/containerd/containerd.sock" "$socket_path"
  assert_eq "dashboard.create is false" "false" "$dashboard_create"
  assert_eq "controllerManager.resources.requests.cpu is 25m" "25m" "$cm_cpu"
}

# --- Run ---
test_flux_system_kustomization_drops_chaos_mesh_entries
test_chaos_mesh_files_local_to_overlay
test_flux_system_namespaces_drops_chaos_mesh
test_kind_base_kustomization_drops_chaos_mesh_patch
test_kind_chaos_overlay_files_exist
test_kustomize_build_flux_system_renders_no_chaos_mesh
test_kustomize_build_kind_base_succeeds_without_chaos_mesh
test_kustomize_build_kind_chaos_overlay_renders_full_bundle

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
