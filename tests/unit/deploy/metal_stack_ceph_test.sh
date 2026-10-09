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
#   5. Every file of ceph/cluster/ carries the SPDX header, and its
#      kustomization lists its six local files, no entry naming a parent
#      directory.
#   6. The render of ceph/cluster/ holds sixteen objects: the CephCluster, two
#      CephBlockPools, two CephClients and the toolbox in rook-ceph, the push
#      side there and the pull side in openstack.
#   7. The CephCluster runs a digest-pinned Ceph 20.2 image with its data under
#      /var/lib/rook, one mon and one mgr, no dashboard, crash collector or
#      metrics, msgr2 required, a default pool size of "2", the
#      AUTH_INSECURE_CLIENT_KEY_TYPE warning muted and an empty cleanup
#      confirmation, and one device set of three portable OSDs on 100Gi Block
#      claims with 2Gi of memory requested and as the limit. No line of either
#      render names a storage class.
#   8. The pools volumes and backups keep two replicas across hosts; the
#      clients cinder and cinder-backup carry the rbd caps on their pool and
#      aes keys rotated by keyGeneration 1.
#   9. The toolbox runs the CephCluster's image as UID 2016 with every
#      capability dropped, no privilege escalation and the RuntimeDefault
#      seccomp profile, and mounts no ServiceAccount token.
#  10. In rook-ceph the PushSecrets push the userKey of Rook's client Secrets
#      to ceph/client-cinder and ceph/client-cinder-backup through the
#      SecretStore openbao-ceph-store, which authenticates as push-ceph-keys
#      on kubernetes/management with a client Certificate of
#      openbao-ca-issuer.
#  11. In openstack the ExternalSecrets read those two keys back into Secrets
#      of their own names through a SecretStore of the same name that
#      authenticates as read-ceph-keys.
#
# Checks 2 to 4 and 6 to 11 are counted as SKIP when kustomize or yq is not on
# PATH; a failing kustomize build counts them as FAIL and prints the build's
# error.
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
CLUSTER_DIR="$CEPH_DIR/cluster"

# The image pattern of the CephCluster, Tentacle by tag and digest.
IMAGE_PATTERN='^quay\.io/ceph/ceph:v20\.2\.[0-9]+@sha256:[a-f0-9]{64}$'

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

# rendered <kind> <namespace> <name> <expression>
# The value <expression> yields on the rendered object of <kind> named <name>
# in <namespace>: the push and the pull side share their store name.
rendered() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"$1\" and .metadata.namespace == \"$2\" and .metadata.name == \"$3\") | $4" - |
    head -n 1
}

# --- Test 5: the files of ceph/cluster/ ---
test_cluster_files() {
  echo "Test: the files of deploy/lab/metal-stack/ceph/cluster carry SPDX headers and the kustomization lists the six local files"

  local f
  for f in kustomization.yaml cluster.yaml pools.yaml clients.yaml toolbox.yaml keys-push.yaml keys-pull.yaml; do
    assert_spdx "$CLUSTER_DIR/$f"
  done
  assert_local_entries "$CLUSTER_DIR/kustomization.yaml" \
    "$(printf '%s\n' cluster.yaml pools.yaml clients.yaml toolbox.yaml keys-push.yaml keys-pull.yaml)"
}

# --- Test 6: the sixteen objects of ceph/cluster/ ---
test_cluster_render_holds_sixteen_objects() {
  echo "Test: kustomize build deploy/lab/metal-stack/ceph/cluster renders the Ceph, its dependents and both halves of the key hand-off"

  render "$CLUSTER_DIR" 1 || return

  assert_eq "the render holds exactly the sixteen objects" \
    "$(printf '%s\n' \
      'CephCluster rook-ceph/rook-ceph' \
      'CephBlockPool rook-ceph/volumes' \
      'CephBlockPool rook-ceph/backups' \
      'CephClient rook-ceph/cinder' \
      'CephClient rook-ceph/cinder-backup' \
      'Deployment rook-ceph/rook-ceph-tools' \
      'ServiceAccount rook-ceph/ceph-keys-push' \
      'Certificate rook-ceph/ceph-keys-push-client-tls' \
      'SecretStore rook-ceph/openbao-ceph-store' \
      'PushSecret rook-ceph/ceph-client-cinder' \
      'PushSecret rook-ceph/ceph-client-cinder-backup' \
      'ServiceAccount openstack/ceph-keys-read' \
      'Certificate openstack/ceph-keys-read-client-tls' \
      'SecretStore openstack/openbao-ceph-store' \
      'ExternalSecret openstack/ceph-client-cinder' \
      'ExternalSecret openstack/ceph-client-cinder-backup' | LC_ALL=C sort)" \
    "$(objects)"
}

