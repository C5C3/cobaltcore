#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the lab hypervisors under deploy/lab/metal-stack/hypervisor and
# deploy/lab/metal-stack/hypervisor-fixtures, applied by hand on the lab:
#   1. The eleven files exist with SPDX headers, and each kustomization lists
#      exactly its resources.
#   2. The fixtures render is the kind fixtures without the volume type, the
#      network and the subnet.
#   3. The hypervisor render is the thirteen objects of the two operators,
#      the libvirt DaemonSet, the CA, the compute CRs, the gateway alias and
#      the chart sources, and the namespace carries the Gardener opt-out
#      label.
#   4. The libvirt DaemonSet runs in the host's namespaces, OnDelete, with the
#      image, the hostPaths and the mount propagation libvirtd and
#      nova-compute share, and without a liveness probe.
#   5. libvirtd.conf and qemu.conf carry every key and value the lab needs.
#   6. libvirtd.sh starts libvirtd in a host scope and writes both host units,
#      and the scripts carry their log messages.
#   7. The CA, its Issuer and the three compute CRs carry their fields: the
#      metadata agent names no resources, and the pool names the CPU model
#      Nova's live-migration pre-check accepts; NovaCompute has no extraConfig.
#   8. The hvo release feeds the auth Secret into the six chart values, sets
#      the lab values and a fullnameOverride that keeps every object name
#      within 63 characters, and its post-renderer aliases the four gateway
#      names to the alias Service's ClusterIP and trusts the gateway
#      certificates.
#   9. The kna release runs the image ghcr.io/c5c3/kvm-node-agent without a
#      tag, sets the libvirt URI, the node label field path, DAC_OVERRIDE
#      alone without a runAsUser or runAsGroup, and NAMESPACE.
#  10. Both chart sources are pinned by digest.
#  11. Each chart tag names the commit its image's resolver prints: the hvo
#      tag hack/ci-resolve-hvo-commit.sh's, the kna tag
#      hack/ci-resolve-kna-commit.sh's.
#  12. Both scripts parse, and pass shellcheck when it is on PATH.
#  13. libvirtd.sh, run against stubs, removes the host units and stops
#      libvirtd however it ends, stops a libvirtd an earlier container left
#      in the scope before it starts its own, and leaves no failed scope
#      behind when a stop runs out.
#  14. Each chart's tag and digest resolve to the same upstream artifact
#      (one SKIP per chart when ghcr.io cannot be reached).
#
# Checks 2 to 5, 7 to 10, 12 and 13 are counted as SKIP when kustomize or yq
# is not on PATH. A failing kustomize build counts them as FAIL and prints the
# build's error. Checks 1, 6 and 11 read the files and need neither tool.
#
# Usage: bash tests/unit/deploy/metal_stack_hypervisor_test.sh

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

HYPERVISOR_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor"
FIXTURES_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor-fixtures"
CONFIGMAP_FILE="$HYPERVISOR_DIR/libvirt-configmap.yaml"
SOURCES_FILE="$HYPERVISOR_DIR/sources.yaml"
RESOLVE_HVO_COMMIT="$PROJECT_ROOT/hack/ci-resolve-hvo-commit.sh"
RESOLVE_KNA_COMMIT="$PROJECT_ROOT/hack/ci-resolve-kna-commit.sh"

HYPERVISOR_FILES="namespace.yaml
libvirt-ca.yaml
libvirt-configmap.yaml
libvirt-daemonset.yaml
compute.yaml
gateway-alias.yaml
sources.yaml
hvo-release.yaml
kna-release.yaml"

AUTH_SECRET="controlplane-nova-hypervisor-operator-auth"

RENDERED=""

# hvo_patch <expression>
# Every value <expression> yields on the hvo release's Deployment
# post-render patch, one per line.
hvo_patch() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"HelmRelease\" and .metadata.name == \"openstack-hypervisor-operator\") |
      .spec.postRenderers[0].kustomize.patches[0].patch | from_yaml | $1" -
}

# containers <expression>
# <expression> on every container of the libvirt DaemonSet, the init
# container first, one line each. Neither this nor mounted_volume collects
# with [...]: yq answers a collect with an empty array for every document
# the select drops, so val would read another document's line.
containers() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"DaemonSet\" and .metadata.name == \"libvirt\") |
      (.spec.template.spec.initContainers + .spec.template.spec.containers)[] | $1" -
}

# mounted_volume <mount path> <expression>
# <expression> on the libvirt DaemonSet volume the libvirtd container mounts
# at <mount path>.
mounted_volume() {
  local name
  name="$(val DaemonSet libvirt \
    ".spec.template.spec.containers[0].volumeMounts[] | select(.mountPath == \"$1\") | .name")"
  val DaemonSet libvirt ".spec.template.spec.volumes[] | select(.name == \"$name\") | $2"
}

# conf_lines <key>
# The lines of the ConfigMap file <key>, as rendered.
conf_lines() {
  printf '%s\n' "$RENDERED" |
    yq -N -r "select(.kind == \"ConfigMap\" and .metadata.name == \"libvirt-lab\") | .data[\"$1\"]" -
}

