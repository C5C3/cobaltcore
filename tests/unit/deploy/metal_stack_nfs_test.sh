#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the NFS overlay of the metal-stack lab, deploy/lab/metal-stack/nfs,
# which hack/deploy-infra.sh applies under EXTERNAL_CLUSTER=true WITH_NFS=true:
#   1. The three files carry SPDX headers, and the kustomization lists
#      exactly ../../../kind/nfs and client-modules-daemonset.yaml.
#   2. The render holds six objects, the five of the kind overlay and the
#      DaemonSet nfs-client-modules, and no Namespace.
#   3. The export claim asks for 100Gi, ReadWriteOnce, and names no storage
#      class; no line of the render sets one.
#   4. The server loads nfsd in load-nfsd before prepare-exports, on the image
#      of host-prepare (deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml),
#      privileged, with a read-only root and the node's /lib/modules mounted
#      read-only; its own container keeps the kind image and SHARED_DIRECTORY.
#   5. The mounter is the kind release: its chart version, inline volumes on.
#   6. nfs-client-modules mounts no token, shares no host namespace, rolls
#      out with RollingUpdate, tolerates every taint, loads in a privileged
#      init container and holds the pod in an unprivileged container, both on
#      host-prepare's image, with /lib/modules mounted read-only.
#   7. The two load scripts, run with a stub modprobe first on PATH, report
#      success and exit 0, or name the module that failed and exit 1 without
#      loading the next one.
#   8. deploy/kind/nfs still renders five documents, its claim on `standard`
#      with 5Gi, and one init container.
#   9. client-policy.yaml, the template hack/deploy-infra.sh applies outside
#      the kustomization, is one NetworkPolicy that selects the server's pods
#      and admits one ipBlock, the placeholder NODE_NETWORK, to TCP 2049.
#
# Checks 2 to 9 are counted as SKIP when kustomize or yq is not on PATH, the
# placeholder line of check 9 aside; a failing kustomize build counts checks 2
# to 8 as FAIL and prints the build's error.
#
# Usage: bash tests/unit/deploy/metal_stack_nfs_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"
# shellcheck source=tests/lib/kustomize_render.sh
source "$PROJECT_ROOT/tests/lib/kustomize_render.sh"

NFS_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/nfs"
KIND_NFS_DIR="$PROJECT_ROOT/deploy/kind/nfs"
LIBVIRT_DAEMONSET="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml"

# The kernel the stub uname reports, the lab's on 2026-10-03.
STUB_KERNEL="6.1.0-49-amd64"

RENDERED=""

# The pod spec of the server and of the DaemonSet, as yq paths.
SERVER_POD='.spec.template.spec'
LOAD_NFSD="$SERVER_POD.initContainers[] | select(.name == \"load-nfsd\")"
DS_POD='.spec.template.spec'
DS_LOAD="$DS_POD.initContainers[] | select(.name == \"load\")"
DS_HOLD="$DS_POD.containers[] | select(.name == \"hold\")"

# host_prepare_image — the image of host-prepare in libvirt-daemonset.yaml.
host_prepare_image() {
  yq -N -r 'select(. != null) | .spec.template.spec.initContainers[] | select(.name == "host-prepare") | .image' \
    "$LIBVIRT_DAEMONSET" | head -n 1
}

