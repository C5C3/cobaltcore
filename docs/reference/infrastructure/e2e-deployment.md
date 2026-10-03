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

```text
┌─────────────────────────────────────────────────────────────────────────┐
│  Developer / CI Runner                                                  │
│                                                                         │
│  make install-test-deps   ──▶  Installs chainsaw, flux, kind, kubectl   │
│  make deploy-infra        ──▶  8-step deployment into kind cluster      │
│  make e2e                 ──▶  Chainsaw E2E tests against the cluster   │
│  make teardown-infra      ──▶  Deletes the kind cluster                 │
│                                                                         │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Kind Cluster (cobaltcore)                                               │
│                                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                   │
│  │ cert-manager │  │   OpenBao    │  │     ESO      │                   │
│  │  (Deployment)│  │ (StatefulSet)│  │ (Deployment) │                   │
│  └──────────────┘  └──────────────┘  └──────────────┘                   │
│  ┌──────────────┐  ┌──────────────┐                                     │
│  │   MariaDB    │  │  Memcached   │                                     │
│  │  Operator    │  │  Operator    │                                     │
│  │ (Deployment) │  │ (Deployment) │                                     │
│  └──────┬───────┘  └──────┬───────┘                                     │
│         │                 │                                             │
│  ┌──────▼───────┐  ┌──────▼───────┐  ┌──────────────────────┐           │
│  │  MariaDB CR  │  │ Memcached CR │  │ ClusterIssuer        │           │
│  │ (openstack-  │  │ (openstack-  │  │ (selfsigned-cluster- │           │
│  │  db)         │  │  memcached)  │  │  issuer)             │           │
│  └──────────────┘  └──────────────┘  └──────────────────────┘           │
│                                                                         │
│  ┌───────────────────────────────────────────────────────────┐          │
│  │ ExternalSecrets: keystone-admin, keystone-db,             │          │
│  │                  mariadb-root-password                    │          │
│  └───────────────────────────────────────────────────────────┘          │
└─────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

| Prerequisite | Details |
| --- | --- |
| Docker | Running Docker daemon (kind uses Docker containers as nodes) |
| kubectl | Kubernetes CLI for cluster interaction |
| kind | Kubernetes IN Docker for local cluster creation |
| flux | **Optional** — the Flux CLI is no longer required by `make deploy-infra`; bootstrap uses flux-operator + FluxInstance. Opt in with `WITH_FLUX_CLI=true make install-test-deps` for ad-hoc `flux logs` debugging. |
| chainsaw | Kyverno Chainsaw for E2E test execution |
| jq | JSON processor used by deployment scripts |

All CLI tools except Docker can be installed via `make install-test-deps`.

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
finalizer run while its controller still exists:

0. The lab hypervisors, when the overlay has a `hypervisor/` kustomization
   (see [Lab hypervisors](infrastructure-manifests.md#lab-hypervisors)), while
   the ControlPlane and every operator still run. Each kind is deleted only
   where its CRD exists:
   1. every `NovaCompute`, `NeutronMetadataAgent` and `OVNChassis` in
      `openstack`. A `NovaCompute` keeps its finalizer
      `nova.openstack.c5c3.io/compute-drain` until Nova counts no server on its
      nodes, so a pool that still holds servers outlives the wait: the teardown
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
      `cobaltcore.cloud.sap/node-hypervisor-lifecycle`, and the annotation
      `nova.openstack.cloud.sap/custom-traits`, from every node.

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
   opt-in `kube-prometheus-stack` release goes last.
4. The PVCs in `shared-services` and `openstack`. The openbao-operator chart's
   admission policy denies deleting its managed PVCs until step 3 has
   uninstalled the chart.
5. The FluxInstance, so the flux-operator uninstalls the toolkit.
6. The `flux-system` namespace and the flux-operator's ClusterRoles and
   ClusterRoleBinding.
7. The namespaces of `deploy/flux-system/namespaces.yaml`, `envoy-gateway-system`
   and `headlamp-system`. Then every ClusterRole, ClusterRoleBinding,
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
   Prometheus Operator, CobaltCore, K-ORC, Flux and flux-operator groups, and
   `kvm.cloud.sap`, whose CRDs the lab hypervisors' charts install and Helm
   leaves behind).

In `kube-system` it deletes only the `maint-<node>` objects of step 0, the
`csi-driver-nfs` HelmRelease of step 2 with its chart, and the two
cert-manager Leases of step 7. It never names `firewall`, `metallb-system`
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

`hack/deploy-infra.sh` implements the following 8-step sequence:

```text
Step 1 ── Create kind cluster (hack/kind-config.yaml)
     │         (EXTERNAL_CLUSTER=true: none is created; the current context
     │         is checked for a default StorageClass, no node-local-dns
     │         DaemonSet and a Ready node, and under WITH_NFS=true for no
     │         CSIDriver nfs.csi.k8s.io of another installer and for a node
     │         network that holds every node)
     │
