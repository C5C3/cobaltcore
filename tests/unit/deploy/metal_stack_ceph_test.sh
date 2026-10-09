#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the Ceph overlays of the metal-stack lab, which hack/deploy-infra.sh
# applies under EXTERNAL_CLUSTER=true WITH_CEPH=true: deploy/lab/metal-stack/ceph
# in Step 3 and deploy/lab/metal-stack/ceph/cluster in Step 5.
#   1. Every file of ceph/ carries the SPDX header, and its kustomization lists
#      namespace.yaml, source.yaml and release.yaml, no entry naming a parent
#      directory.
#   2. The render of ceph/ holds three objects: the Namespace rook-ceph, the
#      HelmRepository flux-system/rook-release and the HelmRelease
#      rook-ceph/rook-ceph.
#   3. The Namespace enforces the privileged PodSecurity level, opts out of
#      Gardener's apiserver-proxy injection and carries no chaos-mesh.org/inject
#      annotation; the HelmRepository points at https://charts.rook.io/release.
#   4. The HelmRelease installs the chart rook-ceph at an exact version (no
#      >=, < or ^) from rook-release, replaces the CRDs on install and upgrade,
#      retries a failed install or upgrade three times, and turns the CSI
#      operator and its resources, the discovery daemon and the monitoring off.
#
# Checks 2 to 4 are counted as SKIP when kustomize or yq is not on PATH; a
# failing kustomize build counts them as FAIL and prints the build's error.
#
# Usage: bash tests/unit/deploy/metal_stack_ceph_test.sh

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

CEPH_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/ceph"

RENDERED=""

# assert_spdx <file>: the file exists and carries both SPDX lines.
assert_spdx() {
  local f="$1" name="${1#"$PROJECT_ROOT"/}"
  if [[ ! -f "$f" ]]; then
    echo "  FAIL: $name does not exist"
    FAIL=$((FAIL + 2))
    return
  fi
  assert_file_contains "$name has SPDX FileCopyrightText header" \
    "$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
  assert_file_contains "$name has SPDX-License-Identifier: Apache-2.0" \
    "$f" "SPDX-License-Identifier: Apache-2.0"
}

# assert_local_entries <kustomization> <expected entries, one per line>
assert_local_entries() {
  local kust="$1" expected="$2" name="${1#"$PROJECT_ROOT"/}" entries
  entries="$(resource_entries "$kust" 2>/dev/null)"
  assert_eq "$name lists $(tr '\n' ' ' <<<"$expected")alone" "$expected" "$entries"
  assert_eq "no resource entry of $name names a parent directory" "" \
    "$(grep -E '(^|/)\.\.(/|$)' <<<"$entries" || true)"
}

# objects: one "<kind> <namespace>/<name>" per rendered object, sorted.
objects() {
  printf '%s\n' "$RENDERED" |
    yq -N -r 'select(. != null) | .kind + " " + (.metadata.namespace // "") + "/" + .metadata.name' - |
    LC_ALL=C sort
}

# --- Test 1: the files of ceph/ ---
test_operator_files() {
  echo "Test: the files of deploy/lab/metal-stack/ceph carry SPDX headers and the kustomization lists the three local files"

  local f
  for f in kustomization.yaml namespace.yaml source.yaml release.yaml; do
    assert_spdx "$CEPH_DIR/$f"
  done
  assert_local_entries "$CEPH_DIR/kustomization.yaml" \
    "$(printf '%s\n' namespace.yaml source.yaml release.yaml)"
}

# --- Test 2: the three objects of ceph/ ---
test_operator_render_holds_three_objects() {
  echo "Test: kustomize build deploy/lab/metal-stack/ceph renders the Namespace, the HelmRepository and the HelmRelease"

  render "$CEPH_DIR" 1 || return

  assert_eq "the render holds exactly the three objects" \
    "$(printf '%s\n' \
      'HelmRelease rook-ceph/rook-ceph' \
      'HelmRepository flux-system/rook-release' \
      'Namespace /rook-ceph' | LC_ALL=C sort)" \
    "$(objects)"
}

