#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the external-cluster teardown of hack/teardown-infra.sh
# (EXTERNAL_CLUSTER=true):
#   1. EXTERNAL_CLUSTER, EXTERNAL_OVERLAY and TEARDOWN_TIMEOUT default and pass
#      through, and the default teardown still deletes the kind cluster without
#      calling kubectl.
#   2. The external teardown never calls kind or docker, deletes in the order
#      that lets every finalizer run while its controller exists (Chaos Mesh
#      before step 0: one CRD scope read, the schedules and workflows, the
#      experiments without the per-pod records, the cluster-scoped kinds, then
#      the overlay render without its Namespace; the OVNCentrals directly after
#      the ControlPlanes, the proving OpenBao instance on DeletePVCs before the
#      infrastructure overlay, a wait for the stack CRs in openstack that are
#      still being reaped before the operators go, which reads every namespaced
#      stack kind in one call, no cluster-scoped or platform kind, and passes
#      the Gateway and the objects nothing reaps), passes --ignore-not-found to
#      every delete, resumes only the suspended Flux objects that installed
#      something, splits the base render into the Gateway pass and the rest,
#      deletes exactly the stack CRDs of a mixed list, chaos-mesh.org included,
#      names no namespace outside the stack's, chaos-mesh and dizzy included,
#      and reads,
#      logs and deletes the cluster-scoped objects whose
#      helm.toolkit.fluxcd.io/namespace label names a stack namespace after the
#      stack namespaces are gone and before the CRDs, reads them again in the
#      final count, and deletes the two cert-manager leader election Leases in
#      kube-system after those objects and before the CRDs. At the end of step
#      2 it reads the HelmRelease kube-system/csi-driver-nfs, then the pods of
#      every namespace and the PersistentVolumes once (no pod mounts an inline
#      nfs.csi.k8s.io volume, no PersistentVolume of that driver is Bound),
#      then deletes the overlay's nfs/, the NetworkPolicy of its
#      nfs/client-policy.yaml and the CSIDriver of the csi-driver-nfs release
#      by its labels, before the first operator is resumed.
#   3. It exits 1 before any delete when the API server does not answer, yq
#      is missing or yq is not mikefarah/yq v4.40.1 or newer, exits 1 when a
#      wait runs out (naming the object, the OVNCentral delete included, after
#      which nothing else is deleted), when the Lease delete is refused
#      (before any CRD is deleted), when
#      a stack CR in openstack that is being deleted or whose owner is gone
#      outlives the wait, or the read of those CRs keeps failing (naming the
#      object or kubectl's error, before any operator is removed), when the
#      CRD scope cannot be read (before any operator is removed), when the
#      proving OpenBao instance cannot be switched to DeletePVCs, when a stack
#      CRD, namespace or chart object is left (naming the chart object), when
#      the read of those chart objects fails or their delete runs out (before
#      any CRD is deleted), when a pod with an inline nfs.csi.k8s.io volume
#      or a Bound PersistentVolume of that driver outlives the wait (naming
#      it, not the pods without one or the other PersistentVolumes), or the
#      read of the pods or the PersistentVolumes keeps failing or cannot be
#      parsed (before any NFS delete and any operator patch), and goes on
#      with the NFS stack once a pod that still mounted a share on the first
#      read is gone on a later one, reads no pod or PersistentVolume without
#      the HelmRelease csi-driver-nfs (a Bound one of nfs.csi.k8s.io then
#      holds nothing) and on a second run, exits 1 when that HelmRelease
#      cannot be read (before any NFS delete and any operator patch), and
#      when the base render, the CRD list of
#      step 8, the final namespace read or the final chart-object read fails,
#      makes no pod read and no NFS delete for an overlay without nfs/, and
#      exits 0 on a second run that finds nothing, without printing kubectl's
#      "No resources found".
#   4. Step 0 removes the lab hypervisors before the ControlPlane: the
#      NovaComputes, metadata agents and OVNChassis, the kvm.cloud.sap
#      objects, the fixtures after disabling their domain and waiting for
#      that, the hypervisor overlay, the maint-<node> objects in kube-system
#      in one delete and the node labels. It makes no call for an overlay
#      without hypervisor/, skips a kind whose CRD is absent, exits 1 with a
#      hint to delete the servers when the NovaCompute delete runs out, exits
#      1 when the domain cannot be read or stays enabled, or the nodes cannot
#      be listed, still deletes the fixtures once their domain is gone, and
#      deletes none of its guarded kinds on a second run.
#   5. Chaos Mesh goes before step 0. A delete of the experiments that runs
#      out exits 1 with kubectl's named object and the finalizer hint, and
#      deletes nothing after it; a CRD scope read that fails exits 1 before
#      any delete, a chaos-mesh/ that does not render exits 1 before its
#      delete and step 0, and an overlay delete that runs out exits 1 before
#      step 0 and the FluxInstance delete. A cluster without chaos-mesh.org
#      CRDs gets no experiment delete but the overlay delete, an overlay
#      without chaos-mesh/ no read and no delete, and a second run exits 0
#      without "No resources found".
#   6. The dizzy stack goes at the end of step 3: its two HelmReleases, then
#      its two HelmRepositories, then the PVCs in dizzy, after the
#      kube-prometheus-stack release and before the PVCs of step 4, and step 7
#      and the final count name the namespace dizzy. An overlay without
#      dizzy/ gets none of the three deletes. A HelmRelease delete that runs
#      out exits 1 with kubectl's line and deletes nothing after it; a second
#      run, on which the Flux kinds are gone, and a cluster deployed without
#      WITH_DIZZY=true, on which the deletes find nothing, exit 0.
#
# main() runs against a recording kubectl stub on a private PATH prefix and the
# real yq; the external-cluster checks are SKIP without yq.
#
# Usage: bash tests/unit/hack/teardown_infra_external_cluster_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
TEARDOWN_SH="$PROJECT_ROOT/hack/teardown-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

STACK_CRDS="certificates.cert-manager.io
controlplanes.c5c3.io
helmreleases.helm.toolkit.fluxcd.io
keystones.keystone.openstack.c5c3.io
images.openstack.k-orc.cloud
fluxinstances.fluxcd.controlplane.io
gateways.gateway.networking.k8s.io
gatewayclasses.gateway.networking.k8s.io
hypervisors.kvm.cloud.sap
evictions.kvm.cloud.sap
migrations.kvm.cloud.sap
networkchaos.chaos-mesh.org
podchaos.chaos-mesh.org
podhttpchaos.chaos-mesh.org
podiochaos.chaos-mesh.org
podnetworkchaos.chaos-mesh.org
remoteclusters.chaos-mesh.org
schedules.chaos-mesh.org
statuschecks.chaos-mesh.org
workflownodes.chaos-mesh.org
workflows.chaos-mesh.org"
# The arguments of step 0's label removal.
HYPERVISOR_LABELS_REMOVED="openstack.c5c3.io/chassis- openstack.c5c3.io/nova-compute-pool- \
nova.openstack.cloud.sap/virt-driver- cobaltcore.cloud.sap/node-hypervisor-lifecycle-"
PLATFORM_CRDS="verticalpodautoscalers.autoscaling.k8s.io
certificates.cert.gardener.cloud
ippools.crd.projectcalico.org"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# kubectl, kind and docker stubs that append their argv to $CALL_LOG. The
# kubectl stub answers from the environment:
#   KUBECTL_VERSION_RC     exit code of `version` (default 0)
#   KUBECTL_SECOND_RUN     non-empty: the stack is gone (no Flux, c5c3 or ovn
#                          CRD, no OpenBao instance, no stack CRD, no chart
#                          object, deletes of CR kinds report a missing
#                          mapping)
#   KUBECTL_DELETE_RC      exit code of every waiting delete (default 0)
#   KUBECTL_OVNCENTRAL_DELETE_RC
#                          exit code of the OVNCentral delete alone (default 0)
#   KUBECTL_LEASE_DELETE_RC
#                          exit code of the Lease delete alone (default 0)
#   KUBECTL_OPENBAO_PATCH_RC
#                          exit code of the OpenBao instance patch (default 0)
#   KUBECTL_CRD_RC         non-empty: `get crd -o name` fails
#   KUBECTL_CRD_SCOPE_RC   non-empty: `get crd -o custom-columns=...` fails
#   KUBECTL_CR_READ_RC     non-empty: the read of the stack CRs in openstack
#                          fails
#   KUBECTL_CR_LEFT        non-empty: that read keeps answering a Keystone of
#                          this name that is being deleted
#   KUBECTL_CR_ORPHANED    non-empty: that read keeps answering a SecretStore of
#                          this name whose ControlPlane is gone
#   KUBECTL_CLUSTER_SCOPED_CR
#                          non-empty: a read that names the cluster-scoped
#                          gatewayclasses answers a GatewayClass being deleted
#   KUBECTL_CRD_LEFT       a stack CRD the final report still finds
#   KUBECTL_NS_LEFT        a namespace the final report still finds
#   KUBECTL_NS_RC          non-empty: the final namespace read fails
#   KUBECTL_RENDER_RC      non-empty: `kustomize` of any directory but
#                          chaos-mesh/ fails
#   BASE_RENDER            file answering that `kustomize` (default:
#                          base-render.yaml)
#   KUBECTL_CHAOS_RENDER_RC
#                          non-empty: `kustomize` of chaos-mesh/ fails; it
#                          answers chaos-render.yaml otherwise
#   KUBECTL_CHAOS_CRD_SCOPE_RC
#                          non-empty: the two-column CRD scope read of the
#                          Chaos Mesh step fails
#   KUBECTL_CHAOS_DELETE_RC
#                          exit code of the delete of the Chaos Mesh
#                          experiments alone, which then times out on a
#                          NetworkChaos (default 0)
#   KUBECTL_CHAOS_OVERLAY_DELETE_RC
#                          non-empty: the `delete -f -` of the chaos-mesh/
#                          render times out on the HelmRelease chaos-mesh
#   KUBECTL_CHAOS_ABSENT   non-empty: no CRD read answers a chaos-mesh.org CRD
#   KUBECTL_ABSENT_CRDS    space-separated step 0 CRDs that `get crd <name>`
#                          reports absent (the six step 0 kinds are present
#                          otherwise, and absent on a second run)
#   KUBECTL_NOVACOMPUTE_DELETE_RC
#                          exit code of the NovaCompute delete alone (default 0)
#   KUBECTL_DOMAIN_GET_RC  exit code of the read of the fixtures domain, which
#                          then times out (default 0)
#   KUBECTL_DOMAIN_NOISE   non-empty: that read first writes an aggregated-API
#                          error to stderr, as kubectl does while the
#                          metrics-server APIService is down
#   KUBECTL_CHART_OBJECTS_RC
#                          non-empty: every read of the cluster-scoped chart
#                          objects fails with Forbidden
#   KUBECTL_CHART_OBJECTS_FINAL_RC
#                          non-empty: that read fails the same way once the
#                          CRDs are deleted, that is, in the final count
#   KUBECTL_CHART_OBJECT_LEFT
#                          a chart object that read still answers after the
#                          chart objects are deleted
#   KUBECTL_CHART_OBJECT_DELETE_RC
#                          exit code of the delete of those objects alone
#                          (default 0)
#   KUBECTL_NFS_POD_LEFT   non-empty: `get pods -A -o json` also answers the
#                          pod openstack/cinder-volume-nfs1-0, which mounts an
#                          inline nfs.csi.k8s.io volume
#   KUBECTL_NFS_POD_READS  a number n: the first n of those reads answer that
#                          pod, the later ones do not (the reads are counted
#                          in pod-reads beside the stub)
#   KUBECTL_PODS_RC        non-empty: that read fails with Forbidden
#   KUBECTL_PODS_TRUNCATED non-empty: that read answers a truncated document
#   KUBECTL_NFS_PV_BOUND   non-empty: `get pv -o json` also answers the
#                          PersistentVolume nfs-health-volumes of driver
#                          nfs.csi.k8s.io, Bound to the claim
#                          nfs-health-probe/nfs-health-volumes
#   KUBECTL_PVS_RC         non-empty: that read fails with Forbidden
#   KUBECTL_NFS_RELEASE_ABSENT
#                          non-empty: the HelmRelease kube-system/csi-driver-nfs
#                          does not exist, as on a cluster deployed without
#                          WITH_NFS=true
#   KUBECTL_NFS_RELEASE_RC non-empty: the read of that HelmRelease fails with
#                          Forbidden
#   KUBECTL_DIZZY_DELETE_RC
#                          exit code of the delete of the dizzy HelmReleases
#                          alone, which then times out on
#                          dizzy-victoria-metrics (default 0)
# The step 0 reads answer two nodes, lab-a and lab-b, and a fixtures domain
# hvo-cc3test that a second run no longer finds. On a second run the named
# deletes of HelmReleases and HelmRepositories fail on the missing kind. The
# read of the HelmRelease kube-system/csi-driver-nfs answers it, and on a
# second run fails on the missing HelmRelease kind. The pod read answers two pods
# without an nfs.csi.k8s.io volume (pods.json), and {"items":[]} on a second
# run. The PersistentVolume read answers a Bound volume of another driver and
# a Released one of nfs.csi.k8s.io (pvs.json), and {"items":[]} on a second
# run. The read of the cluster-scoped
# chart objects answers the three of chart-objects.txt until they are deleted;
# afterwards, and on a second run, it answers nothing and, without
# --ignore-not-found, writes kubectl's "No resources found" to stderr.
# A `delete -f -` records the kinds it received on stdin, and fails the way
# kubectl does when stdin holds no object.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"
  printf '%s\n' "$STACK_CRDS" "$PLATFORM_CRDS" |
    sed 's#^#customresourcedefinition.apiextensions.k8s.io/#' >"$dir/crds.txt"
  printf '%s\n' "$PLATFORM_CRDS" |
    sed 's#^#customresourcedefinition.apiextensions.k8s.io/#' >"$dir/crds-after.txt"
  # The CRDs of crds.txt with their scope and kind, as the custom-columns read
  # prints them. The platform's VerticalPodAutoscaler and Certificate are
  # namespaced, like the stack's.
  cat >"$dir/crd-columns.txt" <<'COLUMNS'