# --- Test 7: the CephCluster ---
test_cephcluster() {
  echo "Test: the CephCluster is one mon, one mgr and three portable OSDs on raw block volumes of the default class"

  render "$CLUSTER_DIR" 24 || return

  local image
  image="$(val CephCluster rook-ceph '.spec.cephVersion.image')"
  assert_eq "the image is Ceph 20.2 by tag and digest" "true" \
    "$([[ "$image" =~ $IMAGE_PATTERN ]] && echo true || echo false)"
  assert_eq "the CephCluster lives in rook-ceph" "rook-ceph" "$(val CephCluster rook-ceph '.metadata.namespace')"
  assert_eq "no unsupported Ceph version is allowed" "false" "$(val CephCluster rook-ceph '.spec.cephVersion.allowUnsupported')"
  assert_eq "the data lives under /var/lib/rook" "/var/lib/rook" "$(val CephCluster rook-ceph '.spec.dataDirHostPath')"
  assert_eq "one mon" "1" "$(val CephCluster rook-ceph '.spec.mon.count')"
  assert_eq "the mon keeps its data on a 10Gi claim" "10Gi" \
    "$(val CephCluster rook-ceph '.spec.mon.volumeClaimTemplate.spec.resources.requests.storage')"
  assert_eq "one mgr" "1" "$(val CephCluster rook-ceph '.spec.mgr.count')"
  assert_eq "no dashboard" "false" "$(val CephCluster rook-ceph '.spec.dashboard.enabled')"
  assert_eq "no crash collector" "true" "$(val CephCluster rook-ceph '.spec.crashCollector.disable')"
  assert_eq "no metrics" "true" "$(val CephCluster rook-ceph '.spec.monitoring.metricsDisabled')"
  assert_eq "msgr2 is required" "true" "$(val CephCluster rook-ceph '.spec.network.connections.requireMsgr2')"
  # A bare 2 would be an integer, which the CRD's string map refuses.
  assert_eq "the default pool size is the string \"2\"" '!!str 2' \
    "$(val CephCluster rook-ceph '.spec.cephConfig.global.osd_pool_default_size | tag + " " + .')"
  assert_eq "AUTH_INSECURE_CLIENT_KEY_TYPE is muted" "mute" \
    "$(val CephCluster rook-ceph '.spec.healthCheck.muteHealthWarning.AUTH_INSECURE_CLIENT_KEY_TYPE.policy')"
  # Rook refuses spec changes once the confirmation is set; only the teardown
  # sets it.
  assert_eq "the cleanup confirmation is the empty string" '!!str ' \
    "$(val CephCluster rook-ceph '.spec.cleanupPolicy.confirmation | tag + " " + .')"
  assert_eq "the CephCluster has one device set" "1" \
    "$(val CephCluster rook-ceph '.spec.storage.storageClassDeviceSets | length')"
  local set='.spec.storage.storageClassDeviceSets[0]'
  assert_eq "the device set runs three OSDs" "3" "$(val CephCluster rook-ceph "$set.count")"
  assert_eq "an OSD's volume follows it to another node" "true" "$(val CephCluster rook-ceph "$set.portable")"
  assert_eq "the device set has one claim template" "1" "$(val CephCluster rook-ceph "$set.volumeClaimTemplates | length")"
  assert_eq "the OSD claim is a raw block volume" "Block" \
    "$(val CephCluster rook-ceph "$set.volumeClaimTemplates[0].spec.volumeMode")"
  assert_eq "the OSD claim asks for 100Gi" "100Gi" \
    "$(val CephCluster rook-ceph "$set.volumeClaimTemplates[0].spec.resources.requests.storage")"
  assert_eq "an OSD requests 2Gi of memory" "2Gi" "$(val CephCluster rook-ceph "$set.resources.requests.memory")"
  assert_eq "and has the same as its limit" "2Gi" "$(val CephCluster rook-ceph "$set.resources.limits.memory")"
  assert_eq "the CephCluster object is rendered once" "1" "$(count_named CephCluster rook-ceph)"

  # Neither kustomization names a class: every claim binds to the cluster's
  # default class.
  local cluster_render="$RENDERED"
  render "$CEPH_DIR" 1 || return
  assert_eq "no line of either render sets storageClassName or storageClass" "" \
    "$(printf '%s\n' "$cluster_render" "$RENDERED" | grep -E '^[[:space:]]*storageClass(Name)?:' || true)"
  RENDERED="$cluster_render"
}

