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
#     one writer on an RWO claim, a privileged NFS-Ganesha container whose
#     configuration exports /exports as the NFSv4 pseudo-root, caches neither
#     attributes nor directory entries, and keeps its client records in
#     /var/lib/nfs/ganesha under a fixed server scope, the
#     claim mounted twice by subPath, digest-pinned images, and the memory
#     Ganesha needs.
#   - The init container's script, run against a temporary directory with a
#     stub chown, lays a claim out as exports/ and ganesha/, moves the entries
#     of a claim the kernel server wrote into exports/, moves nothing on a
#     second run, removes the empty volumes/ and backups/ a later kernel
#     server start leaves at the root, stops on any other name present on
#     both levels and on a failed mv, and hands exports/volumes and
#     exports/backups to 42424:42424 mode 0770.
#   - The Service exposes 2049/TCP alone (the server speaks NFSv4 only, so
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

SERVER_IMAGE_PATTERN='^ghcr\.io/kubernetes-sigs/nfs-ganesha:[^@]+@sha256:[a-f0-9]{64}$'

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

# Read a whole multi-line value out of a rendered stream, such as a script.
render_text() {
  local stream="$1" expression="$2"
  printf '%s\n' "$stream" | yq -r "$expression" 2>/dev/null | grep -v '^---$'
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
  # Renovate bumps the pin (see the csi-driver-nfs chart flux manager rules).
  local version
  version="$(render_value "$rendered" "$release | .spec.chart.spec.version")"
  if grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' <<<"$version"; then
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
    echo "  SKIP: kustomize or yq not installed (34 checks skipped)"
    SKIP=$((SKIP + 34))
    return
  fi

  local rendered
  if ! rendered="$(render_dir "$KIND_NFS_DIR")"; then
    echo "  FAIL: kustomize build $KIND_NFS_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 34))
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

  # The image has no entrypoint; both containers run a script.
  assert_eq "the server container runs its script through /bin/sh -c" \
    "/bin/sh -c" "$(render_value "$rendered" "$server | .command | join(\" \")")"
  assert_eq "the init container runs its script through /bin/sh -c" \
    "/bin/sh -c" "$(render_value "$rendered" "$init | .command | join(\" \")")"
  local env_lines
  env_lines="$(count_matches "$rendered" 'SHARED_DIRECTORY')"
  assert_eq "no rendered line names SHARED_DIRECTORY" "0" "${env_lines// /}"

  # Each line of the Ganesha configuration the export and the client records
  # rest on, the log level that prints the `Root fs for export` line nfs-health
  # requires, and the exec that makes Ganesha PID 1. Lines are compared whole,
  # with their indentation trimmed.
  local script line
  script="$(render_text "$rendered" "$server | .args[0]" | sed -E 's/^[[:space:]]+//')"
  for line in 'Protocols = 4;' 'RecoveryBackend = fs;' 'RecoveryRoot = /var/lib/nfs/ganesha;' \
    'Minor_Versions = 1, 2;' 'Server_Scope = "nfs-server.openstack";' 'Path = /exports;' 'Pseudo = /;' \
    'Squash = No_Root_Squash;' 'Attr_Expiration_Time = 0;' 'Dir_Chunk = 0;' 'FSAL { Name = VFS; }' \
    'COMPONENTS { FSAL = INFO; }' \
    'exec ganesha.nfsd -F -L /dev/stdout -f /tmp/ganesha.conf -p /tmp/ganesha.pid'; do
    if grep -qxF -- "$line" <<<"$script"; then
      echo "  PASS: the server script holds the line '$line'"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: the server script lacks the line '$line'"
      FAIL=$((FAIL + 1))
    fi
  done
  # The core block and the export block both restrict Ganesha to NFSv4.
  assert_eq "the server script sets Protocols = 4 in both blocks" "2" \
    "$(grep -cxF -- 'Protocols = 4;' <<<"$script")"

  assert_eq "the server container is privileged (Ganesha reads the filesystem UUID from the device node)" \
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

  # One claim, two subPath mounts: the export and, outside it, the records.
  assert_eq "the server mounts exports/ at /exports and ganesha/ at /var/lib/nfs/ganesha" \
    "exports:/exports:exports exports:/var/lib/nfs/ganesha:ganesha" \
    "$(render_value "$rendered" "$server | .volumeMounts | map(.name + \":\" + .mountPath + \":\" + (.subPath // \"\")) | join(\" \")")"
  assert_eq "the init container is named prepare-exports" \
    "prepare-exports" "$(render_value "$rendered" "$init | .name")"
  assert_eq "the init container mounts the claim root at /claim, without a subPath" \
    "exports:/claim:" \
    "$(render_value "$rendered" "$init | .volumeMounts | map(.name + \":\" + .mountPath + \":\" + (.subPath // \"\")) | join(\" \")")"

  # Ganesha does the I/O in its own process: 397 MiB at its measured peak.
  assert_eq "the server container is limited to 1Gi" \
    "1Gi" "$(render_value "$rendered" "$server | .resources.limits.memory")"
  assert_eq "the server container requests 128Mi" \
    "128Mi" "$(render_value "$rendered" "$server | .resources.requests.memory")"

  # One pinned image for the whole overlay.
  local server_image init_image
  server_image="$(render_value "$rendered" "$server | .image")"
  init_image="$(render_value "$rendered" "$init | .image")"
  assert_not_empty "the server container declares an image" "$server_image"
  assert_eq "the init container reuses the server image" "$server_image" "$init_image"
  if grep -qE "$SERVER_IMAGE_PATTERN" <<<"$server_image"; then
    echo "  PASS: the server image is a digest-pinned nfs-ganesha tag"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the server image '$server_image' is not a digest-pinned nfs-ganesha tag"
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

# run_prepare_exports <script> <claim dir> <stub dir>
# Runs the init container's script with bash -c against <claim dir>, the stubs
# of <stub dir> first on PATH. Prints its output; returns its exit status.
run_prepare_exports() {
  local script="${1//\/claim/$2}"
  : >"$3/chown.log"
  CHOWN_LOG="$3/chown.log" PATH="$3:$PATH" bash -c "$script" 2>&1
}

# --- Test 6: prepare-exports lays the claim out and moves a flat one ---
test_prepare_exports_script() {
  echo "Test: prepare-exports lays the claim out, moves a kernel server's claim into exports/, and stops on a conflict"

  if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
    echo "  SKIP: kustomize or yq not installed (27 checks skipped)"
    SKIP=$((SKIP + 27))
    return
  fi

  local rendered script
  if ! rendered="$(render_dir "$KIND_NFS_DIR")"; then
    echo "  FAIL: kustomize build $KIND_NFS_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 27))
    return
  fi
  script="$(render_text "$rendered" \
    'select(.kind == "Deployment" and .metadata.name == "nfs-server") | .spec.template.spec.initContainers[] | select(.name == "prepare-exports") | .args[0]')"
  # The script runs on this machine with /claim replaced, so one that names
  # no /claim would act on paths outside the temporary directory.
  if [[ -z "$script" || "$script" != *"/claim"* ]]; then
    echo "  FAIL: the prepare-exports script is empty or names no /claim; not running it (27 checks)"
    FAIL=$((FAIL + 27))
    return
  fi

  local tmp claim out rc
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin" "$tmp/badmv"
  # Not root: the stub chown logs instead of changing the owner.
  printf '#!/bin/sh\necho "$*" >>"$CHOWN_LOG"\n' >"$tmp/bin/chown"
  printf '#!/bin/sh\necho "mv: cannot move $1" >&2\nexit 1\n' >"$tmp/badmv/mv"
  cp "$tmp/bin/chown" "$tmp/badmv/chown"
  chmod +x "$tmp/bin/chown" "$tmp/badmv/mv" "$tmp/badmv/chown"

  # An empty claim, as on a fresh cluster.
  claim="$tmp/empty"
  mkdir -p "$claim"
  rc=0
  out="$(run_prepare_exports "$script" "$claim" "$tmp/bin")" || rc=$?
  assert_eq "an empty claim: exit 0" "0" "$rc"
  assert_eq "an empty claim: exports/volumes, exports/backups and ganesha exist" "yes" \
    "$( [[ -d "$claim/exports/volumes" && -d "$claim/exports/backups" && -d "$claim/ganesha" ]] && echo yes)"
  assert_eq "an empty claim: both exports are handed to 42424:42424" \
    "42424:42424 $claim/exports/volumes $claim/exports/backups" "$(cat "$tmp/bin/chown.log")"
  assert_eq "an empty claim: both exports are mode 0770" "770 770" \
    "$(stat -c '%a' "$claim/exports/volumes" "$claim/exports/backups" | paste -sd ' ' -)"
  assert_eq "an empty claim: ganesha is mode 0700" "700" "$(stat -c '%a' "$claim/ganesha")"
  assert_not_contains "an empty claim: nothing is moved" "$out" "moved"
  assert_contains "an empty claim: the closing line is printed" "$out" \
    "prepare-exports: exports/volumes and exports/backups are 42424:42424 0770"

  # A claim the kernel server wrote: the shares at its root.
  claim="$tmp/flat"
  mkdir -p "$claim/volumes" "$claim/backups" "$claim/volumes-b" "$claim/lost+found"
  touch "$claim/volumes/volume-old" "$claim/.marker"
  rc=0
  out="$(run_prepare_exports "$script" "$claim" "$tmp/bin")" || rc=$?
  assert_eq "a flat claim: exit 0" "0" "$rc"
  assert_eq "a flat claim: four entries are moved into exports/" \
    "$(printf 'prepare-exports: moved %s into exports/\n' .marker backups volumes volumes-b | sort)" \
    "$(grep 'moved' <<<"$out" | sort)"
  assert_eq "a flat claim: the volume file is in exports/volumes" "yes" \
    "$( [[ -f "$claim/exports/volumes/volume-old" ]] && echo yes)"
  assert_eq "a flat claim: lost+found stays at the top" "yes" \
    "$( [[ -d "$claim/lost+found" && ! -e "$claim/exports/lost+found" ]] && echo yes)"
  assert_eq "a flat claim: the root holds exports, ganesha and lost+found alone" \
    "exports ganesha lost+found" "$(ls -A "$claim" | sort | paste -sd ' ' -)"

  # The same claim again: a second start moves nothing.
  rc=0
  out="$(run_prepare_exports "$script" "$claim" "$tmp/bin")" || rc=$?
  assert_eq "a second run: exit 0" "0" "$rc"
  assert_not_contains "a second run: nothing is moved" "$out" "moved"
  assert_eq "a second run: the volume file stays" "yes" \
    "$( [[ -f "$claim/exports/volumes/volume-old" ]] && echo yes)"

  # A deploy of the kernel server over a laid-out claim: empty volumes/ and
  # backups/ at the root again, beside the shares in exports/.
  claim="$tmp/downgraded"
  mkdir -p "$claim/volumes" "$claim/backups" "$claim/exports/volumes" "$claim/exports/backups" "$claim/ganesha"
  touch "$claim/exports/volumes/volume-old"
  rc=0
  out="$(run_prepare_exports "$script" "$claim" "$tmp/bin")" || rc=$?
  assert_eq "a kernel server's empty leftovers: exit 0" "0" "$rc"
  assert_eq "a kernel server's empty leftovers: both are removed" \
    "$(printf 'prepare-exports: removed the empty %s left by a kernel-server start\n' "$claim/backups" "$claim/volumes")" \
    "$(grep 'removed' <<<"$out" | sort)"
  assert_eq "a kernel server's empty leftovers: the root holds exports and ganesha alone" \
    "exports ganesha" "$(ls -A "$claim" | sort | paste -sd ' ' -)"
  assert_eq "a kernel server's empty leftovers: the volume file stays in exports/volumes" "yes" \
    "$( [[ -f "$claim/exports/volumes/volume-old" ]] && echo yes)"

  # A name on both levels that holds a file: the script stops before the next
  # entry.
  claim="$tmp/collision"
  mkdir -p "$claim/volumes" "$claim/exports/volumes" "$claim/zzz"
  touch "$claim/volumes/volume-new"
  rc=0
  out="$(run_prepare_exports "$script" "$claim" "$tmp/bin")" || rc=$?
  assert_eq "a collision: exit 1" "1" "$rc"
  assert_contains "a collision: the line names both paths" "$out" \
    "prepare-exports: $claim/volumes and $claim/exports/volumes both exist; move one of them away"
  assert_eq "a collision: zzz stays at the top" "yes" \
    "$( [[ -d "$claim/zzz" && ! -e "$claim/exports/zzz" ]] && echo yes)"
  assert_eq "a collision: no export is handed over" "" "$(cat "$tmp/bin/chown.log")"

  # A failing mv: set -e ends the script at that line.
  claim="$tmp/badmv-claim"
  mkdir -p "$claim/volumes"
  rc=0
  out="$(run_prepare_exports "$script" "$claim" "$tmp/badmv")" || rc=$?
  assert_nonzero_exit "a failed mv: the script exits non-zero" "$rc"
  assert_not_contains "a failed mv: no moved line for the entry" "$out" "moved volumes"
  assert_not_contains "a failed mv: no closing line" "$out" "42424:42424 0770"
  assert_eq "a failed mv: the entry stays at the top" "yes" \
    "$( [[ -d "$claim/volumes" && ! -e "$claim/exports/volumes" ]] && echo yes)"

  rm -rf "$tmp"
}

# --- Test 7: the Service exposes 2049/TCP alone ---
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

  # The server speaks NFSv4 only, so rpcbind's 111 and a mountd port are
  # neither reachable nor needed anywhere in the overlay.
  local legacy_ports
  legacy_ports="$(count_matches "$rendered" 'port:[[:space:]]+(111|20048)[[:space:]]*$')"
  assert_eq "no rpcbind (111) or mountd (20048) port is exposed" "0" "${legacy_ports// /}"
}

# --- Tests 8-10: default posture renders nothing NFS ---
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
test_prepare_exports_script
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
