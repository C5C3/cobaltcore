#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the lab hypervisors under deploy/lab/metal-stack/hypervisor,
# deploy/lab/metal-stack/migration-ports and
# deploy/lab/metal-stack/hypervisor-fixtures, applied by hand on the lab:
#   1. The twelve files exist with SPDX headers, and each kustomization
#      lists exactly its resources.
#   2. The fixtures render is the kind fixtures without the volume type, the
#      network and the subnet.
#   3. The hypervisor render is the thirteen objects of the two operators,
#      the libvirt DaemonSet, the migration port reservation, the CA, the
#      compute CRs and the chart sources, and the namespace carries the
#      Gardener opt-out label and the chaos-mesh.org/inject annotation.
#   4. The libvirt DaemonSet runs in the host's namespaces, OnDelete, with the
#      image pinned by its keeper tag and digest and pulled IfNotPresent, the
#      hostPaths and the mount propagation libvirtd and nova-compute share,
#      and without a liveness probe. Its third container, ceph-secret, runs
#      unprivileged without a capability, has no probe, and mounts the
#      optional Secret ceph-client-cinder without a subPath.
#   5. libvirtd.conf and qemu.conf carry every key and value the lab needs.
#   6. libvirtd.sh starts libvirtd in a host scope, writes both host units and
#      starts no virtlogd, ceph-secret.sh passes the key to virsh in a file,
#      and the scripts carry their log messages.
#   7. The CA, its Issuer and the three compute CRs carry their fields: the
#      metadata agent and the pool name no resources, and the pool names the
#      CPU model Nova's live-migration pre-check accepts; NovaCompute has no
#      extraConfig.
#   8. The hvo release feeds the auth Secret into the six chart values, sets
#      the lab values and a fullnameOverride that keeps every object name
#      within 63 characters, and its post-renderer sets OS_INTERFACE to
#      internal and the pull policy on the manager and nothing else on the
#      pod: no host alias, no volume, and no gateway hostname or lab service
#      address anywhere in the render. A second post-renderer patch appends
#      --default-high-availability=false to the manager's arguments, and the
#      values set no argument list. The ServiceMonitor and the
#      PrometheusRules are on, the dashboards and the custom-resource metrics
#      off, and a third patch gives the ServiceMonitor's one endpoint the pod's
#      ServiceAccount token file. A fourth patch replaces the version label of
#      both PrometheusRules with the chart's ref.tag, a value the API server
#      accepts, where the version helm-controller renders from ref.tag and
#      ref.digest, with its '+', is not.
#   9. The kna release runs the image ghcr.io/c5c3/kvm-node-agent without a
#      tag, sets the libvirt URI, the node label field path, DAC_OVERRIDE
#      alone without a runAsUser or runAsGroup, NAMESPACE, and the pull
#      policy Always on the manager.
#  10. Both chart sources are pinned by digest.
#  11. Each chart tag names the commit its image's resolver prints: the hvo
#      tag hack/ci-resolve-hvo-commit.sh's, the kna tag
#      hack/ci-resolve-kna-commit.sh's.
#  12. The three scripts parse, and pass shellcheck when it is on PATH.
#  13. libvirtd.sh, run against stubs, removes the host units and stops
#      libvirtd however it ends, starts no virtlogd, stops a libvirtd an
#      earlier container left in the scope before it starts its own, and
#      leaves no failed scope behind when a stop runs out.
#  14. Each chart's tag and digest resolve to the same upstream artifact
#      (one SKIP per chart when ghcr.io cannot be reached).
#  15. The migration port reservation renders on its own as the namespace,
#      annotated chaos-mesh.org/inject: enabled, and one DaemonSet in the
#      host's network and PID namespaces, with a privileged init container,
#      an unprivileged second container and the node probe's image in both.
#      Its range is the migration range of the node port check, and its
#      script, run against a directory, adds the range to the reserved
#      ports, keeps what is there, names the sockets that hold a port of the
#      range, and fails without the sysctl file.
#  16. hvo's pinned chart, rendered with the release's values, gives both
#      PrometheusRules a version label the API server rejects, and after the
#      release's post-renderer patches both carry ref.tag and no label value
#      is rejected (SKIP when ghcr.io cannot be reached).
#  17. ceph-secret.sh, run against a stub virsh, waits for the socket and the
#      key, defines the libvirt secret once, a private and ephemeral ceph
#      secret, and sets its value from the key file, sets it again when the
#      key changes, defines it again when libvirtd lost it, undefines it when
#      the key file goes or is empty, retries a failed set, a failed undefine
#      and a libvirtd that is down, waits 10 seconds between two passes,
#      exits 0 on TERM, and never puts the key on a command line or in its
#      log.
#
# Checks 2 to 5, 7 to 10, 12, 13, 15, 16 and 17 are counted as SKIP when
# kustomize or yq is not on PATH, 16 also without helm and 17 also without
# python3. A failing kustomize build counts them as FAIL and prints the
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
MIGRATION_PORTS_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/migration-ports"
FIXTURES_DIR="$PROJECT_ROOT/deploy/lab/metal-stack/hypervisor-fixtures"
PROBE_FILE="$PROJECT_ROOT/deploy/lab/metal-stack/probe/node-probe.yaml"
NODE_PORTS_SH="$PROJECT_ROOT/hack/lab-node-ports.sh"
CONFIGMAP_FILE="$HYPERVISOR_DIR/libvirt-configmap.yaml"
SOURCES_FILE="$HYPERVISOR_DIR/sources.yaml"
RESOLVE_HVO_COMMIT="$PROJECT_ROOT/hack/ci-resolve-hvo-commit.sh"
RESOLVE_KNA_COMMIT="$PROJECT_ROOT/hack/ci-resolve-kna-commit.sh"

HYPERVISOR_FILES="libvirt-ca.yaml
libvirt-configmap.yaml
libvirt-daemonset.yaml
compute.yaml
sources.yaml
hvo-release.yaml
kna-release.yaml"

MIGRATION_PORTS_FILES="namespace.yaml
reservation-daemonset.yaml"

AUTH_SECRET="controlplane-nova-hypervisor-operator-auth"

