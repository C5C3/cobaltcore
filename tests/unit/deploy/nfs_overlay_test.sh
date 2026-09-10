#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the NFS opt-in overlay:
#   - deploy/kind/nfs/{kustomization,source,release,nfs-server}.yaml exist
#     with SPDX headers, the kustomization references the three local files
#     and nothing outside the overlay (no parent-directory paths), and it
#     creates no Namespace (both halves land in pre-existing ones).
#   - kustomize build of the overlay renders the expected five documents
#     under the default LoadRestrictionsRootOnly security check (no
#     --load-restrictor flag): the csi-driver-nfs HelmRepository in
#     flux-system, the csi-driver-nfs HelmRelease in kube-system, and the
#     PVC + Deployment + Service of the NFS server in openstack.
#   - The HelmRelease overrides three values and no more, so the chart
#     defaults the driver relies on (attachRequired, fsGroupPolicy,
#     kubeletDir) stay untouched.
#   - The server Deployment keeps the contract the Cinder suites depend on:
#     one writer on an RWO claim, SHARED_DIRECTORY=/exports, a privileged
#     container, digest-pinned images, and exports pre-created as
#     42424:42424 mode 0770.
#   - The Service exposes 2049/TCP alone (the image serves NFSv4 only, so
#     neither rpcbind's 111 nor a mountd port is reachable or needed).
#   - kustomize build of deploy/flux-system, deploy/kind/base and
#     deploy/kind/infrastructure renders ZERO NFS resources (default posture:
#     nothing is applied unless the overlay is asked for).
# Usage: bash tests/unit/deploy/nfs_overlay_test.sh

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
KIND_INFRA_DIR="$PROJECT_ROOT/deploy/kind/infrastructure"
KIND_NFS_DIR="$PROJECT_ROOT/deploy/kind/nfs"
KIND_NFS_KUSTOMIZATION="$KIND_NFS_DIR/kustomization.yaml"
KIND_NFS_SOURCE="$KIND_NFS_DIR/source.yaml"
KIND_NFS_RELEASE="$KIND_NFS_DIR/release.yaml"
KIND_NFS_SERVER="$KIND_NFS_DIR/nfs-server.yaml"

SERVER_IMAGE_PATTERN='^docker\.io/itsthenetwork/nfs-server-alpine:[^@]+@sha256:[a-f0-9]{64}$'

# Read a single value out of a rendered stream. Prints the first line of the
# yq result, or the empty string when the expression matches nothing; every
# caller compares against a non-empty expectation, so an empty read is a FAIL
# rather than a silent pass.
render_value() {
  local stream="$1" expression="$2"
  printf '%s\n' "$stream" | yq -r "$expression" 2>/dev/null \
    | grep -v '^---$' | grep -v '^null$' | head -n1
}

# Count lines of the rendered stream matching an extended regular expression.
count_matches() {
  local stream="$1" pattern="$2"
  printf '%s\n' "$stream" | { grep -cE "$pattern" || true; }
}

# Render a kustomization the way hack/deploy-infra.sh does: no
# --load-restrictor flag, because kubectl's embedded kustomize has none.
# Prints the rendered stream on success, the error output on failure.
render_dir() {
  kustomize build "$1" 2>&1
}

# Assert that a rendered stream carries no NFS resource. Used for the three
# default-posture directories.
assert_renders_no_nfs() {
  local label="$1" dir="$2"

  if ! command -v kustomize >/dev/null 2>&1; then
    echo "  SKIP: kustomize not installed (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local rendered
  if ! rendered="$(render_dir "$dir")"; then
    echo "  FAIL: kustomize build $dir failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 1))
    return
  fi

  local resource_count
  resource_count="$(count_matches "$rendered" \
    '^[[:space:]]*name:[[:space:]]+(nfs-server|csi-driver-nfs)[[:space:]]*$')"
  assert_eq "$label renders no resource named nfs-server or csi-driver-nfs" \
    "0" "${resource_count// /}"
}