certificates.cert-manager.io                 Namespaced   Certificate
controlplanes.c5c3.io                        Namespaced   ControlPlane
helmreleases.helm.toolkit.fluxcd.io          Namespaced   HelmRelease
keystones.keystone.openstack.c5c3.io         Namespaced   Keystone
images.openstack.k-orc.cloud                 Namespaced   Image
fluxinstances.fluxcd.controlplane.io         Namespaced   FluxInstance
gateways.gateway.networking.k8s.io           Namespaced   Gateway
gatewayclasses.gateway.networking.k8s.io     Cluster      GatewayClass
hypervisors.kvm.cloud.sap                    Cluster      Hypervisor
evictions.kvm.cloud.sap                      Cluster      Eviction
migrations.kvm.cloud.sap                     Namespaced   Migration
networkchaos.chaos-mesh.org                  Namespaced   NetworkChaos
podchaos.chaos-mesh.org                      Namespaced   PodChaos
podhttpchaos.chaos-mesh.org                  Namespaced   PodHttpChaos
podiochaos.chaos-mesh.org                    Namespaced   PodIOChaos
podnetworkchaos.chaos-mesh.org               Namespaced   PodNetworkChaos
remoteclusters.chaos-mesh.org                Cluster      RemoteCluster
schedules.chaos-mesh.org                     Namespaced   Schedule
statuschecks.chaos-mesh.org                  Namespaced   StatusCheck
workflownodes.chaos-mesh.org                 Namespaced   WorkflowNode
workflows.chaos-mesh.org                     Namespaced   Workflow
verticalpodautoscalers.autoscaling.k8s.io    Namespaced   VerticalPodAutoscaler
certificates.cert.gardener.cloud             Namespaced   Certificate
ippools.crd.projectcalico.org                Cluster      IPPool
COLUMNS
  grep -E '(autoscaling\.k8s\.io|cert\.gardener\.cloud|crd\.projectcalico\.org) ' \
    "$dir/crd-columns.txt" >"$dir/crd-columns-after.txt"
  # The Helm hook objects gateway-helm 1.9.2 leaves behind, in kubectl's order.
  cat >"$dir/chart-objects.txt" <<'OBJECTS'
clusterrole.rbac.authorization.k8s.io/envoy-gateway-gateway-helm-certgen:envoy-gateway-system
clusterrolebinding.rbac.authorization.k8s.io/envoy-gateway-gateway-helm-certgen:envoy-gateway-system
mutatingwebhookconfiguration.admissionregistration.k8s.io/envoy-gateway-topology-injector.envoy-gateway-system
OBJECTS
  # What the stack-CR read finds in openstack after steps 1 and 2 when no
  # ControlPlane was ever applied: the base overlay's Gateway, the eso-tenant
  # Certificate hack/deploy-infra.sh applies, and its CertificateRequest. Nothing
  # reaps any of them before step 3.
  cat >"$dir/openstack-crs.json" <<'JSON'
{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"name":"openstack-gw","uid":"uid-gateway"}},
{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"eso-tenant-client-tls","uid":"uid-eso-cert"}},
{"apiVersion":"cert-manager.io/v1","kind":"CertificateRequest","metadata":{"name":"eso-tenant-client-tls-1","uid":"uid-eso-cr",
 "ownerReferences":[{"apiVersion":"cert-manager.io/v1","kind":"Certificate","name":"eso-tenant-client-tls","uid":"uid-eso-cert"}]}}
JSON
  # Two pods that mount no share of csi-driver-nfs: one without volumes, one
  # with a claim and an inline volume of another driver.
  cat >"$dir/pods.json" <<'JSON'
{"metadata":{"namespace":"openstack","name":"keystone-db-sync-x7k2p"},"spec":{"containers":[{"name":"db-sync"}]}},
{"metadata":{"namespace":"shared-services","name":"garage-0"},"spec":{"volumes":[
  {"name":"data","persistentVolumeClaim":{"claimName":"data-garage-0"}},
  {"name":"secrets","csi":{"driver":"secrets-store.csi.k8s.io","readOnly":true}}]}}
JSON
  # Two PersistentVolumes no pod mounts a share of csi-driver-nfs through: a
  # Bound one of another driver, and one of nfs.csi.k8s.io whose claim is gone.
  cat >"$dir/pvs.json" <<'JSON'
{"metadata":{"name":"pv-shoot-data-garage-0"},"spec":{"csi":{"driver":"io.lightbitslabs.lightos"},
  "claimRef":{"namespace":"shared-services","name":"data-garage-0"}},"status":{"phase":"Bound"}},
{"metadata":{"name":"nfs-health-backups"},"spec":{"csi":{"driver":"nfs.csi.k8s.io"},
  "claimRef":{"namespace":"nfs-health-probe","name":"nfs-health-backups"}},"status":{"phase":"Released"}}
JSON
  cat >"$dir/helmreleases.json" <<'JSON'
{"items":[
  {"metadata":{"namespace":"c5c3-system","name":"c5c3-operator"},"spec":{"suspend":true},
   "status":{"history":[{"name":"c5c3-operator","version":1,"status":"deployed"}]}},
  {"metadata":{"namespace":"nova-system","name":"nova-operator"},"spec":{"suspend":true},"status":{}},
  {"metadata":{"namespace":"cert-manager","name":"cert-manager"},"spec":{},
   "status":{"history":[{"name":"cert-manager","version":1,"status":"deployed"}]}}
]}
JSON
  cat >"$dir/kustomizations.json" <<'JSON'
{"items":[
  {"metadata":{"namespace":"flux-system","name":"k-orc"},"spec":{"suspend":true},
   "status":{"inventory":{"entries":[{"id":"_orc-system__Namespace","v":"v1"}]}}},
  {"metadata":{"namespace":"flux-system","name":"never-applied"},"spec":{"suspend":true},"status":{}},
  {"metadata":{"namespace":"flux-system","name":"rabbitmq-cluster-operator"},"spec":{"prune":true},
   "status":{"inventory":{"entries":[{"id":"_rabbitmq-system__Namespace","v":"v1"}]}}}
]}
JSON
  # What `kubectl kustomize` renders for the overlay's chaos-mesh/.
  cat >"$dir/chaos-render.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: chaos-mesh
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: chaos-mesh
  namespace: flux-system
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: chaos-mesh
  namespace: chaos-mesh
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: chaos-mesh-modules
  namespace: chaos-mesh
YAML
  cat >"$dir/base-render.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: openstack
---
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: envoy
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: openstack-gw
  namespace: openstack
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cert-manager
  namespace: cert-manager
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: jetstack
  namespace: flux-system
YAML

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
args="$*"
# The CRD reads leave out the chaos-mesh.org group under KUBECTL_CHAOS_ABSENT.
crd_filter() {
  if [ -n "${KUBECTL_CHAOS_ABSENT:-}" ]; then
    grep -v 'chaos-mesh\.org' || true
  else
    cat
  fi
}
case "$args" in
  "delete -f -"*)
    kinds="$(grep '^kind:' | sed 's/^kind: //' | sort | tr '\n' ' ')"
    echo "kubectl ${args} [kinds: ${kinds% }]" >>"$CALL_LOG"
    if [ -z "$kinds" ]; then
      echo "error: no objects passed to delete" >&2
      exit 1
    fi
    if [ -n "${KUBECTL_CHAOS_OVERLAY_DELETE_RC:-}" ] && [ "${kinds% }" = "DaemonSet HelmRelease HelmRepository" ]; then
      echo "error: timed out waiting for the condition on helmreleases/chaos-mesh" >&2
      exit 1
    fi
    ;;
  *) echo "kubectl ${args}" >>"$CALL_LOG" ;;