# A label value the API server accepts: empty, or at most 63 alphanumerics,
# '-', '_' and '.', alphanumeric at both ends.
LABEL_VALUE_RE='^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$'

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

  local files=("$FIXTURES_DIR/kustomization.yaml" "$HYPERVISOR_DIR/kustomization.yaml"
    "$MIGRATION_PORTS_DIR/kustomization.yaml") name f
  while IFS= read -r name; do
    files+=("$HYPERVISOR_DIR/$name")
  done <<<"$HYPERVISOR_FILES"
  while IFS= read -r name; do
    files+=("$MIGRATION_PORTS_DIR/$name")
  done <<<"$MIGRATION_PORTS_FILES"
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
    assert_eq "the hypervisor kustomization lists the migration ports and its seven manifests alone" \
      "../migration-ports"$'\n'"$HYPERVISOR_FILES" "$(resource_entries "$HYPERVISOR_DIR/kustomization.yaml")"
  else
    echo "  FAIL: no hypervisor kustomization.yaml whose resources could be read"
    FAIL=$((FAIL + 1))
  fi
  if [[ -f "$MIGRATION_PORTS_DIR/kustomization.yaml" ]]; then
    assert_eq "the migration ports kustomization lists its two manifests alone" \
      "$MIGRATION_PORTS_FILES" "$(resource_entries "$MIGRATION_PORTS_DIR/kustomization.yaml")"
  else
    echo "  FAIL: no migration ports kustomization.yaml whose resources could be read"
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

  render "$HYPERVISOR_DIR" 4 || return

  assert_eq "the render is the thirteen objects of the lab hypervisors" \
    "$(printf '%s\n' \
      Certificate/libvirt-migration-ca \
      ConfigMap/libvirt-lab \
      DaemonSet/libvirt \
      DaemonSet/migration-port-reservation \
      HelmRelease/kvm-node-agent \
      HelmRelease/openstack-hypervisor-operator \
      Issuer/nova-hypervisor-agents-ca-issuer \
      Namespace/hypervisor-system \
      NeutronMetadataAgent/lab-metadata-agent \
      NovaCompute/lab \
      OCIRepository/kvm-node-agent \
      OCIRepository/openstack-hypervisor-operator \
      OVNChassis/lab-chassis)" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(. != null) | .kind + "/" + .metadata.name' - | sort)"
  assert_eq "the namespaced objects live in openstack, hypervisor-system and flux-system" \
    "$(printf '%s\n' flux-system hypervisor-system openstack)" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(. != null and .kind != "Namespace") | .metadata.namespace' - | sort -u)"
  assert_eq "hypervisor-system opts out of Gardener's apiserver-proxy injection" "disable" \
    "$(val Namespace hypervisor-system '.metadata.labels["apiserver-proxy.networking.gardener.cloud/inject"]')"
  assert_eq "hypervisor-system is selectable for Chaos Mesh, like every namespace of the lab" "enabled" \
    "$(val Namespace hypervisor-system '.metadata.annotations["chaos-mesh.org/inject"]')"
}