# --- Test 1: overlay files exist with SPDX headers ---
test_overlay_files_exist_with_spdx() {
  echo "Test: deploy/kind/nfs/{kustomization,source,release,nfs-server}.yaml exist with SPDX headers"

  local f
  for f in "$KIND_NFS_KUSTOMIZATION" "$KIND_NFS_SOURCE" "$KIND_NFS_RELEASE" "$KIND_NFS_SERVER"; do
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

# --- Test 2: the kustomization is self-contained ---
test_kustomization_is_self_contained() {
  echo "Test: kustomization references the three local files and is self-contained"

  if [[ ! -f "$KIND_NFS_KUSTOMIZATION" ]]; then
    echo "  FAIL: $KIND_NFS_KUSTOMIZATION does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  assert_file_contains "kustomization.yaml references the local source.yaml" \
    "$KIND_NFS_KUSTOMIZATION" "source.yaml$"
  assert_file_contains "kustomization.yaml references the local release.yaml" \
    "$KIND_NFS_KUSTOMIZATION" "release.yaml$"
  assert_file_contains "kustomization.yaml references the local nfs-server.yaml" \
    "$KIND_NFS_KUSTOMIZATION" "nfs-server.yaml$"

  # Pin the no-parent-dir contract: any `../` reference re-introduces the
  # kubectl#948 load-restrictor failure, not only a `../../` one.
  local parent_refs
  parent_refs="$( { grep -cE '^[[:space:]]*-[[:space:]]+\.\./' "$KIND_NFS_KUSTOMIZATION" || true; } )"
  assert_eq "kustomization.yaml has no parent-directory resource entries" \
    "0" "${parent_refs// /}"

  # The overlay must NOT ship an inline namespace.yaml: the chart installs
  # into kube-system and the server into openstack, both pre-existing.
  local ns_entry
  ns_entry="$( { grep -cE '^[[:space:]]*-[[:space:]]+namespace\.yaml[[:space:]]*$' "$KIND_NFS_KUSTOMIZATION" || true; } )"
  assert_eq "kustomization.yaml does not list a namespace.yaml resource" \
    "0" "${ns_entry// /}"
}

# --- Test 3: kustomize build renders the five expected documents ---
test_kustomize_build_renders_five_documents() {
  echo "Test: kustomize build deploy/kind/nfs renders the five expected documents"

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
  if ! rendered="$(render_dir "$KIND_NFS_DIR")"; then
    echo "  FAIL: kustomize build $KIND_NFS_DIR failed (default LoadRestrictionsRootOnly):"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 2))
    return
  fi

  local expected actual
  expected="$(printf '%s\n' \
    'Deployment/nfs-server@openstack' \
    'HelmRelease/csi-driver-nfs@kube-system' \
    'HelmRepository/csi-driver-nfs@flux-system' \
    'PersistentVolumeClaim/nfs-server-exports@openstack' \
    'Service/nfs-server@openstack')"
  actual="$(printf '%s\n' "$rendered" \
    | yq -r '.kind + "/" + .metadata.name + "@" + (.metadata.namespace // "")' 2>/dev/null \
    | grep -v '^---$' | sort)"
  assert_eq "the overlay renders the five NFS documents and no others" "$expected" "$actual"

  # No Namespace of its own: both halves land in pre-existing Namespaces.
  local ns_count
  ns_count="$(count_matches "$rendered" '^kind:[[:space:]]+Namespace[[:space:]]*$')"
  assert_eq "the overlay renders no Namespace" "0" "${ns_count// /}"
}

