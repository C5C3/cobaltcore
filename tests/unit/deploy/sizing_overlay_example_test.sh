#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the example sizing and placement overlay under
# deploy/examples/sizing-overlay/, which the reference docs code-import and no
# script applies:
#
#   - kustomize build succeeds for the overlay and for its infrastructure/
#     phase. A failed build fails the test and prints kustomize's stderr.
#   - every value the overlay patches lands on the rendered object: the
#     keystone-operator chart version floor and the keystone-operator and
#     cert-manager HelmRelease values, the JSON6902 patch the k-orc and
#     rabbitmq-cluster-operator Flux Kustomizations and the FluxInstance
#     carry, and the MariaDB and Memcached CR fields.
#   - the base phase renders the PriorityClass both phases' patches
#     reference, and the infrastructure phase does not, so deleting that
#     phase leaves the class in place.
#   - the keystone-operator chart version range keeps the base's upper
#     bound, which the patch would otherwise replace.
#   - each Flux Kustomization and the FluxInstance carries exactly one patch
#     entry with the stated target, and only add operations.
#   - the fields the patches must not touch survive: the keystone-operator
#     image tag, leader election and digest valuesFrom, cert-manager's CRD
#     install, both Flux Kustomizations' image digests, the RabbitMQ
#     cluster operator's upstream resources, the rest of the FluxInstance
#     spec, and the MariaDB replicas and Galera block.
#   - the in-repo keystone-operator chart renders the example's
#     keystone-operator values into its Deployment, so the example cannot go
#     stale against the chart's values schema. The chart's operator-library
#     dependency is rebuilt first. Skipped without helm.
#   - kustomize build deploy/flux-system renders no PriorityClass, no
#     spec.patches on either Flux Kustomization and no spec.kustomize on the
#     FluxInstance: the example leaves the production base unchanged.
#
# SIZING_OVERLAY_DIR points the test at another copy of the overlay, for
# example a deliberately broken one to see the failure path.
#
# Usage: bash tests/unit/deploy/sizing_overlay_example_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

OVERLAY_DIR="${SIZING_OVERLAY_DIR:-$PROJECT_ROOT/deploy/examples/sizing-overlay}"
INFRA_OVERLAY_DIR="$OVERLAY_DIR/infrastructure"
BASE_DIR="$PROJECT_ROOT/deploy/flux-system"
INFRA_BASE_DIR="$BASE_DIR/infrastructure"

# The example's placement values as compact JSON with sorted keys, the form
# cjson and op_value print.
PRIORITY_CLASS="cobaltcore-platform"
NODE_SELECTOR='{"node.c5c3.io/role":"platform"}'
TOLERATIONS='[{"effect":"NoSchedule","key":"node.c5c3.io/role","operator":"Equal","value":"platform"}]'

results() {
  echo ""
  echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
}

# build <var> <dir>: store kustomize build <dir> in <var>. On failure count a
# FAIL, print kustomize's stderr and return 1.
build() {
  local out err_file
  err_file="$(mktemp)"
  if out="$(kustomize build "$2" 2>"$err_file")"; then
    printf -v "$1" '%s' "$out"
    echo "  PASS: kustomize build $2"
    PASS=$((PASS + 1))
    rm -f "$err_file"
    return 0
  fi
  echo "  FAIL: kustomize build $2 failed:"
  sed 's/^/    /' "$err_file"
  FAIL=$((FAIL + 1))
  rm -f "$err_file"
  return 1
}

# obj <yaml> <kind> <name> <namespace>: the rendered object with that kind,
# name and namespace ("" for a cluster-scoped object).
obj() {
  printf '%s\n' "$1" | K="$2" N="$3" NS="$4" yq \
    'select(.kind == strenv(K) and .metadata.name == strenv(N) and (.metadata.namespace // "") == strenv(NS))'
}

# count <yaml> <filter>: how many rendered objects the yq filter selects.
count() {
  printf '%s\n' "$1" | yq eval-all "[$2] | length"
}