Step 2 ── Install flux-operator + apply FluxInstance
     │         kubectl apply -f flux-operator install.yaml
     │         kubectl apply -f deploy/flux-system/fluxinstance.yaml
     │         wait_for_fluxinstance polls Ready condition
     │
     ├── Install Gateway API standard CRDs
     │         kubectl apply --server-side -f <upstream standard-install.yaml>
     │         Required by the keystone-operator HTTPRoute watch; version
     │         pinned via GATEWAY_API_VERSION, default matches go.mod.
     │         Skipped when all ten standard-channel CRDs of the pinned
     │         bundle already exist at that bundle version, or at a newer
     │         or unversioned one (this step never downgrades); a complete
     │         live set OLDER than the pin is upgraded in place with the
     │         same server-side apply. The bundle also ships the
     │         safe-upgrades ValidatingAdmissionPolicy, which denies
     │         applying experimental-channel CRDs over the standard
     │         channel.
     │
     ├── Install Envoy Gateway CRDs (gateway.envoyproxy.io)
     │         kubectl apply --server-side -f <upstream envoy-gateway-crds.yaml>
     │         Version pinned via ENVOY_GATEWAY_VERSION, kept inside the
     │         envoy-gateway chart's SemVer range. The `envoy-gateway`
     │         HelmRelease runs with `crds.enabled: false` — its bundled
     │         CRD copy carries the experimental Gateway API channel,
     │         which the safe-upgrades policy above refuses over the
     │         standard pin — so this step is the only owner of the
     │         gateway.envoyproxy.io group. Skipped when all eight CRDs
     │         already exist (they carry no comparable version
     │         annotation, so present sets are never re-asserted).
     │
     ├── Install Envoy Gateway + Gateway/openstack-gw (kind-only)
     │         Installed as part of the deploy/kind/base/ overlay applied
     │         in Step 3: the `envoy-gateway` HelmRelease brings up the
     │         control plane, and deploy/kind/base/openstack-gateway.yaml
     │         creates GatewayClass/envoy (parametersRef → EnvoyProxy with
     │         NodePort 31443), a cert-manager Certificate for
     │         keystone.127-0-0-1.nip.io signed by selfsigned-cluster-issuer,
     │         and Gateway/openstack-gw on :443. wait_for_gateway_programmed
     │         polls Programmed=True after Phase 3.
     │         The production deploy/flux-system/ overlay does NOT ship
     │         these resources — operators pick their own Gateway
     │         implementation in production.
     │
Step 3 ── Apply base kustomize overlay (deploy/kind/base/)
     │         Namespaces, HelmRepositories, HelmReleases
     │
Step 4 ── Wait for HelmReleases Ready
     │         cert-manager, openbao, mariadb-operator,
     │         external-secrets, memcached-operator
     │
     ├── Phase 1 → 2: cert-manager webhook admits a dry-run
     │         On an install, the release's startup API check holds
     │         Ready until the webhook admits a request. A server-side
     │         dry-run of cluster-issuer.yaml (WEBHOOK_TIMEOUT) is the
     │         script's own gate in front of the TLS-prerequisite
     │         applies, for an upgrade, which runs no hook, and for an
     │         overlay that turns the check off.
     │
     ├── Phase 3b: kustomization/rabbitmq-cluster-operator Ready
     │         The RabbitMQ Cluster Operator arrives as a Flux
     │         Kustomization, which the HelmRelease wait above cannot
     │         see. This wait hard-fails: a ControlPlane projects a
     │         RabbitmqCluster for spec.infrastructure.messaging, so
     │         the operator belongs on every cluster this script
     │         provisions.
     │
Step 5 ── Apply infrastructure kustomize overlay (deploy/kind/infrastructure/)
     │         ClusterIssuer, MariaDB CR, Memcached CR,
     │         OpenBao TLS cert, ESO resources
     │         Gated by wait_for_crds on the operator CRDs: memcacheds,
     │         externalsecrets, clustersecretstores, mariadbs,
     │         envoyproxies, the three garage kinds, openbaoclusters,
     │         rabbitmqclusters.rabbitmq.com
     │
