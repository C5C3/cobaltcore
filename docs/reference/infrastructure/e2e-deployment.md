---
title: Infrastructure E2E Deployment
quadrant: infrastructure
---

# Infrastructure E2E Deployment

Reference documentation for the infrastructure E2E deployment automation.
This feature provides shell-based orchestration to deploy the full infrastructure stack
(cert-manager, OpenBao, ESO, MariaDB Operator, Memcached Operator, infrastructure CRs,
ExternalSecrets) into a local kind cluster and validate it with Chainsaw E2E tests.

## Architecture Overview

Four make targets drive the deployment:

| Target | What it does |
| --- | --- |
| `make install-test-deps` | Installs chainsaw, kind and kubectl, and flux under `WITH_FLUX_CLI=true` |
| `make deploy-infra` | Runs the 8-step deployment into the kind cluster |
| `make e2e` | Runs the Chainsaw E2E tests against the cluster |
| `make teardown-infra` | Deletes the kind cluster |

A plain `make deploy-infra` on kind brings up three groups of the figure:
GitOps, Secrets & PKI, and Infrastructure. The releases of all service
operators are suspended on kind (`deploy/kind/base/kustomization.yaml`).
`WITH_CONTROLPLANE=true` un-suspends them and deploys the c5c3-operator and
K-ORC, and the OpenStack services appear once a service CR or a `ControlPlane`
is applied.

![The management cluster: GitOps (flux-operator, FluxInstance) and Secrets & PKI (cert-manager, OpenBao, External Secrets Operator) next to the c5c3-operator, whose ControlPlane CR creates infrastructure CRs, service CRs, and K-ORC resources. One service operator per service (keystone, horizon, glance, placement, barbican, neutron, cinder, nova, ovn) runs the OpenStack services, exposed via the Gateway API. The infrastructure (MariaDB Galera, Memcached, opt-in RabbitMQ, Garage S3) is managed by its own operators. Optional target clusters, registered via kubeconfig Secrets, receive projected service workloads.](../../diagrams/cobaltcore-management-cluster.svg)

## Prerequisites

| Prerequisite | Details |
| --- | --- |
| Docker | Running Docker daemon (kind uses Docker containers as nodes) |
| kubectl | Kubernetes CLI for cluster interaction |
| kind | Kubernetes IN Docker for local cluster creation |
| flux | **Optional** — the Flux CLI is no longer required by `make deploy-infra`; bootstrap uses flux-operator + FluxInstance. Opt in with `WITH_FLUX_CLI=true make install-test-deps` for ad-hoc `flux logs` debugging. |
| chainsaw | Kyverno Chainsaw for E2E test execution |
| jq | JSON processor used by deployment scripts |

`make install-test-deps` installs chainsaw, kind and kubectl, and flux on request;
Docker and jq come from the package manager of the system.

## Makefile Targets

### `make deploy-infra`