# chart_ref <chart> <field>
# ref.<field> (tag or digest) of the OCIRepository of the upstream chart
# <chart> in sources.yaml: the first such line after its url, read without
# yq.
chart_ref() {
  awk -v url="url: oci://ghcr.io/cobaltcore-dev/charts/$1" -v field="$2" '
    substr($0, length($0) - length(url) + 1) == url { found = 1; next }
    found && $0 ~ "^[[:space:]]*" field ":" {
      sub("^[[:space:]]*" field ":[[:space:]]*", ""); gsub(/"/, ""); print; exit
    }
  ' "$SOURCES_FILE"
}

# --- Test 1: the files, their SPDX headers and the resources lists ---
test_files_spdx_and_resources() {
  echo "Test: the lab hypervisor files carry SPDX headers and are listed"

  local files=("$FIXTURES_DIR/kustomization.yaml" "$HYPERVISOR_DIR/kustomization.yaml") name f
  while IFS= read -r name; do
    files+=("$HYPERVISOR_DIR/$name")
  done <<<"$HYPERVISOR_FILES"
  for f in "${files[@]}"; do
    name="${f#"$PROJECT_ROOT"/}"
    if [[ ! -f "$f" ]]; then
      echo "  FAIL: $name does not exist"
      FAIL=$((FAIL + 2))
      continue
    fi
    assert_file_contains "$name has SPDX FileCopyrightText header" \
      "$f" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_file_contains "$name has SPDX-License-Identifier: Apache-2.0" \
      "$f" "SPDX-License-Identifier: Apache-2.0"
  done

  if [[ -f "$HYPERVISOR_DIR/kustomization.yaml" ]]; then
    assert_eq "the hypervisor kustomization lists the nine manifests alone" \
      "$HYPERVISOR_FILES" "$(resource_entries "$HYPERVISOR_DIR/kustomization.yaml")"
  else
    echo "  FAIL: no hypervisor kustomization.yaml whose resources could be read"
    FAIL=$((FAIL + 1))
  fi
  if [[ -f "$FIXTURES_DIR/kustomization.yaml" ]]; then
    assert_eq "the fixtures kustomization lists the kind fixtures alone" \
      "../../../kind/hypervisor-operator-fixtures" \
      "$(resource_entries "$FIXTURES_DIR/kustomization.yaml")"
  else
    echo "  FAIL: no fixtures kustomization.yaml whose resources could be read"
    FAIL=$((FAIL + 1))
  fi
}

# --- Test 2: the fixtures render ---
test_fixtures_render() {
  echo "Test: kustomize build deploy/lab/metal-stack/hypervisor-fixtures"

  render "$FIXTURES_DIR" 11 || return

  local kinds
  kinds="$(printf '%s\n' "$RENDERED" | yq -N -r 'select(. != null) | .kind' -)"
  assert_eq "the render is eight objects" "8" "$(grep -c . <<<"$kinds")"
  local kind
  for kind in VolumeType Network Subnet; do
    assert_eq "no ${kind} is rendered" "0" "$(grep -cx "$kind" <<<"$kinds")"
  done
  local object
  for object in Domain/hvo-cc3test Project/hvo-test RoleAssignment/hvo-test-admin \
    Flavor/hvo-smoke-test-flavor Image/hvo-cirros-kvm; do
    assert_eq "${object} is rendered once" "1" "$(count_named "${object%%/*}" "${object#*/}")"
  done
  assert_eq "the flavor keeps the ID the lab run boots" "1" \
    "$(val Flavor hvo-smoke-test-flavor '.spec.resource.id')"
  assert_eq "the image keeps the name the lab run boots" "cirros-kvm" \
    "$(val Image hvo-cirros-kvm '.spec.resource.name')"
}

# --- Test 3: the hypervisor render ---
test_hypervisor_render_objects() {
  echo "Test: kustomize build deploy/lab/metal-stack/hypervisor"

  render "$HYPERVISOR_DIR" 3 || return

  assert_eq "the render is the thirteen objects of the lab hypervisors" \
    "$(printf '%s\n' \
      Certificate/libvirt-migration-ca \
      ConfigMap/libvirt-lab \
      DaemonSet/libvirt \
      HelmRelease/kvm-node-agent \
      HelmRelease/openstack-hypervisor-operator \
      Issuer/nova-hypervisor-agents-ca-issuer \
      Namespace/hypervisor-system \
      NeutronMetadataAgent/lab-metadata-agent \
      NovaCompute/lab \
      OCIRepository/kvm-node-agent \
      OCIRepository/openstack-hypervisor-operator \
      OVNChassis/lab-chassis \
      Service/openstack-gw-8443)" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(. != null) | .kind + "/" + .metadata.name' - | sort)"
  assert_eq "the namespaced objects live in openstack, hypervisor-system, envoy-gateway-system and flux-system" \
    "$(printf '%s\n' envoy-gateway-system flux-system hypervisor-system openstack)" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(. != null and .kind != "Namespace") | .metadata.namespace' - | sort -u)"
  assert_eq "hypervisor-system opts out of Gardener's apiserver-proxy injection" "disable" \
    "$(val Namespace hypervisor-system '.metadata.labels["apiserver-proxy.networking.gardener.cloud/inject"]')"
}

# --- Test 4: the libvirt DaemonSet ---
test_libvirt_daemonset() {
  echo "Test: the libvirt DaemonSet"

  render "$HYPERVISOR_DIR" 24 || return

  local pod='.spec.template.spec'
  assert_eq "the DaemonSet lives in openstack" "openstack" \
    "$(val DaemonSet libvirt '.metadata.namespace')"
  assert_eq "it selects the pool's nodes" "lab" \
    "$(val DaemonSet libvirt "$pod.nodeSelector[\"openstack.c5c3.io/nova-compute-pool\"]")"
  assert_eq "it uses the host's network" "true" "$(val DaemonSet libvirt "$pod.hostNetwork")"
  assert_eq "it uses the host's PID namespace" "true" "$(val DaemonSet libvirt "$pod.hostPID")"
  assert_eq "it uses the host's IPC namespace" "true" "$(val DaemonSet libvirt "$pod.hostIPC")"
  assert_eq "it resolves cluster names from the host network" "ClusterFirstWithHostNet" \
    "$(val DaemonSet libvirt "$pod.dnsPolicy")"
  assert_eq "it mounts no ServiceAccount token" "false" \
    "$(val DaemonSet libvirt "$pod.automountServiceAccountToken")"
  assert_eq "a rollout waits for a pod deletion" "OnDelete" \
    "$(val DaemonSet libvirt '.spec.updateStrategy.type')"
  assert_eq "the grace period is 30 seconds" "30" \
    "$(val DaemonSet libvirt "$pod.terminationGracePeriodSeconds")"
  assert_eq "the pod carries the part-of label" "lab-hypervisor" \
    "$(val DaemonSet libvirt '.spec.template.metadata.labels["app.kubernetes.io/part-of"]')"

  assert_eq "both containers run ghcr.io/c5c3/libvirt:latest" \
    "$(printf '%s\n' host-prepare=ghcr.io/c5c3/libvirt:latest libvirtd=ghcr.io/c5c3/libvirt:latest)" \
    "$(containers '.name + "=" + .image')"
  assert_eq "both containers pull Always" \
    "$(printf '%s\n' host-prepare=Always libvirtd=Always)" \
    "$(containers '.name + "=" + .imagePullPolicy')"
  assert_eq "both containers are privileged" \
    "$(printf '%s\n' host-prepare=true libvirtd=true)" \
    "$(containers '.name + "=" + (.securityContext.privileged | tostring)')"
  assert_eq "both containers run as uid 0" \
    "$(printf '%s\n' host-prepare=0 libvirtd=0)" \
    "$(containers '.name + "=" + (.securityContext.runAsUser | tostring)')"
  assert_eq "HOST_IP comes from status.hostIP" "status.hostIP" \
    "$(val DaemonSet libvirt "$pod.containers[0].env[] | select(.name == \"HOST_IP\") | .valueFrom.fieldRef.fieldPath")"

  assert_eq "the hostPaths are the eleven libvirtd and nova-compute share or need" \
    "$(printf '%s\n' /dev /etc/pki/CA /etc/pki/libvirt /etc/pki/qemu /lib/modules \
      /run/dbus /run/libvirt /run/systemd /sys/fs/cgroup /var/lib/libvirt /var/lib/nova)" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r "select(.kind == \"DaemonSet\") |
      $pod.volumes[] | select(.hostPath) | .hostPath.path" - | sort)"
  assert_eq "the six state and PKI hostPaths are DirectoryOrCreate" \
    "$(printf '%s\n' /etc/pki/CA /etc/pki/libvirt /etc/pki/qemu /run/libvirt /var/lib/libvirt /var/lib/nova)" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r "select(.kind == \"DaemonSet\") |
      $pod.volumes[] | select(.hostPath.type == \"DirectoryOrCreate\") | .hostPath.path" - | sort)"
  assert_eq "/var/lib/nova propagates mounts both ways" "Bidirectional" \
    "$(val DaemonSet libvirt "$pod.containers[0].volumeMounts[] | select(.mountPath == \"/var/lib/nova\") | .mountPropagation")"
  assert_eq "libvirtd mounts the PKI, the modules and D-Bus read-only" \
    "$(printf '%s\n' /etc/libvirt-lab /etc/pki/CA /etc/pki/libvirt /etc/pki/qemu /lib/modules /run/dbus)" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r "select(.kind == \"DaemonSet\") |
      $pod.containers[0].volumeMounts[] | select(.readOnly == true) | .mountPath" - | sort)"
  assert_eq "the ConfigMap is mounted at /etc/libvirt-lab" "libvirt-lab" \
    "$(mounted_volume /etc/libvirt-lab '.configMap.name')"

  assert_eq "the init container runs host-prepare.sh" "/bin/bash /etc/libvirt-lab/host-prepare.sh" \
    "$(val DaemonSet libvirt "$pod.initContainers[0].command | join(\" \")")"
  assert_eq "the main container runs libvirtd.sh" "/bin/bash /etc/libvirt-lab/libvirtd.sh" \
    "$(val DaemonSet libvirt "$pod.containers[0].command | join(\" \")")"
  assert_eq "readiness asks libvirtd for its version" "virsh -c qemu:///system version" \
    "$(val DaemonSet libvirt "$pod.containers[0].readinessProbe.exec.command | join(\" \")")"
  assert_eq "no container has a liveness probe" "" \
    "$(containers 'select(has("livenessProbe")) | .name')"
}

# --- Test 5: libvirtd.conf and qemu.conf ---
test_libvirt_config() {
  echo "Test: the rendered libvirtd.conf and qemu.conf"

  render "$HYPERVISOR_DIR" 14 || return

  local libvirtd_conf qemu_conf line
  libvirtd_conf="$(conf_lines libvirtd.conf)"
  qemu_conf="$(conf_lines qemu.conf)"
  while IFS= read -r line; do
    assert_eq "libvirtd.conf sets ${line}" "1" "$(grep -cxF -- "$line" <<<"$libvirtd_conf")"
  done <<'LINES'
listen_tls = 1
listen_tcp = 0
tls_port = "16514"
listen_addr = "@HOST_IP@"
auth_tls = "none"
unix_sock_group = "+108"
unix_sock_rw_perms = "0770"
auth_unix_rw = "none"
LINES
  while IFS= read -r line; do
    assert_eq "qemu.conf sets ${line}" "1" "$(grep -cxF -- "$line" <<<"$qemu_conf")"
  done <<'LINES'
user = "root"
group = "root"
dynamic_ownership = 1
security_driver = "none"
default_tls_x509_cert_dir = "/etc/pki/qemu"
default_tls_x509_verify = 1
LINES
}

# --- Test 6: the scripts ---
test_scripts() {
  echo "Test: libvirtd.sh and host-prepare.sh"

  if [[ ! -f "$CONFIGMAP_FILE" ]]; then
    echo "  FAIL: $CONFIGMAP_FILE does not exist"
    FAIL=$((FAIL + 15))
    return
  fi
  local fixed
  while IFS= read -r fixed; do
    assert_file_contains_fixed "libvirt-configmap.yaml carries: ${fixed}" "$CONFIGMAP_FILE" "$fixed"
  done <<'FIXED'
sed "s/@HOST_IP@/${HOST_IP}/" /etc/libvirt-lab/libvirtd.conf >/etc/libvirt/libvirtd.conf
systemctl stop cobaltcore-libvirtd.scope 2>/dev/null || true
systemctl reset-failed cobaltcore-libvirtd.scope 2>/dev/null || true
systemd-run --collect --scope --slice=system --unit=cobaltcore-libvirtd libvirtd --listen &
cat >"${unit_dir}/libvirtd.service" <<EOF
cat >"${unit_dir}/virt-admin-server-update-tls.service" <<EOF
ExecStart=/usr/bin/tail --pid=${libvirtd_pid} -f /dev/null
ExecStartPre=/usr/bin/grep -q /cobaltcore-libvirtd.scope /proc/${libvirtd_pid}/cgroup
ExecStart=/usr/bin/nsenter --target ${libvirtd_pid} --mount -- /usr/bin/virt-admin server-update-tls libvirtd
# Written by the cobaltcore lab libvirt DaemonSet (openstack/libvirt); removed when its pod stops.
export SYSTEMD_IGNORE_CHROOT=1
echo "libvirtd: waiting for the TLS files kvm-node-agent installs under /etc/pki"
echo "libvirtd: /run/libvirt/libvirt-sock did not appear within 60s"
echo "host-prepare: cannot load vhost_net from /lib/modules/$(uname -r)"
echo "host-prepare: /dev/kvm is missing on this node"
FIXED
}

# --- Test 7: the CA and the compute CRs ---
test_ca_and_compute() {
  echo "Test: the libvirt CA and the three compute CRs"

  render "$HYPERVISOR_DIR" 32 || return

  assert_eq "the Issuer lives in hypervisor-system" "hypervisor-system" \
    "$(val Issuer nova-hypervisor-agents-ca-issuer '.metadata.namespace')"
  assert_eq "the Issuer signs with the CA's Secret" "libvirt-migration-ca" \
    "$(val Issuer nova-hypervisor-agents-ca-issuer '.spec.ca.secretName')"
  assert_eq "the CA lives in hypervisor-system" "hypervisor-system" \
    "$(val Certificate libvirt-migration-ca '.metadata.namespace')"
  assert_eq "the CA is a CA" "true" "$(val Certificate libvirt-migration-ca '.spec.isCA')"
  assert_eq "the CA writes Secret libvirt-migration-ca" "libvirt-migration-ca" \
    "$(val Certificate libvirt-migration-ca '.spec.secretName')"
  assert_eq "the CA's common name is libvirt-migration-ca" "libvirt-migration-ca" \
    "$(val Certificate libvirt-migration-ca '.spec.commonName')"
  assert_eq "the CA key is ECDSA 256" "ECDSA 256" \
    "$(val Certificate libvirt-migration-ca '.spec.privateKey.algorithm + " " + (.spec.privateKey.size | tostring)')"
  assert_eq "the CA lives three years and renews 30 days early" "26280h 720h" \
    "$(val Certificate libvirt-migration-ca '.spec.duration + " " + .spec.renewBefore')"
  assert_eq "the CA is bootstrapped by the selfsigned ClusterIssuer" "ClusterIssuer/selfsigned-cluster-issuer" \
    "$(val Certificate libvirt-migration-ca '.spec.issuerRef.kind + "/" + .spec.issuerRef.name')"

  assert_eq "the chassis follows controlplane-ovn" "controlplane-ovn" \
    "$(val OVNChassis lab-chassis '.spec.centralRef.name')"
  assert_eq "the chassis selects the chassis label, as a string" 'true !!str' \
    "$(val OVNChassis lab-chassis '.spec.nodeSelector["openstack.c5c3.io/chassis"] | . + " " + tag')"
  assert_eq "the chassis has no bridge mappings, gateway or image" "false false false" \
    "$(val OVNChassis lab-chassis '(.spec | has("bridgeMappings") | tostring) + " " +
      (.spec | has("gateway") | tostring) + " " + (.spec | has("image") | tostring)')"

  local agent='.spec'
  assert_eq "the metadata agent runs 2025.2" "2025.2" \
    "$(val NeutronMetadataAgent lab-metadata-agent "$agent.openStackRelease")"
  assert_eq "the metadata agent runs ghcr.io/c5c3/neutron:2025.2" "ghcr.io/c5c3/neutron:2025.2" \
    "$(val NeutronMetadataAgent lab-metadata-agent "$agent.image.repository + \":\" + $agent.image.tag")"
  assert_eq "the metadata agent runs beside lab-chassis" "lab-chassis" \
    "$(val NeutronMetadataAgent lab-metadata-agent "$agent.chassisRef.name")"
  assert_eq "the metadata agent proxies to the in-cluster Nova metadata API" \
    "http://controlplane-nova-metadata.openstack.svc:8775" \
    "$(val NeutronMetadataAgent lab-metadata-agent "$agent.novaMetadata.protocol + \"://\" + $agent.novaMetadata.host + \":\" + ($agent.novaMetadata.port | tostring)")"
  assert_eq "the metadata agent signs with the ControlPlane's shared secret" \
    "controlplane-nova-metadata-secret" \
    "$(val NeutronMetadataAgent lab-metadata-agent "$agent.novaMetadata.sharedSecretRef.name")"
  assert_eq "the metadata agent runs the operator's memory default" "null" \
    "$(val NeutronMetadataAgent lab-metadata-agent "$agent.resources")"

  assert_eq "the pool follows controlplane-nova" "controlplane-nova" \
    "$(val NovaCompute lab '.spec.novaRef.name')"
  assert_eq "the pool selects the pool label" "lab" \
    "$(val NovaCompute lab '.spec.nodeSelector["openstack.c5c3.io/nova-compute-pool"]')"
  assert_eq "the pool selects nothing else" "1" \
    "$(val NovaCompute lab '.spec.nodeSelector | length')"
  assert_eq "the pool runs KVM" "kvm" "$(val NovaCompute lab '.spec.libvirt.virtType')"
  assert_eq "the pool names a CPU model, which Nova's migration pre-check accepts" "custom" \
    "$(val NovaCompute lab '.spec.libvirt.cpuMode')"
  assert_eq "the model is the lab workers' host-model" "Skylake-Server-IBRS" \
    "$(val NovaCompute lab '.spec.libvirt.cpuModels | join(",")')"
  assert_eq "the pool keeps qcow2 disks" "qcow2" "$(val NovaCompute lab '.spec.libvirt.imagesType')"
  assert_eq "the pool has no extraConfig" "false" "$(val NovaCompute lab '.spec | has("extraConfig")')"
  assert_eq "the pool names no image" "false" "$(val NovaCompute lab '.spec | has("image")')"
  assert_eq "the pool has no tolerations" "false" "$(val NovaCompute lab '.spec | has("tolerations")')"
  assert_eq "the libvirt DaemonSet selects the pool's label" \
    "$(val NovaCompute lab '.spec.nodeSelector | to_json')" \
    "$(val DaemonSet libvirt '.spec.template.spec.nodeSelector | to_json')"

  local kind name
  for kind in OVNChassis NeutronMetadataAgent NovaCompute; do
    name="$(printf '%s\n' "$RENDERED" | yq -N -r "select(.kind == \"$kind\") | .metadata.name" - | head -n 1)"
    assert_eq "${kind} ${name} lives in openstack" "openstack" "$(val "$kind" "$name" '.metadata.namespace')"
  done
}