Step 6 ── Wait for OpenBao pods Ready
     │
Step 7 ── OpenBao bootstrap
     │         init-unseal → setup-secret-engines →
     │         setup-auth → setup-policies →
     │         write-bootstrap-secrets
     │
Step 8 ── Wait for ExternalSecrets synced
     │         keystone-admin, keystone-db,
     │         mariadb-root-password
     │
     └── WITH_CONTROLPLANE=true: ControlPlane admission
              On the flux path, unless INFRA_ONLY=true: the ten operator
              HelmReleases Ready (HELMRELEASE_TIMEOUT), then one CRD per
              operator registered (POD_TIMEOUT). Then a server-side
              dry-run of the ControlPlane manifests (WEBHOOK_TIMEOUT)
              until the API server admits or denies it:
              the bundled CR before its one apply under
              WITH_CONTROLPLANE_CR=true, otherwise, after the two waits,
              the render of the overlay's controlplane/ directory or the
              kind controlplane.yaml before the by-hand hint.
```

**kind-only ExternalSecret shims.** The `keystone-admin`, `keystone-db`, and
`mariadb-root-password` ExternalSecrets shown above are **kind-overlay shims**
(`deploy/kind/infrastructure/`), not part of the production base. The production
`deploy/eso/` stack ships only the `ClusterSecretStore`: in a ControlPlane-based
deployment the admin password is projected per ControlPlane by the c5c3-operator, and a
non-kind Flux MariaDB baseline provides the `mariadb-root-password` Secret itself.

**Why two-phase kustomize?** The base kustomization contains only built-in Kubernetes
types (Namespaces, HelmRepository, HelmRelease). The infrastructure kustomization
contains CRD-dependent resources (ClusterIssuer, MariaDB CR, Memcached CR) that require
operator CRDs to be installed first. Applying them in two phases prevents
`kubectl apply` failures on fresh clusters where CRDs do not yet exist.

### Idempotent Re-runs

Re-running `make deploy-infra` against an existing cluster converges rather than
failing — each step detects the work it already completed and skips it:

- **Step 1** skips cluster creation when the kind cluster already exists.
- The **containerd nofile cap** skips the write, the containerd restart, and the
  node-Ready wait only when the node's drop-in *and* the limit the running
  containerd reports both already match. Checking the drop-in alone would
  permanently skip a node whose write landed but whose restart failed, leaving
  containerd uncapped behind a clean-looking deploy.
- **Gateway API CRDs** are skipped when all five standard-channel CRDs are
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

```text
deploy/kind/
├── base/
│   └── kustomization.yaml          References ../../flux-system/
│                                    Patches OpenBao HelmRelease → standalone mode, 100m CPU
│                                    Patches operator HelmReleases → 1 replica
│                                    Patches FluxInstance → 25m CPU per Flux controller
└── infrastructure/
    └── kustomization.yaml          References ../../flux-system/infrastructure/
                                     Patches MariaDB CR → 1 replica, no Galera, 1Gi memory
                                     Patches Memcached CR → 1 replica