# val <yaml> <path>: the value at path as raw text, "null" when absent.
val() {
  printf '%s\n' "$1" | yq -r "$2"
}

# cjson <yaml> <path>: the value at path as compact JSON with sorted keys.
cjson() {
  printf '%s\n' "$1" | yq -o=json -I=0 "$2 | sort_keys(..)"
}

# op_value <json6902> <pointer>: the value the patch adds at pointer, as
# compact JSON with sorted keys; empty when no operation targets it.
op_value() {
  printf '%s\n' "$1" | P="$2" yq -o=json -I=0 \
    '.[] | select(.path == strenv(P)) | .value | sort_keys(..)'
}

# --- Test 1: only the base phase adds the PriorityClass ---
test_object_counts() {
  echo "Test: the base phase adds exactly the PriorityClass, the infrastructure phase nothing"

  if [ "$OVERLAY_OK" != true ] || [ "$INFRA_OVERLAY_OK" != true ]; then
    echo "  SKIP: an overlay did not build (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  assert_eq "the overlay renders one object more than deploy/flux-system" \
    "$(($(count "$BASE" 'select(.kind != null)') + 1))" \
    "$(count "$OVERLAY" 'select(.kind != null)')"
  assert_eq "the overlay renders exactly one PriorityClass" \
    "1" "$(count "$OVERLAY" 'select(.kind == "PriorityClass")')"
  # The infrastructure phase takes the class from the base phase, as it takes
  # its namespace and CRDs. Were both phases to apply it, kubectl delete -k on
  # either would delete it from under the other's pods.
  assert_eq "the infrastructure overlay renders as many objects as its base" \
    "$(count "$INFRA_BASE" 'select(.kind != null)')" \
    "$(count "$INFRA_OVERLAY" 'select(.kind != null)')"
  assert_eq "the infrastructure overlay renders no PriorityClass" \
    "0" "$(count "$INFRA_OVERLAY" 'select(.kind == "PriorityClass")')"
}

# --- Test 2: the PriorityClass ---
test_priority_class() {
  echo "Test: PriorityClass/$PRIORITY_CLASS"

  if [ "$OVERLAY_OK" != true ]; then
    echo "  SKIP: the overlay did not build (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local pc
  pc="$(obj "$OVERLAY" PriorityClass "$PRIORITY_CLASS" "")"
  assert_eq "PriorityClass value" "1000000" "$(val "$pc" '.value')"
  assert_eq "PriorityClass is not the global default" "false" "$(val "$pc" '.globalDefault')"
}