# --- Test 4: HelmRepository / HelmRelease contract ---
test_helm_release_contract() {
  echo "Test: the csi-driver-nfs source and release carry the expected contract"

  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi

  local rendered
  if ! rendered="$(render_dir "$KIND_NFS_DIR")"; then
    echo "  FAIL: kustomize build $KIND_NFS_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 8))
    return
  fi

  local repo='select(.kind == "HelmRepository" and .metadata.name == "csi-driver-nfs")'
  local release='select(.kind == "HelmRelease" and .metadata.name == "csi-driver-nfs")'

  assert_eq "HelmRepository/csi-driver-nfs lives in flux-system" \
    "flux-system" "$(render_value "$rendered" "$repo | .metadata.namespace")"
  assert_eq "HelmRepository/csi-driver-nfs points at the upstream chart directory" \
    "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts" \
    "$(render_value "$rendered" "$repo | .spec.url")"

  assert_eq "the HelmRelease sourceRef targets the HelmRepository in flux-system" \
    "HelmRepository/csi-driver-nfs/flux-system" \
    "$(render_value "$rendered" \
      "$release | .spec.chart.spec.sourceRef | .kind + \"/\" + .name + \"/\" + .namespace")"

  # The chart version is an exact pin, not a range: a range would let Flux
  # adopt a new chart with no repo diff, and this one installs a privileged
  # hostNetwork DaemonSet whose relied-on defaults are not overridden below.
  # Renovate bumps the pin (see the csi-driver-nfs chart customManager).
  local version
  version="$(render_value "$rendered" "$release | .spec.chart.spec.version")"
  if printf '%s' "$version" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "  PASS: chart version '$version' is an exact pin"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: chart version '$version' is not an exact x.y.z pin"
    FAIL=$((FAIL + 1))
  fi

  assert_eq "the release disables the csi-snapshotter sidecar" \
    "false" "$(render_value "$rendered" "$release | .spec.values.controller.enableSnapshotter")"
  assert_eq "the release creates no dynamic StorageClass" \
    "false" "$(render_value "$rendered" "$release | .spec.values.storageClass.create")"
  # The cinder-operator mounts every share as an inline `csi:` volume, which
  # the driver serves only when its CSIDriver lists the Ephemeral lifecycle
  # mode. The chart adds that mode behind this flag.
  assert_eq "the release enables inline CSI volumes" \
    "true" "$(render_value "$rendered" "$release | .spec.values.feature.enableInlineVolume")"

  # Nothing else is overridden, so every chart default the driver relies on
  # (attachRequired, fsGroupPolicy, kubeletDir) stays as shipped. The leaf
  # paths are re-rooted by a second yq pass because `path` is reported from
  # the document root, not from the selected node.
  local value_leaves
  value_leaves="$(printf '%s\n' "$rendered" | yq -r "$release | .spec.values" 2>/dev/null \
    | yq -r '[.. | select(tag != "!!map") | path | join(".")] | sort | join(",")' 2>/dev/null \
    | head -n1)"
  assert_eq "the release overrides only the three documented values" \
    "controller.enableSnapshotter,feature.enableInlineVolume,storageClass.create" \
    "$value_leaves"
}

# --- Test 5: NFS server Deployment and PVC contract ---
test_nfs_server_deployment_contract() {
  echo "Test: the nfs-server Deployment and its claim carry the expected contract"

  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (15 checks skipped)"
    SKIP=$((SKIP + 15))
    return
  fi

  local rendered
  if ! rendered="$(render_dir "$KIND_NFS_DIR")"; then
    echo "  FAIL: kustomize build $KIND_NFS_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 15))
    return
  fi

  local deploy='select(.kind == "Deployment" and .metadata.name == "nfs-server")'
  local server="$deploy | .spec.template.spec.containers[] | select(.name == \"nfs-server\")"
  local init="$deploy | .spec.template.spec.initContainers[] | select(.name == \"prepare-exports\")"

  # An RWO claim admits one writer, so a rolling update would deadlock.
  assert_eq "the Deployment replaces its pod instead of rolling it" \
    "Recreate" "$(render_value "$rendered" "$deploy | .spec.strategy.type")"
  assert_eq "the Deployment runs a single replica" \
    "1" "$(render_value "$rendered" "$deploy | .spec.replicas")"

  # The image entrypoint exits 1 without SHARED_DIRECTORY.
  assert_eq "the server container sets SHARED_DIRECTORY to /exports" \
    "/exports" "$(render_value "$rendered" \
      "$server | .env[] | select(.name == \"SHARED_DIRECTORY\") | .value")"
  assert_eq "the server container is privileged (it drives the host kernel's rpc.nfsd)" \
    "true" "$(render_value "$rendered" "$server | .securityContext.privileged")"
  # The server never calls the Kubernetes API, so the privileged pod must not
  # carry a bearer token for the `openstack` default ServiceAccount.
  assert_eq "the pod mounts no ServiceAccount token" \
    "false" "$(render_value "$rendered" \
      "$deploy | .spec.template.spec.automountServiceAccountToken")"
  assert_eq "the readiness probe checks 2049" \
    "2049" "$(render_value "$rendered" "$server | .readinessProbe.tcpSocket.port")"
  assert_eq "the liveness probe checks 2049" \
    "2049" "$(render_value "$rendered" "$server | .livenessProbe.tcpSocket.port")"

  # The init container hands both exports to the restricted openstack user,
  # so the NFS backup driver skips its chgrp/chmod root branches.
  local init_name init_command
  init_name="$(render_value "$rendered" "$init | .name")"
  assert_eq "the init container is named prepare-exports" "prepare-exports" "$init_name"
  init_command="$(printf '%s\n' "$rendered" | yq -r "$init | .command | join(\" \")" 2>/dev/null \
    | grep -v '^---$' | head -n1)"
  assert_not_empty "the init container declares a command" "$init_command"
  assert_contains "the init container creates both export directories" \
    "$init_command" "mkdir -p /exports/volumes /exports/backups"
  assert_contains "the init container chowns the exports to 42424:42424" \
    "$init_command" "chown 42424:42424"
  assert_contains "the init container sets mode 0770 on the exports" \
    "$init_command" "chmod 0770"

  # One pinned image for the whole overlay.
  local server_image init_image
  server_image="$(render_value "$rendered" "$server | .image")"
  init_image="$(render_value "$rendered" "$init | .image")"
  assert_not_empty "the server container declares an image" "$server_image"
  assert_eq "the init container reuses the server image" "$server_image" "$init_image"
  if printf '%s' "$server_image" | grep -qE "$SERVER_IMAGE_PATTERN"; then
    echo "  PASS: the server image is a digest-pinned nfs-server-alpine tag"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the server image '$server_image' is not a digest-pinned nfs-server-alpine tag"
    FAIL=$((FAIL + 1))
  fi

  assert_eq "the pod mounts the nfs-server-exports claim" \
    "nfs-server-exports" "$(render_value "$rendered" \
      "$deploy | .spec.template.spec.volumes[] | select(.persistentVolumeClaim) | .persistentVolumeClaim.claimName")"

  local pvc='select(.kind == "PersistentVolumeClaim" and .metadata.name == "nfs-server-exports")'
  assert_eq "the claim uses kind's local-path class" \
    "standard" "$(render_value "$rendered" "$pvc | .spec.storageClassName")"
  assert_eq "the claim is ReadWriteOnce and nothing else" \
    "ReadWriteOnce" "$(printf '%s\n' "$rendered" | yq -r "$pvc | .spec.accessModes | join(\",\")" 2>/dev/null \
      | grep -v '^---$' | head -n1)"
}

