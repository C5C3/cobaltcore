#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the Chaos Mesh overlay of the metal-stack lab,
# deploy/lab/metal-stack/chaos-mesh, which hack/deploy-infra.sh applies under
# EXTERNAL_CLUSTER=true WITH_CHAOS_MESH=true:
#   1. Both files carry SPDX headers, and the kustomization lists exactly
#      ../../../kind/chaos-mesh and modules-daemonset.yaml.
#   2. The render holds four objects: the Namespace chaos-mesh, the
#      HelmRepository and the HelmRelease of the kind overlay, and the
#      DaemonSet chaos-mesh-modules.
#   3. The Namespace carries the Gardener opt-out label and the privileged
#      PodSecurity level, and no chaos-mesh.org/inject annotation, so no
#      experiment selects a pod of Chaos Mesh itself.
#   4. The HelmRelease turns the namespace filter on and keeps the kind values
#      beside it (containerd runtime and socket, no dashboard, the reduced
#      requests, the chart range); no line of the render names
#      allowHostNetworkTesting or clusterScoped.
#   5. chaos-mesh-modules mounts no token, shares no host namespace, rolls out
#      with RollingUpdate, tolerates every taint, loads in a privileged init
#      container and holds the pod in an unprivileged container, both on
#      host-prepare's image (deploy/lab/metal-stack/hypervisor/
#      libvirt-daemonset.yaml) pulled IfNotPresent, with /lib/modules mounted
#      read-only.
#   6. The load script, run with a stub modprobe first on PATH, loads the six
#      modules of the kind mode's host load (hack/deploy-infra.sh) in order
#      and reports them, or names the module that failed and exits 1 without
#      loading the next one.
#   7. deploy/kind/chaos-mesh still renders three documents, with the filter
#      off, no annotation and no Gardener label.
#
# Checks 2 to 7 are counted as SKIP when kustomize or yq is not on PATH; a
# failing kustomize build counts them as FAIL and prints the build's error.
#
# Usage: bash tests/unit/deploy/metal_stack_chaos_mesh_test.sh

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
# shellcheck source=tests/lib/module_loader.sh
source "$PROJECT_ROOT/tests/lib/module_loader.sh"

CHAOS_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/chaos-mesh"
KIND_CHAOS_DIR="$PROJECT_ROOT/deploy/kind/chaos-mesh"
LIBVIRT_DAEMONSET="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml"

# The kernel the stub uname reports, the lab's on 2026-10-04.
STUB_KERNEL="6.1.0-49-amd64"

# The modules the kind mode loads on the host; the lab DaemonSet must load the
# same ones, in the same order.
MODULES="$(sed -n 's/^[[:space:]]*load_host_kernel_modules "chaos-mesh NetworkChaos" //p' \
  "$PROJECT_ROOT/hack/deploy-infra.sh")"

RENDERED=""

# The pod spec of the DaemonSet, as yq paths.
DS_POD='.spec.template.spec'
DS_LOAD="$DS_POD.initContainers[] | select(.name == \"load\")"
DS_HOLD="$DS_POD.containers[] | select(.name == \"hold\")"

# --- Test 1: the files ---
test_files_have_spdx_and_two_resources() {
  echo "Test: the overlay files carry SPDX headers and the kustomization lists the kind overlay and the DaemonSet"

  local f
  for f in kustomization.yaml modules-daemonset.yaml; do
    if [[ ! -f "$CHAOS_DIR/$f" ]]; then
      echo "  FAIL: $CHAOS_DIR/$f does not exist"
      FAIL=$((FAIL + 2))
      continue
    fi
    assert_file_contains "$f has SPDX FileCopyrightText header" \
      "$CHAOS_DIR/$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$f has SPDX-License-Identifier: Apache-2.0" \
      "$CHAOS_DIR/$f" "SPDX-License-Identifier: Apache-2.0"
  done
  assert_eq "kustomization.yaml lists ../../../kind/chaos-mesh and modules-daemonset.yaml alone" \
    "$(printf '%s\n' ../../../kind/chaos-mesh modules-daemonset.yaml)" \
    "$(resource_entries "$CHAOS_DIR/kustomization.yaml" 2>/dev/null)"
}