esac
case "$args" in
  version*)
    if [ "${KUBECTL_VERSION_RC:-0}" != "0" ]; then
      echo "The connection to the server api.lab.example:443 was refused - did you specify the right host or port?" >&2
      exit "${KUBECTL_VERSION_RC}"
    fi
    echo "Client Version: v1.35.0"
    ;;
  "config current-context"*) echo "lab-forge" ;;
  "get crd controlplanes.c5c3.io"*)
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "controlplanes.c5c3.io" not found' >&2
      exit 1
    fi
    ;;
  "get openbaoclusters.openbao.org openbao-instance -n openstack"*)
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'error: the server doesn'"'"'t have a resource type "openbaoclusters"' >&2
      exit 1
    fi
    ;;
  "patch openbaoclusters.openbao.org openbao-instance "*)
    if [ "${KUBECTL_OPENBAO_PATCH_RC:-0}" != "0" ]; then
      echo 'Error from server (Forbidden): openbaoclusters.openbao.org "openbao-instance" is forbidden: User "lab" cannot patch resource "openbaoclusters"' >&2
      exit "${KUBECTL_OPENBAO_PATCH_RC}"
    fi
    ;;
  "get crd ovncentrals.ovn.openstack.c5c3.io"*)
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "ovncentrals.ovn.openstack.c5c3.io" not found' >&2
      exit 1
    fi
    ;;
  "get crd novacomputes.nova.openstack.c5c3.io"* | "get crd neutronmetadataagents.neutron.openstack.c5c3.io"* | \
    "get crd ovnchassis.ovn.openstack.c5c3.io"* | "get crd hypervisors.kvm.cloud.sap"* | \
    "get crd evictions.kvm.cloud.sap"* | "get crd migrations.kvm.cloud.sap"*)
    crd="${args#get crd }"
    crd="${crd%% *}"
    if [ -n "${KUBECTL_SECOND_RUN:-}" ] || [[ " ${KUBECTL_ABSENT_CRDS:-} " == *" ${crd} "* ]]; then
      echo "Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io \"${crd}\" not found" >&2
      exit 1
    fi
    ;;
  "get domains.openstack.k-orc.cloud hvo-cc3test -n openstack"*)
    if [ -n "${KUBECTL_DOMAIN_NOISE:-}" ]; then
      echo 'E1001 10:00:00.000000    4242 memcache.go:287] couldn'"'"'t get resource list for metrics.k8s.io/v1beta1: the server is currently unable to handle the request' >&2
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'error: the server doesn'"'"'t have a resource type "domains"' >&2
      exit 1
    fi
    if [ "${KUBECTL_DOMAIN_GET_RC:-0}" != "0" ]; then
      echo 'Error from server (Timeout): the server was unable to return a response in the time allotted, but may still be processing the request (get domains.openstack.k-orc.cloud hvo-cc3test)' >&2
      exit "${KUBECTL_DOMAIN_GET_RC}"
    fi
    if [ -n "${KUBECTL_DOMAIN_ABSENT:-}" ]; then
      [[ "$args" != *--ignore-not-found* ]] || exit 0
      echo 'Error from server (NotFound): domains.openstack.k-orc.cloud "hvo-cc3test" not found' >&2
      exit 1
    fi
    echo 'domain.openstack.k-orc.cloud/hvo-cc3test'
    ;;
  "patch domains.openstack.k-orc.cloud hvo-cc3test "*)
    if [ -n "${KUBECTL_DOMAIN_ABSENT:-}" ]; then
      echo 'Error from server (NotFound): domains.openstack.k-orc.cloud "hvo-cc3test" not found' >&2
      exit 1
    fi
    ;;
  "wait domains.openstack.k-orc.cloud/hvo-cc3test -n openstack"*)
    if [ "${KUBECTL_DOMAIN_WAIT_RC:-0}" != "0" ]; then
      echo 'error: timed out waiting for the condition on domains/hvo-cc3test' >&2
      exit "${KUBECTL_DOMAIN_WAIT_RC}"
    fi
    ;;
  "get nodes -o name")
    if [ "${KUBECTL_NODES_RC:-0}" != "0" ]; then
      echo 'Error from server (Forbidden): nodes is forbidden: User "lab" cannot list resource "nodes"' >&2
      exit "${KUBECTL_NODES_RC}"
    fi
    printf '%s\n' node/lab-a node/lab-b
    ;;
  "get crd -o custom-columns=NAME:.metadata.name,SCOPE:.spec.scope,KIND:.spec.names.kind --no-headers")
    if [ -n "${KUBECTL_CRD_SCOPE_RC:-}" ]; then
      echo 'Error from server (Forbidden): customresourcedefinitions.apiextensions.k8s.io is forbidden' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      cat "$dir/crd-columns-after.txt"
    else
      crd_filter <"$dir/crd-columns.txt"
    fi
    ;;
  "get crd -o custom-columns=NAME:.metadata.name,SCOPE:.spec.scope --no-headers")
    if [ -n "${KUBECTL_CHAOS_CRD_SCOPE_RC:-}" ]; then
      echo 'Error from server (Forbidden): customresourcedefinitions.apiextensions.k8s.io is forbidden: User "lab" cannot list resource "customresourcedefinitions"' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      awk '{ print $1, $2 }' "$dir/crd-columns-after.txt"
    else
      crd_filter <"$dir/crd-columns.txt" | awk '{ print $1, $2 }'
    fi
    ;;
  "get pods -A -o json")
    if [ -n "${KUBECTL_PODS_RC:-}" ]; then
      echo 'Error from server (Forbidden): pods is forbidden: User "lab" cannot list resource "pods" in API group "" at the cluster scope' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_PODS_TRUNCATED:-}" ]; then
      echo '{"apiVersion":"v1","kind":"List","items":['
      exit 0
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo '{"items":[]}'
      exit 0
    fi
    if [ -n "${KUBECTL_NFS_POD_READS:-}" ]; then
      reads=$(( $(cat "$dir/pod-reads" 2>/dev/null || echo 0) + 1 ))
      echo "$reads" >"$dir/pod-reads"
      [ "$reads" -gt "$KUBECTL_NFS_POD_READS" ] || KUBECTL_NFS_POD_LEFT=1
    fi
    items="$(cat "$dir/pods.json")"
    if [ -n "${KUBECTL_NFS_POD_LEFT:-}" ]; then
      items="${items},{\"metadata\":{\"namespace\":\"openstack\",\"name\":\"cinder-volume-nfs1-0\",
        \"deletionTimestamp\":\"2026-10-03T19:00:00Z\"},\"spec\":{\"volumes\":[
        {\"name\":\"nfs-volumes\",\"csi\":{\"driver\":\"nfs.csi.k8s.io\",
        \"volumeAttributes\":{\"server\":\"nfs-server.openstack.svc.cluster.local\",\"share\":\"/volumes\"}}}]}}"
    fi
    printf '{"apiVersion":"v1","kind":"List","items":[%s]}\n' "$items"
    ;;
  "get pv -o json")
    if [ -n "${KUBECTL_PVS_RC:-}" ]; then
      echo 'Error from server (Forbidden): persistentvolumes is forbidden: User "lab" cannot list resource "persistentvolumes" in API group "" at the cluster scope' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo '{"items":[]}'
      exit 0
    fi
    items="$(cat "$dir/pvs.json")"
    if [ -n "${KUBECTL_NFS_PV_BOUND:-}" ]; then
      items="${items},{\"metadata\":{\"name\":\"nfs-health-volumes\"},\"spec\":{\"csi\":{\"driver\":\"nfs.csi.k8s.io\"},
        \"claimRef\":{\"namespace\":\"nfs-health-probe\",\"name\":\"nfs-health-volumes\"}},\"status\":{\"phase\":\"Bound\"}}"
    fi
    printf '{"apiVersion":"v1","kind":"List","items":[%s]}\n' "$items"
    ;;
  "get "*" -n openstack -o json")
    if [ -n "${KUBECTL_CR_READ_RC:-}" ]; then
      echo 'error: You must be logged in to the server (Unauthorized)' >&2
      exit 1
    fi
    items="$(cat "$dir/openstack-crs.json")"
    if [ -n "${KUBECTL_CR_LEFT:-}" ]; then
      items="${items},{\"apiVersion\":\"keystone.openstack.c5c3.io/v1alpha1\",\"kind\":\"Keystone\",
        \"metadata\":{\"name\":\"${KUBECTL_CR_LEFT}\",\"uid\":\"uid-keystone\",\"deletionTimestamp\":\"2026-09-30T19:00:00Z\"}}"
    fi
    if [ -n "${KUBECTL_CR_ORPHANED:-}" ]; then
      items="${items},{\"apiVersion\":\"external-secrets.io/v1\",\"kind\":\"SecretStore\",
        \"metadata\":{\"name\":\"${KUBECTL_CR_ORPHANED}\",\"uid\":\"uid-store\",
        \"ownerReferences\":[{\"apiVersion\":\"c5c3.io/v1alpha1\",\"kind\":\"ControlPlane\",\"name\":\"controlplane\",\"uid\":\"uid-controlplane\"}]}}"
    fi
    if [ -n "${KUBECTL_CLUSTER_SCOPED_CR:-}" ]; then
      case "$args" in
        *gatewayclasses.gateway.networking.k8s.io*)
          items="${items},{\"apiVersion\":\"gateway.networking.k8s.io/v1\",\"kind\":\"GatewayClass\",
            \"metadata\":{\"name\":\"envoy\",\"uid\":\"uid-gatewayclass\",\"deletionTimestamp\":\"2026-09-30T19:00:00Z\"}}"
          ;;
      esac
    fi
    printf '{"apiVersion":"v1","kind":"List","items":[%s]}\n' "$items"
    ;;
  "get crd -o name")
    if [ -n "${KUBECTL_CRD_RC:-}" ]; then
      echo 'error: You must be logged in to the server (Unauthorized)' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ] || grep -q 'delete customresourcedefinition' "$CALL_LOG"; then
      cat "$dir/crds-after.txt"
      if [ -n "${KUBECTL_CRD_LEFT:-}" ]; then
        echo "customresourcedefinition.apiextensions.k8s.io/${KUBECTL_CRD_LEFT}"
      fi
    else
      crd_filter <"$dir/crds.txt"
    fi
    ;;
  "get helmrelease -A -o json")
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'error: the server doesn'"'"'t have a resource type "helmrelease"' >&2
      exit 1
    fi
    cat "$dir/helmreleases.json"
    ;;
  "get helmrelease csi-driver-nfs -n kube-system"*)
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'error: the server doesn'"'"'t have a resource type "helmrelease"' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_NFS_RELEASE_RC:-}" ]; then
      echo 'Error from server (Forbidden): helmreleases.helm.toolkit.fluxcd.io "csi-driver-nfs" is forbidden: User "lab" cannot get resource "helmreleases" in API group "helm.toolkit.fluxcd.io" in the namespace "kube-system"' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_NFS_RELEASE_ABSENT:-}" ]; then
      [[ "$args" != *--ignore-not-found* ]] || exit 0
      echo 'Error from server (NotFound): helmreleases.helm.toolkit.fluxcd.io "csi-driver-nfs" not found' >&2
      exit 1
    fi
    echo 'helmrelease.helm.toolkit.fluxcd.io/csi-driver-nfs'
    ;;
  "get kustomization -n flux-system -o json")
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'error: the server doesn'"'"'t have a resource type "kustomization"' >&2
      exit 1
    fi
    cat "$dir/kustomizations.json"
    ;;
  "get namespace "*)
    if [ -n "${KUBECTL_NS_RC:-}" ]; then
      echo 'Unable to connect to the server: dial tcp 10.128.0.1:443: i/o timeout' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_NS_LEFT:-}" ]; then echo "namespace/${KUBECTL_NS_LEFT}"; fi
    ;;
  "get clusterrole,clusterrolebinding,mutatingwebhookconfiguration,validatingwebhookconfiguration -l "*)
    if [ -n "${KUBECTL_CHART_OBJECTS_RC:-}" ] ||
      { [ -n "${KUBECTL_CHART_OBJECTS_FINAL_RC:-}" ] && grep -q 'delete customresourcedefinition' "$CALL_LOG"; }; then
      echo 'Error from server (Forbidden): clusterroles.rbac.authorization.k8s.io is forbidden: User "lab" cannot list resource "clusterroles" in API group "rbac.authorization.k8s.io" at the cluster scope' >&2
      exit 1
    fi
    if [ -z "${KUBECTL_SECOND_RUN:-}" ] &&
      ! grep -qF 'kubectl delete clusterrole.rbac.authorization.k8s.io/' "$CALL_LOG"; then
      cat "$dir/chart-objects.txt"
    elif [ -n "${KUBECTL_CHART_OBJECT_LEFT:-}" ]; then
      echo "${KUBECTL_CHART_OBJECT_LEFT}"
    else
      [[ "$args" == *--ignore-not-found* ]] || echo 'No resources found' >&2
    fi
    ;;
  "kustomize "*"/chaos-mesh")
    if [ -n "${KUBECTL_CHAOS_RENDER_RC:-}" ]; then
      echo 'error: accumulating resources: accumulation err='"'"'accumulating resources from '"'"'../../../kind/chaos-mesh'"'"': must build at directory' >&2
      exit 1
    fi
    cat "$dir/chaos-render.yaml"
    ;;
  "kustomize "*)
    if [ -n "${KUBECTL_RENDER_RC:-}" ]; then
      echo 'error: accumulating resources: must build at directory: not a valid directory' >&2
      exit 1
    fi
    cat "${BASE_RENDER:-$dir/base-render.yaml}"
    ;;
  delete*)
    case "$args" in
      "delete ovncentrals.ovn.openstack.c5c3.io "*)
        if [ "${KUBECTL_OVNCENTRAL_DELETE_RC:-0}" != "0" ]; then
          echo "error: timed out waiting for the condition on ovncentrals/controlplane-ovn" >&2
          exit "${KUBECTL_OVNCENTRAL_DELETE_RC}"
        fi
        ;;
      "delete clusterrole.rbac.authorization.k8s.io/"*)
        if [ "${KUBECTL_CHART_OBJECT_DELETE_RC:-0}" != "0" ]; then
          echo "error: timed out waiting for the condition on clusterroles/envoy-gateway-gateway-helm-certgen:envoy-gateway-system" >&2
          exit "${KUBECTL_CHART_OBJECT_DELETE_RC}"
        fi
        ;;
      "delete lease "*)
        if [ "${KUBECTL_LEASE_DELETE_RC:-0}" != "0" ]; then
          echo 'Error from server (Forbidden): leases.coordination.k8s.io "cert-manager-controller" is forbidden: User "lab" cannot delete resource "leases" in API group "coordination.k8s.io" in the namespace "kube-system"' >&2
          exit "${KUBECTL_LEASE_DELETE_RC}"
        fi
        ;;
      "delete novacomputes.nova.openstack.c5c3.io "*)
        if [ "${KUBECTL_NOVACOMPUTE_DELETE_RC:-0}" != "0" ]; then
          echo "error: timed out waiting for the condition on novacomputes/lab" >&2
          exit "${KUBECTL_NOVACOMPUTE_DELETE_RC}"
        fi
        ;;
      "delete helmreleases.helm.toolkit.fluxcd.io dizzy-victoria-metrics "*)
        if [ "${KUBECTL_DIZZY_DELETE_RC:-0}" != "0" ]; then
          echo "error: timed out waiting for the condition on helmreleases/dizzy-victoria-metrics" >&2
          exit "${KUBECTL_DIZZY_DELETE_RC}"
        fi
        ;;
      "delete networkchaos.chaos-mesh.org,"*)
        if [ "${KUBECTL_CHAOS_DELETE_RC:-0}" != "0" ]; then
          echo "networkchaos.chaos-mesh.org \"lab-keystone-delay\" deleted" >&2
          echo "error: timed out waiting for the condition on networkchaos/lab-keystone-delay" >&2
          exit "${KUBECTL_CHAOS_DELETE_RC}"
        fi
        ;;
    esac
    if [ "${KUBECTL_DELETE_RC:-0}" != "0" ]; then
      echo "error: timed out waiting for the condition on namespaces/openstack" >&2
      exit "${KUBECTL_DELETE_RC}"
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      case "$args" in
        "delete -k "* | "delete -f "*)
          echo 'error: resource mapping not found for name: "openstack-db" namespace: "openstack" from "STDIN": no matches for kind "MariaDB" in version "k8s.mariadb.com/v1alpha1"' >&2
          echo 'ensure CRDs are installed first' >&2
          exit 1
          ;;
        "delete fluxinstance "*)
          echo 'error: the server doesn'"'"'t have a resource type "fluxinstance"' >&2
          exit 1
          ;;
        "delete helmreleases.helm.toolkit.fluxcd.io "*)
          echo 'error: the server doesn'"'"'t have a resource type "helmreleases"' >&2
          exit 1
          ;;
        "delete helmrepositories.source.toolkit.fluxcd.io "*)
          echo 'error: the server doesn'"'"'t have a resource type "helmrepositories"' >&2
          exit 1
          ;;
      esac
    fi
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"

  local tool
  for tool in kind docker; do
    # shellcheck disable=SC2016 # $* and $CALL_LOG expand when the stub runs
    printf '#!/bin/bash\necho "%s $*" >>"$CALL_LOG"\nexit 0\n' "$tool" >"$dir/$tool"
    chmod +x "$dir/$tool"
  done
}