# --- Test 6: the Service exposes 2049/TCP alone ---
test_service_exposes_only_2049() {
  echo "Test: the nfs-server Service exposes 2049/TCP alone"

  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local rendered
  if ! rendered="$(render_dir "$KIND_NFS_DIR")"; then
    echo "  FAIL: kustomize build $KIND_NFS_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 6))
    return
  fi

  local svc='select(.kind == "Service" and .metadata.name == "nfs-server")'
  assert_eq "the Service is a ClusterIP" \
    "ClusterIP" "$(render_value "$rendered" "$svc | .spec.type")"
  assert_eq "the Service declares a single port" \
    "1" "$(render_value "$rendered" "$svc | .spec.ports | length")"
  assert_eq "the Service port is 2049" \
    "2049" "$(render_value "$rendered" "$svc | .spec.ports[0].port")"
  assert_eq "the Service targets container port 2049" \
    "2049" "$(render_value "$rendered" "$svc | .spec.ports[0].targetPort")"
  assert_eq "the Service port is TCP" \
    "TCP" "$(render_value "$rendered" "$svc | .spec.ports[0].protocol")"

  # The image serves NFSv4 only, so rpcbind's 111 and a mountd port are
  # neither reachable nor needed anywhere in the overlay.
  local legacy_ports
  legacy_ports="$(count_matches "$rendered" 'port:[[:space:]]+(111|20048)[[:space:]]*$')"
  assert_eq "no rpcbind (111) or mountd (20048) port is exposed" "0" "${legacy_ports// /}"
}

# --- Tests 7-9: default posture renders nothing NFS ---
test_flux_system_renders_no_nfs() {
  echo "Test: kustomize build deploy/flux-system renders no NFS resources"
  assert_renders_no_nfs "the production overlay" "$FLUX_SYSTEM_DIR"
}

test_kind_base_renders_no_nfs() {
  echo "Test: kustomize build deploy/kind/base renders no NFS resources"
  assert_renders_no_nfs "kind/base" "$KIND_BASE_DIR"
}

test_kind_infrastructure_renders_no_nfs() {
  echo "Test: kustomize build deploy/kind/infrastructure renders no NFS resources"
  assert_renders_no_nfs "kind/infrastructure" "$KIND_INFRA_DIR"
}

# --- Run ---
test_overlay_files_exist_with_spdx
test_kustomization_is_self_contained
test_kustomize_build_renders_five_documents
test_helm_release_contract
test_nfs_server_deployment_contract
test_service_exposes_only_2049
test_flux_system_renders_no_nfs
test_kind_base_renders_no_nfs
test_kind_infrastructure_renders_no_nfs

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