# --- Test 8: the hvo release ---
test_hvo_release() {
  echo "Test: the openstack-hypervisor-operator release"

  render "$HYPERVISOR_DIR" 19 || return

  local release=openstack-hypervisor-operator env='.spec.values.controllerManager.manager.env'
  assert_eq "the release lives in openstack" "openstack" \
    "$(val HelmRelease "$release" '.metadata.namespace')"
  assert_eq "the release takes its chart from the hvo OCIRepository" \
    "OCIRepository/flux-system/openstack-hypervisor-operator" \
    "$(val HelmRelease "$release" '.spec.chartRef.kind + "/" + .spec.chartRef.namespace + "/" + .spec.chartRef.name')"
  assert_eq "install and upgrade replace the CRDs" "CreateReplace CreateReplace" \
    "$(val HelmRelease "$release" '.spec.install.crds + " " + .spec.upgrade.crds')"
  assert_eq "valuesFrom maps the six auth Secret keys onto their chart values" \
    "$(printf '%s\n' \
      "password=secret.servicePassword" \
      "project_domain_name=controllerManager.manager.env.osProjectDomainName" \
      "project_name=controllerManager.manager.env.osProjectName" \
      "region_name=controllerManager.manager.env.osRegionName" \
      "user_domain_name=controllerManager.manager.env.osUserDomainName" \
      "username=controllerManager.manager.env.osUsername")" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r "select(.kind == \"HelmRelease\" and .metadata.name == \"$release\") |
      .spec.valuesFrom[] | .valuesKey + \"=\" + .targetPath" - | sort)"
  assert_eq "every valuesFrom reads the auth Secret" "Secret/${AUTH_SECRET}" \
    "$(printf '%s\n' "$RENDERED" | yq -N -r "select(.kind == \"HelmRelease\" and .metadata.name == \"$release\") |
      .spec.valuesFrom[] | .kind + \"/\" + .name" - | sort -u)"
  assert_eq "the image is the one #1163 builds" "ghcr.io/c5c3/openstack-hypervisor-operator" \
    "$(val HelmRelease "$release" '.spec.values.controllerManager.manager.image.repository')"
  assert_eq "every chart object name stays within 63 characters" "hypervisor-operator" \
    "$(val HelmRelease "$release" '.spec.values.fullnameOverride')"
  assert_eq "no image tag: the chart's appVersion names it" "false" \
    "$(val HelmRelease "$release" '.spec.values.controllerManager.manager.image | has("tag")')"
  assert_eq "osAuthUrl is the in-cluster Keystone URL" "http://controlplane-keystone.openstack.svc:5000/v3" \
    "$(val HelmRelease "$release" "$env.osAuthUrl")"
  assert_eq "the certificates go to hypervisor-system" "hypervisor-system" \
    "$(val HelmRelease "$release" "$env.certificateNamespace")"
  assert_eq "the agent namespace is openstack" "openstack" \
    "$(val HelmRelease "$release" "$env.agentNamespaces")"
  assert_eq "the monitoring objects are off" "false false false false" \
    "$(val HelmRelease "$release" '.spec.values | (.serviceMonitor.enabled | tostring) + " " +
      (.prometheusRules.create | tostring) + " " + (.dashboards.create | tostring) + " " +
      (.customResourceMetrics.create | tostring)')"

  assert_eq "the post-renderer patches the Deployment" "Deployment" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches[0].target.kind')"
  assert_eq "the four public hostnames are aliased" \
    "$(printf '%s\n' glance.127-0-0-1.nip.io neutron.127-0-0-1.nip.io nova.127-0-0-1.nip.io placement.127-0-0-1.nip.io)" \
    "$(hvo_patch '.spec.template.spec.hostAliases[0].hostnames[]' | sort)"
  assert_eq "the aliases point at the alias Service's ClusterIP" \
    "$(val Service openstack-gw-8443 '.spec.clusterIP')" \
    "$(hvo_patch '.spec.template.spec.hostAliases[0].ip')"
  assert_eq "the alias Service listens on 8443" "8443" \
    "$(val Service openstack-gw-8443 '.spec.ports[0].port')"
  assert_eq "the manager trusts the gateway certificates first" "/etc/lab-gateway-ca:/etc/ssl/certs" \
    "$(hvo_patch '.spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "SSL_CERT_DIR") | .value')"
  assert_eq "the gateway certificates are mounted read-only there" "lab-gateway-ca /etc/lab-gateway-ca true" \
    "$(hvo_patch '.spec.template.spec.containers[] | select(.name == "manager") | .volumeMounts[0] | .name + " " + .mountPath + " " + (.readOnly | tostring)')"
  assert_eq "the projected volume holds the ca.crt of the four gateway Secrets" \
    "$(printf '%s\n' glance-nip-io-tls/ca.crt=glance.crt neutron-nip-io-tls/ca.crt=neutron.crt \
      nova-nip-io-tls/ca.crt=nova.crt placement-nip-io-tls/ca.crt=placement.crt)" \
    "$(hvo_patch '.spec.template.spec.volumes[] | select(.name == "lab-gateway-ca") | .projected.sources[] |
      .secret.name + "/" + .secret.items[0].key + "=" + .secret.items[0].path' | sort)"
}