```

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
| Storage class | default | `standard` |
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

Other operators (cert-manager, mariadb-operator, ESO, memcached-operator) are not
patched — they are single-replica or stateless by default.

### Lab Overlay Patches

```text
deploy/lab/metal-stack/
├── base/
│   └── kustomization.yaml          References ../../../kind/base/
│                                    Patches OpenBao HelmRelease → removes the storage class,
│                                    every Namespace → Gardener apiserver-proxy opt-out label
├── controlplane/                   The quick start's OVNCentral and ControlPlane CR (#1141), applied by hand
│   ├── kustomization.yaml          Lists the two manifests below
│   ├── ovncentral.yaml             OVNCentral controlplane-ovn, as on the quick-start page
│   └── controlplane-lab.yaml       ControlPlane controlplane, plus global_physnet_mtu and hypervisorOperator
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
│   ├── namespace.yaml              Namespace hypervisor-system
│   └── reservation-daemonset.yaml  DaemonSet migration-port-reservation on every node
├── nfs/                            The NFS server and csi-driver-nfs (#1196), applied under WITH_NFS=true
│   ├── kustomization.yaml          References ../../../kind/nfs/
│   │                                Patches the export claim → 100Gi, no storage class,
│   │                                nfs-server → init container load-nfsd
│   ├── client-modules-daemonset.yaml  DaemonSet nfs-client-modules on every node
│   └── client-policy.yaml          NetworkPolicy template, outside the kustomization:
│                                    only the node network reaches the server on 2049
└── probe/                          Node probe and NFS module load test, applied by hand
    ├── kustomization.yaml          Lists node-probe.yaml alone
    ├── node-probe.yaml             Read-only node probe Job (#1139)
    └── nfs-module-load.yaml        NFS module load test Job (#1194), outside the kustomization
```

`EXTERNAL_CLUSTER=true` applies `base/` in Step 3 and `infrastructure/` in
Step 5 in place of the kind overlays, and under `WITH_NFS=true` it applies
`nfs/` in Step 3 in place of `deploy/kind/nfs`. Each takes its kind overlay as
its base, so the lab inherits every patch above. It removes the storage class,
labels the Namespaces with Gardener's apiserver-proxy opt-out and loads the
NFS kernel modules in pods, and changes nothing else. The script never applies
`controlplane/`; the completion hint of
`WITH_CONTROLPLANE=true` names it. Nor does it apply `hypervisor/` or
`hypervisor-fixtures/`, which follow the ControlPlane by hand. Every volume of
the lab, the proving `OpenBaoCluster`'s and the NFS export claim included,
binds to the cluster's default class, which Step 1 checks exists.

| Setting | Kind | Lab |
| --- | --- | --- |
| OpenBao storage class (`dataStorage`) | `standard` | the cluster's default class |
| MariaDB storage class | `standard` | the cluster's default class |
| Garage storage class (metadata and data) | `standard` | the cluster's default class |
| Namespace label `apiserver-proxy.networking.gardener.cloud/inject` | absent | `disable` |
| NFS export claim (`nfs-server-exports`) | `standard`, 5Gi | the cluster's default class, 100Gi |
| NFS kernel modules | `modprobe` on the host by the deploy script | `load-nfsd` in the server pod and the DaemonSet `nfs-client-modules` |
| Clients admitted to the NFS server's port 2049 | every pod and node | the node network, by the NetworkPolicy `nfs-server-clients` |
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
| `WITH_NFS` | `false` | When `true`, deploy the NFS server in `openstack` and the `csi-driver-nfs` mounter in `kube-system` in Step 3, wait for the `nfs-server` rollout, and append `csi-driver-nfs` to the Phase 3 HelmRelease wait. In kind mode it first runs `modprobe` for `nfsd`, `nfs` and `nfsv4` on the host before the cluster is created and applies the `deploy/kind/nfs` overlay; a rollout failure stops the run with an error naming the `nfsd` module. The module load is Linux only and needs root or passwordless sudo: without either the script logs a warning and continues. Under `EXTERNAL_CLUSTER=true` it loads nothing on the host and applies `<overlay>/nfs` instead, whose init container `load-nfsd` and DaemonSet `nfs-client-modules` load the modules on the nodes; it also waits for that DaemonSet's rollout, and either failed wait stops the run with an error naming the log to read. When the overlay ships `nfs/client-policy.yaml`, it applies that NetworkPolicy before `<overlay>/nfs`, with the node network of Step 1 in place of the placeholder `NODE_NETWORK`. The overlays are described in [Infrastructure Manifests](infrastructure-manifests.md#nfs-storage-stack-opt-in) and [Lab NFS stack](infrastructure-manifests.md#lab-nfs-stack) |
| `WITH_MESSAGING` | `false` | When `true`, apply the `deploy/kind/messaging` overlay after Step 5 and wait for `rabbitmqcluster/shared-rabbitmq` in `openstack` to report `AllReplicasReady`. A timeout stops the run and prints the `kubectl describe` output for the broker plus the events of its server pod. The overlay is described in [Infrastructure Manifests](infrastructure-manifests.md#message-bus-kind-only-opt-in). The `e2e-controlplane` job is the broker's second consumer after the cinder `e2e-operator` leg: the full-ControlPlane suite takes a vhost of its own through `tests/e2e/cinder/broker-vhost.sh` and hands the ControlPlane the transport URL of that vhost brownfield |
| `WITH_REGISTRY_CACHE` | `false` | Local-dev only. When `true`, bring up one distribution-registry (`registry:2`) pull-through proxy per upstream registry (`docker.io`, `ghcr.io`, `registry.k8s.io`, `quay.io`, plus the vanity fronts `oci.external-secrets.io` and `docker-registry3.mariadb.com`) on the `kind` Docker network and wire every node's containerd at them via a `certs.d/<host>/hosts.toml` mirror, so unmodified image refs are served from a persistent local cache that survives `kind delete`. The proxy streams and caches inline (fast even on a cold pull). The containerd mirror patch is injected only into the deploy-time kind config, never the checked-in `hack/kind-config.yaml`, so CI is unaffected. Requires `yq`. See the [Extended Quick Start](../../quick-start-extended.md) |
| `PURGE_REGISTRY_CACHE` | `false` | Consumed by `make teardown-infra`. When `true`, also remove the registry pull-through cache containers and their volumes (identified by the `cobaltcore.registry-cache=true` label). The default leaves them running so the warm cache is reused on the next deploy |
| `EXTERNAL_CLUSTER` | `false` | When `true`, deploy onto the cluster the current kubeconfig context points at (the script never switches contexts) with the `EXTERNAL_OVERLAY` overlays in Steps 3 and 5. Docker and kind are not required. Preflight refuses the six kind-bound opt-ins `WITH_VPA`, `WITH_METRICS_SERVER`, `WITH_REGISTRY_CACHE`, `WITH_CHAOS_MESH`, `WITH_OVN_KERNEL_MODULES` and `WITH_DIZZY`, accepts `WITH_NFS=true` only when `<overlay>/nfs/kustomization.yaml` exists, requires the context's API server to answer, and logs the context and the server URL. Step 1 creates no cluster; it checks for a default StorageClass, for the absence of a `node-local-dns` DaemonSet in `kube-system` and for a Ready node, and logs the class and the node names. Under `WITH_NFS=true` it also refuses a `CSIDriver/nfs.csi.k8s.io` whose `helm.toolkit.fluxcd.io` labels do not name the HelmRelease `kube-system/csi-driver-nfs`: the cluster then runs its own NFS CSI driver, which Step 3 would replace and the HelmRelease would adopt. For an overlay with `nfs/client-policy.yaml` it then reads the node network from `data.nodeNetwork` of the ConfigMap `kube-system/shoot-info`, logs it, and refuses a cluster where that value is missing or no IPv4 CIDR, or where a node has no IPv4 `InternalIP` inside it. The nofile cap and the Keystone image preload are skipped. The Gateway is reached through `kubectl port-forward` on local port 8443, which the completion banner prints, and the bundled ControlPlane CR's `publicEndpoint` gets `:8443`. Also consumed by `make teardown-infra`. Any other value keeps the kind mode |
| `EXTERNAL_OVERLAY` | `deploy/lab/metal-stack` | Overlay root of `EXTERNAL_CLUSTER=true`: its `base/` and `infrastructure/` replace `deploy/kind/base` and `deploy/kind/infrastructure`. Relative to the repository root unless absolute. Preflight fails when either kustomization is missing. An overlay may also carry a `controlplane/` kustomization, which `make deploy-infra` names in its `WITH_CONTROLPLANE=true` completion hint and never applies; while it exists and `WITH_CONTROLPLANE_CR` is not `true`, preflight renders it and refuses a failing render, a render with no ControlPlane or more than one, or a ControlPlane other than `openstack/<CONTROLPLANE_NAME>`, the only one Step 7 seeds. A `hypervisor/` kustomization, with `hypervisor-fixtures/` beside it, is applied by hand as well; `make teardown-infra` removes both in its step 0. An optional `nfs/` kustomization is what Step 3 applies under `WITH_NFS=true` in place of `deploy/kind/nfs`; it has to render the Deployment `nfs-server` and the DaemonSet `nfs-client-modules` in `openstack` and the HelmRelease `csi-driver-nfs` in `kube-system`, the three objects the script waits for, and `make teardown-infra` removes it at the end of its step 2. `nfs/` may also ship `client-policy.yaml`, a NetworkPolicy template outside the kustomization whose placeholder `NODE_NETWORK` the script replaces with the node network of a Gardener shoot; an overlay for a cluster without the ConfigMap `kube-system/shoot-info` ships none. Read by `make deploy-infra` and `make teardown-infra` |
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
| 2 | `nfs-server` rolled out and its EndpointSlices carry at least one address | `openstack` | `Deployment`, `EndpointSlice` |
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