# --- Test 2: the four objects ---
test_render_holds_the_four_objects() {
  echo "Test: kustomize build deploy/lab/metal-stack/chaos-mesh renders the four objects"

  render "$CHAOS_DIR" 2 || return

  local objects
  objects="$(printf '%s\n' "$RENDERED" |
    yq -N -r 'select(. != null) | .kind + " " + (.metadata.namespace // "") + "/" + .metadata.name' - |
    LC_ALL=C sort)"
  assert_eq "the render holds four documents" "4" "$(grep -c . <<<"$objects")"
  assert_eq "the render holds the three objects of the kind overlay and the DaemonSet" \
    "$(printf '%s\n' \
      'Namespace /chaos-mesh' \
      'HelmRepository flux-system/chaos-mesh' \
      'HelmRelease chaos-mesh/chaos-mesh' \
      'DaemonSet chaos-mesh/chaos-mesh-modules' | LC_ALL=C sort)" \
    "$objects"
}

# --- Test 3: the Namespace ---
test_namespace_labels_without_the_annotation() {
  echo "Test: the Namespace chaos-mesh carries both labels and is not selectable itself"

  render "$CHAOS_DIR" 3 || return

  assert_eq "the Namespace opts out of Gardener's kubernetes-service-host webhook" "disable" \
    "$(val Namespace chaos-mesh '.metadata.labels["apiserver-proxy.networking.gardener.cloud/inject"]')"
  assert_eq "the Namespace keeps the privileged PodSecurity level" "privileged" \
    "$(val Namespace chaos-mesh '.metadata.labels["pod-security.kubernetes.io/enforce"]')"
  assert_eq "the Namespace carries no chaos-mesh.org/inject annotation" "false" \
    "$(val Namespace chaos-mesh '.metadata.annotations // {} | has("chaos-mesh.org/inject")')"
}

# --- Test 4: the HelmRelease ---
test_release_filters_namespaces_and_keeps_the_kind_values() {
  echo "Test: the HelmRelease turns the namespace filter on and keeps the kind values"

  render "$CHAOS_DIR" 8 || return

  assert_eq "controllerManager.enableFilterNamespace is true" "true" \
    "$(val HelmRelease chaos-mesh '.spec.values.controllerManager.enableFilterNamespace')"
  # The patch merges into the kind values; a patch that replaced
  # controllerManager would drop the kind requests.
  assert_eq "controllerManager keeps the kind CPU request" "25m" \
    "$(val HelmRelease chaos-mesh '.spec.values.controllerManager.resources.requests.cpu')"
  assert_eq "chaosDaemon.runtime stays containerd" "containerd" \
    "$(val HelmRelease chaos-mesh '.spec.values.chaosDaemon.runtime')"
  # The path the survey of #1220 found on both workers.
  assert_eq "chaosDaemon.socketPath stays /run/containerd/containerd.sock" "/run/containerd/containerd.sock" \
    "$(val HelmRelease chaos-mesh '.spec.values.chaosDaemon.socketPath')"
  assert_eq "dashboard.create stays false" "false" \
    "$(val HelmRelease chaos-mesh '.spec.values.dashboard.create')"
  assert_eq "the chart range stays >=2.6.0 <3.0.0" ">=2.6.0 <3.0.0" \
    "$(val HelmRelease chaos-mesh '.spec.chart.spec.version')"
  # allowHostNetworkTesting lets NetworkChaos select a hostNetwork pod;
  # clusterScoped=false limits experiments to one namespace (D2 of #1219).
  assert_eq "no line of the render names allowHostNetworkTesting" "0" \
    "$(grep -c 'allowHostNetworkTesting' <<<"$RENDERED")"
  assert_eq "no line of the render names clusterScoped" "0" \
    "$(grep -c 'clusterScoped' <<<"$RENDERED")"
}