# --- Test 3: a CobaltCore operator HelmRelease ---
test_keystone_release() {
  echo "Test: HelmRelease keystone-system/keystone-operator spec.values"

  if [ "$OVERLAY_OK" != true ]; then
    echo "  SKIP: the overlay did not build (16 checks skipped)"
    SKIP=$((SKIP + 16))
    return
  fi

  local hr base_hr
  hr="$(obj "$OVERLAY" HelmRelease keystone-operator keystone-system)"
  base_hr="$(obj "$BASE" HelmRelease keystone-operator keystone-system)"

  # Charts before 0.11.0 reject the placement keys through their values
  # schema, and the base's floor of 0.8.0 still admits such a chart.
  assert_eq "the chart version floor admits the placement keys" \
    ">=0.11.0 <1.0.0" "$(val "$hr" '.spec.chart.spec.version')"
  # The patch replaces the base's whole range. Once the base moves its upper
  # bound, a stale one here holds the release on an older chart, and Flux
  # downgrades a release that already runs a newer one.
  assert_eq "the chart version range keeps the base's upper bound" \
    "$(val "$base_hr" '.spec.chart.spec.version' | sed 's/^[^ ]* //')" \
    "$(val "$hr" '.spec.chart.spec.version' | sed 's/^[^ ]* //')"
  assert_eq "the rest of spec.chart equals the base" \
    "$(cjson "$base_hr" '.spec.chart | del(.spec.version)')" \
    "$(cjson "$hr" '.spec.chart | del(.spec.version)')"
  assert_eq "replicas" "1" "$(val "$hr" '.spec.values.replicas')"
  assert_eq "resources.requests.cpu" "20m" "$(val "$hr" '.spec.values.resources.requests.cpu')"
  assert_eq "resources.requests.memory" "96Mi" "$(val "$hr" '.spec.values.resources.requests.memory')"
  assert_eq "resources.limits.cpu" "500m" "$(val "$hr" '.spec.values.resources.limits.cpu')"
  assert_eq "resources.limits.memory" "192Mi" "$(val "$hr" '.spec.values.resources.limits.memory')"
  assert_eq "nodeSelector" "$NODE_SELECTOR" "$(cjson "$hr" '.spec.values.nodeSelector')"
  assert_eq "tolerations" "$TOLERATIONS" "$(cjson "$hr" '.spec.values.tolerations')"
  assert_eq "priorityClassName" "$PRIORITY_CLASS" "$(val "$hr" '.spec.values.priorityClassName')"

  assert_eq "image.tag is kept" "latest" "$(val "$hr" '.spec.values.image.tag')"
  assert_eq "leaderElection.enabled is kept" "true" "$(val "$hr" '.spec.values.leaderElection.enabled')"
  assert_eq "the valuesFrom entry names the digest ConfigMap" \
    "keystone-operator-image-digest" "$(val "$hr" '.spec.valuesFrom[0].name')"
  assert_eq "the valuesFrom entry stays optional" "true" "$(val "$hr" '.spec.valuesFrom[0].optional')"
  assert_eq "valuesFrom equals the base" \
    "$(cjson "$base_hr" '.spec.valuesFrom')" "$(cjson "$hr" '.spec.valuesFrom')"
}

# --- Test 4: a third-party HelmRelease ---
test_cert_manager_release() {
  echo "Test: HelmRelease cert-manager/cert-manager spec.values"

  if [ "$OVERLAY_OK" != true ]; then
    echo "  SKIP: the overlay did not build (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local hr
  hr="$(obj "$OVERLAY" HelmRelease cert-manager cert-manager)"

  assert_eq "resources.requests.cpu" "10m" "$(val "$hr" '.spec.values.resources.requests.cpu')"
  assert_eq "resources.requests.memory" "64Mi" "$(val "$hr" '.spec.values.resources.requests.memory')"
  assert_eq "nodeSelector" "$NODE_SELECTOR" "$(cjson "$hr" '.spec.values.nodeSelector')"
  assert_eq "tolerations" "$TOLERATIONS" "$(cjson "$hr" '.spec.values.tolerations')"
  assert_eq "global.priorityClassName" "$PRIORITY_CLASS" "$(val "$hr" '.spec.values.global.priorityClassName')"
  assert_eq "crds.enabled is kept" "true" "$(val "$hr" '.spec.values.crds.enabled')"
}