# run_teardown <stub_dir> [env_var=value...]
# Runs hack/teardown-infra.sh with the stubs first on PATH and the given
# overrides. Echoes combined output; returns the script's exit status.
run_teardown() {
  local stub_dir="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY TEARDOWN_TIMEOUT PURGE_REGISTRY_CACHE
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$stub_dir:$PATH"
    export PATH
    bash "$TEARDOWN_SH"
  ) 2>&1
}

# resolve <expression> [env_var=value...]
# Sources teardown-infra.sh with the given overrides and echoes <expression>.
resolve() {
  local expression="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY TEARDOWN_TIMEOUT
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$TEARDOWN_SH"
    eval "printf '%s' \"${expression}\""
  )
}

# mutations <call log>
# The recorded kubectl calls that change the cluster, plus the render, with the
# delete flags every call carries and the patch payloads stripped, one per line.
mutations() {
  grep -E '^kubectl (delete|patch|kustomize|label|annotate) ' "$1" |
    sed -e 's/ --ignore-not-found --wait --timeout=[0-9]*s//' \
      -e "s# --type merge -p {\"spec\":{\"suspend\":false}}##" \
      -e "s# --type merge -p {\"spec\":{\"deletionPolicy\":\"DeletePVCs\"}}##" \
      -e "s# --type merge -p {\"spec\":{\"resource\":{\"enabled\":false}}}##" \
      -e "s#${PROJECT_ROOT}/##g"
}

have_yq() {
  command -v yq >/dev/null 2>&1
}

# stack_namespace_names — the Namespaces of deploy/flux-system/namespaces.yaml,
# space-separated, then the two the kind base declares and the ones of the
# Chaos Mesh and dizzy overlays.
stack_namespace_names() {
  yq -N -r 'select(.kind == "Namespace") | .metadata.name' \
    "$PROJECT_ROOT/deploy/flux-system/namespaces.yaml" | tr '\n' ' '
  printf '%s' 'envoy-gateway-system headlamp-system chaos-mesh dizzy'
}

# ---------------------------------------------------------------------------
# Test 1: the knobs
# ---------------------------------------------------------------------------
# shellcheck disable=SC2016 # resolve expands the expression after it sources the script
test_knobs() {
  echo "Test: EXTERNAL_CLUSTER, EXTERNAL_OVERLAY and TEARDOWN_TIMEOUT"

  assert_eq "EXTERNAL_CLUSTER defaults to false" "false" "$(resolve '${EXTERNAL_CLUSTER}')"
  assert_eq "EXTERNAL_CLUSTER=yes is preserved verbatim" "yes" \
    "$(resolve '${EXTERNAL_CLUSTER}' EXTERNAL_CLUSTER=yes)"
  assert_eq "TEARDOWN_TIMEOUT defaults to 600" "600" "$(resolve '${TEARDOWN_TIMEOUT}')"
  assert_eq "OVERLAY_ROOT resolves the default overlay against the repository" \
    "$PROJECT_ROOT/deploy/lab/metal-stack" "$(resolve '${OVERLAY_ROOT}')"
  assert_eq "an absolute EXTERNAL_OVERLAY is used unchanged" "/srv/overlays/lab" \
    "$(resolve '${OVERLAY_ROOT}' EXTERNAL_OVERLAY=/srv/overlays/lab)"
}

# ---------------------------------------------------------------------------
# Test 2: the default teardown is the kind one
# ---------------------------------------------------------------------------
test_default_deletes_the_kind_cluster() {
  echo "Test: the default teardown deletes the kind cluster and never calls kubectl"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"
  : >"$CALL_LOG"

  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=yes)"
  rc=$?
  assert_eq "the kind teardown exits 0" "0" "$rc"
  assert_contains "kind delete cluster is called" "$(cat "$CALL_LOG")" "kind delete cluster --name cobaltcore"
  assert_not_contains "kubectl is never called" "$(cat "$CALL_LOG")" "kubectl"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 3: the external teardown, in order