# --- Test 3: the Namespace and the HelmRepository ---
test_operator_namespace_and_source() {
  echo "Test: the Namespace rook-ceph is privileged and opted out of Gardener's proxy, and the source is the Rook repository"

  render "$CEPH_DIR" 5 || return

  assert_eq "the Namespace enforces the privileged PodSecurity level" "privileged" \
    "$(val Namespace rook-ceph '.metadata.labels["pod-security.kubernetes.io/enforce"]')"
  assert_eq "the Namespace opts out of Gardener's apiserver-proxy injection" "disable" \
    "$(val Namespace rook-ceph '.metadata.labels["apiserver-proxy.networking.gardener.cloud/inject"]')"
  assert_eq "the Namespace carries no chaos-mesh.org/inject annotation" "false" \
    "$(val Namespace rook-ceph '(.metadata.annotations // {}) | has("chaos-mesh.org/inject")')"
  assert_eq "the HelmRepository points at the Rook chart repository" "https://charts.rook.io/release" \
    "$(val HelmRepository rook-release '.spec.url')"
  assert_eq "the HelmRepository lives in flux-system" "flux-system" \
    "$(val HelmRepository rook-release '.metadata.namespace')"
}

# --- Test 4: the HelmRelease ---
test_operator_release() {
  echo "Test: the HelmRelease pins rook-ceph, replaces its CRDs and turns CSI, discovery and monitoring off"

  render "$CEPH_DIR" 14 || return

  local version
  version="$(val HelmRelease rook-ceph '.spec.chart.spec.version')"
  assert_eq "the chart is rook-ceph" "rook-ceph" "$(val HelmRelease rook-ceph '.spec.chart.spec.chart')"
  assert_eq "the chart comes from rook-release in flux-system" "HelmRepository flux-system/rook-release" \
    "$(val HelmRelease rook-ceph '.spec.chart.spec.sourceRef | .kind + " " + .namespace + "/" + .name')"
  assert_eq "the chart version is one exact version" "true" \
    "$([[ "$version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]] && echo true || echo false)"
  assert_eq "the chart version is no range" "" "$(grep -E '>=|<|\^' <<<"$version" || true)"
  assert_eq "install replaces the CRDs" "CreateReplace" "$(val HelmRelease rook-ceph '.spec.install.crds')"
  assert_eq "upgrade replaces the CRDs" "CreateReplace" "$(val HelmRelease rook-ceph '.spec.upgrade.crds')"
  assert_eq "a failed install is retried three times" "3" \
    "$(val HelmRelease rook-ceph '.spec.install.remediation.retries')"
  assert_eq "a failed upgrade is retried three times" "3" \
    "$(val HelmRelease rook-ceph '.spec.upgrade.remediation.retries')"
  assert_eq "the CSI operator subchart is off" "false" \
    "$(val HelmRelease rook-ceph '.spec.values.csi.installCsiOperator')"
  assert_eq "the operator writes no CSI operator resources" "false" \
    "$(val HelmRelease rook-ceph '.spec.values.csi.createCsiOperatorResources')"
  assert_eq "the discovery daemon is off" "false" \
    "$(val HelmRelease rook-ceph '.spec.values.enableDiscoveryDaemon')"
  assert_eq "the operator monitoring is off" "false" \
    "$(val HelmRelease rook-ceph '.spec.values.monitoring.enabled')"
  assert_eq "the Ceph pods need no privilege for their hostPath" "false" \
    "$(val HelmRelease rook-ceph '.spec.values.hostpathRequiresPrivileged')"
  assert_eq "the operator keeps the chart's resources" "null" \
    "$(val HelmRelease rook-ceph '.spec.values.resources')"
}

test_operator_files
test_operator_render_holds_three_objects
test_operator_namespace_and_source
test_operator_release

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