# --- Test 4: the libvirt DaemonSet ---
test_libvirt_daemonset() {
  echo "Test: the libvirt DaemonSet"

  render "$HYPERVISOR_DIR" 31 || return

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

  # The keeper tag <libvirt-package-version>-r<N> of
  # hack/ci-tag-libvirt-keeper.sh, pinned by digest, on all containers. The
  # version shape is the script's version_shape without its anchors.
  local images shape pinned
  shape="$(sed -n "s/^version_shape='^\(.*\)\$'$/\1/p" "$PROJECT_ROOT/hack/ci-tag-libvirt-keeper.sh")"
  pinned="^ghcr\\.io/c5c3/libvirt:${shape}-r[0-9]+@sha256:[0-9a-f]{64}\$"
  images="$(containers '.image')"
  if [[ -n "$shape" && "$(sed -n 1p <<<"$images")" =~ $pinned && "$(grep -c . <<<"$images")" -eq 3 &&
    "$(sort -u <<<"$images" | grep -c .)" -eq 1 ]]; then
    echo "  PASS: all three containers run one ghcr.io/c5c3/libvirt:<keeper tag>@sha256:<digest>"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the containers do not run one image matching '$pinned':"
    sed 's/^/    /' <<<"$images"
    FAIL=$((FAIL + 1))
  fi
  assert_eq "all containers pull IfNotPresent" \
    "$(printf '%s\n' host-prepare=IfNotPresent libvirtd=IfNotPresent ceph-secret=IfNotPresent)" \
    "$(containers '.name + "=" + .imagePullPolicy')"
  assert_eq "host-prepare and libvirtd are privileged, ceph-secret is not" \
    "$(printf '%s\n' host-prepare=true libvirtd=true ceph-secret=false)" \
    "$(containers '.name + "=" + (.securityContext.privileged | tostring)')"
  assert_eq "all containers run as uid 0" \
    "$(printf '%s\n' host-prepare=0 libvirtd=0 ceph-secret=0)" \
    "$(containers '.name + "=" + (.securityContext.runAsUser | tostring)')"
  # Root for the key file, root-owned with mode 0400, and the socket of the
  # group 108; nothing else.
  assert_eq "ceph-secret drops every capability and forbids privilege escalation" \
    "drop=ALL escalation=false" \
    "$(containers 'select(.name == "ceph-secret") | "drop=" + (.securityContext.capabilities.drop | join(","))
      + " escalation=" + (.securityContext.allowPrivilegeEscalation | tostring)')"
  assert_eq "ceph-secret requests 10m and 16Mi and is limited to 64Mi" "10m 16Mi 64Mi" \
    "$(containers 'select(.name == "ceph-secret") |
      .resources.requests.cpu + " " + .resources.requests.memory + " " + .resources.limits.memory')"
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
  assert_eq "the libvirtd container runs libvirtd.sh" "/bin/bash /etc/libvirt-lab/libvirtd.sh" \
    "$(val DaemonSet libvirt "$pod.containers[0].command | join(\" \")")"
  assert_eq "readiness asks libvirtd for its version" "virsh -c qemu:///system version" \
    "$(val DaemonSet libvirt "$pod.containers[0].readinessProbe.exec.command | join(\" \")")"
  assert_eq "no container has a liveness probe" "" \
    "$(containers 'select(has("livenessProbe")) | .name')"
  # A lab without the Secret keeps its pods Ready.
  assert_eq "only libvirtd has a readiness probe" "libvirtd" \
    "$(containers 'select(has("readinessProbe")) | .name')"

  # ceph-secret reads the key from a Secret volume, which the kubelet rewrites
  # on every change; a subPath mount would never see one.
  assert_eq "ceph-secret runs ceph-secret.sh" "/bin/bash /etc/libvirt-lab/ceph-secret.sh" \
    "$(containers 'select(.name == "ceph-secret") | .command | join(" ")')"
  assert_eq "ceph-secret mounts the scripts and the key read-only, the socket directory, no subPath" \
    "$(printf '%s\n' '/etc/ceph-client-cinder readOnly=true subPath=false' \
      '/etc/libvirt-lab readOnly=true subPath=false' '/run/libvirt readOnly=false subPath=false')" \
    "$(containers 'select(.name == "ceph-secret") | .volumeMounts[] | .mountPath
      + " readOnly=" + ((.readOnly // false) | tostring) + " subPath=" + (has("subPath") | tostring)' | sort)"
  local socket_volume key_volume
  socket_volume="$(containers 'select(.name == "libvirtd") | .volumeMounts[] |
    select(.mountPath == "/run/libvirt") | .name')"
  assert_eq "ceph-secret's /run/libvirt is the volume libvirtd opens its socket in" "${socket_volume:-none}" \
    "$(containers 'select(.name == "ceph-secret") | .volumeMounts[] | select(.mountPath == "/run/libvirt") | .name')"
  key_volume="$(containers 'select(.name == "ceph-secret") | .volumeMounts[] |
    select(.mountPath == "/etc/ceph-client-cinder") | .name')"
  assert_eq "the volume at /etc/ceph-client-cinder is the optional Secret ceph-client-cinder, mode 0400" \
    "secret=ceph-client-cinder optional=true defaultMode=256" \
    "$(val DaemonSet libvirt "$pod.volumes[] | select(.name == \"${key_volume:-none}\") |
      \"secret=\" + .secret.secretName + \" optional=\" + (.secret.optional | tostring)
      + \" defaultMode=\" + (.secret.defaultMode | tostring)")"
}

# --- Test 5: libvirtd.conf and qemu.conf ---
test_libvirt_config() {
  echo "Test: the rendered libvirtd.conf and qemu.conf"

  render "$HYPERVISOR_DIR" 15 || return

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
dynamic_ownership = 0
security_driver = "none"
default_tls_x509_cert_dir = "/etc/pki/qemu"
default_tls_x509_verify = 1
stdio_handler = "file"
LINES
}

# --- Test 6: the scripts ---
test_scripts() {
  echo "Test: libvirtd.sh, host-prepare.sh and ceph-secret.sh"

  if [[ ! -f "$CONFIGMAP_FILE" ]]; then
    echo "  FAIL: $CONFIGMAP_FILE does not exist"
    FAIL=$((FAIL + 32))
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
uuid=090e4a3c-6c20-4e74-82dc-1a70382babe8
key_file=/etc/ceph-client-cinder/userKey
<secret ephemeral='yes' private='yes'>
if ! virsh -c qemu:///system "$@" >/dev/null; then
virsh -c qemu:///system secret-dumpxml "${uuid}" >/dev/null 2>&1
virsh_or_retry secret-define "${xml_file}" || continue
virsh_or_retry secret-set-value "${uuid}" --file "${key_file}" || continue
virsh_or_retry secret-undefine "${uuid}" || continue
echo "ceph-secret: libvirt secret ${uuid} for ${usage_name} follows the Secret ${secret_ref}"
echo "ceph-secret: waiting for /run/libvirt/libvirt-sock"
echo "ceph-secret: defined libvirt secret ${uuid} for ${usage_name}"
echo "ceph-secret: set the value of libvirt secret ${uuid} from ${secret_ref} (sha256 ${sha})"
echo "ceph-secret: undefined libvirt secret ${uuid} because ${secret_ref} is gone"
echo "ceph-secret: waiting for the Secret ${secret_ref} (deployed by WITH_CEPH=true); no libvirt secret is defined"
echo "ceph-secret: virsh $1 failed; retrying in ${interval}s"
FIXED
  assert_file_not_contains "libvirt-configmap.yaml starts no virtlogd" "$CONFIGMAP_FILE" '^[^#]*virtlogd'
  # hostPID shows every command line on the node to every process on it.
  assert_file_not_contains "ceph-secret.sh passes the key in a file, never on a command line" \
    "$CONFIGMAP_FILE" '^[^#]*secret-set-value.*--base64'
}

# --- Test 7: the CA and the compute CRs ---
test_ca_and_compute() {
  echo "Test: the libvirt CA and the three compute CRs"

  render "$HYPERVISOR_DIR" 33 || return

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
  assert_eq "the metadata agent runs 2026.1" "2026.1" \
    "$(val NeutronMetadataAgent lab-metadata-agent "$agent.openStackRelease")"
  assert_eq "the metadata agent runs ghcr.io/c5c3/neutron:2026.1" "ghcr.io/c5c3/neutron:2026.1" \
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
  assert_eq "the pool runs the operator's resource default" "null" "$(val NovaCompute lab '.spec.resources')"
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

  render "$HYPERVISOR_DIR" 29 || return

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
  # The ServiceMonitor and the alert rules are inert without a Prometheus
  # Operator. The dashboard is a Perses one, and the custom-resource metrics
  # need a kube-state-metrics the lab does not run.
  assert_eq "the ServiceMonitor and the PrometheusRules are on, the dashboards and the custom-resource metrics off" \
    "true true false false" \
    "$(val HelmRelease "$release" '.spec.values | (.serviceMonitor.enabled | tostring) + " " +
      (.prometheusRules.create | tostring) + " " + (.dashboards.create | tostring) + " " +
      (.customResourceMetrics.create | tostring)')"

  assert_eq "the post-renderer patches the Deployment" "Deployment" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches[0].target.kind')"
  # Patch 0004 lets OS_INTERFACE pick the catalog's internal endpoints, the
  # in-cluster Service URLs over plain HTTP, so the pod needs no alias and no
  # gateway certificate.
  assert_eq "the post-renderer adds one environment variable to the manager, OS_INTERFACE=internal" "OS_INTERFACE=internal" \
    "$(hvo_patch '.spec.template.spec.containers[] | select(.name == "manager") | .env[] | .name + "=" + .value')"
  assert_eq "the manager pulls its image on every start: the tag moves to each main build" "Always" \
    "$(hvo_patch '.spec.template.spec.containers[] | select(.name == "manager") | .imagePullPolicy')"
  assert_eq "the pod gets no host alias" "false" \
    "$(hvo_patch '.spec.template.spec | has("hostAliases")')"
  assert_eq "the pod gets no volume" "false" \
    "$(hvo_patch '.spec.template.spec | has("volumes")')"
  assert_eq "the manager mounts nothing" "false" \
    "$(hvo_patch '.spec.template.spec.containers[] | select(.name == "manager") | has("volumeMounts")')"
  assert_eq "the render names no gateway hostname and no lab service address" "" \
    "$(printf '%s\n' "$RENDERED" | grep -E '127-0-0-1\.nip\.io|10\.248\.')"

  # The chart has no value for patch 0002's flag, so a second patch appends
  # it and the chart's own argument list stays as it renders.
  assert_eq "the post-renderer holds four patches" "4" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches | length')"
  assert_eq "the second patch targets the Deployment" "Deployment" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches[1].target.kind')"
  assert_eq "the second patch appends --default-high-availability=false to the manager's arguments" \
    "1 add /spec/template/spec/containers/0/args/- --default-high-availability=false" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches[1].patch | from_yaml |
      (length | tostring) + " " + .[0].op + " " + .[0].path + " " + .[0].value')"
  assert_eq "the values set no argument list" "false" \
    "$(val HelmRelease "$release" '.spec.values.controllerManager.manager | has("args")')"

  # hvo serves its metrics behind an authentication filter, and the chart's
  # ServiceMonitor sends no token.
  assert_eq "the third patch targets the ServiceMonitor" "ServiceMonitor" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches[2].target.kind')"
  assert_eq "the third patch adds the ServiceAccount token file to the first endpoint" \
    "1 add /spec/endpoints/0/bearerTokenFile /var/run/secrets/kubernetes.io/serviceaccount/token" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches[2].patch | from_yaml |
      (length | tostring) + " " + .[0].op + " " + .[0].path + " " + .[0].value')"

  # The chart writes its version into the version label of its
  # PrometheusRules. helm-controller sets the build metadata of a chart
  # pinned by digest to the digest's first 12 characters, so the label reads
  # <version>+<12 characters>, and a '+' is no label value.
  local tag digest rendered
  tag="$(chart_ref openstack-hypervisor-operator tag)"
  digest="$(chart_ref openstack-hypervisor-operator digest)"
  digest="${digest#*:}"
  rendered="${tag%%_*}+${digest:0:12}"
  assert_eq "the fourth patch targets the PrometheusRules" "PrometheusRule" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches[3].target.kind')"
  assert_eq "the fourth patch replaces app.kubernetes.io/version with the chart's ref.tag" \
    "1 replace /metadata/labels/app.kubernetes.io~1version ${tag}" \
    "$(val HelmRelease "$release" '.spec.postRenderers[0].kustomize.patches[3].patch | from_yaml |
      (length | tostring) + " " + .[0].op + " " + .[0].path + " " + .[0].value')"
  assert_eq "its value is a label value the API server accepts" "true" \
    "$(val HelmRelease "$release" ".spec.postRenderers[0].kustomize.patches[3].patch | from_yaml |
      .[0].value | test(\"${LABEL_VALUE_RE}\")")"
  assert_eq "and the version helm-controller renders from ref.tag and ref.digest, ${rendered}, is not one" "false" \
    "$(yq -n "\"${rendered}\" | test(\"${LABEL_VALUE_RE}\")")"
}

# --- Test 9: the kna release ---
test_kna_release() {
  echo "Test: the kvm-node-agent release"

  render "$HYPERVISOR_DIR" 11 || return

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
  # The chart has no value for the pull policy, and build-images.yaml moves
  # the tag sha-<kna-commit> to every main build.
  assert_eq "the post-renderer pulls the manager's image on every pod start" "DaemonSet Always" \
    "$(val HelmRelease kvm-node-agent '.spec.postRenderers[0].kustomize.patches[0] |
      .target.kind + " " + (.patch | from_yaml | .spec.template.spec.containers[] | select(.name == "manager") |
      .imagePullPolicy)')"
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
  echo "Test: host-prepare.sh, libvirtd.sh and ceph-secret.sh parse and pass shellcheck"

  render "$HYPERVISOR_DIR" 6 || return

  local tmp key out
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  for key in host-prepare.sh libvirtd.sh ceph-secret.sh; do
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
    echo "  SKIP: shellcheck not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi
  for key in host-prepare.sh libvirtd.sh ceph-secret.sh; do
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
# (the rig). The stub virtlogd only records its call. Like systemd, the stub
# systemd-run refuses the scope while a libvirtd holds it or the scope is
# failed, and the stub
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
  printf '#!/bin/bash\necho "virtlogd $*" >>"$RIG/calls.log"\n' >"$rig/bin/virtlogd"
  printf '#!/bin/bash\n"$REAL_SLEEP" 0.01\n' >"$rig/bin/sleep"
  for stub in systemctl systemd-run virtlogd libvirtd sleep; do
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

  render "$HYPERVISOR_DIR" 26 || return
  if ! command -v python3 >/dev/null 2>&1; then
    echo "  SKIP: python3 not installed, no stub libvirtd can bind a socket (26 checks skipped)"
    SKIP=$((SKIP + 26))
    return
  fi

  local tmp rig pid rc old new calls
  tmp="$(mktemp -d)"
  # Every stub libvirtd still running exits once its crash file exists.
  trap 'for rig in "$tmp"/*/; do touch "${rig}crash"; done; "$REAL_SLEEP" 0.2; rm -rf "$tmp"' RETURN

  # A libvirtd an earlier container left in the scope, killed after its grace
  # period, holds the scope and the socket. The next start stops it, starts no
  # virtlogd, and its units name the libvirtd that runs; TERM removes them and
  # passes libvirtd's exit status through.
  rig="$tmp/predecessor"
  if ! libvirtd_rig "$rig"; then
    echo "  FAIL: libvirtd.sh names a host path the rig does not move (above)"
    FAIL=$((FAIL + 26))
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
  assert_eq "libvirtd.sh starts no virtlogd" "0" "$(grep -c '^virtlogd' "$rig/calls.log")"
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

# --- Test 15: the migration port reservation ---

# reservation_script
# The script of the reservation's init container, as rendered. Not through
# val, which keeps the first line alone.
reservation_script() {
  printf '%s\n' "$RENDERED" |
    yq -N -r 'select(.kind == "DaemonSet" and .metadata.name == "migration-port-reservation") |
      .spec.template.spec.initContainers[0].args[0]' -
}

# fake_proc <dir> <reserved ports> [<tcp line>...]
# Builds <dir> as a /proc for the reservation script: the sysctl file with
# <reserved ports> and net/tcp with its header and the <tcp line>s.
fake_proc() {
  local dir="$1" reserved="$2"
  shift 2
  mkdir -p "$dir/sys/net/ipv4" "$dir/net"
  printf '%s\n' "$reserved" >"$dir/sys/net/ipv4/ip_local_reserved_ports"
  printf '%s\n' '  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode' \
    "$@" >"$dir/net/tcp"
}

# tcp_line <local port, hex> <inode> [<state, hex>]
# One line of /proc/net/tcp, for an established socket unless <state> says
# otherwise.
tcp_line() {
  printf '   0: 012C800A:%s 02C0299A:01BB %s 00000000:00000000 02:00000A2B 00000000     0        0 %s 2 0000000000000000 20 4 30 10 -1' "$1" "${3:-01}" "$2"
}

test_migration_port_reservation() {
  echo "Test: the migration port reservation"

  render "$MIGRATION_PORTS_DIR" 27 || return

  assert_eq "the render is the namespace and the reservation" \
    "$(printf '%s\n' DaemonSet/migration-port-reservation Namespace/hypervisor-system)" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(. != null) | .kind + "/" + .metadata.name' - | sort)"
  assert_eq "its namespace is selectable for Chaos Mesh" "enabled" \
    "$(val Namespace hypervisor-system '.metadata.annotations["chaos-mesh.org/inject"]')"

  local pod='.spec.template.spec'
  assert_eq "the DaemonSet lives in hypervisor-system" "hypervisor-system" \
    "$(val DaemonSet migration-port-reservation '.metadata.namespace')"
  assert_eq "it selects every node" "null" \
    "$(val DaemonSet migration-port-reservation "$pod.nodeSelector")"
  assert_eq "it uses the host's network, whose reserved ports it sets" "true" \
    "$(val DaemonSet migration-port-reservation "$pod.hostNetwork")"
  assert_eq "it uses the host's PID namespace, to name a port's holder" "true" \
    "$(val DaemonSet migration-port-reservation "$pod.hostPID")"
  assert_eq "it mounts no ServiceAccount token" "false" \
    "$(val DaemonSet migration-port-reservation "$pod.automountServiceAccountToken")"
  assert_eq "the init container reserve is privileged" "reserve=true" \
    "$(val DaemonSet migration-port-reservation \
      "$pod.initContainers[] | .name + \"=\" + (.securityContext.privileged | tostring)")"
  assert_eq "the second container runs as nobody, without a privilege or a capability" \
    "hold user=65534 nonroot=true privileged=false escalation=false drop=ALL" \
    "$(val DaemonSet migration-port-reservation \
      "$pod.containers[] | .name + \" user=\" + (.securityContext.runAsUser | tostring)
        + \" nonroot=\" + (.securityContext.runAsNonRoot | tostring)
        + \" privileged=\" + (.securityContext.privileged // false | tostring)
        + \" escalation=\" + (.securityContext.allowPrivilegeEscalation | tostring)
        + \" drop=\" + (.securityContext.capabilities.drop | join(\",\"))")"

  local probe_image
  probe_image="$(sed -n 's/^[[:space:]]*image: //p' "$PROBE_FILE" | head -n 1)"
  assert_not_empty "the node probe names an image" "$probe_image"
  assert_eq "both containers run the node probe's image" \
    "$(printf '%s\n' "$probe_image" "$probe_image")" \
    "$(printf '%s\n' "$RENDERED" |
      yq -N -r 'select(.kind == "DaemonSet" and .metadata.name == "migration-port-reservation") |
        (.spec.template.spec.initContainers + .spec.template.spec.containers)[] | .image' -)"

  local script range
  script="$(reservation_script)"
  range="$(sed -n 's/^range=//p' <<<"$script")"
  assert_eq "the reserved range is the migration range the node port check defaults to" \
    "$(sed -n 's/^NODE_PORTS_TCP="${NODE_PORTS_TCP:-16514 \(.*\)}"$/\1/p' "$NODE_PORTS_SH")" "$range"

  if ! have dash; then
    echo "  SKIP: dash not installed (15 checks skipped)"
    SKIP=$((SKIP + 15))
    return
  fi

  local tmp out rc knob
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # An empty value, and no socket in the range.
  fake_proc "$tmp/empty" ""
  knob="$tmp/empty/sys/net/ipv4/ip_local_reserved_ports"
  out="$(PROC_ROOT="$tmp/empty" dash -c "$script" 2>&1)"
  rc=$?
  assert_eq "an empty value: the script exits 0" "0" "$rc"
  assert_eq "an empty value: the range is written" "49152-49215" "$(cat "$knob")"
  assert_eq "an empty value: both lines" \
    "$(printf '%s\n' 'reserved: 49152-49215 (before: none)' 'in use: none')" "$out"

  # A value the node already carries is kept, and a second run changes nothing.
  fake_proc "$tmp/kept" "1000-1010"
  knob="$tmp/kept/sys/net/ipv4/ip_local_reserved_ports"
  out="$(PROC_ROOT="$tmp/kept" dash -c "$script" 2>&1)"
  assert_eq "an existing value is kept in front of the range" "1000-1010,49152-49215" "$(cat "$knob")"
  assert_contains "and the line names it" "$out" "reserved: 1000-1010,49152-49215 (before: 1000-1010)"
  out="$(PROC_ROOT="$tmp/kept" dash -c "$script" 2>&1)"
  assert_eq "a second run leaves the value as it is" "1000-1010,49152-49215" "$(cat "$knob")"
  assert_contains "and says it was there" "$out" "(before: 1000-1010,49152-49215)"

  # Sockets: one of the range with an owner, one without, one in TIME_WAIT
  # (state 06), which no file refers to (inode 0) and which still blocks a bind
  # to its port, and one outside the range. The tcp6 table is absent in all of
  # these directories.
  fake_proc "$tmp/held" "" "$(tcp_line C000 4711)" "$(tcp_line C03F 4712)" "$(tcp_line C001 0 06)" \
    "$(tcp_line 8000 4713)"
  mkdir -p "$tmp/held/42/fd" "$tmp/held/43/fd"
  echo calico-node >"$tmp/held/42/comm"
  echo other >"$tmp/held/43/comm"
  ln -s 'socket:[4711]' "$tmp/held/42/fd/9"
  ln -s 'socket:[4713]' "$tmp/held/43/fd/3"
  out="$(PROC_ROOT="$tmp/held" dash -c "$script" 2>&1)"
  rc=$?
  assert_eq "held ports: the script exits 0 without a tcp6 table" "0" "$rc"
  assert_contains "a held port names its process and pid" "$out" "in use: port 49152 by calico-node (pid 42)"
  assert_contains "a socket no process holds has an unknown owner" "$out" "in use: port 49215 by an unknown owner"
  assert_contains "a closing connection without an inode has an unknown owner" "$out" "in use: port 49153 by an unknown owner"
  assert_not_contains "a port outside the range is not named" "$out" "32768"
  assert_not_contains "and no 'in use: none' beside a held port" "$out" "in use: none"

  # No sysctl file: cat fails under set -e, and nothing is written.
  mkdir -p "$tmp/missing/net"
  out="$(PROC_ROOT="$tmp/missing" dash -c "$script" 2>&1)"
  rc=$?
  assert_eq "a missing sysctl file ends the script with cat's status" "1" "$rc"
  if [[ -e "$tmp/missing/sys" ]]; then
    echo "  FAIL: a missing sysctl file is not created"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: a missing sysctl file is not created"
    PASS=$((PASS + 1))
  fi
}

