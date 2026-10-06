#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the dizzy overlay of the metal-stack lab, deploy/lab/metal-stack/dizzy,
# which hack/deploy-infra.sh applies under EXTERNAL_CLUSTER=true WITH_DIZZY=true:
#   1. The directory holds kustomization.yaml alone; it carries the SPDX pair
#      and lists ../../../kind/dizzy as its one resource.
#   2. Without staged dashboards its build fails, as the kind overlay's does.
#   3. With the three dashboards staged the render holds the seven objects of
#      the kind overlay: the Namespace dizzy, the HelmRepositories
#      victoria-metrics and grafana in flux-system, the HelmReleases
#      dizzy-victoria-metrics and dizzy-grafana, the HTTPRoute dizzy-grafana
#      and the ConfigMap grafana-dashboards in dizzy.
#   4. The Namespace carries the Gardener opt-out label and no
#      chaos-mesh.org/inject annotation.
#   5. dizzy-victoria-metrics keeps its metrics on a 10Gi volume without a
#      storageClassName, behind a ClusterIP Service without a nodePort, and
#      keeps the kind render's retentionPeriod, extraArgs and chart range.
#   6. The HelmRelease dizzy-grafana, the HTTPRoute, the ConfigMap and both
#      HelmRepositories are identical to the kind render's.
#   7. deploy/kind/dizzy still renders the NodePort 30428, no volume and a
#      Namespace without labels.
#
# A developer's staged deploy/kind/dizzy/dashboards/ is moved aside before the
# checks and restored on EXIT (tests/lib/dizzy_dashboards.sh); the checks run
# on `{}` placeholders. Checks 2 to 7 are counted as SKIP when kustomize or yq
# is not on PATH; a failing kustomize build counts them as FAIL and prints the
# build's error.
#
# Usage: bash tests/unit/deploy/metal_stack_dizzy_test.sh

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
# shellcheck source=tests/lib/dizzy_dashboards.sh
source "$PROJECT_ROOT/tests/lib/dizzy_dashboards.sh"

LAB_DIZZY_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/dizzy"
KIND_DIZZY_DIR="$PROJECT_ROOT/deploy/kind/dizzy"

RENDERED=""
KIND_RENDERED=""

VM_SERVER='.spec.values.server'

# doc <rendered> <kind> <name> — the whole rendered object of <kind> named <name>.
doc() {
  printf '%s\n' "$1" |
    yq -N "select(.kind == \"$2\" and .metadata.name == \"$3\")" -
}

# render_both <checks> — renders the kind overlay into KIND_RENDERED and the lab
# overlay into RENDERED, with the placeholders staged.
render_both() {
  stage_placeholder_dashboards
  render "$KIND_DIZZY_DIR" "$1" || return 1
  KIND_RENDERED="$RENDERED"
  render "$LAB_DIZZY_DIR" "$1"
}

# --- Test 1: the file ---
test_file_has_spdx_and_one_resource() {
  echo "Test: the overlay is one kustomization with SPDX headers and the kind overlay as its one resource"

  assert_eq "deploy/lab/metal-stack/dizzy holds kustomization.yaml alone" "kustomization.yaml" \
    "$(find "$LAB_DIZZY_DIR" -type f 2>/dev/null | sed 's#.*/##' | sort | paste -sd' ' -)"
  if [[ ! -f "$LAB_DIZZY_DIR/kustomization.yaml" ]]; then
    echo "  FAIL: $LAB_DIZZY_DIR/kustomization.yaml does not exist"
    FAIL=$((FAIL + 3))
    return
  fi
  assert_file_contains "kustomization.yaml has SPDX FileCopyrightText header" \
    "$LAB_DIZZY_DIR/kustomization.yaml" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
  assert_file_contains "kustomization.yaml has SPDX-License-Identifier: Apache-2.0" \
    "$LAB_DIZZY_DIR/kustomization.yaml" "SPDX-License-Identifier: Apache-2.0"
  assert_eq "kustomization.yaml lists ../../../kind/dizzy alone" "../../../kind/dizzy" \
    "$(resource_entries "$LAB_DIZZY_DIR/kustomization.yaml")"
}

# --- Test 2: no dashboards, no render ---
test_build_fails_without_dashboards() {
  echo "Test: without staged dashboards the lab overlay does not build, as the kind overlay does not"

  if ! have kustomize; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  rm -rf "$DASHBOARDS_DIR"
  local rc
  rc=0
  kustomize build "$KIND_DIZZY_DIR" >/dev/null 2>&1 || rc=$?
  assert_nonzero_exit "kustomize build deploy/kind/dizzy fails without dashboards/" "$rc"
  rc=0
  kustomize build "$LAB_DIZZY_DIR" >/dev/null 2>&1 || rc=$?
  assert_nonzero_exit "kustomize build deploy/lab/metal-stack/dizzy fails without dashboards/" "$rc"
}