# assert_same_nonempty <description> <value> <reference value>
# An empty string on either side fails: two empty images are not one image.
assert_same_nonempty() {
  if [[ -n "$2" && -n "$3" && "$2" == "$3" ]]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (got '$2', want '$3')"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 1: the files ---
test_files_have_spdx_and_two_resources() {
  echo "Test: the overlay files carry SPDX headers and the kustomization lists the kind overlay and the DaemonSet"

  local f
  for f in kustomization.yaml client-modules-daemonset.yaml client-policy.yaml; do
    if [[ ! -f "$NFS_DIR/$f" ]]; then
      echo "  FAIL: $NFS_DIR/$f does not exist"
      FAIL=$((FAIL + 2))
      continue
    fi
    assert_file_contains "$f has SPDX FileCopyrightText header" \
      "$NFS_DIR/$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$f has SPDX-License-Identifier: Apache-2.0" \
      "$NFS_DIR/$f" "SPDX-License-Identifier: Apache-2.0"
  done
  assert_eq "kustomization.yaml lists ../../../kind/nfs and client-modules-daemonset.yaml alone" \
    "$(printf '%s\n' ../../../kind/nfs client-modules-daemonset.yaml)" \
    "$(resource_entries "$NFS_DIR/kustomization.yaml" 2>/dev/null)"
}

# --- Test 2: the six objects ---
test_render_holds_the_six_objects() {
  echo "Test: kustomize build deploy/lab/metal-stack/nfs renders the six objects and no Namespace"

  render "$NFS_DIR" 3 || return

  local objects
  objects="$(printf '%s\n' "$RENDERED" |
    yq -N -r 'select(. != null) | .kind + " " + (.metadata.namespace // "") + "/" + .metadata.name' - |
    LC_ALL=C sort)"
  assert_eq "the render holds six documents" "6" "$(grep -c . <<<"$objects")"
  assert_eq "the render holds the five objects of the kind overlay and the DaemonSet" \
    "$(printf '%s\n' \
      'HelmRepository flux-system/csi-driver-nfs' \
      'HelmRelease kube-system/csi-driver-nfs' \
      'PersistentVolumeClaim openstack/nfs-server-exports' \
      'Deployment openstack/nfs-server' \
      'Service openstack/nfs-server' \
      'DaemonSet openstack/nfs-client-modules' | LC_ALL=C sort)" \
    "$objects"
  assert_eq "the render holds no Namespace" "0" "$(grep -c '^Namespace ' <<<"$objects")"
}

# --- Test 3: the export claim ---
test_claim_is_100gi_without_a_class() {
  echo "Test: the export claim asks for 100Gi and names no storage class"

  render "$NFS_DIR" 4 || return

  assert_eq "the claim names no storage class" "false" \
    "$(val PersistentVolumeClaim nfs-server-exports '.spec | has("storageClassName")')"
  # A patch value that kustomize kept as `storageClassName: null` fails this
  # check too. Unlike assert_no_class of metal_stack_overlay_test.sh it does
  # not match `storageClass:`, a values key of the csi-driver-nfs release.
  assert_eq "no line of the render sets storageClassName" "" \
    "$(printf '%s\n' "$RENDERED" | grep -E '^[[:space:]]*storageClassName:')"
  assert_eq "the claim asks for 100Gi" "100Gi" \
    "$(val PersistentVolumeClaim nfs-server-exports '.spec.resources.requests.storage')"
  assert_eq "the claim stays ReadWriteOnce" "ReadWriteOnce" \
    "$(val PersistentVolumeClaim nfs-server-exports '.spec.accessModes | join(",")')"
}

# --- Test 4: the server ---
test_server_loads_nfsd_before_the_exports() {
  echo "Test: the server loads nfsd in load-nfsd, before prepare-exports, on host-prepare's image"

  render "$NFS_DIR" 12 || return

  assert_eq "the init containers are load-nfsd, then prepare-exports" "load-nfsd prepare-exports" \
    "$(val Deployment nfs-server "$SERVER_POD.initContainers | map(.name) | join(\" \")")"
  assert_same_nonempty "load-nfsd runs on the image of host-prepare in libvirt-daemonset.yaml" \
    "$(val Deployment nfs-server "$LOAD_NFSD | .image")" "$(host_prepare_image)"
  assert_eq "load-nfsd is privileged" "true" \
    "$(val Deployment nfs-server "$LOAD_NFSD | .securityContext.privileged // false")"
  assert_eq "load-nfsd has a read-only root filesystem" "true" \
    "$(val Deployment nfs-server "$LOAD_NFSD | .securityContext.readOnlyRootFilesystem // false")"
  assert_eq "load-nfsd has one volume mount" "1" \
    "$(val Deployment nfs-server "$LOAD_NFSD | .volumeMounts // [] | length")"
  assert_eq "the mount is the volume lib-modules" "lib-modules" \
    "$(val Deployment nfs-server "$LOAD_NFSD | .volumeMounts[0].name")"
  assert_eq "the mount is at /lib/modules" "/lib/modules" \
    "$(val Deployment nfs-server "$LOAD_NFSD | .volumeMounts[0].mountPath")"
  assert_eq "the mount is read-only" "true" \
    "$(val Deployment nfs-server "$LOAD_NFSD | .volumeMounts[0].readOnly // false")"
  assert_eq "the volumes are exports, then lib-modules" "exports lib-modules" \
    "$(val Deployment nfs-server "$SERVER_POD.volumes | map(.name) | join(\" \")")"
  assert_eq "lib-modules is the node's /lib/modules" "/lib/modules" \
    "$(val Deployment nfs-server "$SERVER_POD.volumes[] | select(.name == \"lib-modules\") | .hostPath.path")"

  local kind_image
  kind_image="$(yq -N -r 'select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "nfs-server") | .image' \
    "$KIND_NFS_DIR/nfs-server.yaml")"
  assert_same_nonempty "the server container keeps the image of deploy/kind/nfs/nfs-server.yaml" \
    "$(val Deployment nfs-server "$SERVER_POD.containers[] | select(.name == \"nfs-server\") | .image")" "$kind_image"
  assert_eq "the server exports /exports" "/exports" \
    "$(val Deployment nfs-server "$SERVER_POD.containers[] | select(.name == \"nfs-server\") | .env[] | select(.name == \"SHARED_DIRECTORY\") | .value")"
}