# --- Test 16: hvo's chart through the release's post-renderer ---
#
# Renders the pinned chart with the release's values, the six values of the
# auth Secret stubbed, and runs the post-renderer's patches over it with
# kustomize, as helm-controller does. It pulls the chart by ref.tag, which
# Test 14 ties to ref.digest, and skips like Test 14 when the chart cannot be
# pulled. The first check fails once the chart writes a valid version label
# on its own, and the version-label patch goes then.

# pull_hvo_chart <version> <dir> pulls hvo's chart into <dir>, bounded by
# timeout(1) when it is installed: helm pull has no timeout of its own.
pull_hvo_chart() {
  local chart=oci://ghcr.io/cobaltcore-dev/charts/openstack-hypervisor-operator
  if have timeout; then
    timeout 60 helm pull "$chart" --version "$1" -d "$2" >/dev/null 2>&1
  else
    helm pull "$chart" --version "$1" -d "$2" >/dev/null 2>&1
  fi
}

test_hvo_post_render() {
  echo "Test: hvo's chart, run through the release's post-renderer, carries only valid label values"

  if ! have helm || ! have kustomize || ! have yq; then
    echo "  SKIP: helm, kustomize or yq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local tmp tag out release="$HYPERVISOR_DIR/hvo-release.yaml"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  tag="$(chart_ref openstack-hypervisor-operator tag)"

  # helm pulls the tag <version>_<build> by the version <version>+<build>.
  if ! pull_hvo_chart "${tag//_/+}" "$tmp"; then
    echo "  SKIP: ghcr.io unreachable, cannot pull the hvo chart ${tag} (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  yq '.spec.values as $values | .spec.valuesFrom[] as $from
    ireduce ($values; setpath($from.targetPath | split("."); "stub"))' "$release" >"$tmp/values.yaml"
  mkdir "$tmp/post"
  if ! out="$(helm template openstack-hypervisor-operator "$tmp"/*.tgz -n openstack \
    -f "$tmp/values.yaml" 2>&1 >"$tmp/post/raw.yaml")"; then
    echo "  FAIL: helm template rejects the release's values:"
    printf '%s\n' "$out" | head -20
    FAIL=$((FAIL + 3))
    return
  fi
  yq '{"resources": ["raw.yaml"], "patches": .spec.postRenderers[0].kustomize.patches}' \
    "$release" >"$tmp/post/kustomization.yaml"
  if ! out="$(kustomize build "$tmp/post" 2>&1 >"$tmp/patched.yaml")"; then
    echo "  FAIL: the post-renderer's patches do not apply to the chart:"
    printf '%s\n' "$out" | head -20
    FAIL=$((FAIL + 3))
    return
  fi

  local version='select(.kind == "PrometheusRule") | .metadata.labels["app.kubernetes.io/version"]'
  assert_eq "without the patches, both PrometheusRules carry a version label the API server rejects" "2" \
    "$(yq -N "$version | select(test(\"${LABEL_VALUE_RE}\") | not)" "$tmp/post/raw.yaml" | grep -c .)"
  assert_eq "with them, both carry the ref.tag ${tag}" "2" \
    "$(yq -N "$version | select(. == \"${tag}\")" "$tmp/patched.yaml" | grep -c .)"
  # yq's test stops at a label value that is not a string, one the API server
  # rejects as well, so its error and exit status fail the check.
  assert_eq "and no label value of the render is one the API server rejects" "" \
    "$(yq -N "(.metadata.labels, .spec.template.metadata.labels) | select(.) | .[] |
      select(test(\"${LABEL_VALUE_RE}\") | not)" "$tmp/patched.yaml" 2>&1 || echo "yq failed")"
}

# --- Test 17: ceph-secret.sh follows the key ---
#
# ceph-secret.sh runs with stubs of virsh and sleep first on PATH, and with
# the socket directory, the key directory and /tmp moved under a rig. Both
# stubs append each call to <rig>/calls.log, and the stub sleep waits 0.01 s
# whatever it is asked. The stub virsh answers secret-dumpxml with success
# only while <rig>/defined exists, copies the XML file of secret-define to
# <rig>/defined and removes that file on secret-undefine. It fails every call
# with a line on stderr while <rig>/fail-all exists (libvirtd is down), and
# each call of <subcommand> while <rig>/fail-<subcommand> exists. Every pass
# of the script asks secret-dumpxml once, so the number of those calls counts
# the passes.

CEPH_SECRET_UUID=090e4a3c-6c20-4e74-82dc-1a70382babe8

# ceph_secret_rig <rig>
# Writes the stubs to <rig>/bin and ceph-secret.sh with its paths moved below
# <rig> to <rig>/ceph-secret.sh. Fails, printing the lines, when a command of
# the moved script still names a path under /run, /etc or /tmp; comments and
# echo messages may.
ceph_secret_rig() {
  local rig="$1" stub
  mkdir -p "$rig/bin" "$rig/run/libvirt" "$rig/key" "$rig/tmp"
  touch "$rig/calls.log" "$rig/out"
  conf_lines ceph-secret.sh | sed \
    -e 's#/run/libvirt/#@RIG@/run/libvirt/#g' \
    -e 's#/etc/ceph-client-cinder/#@RIG@/key/#g' \
    -e 's#/tmp/#@RIG@/tmp/#g' >"$rig/ceph-secret.sh.in"
  if grep -nE '(^|[^@])/(run|etc|tmp)/' "$rig/ceph-secret.sh.in" | grep -vE '^[0-9]+:[[:space:]]*(#|echo )'; then
    return 1
  fi
  sed "s#@RIG@#${rig}#g" "$rig/ceph-secret.sh.in" >"$rig/ceph-secret.sh"

  cat >"$rig/bin/virsh" <<'STUB'
#!/bin/bash
echo "virsh $*" >>"$RIG/calls.log"
[[ "$1" != -c ]] || shift 2
if [[ -f "$RIG/fail-all" ]]; then
  echo "error: failed to connect to the hypervisor" >&2
  exit 1
fi
if [[ -f "$RIG/fail-$1" ]]; then
  echo "error: Failed to $1 $2" >&2
  exit 1
fi
case "$1" in
  secret-dumpxml) [[ -f "$RIG/defined" ]] ;;
  secret-define) cp "$2" "$RIG/defined" && echo "Secret $2 created" ;;
  secret-undefine) rm "$RIG/defined" && echo "Secret $2 deleted" ;;
  secret-set-value) echo "Secret value set" ;;
  *) exit 2 ;;
