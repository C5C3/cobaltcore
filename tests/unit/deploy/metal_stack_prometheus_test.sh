#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the Prometheus overlay of the metal-stack lab,
# deploy/lab/metal-stack/prometheus, which hack/deploy-infra.sh applies under
# EXTERNAL_CLUSTER=true WITH_PROMETHEUS=true:
#   1. The directory holds kustomization.yaml and hypervisor-operator.json
#      alone; the kustomization carries the SPDX pair and lists
#      ../../../kind/prometheus as its one resource.
#   2. Without the staged deploy/kind/prometheus/keystone-operator.json its
#      build fails, as the kind overlay's does.
#   3. With the Keystone dashboard staged the render holds three objects, the
#      HelmRelease kube-prometheus-stack and the ConfigMaps
#      keystone-operator-dashboard and hypervisor-operator-dashboard in
#      monitoring, and no Namespace.
#   4. The HelmRelease turns off the etcd, scheduler, controller-manager and
#      kube-proxy jobs and sets nothing else in them, and sets retention 7d,
#      retentionSize 8GB, ruleSelectorNilUsesHelmValues false, a memory request
#      of 1Gi, a memory limit of 2Gi and a storageSpec that is a 10Gi
#      ReadWriteOnce volume claim template without a storageClassName and
#      nothing else.
#   5. Without those values the HelmRelease is the kind render's, chart range
#      and dependsOn included, and the Keystone ConfigMap is identical to it.
#   6. The ConfigMap hypervisor-operator-dashboard carries the two labels of
#      the Keystone entry and the one key hypervisor-operator.json, whose
#      value is the file.
#   7. The dashboard has the uid hypervisor-operator, the title Hypervisor
#      Operator, the tags hypervisor and operator, and four timeseries panels
#      with the four expressions and legends of the upstream dashboard's
#      controller-runtime panels, none of which reads kube_customresource_*.
#   8. deploy/kind/prometheus still renders the Namespace monitoring without
#      labels, retention 6h and no storageSpec.
#
# The Keystone dashboard is staged from operators/keystone/dashboards/ when
# deploy/kind/prometheus/keystone-operator.json is absent, and removed on EXIT
# only when this test staged it; a file found at start is moved aside for
# check 2 alone and is byte-identical afterwards. Checks 2 to 6 and 8 are
# counted as SKIP when kustomize or yq is not on PATH, check 7 when jq is not;
# a failing kustomize build counts its checks as FAIL and prints the build's
# error.
#
# Usage: bash tests/unit/deploy/metal_stack_prometheus_test.sh

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

LAB_PROMETHEUS_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/prometheus"
KIND_PROMETHEUS_DIR="$PROJECT_ROOT/deploy/kind/prometheus"
DASHBOARD="$LAB_PROMETHEUS_DIR/hypervisor-operator.json"
KEYSTONE_SOURCE="$PROJECT_ROOT/operators/keystone/dashboards/keystone-operator.json"
KEYSTONE_STAGED="$KIND_PROMETHEUS_DIR/keystone-operator.json"

RENDERED=""
KIND_RENDERED=""

SPEC='.spec.values.prometheus.prometheusSpec'

# A yq filter that deletes the values the lab overlay sets on top of the kind
# release, so what remains can be compared with the kind render. It deletes
# the four job maps and storageSpec whole, so test 4 pins each of them whole.
WITHOUT_LAB_VALUES="del(.spec.values.kubeEtcd, .spec.values.kubeScheduler,
  .spec.values.kubeControllerManager, .spec.values.kubeProxy,
  $SPEC.retention, $SPEC.retentionSize, $SPEC.ruleSelectorNilUsesHelmValues,
  $SPEC.resources.requests.memory, $SPEC.resources.limits.memory, $SPEC.storageSpec)"

# --- The staged Keystone dashboard ---
# STAGED_BY_TEST is true when this test copied the file; ASIDE holds a file
# moved aside during check 2 until it is put back.
STAGED_BY_TEST=false
ASIDE=""

stage_keystone_dashboard() {
  if [[ ! -e "$KEYSTONE_STAGED" ]]; then
    cp "$KEYSTONE_SOURCE" "$KEYSTONE_STAGED"
    STAGED_BY_TEST=true
  fi
}

# Put a file moved aside back, and remove the one this test staged.
restore_keystone_dashboard() {
  if [[ -n "$ASIDE" && -e "$ASIDE/keystone-operator.json" ]]; then
    mv "$ASIDE/keystone-operator.json" "$KEYSTONE_STAGED"
  fi
  [[ -z "$ASIDE" ]] || rmdir "$ASIDE" 2>/dev/null || true
  ASIDE=""
  if [[ "$STAGED_BY_TEST" == "true" ]]; then
    rm -f "$KEYSTONE_STAGED"
  fi
}

