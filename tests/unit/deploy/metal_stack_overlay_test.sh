#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the metal-stack lab overlay that hack/deploy-infra.sh applies under
# EXTERNAL_CLUSTER=true:
#   1. deploy/lab/metal-stack/{base,infrastructure}/kustomization.yaml exist
#      with SPDX headers, and each lists exactly one resource: the matching
#      kind overlay (../../../kind/base, ../../../kind/infrastructure).
#   2. The base render carries the OpenBao HelmRelease on `premium` in
#      standalone mode, the envoy-gateway HelmRelease, the twelve-listener
#      openstack-gw Gateway and the nine suspended service-operator releases,
#      and no metrics-server or vertical-pod-autoscaler release (the platform
#      runs both).
#   3. The infrastructure render carries MariaDB and both Garage volumes on
#      `premium` at one replica, the NodePort EnvoyProxy on 31443, the paused
#      proving OpenBaoCluster without egress fields (hack/deploy-infra.sh
#      patches those in) and the openstack OpenBaoTenant.
#   4. Neither render names local-path, ceph-rbd or the `standard` class.
#   5. The kind infrastructure overlay still renders MariaDB on `standard`,
#      so the lab overlay changed nothing under deploy/kind/.
#
# Checks 2 to 5 are counted as SKIP when kustomize or yq is not on PATH; a
# failing kustomize build counts them as FAIL and prints the build's error. An
# empty render fails the object checks instead of passing on zero documents.
#
# Usage: bash tests/unit/deploy/metal_stack_overlay_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

LAB_DIR="$PROJECT_ROOT/deploy/lab/metal-stack"
BASE_DIR="$LAB_DIR/base"
INFRA_DIR="$LAB_DIR/infrastructure"
KIND_INFRA_DIR="$PROJECT_ROOT/deploy/kind/infrastructure"

SERVICE_OPERATORS="keystone horizon glance placement barbican ovn neutron cinder nova"

RENDERED=""

have() {
  command -v "$1" >/dev/null 2>&1
}

# render <dir> <checks>
# Renders <dir> into RENDERED. When the render cannot be read, counts the
# caller's checks as SKIP (kustomize or yq missing) or FAIL (the build failed,
# or it produced no document) and returns 1.
render() {
  local dir="$1" checks="$2"

  if ! have kustomize || ! have yq; then
    echo "  SKIP: kustomize or yq not installed ($checks checks skipped)"
    SKIP=$((SKIP + checks))
    return 1
  fi

  # No --load-restrictor: kubectl apply -k cannot pass one either.
  if ! RENDERED="$(kustomize build "$dir" 2>&1)"; then
    echo "  FAIL: kustomize build $dir failed (default LoadRestrictionsRootOnly):"
    echo "$RENDERED" | head -20
    FAIL=$((FAIL + checks))
    return 1
  fi

  local count
  count="$(printf '%s\n' "$RENDERED" | yq -N -r 'select(. != null) | .kind' - | grep -c .)"
  if [[ "$count" -eq 0 ]]; then
    echo "  FAIL: kustomize build $dir rendered no document"
    FAIL=$((FAIL + checks))
    return 1
  fi
}

# val <kind> <name> <expression>
# The value <expression> yields on the rendered object of <kind> named <name>.
val() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"$1\" and .metadata.name == \"$2\") | $3" - | head -n 1
}

# count_named <kind> <name>
# How many rendered objects of <kind> are named <name>.
count_named() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"$1\" and .metadata.name == \"$2\") | .metadata.name" - | grep -c .
}