esac
STUB
  printf '#!/bin/bash\necho "sleep $*" >>"$RIG/calls.log"\n"$REAL_SLEEP" 0.01\n' >"$rig/bin/sleep"
  for stub in virsh sleep; do
    chmod +x "$rig/bin/$stub"
  done
}

# put_key <rig> <key>
# Replaces the key file in one rename, as the kubelet does; an empty <key>
# leaves a file of zero bytes.
put_key() {
  printf '%s' "$2" >"$1/key/userKey.new"
  [[ -z "$2" ]] || echo >>"$1/key/userKey.new"
  mv "$1/key/userKey.new" "$1/key/userKey"
}

# key_digest <key> — what the script logs for <key>.
key_digest() {
  printf '%s' "$1" | sha256sum | cut -c1-12
}

# lines <file> <line> — how many lines of <file> are <line>.
lines() {
  grep -cxF -- "$2" "$1"
}

# has_lines <file> <line> <n> — true once <file> holds <line> <n> times.
has_lines() {
  [[ "$(lines "$1" "$2")" -ge "$3" ]]
}

# virsh_calls <rig>
# The virsh calls other than secret-dumpxml, without the URI, one per line.
virsh_calls() {
  grep '^virsh ' "$1/calls.log" | grep -v ' secret-dumpxml ' | sed 's#^virsh -c qemu:///system ##'
}