# ---------------------------------------------------------------------------
test_external_teardown_order() {
  echo "Test: the external teardown removes the stack in finalizer order"

  if ! have_yq; then
    echo "  SKIP: yq not installed (34 checks skipped)"
    SKIP=$((SKIP + 34))
    return
  fi

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"
  : >"$CALL_LOG"

  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true PURGE_REGISTRY_CACHE=true)"
  rc=$?
  assert_eq "the external teardown exits 0" "0" "$rc"
  # By line start: the CRD scope read's custom-columns end in `.kind`.
  assert_eq "kind is never called" "" "$(grep '^kind ' "$CALL_LOG" || true)"
  assert_not_contains "docker is never called, even with PURGE_REGISTRY_CACHE=true" \
    "$(cat "$CALL_LOG")" "docker "
  assert_contains "the context is logged" "$output" "Kubeconfig context  : lab-forge"
  assert_contains "the final report counts nothing left" "$output" \
    "Stack CRDs left: 0; stack namespaces left: 0; cluster-scoped chart objects left: 0"

  local expected
  expected="$(printf '%s\n' \
    'kubectl delete schedules.chaos-mesh.org,workflows.chaos-mesh.org --all -A' \
    'kubectl delete networkchaos.chaos-mesh.org,podchaos.chaos-mesh.org,statuschecks.chaos-mesh.org,workflownodes.chaos-mesh.org --all -A' \
    'kubectl delete remoteclusters.chaos-mesh.org --all' \
    'kubectl kustomize deploy/lab/metal-stack/chaos-mesh' \
    'kubectl delete -f - [kinds: DaemonSet HelmRelease HelmRepository]' \
    'kubectl delete novacomputes.nova.openstack.c5c3.io --all -n openstack' \
    'kubectl delete neutronmetadataagents.neutron.openstack.c5c3.io --all -n openstack' \
    'kubectl delete ovnchassis.ovn.openstack.c5c3.io --all -n openstack' \
    'kubectl delete hypervisors.kvm.cloud.sap --all' \
    'kubectl delete evictions.kvm.cloud.sap --all' \
    'kubectl delete migrations.kvm.cloud.sap --all -A' \
    'kubectl patch domains.openstack.k-orc.cloud hvo-cc3test -n openstack' \
    'kubectl delete -k deploy/lab/metal-stack/hypervisor-fixtures' \
    'kubectl delete -k deploy/lab/metal-stack/hypervisor' \
    'kubectl delete deployment,poddisruptionbudget maint-lab-a maint-lab-b -n kube-system' \
    "kubectl label nodes --all ${HYPERVISOR_LABELS_REMOVED}" \
    'kubectl annotate nodes --all nova.openstack.cloud.sap/custom-traits-' \
    'kubectl delete controlplane --all -n openstack' \
    'kubectl delete ovncentrals.ovn.openstack.c5c3.io --all -n openstack' \
    'kubectl patch openbaoclusters.openbao.org openbao-instance -n openstack' \
    'kubectl delete -k deploy/lab/metal-stack/infrastructure' \
    'kubectl delete -k deploy/kind/messaging' \
    'kubectl delete -k deploy/lab/metal-stack/nfs' \
    'kubectl delete -f deploy/lab/metal-stack/nfs/client-policy.yaml' \
    'kubectl delete csidriver -l helm.toolkit.fluxcd.io/name=csi-driver-nfs,helm.toolkit.fluxcd.io/namespace=kube-system' \
    'kubectl patch helmrelease c5c3-operator -n c5c3-system' \
    'kubectl patch kustomization k-orc -n flux-system' \
    'kubectl kustomize deploy/lab/metal-stack/base' \
    'kubectl delete -f - [kinds: Gateway GatewayClass]' \
    'kubectl delete -f - [kinds: HelmRelease HelmRepository]' \
    'kubectl delete -f deploy/kind/prometheus/release.yaml' \
    'kubectl delete helmreleases.helm.toolkit.fluxcd.io dizzy-victoria-metrics dizzy-grafana -n dizzy' \
    'kubectl delete helmrepositories.source.toolkit.fluxcd.io victoria-metrics grafana -n flux-system' \
    'kubectl delete pvc --all -n dizzy' \
    'kubectl delete pvc --all -n shared-services' \
    'kubectl delete pvc --all -n openstack' \
    'kubectl delete fluxinstance flux -n flux-system' \
    'kubectl delete namespace flux-system' \
    'kubectl delete clusterrolebinding flux-operator-cluster-admin' \
    'kubectl delete clusterrole flux-operator-edit flux-operator-view flux-web-admin flux-web-user' \
    "kubectl delete namespace $(stack_namespace_names)" \
    "kubectl delete $(paste -sd' ' "$tmp/bin/chart-objects.txt")" \
    'kubectl delete lease cert-manager-cainjector-leader-election cert-manager-controller -n kube-system')"
  local actual
  actual="$(mutations "$CALL_LOG" | grep -v 'customresourcedefinition')"
  assert_eq "the deletes run in finalizer order, Chaos Mesh and the lab hypervisors first (patches only for installed, suspended objects)" \
    "$expected" "$actual"

  # Chaos Mesh goes first: its CRD scope read, then its deletes, all before the
  # first call of step 0, which reads the NovaCompute CRD. The per-pod records
  # are left to the CRD delete of step 8.
  local scope_read scope_line chaos_first_line chaos_overlay_line step0_line
  scope_read='kubectl get crd -o custom-columns=NAME:.metadata.name,SCOPE:.spec.scope --no-headers'
  scope_line="$(grep -nxF "$scope_read" "$CALL_LOG" | cut -d: -f1 | head -n1)"
  chaos_first_line="$(grep -n '^kubectl delete schedules\.chaos-mesh\.org,' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  chaos_overlay_line="$(grep -nF '[kinds: DaemonSet HelmRelease HelmRepository]' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  step0_line="$(grep -n '^kubectl get crd novacomputes\.nova\.openstack\.c5c3\.io' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  assert_eq "the Chaos Mesh CRD scope is read once" "1" "$(grep -cxF "$scope_read" "$CALL_LOG")"
  assert_eq "and before the schedules and workflows are deleted" "true" \
    "$([[ -n "$scope_line" && -n "$chaos_first_line" && "$scope_line" -lt "$chaos_first_line" ]] && echo true || echo false)"
  assert_eq "the Chaos Mesh overlay is deleted before the first call of step 0" "true" \
    "$([[ -n "$chaos_overlay_line" && -n "$step0_line" && "$chaos_overlay_line" -lt "$step0_line" ]] && echo true || echo false)"
  assert_eq "no Chaos Mesh delete names podnetworkchaos, podiochaos or podhttpchaos" "" \
    "$(grep -E '^kubectl delete [a-z]+\.chaos-mesh\.org' "$CALL_LOG" | grep -E 'pod(network|io|http)chaos' || true)"

  # The fixtures' domain: disabled, then waited for, then deleted with them.
  local patch_line domain_wait_line fixtures_line
  patch_line="$(grep -n '^kubectl patch domains.openstack.k-orc.cloud hvo-cc3test ' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  domain_wait_line="$(grep -n '^kubectl wait domains.openstack.k-orc.cloud/hvo-cc3test ' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  fixtures_line="$(grep -n 'delete -k .*/hypervisor-fixtures ' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  assert_eq "the domain is waited for after it is disabled" "true" \
    "$([[ -n "$patch_line" && -n "$domain_wait_line" && "$domain_wait_line" -gt "$patch_line" ]] && echo true || echo false)"
  assert_eq "and the fixtures are deleted after the wait" "true" \
    "$([[ -n "$domain_wait_line" && -n "$fixtures_line" && "$fixtures_line" -gt "$domain_wait_line" ]] && echo true || echo false)"

  # The wait for the ControlPlane's descendants: after the overlays, before the
  # base overlay removes the operators that finalize them. One read of the
  # namespaced stack kinds; neither the cluster-scoped GatewayClass nor a
  # platform kind is read. The Gateway, the eso-tenant Certificate and its
  # CertificateRequest are not being reaped, so the first read ends the wait.
  local messaging_line wait_line operators_line reads read_kinds
  messaging_line="$(grep -n 'delete -k .*deploy/kind/messaging' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  wait_line="$(grep -n -- '^kubectl get [^ ]* -n openstack -o json$' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  operators_line="$(grep -n 'patch helmrelease c5c3-operator' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  assert_eq "the stack CRs in openstack are read after the message-bus overlay" "true" \
    "$([[ -n "$wait_line" && -n "$messaging_line" && "$wait_line" -gt "$messaging_line" ]] && echo true || echo false)"
  assert_eq "and before the operators are resumed and removed" "true" \
    "$([[ -n "$wait_line" && -n "$operators_line" && "$wait_line" -lt "$operators_line" ]] && echo true || echo false)"
  reads="$(grep -c -- '^kubectl get [^ ]* -n openstack -o json$' "$CALL_LOG")"
  assert_eq "the Gateway and the objects nothing reaps end the wait on the first read" "1" "$reads"
  read_kinds="$(grep -- '^kubectl get [^ ]* -n openstack -o json$' "$CALL_LOG" | head -n1 |
    cut -d' ' -f3 | tr ',' '\n' | sort)"
  assert_eq "the one read names every namespaced stack kind and nothing else" \
    "$(grep -vx -e 'gatewayclasses.gateway.networking.k8s.io' -e 'hypervisors.kvm.cloud.sap' \
      -e 'evictions.kvm.cloud.sap' -e 'remoteclusters.chaos-mesh.org' <<<"$STACK_CRDS" | sort)" "$read_kinds"

  # The NFS stack goes after that wait, once no pod mounts a share through its
  # node plugin, which the HelmRelease csi-driver-nfs installed. Neither pod of
  # pods.json mounts one and neither PersistentVolume of pvs.json is a Bound
  # one of the driver, so one read ends the wait.
  local release_line pods_line pvs_line nfs_line
  release_line="$(grep -n -- '^kubectl get helmrelease csi-driver-nfs -n kube-system ' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  pods_line="$(grep -nx -- 'kubectl get pods -A -o json' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  pvs_line="$(grep -nx -- 'kubectl get pv -o json' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  nfs_line="$(grep -n 'delete -k .*/deploy/lab/metal-stack/nfs ' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  assert_eq "the csi-driver-nfs HelmRelease is read after the stack CRs in openstack and before the pods" "true" \
    "$([[ -n "$release_line" && -n "$wait_line" && -n "$pods_line" &&
      "$release_line" -gt "$wait_line" && "$release_line" -lt "$pods_line" ]] && echo true || echo false)"
  assert_eq "the pods are read after the stack CRs in openstack" "true" \
    "$([[ -n "$pods_line" && -n "$wait_line" && "$pods_line" -gt "$wait_line" ]] && echo true || echo false)"
  assert_eq "and before the NFS overlay is deleted" "true" \
    "$([[ -n "$pods_line" && -n "$nfs_line" && "$pods_line" -lt "$nfs_line" ]] && echo true || echo false)"
  assert_eq "so are the PersistentVolumes" "true" \
    "$([[ -n "$pvs_line" && -n "$nfs_line" && "$pvs_line" -gt "$wait_line" && "$pvs_line" -lt "$nfs_line" ]] && echo true || echo false)"
  assert_eq "pods without an nfs.csi.k8s.io volume end the wait on the first read" "1" \
    "$(grep -cx -- 'kubectl get pods -A -o json' "$CALL_LOG")"
  assert_eq "and so do another driver's Bound and the driver's Released PersistentVolume" "1" \
    "$(grep -cx -- 'kubectl get pv -o json' "$CALL_LOG")"

  # The cluster-scoped objects the charts left behind: selected by the release
  # namespace label, read once the stack namespaces are gone (before that the
  # selector also matches the objects of every installed chart), and named.
  local chart_read namespaces_line chart_read_line
  chart_read="kubectl get clusterrole,clusterrolebinding,mutatingwebhookconfiguration,validatingwebhookconfiguration -l helm.toolkit.fluxcd.io/namespace in ($(stack_namespace_names | tr ' ' ','),flux-system) --ignore-not-found -o name"
  assert_contains "the chart objects are read by the label of every stack namespace" \
    "$(cat "$CALL_LOG")" "$chart_read"
  namespaces_line="$(grep -nF "kubectl delete namespace $(stack_namespace_names) " "$CALL_LOG" | cut -d: -f1 | head -n1)"
  chart_read_line="$(grep -nxF "$chart_read" "$CALL_LOG" | cut -d: -f1 | head -n1)"
  assert_eq "the chart objects are read after the stack namespaces are deleted" "true" \
    "$([[ -n "$namespaces_line" && -n "$chart_read_line" && "$chart_read_line" -gt "$namespaces_line" ]] && echo true || echo false)"
  assert_eq "each of the three chart objects is logged" "3" "$(grep -c 'Chart leftover: ' <<<"$output")"
  assert_eq "the chart objects are read twice: before their delete and in the final count" "2" \
    "$(grep -cxF "$chart_read" "$CALL_LOG")"

  local deletes without_flag
  deletes="$(grep -E '^kubectl delete ' "$CALL_LOG")"
  without_flag="$(grep -v -e '--ignore-not-found' <<<"$deletes" || true)"
  assert_eq "every delete carries --ignore-not-found" "" "$without_flag"
  assert_eq "every delete waits, bounded by TEARDOWN_TIMEOUT" "" \
    "$(grep -v -e '--wait --timeout=600s' <<<"$deletes" || true)"

  local crd_line crd_args
  crd_line="$(grep -E '^kubectl delete customresourcedefinition' "$CALL_LOG")"
  assert_eq "the CRDs are deleted last" "$crd_line" "$(tail -n1 <<<"$deletes")"
  crd_args="$(tr ' ' '\n' <<<"$crd_line" | grep '^customresourcedefinition' |
    sed 's#^customresourcedefinition.apiextensions.k8s.io/##' | sort)"
  assert_eq "exactly the stack CRDs are deleted" "$(sort <<<"$STACK_CRDS")" "$crd_args"
  local platform
  for platform in $PLATFORM_CRDS; do
    assert_not_contains "the platform CRD ${platform} is never deleted" "$crd_line" "$platform"
  done

  # Every namespace the teardown names: none outside the stack's.
  local named
  named="$(grep -E '^kubectl delete (namespace|ns) ' "$CALL_LOG" |
    sed -e 's/^kubectl delete [a-z]* //' -e 's/ --ignore-not-found.*//' | tr ' ' '\n' |
    grep -vxF -f <({ echo flux-system; stack_namespace_names | tr ' ' '\n'; }) || true)"
  assert_eq "no namespace outside the stack's is deleted" "" "$named"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 4: the script names no platform namespace
# ---------------------------------------------------------------------------
test_script_names_no_platform_namespace() {
  echo "Test: teardown-infra.sh names no namespace outside the stack's"

  local deletes named
  deletes="$(grep -E '(delete|delete_and_wait "[^"]*") (namespace|ns) ' "$TEARDOWN_SH")"
  assert_eq "the script deletes namespaces by name on two lines" "2" "$(grep -c . <<<"$deletes")"
  # The one list it deletes is read from stack_namespaces; the run in Test 3
  # checks what that list holds.
  # shellcheck disable=SC2016 # the script's literal text, not expanded here
  named="$(sed -E 's/.* (namespace|ns) //' <<<"$deletes" | tr ' ' '\n' |
    grep -vxF -e flux-system -e '"${namespaces_to_delete[@]}"' || true)"
  assert_eq "every namespace delete in the script names flux-system or the stack_namespaces list" "" "$named"
  # shellcheck disable=SC2016 # the script's literal text, not expanded here
  assert_file_contains_fixed "the deleted list is read from stack_namespaces" \
    "$TEARDOWN_SH" 'namespace_list="$(stack_namespaces)"'
  local ns
  for ns in kube-system firewall metallb-system default; do
    assert_file_not_contains "the script never names namespace ${ns} for deletion" \
      "$TEARDOWN_SH" "namespace ${ns}"
  done
}