# --- Test 5: the mounter ---
test_mounter_is_the_kind_release() {
  echo "Test: the csi-driver-nfs HelmRelease is the kind release"

  render "$NFS_DIR" 2 || return

  local kind_version
  kind_version="$(yq -N -r 'select(.kind == "HelmRelease") | .spec.chart.spec.version' "$KIND_NFS_DIR/release.yaml")"
  assert_same_nonempty "the chart version equals the one in deploy/kind/nfs/release.yaml" \
    "$(val HelmRelease csi-driver-nfs '.spec.chart.spec.version')" "$kind_version"
  assert_eq "inline volumes stay enabled" "true" \
    "$(val HelmRelease csi-driver-nfs '.spec.values.feature.enableInlineVolume')"
}

# --- Test 6: the DaemonSet ---
test_client_modules_daemonset() {
  echo "Test: DaemonSet nfs-client-modules loads in a privileged init container and holds without a privilege"

  render "$NFS_DIR" 20 || return

  local image
  image="$(host_prepare_image)"

  assert_eq "the pod mounts no ServiceAccount token" "false" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.automountServiceAccountToken")"
  assert_eq "the pod does not share the host network namespace" "false" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.hostNetwork // false")"
  assert_eq "the pod does not share the host PID namespace" "false" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.hostPID // false")"
  # kubectl rollout status, the deploy's gate, does not support OnDelete.
  assert_not_contains "the update strategy is not OnDelete" \
    "$(val DaemonSet nfs-client-modules '.spec.updateStrategy.type // "RollingUpdate"')" "OnDelete"
  assert_eq "the pod has one toleration" "1" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.tolerations // [] | length")"
  assert_eq "the toleration matches every taint" "Exists" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.tolerations[0].operator")"

  assert_eq "the one init container is load" "load" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.initContainers | map(.name) | join(\" \")")"
  assert_same_nonempty "load runs on the image of host-prepare" \
    "$(val DaemonSet nfs-client-modules "$DS_LOAD | .image")" "$image"
  assert_eq "load is privileged" "true" \
    "$(val DaemonSet nfs-client-modules "$DS_LOAD | .securityContext.privileged // false")"
  assert_eq "load has a read-only root filesystem" "true" \
    "$(val DaemonSet nfs-client-modules "$DS_LOAD | .securityContext.readOnlyRootFilesystem // false")"
  assert_eq "load has one volume mount" "1" \
    "$(val DaemonSet nfs-client-modules "$DS_LOAD | .volumeMounts // [] | length")"
  assert_eq "load mounts lib-modules at /lib/modules read-only" "lib-modules /lib/modules true" \
    "$(val DaemonSet nfs-client-modules "$DS_LOAD | .volumeMounts[0] | .name + \" \" + .mountPath + \" \" + ((.readOnly // false) | tostring)")"

  assert_eq "the one container is hold" "hold" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.containers | map(.name) | join(\" \")")"
  assert_same_nonempty "hold runs on the image of host-prepare" \
    "$(val DaemonSet nfs-client-modules "$DS_HOLD | .image")" "$image"
  assert_eq "hold is not privileged" "false" \
    "$(val DaemonSet nfs-client-modules "$DS_HOLD | .securityContext.privileged // false")"
  assert_eq "hold runs as non-root" "true" \
    "$(val DaemonSet nfs-client-modules "$DS_HOLD | .securityContext.runAsNonRoot // false")"
  assert_eq "hold cannot escalate privileges" "false" \
    "$(val DaemonSet nfs-client-modules "$DS_HOLD | .securityContext.allowPrivilegeEscalation")"
  assert_eq "hold drops all capabilities" "ALL" \
    "$(val DaemonSet nfs-client-modules "$DS_HOLD | .securityContext.capabilities.drop // [] | join(\",\")")"

  assert_eq "the pod has one volume" "1" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.volumes // [] | length")"
  assert_eq "the volume is the node's /lib/modules" "lib-modules /lib/modules" \
    "$(val DaemonSet nfs-client-modules "$DS_POD.volumes[0] | .name + \" \" + .hostPath.path")"
}