# secret_state <rig> — "defined" while the stub holds the secret, else
# "undefined".
secret_state() {
  if [[ -f "$1/defined" ]]; then echo defined; else echo undefined; fi
}

# counts <rig> <undefined line> <waiting line>
# How often the output holds each of the two lines.
counts() {
  echo "undefined=$(lines "$1/out" "$2") waiting=$(lines "$1/out" "$3")"
}

# passes <rig> — the passes the script began so far.
passes() {
  grep -c ' secret-dumpxml ' "$1/calls.log"
}

# passes_reached <rig> <n> — true once the script began <n> passes.
passes_reached() {
  [[ "$(passes "$1")" -ge "$2" ]]
}

# await_passes <rig> <n>
# Waits until the script began <n> more passes, so that a pass that would
# call virsh once more has done so.
await_passes() {
  await 10 passes_reached "$1" $(($(passes "$1") + $2))
}

test_ceph_secret_sync() {
  echo "Test: ceph-secret.sh defines the libvirt secret from the key file and follows it"

  render "$HYPERVISOR_DIR" 39 || return
  if ! command -v python3 >/dev/null 2>&1; then
    echo "  SKIP: python3 not installed, no socket can be bound (39 checks skipped)"
    SKIP=$((SKIP + 39))
    return
  fi

  local tmp rig pid="" rc out key1 key2 define set_value undefine mark
  local uuid="$CEPH_SECRET_UUID" secret_ref=openstack/ceph-client-cinder
  local waiting="ceph-secret: waiting for the Secret ${secret_ref} (deployed by WITH_CEPH=true); no libvirt secret is defined"
  local defined_line="ceph-secret: defined libvirt secret ${uuid} for client.cinder"
  local undefined_line="ceph-secret: undefined libvirt secret ${uuid} because ${secret_ref} is gone"
  local set_line="ceph-secret: set the value of libvirt secret ${uuid} from ${secret_ref} (sha256"
  local retry="ceph-secret: virsh secret-set-value failed; retrying in 10s"
  local retry_undefine="ceph-secret: virsh secret-undefine failed; retrying in 10s"
  local retry_define="ceph-secret: virsh secret-define failed; retrying in 10s"
  tmp="$(mktemp -d)"
  trap 'kill -KILL "$pid" 2>/dev/null; rm -rf "$tmp"' RETURN

  rig="$tmp/rig"
  if ! ceph_secret_rig "$rig"; then
    echo "  FAIL: ceph-secret.sh names a host path the rig does not move (above)"
    FAIL=$((FAIL + 39))
    return
  fi
  define="secret-define $rig/tmp/ceph-secret.xml"
  set_value="secret-set-value ${uuid} --file $rig/key/userKey"
  undefine="secret-undefine ${uuid}"
  # Two made-up keys of the shape Ceph writes.
  key1=AQDhK2VnAAAAABAA7q3+8z9Hq1lnO4JmNo2Gkw==
  key2=AQBzM3VnAAAAABAAp1u7XyQv9mN2c6rTqW8eLg==

  # Before libvirtd opens its socket the script waits, and calls no virsh.
  rig_start "$rig" bash "$rig/ceph-secret.sh"
  pid=$RIG_PID
  await 10 grep -q '^ceph-secret: waiting for .*/run/libvirt/libvirt-sock$' "$rig/out"
  "$REAL_SLEEP" 0.3
  assert_eq "without the socket it logs the wait for it once" "1" \
    "$(grep -c '^ceph-secret: waiting for .*/run/libvirt/libvirt-sock$' "$rig/out")"
  assert_eq "and calls no virsh" "" "$(grep '^virsh ' "$rig/calls.log")"

  # (a) The socket, no key file: one waiting line, nothing defined.
  python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' \
    "$rig/run/libvirt/libvirt-sock"
  await 10 grep -qxF "$waiting" "$rig/out"
  await_passes "$rig" 5
  assert_eq "(a) without a key file it logs the waiting line once over five passes" "1" \
    "$(lines "$rig/out" "$waiting")"
  assert_eq "(a) and defines and sets nothing" "" "$(virsh_calls "$rig")"

  # (b) A key: one define, then one set from the file.
  put_key "$rig" "$key1"
  await 10 has_lines "$rig/out" "$set_line $(key_digest "$key1"))" 1
  await_passes "$rig" 3
  assert_eq "(b) a key file leads to one secret-define, then one secret-set-value --file" \
    "$(printf '%s\n' "$define" "$set_value")" "$(virsh_calls "$rig")"
  assert_file_contains_fixed "(b) the secret carries the UUID" "$rig/defined" "<uuid>${uuid}</uuid>"
  assert_file_contains_fixed "(b) it is private" "$rig/defined" "private='yes'"
  assert_file_contains_fixed "(b) it is ephemeral, kept in libvirtd's memory alone" "$rig/defined" "ephemeral='yes'"
  assert_file_contains_fixed "(b) it is a ceph secret" "$rig/defined" "<usage type='ceph'>"
  assert_file_contains_fixed "(b) of client.cinder" "$rig/defined" "<name>client.cinder</name>"
  assert_eq "(b) the defined line is logged once" "1" "$(lines "$rig/out" "$defined_line")"
  assert_eq "(b) the set line carries the key's digest, once" "1" \
    "$(lines "$rig/out" "$set_line $(key_digest "$key1"))")"

  # (c) Another key: one more set, no define.
  put_key "$rig" "$key2"
  await 10 has_lines "$rig/out" "$set_line $(key_digest "$key2"))" 1
  await_passes "$rig" 3
  assert_eq "(c) a changed key leads to one more secret-set-value and no secret-define" \
    "$(printf '%s\n' "$define" "$set_value" "$set_value")" "$(virsh_calls "$rig")"
  assert_eq "(c) with the new digest, once" "1" "$(lines "$rig/out" "$set_line $(key_digest "$key2"))")"

  # (d) A restarted libvirtd lost the secret: defined and set again within
  # two passes.
  rm "$rig/defined"
  await_passes "$rig" 2
  assert_eq "(d) a lost secret is defined and set again within two passes" \
    "$(printf '%s\n' "$define" "$set_value" "$set_value" "$define" "$set_value")" "$(virsh_calls "$rig")"
  assert_eq "(d) with the unchanged key's digest" "2" "$(lines "$rig/out" "$set_line $(key_digest "$key2"))")"

  # (e) The key file goes, and later one of zero bytes: each time one
  # undefine and one waiting line.
  rm "$rig/key/userKey"
  await 10 has_lines "$rig/out" "$undefined_line" 1
  await_passes "$rig" 3
  assert_eq "(e) a removed key file leads to one secret-undefine" \
    "$(printf '%s\n' "$define" "$set_value" "$set_value" "$define" "$set_value" "$undefine")" \
    "$(virsh_calls "$rig")"
  assert_eq "(e) the undefined line is logged once" "1" "$(lines "$rig/out" "$undefined_line")"
  assert_eq "(e) the removed key file is followed by the waiting line, once" "2" "$(lines "$rig/out" "$waiting")"
  put_key "$rig" "$key2"
  await 10 has_lines "$rig/out" "$set_line $(key_digest "$key2"))" 3
  put_key "$rig" ""
  await 10 has_lines "$rig/out" "$undefined_line" 2
  await_passes "$rig" 3
  assert_eq "(e) an empty key file is undefined as well" \
    "$(printf '%s\n' "$define" "$set_value" "$undefine")" "$(virsh_calls "$rig" | tail -n 3)"
  assert_eq "(e) the empty key file is followed by the waiting line, once" "3" "$(lines "$rig/out" "$waiting")"

  # (f) secret-set-value fails until the marker goes: the retry line follows
  # virsh's error, the script runs on, and then one set succeeds.
  touch "$rig/fail-secret-set-value"
  put_key "$rig" "$key1"
  await 10 grep -qxF "$retry" "$rig/out"
  await_passes "$rig" 3
  assert_eq "(f) the retry line follows virsh's error" \
    "$(printf '%s\n' "error: Failed to secret-set-value ${uuid}" "$retry")" \
    "$(grep -B 1 -xF "$retry" "$rig/out" | head -n 2)"
  assert_eq "(f) no set line while the set fails" "1" "$(lines "$rig/out" "$set_line $(key_digest "$key1"))")"
  assert_eq "(f) and the script keeps running" "running" "$(kill -0 "$pid" 2>/dev/null && echo running)"
  rm "$rig/fail-secret-set-value"
  await 10 has_lines "$rig/out" "$set_line $(key_digest "$key1"))" 2
  await_passes "$rig" 3
  assert_eq "(f) once it succeeds, the set line follows, once" "2" \
    "$(lines "$rig/out" "$set_line $(key_digest "$key1"))")"

  # (g) secret-undefine fails until the marker goes: the retry line follows
  # virsh's error, the secret stays and no waiting line is logged, and then
  # the undefined line and the waiting line follow.
  touch "$rig/fail-secret-undefine"
  rm "$rig/key/userKey"
  await 10 grep -qxF "$retry_undefine" "$rig/out"
  await_passes "$rig" 3
  assert_eq "(g) the retry line follows virsh's error" \
    "$(printf '%s\n' "error: Failed to secret-undefine ${uuid}" "$retry_undefine")" \
    "$(grep -B 1 -xF "$retry_undefine" "$rig/out" | head -n 2)"
  assert_eq "(g) while the undefine fails, the secret stays defined" "defined" "$(secret_state "$rig")"
  assert_eq "(g) and neither the undefined line nor the waiting line is logged" "undefined=2 waiting=3" \
    "$(counts "$rig" "$undefined_line" "$waiting")"
  rm "$rig/fail-secret-undefine"
  await 10 has_lines "$rig/out" "$undefined_line" 3
  await_passes "$rig" 3
  assert_eq "(g) once it succeeds, the undefined line and the waiting line follow, once each" \
    "undefined=3 waiting=4" "$(counts "$rig" "$undefined_line" "$waiting")"

  # (h) libvirtd is down when the key comes back: every virsh call fails, the
  # define is retried and nothing is set until libvirtd is back. The script
  # is between passes without a key here, so mark counts every call before
  # the outage.
  touch "$rig/fail-all"
  mark="$(virsh_calls "$rig" | wc -l)"
  put_key "$rig" "$key2"
  await 10 grep -qxF "$retry_define" "$rig/out"
  await_passes "$rig" 3
  assert_eq "(h) the retry line follows virsh's error" \
    "$(printf '%s\n' "error: failed to connect to the hypervisor" "$retry_define")" \
    "$(grep -B 1 -xF "$retry_define" "$rig/out" | head -n 2)"
  assert_eq "(h) while libvirtd is down, secret-define is all it tries" "$define" \
    "$(virsh_calls "$rig" | tail -n "+$((mark + 1))" | sort -u)"
  rm "$rig/fail-all"
  await 10 has_lines "$rig/out" "$set_line $(key_digest "$key2"))" 4
  await_passes "$rig" 3
  assert_eq "(h) once libvirtd is back, the secret is defined and set, once" \
    "$(printf '%s\n' "$define" "$set_value")" "$(virsh_calls "$rig" | tail -n 2)"

  # (i) TERM: exit 0, the secret stays.
  kill -TERM "$pid"
  rc=0
  exit_status "$pid" || rc=$?
  assert_eq "(i) TERM ends the script with status 0" "0" "$rc"
  assert_eq "(i) without undefining the secret" "defined" "$(secret_state "$rig")"

  # (j) The key reaches neither a command line nor the log.
  out="$(cat "$rig/calls.log" "$rig/out")"
  assert_not_contains "(j) the first key occurs in neither calls.log nor the output" "$out" "$key1"
  assert_not_contains "(j) the second key occurs in neither" "$out" "$key2"
  assert_eq "every virsh call names qemu:///system" "" \
    "$(grep '^virsh ' "$rig/calls.log" | grep -v '^virsh -c qemu:///system ')"

  # (k) Pacing: every sleep waits the interval, and exactly one separates two
  # passes, a pass that ended on a failed call included.
  assert_eq "(k) every sleep waits 10 seconds" "" "$(grep '^sleep ' "$rig/calls.log" | grep -vx 'sleep 10')"
  assert_eq "(k) one sleep separates each two passes" "0" \
    "$(awk '/ secret-dumpxml /{ if (seen && n != 1) bad++; seen = 1; n = 0; next }
      /^sleep /{ n++ } END { print bad + 0 }' "$rig/calls.log")"
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
test_migration_port_reservation
test_hvo_post_render
test_ceph_secret_sync

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