# resource_entries <kustomization>
# The items of the top-level resources key; the range ends at the next
# top-level key.
resource_entries() {
  awk '
    /^resources:/ { in_list = 1; next }
    in_list && /^[^[:space:]#-]/ { in_list = 0 }
    in_list && /^[[:space:]]*-[[:space:]]+/ {
      sub(/^[[:space:]]*-[[:space:]]+/, ""); sub(/[[:space:]]+$/, ""); print
    }
  ' "$1"
}

# --- Test 1: both kustomizations exist, carry SPDX and name the kind overlay ---
test_files_have_spdx_and_one_resource() {
  echo "Test: the lab kustomizations carry SPDX headers and take the kind overlay as their one resource"

  local dir expected f
  for dir in base infrastructure; do
    f="$LAB_DIR/$dir/kustomization.yaml"
    if [[ ! -f "$f" ]]; then
      echo "  FAIL: $f does not exist"
      FAIL=$((FAIL + 3))
      continue
    fi
    assert_file_contains "$dir/kustomization.yaml has SPDX FileCopyrightText header" \
      "$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$dir/kustomization.yaml has SPDX-License-Identifier: Apache-2.0" \
      "$f" "SPDX-License-Identifier: Apache-2.0"
    expected="../../../kind/$dir"
    assert_eq "$dir/kustomization.yaml lists $expected alone" \
      "$expected" "$(resource_entries "$f")"
  done
}

# --- Test 2: the base render ---
test_base_render() {
  echo "Test: kustomize build deploy/lab/metal-stack/base"

  render "$BASE_DIR" 20 || return

  assert_eq "HelmRelease openbao stores its data on premium" "premium" \
    "$(val HelmRelease openbao '.spec.values.server.dataStorage.storageClass')"
  assert_eq "HelmRelease openbao keeps HA disabled (inherited)" "false" \
    "$(val HelmRelease openbao '.spec.values.server.ha.enabled')"
  assert_eq "HelmRelease openbao keeps standalone mode (inherited)" "true" \
    "$(val HelmRelease openbao '.spec.values.server.standalone.enabled')"
  assert_eq "HelmRelease envoy-gateway is rendered" "1" \
    "$(count_named HelmRelease envoy-gateway)"
  assert_eq "Gateway openstack-gw carries twelve listeners" "12" \
    "$(val Gateway openstack-gw '.spec.listeners | length')"

  local svc
  for svc in $SERVICE_OPERATORS; do
    assert_eq "HelmRelease ${svc}-operator stays suspended" "true" \
      "$(val HelmRelease "${svc}-operator" '.spec.suspend')"
  done

  assert_eq "no metrics-server HelmRelease is rendered" "0" \
    "$(count_named HelmRelease metrics-server)"
  assert_eq "no vertical-pod-autoscaler HelmRelease is rendered" "0" \
    "$(count_named HelmRelease vertical-pod-autoscaler)"
  assert_no_foreign_class "base"
}

# --- Test 3: the infrastructure render ---
test_infrastructure_render() {
  echo "Test: kustomize build deploy/lab/metal-stack/infrastructure"

  render "$INFRA_DIR" 16 || return

  assert_eq "MariaDB openstack-db stores its data on premium" "premium" \
    "$(val MariaDB openstack-db '.spec.storage.storageClassName')"
  assert_eq "MariaDB openstack-db keeps one replica (inherited)" "1" \
    "$(val MariaDB openstack-db '.spec.replicas')"
  assert_eq "MariaDB openstack-db keeps Galera disabled (inherited)" "false" \
    "$(val MariaDB openstack-db '.spec.galera.enabled')"
  assert_eq "GarageCluster garage keeps its metadata on premium" "premium" \
    "$(val GarageCluster garage '.spec.storage.metadata.storageClassName')"
  assert_eq "GarageCluster garage keeps its data on premium" "premium" \
    "$(val GarageCluster garage '.spec.storage.data.storageClassName')"
  assert_eq "GarageCluster garage keeps one storage node (inherited)" "1" \
    "$(val GarageCluster garage '.spec.storage.replicas')"
  assert_eq "EnvoyProxy envoy-nodeport keeps the NodePort Service" "NodePort" \
    "$(val EnvoyProxy envoy-nodeport '.spec.provider.kubernetes.envoyService.type')"
  assert_eq "EnvoyProxy envoy-nodeport keeps nodePort 31443" "31443" \
    "$(val EnvoyProxy envoy-nodeport '.spec.provider.kubernetes.envoyService.patch.value.spec.ports[0].nodePort')"
  assert_eq "OpenBaoCluster openbao-instance starts paused" "true" \
    "$(val OpenBaoCluster openbao-instance '.spec.paused')"
  # hack/deploy-infra.sh patches both in from the live cluster together with
  # the un-pause; a hardcoded value would be wrong on the next cluster.
  assert_eq "OpenBaoCluster openbao-instance hardcodes no apiServerEndpointIPs" "null" \
    "$(val OpenBaoCluster openbao-instance '.spec.network.apiServerEndpointIPs')"
  assert_eq "OpenBaoCluster openbao-instance hardcodes no egressRules" "null" \
    "$(val OpenBaoCluster openbao-instance '.spec.network.egressRules')"
  assert_eq "OpenBaoTenant openstack is rendered" "1" \
    "$(count_named OpenBaoTenant openstack)"
  assert_no_foreign_class "infrastructure"
}

# assert_no_foreign_class <label>
# Four checks: the render in RENDERED names none of the production classes
# and not the kind class.
assert_no_foreign_class() {
  local needle
  for needle in "local-path" "ceph-rbd" "storageClass: standard" "storageClassName: standard"; do
    assert_not_contains "the $1 render names no '$needle'" "$RENDERED" "$needle"
  done
}

# --- Test 4: the kind overlay is unchanged ---
test_kind_overlay_unchanged() {
  echo "Test: kustomize build deploy/kind/infrastructure still renders MariaDB on standard"

  render "$KIND_INFRA_DIR" 1 || return

  assert_eq "the kind MariaDB stays on the standard class" "standard" \
    "$(val MariaDB openstack-db '.spec.storage.storageClassName')"
}

# --- Run ---
test_files_have_spdx_and_one_resource
test_base_render
test_infrastructure_render
test_kind_overlay_unchanged

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