# ---------------------------------------------------------------------------
# Test 5: the abort paths and the second run
# ---------------------------------------------------------------------------
test_external_teardown_failures() {
  echo "Test: the external teardown aborts on an unreachable cluster, a timeout or leftovers"

  if ! have_yq; then
    echo "  SKIP: yq not installed (116 checks skipped)"
    SKIP=$((SKIP + 116))
    return
  fi

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_VERSION_RC=1)"
  rc=$?
  assert_nonzero_exit "an unreachable API server exits non-zero" "$rc"
  assert_contains "quotes kubectl's error" "$output" "api.lab.example:443 was refused"
  assert_not_contains "nothing is deleted" "$(cat "$CALL_LOG")" "kubectl delete"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_DELETE_RC=1)"
  rc=$?
  assert_eq "a wait that runs out exits 1" "1" "$rc"
  assert_contains "the error says the delete did not finish" "$output" "did not finish within 600s"
  assert_contains "the error names the object still present" "$output" "namespaces/openstack"

  # A ControlPlane descendant that outlives the wait: exit 1 before any
  # operator is resumed or removed, naming the object, so its controller is
  # still there when the operator looks at it. One is being deleted, the other
  # has lost its ControlPlane and waits for garbage collection to mark it.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_CR_LEFT=controlplane-keystone)"
  rc=$?
  assert_eq "a stack CR being deleted in openstack exits 1" "1" "$rc"
  assert_contains "the error says the CRs were not finalized" "$output" \
    "the stack CRs in openstack were not finalized within 1s"
  assert_contains "and names the object" "$output" "Keystone/controlplane-keystone"
  assert_not_contains "and not the Gateway nothing reaps before step 3" "$output" "Gateway/openstack-gw"
  assert_not_contains "no operator is resumed or removed afterwards" "$(cat "$CALL_LOG")" "patch helmrelease"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_CR_ORPHANED=openbao-tenant-store)"
  rc=$?
  assert_eq "a stack CR whose ControlPlane is gone exits 1" "1" "$rc"
  assert_contains "and names the object" "$output" "SecretStore/openbao-tenant-store"
  assert_not_contains "and not the CertificateRequest whose Certificate is there" "$output" \
    "CertificateRequest/eso-tenant-client-tls-1"
  assert_not_contains "no operator is resumed or removed afterwards" "$(cat "$CALL_LOG")" "patch helmrelease"

  # A read that keeps failing is not an empty namespace.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_CR_READ_RC=1)"
  rc=$?
  assert_eq "a stack-CR read that keeps failing exits 1" "1" "$rc"
  assert_contains "and names kubectl's error" "$output" \
    "cannot read them: error: You must be logged in to the server (Unauthorized)"
  assert_not_contains "no operator is resumed or removed afterwards" "$(cat "$CALL_LOG")" "patch helmrelease"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CRD_SCOPE_RC=1)"
  rc=$?
  assert_eq "a CRD scope that cannot be read exits 1" "1" "$rc"
  assert_contains "says the scope cannot be read" "$output" "cannot read the scope of the cluster's CRDs"
  assert_not_contains "no operator is resumed or removed afterwards" "$(cat "$CALL_LOG")" "patch helmrelease"

  # A pod that still mounts a share of csi-driver-nfs: exit 1 before any part
  # of the NFS stack or any operator goes, naming that pod alone.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_NFS_POD_LEFT=1)"
  rc=$?
  assert_eq "a pod with an nfs.csi.k8s.io volume that outlives the wait exits 1" "1" "$rc"
  assert_contains "the error says pods still mount a share" "$output" \
    "ERROR: pods or bound PersistentVolumes still use an nfs.csi.k8s.io volume after 1s:"
  assert_contains "and names the pod" "$output" "openstack/cinder-volume-nfs1-0"
  assert_not_contains "and not the pod without volumes" "$output" "keystone-db-sync-x7k2p"
  assert_not_contains "nor the pod with another driver's inline volume" "$output" "garage-0"
  assert_contains "and says what has to happen first" "$output" \
    "The csi-driver-nfs node plugin has to unmount them before it is removed; delete what owns these pods and claims and rerun."
  assert_eq "the NFS overlay is not deleted" "" "$(grep -E 'delete -k .*/nfs ' "$CALL_LOG" || true)"
  assert_not_contains "nor the CSIDriver" "$(cat "$CALL_LOG")" "delete csidriver"
  assert_not_contains "and no operator is resumed or removed" "$(cat "$CALL_LOG")" "patch helmrelease"

  # A share mounted through a claim, as the nfs-health probe does: its Bound
  # PersistentVolume holds the wait, and the error names it with its claim.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_NFS_PV_BOUND=1)"
  rc=$?
  assert_eq "a Bound nfs.csi.k8s.io PersistentVolume that outlives the wait exits 1" "1" "$rc"
  assert_contains "the error names the PersistentVolume and its claim" "$output" \
    "pv/nfs-health-volumes (claim nfs-health-probe/nfs-health-volumes)"
  assert_not_contains "and not the driver's Released one" "$output" "pv/nfs-health-backups"
  assert_not_contains "nor another driver's Bound one" "$output" "pv-shoot-data-garage-0"
  assert_eq "the NFS overlay is not deleted while the claim binds it" "" \
    "$(grep -E 'delete -k .*/nfs ' "$CALL_LOG" || true)"
  assert_not_contains "and no operator is resumed or removed" "$(cat "$CALL_LOG")" "patch helmrelease"

  # A pod that unmounts its share during the wait: the second read finds none,
  # and the NFS stack goes.
  rm -f "$tmp/bin/pod-reads"
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=30 KUBECTL_NFS_POD_READS=1)"
  rc=$?
  assert_eq "a pod that unmounts its share during the wait lets the teardown go on" "0" "$rc"
  assert_eq "after a second pod read" "2" "$(grep -cx -- 'kubectl get pods -A -o json' "$CALL_LOG")"
  assert_contains "and the NFS overlay is deleted then" "$(cat "$CALL_LOG")" \
    "delete -k $PROJECT_ROOT/deploy/lab/metal-stack/nfs "

  # A read that keeps failing is not a cluster without mounts.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_PODS_RC=1)"
  rc=$?
  assert_eq "a pod read that keeps failing exits 1" "1" "$rc"
  assert_contains "and names kubectl's error" "$output" \
    "cannot read them: Error from server (Forbidden): pods is forbidden"
  assert_eq "the NFS overlay is not deleted after a failed read" "" \
    "$(grep -E 'delete -k .*/nfs ' "$CALL_LOG" || true)"
  assert_not_contains "and no operator is resumed or removed" "$(cat "$CALL_LOG")" "patch helmrelease"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_PVS_RC=1)"
  rc=$?
  assert_eq "a PersistentVolume read that keeps failing exits 1" "1" "$rc"
  assert_contains "and names kubectl's error" "$output" \
    "cannot read them: Error from server (Forbidden): persistentvolumes is forbidden"
  assert_eq "the NFS overlay is not deleted after a failed PersistentVolume read" "" \
    "$(grep -E 'delete -k .*/nfs ' "$CALL_LOG" || true)"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_PODS_TRUNCATED=1)"
  rc=$?
  assert_eq "a pod read yq cannot parse exits 1" "1" "$rc"
  assert_contains "and says yq cannot read it" "$output" "yq cannot read them (its error is above)"
  assert_eq "the NFS overlay is not deleted after an unreadable answer" "" \
    "$(grep -E 'delete -k .*/nfs ' "$CALL_LOG" || true)"

  # A cluster whose NFS CSI driver the platform runs: the deploy refused
  # WITH_NFS=true there, so no HelmRelease csi-driver-nfs exists, and a Bound
  # PersistentVolume of nfs.csi.k8s.io is the platform's, not the teardown's
  # to wait for.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 \
    KUBECTL_NFS_RELEASE_ABSENT=1 KUBECTL_NFS_PV_BOUND=1)"
  rc=$?
  assert_eq "without the csi-driver-nfs HelmRelease a Bound nfs.csi.k8s.io PersistentVolume does not hold the teardown" \
    "0" "$rc"
  assert_not_contains "no pod is read" "$(cat "$CALL_LOG")" "get pods -A"
  assert_not_contains "nor a PersistentVolume" "$(cat "$CALL_LOG")" "get pv "
  assert_contains "and the NFS overlay delete still runs" "$(cat "$CALL_LOG")" \
    "delete -k $PROJECT_ROOT/deploy/lab/metal-stack/nfs "

  # A read of that HelmRelease that fails is not an absent one.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_NFS_RELEASE_RC=1)"
  rc=$?
  assert_eq "a csi-driver-nfs HelmRelease read that fails exits 1" "1" "$rc"
  assert_contains "the error names the HelmRelease" "$output" \
    "ERROR: cannot read the HelmRelease kube-system/csi-driver-nfs:"
  assert_contains "and quotes kubectl's error" "$output" \
    'helmreleases.helm.toolkit.fluxcd.io "csi-driver-nfs" is forbidden'
  assert_eq "the NFS overlay is not deleted after that read failed" "" \
    "$(grep -E 'delete -k .*/nfs ' "$CALL_LOG" || true)"
  assert_not_contains "and no operator is resumed or removed" "$(cat "$CALL_LOG")" "patch helmrelease"

  # An overlay without nfs/: no pod read, no NFS delete.
  mkdir -p "$tmp/no-nfs/base" "$tmp/no-nfs/infrastructure"
  : >"$tmp/no-nfs/base/kustomization.yaml"
  : >"$tmp/no-nfs/infrastructure/kustomization.yaml"
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY="$tmp/no-nfs")"
  rc=$?
  assert_eq "an overlay without nfs/ tears down" "0" "$rc"
  assert_not_contains "without reading the pods" "$(cat "$CALL_LOG")" "get pods -A"
  assert_not_contains "or the PersistentVolumes" "$(cat "$CALL_LOG")" "get pv "
  assert_eq "without deleting an nfs/ overlay" "" "$(grep -E 'delete -k .*/nfs ' "$CALL_LOG" || true)"
  assert_not_contains "or a client policy" "$(cat "$CALL_LOG")" "client-policy.yaml"
  assert_not_contains "or a CSIDriver" "$(cat "$CALL_LOG")" "delete csidriver"

  # An overlay whose nfs/ ships no client-policy.yaml: the overlay and the
  # CSIDriver go, and no policy is named.
  mkdir -p "$tmp/no-policy/base" "$tmp/no-policy/infrastructure" "$tmp/no-policy/nfs"
  : >"$tmp/no-policy/base/kustomization.yaml"
  : >"$tmp/no-policy/infrastructure/kustomization.yaml"
  : >"$tmp/no-policy/nfs/kustomization.yaml"
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY="$tmp/no-policy")"
  rc=$?
  assert_eq "an overlay whose nfs/ has no client-policy.yaml tears down" "0" "$rc"
  assert_contains "its nfs/ overlay is deleted" "$(cat "$CALL_LOG")" "delete -k $tmp/no-policy/nfs "
  assert_not_contains "and no client policy" "$(cat "$CALL_LOG")" "client-policy.yaml"

  # The cluster-scoped GatewayClass is never read in openstack: kubectl would
  # ignore -n and report the one step 3 deletes.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_CLUSTER_SCOPED_CR=1)"
  rc=$?
  assert_eq "a GatewayClass being deleted does not hold the wait" "0" "$rc"
  assert_not_contains "the cluster-scoped CRD is not read in openstack" \
    "$(grep -- ' -n openstack -o json$' "$CALL_LOG")" "gatewayclasses"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_NS_LEFT=openstack)"
  rc=$?
  assert_eq "a namespace left behind exits 1" "1" "$rc"
  assert_contains "the report counts it" "$output" "stack namespaces left: 1"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CRD_LEFT=certificates.cert-manager.io)"
  rc=$?
  assert_eq "a stack CRD left behind exits 1" "1" "$rc"
  assert_contains "the report counts it" "$output" "Stack CRDs left: 1; stack namespaces left: 0"
  assert_contains "and says objects were left" "$output" "the teardown left stack objects behind"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true \
    KUBECTL_CHART_OBJECT_LEFT=clusterrole.rbac.authorization.k8s.io/new-hook)"
  rc=$?
  assert_eq "a chart object left behind exits 1" "1" "$rc"
  assert_contains "the report counts it" "$output" \
    "Stack CRDs left: 0; stack namespaces left: 0; cluster-scoped chart objects left: 1"
  assert_contains "and names it" "$output" "Still present: clusterrole.rbac.authorization.k8s.io/new-hook"
  assert_contains "and says objects were left" "$output" "the teardown left stack objects behind"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CRD_RC=1)"
  rc=$?
  assert_eq "a CRD list that cannot be read exits 1" "1" "$rc"
  assert_contains "says the CRDs cannot be listed" "$output" "cannot list the cluster's CRDs"
  assert_contains "in step 8, after the stack namespaces are deleted" "$(cat "$CALL_LOG")" \
    "kubectl delete namespace $(stack_namespace_names)"
  assert_not_contains "and deletes no CRD blind" "$(cat "$CALL_LOG")" "delete customresourcedefinition"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_RENDER_RC=1)"
  rc=$?
  assert_eq "a base overlay that does not render exits 1" "1" "$rc"
  assert_contains "says the overlay cannot be rendered" "$output" "cannot render"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_NS_RC=1)"
  rc=$?
  assert_eq "a final namespace read that fails exits 1" "1" "$rc"
  assert_contains "says the namespaces cannot be read" "$output" "cannot read the stack namespaces"

  # The final read of the chart objects fails: an unreadable cluster is not one
  # without leftovers, so no report is printed.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CHART_OBJECTS_FINAL_RC=1)"
  rc=$?
  assert_eq "a final chart-object read that fails exits 1" "1" "$rc"
  assert_contains "says the chart objects cannot be read" "$output" \
    "cannot read the cluster-scoped objects of the stack's charts"
  assert_not_contains "and prints no report" "$output" "Stack CRDs left"

  : >"$CALL_LOG"
  yq 'select(.kind != "Gateway" and .kind != "GatewayClass")' "$tmp/bin/base-render.yaml" \
    >"$tmp/no-gateway.yaml"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true BASE_RENDER="$tmp/no-gateway.yaml")"
  rc=$?
  assert_eq "an overlay without a Gateway still tears down (the empty pass is absence)" "0" "$rc"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_SECOND_RUN=1)"
  rc=$?
  assert_eq "a second run that finds nothing exits 0" "0" "$rc"
  assert_contains "and reports nothing left" "$output" \
    "Stack CRDs left: 0; stack namespaces left: 0; cluster-scoped chart objects left: 0"
  assert_not_contains "and deletes no ControlPlane without the c5c3 CRD" \
    "$(cat "$CALL_LOG")" "delete controlplane"
  assert_not_contains "and deletes no OVNCentral without the ovn CRD" \
    "$(cat "$CALL_LOG")" "delete ovncentrals"
  assert_not_contains "and patches no OpenBao instance that is gone" \
    "$(cat "$CALL_LOG")" "patch openbaoclusters"
  # Step 6 deletes the flux-operator ClusterRoles on every run; the chart
  # objects are named with their group.
  assert_not_contains "and deletes no chart object when the read finds none" \
    "$(cat "$CALL_LOG")" "delete clusterrole.rbac.authorization.k8s.io/"
  assert_not_contains "and does not print kubectl's empty-read notice" "$output" "No resources found"
  assert_contains "and still deletes the cert-manager Leases, which --ignore-not-found lets pass" \
    "$(cat "$CALL_LOG")" \
    "kubectl delete lease cert-manager-cainjector-leader-election cert-manager-controller -n kube-system"
  assert_not_contains "and, without the HelmRelease kind, reads no pod for the NFS wait" \
    "$(cat "$CALL_LOG")" "get pods -A"
  assert_not_contains "and no PersistentVolume" "$(cat "$CALL_LOG")" "get pv "
  assert_contains "and its NFS overlay delete, whose HelmRelease kind has no mapping, passes" \
    "$(cat "$CALL_LOG")" "kubectl delete -k $PROJECT_ROOT/deploy/lab/metal-stack/nfs "

  # The chart objects cannot be read: exit 1 after the stack namespaces, with
  # kubectl's error, and before any CRD is deleted.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CHART_OBJECTS_RC=1)"
  rc=$?
  assert_eq "a chart-object read that fails exits 1" "1" "$rc"
  assert_contains "says the chart objects cannot be read" "$output" \
    "cannot read the cluster-scoped objects of the stack's charts"
  assert_contains "and quotes kubectl's error" "$output" "clusterroles.rbac.authorization.k8s.io is forbidden"
  assert_contains "in step 7, after the stack namespaces are deleted" "$(cat "$CALL_LOG")" \
    "kubectl delete namespace $(stack_namespace_names)"
  assert_not_contains "and deletes no CRD" "$(cat "$CALL_LOG")" "delete customresourcedefinition"

  # The delete of the chart objects runs out: exit 1, and the CRDs stay.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CHART_OBJECT_DELETE_RC=1)"
  rc=$?
  assert_eq "a chart-object delete that runs out exits 1" "1" "$rc"
  assert_contains "the error names the chart-object step" "$output" \
    "deleting the cluster-scoped objects the stack's charts left behind failed or did not finish within 600s"
  assert_not_contains "and no CRD is deleted afterwards" "$(cat "$CALL_LOG")" "delete customresourcedefinition"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_OPENBAO_PATCH_RC=1)"
  rc=$?
  assert_nonzero_exit "an OpenBao instance that cannot be switched to DeletePVCs exits non-zero" "$rc"
  assert_contains "with kubectl's error" "$output" 'openbaoclusters.openbao.org "openbao-instance" is forbidden'
  # Step 0 deletes the hypervisor overlays before the patch; no other overlay goes.
  assert_eq "before any later overlay is deleted" "" \
    "$(grep 'kubectl delete -k' "$CALL_LOG" | grep -v '/deploy/lab/metal-stack/hypervisor' || true)"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_OVNCENTRAL_DELETE_RC=1)"
  rc=$?
  assert_eq "an OVNCentral delete that runs out exits 1" "1" "$rc"
  assert_contains "the error names the OVNCentral step" "$output" \
    "deleting the OVNCentrals in openstack failed or did not finish within 600s"
  assert_contains "the error names the OVNCentral still present" "$output" \
    "error: timed out waiting for the condition on ovncentrals/controlplane-ovn"
  assert_eq "and no later step deletes an overlay" "" \
    "$(grep 'kubectl delete -k' "$CALL_LOG" | grep -v '/deploy/lab/metal-stack/hypervisor' || true)"

  # A Lease delete the cluster refuses: exit 1 before step 8 deletes a CRD.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_LEASE_DELETE_RC=1)"
  rc=$?
  assert_eq "a refused Lease delete exits 1" "1" "$rc"
  assert_contains "the error names the Lease step" "$output" \
    "deleting the cert-manager leader election Leases in kube-system failed or did not finish within 600s"
  assert_contains "and quotes kubectl's error" "$output" \
    'leases.coordination.k8s.io "cert-manager-controller" is forbidden'
  assert_not_contains "and no CRD is deleted" "$(cat "$CALL_LOG")" "delete customresourcedefinition"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 6: step 0, the lab hypervisors