# --- Test 5: the two Git-sourced Flux Kustomizations ---
test_flux_kustomization_patches() {
  echo "Test: Flux Kustomizations k-orc and rabbitmq-cluster-operator spec.patches"

  if [ "$OVERLAY_OK" != true ]; then
    echo "  SKIP: the overlay did not build (20 checks skipped)"
    SKIP=$((SKIP + 20))
    return
  fi

  local name ks base_ks patch
  for name in k-orc rabbitmq-cluster-operator; do
    ks="$(obj "$OVERLAY" Kustomization "$name" flux-system)"
    base_ks="$(obj "$BASE" Kustomization "$name" flux-system)"
    patch="$(val "$ks" '.spec.patches[0].patch')"

    assert_eq "$name: exactly one patch entry" "1" "$(val "$ks" '.spec.patches | length')"
    assert_eq "$name: the entry targets every Deployment" \
      '{"kind":"Deployment"}' "$(cjson "$ks" '.spec.patches[0].target')"
    assert_eq "$name: every operation is an add" \
      "0" "$(printf '%s\n' "$patch" | yq '[.[] | select(.op != "add")] | length')"
    assert_eq "$name: nodeSelector" \
      "$NODE_SELECTOR" "$(op_value "$patch" /spec/template/spec/nodeSelector)"
    assert_eq "$name: tolerations" \
      "$TOLERATIONS" "$(op_value "$patch" /spec/template/spec/tolerations)"
    assert_eq "$name: priorityClassName" \
      "\"$PRIORITY_CLASS\"" "$(op_value "$patch" /spec/template/spec/priorityClassName)"
    case "$name" in
      k-orc)
        assert_eq "$name: the JSON6902 patch has four operations" \
          "4" "$(printf '%s\n' "$patch" | yq '. | length')"
        assert_eq "$name: manager container resources" \
          '{"limits":{"memory":"128Mi"},"requests":{"cpu":"10m","memory":"64Mi"}}' \
          "$(op_value "$patch" /spec/template/spec/containers/0/resources)"
        ;;
      rabbitmq-cluster-operator)
        # The installer sizes the manager at 200m / 500Mi; an add on
        # resources would replace that block whole.
        assert_eq "$name: the JSON6902 patch has three operations" \
          "3" "$(printf '%s\n' "$patch" | yq '. | length')"
        assert_eq "$name: the patch keeps the upstream resources" \
          "" "$(op_value "$patch" /spec/template/spec/containers/0/resources)"
        ;;
    esac
    assert_starts_with "$name: the base pins the image by digest" \
      "$(val "$base_ks" '.spec.images[0].digest')" "sha256:"
    assert_eq "$name: spec.images equals the base" \
      "$(cjson "$base_ks" '.spec.images')" "$(cjson "$ks" '.spec.images')"
  done
}

# --- Test 6: the FluxInstance ---
test_fluxinstance_patch() {
  echo "Test: FluxInstance flux-system/flux spec.kustomize.patches"

  if [ "$OVERLAY_OK" != true ]; then
    echo "  SKIP: the overlay did not build (9 checks skipped)"
    SKIP=$((SKIP + 9))
    return
  fi

  local flux_instance base_fi patch
  flux_instance="$(obj "$OVERLAY" FluxInstance flux flux-system)"
  base_fi="$(obj "$BASE" FluxInstance flux flux-system)"
  patch="$(val "$flux_instance" '.spec.kustomize.patches[0].patch')"

  assert_eq "exactly one patch entry" "1" "$(val "$flux_instance" '.spec.kustomize.patches | length')"
  assert_eq "the entry targets the helm-controller Deployment" \
    '{"kind":"Deployment","name":"helm-controller"}' "$(cjson "$flux_instance" '.spec.kustomize.patches[0].target')"
  assert_eq "the JSON6902 patch has three operations" \
    "3" "$(printf '%s\n' "$patch" | yq '. | length')"
  assert_eq "every operation is an add" \
    "0" "$(printf '%s\n' "$patch" | yq '[.[] | select(.op != "add")] | length')"
  assert_eq "nodeSelector" "$NODE_SELECTOR" "$(op_value "$patch" /spec/template/spec/nodeSelector)"
  assert_eq "tolerations" "$TOLERATIONS" "$(op_value "$patch" /spec/template/spec/tolerations)"
  assert_eq "manager container resources" \
    '{"limits":{"memory":"512Mi"},"requests":{"cpu":"50m","memory":"128Mi"}}' \
    "$(op_value "$patch" /spec/template/spec/containers/0/resources)"
  # The helm-controller already runs as system-cluster-critical, which ranks
  # above the example class; replacing it would lower its priority.
  assert_eq "the patch keeps the controller's own priorityClassName" \
    "" "$(op_value "$patch" /spec/template/spec/priorityClassName)"
  assert_eq "the rest of the FluxInstance spec equals the base" \
    "$(cjson "$base_fi" '.spec')" "$(cjson "$flux_instance" '.spec | del(.kustomize)')"
}