# --- Test 3: the seven objects ---
test_render_holds_the_seven_objects() {
  echo "Test: kustomize build deploy/lab/metal-stack/dizzy renders the seven objects of the kind overlay"

  render_both 2 || return

  local objects
  objects="$(printf '%s\n' "$RENDERED" |
    yq -N -r 'select(. != null) | .kind + " " + (.metadata.namespace // "") + "/" + .metadata.name' - |
    LC_ALL=C sort)"
  assert_eq "the render holds seven documents" "7" "$(grep -c . <<<"$objects")"
  assert_eq "the render holds the seven objects of the kind overlay" \
    "$(printf '%s\n' \
      'Namespace /dizzy' \
      'HelmRepository flux-system/victoria-metrics' \
      'HelmRepository flux-system/grafana' \
      'HelmRelease dizzy/dizzy-victoria-metrics' \
      'HelmRelease dizzy/dizzy-grafana' \
      'HTTPRoute dizzy/dizzy-grafana' \
      'ConfigMap dizzy/grafana-dashboards' | LC_ALL=C sort)" \
    "$objects"
}

# --- Test 4: the Namespace ---
test_namespace_label_without_the_annotation() {
  echo "Test: the Namespace dizzy carries the Gardener label and is no Chaos Mesh target"

  render_both 2 || return

  assert_eq "the Namespace opts out of Gardener's kubernetes-service-host webhook" "disable" \
    "$(val Namespace dizzy '.metadata.labels["apiserver-proxy.networking.gardener.cloud/inject"]')"
  assert_eq "the Namespace carries no chaos-mesh.org/inject annotation" "false" \
    "$(val Namespace dizzy '.metadata.annotations // {} | has("chaos-mesh.org/inject")')"
}

# --- Test 5: VictoriaMetrics ---
test_victoria_metrics_volume_and_service() {
  echo "Test: dizzy-victoria-metrics keeps its metrics on a volume behind a ClusterIP Service"

  render_both 9 || return

  assert_eq "persistentVolume.enabled is true" "true" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.persistentVolume.enabled")"
  assert_eq "persistentVolume.size is 10Gi" "10Gi" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.persistentVolume.size")"
  # The claim binds to the cluster's default class (D7 of #1219).
  assert_eq "persistentVolume names no storageClassName" "false" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.persistentVolume | has(\"storageClassName\")")"
  assert_eq "service.type is ClusterIP" "ClusterIP" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.service.type")"
  # A null in the patch deletes the key; a kept null would still be a key.
  assert_eq "service has no nodePort key" "false" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.service | has(\"nodePort\")")"

  local expression kind_value
  for expression in "$VM_SERVER.retentionPeriod" "$VM_SERVER.extraArgs" '.spec.chart.spec.version'; do
    kind_value="$(doc "$KIND_RENDERED" HelmRelease dizzy-victoria-metrics | yq -N -o json -I0 "$expression" -)"
    assert_eq "$expression equals the kind render's ($kind_value)" "$kind_value" \
      "$(doc "$RENDERED" HelmRelease dizzy-victoria-metrics | yq -N -o json -I0 "$expression" -)"
  done
  assert_eq "retentionPeriod stays 30d" "30d" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.retentionPeriod")"
}

# --- Test 6: the rest is the kind overlay's ---
test_other_objects_match_kind() {
  echo "Test: the Grafana release, the HTTPRoute, the ConfigMap and both HelmRepositories are the kind overlay's"

  render_both 5 || return

  local object kind name lab kind_doc
  for object in HelmRelease/dizzy-grafana HTTPRoute/dizzy-grafana ConfigMap/grafana-dashboards \
    HelmRepository/victoria-metrics HelmRepository/grafana; do
    kind="${object%%/*}"
    name="${object#*/}"
    lab="$(doc "$RENDERED" "$kind" "$name")"
    kind_doc="$(doc "$KIND_RENDERED" "$kind" "$name")"
    if [[ -n "$lab" && "$lab" == "$kind_doc" ]]; then
      echo "  PASS: $object is identical to the kind render's"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $object differs from the kind render's, or is missing"
      diff <(printf '%s\n' "$kind_doc") <(printf '%s\n' "$lab") | head -20
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 7: the kind overlay is unchanged ---
test_kind_overlay_unchanged() {
  echo "Test: kustomize build deploy/kind/dizzy still publishes NodePort 30428 and keeps an emptyDir"

  stage_placeholder_dashboards
  render "$KIND_DIZZY_DIR" 4 || return

  assert_eq "the kind Service is a NodePort" "NodePort" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.service.type")"
  assert_eq "on node port 30428" "30428" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.service.nodePort")"
  assert_eq "the kind release keeps no volume" "false" \
    "$(val HelmRelease dizzy-victoria-metrics "$VM_SERVER.persistentVolume.enabled")"
  assert_eq "the kind Namespace carries no label" "0" \
    "$(val Namespace dizzy '.metadata.labels // {} | length')"
}

# --- Run ---
guard_setup_dashboards
trap guard_restore_dashboards EXIT

test_file_has_spdx_and_one_resource
test_build_fails_without_dashboards
test_render_holds_the_seven_objects
test_namespace_label_without_the_annotation
test_victoria_metrics_volume_and_service
test_other_objects_match_kind
test_kind_overlay_unchanged

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