# --- Test 5: the DaemonSet ---
test_modules_daemonset() {
  echo "Test: DaemonSet chaos-mesh-modules loads in a privileged init container and holds without a privilege"

  render "$CHAOS_DIR" 23 || return

  local image
  image="$(host_prepare_image)"

  assert_eq "the pod mounts no ServiceAccount token" "false" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.automountServiceAccountToken")"
  assert_eq "the pod does not share the host network namespace" "false" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.hostNetwork // false")"
  assert_eq "the pod does not share the host PID namespace" "false" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.hostPID // false")"
  assert_eq "the pod does not share the host IPC namespace" "false" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.hostIPC // false")"
  # kubectl rollout status, the deploy's gate, does not support OnDelete.
  assert_eq "the update strategy is RollingUpdate" "RollingUpdate" \
    "$(val DaemonSet chaos-mesh-modules '.spec.updateStrategy.type // "RollingUpdate"')"
  assert_eq "the pod has one toleration" "1" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.tolerations // [] | length")"
  assert_eq "the toleration matches every taint" "Exists" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.tolerations[0].operator")"

  assert_eq "the one init container is load" "load" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.initContainers | map(.name) | join(\" \")")"
  assert_same_nonempty "load runs on the image of host-prepare" \
    "$(val DaemonSet chaos-mesh-modules "$DS_LOAD | .image")" "$image"
  assert_eq "load pulls the pinned image IfNotPresent" "IfNotPresent" \
    "$(val DaemonSet chaos-mesh-modules "$DS_LOAD | .imagePullPolicy")"
  assert_eq "load is privileged" "true" \
    "$(val DaemonSet chaos-mesh-modules "$DS_LOAD | .securityContext.privileged // false")"
  assert_eq "load has a read-only root filesystem" "true" \
    "$(val DaemonSet chaos-mesh-modules "$DS_LOAD | .securityContext.readOnlyRootFilesystem // false")"
  assert_eq "load has one volume mount" "1" \
    "$(val DaemonSet chaos-mesh-modules "$DS_LOAD | .volumeMounts // [] | length")"
  assert_eq "load mounts lib-modules at /lib/modules read-only" "lib-modules /lib/modules true" \
    "$(val DaemonSet chaos-mesh-modules "$DS_LOAD | .volumeMounts[0] | .name + \" \" + .mountPath + \" \" + ((.readOnly // false) | tostring)")"

  assert_eq "the one container is hold" "hold" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.containers | map(.name) | join(\" \")")"
  assert_same_nonempty "hold runs on the image of host-prepare" \
    "$(val DaemonSet chaos-mesh-modules "$DS_HOLD | .image")" "$image"
  assert_eq "hold pulls the pinned image IfNotPresent" "IfNotPresent" \
    "$(val DaemonSet chaos-mesh-modules "$DS_HOLD | .imagePullPolicy")"
  assert_eq "hold is not privileged" "false" \
    "$(val DaemonSet chaos-mesh-modules "$DS_HOLD | .securityContext.privileged // false")"
  assert_eq "hold runs as non-root" "true" \
    "$(val DaemonSet chaos-mesh-modules "$DS_HOLD | .securityContext.runAsNonRoot // false")"
  assert_eq "hold cannot escalate privileges" "false" \
    "$(val DaemonSet chaos-mesh-modules "$DS_HOLD | .securityContext.allowPrivilegeEscalation")"
  assert_eq "hold drops all capabilities" "ALL" \
    "$(val DaemonSet chaos-mesh-modules "$DS_HOLD | .securityContext.capabilities.drop // [] | join(\",\")")"

  assert_eq "the pod has one volume" "1" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.volumes // [] | length")"
  assert_eq "the volume is the node's /lib/modules" "lib-modules /lib/modules" \
    "$(val DaemonSet chaos-mesh-modules "$DS_POD.volumes[0] | .name + \" \" + .hostPath.path")"
}