# --- Test 8: the pools and the clients ---
test_pools_and_clients() {
  echo "Test: the pools keep two replicas across hosts, and the clients carry rbd caps on their pool and aes keys"

  render "$CLUSTER_DIR" 20 || return

  local pool
  for pool in volumes backups; do
    assert_eq "the pool $pool spreads its replicas across hosts" "host" \
      "$(val CephBlockPool "$pool" '.spec.failureDomain')"
    assert_eq "the pool $pool keeps two replicas" "2" \
      "$(val CephBlockPool "$pool" '.spec.replicated.size')"
    assert_eq "the pool $pool refuses a single replica" "true" \
      "$(val CephBlockPool "$pool" '.spec.replicated.requireSafeReplicaSize')"
  done

  local client
  for client in cinder:volumes cinder-backup:backups; do
    pool="${client#*:}"
    client="${client%%:*}"
    assert_eq "the client $client may read the mon map as an rbd client" "profile rbd" \
      "$(val CephClient "$client" '.spec.caps.mon')"
    assert_eq "the client $client uses the pool $pool alone on the OSDs" "profile rbd pool=$pool" \
      "$(val CephClient "$client" '.spec.caps.osd')"
    assert_eq "and on the mgr" "profile rbd pool=$pool" \
      "$(val CephClient "$client" '.spec.caps.mgr')"
    assert_eq "the key of $client rotates by generation" "KeyGeneration" \
      "$(val CephClient "$client" '.spec.security.cephx.keyRotationPolicy')"
    assert_eq "at generation 1" "1" "$(val CephClient "$client" '.spec.security.cephx.keyGeneration')"
    assert_eq "with an aes key" "aes" "$(val CephClient "$client" '.spec.security.cephx.keyType')"
    assert_eq "the client $client has no other caps" "mgr mon osd" \
      "$(val CephClient "$client" '.spec.caps | keys | sort | join(" ")')"
  done
}

# --- Test 9: the toolbox ---
test_toolbox() {
  echo "Test: the toolbox runs the CephCluster's image as UID 2016 with every capability dropped"

  render "$CLUSTER_DIR" 9 || return

  local container='.spec.template.spec.containers[0]'
  assert_eq "the toolbox has one container" "1" \
    "$(val Deployment rook-ceph-tools '.spec.template.spec.containers | length')"
  assert_eq "the toolbox lives in rook-ceph" "rook-ceph" "$(val Deployment rook-ceph-tools '.metadata.namespace')"
  assert_eq "its image is the CephCluster's" "$(val CephCluster rook-ceph '.spec.cephVersion.image')" \
    "$(val Deployment rook-ceph-tools "$container.image")"
  assert_eq "it runs as UID 2016" "2016" "$(val Deployment rook-ceph-tools "$container.securityContext.runAsUser")"
  assert_eq "with every capability dropped" "ALL" \
    "$(val Deployment rook-ceph-tools "$container.securityContext.capabilities.drop | join(\",\")")"
  # The pod holds the admin keyring, and its namespace enforces the
  # privileged PodSecurity level, so admission sets no floor on it.
  assert_eq "it cannot escalate its privileges" "false" \
    "$(val Deployment rook-ceph-tools "$container.securityContext.allowPrivilegeEscalation")"
  assert_eq "it runs the RuntimeDefault seccomp profile" "RuntimeDefault" \
    "$(val Deployment rook-ceph-tools "$container.securityContext.seccompProfile.type")"
  assert_eq "and mounts no ServiceAccount token" "false" \
    "$(val Deployment rook-ceph-tools '.spec.template.spec.automountServiceAccountToken')"
  assert_eq "and a memory limit" "256Mi" "$(val Deployment rook-ceph-tools "$container.resources.limits.memory")"
}