# doc <rendered> <kind> <name> — the whole rendered object of <kind> named
# <name>, as one line of JSON.
doc() {
  printf '%s\n' "$1" |
    yq -N -o json -I0 "select(.kind == \"$2\" and .metadata.name == \"$3\")" -
}

# render_both <checks> — renders the kind overlay into KIND_RENDERED and the lab
# overlay into RENDERED.
render_both() {
  render "$KIND_PROMETHEUS_DIR" "$1" || return 1
  KIND_RENDERED="$RENDERED"
  render "$LAB_PROMETHEUS_DIR" "$1"
}

# --- Test 1: the files ---
test_files() {
  echo "Test: the overlay is one kustomization and the dashboard, with SPDX headers and the kind overlay as its one resource"

  assert_eq "deploy/lab/metal-stack/prometheus holds the kustomization and the dashboard alone" \
    "hypervisor-operator.json kustomization.yaml" \
    "$(find "$LAB_PROMETHEUS_DIR" -type f 2>/dev/null | sed 's#.*/##' | LC_ALL=C sort | paste -sd' ' -)"
  if [[ ! -f "$LAB_PROMETHEUS_DIR/kustomization.yaml" ]]; then
    echo "  FAIL: $LAB_PROMETHEUS_DIR/kustomization.yaml does not exist"
    FAIL=$((FAIL + 3))
    return
  fi
  assert_file_contains "kustomization.yaml has SPDX FileCopyrightText header" \
    "$LAB_PROMETHEUS_DIR/kustomization.yaml" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
  assert_file_contains "kustomization.yaml has SPDX-License-Identifier: Apache-2.0" \
    "$LAB_PROMETHEUS_DIR/kustomization.yaml" "SPDX-License-Identifier: Apache-2.0"
  assert_eq "kustomization.yaml lists ../../../kind/prometheus alone" "../../../kind/prometheus" \
    "$(resource_entries "$LAB_PROMETHEUS_DIR/kustomization.yaml")"
}

# --- Test 2: no Keystone dashboard, no render ---
test_build_fails_without_keystone_dashboard() {
  echo "Test: without the staged Keystone dashboard the lab overlay does not build, as the kind overlay does not"

  if ! have kustomize; then
    echo "  SKIP: kustomize not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  ASIDE="$(mktemp -d)"
  mv "$KEYSTONE_STAGED" "$ASIDE/keystone-operator.json"
  local rc
  rc=0
  kustomize build "$KIND_PROMETHEUS_DIR" >/dev/null 2>&1 || rc=$?
  assert_nonzero_exit "kustomize build deploy/kind/prometheus fails without keystone-operator.json" "$rc"
  rc=0
  kustomize build "$LAB_PROMETHEUS_DIR" >/dev/null 2>&1 || rc=$?
  assert_nonzero_exit "kustomize build deploy/lab/metal-stack/prometheus fails without it" "$rc"
  mv "$ASIDE/keystone-operator.json" "$KEYSTONE_STAGED"
  rmdir "$ASIDE"
  ASIDE=""
}

# --- Test 3: the three objects ---
test_render_holds_three_objects() {
  echo "Test: kustomize build deploy/lab/metal-stack/prometheus renders the HelmRelease and the two dashboard ConfigMaps"

  render "$LAB_PROMETHEUS_DIR" 3 || return

  local objects
  objects="$(printf '%s\n' "$RENDERED" |
    yq -N -r 'select(. != null) | .kind + " " + (.metadata.namespace // "") + "/" + .metadata.name' - |
    LC_ALL=C sort)"
  assert_eq "the render holds three documents" "3" "$(grep -c . <<<"$objects")"
  assert_eq "the render holds the HelmRelease and both dashboard ConfigMaps in monitoring" \
    "$(printf '%s\n' \
      'HelmRelease monitoring/kube-prometheus-stack' \
      'ConfigMap monitoring/keystone-operator-dashboard' \
      'ConfigMap monitoring/hypervisor-operator-dashboard' | LC_ALL=C sort)" \
    "$objects"
  # The base overlay declares, labels and annotates monitoring; an apply of
  # this render must not touch it.
  assert_eq "the render holds no Namespace" "0" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r 'select(.kind == "Namespace") | .metadata.name' - | grep -c . || true)"
}