# ---------------------------------------------------------------------------
test_hypervisor_step_zero() {
  echo "Test: step 0 removes the lab hypervisors only where the overlay and the CRDs have them"

  if ! have_yq; then
    echo "  SKIP: yq not installed (40 checks skipped)"
    SKIP=$((SKIP + 40))
    return
  fi

  local tmp output rc calls
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  # An overlay without hypervisor/: step 0 reads and deletes nothing.
  mkdir -p "$tmp/overlay/base" "$tmp/overlay/infrastructure"
  : >"$tmp/overlay/base/kustomization.yaml"
  : >"$tmp/overlay/infrastructure/kustomization.yaml"
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY="$tmp/overlay")"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "an overlay without hypervisor/ tears down as before" "0" "$rc"
  assert_eq "and step 0 makes no call" "" \
    "$(grep -E -e '^kubectl (get crd|delete) (novacomputes|neutronmetadataagents|ovnchassis|hypervisors|evictions|migrations)\.' \
      -e 'domains\.openstack' -e 'delete -k [^ ]*/hypervisor' -e 'maint-' -e '^kubectl (get|label|annotate) nodes' \
      <<<"$calls" || true)"
  assert_eq "the ControlPlane delete still comes first" "kubectl delete controlplane --all -n openstack" \
    "$(mutations "$CALL_LOG" | head -n 1)"

  # A step 0 kind whose CRD is absent is skipped without an error.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true \
    KUBECTL_ABSENT_CRDS="hypervisors.kvm.cloud.sap novacomputes.nova.openstack.c5c3.io")"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "absent step 0 CRDs do not fail the teardown" "0" "$rc"
  assert_not_contains "no Hypervisor is deleted without its CRD" "$calls" "delete hypervisors.kvm.cloud.sap"
  assert_not_contains "no NovaCompute is deleted without its CRD" "$calls" "delete novacomputes"
  assert_contains "the Evictions, whose CRD exists, still are" "$calls" "delete evictions.kvm.cloud.sap --all"

  # A pool that still holds servers: the wait runs out, the hint follows the
  # error, and nothing after it is deleted.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_NOVACOMPUTE_DELETE_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a NovaCompute delete that runs out exits 1" "1" "$rc"
  assert_contains "the error names the NovaCompute still present" "$output" \
    "error: timed out waiting for the condition on novacomputes/lab"
  assert_eq "the last line is the servers hint" \
    "Delete the servers on the lab hypervisors first (openstack server list --all-projects)." \
    "$(tail -n 1 <<<"$output" | sed 's/^\[[^]]*\] //')"
  assert_not_contains "no ControlPlane is deleted" "$calls" "delete controlplane"
  assert_not_contains "and no later step 0 delete runs" "$calls" "delete -k"

  # The read of the domain fails (a timeout, an API server restarting): exit
  # 1 before the fixtures are deleted. An enabled domain whose delete has
  # begun stays: Keystone refuses the delete, and K-ORC applies no spec change
  # to an object that is being deleted.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_DOMAIN_GET_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a domain read that fails exits 1" "1" "$rc"
  assert_contains "the error names the domain" "$output" \
    "ERROR: cannot read the fixtures' domain hvo-cc3test:"
  assert_contains "and quotes kubectl's error" "$output" \
    "Error from server (Timeout): the server was unable to return a response"
  assert_not_contains "no fixture or overlay is deleted" "$calls" "delete -k"
  assert_not_contains "no ControlPlane is deleted" "$calls" "delete controlplane"

  # K-ORC does not disable the domain in time: exit 1 before the fixtures,
  # the overlay or the ControlPlane are deleted.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_DOMAIN_WAIT_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a domain that stays enabled exits 1" "1" "$rc"
  assert_contains "the error names the domain" "$output" \
    "ERROR: K-ORC did not disable the domain hvo-cc3test within 600s:"
  assert_contains "and quotes kubectl's error" "$output" \
    "error: timed out waiting for the condition on domains/hvo-cc3test"
  assert_not_contains "no fixture or overlay is deleted" "$calls" "delete -k"
  assert_not_contains "no ControlPlane is deleted" "$calls" "delete controlplane"

  # A rerun after a partial delete: the domain is gone, the image and the
  # flavor may not be. The fixtures are deleted without the disable.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_DOMAIN_ABSENT=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a domain already gone does not fail the teardown" "0" "$rc"
  assert_not_contains "nothing patches the domain" "$calls" "patch domains"
  assert_not_contains "nothing waits for it" "$calls" "wait domains"
  assert_contains "the fixtures are still deleted" "$calls" \
    "kubectl delete -k $PROJECT_ROOT/deploy/lab/metal-stack/hypervisor-fixtures "

  # The same rerun while kubectl writes to stderr and exits 0: only the
  # domain's name in the read counts, not the noise beside it.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_DOMAIN_ABSENT=1 KUBECTL_DOMAIN_NOISE=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "stderr beside a domain already gone does not fail the teardown" "0" "$rc"
  assert_not_contains "nothing patches the absent domain" "$calls" "patch domains"

  # A domain that exists is still disabled while kubectl writes to stderr.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_DOMAIN_NOISE=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "stderr beside the domain does not fail the teardown" "0" "$rc"
  assert_contains "the domain is still disabled" "$calls" \
    "kubectl patch domains.openstack.k-orc.cloud hvo-cc3test -n openstack"

  # The nodes cannot be listed: exit 1 before the maint- objects, the labels
  # and the ControlPlane.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_NODES_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a node list that fails exits 1" "1" "$rc"
  assert_contains "the error says the nodes cannot be listed" "$output" \
    "ERROR: cannot list the nodes (kubectl's error is above)."
  assert_not_contains "no maint- object is deleted" "$calls" "maint-"
  assert_not_contains "no node label is removed" "$calls" "label nodes"
  assert_not_contains "no ControlPlane is deleted" "$calls" "delete controlplane"

  # A second run: the CRDs and the domain are gone.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_SECOND_RUN=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a second run exits 0" "0" "$rc"
  assert_eq "it deletes none of the six step 0 kinds" "" \
    "$(grep -E '^kubectl delete (novacomputes|neutronmetadataagents|ovnchassis|hypervisors|evictions|migrations)' <<<"$calls" || true)"
  assert_not_contains "it patches no domain" "$calls" "patch domains"
  assert_contains "its fixtures delete finds nothing and passes" "$calls" \
    "kubectl delete -k $PROJECT_ROOT/deploy/lab/metal-stack/hypervisor-fixtures "
  assert_contains "it reads the CRDs it would delete from" "$calls" "get crd hypervisors.kvm.cloud.sap"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 7: Chaos Mesh, before step 0