# assert_store <namespace> <role> <service account> <certificate>
# The SecretStore openbao-ceph-store in <namespace> reaches OpenBao with the
# client Certificate <certificate> and authenticates as <role>.
assert_store() {
  local ns="$1" role="$2" sa="$3" cert="$4" vault='.spec.provider.vault'
  assert_eq "the store in $ns reaches the shared OpenBao on the KV v2 mount" \
    "https://openbao.shared-services.svc:8200 kv-v2 v2" \
    "$(rendered SecretStore "$ns" openbao-ceph-store "$vault | .server + \" \" + .path + \" \" + .version")"
  assert_eq "it authenticates on kubernetes/management" "kubernetes/management" \
    "$(rendered SecretStore "$ns" openbao-ceph-store "$vault.auth.kubernetes.mountPath")"
  assert_eq "as the role $role" "$role" \
    "$(rendered SecretStore "$ns" openbao-ceph-store "$vault.auth.kubernetes.role")"
  assert_eq "with the ServiceAccount $sa" "$sa" \
    "$(rendered SecretStore "$ns" openbao-ceph-store "$vault.auth.kubernetes.serviceAccountRef.name")"
  assert_eq "and the client certificate and CA of $cert" "$cert $cert $cert/ca.crt" \
    "$(rendered SecretStore "$ns" openbao-ceph-store "$vault | .tls.certSecretRef.name + \" \" + .tls.keySecretRef.name + \" \" + .caProvider.name + \"/\" + .caProvider.key")"
  assert_eq "the ServiceAccount $sa exists in $ns" "1" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r "select(.kind == \"ServiceAccount\" and .metadata.namespace == \"$ns\" and .metadata.name == \"$sa\") | .metadata.name" - | grep -c .)"
  assert_eq "the Certificate $cert is a client certificate" "client auth" \
    "$(rendered Certificate "$ns" "$cert" '.spec.usages | join(",")')"
  assert_eq "of the ClusterIssuer openbao-ca-issuer" "ClusterIssuer openbao-ca-issuer" \
    "$(rendered Certificate "$ns" "$cert" '.spec.issuerRef | .kind + " " + .name')"
  assert_eq "written to a Secret of its own name" "$cert" \
    "$(rendered Certificate "$ns" "$cert" '.spec.secretName')"
}

# --- Test 10: the push side ---
test_push_side() {
  echo "Test: the PushSecrets in rook-ceph push the raw client keys to OpenBao as push-ceph-keys"

  render "$CLUSTER_DIR" 25 || return

  assert_store rook-ceph push-ceph-keys ceph-keys-push ceph-keys-push-client-tls

  local ps
  for ps in ceph-client-cinder ceph-client-cinder-backup; do
    assert_eq "the PushSecret $ps reads Rook's Secret rook-$ps" "rook-$ps" \
      "$(rendered PushSecret rook-ceph "$ps" '.spec.selector.secret.name')"
    assert_eq "it pushes one key" "1" "$(rendered PushSecret rook-ceph "$ps" '.spec.data | length')"
    assert_eq "the key userKey" "userKey" "$(rendered PushSecret rook-ceph "$ps" '.spec.data[0].match.secretKey')"
    assert_eq "to ceph/${ps#ceph-}" "ceph/${ps#ceph-}" \
      "$(rendered PushSecret rook-ceph "$ps" '.spec.data[0].match.remoteRef.remoteKey')"
    assert_eq "under the property userKey" "userKey" \
      "$(rendered PushSecret rook-ceph "$ps" '.spec.data[0].match.remoteRef.property')"
    assert_eq "through the store of its namespace" "SecretStore openbao-ceph-store" \
      "$(rendered PushSecret rook-ceph "$ps" '.spec.secretStoreRefs | map(.kind + " " + .name) | join(",")')"
    assert_eq "leaving the value in OpenBao when it is deleted" "None" \
      "$(rendered PushSecret rook-ceph "$ps" '.spec.deletionPolicy')"
    assert_eq "and replacing it on every change" "Replace" \
      "$(rendered PushSecret rook-ceph "$ps" '.spec.updatePolicy')"
  done
}

# --- Test 11: the pull side ---
test_pull_side() {
  echo "Test: the ExternalSecrets in openstack read the client keys back as read-ceph-keys"

  render "$CLUSTER_DIR" 21 || return

  assert_store openstack read-ceph-keys ceph-keys-read ceph-keys-read-client-tls

  local es
  for es in ceph-client-cinder ceph-client-cinder-backup; do
    assert_eq "the ExternalSecret $es writes a Secret of its own name" "$es" \
      "$(rendered ExternalSecret openstack "$es" '.spec.target.name')"
    assert_eq "which it owns" "Owner" "$(rendered ExternalSecret openstack "$es" '.spec.target.creationPolicy')"
    assert_eq "it reads one key" "1" "$(rendered ExternalSecret openstack "$es" '.spec.data | length')"
    assert_eq "the property userKey of ceph/${es#ceph-} into userKey" "userKey ceph/${es#ceph-} userKey" \
      "$(rendered ExternalSecret openstack "$es" '.spec.data[0] | .secretKey + " " + .remoteRef.key + " " + .remoteRef.property')"
    assert_eq "through the store of its namespace" "SecretStore openbao-ceph-store" \
      "$(rendered ExternalSecret openstack "$es" '.spec.secretStoreRef | .kind + " " + .name')"
    assert_eq "and refreshes every minute" "1m" "$(rendered ExternalSecret openstack "$es" '.spec.refreshInterval')"
  done
}

test_operator_files
test_operator_render_holds_three_objects
test_operator_namespace_and_source
test_operator_release
test_cluster_files
test_cluster_render_holds_sixteen_objects
test_cephcluster
test_pools_and_clients
test_toolbox
test_push_side
test_pull_side

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