# --- Test 6: the load script ---
test_load_script_reports_and_fails_loudly() {
  echo "Test: the load script loads the six modules in order and fails on the first one modprobe cannot load"

  render "$CHAOS_DIR" 8 || return

  # val keeps the first line of a value, so the script is read whole.
  local load
  load="$(printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"DaemonSet\" and .metadata.name == \"chaos-mesh-modules\") | $DS_LOAD | .args[0] // \"\"" -)"

  if [[ -z "$MODULES" ]]; then
    echo "  FAIL: hack/deploy-infra.sh has no load_host_kernel_modules \"chaos-mesh NetworkChaos\" line to compare the load script with (8 checks)"
    FAIL=$((FAIL + 8))
    return
  fi

  # The stubs replace modprobe and uname. A script that calls modprobe by
  # path, or kmod, insmod or rmmod, would reach this machine's kernel past
  # them.
  if [[ -z "$load" ]] || unsafe_load_script "$load"; then
    echo "  FAIL: the load script is empty, or calls modprobe by path, or kmod, insmod or rmmod; not running it on this host (8 checks)"
    FAIL=$((FAIL + 8))
    return
  fi

  local tmp out rc
  tmp="$(mktemp -d)"
  write_stubs "$tmp/bin"

  rc=0
  out="$(run_script "$load" "$tmp/bin" "$tmp/calls")" || rc=$?
  assert_eq "load exits 0 when modprobe loads all six modules" "0" "$rc"
  assert_eq "load asks modprobe for the six modules in order, and nothing else" \
    "$(tr ' ' '\n' <<<"$MODULES")" "$(cat "$tmp/calls")"
  assert_eq "load reports the six modules" \
    "chaos-mesh-modules: ip_set, ip_set_hash_ip, ip_set_hash_net, xt_set, sch_netem and sch_tbf are loaded" "$out"

  rc=0
  out="$(run_script "$load" "$tmp/bin" "$tmp/calls" sch_netem)" || rc=$?
  assert_eq "load exits 1 when modprobe cannot load sch_netem" "1" "$rc"
  assert_eq "load stops after sch_netem and never asks for sch_tbf" \
    "$(printf '%s\n' ip_set ip_set_hash_ip ip_set_hash_net xt_set sch_netem)" "$(cat "$tmp/calls")"
  assert_eq "load names sch_netem and the kernel's module directory, and reports no success" \
    "chaos-mesh-modules: cannot load sch_netem from /lib/modules/${STUB_KERNEL}" "$out"

  rc=0
  out="$(run_script "$load" "$tmp/bin" "$tmp/calls" ip_set)" || rc=$?
  assert_eq "load exits 1 when modprobe cannot load the first module, ip_set" "1" "$rc"
  assert_eq "load asked for ip_set alone" "ip_set" "$(cat "$tmp/calls")"

  rm -rf "$tmp"
}

# --- Test 7: the kind overlay is unchanged ---
test_kind_overlay_unchanged() {
  echo "Test: kustomize build deploy/kind/chaos-mesh still renders the kind bundle without the lab's changes"

  render "$KIND_CHAOS_DIR" 4 || return

  assert_eq "the kind overlay renders three documents" "3" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r 'select(. != null) | .kind' - | grep -c .)"
  assert_eq "the kind release leaves the namespace filter off" "0" \
    "$(grep -c 'enableFilterNamespace' <<<"$RENDERED")"
  assert_eq "no kind Namespace carries chaos-mesh.org/inject" "0" \
    "$(grep -c 'chaos-mesh.org/inject' <<<"$RENDERED")"
  assert_eq "the kind Namespace carries no Gardener label" "0" \
    "$(grep -c 'apiserver-proxy.networking.gardener.cloud' <<<"$RENDERED")"
}

# --- Run ---
test_files_have_spdx_and_two_resources
test_render_holds_the_four_objects
test_namespace_labels_without_the_annotation
test_release_filters_namespaces_and_keeps_the_kind_values
test_modules_daemonset
test_load_script_reports_and_fails_loudly
test_kind_overlay_unchanged

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