# write_stubs <dir>
# A modprobe that appends its arguments to $MODPROBE_LOG and fails, as modprobe
# does, for a module named in $MODPROBE_FAIL, and a uname that reports
# STUB_KERNEL.
write_stubs() {
  mkdir -p "$1"
  cat >"$1/modprobe" <<'EOF'
#!/bin/sh
echo "$*" >>"$MODPROBE_LOG"
case " ${MODPROBE_FAIL:-} " in
  *" $1 "*)
    echo "modprobe: FATAL: Module $1 not found in directory /lib/modules/stub" >&2
    exit 1
    ;;
esac
exit 0
EOF
  printf '#!/bin/sh\necho %s\n' "$STUB_KERNEL" >"$1/uname"
  chmod +x "$1/modprobe" "$1/uname"
}

# run_script <script> <stub dir> <log> [failing modules]
# Runs <script> with bash -c, the stubs first on PATH. Prints its stdout;
# returns its exit status.
run_script() {
  : >"$3"
  MODPROBE_LOG="$3" MODPROBE_FAIL="${4:-}" PATH="$2:$PATH" bash -c "$1" 2>/dev/null
}

# --- Test 7: the two load scripts ---
test_load_scripts_report_and_fail_loudly() {
  echo "Test: the load scripts report what they loaded and fail on the first module modprobe cannot load"

  render "$NFS_DIR" 15 || return

  # val keeps the first line of a value, so the scripts are read whole.
  local load load_nfsd
  load="$(printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"DaemonSet\" and .metadata.name == \"nfs-client-modules\") | $DS_LOAD | .args[0] // \"\"" -)"
  load_nfsd="$(printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"Deployment\" and .metadata.name == \"nfs-server\") | $LOAD_NFSD | .args[0] // \"\"" -)"

  # The stubs replace modprobe and uname. A script that calls modprobe by
  # path, or kmod, insmod or rmmod, would reach this machine's kernel past
  # them.
  if grep -qE '/modprobe([^[:alnum:]_]|$)|(^|[^[:alnum:]_])(kmod|insmod|rmmod)([^[:alnum:]_]|$)' <<<"$load"$'\n'"$load_nfsd"; then
    echo "  FAIL: a load script calls modprobe by path, or kmod, insmod or rmmod; not running it on this host (15 checks)"
    FAIL=$((FAIL + 15))
    return
  fi

  local tmp out rc
  tmp="$(mktemp -d)"
  write_stubs "$tmp/bin"

  rc=0
  out="$(run_script "$load" "$tmp/bin" "$tmp/calls")" || rc=$?
  assert_eq "load exits 0 when modprobe loads both modules" "0" "$rc"
  assert_eq "load asks modprobe for nfs, then nfsv4, and nothing else" \
    "$(printf '%s\n' nfs nfsv4)" "$(cat "$tmp/calls")"
  assert_eq "load reports both modules" "nfs-client-modules: nfs and nfsv4 are loaded" "$out"

  rc=0
  out="$(run_script "$load" "$tmp/bin" "$tmp/calls" nfs)" || rc=$?
  assert_eq "load exits 1 when modprobe cannot load nfs" "1" "$rc"
  assert_eq "load stops after nfs and never asks for nfsv4" "nfs" "$(cat "$tmp/calls")"
  assert_eq "load names nfs and the kernel's module directory" \
    "nfs-client-modules: cannot load nfs from /lib/modules/${STUB_KERNEL}" "$out"

  rc=0
  out="$(run_script "$load" "$tmp/bin" "$tmp/calls" nfsv4)" || rc=$?
  assert_eq "load exits 1 when modprobe cannot load nfsv4" "1" "$rc"
  assert_eq "load asked for nfs, then nfsv4" "$(printf '%s\n' nfs nfsv4)" "$(cat "$tmp/calls")"
  assert_eq "load names nfsv4 and reports no success" \
    "nfs-client-modules: cannot load nfsv4 from /lib/modules/${STUB_KERNEL}" "$out"

  rc=0
  out="$(run_script "$load_nfsd" "$tmp/bin" "$tmp/calls")" || rc=$?
  assert_eq "load-nfsd exits 0 when modprobe loads nfsd" "0" "$rc"
  assert_eq "load-nfsd asks modprobe for nfsd alone" "nfsd" "$(cat "$tmp/calls")"
  assert_eq "load-nfsd reports nfsd" "load-nfsd: nfsd is loaded" "$out"

  rc=0
  out="$(run_script "$load_nfsd" "$tmp/bin" "$tmp/calls" nfsd)" || rc=$?
  assert_eq "load-nfsd exits 1 when modprobe cannot load nfsd" "1" "$rc"
  assert_eq "load-nfsd asked for nfsd once" "nfsd" "$(cat "$tmp/calls")"
  assert_eq "load-nfsd names nfsd and the kernel's module directory" \
    "load-nfsd: cannot load nfsd from /lib/modules/${STUB_KERNEL}" "$out"

  rm -rf "$tmp"
}