Deploys the full infrastructure stack to a kind cluster by running
`hack/deploy-infra.sh`. The script executes an 8-step deployment sequence
(see [Deployment Sequence](#deployment-sequence) below). Exits 0 on success,
non-zero on any failure with a descriptive error message.
`EXTERNAL_CLUSTER=true make deploy-infra` deploys onto the cluster the current
kubeconfig context points at instead, with the
[`deploy/lab/metal-stack/`](#lab-overlay-patches) overlay, and needs neither
Docker nor kind (see [`EXTERNAL_CLUSTER`](#environment-variables)).

### `make teardown-infra`

Deletes the kind cluster by running `hack/teardown-infra.sh`. Idempotent —
succeeds silently if no cluster exists. The kind teardown always exits 0.

`EXTERNAL_CLUSTER=true make teardown-infra` leaves the cluster in place and
removes the stack that `EXTERNAL_CLUSTER=true make deploy-infra` put on it. It
needs `kubectl` and [`yq`](https://github.com/mikefarah/yq) v4.40.1 or newer,
checks both before the first delete, and deletes in an order that lets every
finalizer run while its controller still exists.

When the overlay has a `chaos-mesh/` kustomization (see
[Lab Chaos Mesh](infrastructure-manifests.md#lab-chaos-mesh)), Chaos Mesh goes
first, while `chaos-controller-manager`, `chaos-daemon` and the
helm-controller still run. The teardown reads the scope of the
`chaos-mesh.org` CRDs and deletes the schedules and workflows, which create
experiments, then every other namespaced kind of the group, then its
cluster-scoped kinds. The per-pod records `podnetworkchaos`, `podiochaos` and
`podhttpchaos` are left out: the controller writes them while it releases a
fault, they carry no finalizer, and step 8 removes them with their CRDs. An
experiment keeps its finalizer until the controller has released its fault,
so a delete that runs out exits 1 with kubectl's error and
`A Chaos Mesh experiment keeps its finalizer until chaos-controller-manager has released its fault. ...`,
which names the controller's log and warns that a finalizer removed by hand
leaves the fault injected. Then it deletes the render of `<overlay>/chaos-mesh`
without its Namespace: the HelmRepository, the HelmRelease, whose finalizer
has the helm-controller uninstall the chart, and the DaemonSet
`chaos-mesh-modules`. The Namespace holds Helm's release Secret, so step 7
deletes it. A CRD read that fails exits 1 with
`ERROR: cannot read the scope of the cluster's CRDs (kubectl's error is above).`
before any delete, and a render that fails exits 1 with
`ERROR: cannot render <overlay>/chaos-mesh (kustomize's error is above).`
before the overlay delete. The NetworkChaos modules the loader put on the
nodes stay until a node reboots.

When the overlay has a `dizzy-soak/` kustomization (see
[Lab dizzy soak](infrastructure-manifests.md#lab-dizzy-soak)), the dizzy soak
goes next, before the hypervisors: its servers would keep a `NovaCompute`
from being finalized. The teardown deletes the Job `dizzy-soak` and the pod
`dizzy-soak-reader` in `dizzy` in the foreground. The runner gets TERM, dizzy
removes the run's servers, volumes, ports and networks, and the runner writes
its report, within the pod's grace period of 540 seconds. Then the teardown
deletes the render of `<overlay>/dizzy-soak`: the K-ORC identity of the soak,
while K-ORC and Keystone still run, its ServiceAccount, ClusterRole and
ClusterRoleBinding, and the claim `dizzy-soak-reports` with the reports. A
delete that runs out exits 1 with kubectl's error before step 0, and a render
that fails exits 1 with
`ERROR: cannot render <overlay>/dizzy-soak (kustomize's error is above).`
before its delete. Then:

0. The lab hypervisors, when the overlay has a `hypervisor/` kustomization
   (see [Lab hypervisors](infrastructure-manifests.md#lab-hypervisors)), while
   the ControlPlane and every operator still run. Each kind is deleted only
   where its CRD exists:
   1. every `NovaCompute`, `NeutronMetadataAgent` and `OVNChassis` in
      `openstack`. A `NovaCompute` keeps its finalizer
      `nova.openstack.c5c3.io/compute-drain` until every one of its nodes is
      released: no server on it, its pod gone and its compute service deleted,
      so a pool that still holds servers outlives the wait: the teardown
      exits 1 with the delete's error and the line
      `Delete the servers on the lab hypervisors first (openstack server list --all-projects).`;
   2. the `Hypervisor`, `Eviction` and `Migration` objects of `kvm.cloud.sap`,
      which Nodes own and garbage collection therefore never reaps;
   3. the fixtures, when the overlay has `hypervisor-fixtures/`. While the
      K-ORC domain `hvo-cc3test` exists, it is disabled first and the wait
      for K-ORC to apply that is bounded by `TEARDOWN_TIMEOUT`, because
      Keystone refuses to delete an enabled domain. The delete runs either
      way, so a rerun after a partial delete still removes the image and the
      flavor. A read of the domain that fails exits 1 before the delete:
      K-ORC applies no spec change to a domain that is being deleted, so one
      deleted while enabled could never be disabled;
   4. the hypervisor overlay, with the two operators, the libvirt DaemonSet
      and the migration port reservation;
   5. the `Deployment` and `PodDisruptionBudget` `maint-<node>` the hypervisor
      operator leaves in `kube-system`, for every node. Its lifecycle
      controller recreates them while it runs, so they go after sub-step 4;
   6. the labels `openstack.c5c3.io/chassis`,
      `openstack.c5c3.io/nova-compute-pool`,
      `nova.openstack.cloud.sap/virt-driver` and
      `cobaltcore.cloud.sap/node-hypervisor-lifecycle`, and the annotations
      `nova.openstack.cloud.sap/custom-traits` and
      `nova.openstack.cloud.sap/aggregates`, from every node.

   What the hypervisors left on the nodes, under `/var/lib/nova`,
   `/var/lib/libvirt` and `/etc/pki`, stays, and so do the reserved ports in
   `net.ipv4.ip_local_reserved_ports` until a node reboots.
1. Every `ControlPlane` in `openstack`, when the `controlplanes.c5c3.io` CRD
   exists, so the c5c3-operator reaps its children. Then every `OVNCentral` in
   `openstack`, when the `ovncentrals.ovn.openstack.c5c3.io` CRD exists: the
   quick start's `controlplane-ovn` is referenced by the ControlPlane, not
   owned by it, carries no finalizer, and its database pods would otherwise
   hold their PVCs through step 4.
2. The infrastructure overlay (`kubectl delete -k <overlay>/infrastructure`)
   and the opt-in `deploy/kind/messaging` overlay, while their operators run.
   The proving `OpenBaoCluster` `openbao-instance` is switched to
   `deletionPolicy: DeletePVCs` first. Under the default `Retain` the
   openbao-operator strips the owner references of the instance's unseal-key
   and root-token Secrets before it clears its finalizer, and it is allowed
   neither: its admission policy denies the patch on the ESO-materialized
   unseal key, and the tenant RBAC grants no read on the root token. The step
   ends with a wait, bounded by `TEARDOWN_TIMEOUT`, until no CR of the stack's
   namespaced CRDs in `openstack` is still being reaped, that is, carries a
   deletion timestamp or has lost an owner of a stack kind. The ControlPlane
   delete of step 1 returns when the ControlPlane is gone. Its finalizer
   deletes the co-located Keystone and waits for it first, so the Keystone and
   the backup PushSecrets its OpenBao finalizer purges are finalized by then.
   Garbage collection reaps the other children afterwards (the SecretStore, the
   dedicated Barbican OpenBao instance), and the Keystone too when the
   finalizer gave up on it at its deadline; their finalizers need the operators
   step 3 uninstalls. Without the wait a slow finalizer loses its controller
   and holds the namespace in `Terminating`. The base overlay's Gateway and
   the objects the deploy applies outside the overlays are nobody's children
   and are left to steps 3 and 7. One `kubectl get` reads every kind per pass,
   and a read that fails counts as objects left, not as an empty namespace.

   When the overlay has an `nfs/` kustomization (see
   [Lab NFS stack](infrastructure-manifests.md#lab-nfs-stack)), step 2 ends
   with the NFS stack, while the helm-controller still runs to uninstall its
   chart. While the HelmRelease `kube-system/csi-driver-nfs` exists, it first
   waits, bounded by `TEARDOWN_TIMEOUT`, until no pod in any namespace mounts
   an inline `nfs.csi.k8s.io` volume and no PersistentVolume of that driver is
   `Bound`. The kubelet unmounts such a volume through the
   `csi-nfs-node` pod, and a pod that garbage collection terminates after that
   pod is gone stays in `Terminating` with its mount on the node and holds its
   namespace through step 7. A pod that mounts a share through a claim, as the
   `nfs-health` probe does, is found by the PersistentVolume its claim binds;
   the teardown never deletes that namespace. A read of the pods or the
   PersistentVolumes that fails, or that `yq` cannot parse, counts as pods
   left. Without the release the stack runs no NFS CSI driver, so there is no
   wait, and the pods and claims of a driver the platform runs are left alone;
   a read of the release that fails exits 1. A wait that runs out exits 1 with
   `ERROR: pods or bound PersistentVolumes still use an nfs.csi.k8s.io volume after <n>s:`
   and one `<namespace>/<name>` line per pod and one
   `pv/<name> (claim <namespace>/<name>)` line per PersistentVolume, before
   anything of the stack is deleted. Then it deletes `<overlay>/nfs`: the
   HelmRelease `csi-driver-nfs` in `kube-system`, whose finalizer has the
   helm-controller uninstall the chart, the NFS server with its Service and
   claim, and the DaemonSet `nfs-client-modules`. The NetworkPolicy
   `nfs-server-clients` of `<overlay>/nfs/client-policy.yaml` follows the
   server it guarded, when the overlay ships that file. Last comes the
   `CSIDriver` with the labels
   `helm.toolkit.fluxcd.io/name=csi-driver-nfs` and
   `helm.toolkit.fluxcd.io/namespace=kube-system`, which the helm-controller
   sets on every object of that release, so a `CSIDriver` the platform ships
   is never named; once the uninstall has finished, the delete finds nothing.
   The kernel modules the stack's pods loaded stay on the nodes until they
   reboot.
3. The base overlay without its Namespaces and FluxInstance. Every suspended
   HelmRelease with a release history and every suspended Flux Kustomization
   with an inventory is resumed first, because Flux neither uninstalls a
   suspended release nor prunes a suspended Kustomization. The Gateway and
   GatewayClass go first, while Envoy Gateway clears their finalizer, and the
   opt-in `kube-prometheus-stack` goes after the rest, in three deletes: its
   HelmRelease, by the file `deploy/kind/prometheus/release.yaml`, whose
   finalizer has the helm-controller uninstall the chart, then the PVCs in
   `monitoring`, which Helm and the Prometheus Operator leave behind, then the
   Service and the Endpoints `kube-prometheus-stack-kubelet` in `kube-system`,
   which the Prometheus Operator writes for its `kubelet` job and no uninstall
   removes. They run whatever the overlay holds and find nothing on a cluster
   deployed without `WITH_PROMETHEUS=true` (see
   [Lab Prometheus stack](infrastructure-manifests.md#lab-prometheus-stack));
   a release delete that runs out exits 1 before any claim is deleted. When the
   overlay has a `dizzy/` kustomization (see
   [Lab dizzy stack](infrastructure-manifests.md#lab-dizzy-stack)), the dizzy
   stack follows, by name: the HelmReleases `dizzy-victoria-metrics` and
   `dizzy-grafana` in `dizzy`, whose finalizer has the helm-controller
   uninstall both charts, then the HelmRepositories `victoria-metrics` and
   `grafana` in `flux-system`, then the PVCs in `dizzy`, which Helm leaves
   behind; the soak's claim went before step 0. Where the default class has the
   reclaim policy `Delete`, the VictoriaMetrics volume goes with its claim. A
   HelmRelease delete that runs out exits 1 before any claim is deleted.
4. The PVCs in `shared-services` and `openstack`. The openbao-operator chart's
   admission policy denies deleting its managed PVCs until step 3 has
   uninstalled the chart.
5. The FluxInstance, so the flux-operator uninstalls the toolkit.
6. The `flux-system` namespace and the flux-operator's ClusterRoles and
   ClusterRoleBinding.
7. The namespaces of `deploy/flux-system/namespaces.yaml`, `envoy-gateway-system`,
   `headlamp-system`, `chaos-mesh` and `dizzy`. Then every ClusterRole, ClusterRoleBinding,
   MutatingWebhookConfiguration and ValidatingWebhookConfiguration whose label
   `helm.toolkit.fluxcd.io/namespace` names one of these namespaces or
   `flux-system`. The helm-controller sets that label, the namespace of the
   HelmRelease, on every object it renders, hooks included. Helm does not track
   a hook object as part of its release, so no uninstall removes one:
   `gateway-helm` 1.9.2 leaves the ClusterRole and the ClusterRoleBinding
   `envoy-gateway-gateway-helm-certgen:envoy-gateway-system` and the
   MutatingWebhookConfiguration
   `envoy-gateway-topology-injector.envoy-gateway-system`. The read runs once
   the namespaces are gone, when no HelmRelease is left to own such an object.
   Each object is logged as `Chart leftover: <kind>.<group>/<name>` before it is
   deleted, and a read that fails exits 1 with kubectl's error. Then the Leases
   `cert-manager-cainjector-leader-election` and `cert-manager-controller` in
   `kube-system`, on which cert-manager's cainjector and controller elect their
   leader. The chart keeps them outside the `cert-manager` namespace, so the
   namespace delete leaves them, and a cainjector deployed within the lease
   duration waits for the old holder's Lease before it injects the webhook's
   CA. They go after the namespaces because a cert-manager pod that still runs
   writes its Lease again.
8. The CRDs of the stack's API groups (the cert-manager, External Secrets,
   MariaDB, Memcached, OpenBao, Garage, RabbitMQ, Gateway API, Envoy Gateway,
   Prometheus Operator, CobaltCore, K-ORC, Flux and flux-operator groups,
   `kvm.cloud.sap`, whose CRDs the lab hypervisors' charts install and Helm
   leaves behind, and `chaos-mesh.org`, whose CRDs the Chaos Mesh chart
   installs from its `crds/` and Helm never deletes).

In `kube-system` it deletes only the `maint-<node>` objects of step 0, the
`csi-driver-nfs` HelmRelease of step 2 with its chart, the Service and the
Endpoints `kube-prometheus-stack-kubelet` of step 3, and the two cert-manager
Leases of step 7. It never names `firewall`, `metallb-system`
or `default`, nor a CRD of the platform (`autoscaling.k8s.io`,
`cert.gardener.cloud`, `dns.gardener.cloud`, `crd.projectcalico.org`,
`metallb.io`). Every delete ignores absence, so a second run finds nothing and
exits 0. A delete that does not finish within `TEARDOWN_TIMEOUT` seconds exits
1 with the objects kubectl names, and so does a final count of stack CRDs,
stack namespaces or cluster-scoped chart objects above zero. Each chart object
still present is logged as `Still present: <kind>.<group>/<name>`. The teardown
selects the chart objects by label and names none of them: the Envoy Gateway
release floats inside `>=1.9.2 <2.0.0`, so Flux can install a chart whose hook
objects carry other names.

The startup API check and the Lease delete were run on the metal-stack lab
(shoot `forge`) on 2026-10-03, from commit `36a34e4b`.
`EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true make deploy-infra` exited 0, and
`kubectl get events -n cert-manager --field-selector involvedObject.name=cert-manager-startupapicheck`
listed the Job's `Completed` event. In that deploy cainjector waited 87 seconds
for the Lease an earlier stack had left in `kube-system`, and the cert-manager
release was `Ready` 97 seconds after its pods started.
`EXTERNAL_CLUSTER=true make teardown-infra` exited 0, and
`kubectl get lease -n kube-system cert-manager-cainjector-leader-election cert-manager-controller`
answered `NotFound` for both names. The same deploy, started one second after
the teardown's `=== Done ===` line, exited 0, and none of its 22 HelmReleases
had a `Stalled` condition with status `True`. Its cainjector took the Lease on
the first attempt, and the cert-manager release was `Ready` 18 seconds after
its pods started.

### `make install-test-deps`

Installs pinned versions of chainsaw, flux, kind, and kubectl by running
`hack/install-test-deps.sh`. Idempotent — skips tools already installed at the
correct version. Installs to `$INSTALL_DIR` (default: `~/.local/bin`).

### `make e2e`

Runs all Chainsaw E2E tests: `chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/`.
Produces JUnit XML reports in `_output/reports/`.

## Deployment Sequence

`hack/deploy-infra.sh` runs eight steps and logs each as
`=== Step n/8: ... ===`. The figure shows them with the waits between them,
the two cluster modes and the step each opt-in changes. The list under the
figure describes every step under its number.

![The run of make deploy-infra as eight numbered steps from top to bottom, with the opt-ins beside the step each one changes. Before Step 1 the script runs its preflight checks and, in kind mode, loads the kernel modules some opt-ins need. Step 1 forks: kind mode creates the cluster from hack/kind-config.yaml or keeps the one that exists, and EXTERNAL_CLUSTER=true creates none and checks the cluster of the current context. Step 2 installs the flux-operator, the Namespaces and the FluxInstance, waits for the FluxInstance to be Ready, and installs the Gateway API and Envoy Gateway CRDs. Step 3 applies the base overlay, deploy/kind/base or base/ of EXTERNAL_OVERLAY. Step 4 waits for the releases in four phases: cert-manager and its webhook, the four TLS prerequisites, the infrastructure releases, and the Kustomization rabbitmq-cluster-operator. Once the operator CRDs are registered, Step 5 applies the infrastructure overlay and waits for the Gateway openstack-gw to be Programmed. Step 6 waits for the OpenBao pods to run. Step 7 initialises, unseals and configures OpenBao and waits for its pods to be Ready. Step 8 waits for the ExternalSecrets keystone-admin, keystone-db and mariadb-root-password. After Step 8 the script waits for the proving OpenBao instance, for Garage and, without a ControlPlane, for the MariaDB openstack-db. The opt-in boxes: KIND_HOST_PORT, KIND_CONFIG, SKIP_KIND_CREATE and WITH_REGISTRY_CACHE act on the cluster creation. WITH_CHAOS_MESH, WITH_PROMETHEUS, WITH_DIZZY, WITH_NFS, WITH_METRICS_SERVER and WITH_VPA each add an overlay in Step 3 and a release to the wait of Step 4. WITH_CONTROLPLANE resumes the service-operator releases in Step 3, leaves MariaDB, Memcached and the three ExternalSecrets out of Step 5, skips the wait of Step 8, and ends with the operator stack Ready and a dry-run of the ControlPlane admitted. WITH_MESSAGING adds a RabbitMQ broker after Step 5.](../../diagrams/deploy-infra-run.svg)

Before Step 1 the script runs its preflight checks: the tools on the `PATH`
(`docker`, `kind`, `kubectl` and `jq`, under `EXTERNAL_CLUSTER=true` only
`kubectl` and `jq`), the flag combinations it refuses, and under
`EXTERNAL_CLUSTER=true` the directories of the overlay. In kind mode it then
runs `modprobe` on the host for `WITH_CHAOS_MESH`, `WITH_OVN_KERNEL_MODULES`
and `WITH_NFS`.

1. **Create or check the cluster.** In kind mode the script creates the
   cluster from `hack/kind-config.yaml`, or keeps a cluster of that name, and
   then caps the `RLIMIT_NOFILE` of containerd on every node. Under
   `EXTERNAL_CLUSTER=true` it creates none and checks the current context for
   a default StorageClass, a Ready node and no `node-local-dns` DaemonSet.
   Under `WITH_NFS=true` it also checks for no `CSIDriver` `nfs.csi.k8s.io` of
   another installer and for a node network that holds every node. Before
   Step 2 the script refuses a cluster deployed before the relocation to
   `shared-services`, and under `WITH_REGISTRY_CACHE=true` it starts the
   pull-through caches.
2. **Install Flux.** `kubectl apply -f` of the flux-operator `install.yaml`,
   of `deploy/flux-system/namespaces.yaml` and of
   `deploy/flux-system/fluxinstance.yaml`. `wait_for_fluxinstance` polls the
   `Ready` condition for `HELMRELEASE_TIMEOUT` seconds. Two CRD installs
   follow, each a `kubectl apply --server-side` of an upstream file:
   - Install Gateway API standard CRDs, from `standard-install.yaml` of
     `GATEWAY_API_VERSION`, whose default matches `go.mod`. The
     keystone-operator watches `HTTPRoute`. The install is skipped when all
     ten standard-channel CRDs of the pinned bundle exist at that version, a
     newer one or an unversioned one. It never downgrades, and it upgrades a
     complete set that is older than the pin in place. The bundle also ships
     the `safe-upgrades` ValidatingAdmissionPolicy, which denies applying
     experimental-channel CRDs over the standard channel.
   - Install Envoy Gateway CRDs (`gateway.envoyproxy.io`), from
     `envoy-gateway-crds.yaml` of `ENVOY_GATEWAY_VERSION`, which stays inside
     the SemVer range of the `envoy-gateway` chart. The HelmRelease runs with
     `crds.enabled: false`: its bundled CRD copy carries the experimental
     Gateway API channel, which the policy above refuses. This install is
     therefore the only owner of the group. It is skipped when all eight CRDs
     exist, because they carry no version annotation to compare.
3. **Apply the base overlay**, `deploy/kind/base/` or `base/` of
   `EXTERNAL_OVERLAY`: the Namespaces, the Flux sources, the HelmReleases and
   the two Flux Kustomizations of `deploy/flux-system/`, and what the kind
   overlay adds, among it the `envoy-gateway` HelmRelease, `GatewayClass/envoy`
   and `Gateway/openstack-gw` on port 443. The production
   `deploy/flux-system/` ships no Gateway, and the lab overlay inherits this
   one. Still in Step 3:
   - Each of `WITH_CHAOS_MESH`, `WITH_PROMETHEUS`, `WITH_DIZZY` and `WITH_NFS`
     applies the directory of its name under `deploy/kind/` or under
     `EXTERNAL_OVERLAY`. `WITH_METRICS_SERVER` and `WITH_VPA` apply
     `deploy/kind/metrics-server` and `deploy/kind/vpa`. Under
     `EXTERNAL_CLUSTER=true` the script waits for the rollout of the
     DaemonSets `chaos-mesh-modules` and `nfs-client-modules`, and under
     `WITH_NFS=true` in both modes for the rollout of `nfs-server`.
   - `WITH_CONTROLPLANE=true` with `CONTROLPLANE_OPERATORS=flux` resumes the
     nine service-operator HelmReleases. Every other run suspends the
     `c5c3-operator` HelmRelease and the `k-orc` Kustomization with their
     sources. `INFRA_ONLY=true` suspends all ten operator HelmReleases and
     scales their Deployments to zero.
4. **Wait for the releases**, in four phases:
   - Phase 1: `cert-manager` is Ready (`HELMRELEASE_TIMEOUT`). Then the
     cert-manager webhook has to admit a server-side dry-run of
     `cluster-issuer.yaml` (`WEBHOOK_TIMEOUT`). On an install the startup API
     check of the release already holds Ready until the webhook admits a
     request. The dry-run is the script's own gate for an upgrade, which runs
     no hook, and for an overlay that turns the check off.
   - Phase 2: `kubectl apply -f` of the four TLS prerequisites
     `cluster-issuer.yaml`, `openbao-ca-issuer.yaml`, `openbao-tls-cert.yaml`
     and `db-ca-issuer.yaml` from `deploy/flux-system/infrastructure/`.
     OpenBao and MariaDB cannot start without them.
   - Phase 3: `prometheus-operator-crds`, `openbao`, `mariadb-operator-crds`,
     `mariadb-operator`, `external-secrets`, `memcached-operator`,
     `envoy-gateway`, `garage-operator` and `openbao-operator` are Ready
     (`HELMRELEASE_TIMEOUT`), and with them the release of every opt-in
     overlay of Step 3. `WITH_PROMETHEUS=true` raises the wait to at least
     1200 seconds.
   - Phase 3b: `kustomization/rabbitmq-cluster-operator` is Ready. The
     RabbitMQ Cluster Operator arrives as a Flux Kustomization, which the
     HelmRelease wait cannot see. This wait fails the run on every cluster,
     because a ControlPlane projects a `RabbitmqCluster` for
     `spec.infrastructure.messaging`. The script then requires every image in
     `rabbitmq-system` to be pinned by digest. Under `WITH_PROMETHEUS=true` it
     turns on the ServiceMonitor of the nine service operators.
5. **Apply the infrastructure overlay**, `deploy/kind/infrastructure/` or
   `infrastructure/` of `EXTERNAL_OVERLAY`. The step starts with
   `wait_for_crds` (`POD_TIMEOUT`) on ten operator CRDs: `memcacheds`,
   `externalsecrets`, `clustersecretstores`, `mariadbs`, `envoyproxies`, the
   three Garage kinds, `openbaoclusters` and `rabbitmqclusters.rabbitmq.com`.
   The overlay holds the ClusterIssuers, the CA certificates, the Gateway
   certificates that `selfsigned-cluster-issuer` signs (the one for
   `keystone.127-0-0-1.nip.io` among them), the NodePort `EnvoyProxy` on
   31443, the MariaDB, Memcached and Garage CRs, the proving `OpenBaoCluster`
   and the ESO resources. Under `WITH_CONTROLPLANE=true` the script applies
   the render without `MariaDB`, `Memcached` and the three ExternalSecrets of
   Step 8. Under `WITH_MESSAGING=true` it then applies `deploy/kind/messaging`
   and waits for `rabbitmqcluster/shared-rabbitmq`. The step ends with
   `wait_for_gateway_programmed`, which polls `Programmed=True` on
   `Gateway/openstack-gw` (`HELMRELEASE_TIMEOUT`): the Gateway is programmed
   only once the `EnvoyProxy` of this overlay exists.
6. **Wait for the OpenBao pods** to reach the phase `Running`
   (`POD_TIMEOUT`). A sealed pod is not Ready yet.
7. **Bootstrap OpenBao.** `openbao_init_unseal` initialises and unseals it.
   `openbao_bootstrap` then runs `setup-secret-engines.sh`, `setup-auth.sh`,
   `setup-policies.sh` and `write-bootstrap-secrets.sh` from
   `deploy/openbao/bootstrap/`. Under `WITH_CONTROLPLANE=true` the last one
   seeds the paths of `openstack/<CONTROLPLANE_NAME>`. The step ends when the
   OpenBao pods are Ready (`POD_TIMEOUT`), and the script asks ESO to validate
   `openbao-cluster-store` again.
8. **Wait for the ExternalSecrets** `keystone-admin`, `keystone-db` and
   `mariadb-root-password` to sync (`EXTERNALSECRET_TIMEOUT`). The script
   first provisions the tenant store of the `openstack` namespace with
   `setup-eso-tenant.sh`. Under `WITH_CONTROLPLANE=true` the step waits for
   nothing: Step 5 left the three out, and the c5c3-operator projects the
   credentials of each ControlPlane.

After Step 8 the script un-pauses the proving `OpenBaoCluster` and waits for
it to be `Available`, and it waits for the Garage ExternalSecrets and for
`garagecluster/garage` to reach the phase `Running`. A standalone run ends
when `mariadb/openstack-db` is Ready.

A run under `WITH_CONTROLPLANE=true` ends with the ControlPlane admission
instead. On the `flux` path, unless `INFRA_ONLY=true`, it waits for
`kustomization/k-orc`, for the ten operator HelmReleases
(`HELMRELEASE_TIMEOUT`) and for one CRD per operator (`POD_TIMEOUT`). Then a
server-side dry-run of the ControlPlane manifests has to be admitted or
denied by the API server (`WEBHOOK_TIMEOUT`). Under `WITH_CONTROLPLANE_CR=true`
the script probes the bundled CR and applies it once. Otherwise it probes the
render of the overlay's `controlplane/` directory, or the kind
`controlplane.yaml`, and prints the command that applies it by hand.

**ExternalSecret shims of the kind overlay.** The `keystone-admin`,
`keystone-db` and `mariadb-root-password` ExternalSecrets of Step 8 come from
`deploy/kind/infrastructure/`, and the lab overlay inherits them. The
production base has none of them. The production `deploy/eso/` stack ships only
the `ClusterSecretStore`: in a ControlPlane-based deployment the admin password
is projected per ControlPlane by the c5c3-operator, and a non-kind Flux MariaDB
baseline provides the `mariadb-root-password` Secret itself.

**Why two-phase kustomize?** The base kustomization holds Namespaces and
resources whose CRDs Step 2 has registered: the Flux sources, the HelmReleases,
the two Flux Kustomizations, and on kind the GatewayClass and the Gateway. The
infrastructure kustomization holds resources of the operators those releases
install (ClusterIssuer, MariaDB CR, Memcached CR). Applying them in two phases
prevents `kubectl apply` failures on fresh clusters where the operator CRDs do
not yet exist.

### Steps and phases

This page and [Infrastructure Manifests](infrastructure-manifests.md) count
in five ways:

| Term | Where | Means | In the run above |
| --- | --- | --- | --- |
| Step 1 to Step 8 | this page, the log of the script | the steps of `make deploy-infra` | the numbered boxes |
| Phase 1, 2, 3 and 3b | this page, the log of the script | the four parts of the release wait | inside Step 4 |
| base phase and infrastructure phase, "two-phase kustomize" | both pages | the two kustomize applies | Step 3 and Step 5 |
| "Step 1: Apply base resources", "Step 2: Apply infrastructure resources" | [Deployment](infrastructure-manifests.md#deployment) | the same two applies, by hand on `deploy/flux-system/` | Step 3 and Step 5 |
| step 0 to step 8, lower case | [`make teardown-infra`](#make-teardown-infra) | the order of the teardown | not part of the run |

### Idempotent Re-runs

Re-running `make deploy-infra` against an existing cluster converges rather than
failing — each step detects the work it already completed and skips it:

- **Step 1** skips cluster creation when the kind cluster already exists.
- The **containerd nofile cap** skips the write, the containerd restart, and the
  node-Ready wait only when the node's drop-in *and* the limit the running
  containerd reports both already match. Checking the drop-in alone would
  permanently skip a node whose write landed but whose restart failed, leaving
  containerd uncapped behind a clean-looking deploy.
- **Gateway API CRDs** are skipped when all ten standard-channel CRDs are
  present (see the sequence above).
- The **kustomize overlays** and **TLS prerequisites** re-apply convergently
  (`kubectl apply` / upserts).
- **OpenBao init, unseal, and bootstrap** detect completed work: initialized
  and unsealed checks, enable-if-missing for engines/auth/policies, and
  write-if-missing for the bootstrap secrets.
- Additional `WITH_*` opt-ins on a re-run install only the newly enabled
  components; the already-deployed ones are left untouched.
- Removing a previously enabled flag does **not** uninstall that component —
  cleanup is `make teardown-infra`'s job.
- `WITH_REGISTRY_CACHE` needs a cluster created with the flag; enabling it on a
  pre-existing cluster leaves the mirrors inert and prints a warning.
- `WITH_CONTROLPLANE` on a provisioned standalone stack is a mode change (the
  ControlPlane owns MariaDB/Memcached), not supported as a re-run — start from a
  fresh cluster.

## Kustomize Overlay Structure

The lab overlay takes the kind overlay as its base, and the kind overlay takes
the production manifests. The figure shows that chain and who applies each
directory. The table lists every directory under `deploy/` that holds a
`kustomization.yaml`, with the bases it lists under `resources`.

![The kustomize overlays under deploy/ in three columns: production, kind and the lab on metal-stack. An arrow runs from a directory to the overlay that takes it as its base. deploy/flux-system is the base of deploy/kind/base, which is the base of deploy/lab/metal-stack/base. deploy/flux-system/infrastructure, which includes deploy/eso, is the base of deploy/kind/infrastructure, which is the base of deploy/lab/metal-stack/infrastructure. The kind overlays add Envoy Gateway, the Gateway, the certificates and the ExternalSecrets and patch the stack down to one node. The lab overlays remove the storage class and label the Namespaces. The four opt-in directories chaos-mesh, dizzy, nfs and prometheus exist under deploy/kind, and the lab directory of the same name takes each as its base. The lab hypervisor-fixtures take the kind hypervisor-operator-fixtures as their base. Without a base are metrics-server, vpa, messaging, controlplane and fake-compute under deploy/kind, and controlplane, probe, migration-ports, dizzy-soak, hypervisor, ceph and ceph/cluster under the lab. A person applies the two production directories, the fixtures, the controlplane directories, fake-compute and the lab directories without a base with kubectl, except dizzy-soak, which make dizzy-soak-start applies, and ceph and ceph/cluster, which make deploy-infra applies in Steps 3 and 5 under WITH_CEPH=true. make deploy-infra applies the base overlay in Step 3 and the infrastructure overlay in Step 5, from the kind column or, under EXTERNAL_CLUSTER=true, from the lab column. deploy/examples/sizing-overlay is a template for a production overlay of the same shape.](../../diagrams/deploy-overlay-inheritance.svg)

| Directory | Base | Applied by |
| --- | --- | --- |
| `deploy/flux-system` | none | a person, with `kubectl apply -k` ([Deployment](infrastructure-manifests.md#deployment)) |
| `deploy/flux-system/infrastructure` | `deploy/eso` | a person, once the operators have installed their CRDs |
| `deploy/eso` | none | nothing on its own |
| `deploy/examples/sizing-overlay` | `deploy/flux-system` | nothing: a template to copy ([Sizing and placement overrides](infrastructure-manifests.md#sizing-and-placement-overrides)) |
| `deploy/examples/sizing-overlay/infrastructure` | `deploy/flux-system/infrastructure` | nothing: the second phase of that template |
| `deploy/kind/base` | `deploy/flux-system` | `make deploy-infra`, Step 3 |
| `deploy/kind/infrastructure` | `deploy/flux-system/infrastructure` | `make deploy-infra`, Step 5 |
| `deploy/kind/chaos-mesh` | none | Step 3 under `WITH_CHAOS_MESH=true` |
| `deploy/kind/dizzy` | none | Step 3 under `WITH_DIZZY=true` |
| `deploy/kind/nfs` | none | Step 3 under `WITH_NFS=true` |
| `deploy/kind/prometheus` | none | Step 3 under `WITH_PROMETHEUS=true` |
| `deploy/kind/metrics-server` | none | Step 3 under `WITH_METRICS_SERVER=true` or `WITH_VPA=true`, in kind mode only |
| `deploy/kind/vpa` | none | Step 3 under `WITH_VPA=true`, in kind mode only |
| `deploy/kind/messaging` | none | after the apply of Step 5 under `WITH_MESSAGING=true`, in both cluster modes |
| `deploy/kind/controlplane` | none | a person, as the file `controlplane.yaml` ([Quick Start (ControlPlane)](../../quick-start-controlplane.md)), or the script under `WITH_CONTROLPLANE_CR=true` |
| `deploy/kind/fake-compute` | none | a person ([Run a Fake Compute for Testing](../../guides/nova/run-a-fake-compute-for-testing.md)) |
| `deploy/kind/hypervisor-operator-fixtures` | none | a person ([Connect a Compute Cluster](../../guides/nova/connect-a-compute-cluster.md)) |
| `deploy/lab/metal-stack/base` | `deploy/kind/base` | Step 3 under `EXTERNAL_CLUSTER=true` |
| `deploy/lab/metal-stack/infrastructure` | `deploy/kind/infrastructure` | Step 5 under `EXTERNAL_CLUSTER=true` |
| `deploy/lab/metal-stack/chaos-mesh` | `deploy/kind/chaos-mesh` | Step 3 under `EXTERNAL_CLUSTER=true` and `WITH_CHAOS_MESH=true` |
| `deploy/lab/metal-stack/dizzy` | `deploy/kind/dizzy` | Step 3 under `EXTERNAL_CLUSTER=true` and `WITH_DIZZY=true` |
| `deploy/lab/metal-stack/nfs` | `deploy/kind/nfs` | Step 3 under `EXTERNAL_CLUSTER=true` and `WITH_NFS=true` |
| `deploy/lab/metal-stack/prometheus` | `deploy/kind/prometheus` | Step 3 under `EXTERNAL_CLUSTER=true` and `WITH_PROMETHEUS=true` |
| `deploy/lab/metal-stack/ceph` | none | Step 3 under `EXTERNAL_CLUSTER=true` and `WITH_CEPH=true` ([Lab Ceph](infrastructure-manifests.md#lab-ceph)) |
| `deploy/lab/metal-stack/controlplane` | none | a person ([Quick Start (metal-stack)](../../quick-start-metal-stack.md)) |
| `deploy/lab/metal-stack/hypervisor-fixtures` | `deploy/kind/hypervisor-operator-fixtures` | a person, after the ControlPlane |
| `deploy/lab/metal-stack/hypervisor` | `deploy/lab/metal-stack/migration-ports` | a person, after the ControlPlane |
| `deploy/lab/metal-stack/migration-ports` | none | a person, and as the base of `hypervisor/` |
| `deploy/lab/metal-stack/probe` | none | a person ([Node probe](infrastructure-manifests.md#node-probe)) |
| `deploy/lab/metal-stack/dizzy-soak` | none | `make dizzy-soak-start` ([Lab dizzy soak](infrastructure-manifests.md#lab-dizzy-soak)) |

The overlays reference the production FluxCD manifests as their base and apply
strategic merge patches to reduce resource requirements for a single-node kind
cluster. With the ControlPlane on the `Minimal` sizing profile, the full stack
requests at most 4 CPU and 16 GiB of memory, which the
[node budget gate](../testing/controlplane-e2e-tests.md#node-budget-link-6z)
of the `e2e-controlplane` job enforces.

### Base Overlay Patches (OpenBao)

| Setting | Production | Kind |
| --- | --- | --- |
| Replicas | 3 (HA) | 1 (standalone) |
| HA enabled | `true` | `false` |
| Raft config | `retry_join` with 3 peers | No `retry_join` (standalone) |
| Storage class | `local-path` | `standard` |
| CPU request | `250m` | `100m` |

### Base Overlay Patches (operators and Flux)

| Setting | Production | Kind |
| --- | --- | --- |
| Operator HelmReleases (`spec.values.replicas`) | `2` (chart default) | `1` |
| Flux controllers (CPU request) | `100m` (upstream manifests) | `25m`, through `spec.kustomize.patches` on the FluxInstance |

The overlay applies the nine service-operator HelmReleases suspended and the
`c5c3-operator` one active, because the `CONTROLPLANE_OPERATORS=flux` path
relies on it. The patches lower requests and change no limit.

### Infrastructure Overlay Patches

**MariaDB CR (`openstack-db`):**

| Setting | Production | Kind |
| --- | --- | --- |
| Replicas | 3 | 1 |
| Galera | enabled | disabled |
| MaxScale | enabled | disabled |
| Storage class | `ceph-rbd` | `standard` |
| Resources | operator defaults (none) | memory request and limit `1Gi`, no CPU |

The memory request takes the single database out of the BestEffort class, which
the kernel OOM killer drains first when parallel e2e suites exhaust the memory
of the kind node; every service workload already requests between 368Mi and
2064Mi of memory, and no service container carries a default CPU limit. The CPU fields
stay unset on purpose: a 500m request left pods Pending on the keystone leg
(#970), and a limit would throttle the liveness probe the overlay relaxes.

**Memcached CR (`openstack-memcached`):**

| Setting | Production | Kind |
| --- | --- | --- |
| Replicas | 3 | 1 |

cert-manager, mariadb-operator and ESO are not patched — they are single-replica or
stateless by default. The memcached-operator release gets one patch,
`webhook.enabled: false`.

### Lab Overlay Patches

```text
deploy/lab/metal-stack/
├── base/
│   └── kustomization.yaml          References ../../../kind/base/
│                                    Patches OpenBao HelmRelease → removes the storage class,
│                                    every Namespace → Gardener apiserver-proxy opt-out label
│                                    and chaos-mesh.org/inject annotation
├── chaos-mesh/                     Chaos Mesh (#1221), applied under WITH_CHAOS_MESH=true
│   ├── kustomization.yaml          References ../../../kind/chaos-mesh/
│   │                                Patches the Namespace → Gardener opt-out label,
│   │                                HelmRelease → enableFilterNamespace: true
│   └── modules-daemonset.yaml      DaemonSet chaos-mesh-modules on every node
├── controlplane/                   The quick start's OVNCentral and ControlPlane CR (#1141), applied by hand
│   ├── kustomization.yaml          Lists the two manifests below
│   ├── ovncentral.yaml             OVNCentral controlplane-ovn, as on the quick-start page
│   └── controlplane-lab.yaml       ControlPlane controlplane with its cinder block, plus global_physnet_mtu and hypervisorOperator
├── dizzy/                          The dizzy metrics stack (#1225), applied under WITH_DIZZY=true
│   └── kustomization.yaml          References ../../../kind/dizzy/
│                                    Patches the Namespace → Gardener opt-out label,
│                                    VictoriaMetrics HelmRelease → 10Gi volume, ClusterIP Service
├── dizzy-soak/                     The dizzy soak (#1274), applied by make dizzy-soak-start
│   ├── kustomization.yaml          Lists the three manifests below
│   ├── identity.yaml               K-ORC Domain, Project, User, Role and RoleAssignment of the user dizzy-soak
│   ├── rbac.yaml                   ServiceAccount, read-only ClusterRole and ClusterRoleBinding
│   ├── reports-pvc.yaml            Claim dizzy-soak-reports, 5Gi, no storage class
│   ├── job.yaml                    Job dizzy-soak, outside the kustomization, created by start
│   └── scenario.yaml               dizzy's mix small profile with the lab's names, outside the kustomization
├── hypervisor/                     The two workers as KVM hypervisors (#1142), applied by hand
│   ├── kustomization.yaml          Lists ../migration-ports and the seven manifests below; the apply order and node labels in its header
│   ├── libvirt-ca.yaml             The libvirt migration CA and Issuer nova-hypervisor-agents-ca-issuer
│   ├── libvirt-configmap.yaml      host-prepare.sh, libvirtd.sh, libvirtd.conf, qemu.conf
│   ├── libvirt-daemonset.yaml      DaemonSet libvirt on the pool's nodes
│   ├── compute.yaml                OVNChassis, NeutronMetadataAgent, NovaCompute
│   ├── sources.yaml                OCIRepository of each chart, digest-pinned
│   ├── hvo-release.yaml            HelmRelease openstack-hypervisor-operator
│   └── kna-release.yaml            HelmRelease kvm-node-agent
├── hypervisor-fixtures/
│   └── kustomization.yaml          References ../../../kind/hypervisor-operator-fixtures/
│                                    Deletes VolumeType, Network and Subnet
├── infrastructure/
│   └── kustomization.yaml          References ../../../kind/infrastructure/
│                                    Patches MariaDB CR, GarageCluster → removes the storage class
├── migration-ports/                The reservation of QEMU's migration ports (#1189), applied by hand
│   ├── kustomization.yaml          Lists the two manifests below
│   ├── namespace.yaml              Namespace hypervisor-system, labelled and annotated like base/
│   └── reservation-daemonset.yaml  DaemonSet migration-port-reservation on every node
├── nfs/                            The NFS server and csi-driver-nfs (#1196), applied under WITH_NFS=true
│   ├── kustomization.yaml          References ../../../kind/nfs/
│   │                                Patches the export claim → 100Gi, no storage class
│   ├── client-modules-daemonset.yaml  DaemonSet nfs-client-modules on every node
│   └── client-policy.yaml          NetworkPolicy template, outside the kustomization:
│                                    only the node network reaches the server on 2049
├── probe/                          Node probe and NFS module load test, applied by hand
│   ├── kustomization.yaml          Lists node-probe.yaml alone
│   ├── node-probe.yaml             Read-only node probe Job (#1139)
│   └── nfs-module-load.yaml        NFS module load test Job (#1194), outside the kustomization
└── prometheus/                     kube-prometheus-stack (#1226), applied under WITH_PROMETHEUS=true
    ├── kustomization.yaml          References ../../../kind/prometheus/
    │                                Deletes the Namespace, patches the HelmRelease →
    │                                four scrape jobs off, 7d on a 10Gi volume, ruleSelector {}, 1Gi/2Gi
    └── hypervisor-operator.json    Grafana dashboard Hypervisor Operator, read by the configMapGenerator
```

`EXTERNAL_CLUSTER=true` applies `base/` in Step 3 and `infrastructure/` in
Step 5 in place of the kind overlays, under `WITH_NFS=true` it applies
`nfs/` in Step 3 in place of `deploy/kind/nfs`, under
`WITH_CHAOS_MESH=true` `chaos-mesh/` in place of `deploy/kind/chaos-mesh`,
under `WITH_DIZZY=true` `dizzy/` in place of `deploy/kind/dizzy`, and under
`WITH_PROMETHEUS=true` `prometheus/` in place of `deploy/kind/prometheus`.
Each takes its kind overlay as its base, so the lab inherits every patch
above. It removes the storage class, labels the Namespaces with Gardener's
apiserver-proxy opt-out and annotates them for Chaos Mesh, turns on Chaos
Mesh's namespace filter, loads the NFS and NetworkChaos kernel modules in
pods, keeps the dizzy metrics on a volume behind a ClusterIP Service, leaves
the Namespace `monitoring` to the base, keeps the Prometheus metrics on a
volume, turns off four scrape jobs and adds the hvo dashboard. It changes
nothing else. The script never applies
`controlplane/`; the completion hint of
`WITH_CONTROLPLANE=true` names it. Nor does it apply `hypervisor/` or
`hypervisor-fixtures/`, which follow the ControlPlane by hand, or
`dizzy-soak/`, which `make dizzy-soak-start` applies. Every volume of
the lab, the proving `OpenBaoCluster`'s, the NFS export claim, the dizzy
VictoriaMetrics claim, the soak's report claim and the Prometheus claim
included, binds to the cluster's default class, which Step 1 checks exists.

| Setting | Kind | Lab |
| --- | --- | --- |
| OpenBao storage class (`dataStorage`) | `standard` | the cluster's default class |
| MariaDB storage class | `standard` | the cluster's default class |
| Garage storage class (metadata and data) | `standard` | the cluster's default class |
| Namespace label `apiserver-proxy.networking.gardener.cloud/inject` | absent | `disable` |
| Namespace annotation `chaos-mesh.org/inject` | absent | `enabled` |
| Chaos Mesh namespace filter (`controllerManager.enableFilterNamespace`) | off | on |
| NetworkChaos kernel modules | `modprobe` on the host by the deploy script | the DaemonSet `chaos-mesh-modules` |
| NFS export claim (`nfs-server-exports`) | `standard`, 5Gi | the cluster's default class, 100Gi |
| NFS client modules (`nfs`, `nfsv4`) | `modprobe` on the host by the deploy script | the DaemonSet `nfs-client-modules` |
| Clients admitted to the NFS server's port 2049 | every pod and node | the node network, by the NetworkPolicy `nfs-server-clients` |
| Namespace `dizzy` | no label | the label `apiserver-proxy.networking.gardener.cloud/inject: disable`, no `chaos-mesh.org/inject` annotation |
| VictoriaMetrics Service (`dizzy-victoria-metrics-server`) | NodePort 30428, mapped to host port 8428 | ClusterIP (headless), reached through `kubectl port-forward` on local port 8428 |
| VictoriaMetrics storage | emptyDir | the claim `server-volume-dizzy-victoria-metrics-server-0`, 10Gi on the cluster's default class |
| Namespace `monitoring` | declared by the Prometheus overlay, without labels | not in the render of `prometheus/`; the base overlay declares, labels and annotates it |
| Prometheus scrape jobs | the chart's defaults | `kubeEtcd`, `kubeScheduler`, `kubeControllerManager` and `kubeProxy` off: Gardener runs the first three in the seed, and the chart's kube-proxy Service selects no pod of the shoot |
| Prometheus retention | `6h` | `7d`, bounded by `retentionSize: 8GB` |
| Prometheus storage | emptyDir | a 10Gi volume claim template on the cluster's default class |
| Prometheus rule selector | `release: kube-prometheus-stack` | every PrometheusRule (`ruleSelectorNilUsesHelmValues: false`) |
| Prometheus memory request and limit | `256Mi`, `512Mi` | `1Gi`, `2Gi` |
| Grafana dashboards | Keystone Operator | Keystone Operator and Hypervisor Operator |
| Everything else | as above | inherited from the kind overlay |

The overlay is described in
[Infrastructure Manifests](infrastructure-manifests.md#lab-overlay).

## Environment Variables

The deployment script supports configurable timeouts via environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `CLUSTER_NAME` | `cobaltcore` | Kind cluster name |
| `FLUX_OPERATOR_VERSION` | _pinned in script_ | Tag of the flux-operator `install.yaml` release applied in Step 2; kept in sync by Renovate via a `customManager` on `hack/deploy-infra.sh` |
| `HELMRELEASE_TIMEOUT` | `600` | Seconds to wait for HelmReleases Ready (also bounds the `wait_for_fluxinstance` poll in Step 2) |
| `POD_TIMEOUT` | `300` | Seconds to wait for OpenBao pods Ready |
| `EXTERNALSECRET_TIMEOUT` | `120` | Seconds to wait for ExternalSecrets synced |
| `WEBHOOK_TIMEOUT` | `120` | Seconds to wait, after cert-manager is Ready, for its webhook to admit a server-side dry-run of the ClusterIssuer before the Phase-2 TLS prerequisites are applied. Also bounds the second probe, under `WITH_CONTROLPLANE=true`: the wait for the API server to admit or deny a server-side dry-run of the ControlPlane manifests. Under `WITH_CONTROLPLANE_CR=true` it probes the bundled CR before its apply; otherwise, on the `flux` path, it probes the render of the overlay's `controlplane/` directory, or the kind `controlplane/controlplane.yaml` together with the lab's OVNCentral, before the by-hand hint. A timeout stops the run and prints the `c5c3-operator` and `ovn-operator` HelmReleases, pods and pod logs |
| `SKIP_KIND_CREATE` | `false` | Skip kind cluster creation (CI mode where cluster is pre-created) |
| `KIND_CONFIG` | `hack/kind-config.yaml` | The kind config `render_kind_config` starts from. Set it to `hack/kind-config-multinode.yaml` (1 control-plane node + 2 workers) for suites that need more than one schedulable node. Both configs bind the same host ports, so two clusters created from them cannot coexist on one host. A custom config must keep its control-plane node at `nodes[0]`, which is the only node the `KIND_HOST_PORT` override rewrites. Read only on the run that creates the cluster: with `SKIP_KIND_CREATE=true` or an existing cluster of that name the value is ignored and the script warns |
| `OPENBAO_NAMESPACE` | `shared-services` | OpenBao namespace (propagated to the bootstrap scripts, which resolve the same variable in `common.sh`). The generic `NAMESPACE` variable is deliberately ignored — chainsaw injects `NAMESPACE=<test namespace>` into e2e script steps |
| `INSTALL_DIR` | `~/.local/bin` | Directory for `install-test-deps.sh` to install tools |
| `WITH_CONTROLPLANE` | `false` | When `true`, the c5c3 `ControlPlane` provisions MariaDB/Memcached in managed mode: deploy-infra skips the shared MariaDB/Memcached CRs and seeds the per-CR OpenBao admin-password paths instead |
| `CONTROLPLANE_OPERATORS` | `flux` | How the ControlPlane operator stack is provided (only when `WITH_CONTROLPLANE=true`). `flux` deploys the published c5c3-operator chart + K-ORC Flux source, un-suspends the keystone-, horizon-, glance-, placement-, barbican-, ovn-, neutron-, cinder- and nova-operator releases, and pins the self-built operators' `:latest` images to their current digests via `hack/refresh-operator-image-digests.sh` (per-operator image-digest ConfigMaps consumed via `valuesFrom`; re-run with `make refresh-operator-digests` after a merge). On this path the run fails when the `k-orc` Kustomization is not Ready within `HELMRELEASE_TIMEOUT`, because the c5c3-operator cannot start without the K-ORC CRDs; the error prints the state of the `k-orc` GitRepository and Kustomization. The run then waits for the ten operator HelmReleases (the nine service operators and `c5c3-operator`) to be Ready within `HELMRELEASE_TIMEOUT` and for one primary CRD per operator to be registered within `POD_TIMEOUT`, and fails when either is not; after that it waits up to `WEBHOOK_TIMEOUT` for the API server to admit a server-side dry-run of the ControlPlane manifests. The two waits, and the probe before the by-hand hint, are skipped under `INFRA_ONLY=true`, and preflight refuses `INFRA_ONLY=true` together with `WITH_CONTROLPLANE_CR=true`, because such a cluster runs no operator to admit the bundled CR. `external` suspends the Flux stack and expects the operators to be deployed out of band (as the `e2e-controlplane` CI job does with local dev images + `hack/ci-deploy-korc.sh`) |
| `CONTROLPLANE_NAME` | `controlplane` | Name of the ControlPlane CR under `WITH_CONTROLPLANE=true`; the per-CR OpenBao admin-password bootstrap path derives from it, so it must match the applied CR (the `e2e-controlplane` job sets `controlplane-keystone`) |
| `WITH_OVN_KERNEL_MODULES` | `false` | When `true`, `modprobe` `openvswitch` and `geneve` on the host before the cluster is created, so the OVN chassis suites find the datapath and tunnel modules in the kernel the kind nodes share. Linux only, and it needs root or passwordless sudo: without either the script logs a warning and continues |
| `WITH_CHAOS_MESH` | `false` | When `true`, deploy Chaos Mesh in Step 3 and append `chaos-mesh` to the Phase 3 HelmRelease wait. In kind mode it first runs `modprobe` for `ip_set`, `ip_set_hash_ip`, `ip_set_hash_net`, `xt_set`, `sch_netem` and `sch_tbf` on the host before the cluster is created and applies `deploy/kind/chaos-mesh`. The module load is Linux only and needs root or passwordless sudo: without either the script logs a warning and continues. Under `EXTERNAL_CLUSTER=true` it loads nothing on the host and applies `<overlay>/chaos-mesh` instead, whose DaemonSet `chaos-mesh-modules` loads the modules on the nodes, and waits up to `POD_TIMEOUT` seconds for that DaemonSet's rollout; a failed wait stops the run with `ERROR: DaemonSet chaos-mesh/chaos-mesh-modules did not roll out, ...` and the command that reads the loader's log. The overlays are described in [Infrastructure Manifests](infrastructure-manifests.md#chaos-mesh-kind-only-opt-in) and [Lab Chaos Mesh](infrastructure-manifests.md#lab-chaos-mesh) |
| `WITH_NFS` | `false` | When `true`, deploy the NFS server in `openstack` and the `csi-driver-nfs` mounter in `kube-system` in Step 3, wait for the `nfs-server` rollout, and append `csi-driver-nfs` to the Phase 3 HelmRelease wait. In kind mode it first runs `modprobe` for `nfs` and `nfsv4`, the modules the `csi-driver-nfs` node plugin mounts with, on the host before the cluster is created and applies the `deploy/kind/nfs` overlay; the server, NFS-Ganesha, needs no module. The module load is Linux only and needs root or passwordless sudo: without either the script logs a warning and continues. A rollout failure of the server stops the run with `ERROR: the NFS server did not roll out.` and the commands that read the logs of `prepare-exports` and `nfs-server`. Under `EXTERNAL_CLUSTER=true` it loads nothing on the host and applies `<overlay>/nfs` instead, whose DaemonSet `nfs-client-modules` loads the modules on the nodes; it also waits for that DaemonSet's rollout, and a failed wait stops the run with an error naming the log to read. When the overlay ships `nfs/client-policy.yaml`, it applies that NetworkPolicy before `<overlay>/nfs`, with the node network of Step 1 in place of the placeholder `NODE_NETWORK`. The overlays are described in [Infrastructure Manifests](infrastructure-manifests.md#nfs-storage-stack-opt-in) and [Lab NFS stack](infrastructure-manifests.md#lab-nfs-stack) |
| `WITH_DIZZY` | `false` | When `true`, stage dizzy's three Grafana dashboards into `deploy/kind/dizzy/dashboards/` with `hack/dizzy.sh stage-dashboards`, deploy the dizzy metrics stack (VictoriaMetrics and Grafana) in Step 3, append `dizzy-victoria-metrics` and `dizzy-grafana` to the Phase 3 HelmRelease wait, and log the Grafana URL `https://dizzy.127-0-0-1.nip.io` on the public port. A dashboard download that fails stops the run before the stack is applied. In kind mode it applies `deploy/kind/dizzy`, whose VictoriaMetrics keeps its metrics in an emptyDir and takes dizzy's OTLP export on NodePort 30428, which `hack/kind-config.yaml` maps to host port 8428; the script warns when the cluster predates that mapping. Under `EXTERNAL_CLUSTER=true` it applies `<overlay>/dizzy` instead, whose VictoriaMetrics keeps its metrics on a 10Gi claim on the cluster's default class behind a ClusterIP Service, calls no `docker port`, and the completion banner names the `kubectl port-forward` to VictoriaMetrics on local port 8428 through which `EXTERNAL_CLUSTER=true make dizzy-keystone` exports, beside Grafana on the Gateway port-forward. The overlays are described in [Infrastructure Manifests](infrastructure-manifests.md#dizzy-load-chaos-stack-kind-only-opt-in) and [Lab dizzy stack](infrastructure-manifests.md#lab-dizzy-stack) |
| `WITH_PROMETHEUS` | `false` | When `true`, stage the Keystone Operator dashboard into `deploy/kind/prometheus/keystone-operator.json`, deploy kube-prometheus-stack (the Prometheus Operator, Prometheus and Grafana) in `monitoring` in Step 3, append `kube-prometheus-stack` to the Phase 3 HelmRelease wait and raise that wait to at least 1200 seconds, and then set `monitoring.serviceMonitor.enabled=true` on the nine service-operator HelmReleases, waiting for each one that is not suspended. In kind mode it applies `deploy/kind/prometheus`, whose Prometheus keeps 6 hours of metrics in an emptyDir. Under `EXTERNAL_CLUSTER=true` it applies `<overlay>/prometheus` instead, and preflight refuses the flag for an overlay without `prometheus/kustomization.yaml`. The lab's overlay leaves the Namespace `monitoring` to the base, turns off the scrape jobs of etcd, the scheduler, the controller manager and kube-proxy, keeps 7 days on a 10Gi claim on the cluster's default class, loads every PrometheusRule, and adds the dashboard of openstack-hypervisor-operator. `make teardown-infra` deletes the release, the PVCs in `monitoring` and the Service and Endpoints `kube-system/kube-prometheus-stack-kubelet` in its step 3. The overlays are described in [Infrastructure Manifests](infrastructure-manifests.md#kube-prometheus-stack-kind-only-opt-in) and [Lab Prometheus stack](infrastructure-manifests.md#lab-prometheus-stack) |
| `WITH_MESSAGING` | `false` | When `true`, apply the `deploy/kind/messaging` overlay after Step 5 and wait for `rabbitmqcluster/shared-rabbitmq` in `openstack` to report `AllReplicasReady`. A timeout stops the run and prints the `kubectl describe` output for the broker plus the events of its server pod. The overlay is described in [Infrastructure Manifests](infrastructure-manifests.md#message-bus-kind-only-opt-in). The `e2e-controlplane` job is the broker's second consumer after the cinder `e2e-operator` leg: the full-ControlPlane suite takes a vhost of its own through `tests/e2e/cinder/broker-vhost.sh` and hands the ControlPlane the transport URL of that vhost brownfield |
| `WITH_REGISTRY_CACHE` | `false` | Local-dev only. When `true`, bring up one distribution-registry (`registry:2`) pull-through proxy per upstream registry (`docker.io`, `ghcr.io`, `registry.k8s.io`, `quay.io`, plus the vanity fronts `oci.external-secrets.io` and `docker-registry3.mariadb.com`) on the `kind` Docker network and wire every node's containerd at them via a `certs.d/<host>/hosts.toml` mirror, so unmodified image refs are served from a persistent local cache that survives `kind delete`. The proxy streams and caches inline (fast even on a cold pull). The containerd mirror patch is injected only into the deploy-time kind config, never the checked-in `hack/kind-config.yaml`, so CI is unaffected. Requires `yq`. See the [Extended Quick Start](../../quick-start-extended.md) |
| `PURGE_REGISTRY_CACHE` | `false` | Consumed by `make teardown-infra`. When `true`, also remove the registry pull-through cache containers and their volumes (identified by the `cobaltcore.registry-cache=true` label). The default leaves them running so the warm cache is reused on the next deploy |
| `EXTERNAL_CLUSTER` | `false` | When `true`, deploy onto the cluster the current kubeconfig context points at (the script never switches contexts) with the `EXTERNAL_OVERLAY` overlays in Steps 3 and 5. Docker and kind are not required. Preflight refuses the four kind-bound opt-ins `WITH_VPA`, `WITH_METRICS_SERVER`, `WITH_REGISTRY_CACHE` and `WITH_OVN_KERNEL_MODULES`; the messages for `WITH_VPA` and `WITH_METRICS_SERVER` name the opt-in fields under `spec.sizing` of the ControlPlane, since the platform already runs a VPA and serves the metrics API. Preflight accepts `WITH_NFS=true` only when `<overlay>/nfs/kustomization.yaml` exists, `WITH_CHAOS_MESH=true` only when `<overlay>/chaos-mesh/kustomization.yaml` exists, `WITH_DIZZY=true` only when `<overlay>/dizzy/kustomization.yaml` exists and `WITH_PROMETHEUS=true` only when `<overlay>/prometheus/kustomization.yaml` exists, requires the context's API server to answer, and logs the context and the server URL. Step 1 creates no cluster; it checks for a default StorageClass, for the absence of a `node-local-dns` DaemonSet in `kube-system` and for a Ready node, and logs the class and the node names. Under `WITH_NFS=true` it also refuses a `CSIDriver/nfs.csi.k8s.io` whose `helm.toolkit.fluxcd.io` labels do not name the HelmRelease `kube-system/csi-driver-nfs`: the cluster then runs its own NFS CSI driver, which Step 3 would replace and the HelmRelease would adopt. For an overlay with `nfs/client-policy.yaml` it then reads the node network from `data.nodeNetwork` of the ConfigMap `kube-system/shoot-info`, logs it, and refuses a cluster where that value is missing or no IPv4 CIDR, or where a node has no IPv4 `InternalIP` inside it. Without `WITH_NFS=true`, when the ControlPlane of the overlay's `controlplane/` (see `EXTERNAL_OVERLAY`) has a Cinder backend on NFS, it refuses a cluster whose `CSIDriver/nfs.csi.k8s.io` is missing or does not list the `Ephemeral` lifecycle mode, and logs the driver's modes: the Cinder pods mount every export as an inline volume of that driver, and only `WITH_NFS=true` installs it. The nofile cap and the Keystone image preload are skipped. The Gateway is reached through `kubectl port-forward` on local port 8443, which the completion banner prints, and the bundled ControlPlane CR's `publicEndpoint` gets `:8443`. Also consumed by `make teardown-infra`. Any other value keeps the kind mode |
| `EXTERNAL_OVERLAY` | `deploy/lab/metal-stack` | Overlay root of `EXTERNAL_CLUSTER=true`: its `base/` and `infrastructure/` replace `deploy/kind/base` and `deploy/kind/infrastructure`. Relative to the repository root unless absolute. Preflight fails when either kustomization is missing. An overlay may also carry a `controlplane/` kustomization, which `make deploy-infra` names in its `WITH_CONTROLPLANE=true` completion hint and never applies; while it exists and `WITH_CONTROLPLANE_CR` is not `true`, preflight renders it and refuses a failing render, a render with no ControlPlane or more than one, a ControlPlane other than `openstack/<CONTROLPLANE_NAME>`, the only one Step 7 seeds, and, unless `WITH_NFS=true`, a ControlPlane with a Cinder backend on the in-cluster NFS server `nfs-server.openstack`, which only `WITH_NFS=true` deploys. The server counts under `nfs-server.openstack`, `nfs-server.openstack.svc` and `nfs-server.openstack.svc.cluster.local`, in any case and the last with a trailing dot too. A backend on any other NFS server passes preflight, and Step 1 checks the cluster for the CSI driver it mounts through (see `EXTERNAL_CLUSTER`). A `hypervisor/` kustomization, with `hypervisor-fixtures/` beside it, is applied by hand as well; `make teardown-infra` removes both in its step 0. An optional `nfs/` kustomization is what Step 3 applies under `WITH_NFS=true` in place of `deploy/kind/nfs`; it has to render the Deployment `nfs-server` and the DaemonSet `nfs-client-modules` in `openstack` and the HelmRelease `csi-driver-nfs` in `kube-system`, the three objects the script waits for, and `make teardown-infra` removes it at the end of its step 2. `nfs/` may also ship `client-policy.yaml`, a NetworkPolicy template outside the kustomization whose placeholder `NODE_NETWORK` the script replaces with the node network of a Gardener shoot; an overlay for a cluster without the ConfigMap `kube-system/shoot-info` ships none. An optional `chaos-mesh/` kustomization is what Step 3 applies under `WITH_CHAOS_MESH=true` in place of `deploy/kind/chaos-mesh`; it has to render the DaemonSet `chaos-mesh-modules` and the HelmRelease `chaos-mesh` in `chaos-mesh`, the two objects the script waits for, and `make teardown-infra` removes it before its step 0. An optional `dizzy/` kustomization is what Step 3 applies under `WITH_DIZZY=true` in place of `deploy/kind/dizzy`; it has to render the HelmReleases `dizzy-victoria-metrics` and `dizzy-grafana` in `dizzy`, the two objects the script waits for and `make teardown-infra` deletes in its step 3, and it takes its dashboards from `deploy/kind/dizzy/dashboards/`, which Step 3 stages. An optional `prometheus/` kustomization is what Step 3 applies under `WITH_PROMETHEUS=true` in place of `deploy/kind/prometheus`; it has to render the HelmRelease `kube-prometheus-stack` in `monitoring`, the object the script waits for and `make teardown-infra` deletes in its step 3, and it takes the Keystone dashboard from `deploy/kind/prometheus/keystone-operator.json`, which Step 3 stages. Read by `make deploy-infra` and `make teardown-infra` |
| `TEARDOWN_TIMEOUT` | `600` | Consumed by `make teardown-infra` under `EXTERNAL_CLUSTER=true`: seconds each delete waits for its objects to be gone before the teardown exits 1 |

**Example: override HelmRelease timeout:**

```bash
HELMRELEASE_TIMEOUT=600 make deploy-infra
```

## CI Job

The `e2e-infra` job in `.github/workflows/ci.yaml` runs only on pull requests
(`github.event_name == 'pull_request'`) and only when the `e2e-infra` path filter
of the `changes` job matches. It depends only on `changes` — not on the `lint` or
`test` jobs — so it starts as soon as the path filters are resolved.

**Job steps:**

1. Checkout repository (SHA-pinned `actions/checkout`)
2. Setup Go (SHA-pinned `actions/setup-go` with `go-version-file: go.work`)
3. Create kind cluster (`create-kind-cluster` composite action with `hack/kind-config.yaml`)
4. Install Flux CLI (SHA-pinned `fluxcd/flux2/action`)
5. Install test dependencies (`make install-test-deps`, adds `~/.local/bin` to `PATH`)
6. Deploy infrastructure stack (`make deploy-infra` with `SKIP_KIND_CREATE=true`)
7. Run Chainsaw E2E tests against `tests/e2e/infrastructure/`
8. Re-run `make deploy-infra` with unchanged parameters (no `SKIP_KIND_CREATE`, exercises the script's existing-cluster detection)
9. Re-run the full infrastructure suite (report `chainsaw-report-rerun`) to prove the healthy stack is left unchanged
10. Re-run `make deploy-infra` with `WITH_METRICS_SERVER=true` and `WITH_NFS=true` (additive leg — the script's Phase 3 wait gates the new metrics-server and `csi-driver-nfs` HelmReleases on Ready, and its Step 3 rollout wait gates `Deployment/nfs-server`). Both opt-ins ride one convergence run: `make deploy-infra` re-applies every overlay and re-walks all eight steps, so a second additive leg would repeat the base install for one `kubectl apply -k deploy/kind/nfs`
11. Assert the additive `WITH_NFS` opt-in landed (`kubectl get deployment nfs-server -n openstack` and `kubectl get helmrelease csi-driver-nfs -n kube-system`). nfs-health skips when the server is absent, so this is what turns a dropped `WITH_NFS: "true"` in step 10 into a red leg instead of a green one over an untested stack
12. Run a scoped Chainsaw suite (report `chainsaw-report-additive`) over infra-stack-health, garage-health, flux-web-health, no-prometheus-when-disabled, openbao-instance, and nfs-health, skipping the metrics-server and NFS absence suites it would now rightly fail
13. Dump diagnostic info on failure (`kubectl get`, `flux logs` for troubleshooting)
14. Upload JUnit report as workflow artifact (SHA-pinned `actions/upload-artifact`, `if: always()`)
15. Delete the kind cluster (`hack/ci-delete-kind-cluster.sh`, `if: always()`) — last, so step 13 still had a cluster to read, and a cluster that survives is a warning rather than a failed job

**Configuration:**

| Setting | Value |
| --- | --- |
| `timeout-minutes` | 50 |
| `permissions` | `contents: read` (inherited from workflow-level) |
| `concurrency` | Cancel-in-progress on PRs (inherited from workflow-level) |
| Action pinning | All `uses:` references are SHA-pinned with version comments |

## Chainsaw E2E Test

**File:** `tests/e2e/infrastructure/infra-stack-health/chainsaw-test.yaml`

The test asserts readiness of all deployed components:

| # | Assertion | Namespace | Resource |
| --- | --- | --- | --- |
| 1 | cert-manager Deployment ready | `cert-manager` | `Deployment` |
| 2 | OpenBao StatefulSet ready | `shared-services` | `StatefulSet` |
| 3 | ESO Deployment ready | `external-secrets` | `Deployment` |
| 4 | MariaDB Operator Deployment ready | `mariadb-system` | `Deployment` |
| 5 | Memcached Operator Deployment ready | `memcached-system` | `Deployment` |
| 6 | ClusterIssuer Ready condition | (cluster-scoped) | `ClusterIssuer` |
| 7 | MariaDB CR Ready condition | `openstack` | `MariaDB` |
| 8 | Memcached CR Ready condition | `openstack` | `Memcached` |
| 9 | ClusterSecretStore Valid condition | (cluster-scoped) | `ClusterSecretStore` |
| 10 | ExternalSecrets SecretSynced | `openstack` | `ExternalSecret` (x3) |

Assert timeout is ~5 minutes to account for operator startup time.

The `e2e-infra` job auto-discovers every `chainsaw-test.yaml` under
`tests/e2e/infrastructure/`, so sibling suites run in the same job with no CI wiring.

**File:** `tests/e2e/infrastructure/garage-health/chainsaw-test.yaml`

Covers the Garage object store (the S3 backend for the Glance e2e suites):

| # | Assertion | Namespace | Resource |
| --- | --- | --- | --- |
| 1 | garage-operator Deployment ready + HelmRelease Ready | `garage-system` | `Deployment`, `HelmRelease` |
| 2 | Credential ExternalSecrets SecretSynced: `garage-admin-token` and `garage-s3-credentials` beside the CRs, plus the retained `garage-s3-credentials` copy the Glance consumers read | `shared-services`, `openstack` | `ExternalSecret` (x3) |
| 3 | GarageCluster `Running`; GarageBucket / GarageKey `Ready` | `shared-services` | `GarageCluster`, `GarageBucket`, `GarageKey` |
| 4 | S3 put + list with the imported key over path-style HTTP; the probe pod stays in `openstack` and reaches Garage through `garage.shared-services.svc.cluster.local` | `openstack` | `script` (throwaway `aws-cli` pod) |

**File:** `tests/e2e/infrastructure/nfs-health/chainsaw-test.yaml`

Covers the NFS stack behind `WITH_NFS`, from the kind overlay or from the
metal-stack lab's `nfs/` (see
[Lab NFS stack](infrastructure-manifests.md#lab-nfs-stack)); against the lab it
runs by hand. It skips with a `SKIP:` line when `Deployment/nfs-server` is
absent, so the default legs pass, and it runs
as one `script` step because chainsaw has no step-level skip:

| # | Assertion | Namespace | Resource |
| --- | --- | --- | --- |
| 1 | Presence gate on `Deployment/nfs-server` (`--ignore-not-found`, so only a genuine absence skips and a failed lookup fails), then leftover `Namespace/nfs-health-probe` and the two `nfs-health-*` PersistentVolumes deleted up front | `openstack`, (cluster-scoped) | `Deployment`, `Namespace`, `PersistentVolume` |
| 2 | `nfs-server` rolled out, its container log holds `Root fs for export /exports is /exports` (otherwise the suite prints the log's `FSAL` lines), and its EndpointSlices carry at least one address | `openstack` | `Deployment`, `EndpointSlice` |
| 3 | `HelmRelease/csi-driver-nfs` Ready, `DaemonSet/csi-nfs-node` with `numberReady >= 1`, `CSIDriver/nfs.csi.k8s.io` present | `kube-system`, (cluster-scoped) | `HelmRelease`, `DaemonSet`, `CSIDriver` |
| 4 | Two static PV/PVC pairs (`nfs.csi.k8s.io`, `nfsvers=4.1,soft,timeo=30,retrans=2`) bound at the os-brick mount points `/var/lib/cinder/mnt/<md5(share)>` and `/var/lib/cinder/backup_mount/<md5(share)>`, the md5 taken over the share string with no trailing newline | `nfs-health-probe` | `PersistentVolume`, `PersistentVolumeClaim` |
| 5 | A `restricted`-admitted probe pod (UID 42424, no `fsGroup`) finds both mounts in `/proc/mounts`, writes and reads back `nfs-health.probe` under each, and `stat` reports `42424 42424 660` twice; the pod phase must be `Succeeded` and the log capture non-empty | `nfs-health-probe` | `Pod` (throwaway, the pinned server image) |
| 6 | `finally` deletes the Pod, then the Namespace, then both PersistentVolumes (`--ignore-not-found --wait=false`), on the passing path too | `nfs-health-probe`, (cluster-scoped) | `Pod`, `Namespace`, `PersistentVolume` |

**File:** `tests/e2e/infrastructure/no-nfs-when-disabled/chainsaw-test.yaml`

Pins the default posture: a cluster deployed without `WITH_NFS` carries none of
the four resources. It mirrors `no-metrics-server-when-disabled`, and the
additive `WITH_NFS=true` CI leg excludes it because this suite would then
rightly fail. Every lookup runs through one helper that separates a genuine
absence (`--ignore-not-found`, or a missing CRD on a bare kind cluster) from a
failed lookup, so a transient API error stops the suite instead of passing as
absence:

| # | Assertion | Namespace | Resource |
| --- | --- | --- | --- |
| 1 | No `HelmRepository/csi-driver-nfs` | `flux-system` | `HelmRepository` |
| 2 | No `HelmRelease` named `csi-driver-nfs` in any namespace, from a `--field-selector` list | (all) | `HelmRelease` |
| 3 | No `Deployment/nfs-server` | `openstack` | `Deployment` |
| 4 | No `CSIDriver/nfs.csi.k8s.io` | (cluster-scoped) | `CSIDriver` |

**File:** `tests/e2e/infrastructure/openbao-instance/chainsaw-test.yaml`

Covers the openbao-operator and the proving `OpenBaoCluster` instance:

| # | Assertion | Namespace | Resource |
| --- | --- | --- | --- |
| 1 | openbao-operator Deployment ready + HelmRelease Ready | `openbao-operator-system` | `Deployment`, `HelmRelease` |
| 2 | Unseal-key ExternalSecret SecretSynced, and the instance's `ownerReference` adoption of the Secret it materialized | `openstack` | `ExternalSecret`, `Secret`, `OpenBaoCluster` |
| 3 | OpenBaoCluster `Available` with `readyReplicas: 1`, `APIServerNetworkReady`, and a non-empty `spec.network.apiServerEndpointIPs` | `openstack` | `OpenBaoCluster` |
| 4 | Kubernetes auth to AppRole to KV v2 round-trip, including a rejected login with a wrong secret ID | `openstack` | `script` (`kubectl exec` into the instance pod) |
| 5 | Pod deletion, then the replacement pod reports `sealed: false` without any unseal command | `openstack` | `script` (`kubectl exec` into the instance pod) |

## Pinned Tool Versions

`hack/install-test-deps.sh` installs these pinned versions with SHA256 checksum
verification.  For flux, kind, and kubectl, SHA256 hashes are pinned as constants
in the script (per-platform).  For chainsaw, checksums are fetched from upstream
until pinned hashes are available.  To update hashes after a version bump, download
the new release artifacts, compute `sha256sum`, and replace the values in the script.

| Tool | Version | SHA256 Pinning |
| --- | --- | --- |
| chainsaw | v0.2.15 | upstream (fetched) |
| flux | 2.9.5 | pinned |
| kind | v0.33.0 | pinned |
| kubectl | v1.37.0 | pinned |

## Quick Start

```bash
# Install prerequisites (installs to ~/.local/bin — ensure it is in PATH)
make install-test-deps
export PATH="${HOME}/.local/bin:${PATH}"

# Deploy infrastructure stack
make deploy-infra

# Run E2E tests
make e2e

# Clean up
make teardown-infra
```

## Related Resources

- [OpenBao Bootstrap Procedure](openbao-bootstrap.md) — OpenBao deployment and bootstrap
- `deploy/flux-system/` — Production FluxCD base manifests
- `deploy/kind/` — Kind-specific kustomize overlays
- `tests/e2e/infrastructure/` — Chainsaw E2E test files
- `.github/workflows/ci.yaml` — CI workflow with `e2e-infra` job