# --- Test 9: the kna release ---
test_kna_release() {
  echo "Test: the kvm-node-agent release"

  render "$HYPERVISOR_DIR" 10 || return

  local env='.spec.values.controllerManager.manager.env'
  assert_eq "the release lives in hypervisor-system" "hypervisor-system" \
    "$(val HelmRelease kvm-node-agent '.metadata.namespace')"
  assert_eq "the release takes its chart from the kna OCIRepository" \
    "OCIRepository/flux-system/kvm-node-agent" \
    "$(val HelmRelease kvm-node-agent '.spec.chartRef.kind + "/" + .spec.chartRef.namespace + "/" + .spec.chartRef.name')"
  assert_eq "install and upgrade replace the Migration CRD the chart ships" "CreateReplace CreateReplace" \
    "$(val HelmRelease kvm-node-agent '.spec.install.crds + " " + .spec.upgrade.crds')"
  assert_eq "the agent talks to QEMU" "qemu:///system" \
    "$(val HelmRelease kvm-node-agent "$env.libvirtDefaultUri")"
  assert_eq "NODE_LABEL reads the node name" "spec.nodeName" \
    "$(val HelmRelease kvm-node-agent "$env.nodeLabelFieldPath")"
  assert_eq "the release runs the image this repository builds" "ghcr.io/c5c3/kvm-node-agent" \
    "$(val HelmRelease kvm-node-agent '.spec.values.controllerManager.manager.image.repository')"
  assert_eq "no image tag: the chart's appVersion sha-<pin> names it" "false" \
    "$(val HelmRelease kvm-node-agent '.spec.values.controllerManager.manager.image | has("tag")')"
  assert_eq "the image's own 0:0 decides the user, so the release sets neither id" "null null" \
    "$(val HelmRelease kvm-node-agent '.spec.values.controllerManager.manager.containerSecurityContext |
      ((.runAsUser // "null") | tostring) + " " + ((.runAsGroup // "null") | tostring)')"
  assert_eq "it keeps DAC_OVERRIDE alone, for the PKI directories its init container hands to 42438" \
    "drop=ALL add=DAC_OVERRIDE escalation=false" \
    "$(val HelmRelease kvm-node-agent '.spec.values.controllerManager.manager.containerSecurityContext |
      "drop=" + (.capabilities.drop | join(",")) + " add=" + (.capabilities.add | join(",")) +
      " escalation=" + (.allowPrivilegeEscalation | tostring)')"
  assert_eq "the post-renderer sets NAMESPACE on the DaemonSet's manager" "DaemonSet hypervisor-system" \
    "$(val HelmRelease kvm-node-agent '.spec.postRenderers[0].kustomize.patches[0] |
      .target.kind + " " + (.patch | from_yaml | .spec.template.spec.containers[] | select(.name == "manager") |
      .env[] | select(.name == "NAMESPACE") | .value)')"
}

# --- Test 10: the chart sources ---
test_sources() {
  echo "Test: the two chart sources are pinned by digest"

  render "$HYPERVISOR_DIR" 6 || return

  local source
  for source in openstack-hypervisor-operator kvm-node-agent; do
    assert_eq "${source} addresses its upstream chart" \
      "oci://ghcr.io/cobaltcore-dev/charts/${source}" \
      "$(val OCIRepository "$source" '.spec.url')"
    assert_eq "${source} pins ref.digest to a full sha256 digest" "true" \
      "$(val OCIRepository "$source" '.spec.ref.digest | test("^sha256:[a-f0-9]{64}$")')"
    assert_eq "${source} hands the chart layer over unaltered" "copy" \
      "$(val OCIRepository "$source" '.spec.layerSelector.operation')"
  done
}

# --- Test 11: each chart follows its image's pin ---

# chart_lockstep <short name> <chart> <resolver>
# Compares the chart's ref.tag with the commit <resolver> prints.
chart_lockstep() {
  local short="$1" chart="$2" resolver="$3" sha tag
  sha="$(bash "$resolver")" || sha=""
  if [ -z "$sha" ]; then
    echo "  FAIL: hack/${resolver##*/} printed no pinned commit"
    FAIL=$((FAIL + 1))
    return
  fi
  tag="$(chart_ref "$chart" tag)"
  if [[ "$tag" == *"_sha-${sha:0:7}" ]]; then
    echo "  PASS: $short chart ref $tag matches the pinned commit $sha"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $short chart ref ${tag:-<none>} does not match the pinned commit $sha"
    FAIL=$((FAIL + 1))
  fi
}

test_chart_lockstep() {
  echo "Test: the hvo and kna chart refs name their pinned commits"

  chart_lockstep hvo openstack-hypervisor-operator "$RESOLVE_HVO_COMMIT"
  chart_lockstep kna kvm-node-agent "$RESOLVE_KNA_COMMIT"
}

# --- Test 12: the scripts parse and pass shellcheck ---
test_scripts_lint() {
  echo "Test: host-prepare.sh and libvirtd.sh parse and pass shellcheck"

  render "$HYPERVISOR_DIR" 4 || return

  local tmp key out
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  for key in host-prepare.sh libvirtd.sh; do
    conf_lines "$key" >"$tmp/$key"
    if out="$(bash -n "$tmp/$key" 2>&1)"; then
      echo "  PASS: ${key} parses"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: ${key} does not parse:"
      echo "$out" | head -20
      FAIL=$((FAIL + 1))
    fi
  done

  if ! command -v shellcheck >/dev/null 2>&1; then
    echo "  SKIP: shellcheck not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi
  for key in host-prepare.sh libvirtd.sh; do
    if out="$(shellcheck --severity=warning "$tmp/$key" 2>&1)"; then
      echo "  PASS: shellcheck reports no warnings in ${key}"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: shellcheck reported findings in ${key}:"
      echo "$out" | head -20
      FAIL=$((FAIL + 1))
    fi
  done
}

# --- Test 13: how libvirtd.sh ends ---
#
# libvirtd.sh runs with stubs of systemctl, systemd-run, virtlogd, libvirtd and
# sleep first on PATH, and with its host paths moved under a scratch directory
# (the rig). Like systemd, the stub systemd-run refuses the scope while a
# libvirtd holds it or the scope is failed, and the stub
# `systemctl stop cobaltcore-libvirtd.scope` stops that libvirtd. A stop that
# runs out kills it and leaves the scope failed, unless systemd-run had
# --collect; `systemctl reset-failed cobaltcore-libvirtd.scope` clears that.
# A stub libvirtd binds the socket unless LIBVIRTD_NO_SOCKET is set, exits
# LIBVIRTD_RC at once when that is set, exits LIBVIRTD_TERM_RC on TERM unless
# LIBVIRTD_IGNORE_TERM is set, and exits 3 once <rig>/crash exists.

REAL_SLEEP="$(command -v sleep)"

# libvirtd_rig <rig>
# Writes the stubs to <rig>/bin, the TLS files and the ConfigMap's files below
# <rig>, and libvirtd.sh with its host paths moved below <rig> to
# <rig>/libvirtd.sh. Fails, printing the lines, when a command of the moved
# script still names a path under /run or /etc, which a run would touch on
# this machine; comments and echo messages may.
libvirtd_rig() {
  local rig="$1" stub
  mkdir -p "$rig/bin" "$rig/units" "$rig/pki/CA" "$rig/pki/libvirt/private" "$rig/lab" \
    "$rig/etc-libvirt" "$rig/run/libvirt"
  touch "$rig/pki/CA/cacert.pem" "$rig/pki/libvirt/servercert.pem" \
    "$rig/pki/libvirt/private/serverkey.pem" "$rig/calls.log"
  conf_lines libvirtd.conf >"$rig/lab/libvirtd.conf"
  conf_lines qemu.conf >"$rig/lab/qemu.conf"
  conf_lines libvirtd.sh | sed \
    -e 's#^unit_dir=/run/systemd/system$#unit_dir=@RIG@/units#' \
    -e 's#/etc/pki/#@RIG@/pki/#g' \
    -e 's#/etc/libvirt-lab/#@RIG@/lab/#g' \
    -e 's#/etc/libvirt/#@RIG@/etc-libvirt/#g' \
    -e 's#/run/libvirt/#@RIG@/run/libvirt/#g' \
    -e 's#/run/libvirtd\.pid#@RIG@/run/libvirtd.pid#g' >"$rig/libvirtd.sh.in"
  if grep -nE '(^|[^@])/(run|etc)/' "$rig/libvirtd.sh.in" | grep -vE '^[0-9]+:[[:space:]]*(#|echo )'; then
    return 1
  fi
  sed "s#@RIG@#${rig}#g" "$rig/libvirtd.sh.in" >"$rig/libvirtd.sh"

  cat >"$rig/bin/systemctl" <<'STUB'
#!/bin/bash
echo "systemctl $*" >>"$RIG/calls.log"
case "$*" in
  "stop cobaltcore-libvirtd.scope")
    [[ -f "$RIG/scope.pid" ]] || exit 5
    pid="$(cat "$RIG/scope.pid")"
    rm -f "$RIG/scope.pid"
    kill -TERM "$pid" 2>/dev/null || exit 0
    for _ in $(seq 100); do
      kill -0 "$pid" 2>/dev/null || exit 0
      "$REAL_SLEEP" 0.05
    done
    kill -KILL "$pid" 2>/dev/null
    [[ -f "$RIG/scope.collect" ]] || touch "$RIG/scope.failed"
    ;;
  "reset-failed cobaltcore-libvirtd.scope") rm -f "$RIG/scope.failed" ;;
  daemon-reload) exit "${SYSTEMCTL_RELOAD_RC:-0}" ;;