# --- Test 8: the kind overlay is unchanged ---
test_kind_overlay_unchanged() {
  echo "Test: kustomize build deploy/kind/nfs still renders the kind claim and one init container"

  render "$KIND_NFS_DIR" 4 || return

  assert_eq "the kind overlay renders five documents" "5" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r 'select(. != null) | .kind' - | grep -c .)"
  assert_eq "the kind claim stays on standard" "standard" \
    "$(val PersistentVolumeClaim nfs-server-exports '.spec.storageClassName')"
  assert_eq "the kind claim stays at 5Gi" "5Gi" \
    "$(val PersistentVolumeClaim nfs-server-exports '.spec.resources.requests.storage')"
  assert_eq "the kind server has one init container" "1" \
    "$(val Deployment nfs-server '.spec.template.spec.initContainers | length')"
}

# --- Test 9: the client policy template ---
test_client_policy_template() {
  echo "Test: client-policy.yaml admits the placeholder NODE_NETWORK to the server's port 2049"

  local policy="$NFS_DIR/client-policy.yaml"
  # deploy-infra.sh replaces the placeholder with sed, anchored on this line.
  assert_eq "one line of the template is the placeholder" "1" \
    "$(grep -cE '^[[:space:]]+cidr: NODE_NETWORK$' "$policy")"

  if ! have kustomize || ! have yq; then
    echo "  SKIP: kustomize or yq not installed (7 checks skipped)"
    SKIP=$((SKIP + 7))
    return
  fi
  local server_labels
  server_labels="$(kustomize build "$NFS_DIR" 2>/dev/null |
    yq -N -r 'select(.kind == "Deployment" and .metadata.name == "nfs-server") | .spec.template.metadata.labels | to_entries | map(.key + "=" + .value) | join(",")' -)"

  assert_eq "the template holds one document, a NetworkPolicy" "NetworkPolicy" \
    "$(yq -N -r '.kind' "$policy")"
  assert_eq "named openstack/nfs-server-clients" "openstack/nfs-server-clients" \
    "$(yq -N -r '.metadata.namespace + "/" + .metadata.name' "$policy")"
  assert_same_nonempty "it selects the labels of the server's pod template" \
    "$(yq -N -r '.spec.podSelector.matchLabels | to_entries | map(.key + "=" + .value) | join(",")' "$policy")" \
    "$server_labels"
  assert_eq "it restricts ingress alone" "Ingress" \
    "$(yq -N -r '.spec.policyTypes | join(",")' "$policy")"
  assert_eq "its one rule has one source, the ipBlock of the placeholder" "1 1 NODE_NETWORK" \
    "$(yq -N -r '(.spec.ingress | length | tostring) + " " + (.spec.ingress[0].from | length | tostring) + " " + .spec.ingress[0].from[0].ipBlock.cidr' "$policy")"
  assert_eq "without an except list or a second selector" "ipBlock cidr" \
    "$(yq -N -r '(.spec.ingress[0].from[0] | keys | join(",")) + " " + (.spec.ingress[0].from[0].ipBlock | keys | join(","))' "$policy")"
  assert_eq "and one port, TCP 2049" "TCP/2049" \
    "$(yq -N -r '.spec.ingress[0].ports | map(.protocol + "/" + (.port | tostring)) | join(",")' "$policy")"
}

# --- Run ---
test_files_have_spdx_and_two_resources
test_render_holds_the_six_objects
test_claim_is_100gi_without_a_class
test_server_loads_nfsd_before_the_exports
test_mounter_is_the_kind_release
test_client_modules_daemonset
test_load_scripts_report_and_fail_loudly
test_kind_overlay_unchanged
test_client_policy_template

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