# --- Test 7: the infrastructure CRs ---
test_infrastructure_patches() {
  echo "Test: MariaDB openstack/openstack-db and Memcached openstack/openstack-memcached"

  if [ "$INFRA_OVERLAY_OK" != true ]; then
    echo "  SKIP: the infrastructure overlay did not build (13 checks skipped)"
    SKIP=$((SKIP + 13))
    return
  fi

  local db base_db mc
  db="$(obj "$INFRA_OVERLAY" MariaDB openstack-db openstack)"
  base_db="$(obj "$INFRA_BASE" MariaDB openstack-db openstack)"
  mc="$(obj "$INFRA_OVERLAY" Memcached openstack-memcached openstack)"

  assert_eq "MariaDB resources.requests.cpu" "500m" "$(val "$db" '.spec.resources.requests.cpu')"
  assert_eq "MariaDB resources.requests.memory" "2Gi" "$(val "$db" '.spec.resources.requests.memory')"
  assert_eq "MariaDB resources.limits.memory" "2Gi" "$(val "$db" '.spec.resources.limits.memory')"
  assert_eq "MariaDB nodeSelector" "$NODE_SELECTOR" "$(cjson "$db" '.spec.nodeSelector')"
  assert_eq "MariaDB tolerations" "$TOLERATIONS" "$(cjson "$db" '.spec.tolerations')"
  assert_eq "MariaDB priorityClassName" "$PRIORITY_CLASS" "$(val "$db" '.spec.priorityClassName')"
  assert_eq "MariaDB replicas are kept" "3" "$(val "$db" '.spec.replicas')"
  assert_eq "MariaDB galera.enabled is kept" "true" "$(val "$db" '.spec.galera.enabled')"
  assert_eq "MariaDB galera equals the base" \
    "$(cjson "$base_db" '.spec.galera')" "$(cjson "$db" '.spec.galera')"

  assert_eq "Memcached resources.requests.cpu" "100m" "$(val "$mc" '.spec.resources.requests.cpu')"
  assert_eq "Memcached resources.requests.memory" "128Mi" "$(val "$mc" '.spec.resources.requests.memory')"
  assert_eq "Memcached resources.limits.memory" "128Mi" "$(val "$mc" '.spec.resources.limits.memory')"
  assert_eq "Memcached gets no nodeSelector (the CRD has none)" "null" "$(val "$mc" '.spec.nodeSelector')"
}

# --- Test 8: the production base stays unpatched ---
test_production_base_unchanged() {
  echo "Test: kustomize build deploy/flux-system carries none of the example's patches"

  if [ "$BASE_OK" != true ]; then
    echo "  SKIP: deploy/flux-system did not build (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  assert_eq "the base renders no PriorityClass" "0" "$(count "$BASE" 'select(.kind == "PriorityClass")')"
  assert_eq "the base k-orc Kustomization has no spec.patches" \
    "null" "$(val "$(obj "$BASE" Kustomization k-orc flux-system)" '.spec.patches')"
  assert_eq "the base rabbitmq-cluster-operator Kustomization has no spec.patches" \
    "null" "$(val "$(obj "$BASE" Kustomization rabbitmq-cluster-operator flux-system)" '.spec.patches')"
  assert_eq "the base FluxInstance has no spec.kustomize" \
    "null" "$(val "$(obj "$BASE" FluxInstance flux flux-system)" '.spec.kustomize')"
}