esac
exit 0
STUB
  cat >"$rig/bin/systemd-run" <<'STUB'
#!/bin/bash
echo "systemd-run $*" >>"$RIG/calls.log"
if [[ -f "$RIG/scope.failed" ]] ||
  { [[ -f "$RIG/scope.pid" ]] && kill -0 "$(cat "$RIG/scope.pid")" 2>/dev/null; }; then
  echo "Failed to start transient scope unit: Unit cobaltcore-libvirtd.scope was already loaded or has a fragment file." >&2
  exit 1
fi
rm -f "$RIG/scope.collect"
while [[ "$1" == --* ]]; do
  [[ "$1" != --collect ]] || touch "$RIG/scope.collect"
  shift
done
echo "$$" >"$RIG/scope.pid"
exec "$@"
STUB
  cat >"$rig/bin/libvirtd" <<'STUB'
#!/bin/bash
trap 'echo "libvirtd $$ TERM" >>"$RIG/calls.log"; [[ -n "${LIBVIRTD_IGNORE_TERM:-}" ]] || exit "${LIBVIRTD_TERM_RC:-0}"' TERM
echo "libvirtd $$ start" >>"$RIG/calls.log"
if [[ -n "${LIBVIRTD_RC:-}" ]]; then
  exit "$LIBVIRTD_RC"