# --- Test 4: the lab values ---
test_lab_values() {
  echo "Test: the HelmRelease turns off the control-plane jobs and keeps 7 days on a 10Gi volume"

  render "$LAB_PROMETHEUS_DIR" 17 || return

  local release=kube-prometheus-stack job
  for job in kubeEtcd kubeScheduler kubeControllerManager kubeProxy; do
    assert_eq "$job.enabled is false" "false" \
      "$(val HelmRelease "$release" ".spec.values.$job.enabled")"
    assert_eq "$job holds enabled alone" "enabled" \
      "$(val HelmRelease "$release" ".spec.values.$job | keys | join(\",\")")"
  done
  assert_eq "retention is 7d" "7d" "$(val HelmRelease "$release" "$SPEC.retention")"
  assert_eq "retentionSize is 8GB" "8GB" "$(val HelmRelease "$release" "$SPEC.retentionSize")"
  # false renders ruleSelector: {}, so the hypervisor operator's
  # PrometheusRules load without a release label.
  assert_eq "ruleSelectorNilUsesHelmValues is false" "false" \
    "$(val HelmRelease "$release" "$SPEC.ruleSelectorNilUsesHelmValues")"
  assert_eq "the memory request is 1Gi" "1Gi" "$(val HelmRelease "$release" "$SPEC.resources.requests.memory")"
  assert_eq "the memory limit is 2Gi" "2Gi" "$(val HelmRelease "$release" "$SPEC.resources.limits.memory")"
  local claim="$SPEC.storageSpec.volumeClaimTemplate.spec"
  assert_eq "the volume claim template requests 10Gi" "10Gi" \
    "$(val HelmRelease "$release" "$claim.resources.requests.storage")"
  assert_eq "with the one access mode ReadWriteOnce" "ReadWriteOnce" \
    "$(val HelmRelease "$release" "$claim.accessModes | join(\",\")")"
  # The claim binds to the cluster's default class (D7 of #1219).
  assert_eq "and no storageClassName" "false" \
    "$(val HelmRelease "$release" "$claim | has(\"storageClassName\")")"
  assert_eq "storageSpec holds that claim template and nothing else" \
    '{"volumeClaimTemplate":{"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"10Gi"}}}}}' \
    "$(val HelmRelease "$release" "$SPEC.storageSpec | to_json(0)")"
}

# --- Test 5: the rest is the kind overlay's ---
test_rest_matches_kind() {
  echo "Test: without the lab values the HelmRelease is the kind render's, and so is the Keystone ConfigMap"

  render_both 4 || return

  local lab kind_doc
  lab="$(doc "$RENDERED" HelmRelease kube-prometheus-stack | yq -N -o json -I0 "$WITHOUT_LAB_VALUES" -)"
  kind_doc="$(doc "$KIND_RENDERED" HelmRelease kube-prometheus-stack | yq -N -o json -I0 "$WITHOUT_LAB_VALUES" -)"
  if [[ -n "$lab" && "$lab" == "$kind_doc" ]]; then
    echo "  PASS: every other value of the HelmRelease is the kind render's"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the HelmRelease differs from the kind render's beyond the lab values, or is missing"
    diff <(yq -P <<<"$kind_doc") <(yq -P <<<"$lab") | head -20
    FAIL=$((FAIL + 1))
  fi
  assert_eq "the chart range is the kind render's (>=65.0.0 <70.0.0)" ">=65.0.0 <70.0.0" \
    "$(val HelmRelease kube-prometheus-stack '.spec.chart.spec.version')"
  assert_eq "dependsOn is the kind render's" \
    "$(doc "$KIND_RENDERED" HelmRelease kube-prometheus-stack | yq -N -o json -I0 '.spec.dependsOn' -)" \
    "$(doc "$RENDERED" HelmRelease kube-prometheus-stack | yq -N -o json -I0 '.spec.dependsOn' -)"

  lab="$(doc "$RENDERED" ConfigMap keystone-operator-dashboard)"
  kind_doc="$(doc "$KIND_RENDERED" ConfigMap keystone-operator-dashboard)"
  if [[ -n "$lab" && "$lab" == "$kind_doc" ]]; then
    echo "  PASS: ConfigMap keystone-operator-dashboard is identical to the kind render's"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: ConfigMap keystone-operator-dashboard differs from the kind render's, or is missing"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 6: the dashboard ConfigMap ---
test_dashboard_configmap() {
  echo "Test: the ConfigMap hypervisor-operator-dashboard carries the Keystone entry's labels and the file"

  render "$LAB_PROMETHEUS_DIR" 4 || return

  local name=hypervisor-operator-dashboard
  assert_eq "it carries the sidecar label grafana_dashboard: \"1\"" "1" \
    "$(val ConfigMap "$name" '.metadata.labels.grafana_dashboard')"
  assert_eq "and app.kubernetes.io/part-of: kube-prometheus-stack" "kube-prometheus-stack" \
    "$(val ConfigMap "$name" '.metadata.labels["app.kubernetes.io/part-of"]')"
  assert_eq "its one key is hypervisor-operator.json" "hypervisor-operator.json" \
    "$(val ConfigMap "$name" '.data | keys | join(" ")')"
  local data
  data="$(printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"ConfigMap\" and .metadata.name == \"$name\") | .data[\"hypervisor-operator.json\"]" -)"
  assert_eq "whose value is deploy/lab/metal-stack/prometheus/hypervisor-operator.json" \
    "$(cat "$DASHBOARD")" "$data"
}