# --- Test 9: the keystone-operator chart takes the example's values ---
test_keystone_chart_renders_values() {
  echo "Test: the keystone-operator chart renders the example's spec.values"

  if [ "$OVERLAY_OK" != true ]; then
    echo "  SKIP: the overlay did not build (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi
  if ! command -v helm >/dev/null 2>&1; then
    echo "  SKIP: helm not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local chart hr values_file err_file rendered deploy
  chart="$PROJECT_ROOT/operators/keystone/helm/keystone-operator"
  err_file="$(mktemp)"
  # The helm-validate job vendors the operator-library subchart with make
  # helm-deps before it renders. charts/ is gitignored and survives branch
  # switches, and helm template matches a subchart by name only, so always
  # rebuild: a populated charts/ may hold an operator-library version
  # Chart.lock has moved past (see hack/ci-deploy-operator.sh).
  if ! helm dependency build --skip-refresh "$chart" >/dev/null 2>"$err_file"; then
    echo "  FAIL: helm dependency build $chart failed:"
    sed 's/^/    /' "$err_file"
    FAIL=$((FAIL + 1))
    echo "  SKIP: nothing rendered (5 checks skipped)"
    SKIP=$((SKIP + 5))
    rm -f "$err_file"
    return
  fi

  hr="$(obj "$OVERLAY" HelmRelease keystone-operator keystone-system)"
  values_file="$(mktemp)"
  val "$hr" '.spec.values' >"$values_file"
  # The chart's values schema rejects a key or value it does not admit, so a
  # failed render is the example gone stale against the chart it targets.
  if ! rendered="$(helm template test "$chart" -f "$values_file" 2>"$err_file")"; then
    echo "  FAIL: helm template rejects the example's values:"
    sed 's/^/    /' "$err_file"
    FAIL=$((FAIL + 1))
    echo "  SKIP: nothing rendered (5 checks skipped)"
    SKIP=$((SKIP + 5))
    rm -f "$values_file" "$err_file"
    return
  fi
  echo "  PASS: helm template renders the chart with the example's values"
  PASS=$((PASS + 1))
  rm -f "$values_file" "$err_file"

  deploy="$(printf '%s\n' "$rendered" | yq 'select(.kind == "Deployment")')"
  assert_eq "the Deployment's replicas" \
    "$(val "$hr" '.spec.values.replicas')" "$(val "$deploy" '.spec.replicas')"
  assert_eq "the manager container's resources" \
    "$(cjson "$hr" '.spec.values.resources')" \
    "$(cjson "$deploy" '.spec.template.spec.containers[0].resources')"
  assert_eq "the pod's nodeSelector" "$NODE_SELECTOR" "$(cjson "$deploy" '.spec.template.spec.nodeSelector')"
  assert_eq "the pod's tolerations" "$TOLERATIONS" "$(cjson "$deploy" '.spec.template.spec.tolerations')"
  assert_eq "the pod's priorityClassName" "$PRIORITY_CLASS" "$(val "$deploy" '.spec.template.spec.priorityClassName')"
}

# --- Run ---
if ! command -v kustomize >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1; then
  echo "SKIP: kustomize or yq not installed (all checks skipped)"
  SKIP=$((SKIP + 1))
  results
  exit 0
fi

echo "Test: kustomize builds the overlay, its infrastructure phase and both bases"
OVERLAY="" INFRA_OVERLAY="" BASE="" INFRA_BASE=""
OVERLAY_OK=false INFRA_OVERLAY_OK=false BASE_OK=false
build OVERLAY "$OVERLAY_DIR" && OVERLAY_OK=true
build INFRA_OVERLAY "$INFRA_OVERLAY_DIR" && INFRA_OVERLAY_OK=true
build BASE "$BASE_DIR" && BASE_OK=true
build INFRA_BASE "$INFRA_BASE_DIR" || INFRA_OVERLAY_OK=false
# The overlay checks compare against the bases.
[ "$BASE_OK" = true ] || OVERLAY_OK=false

test_object_counts
test_priority_class
test_keystone_release
test_cert_manager_release
test_flux_kustomization_patches
test_fluxinstance_patch
test_infrastructure_patches
test_production_base_unchanged
test_keystone_chart_renders_values

results
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