fi
if [[ -z "${LIBVIRTD_NO_SOCKET:-}" ]]; then
  rm -f "$RIG/run/libvirt/libvirt-sock"
  python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' \
    "$RIG/run/libvirt/libvirt-sock"
fi
for _ in $(seq 600); do
  [[ -e "$RIG/crash" ]] && exit 3
  "$REAL_SLEEP" 0.05
done
exit 3
STUB
  printf '#!/bin/bash\nexit 0\n' >"$rig/bin/virtlogd"
  printf '#!/bin/bash\n"$REAL_SLEEP" 0.01\n' >"$rig/bin/sleep"
  for stub in systemctl systemd-run libvirtd virtlogd sleep; do
    chmod +x "$rig/bin/$stub"
  done
}

# rig_start <rig> [VAR=value...] <command...>
# Starts <command> in the background with the rig's stubs first on PATH and
# its variables set, its output appended to <rig>/out. RIG_PID is its PID: env
# execs the command, so a signal sent there reaches it.
rig_start() {
  local rig="$1"
  shift
  env PATH="$rig/bin:$PATH" RIG="$rig" REAL_SLEEP="$REAL_SLEEP" HOST_IP=192.0.2.10 "$@" \
    >>"$rig/out" 2>&1 &
  RIG_PID=$!
}