# ---------------------------------------------------------------------------
test_chaos_mesh_step() {
  echo "Test: the Chaos Mesh step releases the faults first and stops on what it cannot read, delete or render"

  if ! have_yq; then
    echo "  SKIP: yq not installed (35 checks skipped)"
    SKIP=$((SKIP + 35))
    return
  fi

  local tmp output rc calls scope_read overlay_delete hint
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"
  scope_read='kubectl get crd -o custom-columns=NAME:.metadata.name,SCOPE:.spec.scope --no-headers'
  overlay_delete='[kinds: DaemonSet HelmRelease HelmRepository]'
  hint="A Chaos Mesh experiment keeps its finalizer until chaos-controller-manager has released its fault. Read 'kubectl logs -n chaos-mesh deployment/chaos-controller-manager' before rerunning; do not remove the finalizer by hand, the fault would stay injected."

  # An experiment whose fault is not released in time: kubectl's error names
  # it, the hint follows, and nothing after it is deleted.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CHAOS_DELETE_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "an experiment delete that runs out exits 1" "1" "$rc"
  assert_contains "the error names the experiment step" "$output" \
    "deleting the Chaos Mesh experiments failed or did not finish within 600s"
  assert_contains "and the object kubectl still waits for" "$output" \
    "error: timed out waiting for the condition on networkchaos/lab-keystone-delay"
  assert_eq "the last line is the finalizer hint" "$hint" \
    "$(tail -n 1 <<<"$output" | sed 's/^\[[^]]*\] //')"
  assert_contains "the schedules and workflows went first" "$calls" \
    "kubectl delete schedules.chaos-mesh.org,workflows.chaos-mesh.org --all -A --ignore-not-found"
  assert_not_contains "no cluster-scoped Chaos Mesh object is deleted after it" "$calls" "delete remoteclusters"
  assert_not_contains "the overlay is not rendered" "$calls" "kustomize"
  assert_not_contains "nor deleted" "$calls" "delete -f -"
  assert_not_contains "step 0 deletes nothing" "$calls" "delete novacomputes"
  assert_not_contains "nor does step 1" "$calls" "delete controlplane"

  # The scope read fails: exit 1 before any delete.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CHAOS_CRD_SCOPE_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a CRD scope read that fails exits 1" "1" "$rc"
  assert_contains "says the scope cannot be read" "$output" \
    "ERROR: cannot read the scope of the cluster's CRDs (kubectl's error is above)."
  assert_contains "below kubectl's error" "$output" "customresourcedefinitions.apiextensions.k8s.io is forbidden"
  assert_not_contains "before any delete" "$calls" "kubectl delete"

  # chaos-mesh/ does not render: exit 1 before its delete and before step 0.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CHAOS_RENDER_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a chaos-mesh/ that does not render exits 1" "1" "$rc"
  assert_contains "says which overlay cannot be rendered" "$output" \
    "ERROR: cannot render $PROJECT_ROOT/deploy/lab/metal-stack/chaos-mesh (kustomize's error is above)."
  assert_not_contains "no overlay delete follows" "$calls" "delete -f -"
  assert_not_contains "and no step 0 delete" "$calls" "delete novacomputes"

  # The helm-controller's uninstall outlives the overlay delete: exit 1 before
  # step 0, and before Flux, which clears the HelmRelease finalizer, goes.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CHAOS_OVERLAY_DELETE_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "an overlay delete that runs out exits 1" "1" "$rc"
  assert_contains "names the overlay step" "$output" \
    "deleting the Chaos Mesh overlay (HelmRelease, HelmRepository and module loader) failed or did not finish within 600s"
  assert_contains "and the HelmRelease kubectl still waits for" "$output" \
    "error: timed out waiting for the condition on helmreleases/chaos-mesh"
  assert_not_contains "step 0 deletes nothing" "$calls" "delete novacomputes"
  assert_not_contains "and Flux stays" "$calls" "delete fluxinstance"

  # A cluster without any chaos-mesh.org CRD: no experiment delete, and the
  # overlay delete passes on what is there.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CHAOS_ABSENT=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a cluster without Chaos Mesh CRDs tears down" "0" "$rc"
  assert_eq "without an experiment delete" "" \
    "$(grep -E '^kubectl delete [a-z]+\.chaos-mesh\.org' <<<"$calls" || true)"
  assert_contains "with the overlay delete" "$calls" "$overlay_delete"
  assert_contains "and nothing left" "$output" \
    "Stack CRDs left: 0; stack namespaces left: 0; cluster-scoped chart objects left: 0"

  # An overlay without chaos-mesh/: the step reads and deletes nothing.
  mkdir -p "$tmp/no-chaos/base" "$tmp/no-chaos/infrastructure"
  : >"$tmp/no-chaos/base/kustomization.yaml"
  : >"$tmp/no-chaos/infrastructure/kustomization.yaml"
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY="$tmp/no-chaos")"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "an overlay without chaos-mesh/ tears down" "0" "$rc"
  assert_eq "without the CRD scope read of the Chaos Mesh step" "0" "$(grep -cxF "$scope_read" <<<"$calls")"
  assert_eq "without an experiment delete or a chaos-mesh/ render" "" \
    "$(grep -E -e '^kubectl delete [a-z]+\.chaos-mesh\.org' -e '^kubectl kustomize .*/chaos-mesh$' <<<"$calls" || true)"
  assert_eq "and the ControlPlane delete is still the first mutation" \
    "kubectl delete controlplane --all -n openstack" "$(mutations "$CALL_LOG" | head -n 1)"

  # A second run: the Chaos Mesh CRDs are gone, and so is the HelmRelease kind.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_SECOND_RUN=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a second run exits 0" "0" "$rc"
  assert_eq "it deletes no experiment" "" \
    "$(grep -E '^kubectl delete [a-z]+\.chaos-mesh\.org' <<<"$calls" || true)"
  assert_contains "its overlay delete, whose HelmRelease kind has no mapping, passes" "$calls" "$overlay_delete"
  assert_not_contains "and it prints no 'No resources found'" "$output" "No resources found"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 7b: the dizzy stack, at the end of step 3
# ---------------------------------------------------------------------------
test_dizzy_step() {
  echo "Test: the dizzy step uninstalls both charts before it deletes the claims, and stops on a delete that runs out"

  if ! have_yq; then
    echo "  SKIP: yq not installed (28 checks skipped)"
    SKIP=$((SKIP + 28))
    return
  fi

  local tmp output rc calls releases repositories claims flags
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"
  flags='--ignore-not-found --wait --timeout=600s'
  releases="kubectl delete helmreleases.helm.toolkit.fluxcd.io dizzy-victoria-metrics dizzy-grafana -n dizzy $flags"
  repositories="kubectl delete helmrepositories.source.toolkit.fluxcd.io victoria-metrics grafana -n flux-system $flags"
  claims="kubectl delete pvc --all -n dizzy $flags"

  # The default overlay carries dizzy/. The stub answers the three deletes
  # with no output and exit 0, as a cluster deployed without WITH_DIZZY=true
  # does.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "the teardown exits 0" "0" "$rc"
  local monitoring_line releases_line repositories_line claims_line shared_line
  monitoring_line="$(grep -nF 'kubectl delete -f '"$PROJECT_ROOT"'/deploy/kind/prometheus/release.yaml ' <<<"$calls" | cut -d: -f1 | head -n1)"
  releases_line="$(grep -nxF "$releases" <<<"$calls" | cut -d: -f1 | head -n1)"
  repositories_line="$(grep -nxF "$repositories" <<<"$calls" | cut -d: -f1 | head -n1)"
  claims_line="$(grep -nxF "$claims" <<<"$calls" | cut -d: -f1 | head -n1)"
  shared_line="$(grep -nF 'kubectl delete pvc --all -n shared-services ' <<<"$calls" | cut -d: -f1 | head -n1)"
  assert_eq "the dizzy HelmReleases are deleted once, ignoring absence and waiting" "1" "$(grep -cxF "$releases" <<<"$calls")"
  assert_eq "the dizzy HelmRepositories are deleted once, ignoring absence and waiting" "1" "$(grep -cxF "$repositories" <<<"$calls")"
  assert_eq "the PVCs in dizzy are deleted once, ignoring absence and waiting" "1" "$(grep -cxF "$claims" <<<"$calls")"
  assert_eq "the HelmReleases go after the kube-prometheus-stack release" "true" \
    "$([[ -n "$monitoring_line" && -n "$releases_line" && "$monitoring_line" -lt "$releases_line" ]] && echo true || echo false)"
  assert_eq "the HelmRepositories after the HelmReleases" "true" \
    "$([[ -n "$releases_line" && -n "$repositories_line" && "$releases_line" -lt "$repositories_line" ]] && echo true || echo false)"
  assert_eq "the claims after the HelmRepositories" "true" \
    "$([[ -n "$repositories_line" && -n "$claims_line" && "$repositories_line" -lt "$claims_line" ]] && echo true || echo false)"
  assert_eq "and before the PVCs in shared-services" "true" \
    "$([[ -n "$claims_line" && -n "$shared_line" && "$claims_line" -lt "$shared_line" ]] && echo true || echo false)"
  assert_contains "the HelmRelease delete is logged" "$output" "Deleting the dizzy HelmReleases..."
  assert_contains "the claim delete is logged" "$output" "Deleting the PVCs in dizzy..."
  assert_contains "the namespace delete of step 7 names dizzy" \
    "$(grep -E '^kubectl delete namespace ' <<<"$calls" | grep -v '^kubectl delete namespace flux-system ')" " dizzy "
  assert_contains "the final count reads dizzy" \
    "$(grep -E '^kubectl get namespace .* --ignore-not-found -o name$' <<<"$calls")" " dizzy "
  assert_contains "and finds nothing left" "$output" \
    "Stack CRDs left: 0; stack namespaces left: 0; cluster-scoped chart objects left: 0"

  # An overlay without dizzy/: the step deletes nothing.
  mkdir -p "$tmp/no-dizzy/base" "$tmp/no-dizzy/infrastructure"
  : >"$tmp/no-dizzy/base/kustomization.yaml"
  : >"$tmp/no-dizzy/infrastructure/kustomization.yaml"
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY="$tmp/no-dizzy")"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "an overlay without dizzy/ tears down" "0" "$rc"
  assert_not_contains "without the HelmRelease delete" "$calls" "delete helmreleases.helm.toolkit.fluxcd.io dizzy-victoria-metrics"
  assert_not_contains "without the HelmRepository delete" "$calls" "delete helmrepositories.source.toolkit.fluxcd.io victoria-metrics"
  assert_not_contains "without the claim delete" "$calls" "delete pvc --all -n dizzy"

  # The helm-controller's uninstall outlives the HelmRelease delete: exit 1
  # with kubectl's line, and nothing after it is deleted, the claims and Flux
  # included.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_DIZZY_DELETE_RC=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a HelmRelease delete that runs out exits 1" "1" "$rc"
  assert_contains "the error names the step" "$output" \
    "ERROR: deleting the dizzy HelmReleases failed or did not finish within 600s:"
  assert_contains "and the HelmRelease kubectl still waits for" "$output" \
    "error: timed out waiting for the condition on helmreleases/dizzy-victoria-metrics"
  assert_not_contains "no HelmRepository delete follows" "$calls" "delete helmrepositories.source.toolkit.fluxcd.io victoria-metrics"
  assert_not_contains "no claim delete" "$calls" "delete pvc --all -n dizzy"
  assert_not_contains "no PVC delete of step 4" "$calls" "delete pvc --all -n shared-services"
  assert_not_contains "and Flux stays" "$calls" "delete fluxinstance"

  # A second run: the HelmRelease and HelmRepository kinds are gone.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_SECOND_RUN=1)"
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a second run exits 0" "0" "$rc"
  assert_contains "its HelmRelease delete, whose kind has no mapping, passes" "$calls" "$releases"
  assert_contains "and so does the claim delete after it" "$calls" "$claims"
  assert_not_contains "and it prints no 'No resources found'" "$output" "No resources found"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 8: yq is required
# ---------------------------------------------------------------------------
test_requires_yq() {
  echo "Test: the external teardown requires yq"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  ln -s "$(command -v dirname)" "$tmp/bin/dirname"
  ln -s "$(command -v date)" "$tmp/bin/date"
  export CALL_LOG="$tmp/calls.log"
  : >"$CALL_LOG"

  output="$(
    unset TEARDOWN_TIMEOUT
    EXTERNAL_CLUSTER=true PATH="$tmp/bin" "$BASH" "$TEARDOWN_SH" 2>&1
  )"
  rc=$?
  assert_nonzero_exit "the external teardown exits non-zero without yq" "$rc"
  assert_contains "names yq as missing" "$output" "'yq' is not installed"
  assert_not_contains "nothing is deleted" "$(cat "$CALL_LOG")" "kubectl delete"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 9: yq must be mikefarah/yq v4.40.1 or newer
# ---------------------------------------------------------------------------
test_requires_mikefarah_yq() {
  echo "Test: the external teardown requires mikefarah/yq v4.40.1 or newer"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  ln -s "$(command -v dirname)" "$tmp/bin/dirname"
  ln -s "$(command -v date)" "$tmp/bin/date"
  # The jq wrapper Debian packages as yq: it hands the expression to jq, which
  # does not know strenv.
  cat >"$tmp/bin/yq" <<'STUB'
#!/bin/bash
if [ "$1" = "--version" ]; then
  echo "yq 3.4.3"
  exit 0
fi
echo "jq: error: strenv/1 is not defined at <top-level>, line 1:" >&2
exit 3
STUB
  chmod +x "$tmp/bin/yq"
  export CALL_LOG="$tmp/calls.log"
  : >"$CALL_LOG"

  output="$(
    unset TEARDOWN_TIMEOUT
    EXTERNAL_CLUSTER=true PATH="$tmp/bin" "$BASH" "$TEARDOWN_SH" 2>&1
  )"
  rc=$?
  assert_nonzero_exit "the external teardown exits non-zero with the jq wrapper" "$rc"
  assert_contains "names the yq it wants" "$output" "is not mikefarah/yq v4.40.1 or newer"
  assert_contains "names the yq it found" "$output" "yq --version: yq 3.4.3"
  assert_not_contains "nothing is deleted" "$(cat "$CALL_LOG")" "kubectl delete"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_knobs
test_default_deletes_the_kind_cluster
test_external_teardown_order
test_script_names_no_platform_namespace
test_external_teardown_failures
test_hypervisor_step_zero
test_chaos_mesh_step
test_dizzy_step
test_requires_yq
test_requires_mikefarah_yq

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