# --- Test 7: the dashboard ---
# shellcheck disable=SC2016 # PromQL and legend templates, not shell expansions
test_dashboard() {
  echo "Test: hypervisor-operator.json holds the four controller-runtime panels"

  if ! have jq; then
    echo "  SKIP: jq not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi
  if ! jq -e . "$DASHBOARD" >/dev/null 2>&1; then
    echo "  FAIL: $DASHBOARD does not parse as JSON (8 checks)"
    FAIL=$((FAIL + 8))
    return
  fi

  assert_eq "the uid is hypervisor-operator" "hypervisor-operator" "$(jq -r '.uid' "$DASHBOARD")"
  assert_eq "the title is Hypervisor Operator" "Hypervisor Operator" "$(jq -r '.title' "$DASHBOARD")"
  assert_eq "the tags are hypervisor and operator" "hypervisor operator" "$(jq -r '.tags | join(" ")' "$DASHBOARD")"
  assert_eq "time and refresh are the Keystone dashboard's" "now-6h 30s" \
    "$(jq -r '.time.from + " " + .refresh' "$DASHBOARD")"
  assert_eq "the one variable is the datasource DS_PROMETHEUS" "DS_PROMETHEUS datasource" \
    "$(jq -r '.templating.list[] | .name + " " + .type' "$DASHBOARD")"
  assert_eq "four timeseries panels, one target each" "timeseries:1 timeseries:1 timeseries:1 timeseries:1" \
    "$(jq -r '[.panels[] | .type + ":" + (.targets | length | tostring)] | join(" ")' "$DASHBOARD")"
  assert_eq "the panels carry the expressions and legends of the upstream dashboard" \
    "$(printf '%s\n' \
      'Reconciliation rate|sum by (controller, result) (rate(controller_runtime_reconcile_total{job=~".*hypervisor-operator.*"}[5m]))|{{controller}} {{result}}' \
      'Reconciliation errors|sum by (controller) (rate(controller_runtime_reconcile_errors_total{job=~".*hypervisor-operator.*"}[5m]))|{{controller}}' \
      'Reconciliation duration (p99)|histogram_quantile(0.99, sum by (controller, le) (rate(controller_runtime_reconcile_time_seconds_bucket{job=~".*hypervisor-operator.*"}[5m])))|{{controller}}' \
      'Workqueue depth|workqueue_depth{job=~".*hypervisor-operator.*"}|{{name}}')" \
    "$(jq -r '.panels[] | .title + "|" + .targets[0].expr + "|" + .targets[0].legendFormat' "$DASHBOARD")"
  # Those metrics come from kube-state-metrics, which the lab does not run.
  assert_eq "no expression reads kube_customresource_*" "0" \
    "$(jq '[.panels[].targets[].expr | select(contains("kube_customresource"))] | length' "$DASHBOARD")"
}

# --- Test 8: the kind overlay is unchanged ---
test_kind_overlay_unchanged() {
  echo "Test: kustomize build deploy/kind/prometheus still renders its Namespace and keeps 6 hours in an emptyDir"

  render "$KIND_PROMETHEUS_DIR" 4 || return

  assert_eq "the kind render holds the Namespace monitoring" "1" "$(count_named Namespace monitoring)"
  assert_eq "without labels" "0" "$(val Namespace monitoring '.metadata.labels // {} | length')"
  assert_eq "the kind release keeps retention 6h" "6h" \
    "$(val HelmRelease kube-prometheus-stack "$SPEC.retention")"
  assert_eq "and no storageSpec" "false" \
    "$(val HelmRelease kube-prometheus-stack "$SPEC | has(\"storageSpec\")")"
}

# --- Run ---
trap restore_keystone_dashboard EXIT
stage_keystone_dashboard

test_files
test_build_fails_without_keystone_dashboard
test_render_holds_three_objects
test_lab_values
test_rest_matches_kind
test_dashboard_configmap
test_dashboard
test_kind_overlay_unchanged

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