# await <seconds> <command...>
# True as soon as <command> succeeds, false after <seconds>.
await() {
  local tries=$(($1 * 20))
  shift
  while ((tries-- > 0)); do
    "$@" && return 0
    "$REAL_SLEEP" 0.05
  done
  return 1
}

not_running() {
  ! kill -0 "$1" 2>/dev/null
}

# exit_status <pid>
# The exit status of the background job <pid>, which is killed when it is
# still running after 15 seconds.
exit_status() {
  if ! await 15 not_running "$1"; then
    kill -KILL "$1" 2>/dev/null
  fi
  wait "$1"
}

# unit_files <rig> — the host units left in the rig, one per line.
unit_files() {
  ls -A "$1/units"
}

test_libvirtd_exits() {
  echo "Test: libvirtd.sh removes the host units and stops libvirtd however it ends"

  render "$HYPERVISOR_DIR" 25 || return
  if ! command -v python3 >/dev/null 2>&1; then
    echo "  SKIP: python3 not installed, no stub libvirtd can bind a socket (25 checks skipped)"
    SKIP=$((SKIP + 25))
    return
  fi

  local tmp rig pid rc old new calls
  tmp="$(mktemp -d)"
  # Every stub libvirtd still running exits once its crash file exists.
  trap 'for rig in "$tmp"/*/; do touch "${rig}crash"; done; "$REAL_SLEEP" 0.2; rm -rf "$tmp"' RETURN

  # A libvirtd an earlier container left in the scope, killed after its grace
  # period, holds the scope and the socket. The next start stops it, and its
  # units name the libvirtd that runs; TERM removes them and passes
  # libvirtd's exit status through.
  rig="$tmp/predecessor"
  if ! libvirtd_rig "$rig"; then
    echo "  FAIL: libvirtd.sh names a host path the rig does not move (above)"
    FAIL=$((FAIL + 25))
    return
  fi
  rig_start "$rig" systemd-run --scope --unit=cobaltcore-libvirtd libvirtd --listen
  await 10 test -S "$rig/run/libvirt/libvirt-sock"
  old="$(cat "$rig/scope.pid")"
  rig_start "$rig" LIBVIRTD_TERM_RC=7 bash "$rig/libvirtd.sh"
  pid=$RIG_PID
  await 10 grep -qx 'systemctl restart libvirtd.service' "$rig/calls.log"
  new="$(cat "$rig/scope.pid" 2>/dev/null)"
  assert_contains "the earlier libvirtd is stopped" "$(cat "$rig/calls.log")" "libvirtd ${old} TERM"
  assert_eq "a new libvirtd holds the scope" "true" \
    "$([[ -n "$new" && "$new" != "$old" ]] && kill -0 "$new" 2>/dev/null && echo true || echo false)"
  assert_eq "both units name the new libvirtd" "2" \
    "$(grep -l "PID ${new:-none}\$" "$rig/units/libvirtd.service" "$rig/units/virt-admin-server-update-tls.service" 2>/dev/null | grep -c .)"
  kill -TERM "$pid"
  rc=0
  exit_status "$pid" || rc=$?
  assert_eq "TERM exits with libvirtd's status" "7" "$rc"
  assert_eq "and removes both units" "" "$(unit_files "$rig")"

  # The earlier libvirtd ignores TERM, in a scope started without --collect:
  # the stop runs out, and systemd kills it and keeps the scope loaded as
  # failed. The next libvirtd still gets the scope.
  rig="$tmp/failed-scope"
  libvirtd_rig "$rig" >/dev/null
  rig_start "$rig" LIBVIRTD_IGNORE_TERM=1 systemd-run --scope --unit=cobaltcore-libvirtd libvirtd --listen
  await 10 test -S "$rig/run/libvirt/libvirt-sock"
  old="$(cat "$rig/scope.pid")"
  rig_start "$rig" bash "$rig/libvirtd.sh"
  pid=$RIG_PID
  await 15 grep -qx 'systemctl restart libvirtd.service' "$rig/calls.log"
  new="$(cat "$rig/scope.pid" 2>/dev/null)"
  assert_eq "a scope a stop left failed does not block the next libvirtd" "true" \
    "$([[ -n "$new" && "$new" != "$old" ]] && kill -0 "$new" 2>/dev/null && echo true || echo false)"
  touch "$rig/crash"
  exit_status "$pid" || true

  # The script's own libvirtd ignores TERM, and its socket never appears: the
  # stop at the end runs out, and the scope is unloaded rather than left
  # failed on the host.
  rig="$tmp/collect"
  libvirtd_rig "$rig" >/dev/null
  rig_start "$rig" LIBVIRTD_IGNORE_TERM=1 LIBVIRTD_NO_SOCKET=1 bash "$rig/libvirtd.sh"
  pid=$RIG_PID
  rc=0
  exit_status "$pid" || rc=$?
  assert_eq "a libvirtd that ignores TERM still ends the script with 1" "1" "$rc"
  assert_eq "and its scope is not left failed" "false" \
    "$([[ -e "$rig/scope.failed" ]] && echo true || echo false)"

  # The socket never appears: exit 1, no unit is written, and the libvirtd in
  # the scope is stopped.
  rig="$tmp/no-socket"
  libvirtd_rig "$rig" >/dev/null
  rig_start "$rig" LIBVIRTD_NO_SOCKET=1 bash "$rig/libvirtd.sh"
  pid=$RIG_PID
  rc=0
  exit_status "$pid" || rc=$?
  calls="$(cat "$rig/calls.log")"
  assert_eq "a socket that never appears exits 1" "1" "$rc"
  assert_contains "with the timeout message" "$(cat "$rig/out")" \
    "/run/libvirt/libvirt-sock did not appear within 60s"
  assert_not_contains "no unit is started" "$calls" "systemctl restart libvirtd.service"
  assert_eq "no unit is left" "" "$(unit_files "$rig")"
  assert_eq "the libvirtd in the scope is stopped" "1" "$(grep -cE '^libvirtd [0-9]+ TERM$' <<<"$calls")"

  # libvirtd dies during the wait: its status ends the script.
  rig="$tmp/dies"
  libvirtd_rig "$rig" >/dev/null
  rig_start "$rig" LIBVIRTD_RC=5 LIBVIRTD_NO_SOCKET=1 bash "$rig/libvirtd.sh"
  pid=$RIG_PID
  rc=0
  exit_status "$pid" || rc=$?
  assert_eq "a libvirtd that dies during the wait ends the script with its status" "5" "$rc"
  assert_not_contains "before any unit is started" "$(cat "$rig/calls.log")" "systemctl restart libvirtd.service"
  assert_eq "and no unit is left" "" "$(unit_files "$rig")"

  # libvirtd exits on its own once the units are written.
  rig="$tmp/crash"
  libvirtd_rig "$rig" >/dev/null
  rig_start "$rig" bash "$rig/libvirtd.sh"
  pid=$RIG_PID
  await 10 grep -qx 'systemctl restart libvirtd.service' "$rig/calls.log"
  assert_eq "both units are written" \
    "$(printf '%s\n' libvirtd.service virt-admin-server-update-tls.service)" "$(unit_files "$rig")"
  touch "$rig/crash"
  rc=0
  exit_status "$pid" || rc=$?
  assert_eq "a libvirtd that exits on its own ends the script with its status" "3" "$rc"
  assert_eq "and the units go with it" "" "$(unit_files "$rig")"

  # A step that fails under set -e: the units go, and so does libvirtd.
  rig="$tmp/reload"
  libvirtd_rig "$rig" >/dev/null
  rig_start "$rig" SYSTEMCTL_RELOAD_RC=1 bash "$rig/libvirtd.sh"
  pid=$RIG_PID
  rc=0
  exit_status "$pid" || rc=$?
  calls="$(cat "$rig/calls.log")"
  assert_eq "a failed daemon-reload exits 1" "1" "$rc"
  assert_eq "and leaves no unit" "" "$(unit_files "$rig")"
  assert_eq "and stops the libvirtd in the scope" "1" "$(grep -cE '^libvirtd [0-9]+ TERM$' <<<"$calls")"

  # TERM while the TLS files are missing: an earlier pod's units go, nothing
  # starts, exit 0.
  rig="$tmp/early-term"
  libvirtd_rig "$rig" >/dev/null
  rm "$rig/pki/CA/cacert.pem"
  touch "$rig/units/libvirtd.service" "$rig/units/virt-admin-server-update-tls.service"
  rig_start "$rig" bash "$rig/libvirtd.sh"
  pid=$RIG_PID
  await 10 grep -q 'waiting for the TLS files' "$rig/out"
  kill -TERM "$pid"
  rc=0
  exit_status "$pid" || rc=$?
  assert_eq "TERM before libvirtd starts exits 0" "0" "$rc"
  assert_eq "and removes the units an earlier pod left" "" "$(unit_files "$rig")"
  assert_not_contains "and starts nothing" "$(cat "$rig/calls.log")" "systemd-run"
}

# --- Test 14: each chart's tag and digest name the same upstream artifact ---
#
# ref.digest takes precedence over ref.tag, so the digest decides which chart
# Flux runs, and Test 11 compares only the tag with the pin. Skips rather than
# fails when the registry cannot be reached: an offline workstation or a
# rate-limited runner must not turn the suite red.

# chart_tag_and_digest_agree <short name> <chart>
chart_tag_and_digest_agree() {
  local short="$1" chart="$2"

  if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: curl or jq not installed, cannot resolve the $short chart (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local tag digest registry=ghcr.io repository="cobaltcore-dev/charts/$chart"
  tag="$(chart_ref "$chart" tag)"
  digest="$(chart_ref "$chart" digest)"

  # GHCR serves public packages to an anonymous pull token.
  local token
  token="$(curl -sS --max-time 20 \
    "https://${registry}/token?service=${registry}&scope=repository:${repository}:pull" \
    2>/dev/null | jq -r '.token // empty')"
  if [ -z "$token" ]; then
    echo "  SKIP: ${registry} unreachable, cannot resolve the $short chart ${tag} (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  local resolved
  resolved="$(curl -sS --max-time 20 -I \
    -H "Authorization: Bearer ${token}" \
    -H "Accept: application/vnd.oci.image.manifest.v1+json,application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.v2+json" \
    "https://${registry}/v2/${repository}/manifests/${tag}" 2>/dev/null |
    tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" { print $2 }' | head -1)"
  if [ -z "$resolved" ]; then
    echo "  SKIP: ${registry} did not resolve the $short chart tag ${tag} (1 check skipped)"
    SKIP=$((SKIP + 1))
    return
  fi

  assert_eq "the $short ref.tag ${tag} resolves to the pinned ref.digest" "$digest" "$resolved"
}

test_chart_tag_and_digest_agree_upstream() {
  echo "Test: each chart's ref.tag and ref.digest resolve to the same upstream artifact"

  chart_tag_and_digest_agree hvo openstack-hypervisor-operator
  chart_tag_and_digest_agree kna kvm-node-agent
}

# --- Run ---
test_files_spdx_and_resources
test_fixtures_render
test_hypervisor_render_objects
test_libvirt_daemonset
test_libvirt_config
test_scripts
test_ca_and_compute
test_hvo_release
test_kna_release
test_sources
test_chart_lockstep
test_scripts_lint
test_libvirtd_exits
test_chart_tag_and_digest_agree_upstream

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
