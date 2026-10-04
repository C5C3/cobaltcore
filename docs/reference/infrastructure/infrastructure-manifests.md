---
title: Infrastructure Manifests
quadrant: infrastructure
---

# Infrastructure Manifests

Reference documentation for the FluxCD infrastructure manifests. These manifests
define HelmRepository sources, HelmRelease operators, and infrastructure custom resources
that provision the shared platform services required by OpenStack operators. Deployment is
split into two phases: base resources (namespaces, sources, releases) and CRD-dependent
infrastructure resources (applied after operators install their CRDs).

## Directory Layout

```text
deploy/
└── flux-system/
    ├── kustomization.yaml                Base kustomize overlay (namespaces, FluxInstance, sources, releases)
    ├── namespaces.yaml                   Namespace resources for all components
    ├── fluxinstance.yaml                 FluxInstance CR driving the flux-operator
    ├── sources/                          FluxCD HelmRepository CRs
    │   ├── cert-manager.yaml             Jetstack Helm chart registry
    │   ├── mariadb-operator.yaml         MariaDB Operator Helm chart registry
    │   ├── external-secrets.yaml         External Secrets Operator Helm chart registry
    │   ├── openbao.yaml                  OpenBao Helm chart registry
    │   ├── openbao-operator.yaml         OpenBao Operator OCI chart artifact (digest-pinned OCIRepository)
    │   ├── c5c3-charts.yaml              C5C3 shared OCI chart registry
    │   ├── k-orc.yaml                    K-ORC (OpenStack Resource Controller) Helm chart registry
    │   ├── rabbitmq-cluster-operator.yaml RabbitMQ Cluster Operator GitRepository (tag + commit pinned)
    │   ├── prometheus-community.yaml     Prometheus Community OCI chart registry
    │   └── chaos-mesh.yaml               Chaos Mesh Helm chart registry (kind-only addon — see "Kind Overlay Demo Addons")
    ├── releases/                         FluxCD HelmRelease CRs
    │   ├── cert-manager.yaml             cert-manager
    │   ├── prometheus-operator-crds.yaml Prometheus Operator CRDs
    │   ├── mariadb-operator-crds.yaml    MariaDB Operator CRDs
    │   ├── mariadb-operator.yaml         MariaDB Operator
    │   ├── external-secrets.yaml         External Secrets Operator
    │   ├── memcached-operator.yaml       Memcached Operator (from c5c3-charts)
    │   ├── openbao.yaml                  OpenBao HA Raft cluster
    │   ├── openbao-operator.yaml         OpenBao Operator (per-service OpenBao instances)
    │   ├── keystone-operator.yaml        Keystone Operator (from c5c3-charts)
    │   ├── glance-operator.yaml          Glance Operator (from c5c3-charts)
    │   ├── placement-operator.yaml       Placement Operator (from c5c3-charts)
    │   ├── ovn-operator.yaml             OVN Operator (from c5c3-charts)
    │   ├── neutron-operator.yaml         Neutron Operator (from c5c3-charts)
    │   ├── cinder-operator.yaml          Cinder Operator (from c5c3-charts)
    │   ├── nova-operator.yaml            Nova Operator (from c5c3-charts)
    │   ├── k-orc.yaml                    K-ORC OpenStack Resource Controller
    │   ├── rabbitmq-cluster-operator.yaml RabbitMQ Cluster Operator (Flux Kustomization over config/installation)
    │   ├── c5c3-operator.yaml            c5c3-operator ControlPlane orchestrator (from c5c3-charts)
    │   └── chaos-mesh.yaml               Chaos Mesh (kind-only addon — see "Kind Overlay Demo Addons")
    └── infrastructure/                   CRD-dependent infrastructure resources
        ├── kustomization.yaml            Infrastructure kustomize overlay
        ├── cluster-issuer.yaml           Self-signed ClusterIssuer (requires cert-manager CRDs)
        ├── db-ca-issuer.yaml             OpenStack DB CA Certificate + ClusterIssuer
        ├── ovn-ca-issuer.yaml            OVN CA Certificate + ClusterIssuer
        ├── mariadb.yaml                  MariaDB Galera cluster for OpenStack (with TLS)
        └── memcached.yaml                Memcached cluster for OpenStack
```

The proving `OpenBaoCluster` instance is **not** part of this tree. It is CI/dev-only and
lives at `deploy/kind/infrastructure/openbao-instance.yaml` — see
[OpenBao Proving Instance](#openbao-proving-instance).

All YAML files carry the SPDX Apache-2.0 license header (3 lines: copyright, blank
comment, license identifier).

## Namespaces

Twenty-one `Namespace` resources are defined in `namespaces.yaml` and included as the first
entry in the base kustomization. Kustomize applies `Namespace` resources before other
resource kinds, ensuring target namespaces exist before any namespaced resources are
created.

| Namespace | Purpose |
| --- | --- |
| `cert-manager` | cert-manager operator and its resources |
| `mariadb-system` | MariaDB Operator |
| `external-secrets` | External Secrets Operator |
| `monitoring` | Prometheus Operator CRDs (consumed by the optional kube-prometheus-stack kind overlay) |
| `memcached-system` | Memcached Operator |
| `garage-system` | Garage Operator (S3 object store for the CI/e2e stack) |
| `keystone-system` | Keystone Operator controller (workload CRs continue to live in `openstack`) |
| `horizon-system` | Horizon Operator controller (Horizon CRs and the operator-managed dashboard live in `openstack`) |
| `glance-system` | Glance Operator controller (Glance/GlanceBackend CRs and the operator-managed payload live in `openstack`) |
| `placement-system` | Placement Operator controller (Placement CRs and the operator-managed payload live in `openstack`) |
| `barbican-system` | Barbican Operator controller (Barbican/BarbicanSecretStore CRs and the operator-managed payload live in `openstack`) |
| `ovn-system` | OVN Operator controller (OVNCentral/OVNChassis CRs and the operator-managed payload live in the tenant namespace they are created in) |
| `neutron-system` | Neutron Operator controller (Neutron/NeutronMetadataAgent CRs and the operator-managed payload live in the tenant namespace they are created in) |
| `cinder-system` | Cinder Operator controller (Cinder/CinderBackend/CinderBackupBackend CRs and the operator-managed payload live in the tenant namespace they are created in) |
| `nova-system` | Nova Operator controller (Nova CRs and the operator-managed payload live in the tenant namespace they are created in) |
| `openstack` | Infrastructure instance CRs that exist to run the operators standalone (MariaDB cluster, Memcached cluster; on kind also the OpenBao proving instance and the shared Gateway) |
| `shared-services` | Infrastructure consumed by more than one control plane: the OpenBao HA Raft cluster and the Garage object store |
| `openbao-operator-system` | openbao-operator controller. It stays out of `shared-services` so the shared OpenBao cluster and the operator that manages per-service instances keep separate lifecycles |
| `c5c3-system` | c5c3-operator controller; the `ControlPlane` and its child CRs are created in the `ControlPlane`'s own namespace |
| `orc-system` | K-ORC (OpenStack Resource Controller) and its installer resources |
| `rabbitmq-system` | RabbitMQ Cluster Operator controller and its webhook certificates. A `RabbitmqCluster` a ControlPlane declares lives in that ControlPlane's own namespace, not here |

`shared-services` is a trust zone, not just a placement bucket. It holds the
credentials that unlock every other secret in the stack — `openbao-init-keys` (the root
token and all Shamir unseal key shares, in plaintext), `openbao-tls`, and
`eso-openbao-client-tls` — and it is on the `openbao-cluster-store` allow-list. Read
access to Secrets in this namespace, whether through RBAC or through an `ExternalSecret`
created there, is therefore equivalent to read access to the whole store. Garage is the
one accepted co-tenant and holds neither grant; do not add a workload or a
Secret-reading `Role` here without treating it as a security review.

The `chaos-mesh` namespace is **not** part of the production base. It is created
inline by the kind-only opt-in overlay at `deploy/kind/chaos-mesh/` when
`WITH_CHAOS_MESH=true make deploy-infra` is used. See
[Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in) below.

**Note:** The `install.createNamespace: true` setting on HelmReleases instructs FluxCD's
helm-controller to create namespaces when installing charts. However, this does not help
when applying HelmRelease CRs via `kubectl apply -k` — the target namespace must already
exist for the API server to accept namespaced resources. The explicit `Namespace` resources
solve this chicken-and-egg problem.

### Migrating a cluster deployed before the relocation

The OpenBao cluster moved out of `openbao-system`, and Garage out of `openstack`, into
`shared-services`. Both keep their state in namespace-scoped `StatefulSet` PVCs, so the
move brings up **empty** volumes and leaves the originals running: `kubectl apply -k`
does not prune, and the `FluxInstance` declares no `spec.sync`, so Flux does not either.
An abandoned OpenBao keeps serving every historical secret alongside its plaintext root
token, and an abandoned Garage keeps holding the objects Glance's database still points
at while Glance is repointed at empty buckets.

`make deploy-infra` refuses to run against such a cluster and names what to delete. On a
cluster you do not intend to recreate, capture the old store first — `openbao-init-keys`
holds the root token and every Shamir unseal-key share, and it has no second copy:

```bash
(umask 077 && kubectl get secret openbao-init-keys -n openbao-system -o yaml \
  > ~/openbao-init-keys.backup.yaml)
```

That file carries the root token and every unseal share in the clear, so it unseals the
retired instance forever and expires never. Keep it where you keep a root credential —
outside the working tree, so a routine `git add -A` cannot stage it — and delete it once
the secrets it protects have been re-applied at the source.

Then delete both explicitly. The `openstack` namespace also holds the MariaDB volume
carrying the Keystone database, so scope the PVC listing to Garage rather than deleting
from an unfiltered one:

```bash
kubectl delete namespace openbao-system
kubectl delete garagecluster garage -n openstack
kubectl get pvc -n openstack | grep garage   # delete only these
```

Re-bootstrapping OpenBao afterwards re-seeds `bootstrap/*` from scratch, so any secret
that was rotated in the old instance has to be re-applied at the source. To read those
secrets out of the old instance before it goes away, run
`ALLOW_PRE_RELOCATION=true make deploy-infra` instead: the guard downgrades to a warning
and both stacks run side by side for a migration window. That defers the split-brain
rather than resolving it — delete the retired stack once the new one serves.

## FluxInstance

**File:** `deploy/flux-system/fluxinstance.yaml`

A single `FluxInstance` CR drives the
[flux-operator](https://github.com/controlplaneio-fluxcd/flux-operator), which replaces
the imperative `flux install` / `flux bootstrap` path with a declarative,
operator-managed Flux lifecycle. The flux-operator reconciles the Flux
controller Deployments from this spec and publishes a `FluxReport/flux` summarizing the
installation state.

| Property | Value |
| --- | --- |
| API version | `fluxcd.controlplane.io/v1` |
| Kind | `FluxInstance` |
| Name | `flux` |
| Namespace | `flux-system` |

**Spec fields:**

| Field | Value | Purpose |
| --- | --- | --- |
| `distribution.version` | `"2.x"` | Minor-version track pinned by the operator; picks the latest Flux 2.x release |
| `distribution.registry` | `ghcr.io/fluxcd` | Controller image registry |
| `components` | `source-controller`, `kustomize-controller`, `helm-controller`, `notification-controller` | Four Flux controllers installed — image-automation and image-reflector controllers are omitted (not used in this project) |
| `cluster.type` | `kubernetes` | Generic Kubernetes distribution (not OpenShift/EKS-specific) |
| `cluster.size` | `small` | Small resource profile suitable for single-node kind and low-traffic management clusters |
| `cluster.multitenant` | `false` | Cross-namespace references allowed — simplifies the single-tenant management cluster model |
| `cluster.networkPolicy` | `false` | No NetworkPolicies applied to flux-system (kind overlay assumes a permissive default; production overlays opt in) |

**No `spec.sync` block.** The kind Quick Start applies Helm sources and releases
directly via `kubectl apply -k deploy/kind/base/`, so the `FluxInstance` here does not
carry a `GitRepository` sync. Production overlays that want continuous reconciliation
from Git add a `spec.sync` block on top of this base, with the overlay technique
that [Sizing and placement overrides](#sizing-and-placement-overrides) uses.

**Kustomize ordering.** Kustomize applies `Namespace` resources first by default, so
`flux-system` exists before the `FluxInstance` is created. The flux-operator itself is
installed out-of-band by `hack/deploy-infra.sh` (pinned `FLUX_OPERATOR_VERSION`,
applied via `kubectl apply -f install.yaml`) before this kustomization is applied.

## HelmRepository Sources

Seven HelmRepository CRs define the Helm chart registries that FluxCD pulls from. All
use `apiVersion: source.toolkit.fluxcd.io/v1`, are deployed to the `flux-system`
namespace, and poll at `interval: 1h`.

| File | `metadata.name` | Registry URL | Type |
| --- | --- | --- | --- |
| `sources/cert-manager.yaml` | `cert-manager` | `https://charts.jetstack.io` | HTTPS |
| `sources/mariadb-operator.yaml` | `mariadb-operator` | `https://mariadb-operator.github.io/mariadb-operator` | HTTPS |
| `sources/external-secrets.yaml` | `external-secrets` | `https://charts.external-secrets.io` | HTTPS |
| `sources/openbao.yaml` | `openbao` | `https://openbao.github.io/openbao-helm` | HTTPS |
| `sources/c5c3-charts.yaml` | `c5c3-charts` | `oci://ghcr.io/c5c3/charts` | OCI |
| `sources/prometheus-community.yaml` | `prometheus-community` | `oci://ghcr.io/prometheus-community/charts` | OCI |
| `sources/garage-operator.yaml` | `garage-operator` | `oci://ghcr.io/rajsinghtech/charts` | OCI |

**The openbao-operator chart is sourced from an OCIRepository, not a HelmRepository.**
`sources/openbao-operator.yaml` addresses the chart artifact directly
(`oci://ghcr.io/dc-tec/charts/openbao-operator`) and pins `spec.ref.digest` next to
`spec.ref.tag`. A HelmRepository can only carry a chart *version*, and on a mutable OCI
tag that gates version drift alone: Flux resolves the tag on every interval and tracks
the resulting artifact digest, so a re-pushed tag upgrades the release with no Git change
and no reviewer. See [OpenBao Operator](#openbao-operator) for why that matters for this
particular chart.

**K-ORC is sourced from Git, not Helm.** K-ORC publishes no Helm chart (its
`github.io` page serves no Helm index), so `sources/k-orc.yaml` is a `GitRepository`
— still `source.toolkit.fluxcd.io/v1`, in `flux-system`, polling at `interval: 1h`
— pinned by `ref.commit` to an upstream `main` commit. No released K-ORC ships the
`RoleAssignment` and `Region` kinds the c5c3-operator owns, so the pin returns to a
release tag once one does. `spec.sparseCheckout` limits the checkout to `config`, and
`spec.ignore` scopes the artifact to the same directory. The sparse checkout is what
keeps the source working while upstream moves: for a `ref.commit` the source-controller
clones the tip of `main` and hard-resets to the pin, and that reset fails when the tip
carries a symlink to a non-empty directory that the pin lacks. A sparse checkout starts
from an empty worktree, so paths outside `config` are never removed.
`tests/unit/deploy/korc_flux_source_test.sh` checks that the list covers the path the
Kustomization builds.
It is applied by a Flux `Kustomization`, not a HelmRelease; see
[K-ORC (OpenStack Resource Controller)](#k-orc-openstack-resource-controller).

**The RabbitMQ Cluster Operator is sourced from Git too.** Upstream publishes no
chart, so `sources/rabbitmq-cluster-operator.yaml` is a second `GitRepository` in
`flux-system` at `interval: 1h`, scoped to `/config` via `spec.ignore`. It pins
both halves of the ref: `ref.tag: "v2.22.5"` as the readable version and as
Renovate's lookup handle, and `ref.commit` as the content pin. Flux's gogit
client evaluates `commit` ahead of every other ref field, so a re-pushed tag
cannot hand different bytes to a controller that runs with cluster-wide RBAC.
This source is applied by a Flux `Kustomization` as well; see
[RabbitMQ Cluster Operator](#rabbitmq-cluster-operator).

The `chaos-mesh` HelmRepository ships in the kind-only opt-in overlay at
`deploy/kind/chaos-mesh/source.yaml` — it is intentionally absent
from `deploy/flux-system/{sources,kustomization.yaml}`. See
[Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in).

The `c5c3-charts`, `prometheus-community`, and `garage-operator` repositories are
OCI-type sources (`spec.type: oci`). `c5c3-charts` hosts internally-built operator
charts (e.g., memcached-operator) in the GitHub Container Registry;
`prometheus-community` hosts Prometheus community charts (e.g.,
prometheus-operator-crds); `garage-operator` is the first **third-party** OCI-type
source and hosts the upstream garage-operator chart. For every OCI-type HelmRepository
the registry URL is the chart *namespace* and the chart name lives in the HelmRelease's
`chart.spec.chart` — the OCIRepository above is the exception, because it names one
artifact. All other repositories use standard HTTPS Helm registries.

## HelmRelease Operators

Fifteen HelmRelease CRs deploy the infrastructure operators and CRD charts (K-ORC and the
RabbitMQ Cluster Operator are applied separately, each via a Flux `Kustomization` — see
[K-ORC (OpenStack Resource Controller)](#k-orc-openstack-resource-controller) and
[RabbitMQ Cluster Operator](#rabbitmq-cluster-operator)). All use
`apiVersion: helm.toolkit.fluxcd.io/v2` and share these common settings:

| Setting | Value | Purpose |
| --- | --- | --- |
| `spec.interval` | `30m` | Reconciliation interval |
| `spec.install.crds` | `CreateReplace` | Install CRDs if missing, replace if outdated |
| `spec.install.createNamespace` | `true` | Auto-create target namespace |
| `spec.upgrade.crds` | `CreateReplace` | Update CRDs on chart upgrade |
| `spec.upgrade.remediation.retries` | `3` | Retry failed upgrades up to 3 times |

### Dependency Order

cert-manager is the base layer (no `dependsOn`). The CRD-only charts
(prometheus-operator-crds, mariadb-operator-crds) also have no dependencies. All other
operators depend on cert-manager because they require TLS certificates for webhook
servers. Some operators have additional dependencies on CRD charts or other operators:

```text
cert-manager              (base — no dependencies)
prometheus-operator-crds  (no dependencies)
mariadb-operator-crds     (no dependencies)
├── mariadb-operator      dependsOn: cert-manager, mariadb-operator-crds
├── external-secrets      dependsOn: cert-manager
├── memcached-operator    dependsOn: cert-manager, prometheus-operator-crds
├── garage-operator       dependsOn: cert-manager
├── openbao-operator      dependsOn: cert-manager
├── openbao               dependsOn: cert-manager
├── keystone-operator     dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets
├── horizon-operator      dependsOn: cert-manager, memcached-operator, external-secrets, keystone-operator
├── glance-operator       dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets, keystone-operator
├── placement-operator    dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets, keystone-operator
├── barbican-operator     dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets, keystone-operator, openbao-operator
├── ovn-operator          dependsOn: cert-manager
├── neutron-operator      dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets, keystone-operator, ovn-operator
├── cinder-operator       dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets, keystone-operator
├── nova-operator         dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets, keystone-operator
└── c5c3-operator         dependsOn: keystone-operator, external-secrets, mariadb-operator, memcached-operator
```

K-ORC is **not** in this graph: it is applied by a Flux `Kustomization`, not a
HelmRelease, and a HelmRelease `dependsOn` can only reference other HelmReleases. The
c5c3-operator therefore does **not** `dependsOn` K-ORC even though K-ORC is a **hard
dependency**: `SetupWithManager` `Owns` the K-ORC kinds, so the manager only starts
once those CRDs are installed (until then the pod restarts), and converges once they
appear. The crash-looping pod also fails the install Helm waits on, so
`c5c3-operator` is the one release that widens `spec.install.remediation.retries`
to `30`: the default budget of `3` can be spent before the `k-orc` Kustomization
lands, and a spent one stalls the release until the chart, the values or the spec
change. At the 5m default wait, `30` attempts are ~2.5h of installing. It stays
bounded on purpose — `-1` would never set `Stalled` on a permanently broken
install, and each remediation uninstalls the release, so an unbounded budget keeps
reopening a window in which the templated webhook configurations are gone while
the CRDs under `crds/` survive. Its `spec.upgrade.remediation.retries` stays at
`3`, where remediation rolls back to the running operator.

The `rabbitmq-cluster-operator` Kustomization is outside the graph for the same
reason, and it needs cert-manager: the upstream base carries the two admission
webhooks, a self-signed `Issuer`, and the `Certificate`s backing them. It declares
no `dependsOn`, because a Flux `Kustomization` can only depend on other
Kustomizations and cert-manager is a HelmRelease. On a cold cluster the
Issuer/Certificate apply fails while cert-manager's CRDs are still missing, the
Kustomization reports NotReady, and one of the following `retryInterval` passes
(2m) succeeds once cert-manager is up. The NotReady window is that convergence.
That is also why `neutron-operator`, `cinder-operator` and `nova-operator` carry
no edge to it: the Neutron agents, the Cinder services and every Nova process
talk over the shared message bus, but a HelmRelease cannot depend on a
Kustomization.

The `c5c3-operator` HelmRelease sits at the top of this graph: it
`dependsOn` the four operators whose CRs it projects (keystone-operator,
external-secrets, mariadb-operator, memcached-operator). It also drives K-ORC's
ApplicationCredential / Service / Endpoint CRDs, but K-ORC is applied by the separate
Flux `Kustomization` above, so it cannot be a `dependsOn` edge — the c5c3-operator's
manager instead requires those CRDs to be present at startup.

FluxCD resolves this dependency graph and installs operators in the correct order.
If cert-manager is not ready, dependent operators are held in a pending state.

The kind-only `chaos-mesh` HelmRelease (`deploy/kind/chaos-mesh/`) also
declares `dependsOn: cert-manager` but is only installed when
`WITH_CHAOS_MESH=true make deploy-infra` is used. Production overlays do not
install it. See [Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in).

### cert-manager

**File:** `deploy/flux-system/releases/cert-manager.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `cert-manager` |
| Chart | `cert-manager` |
| Version constraint | `>=1.16.0 <2.0.0` |
| Source | `cert-manager` HelmRepository |
| Dependencies | None (base layer) |

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `crds.enabled` | `true` | Install CRDs via the Helm chart |
| `prometheus.enabled` | `false` | Prometheus metrics disabled |
| `startupapicheck.enabled` | `true` | Run the startup API check Job, so the release is Ready only once the webhook admits a request |

The production release sets `startupapicheck.enabled`, and the kind and the
metal-stack lab base renders inherit it. Every release that depends on
cert-manager creates an `Issuer` or a `Certificate`. Without the check the
release turns `Ready` before the webhook admits, and a dependent release can
use up its install retries and stay `Stalled`.

The chart renders the check as Job `cert-manager-startupapicheck`, a
`post-install` hook. It runs on install, and helm-controller waits for it, so
`dependsOn: cert-manager` holds the dependent releases until the webhook
answers. An upgrade of an existing installation starts no Job.

A fresh install pulls `quay.io/jetstack/cert-manager-startupapicheck` at the
chart's app version. It is a fourth image beside the controller, cainjector
and webhook, from the same registry, and a cluster that mirrors or allowlists
images has to provide it.

The Job runs `check api --wait=1m` and restarts up to `backoffLimit: 4`. The
release sets no `spec.timeout`, so helm-controller's default release timeout
of 5 minutes bounds the wait. When the check does not pass in that time, the
install fails. The release has no install remediation, so it stays failed and
not `Ready`, the terminal state the install comment in
`deploy/flux-system/releases/external-secrets.yaml` describes. Every dependent
release waits at `dependsOn`, so the release that reports the failure is
cert-manager itself. `tests/unit/deploy/cert_manager_release_test.sh` pins the
value in the three renders.

### Prometheus Operator CRDs

**File:** `deploy/flux-system/releases/prometheus-operator-crds.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `monitoring` |
| Chart | `prometheus-operator-crds` |
| Version constraint | `>=17.0.0 <20.0.0` |
| Source | `prometheus-community` HelmRepository |
| Dependencies | None |

The Prometheus Operator CRDs chart installs ServiceMonitor, PodMonitor, PrometheusRule,
and related monitoring.coreos.com CRDs. These are required by the memcached-operator
controller, which unconditionally watches ServiceMonitor resources via Owns().

### MariaDB Operator CRDs

**File:** `deploy/flux-system/releases/mariadb-operator-crds.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `mariadb-system` |
| Chart | `mariadb-operator-crds` |
| Version constraint | `>=0.30.0 <1.0.0` |
| Source | `mariadb-operator` HelmRepository |
| Dependencies | None |

A separate CRD chart is required since mariadb-operator v0.35.0. Must be installed before
mariadb-operator so CRDs are available for the operator and for infrastructure CRs
(e.g., MariaDB Galera cluster).

### MariaDB Operator

**File:** `deploy/flux-system/releases/mariadb-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `mariadb-system` |
| Chart | `mariadb-operator` |
| Version constraint | `>=0.30.0 <1.0.0` |
| Source | `mariadb-operator` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace, `mariadb-operator-crds` in `mariadb-system` namespace |

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `metrics.enabled` | `false` | Prometheus metrics disabled |
| `webhook.enabled` | `true` | Enable admission webhooks for MariaDB CRDs |

### External Secrets Operator

**File:** `deploy/flux-system/releases/external-secrets.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `external-secrets` |
| Chart | `external-secrets` |
| Version constraint | `>=0.10.0 <1.0.0` |
| Source | `external-secrets` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace |

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `installCRDs` | `true` | Install CRDs via the Helm chart |
| `webhook.port` | `9443` | Webhook server listen port |
| `certController.enabled` | `true` | Manage webhook TLS certificates |

The production ESO kustomization renders the shared cluster-scoped
`ClusterSecretStore/openbao-cluster-store` (`deploy/eso/`), which remains the
**default** store every ControlPlane and its children use. Per-tenant namespaced
`SecretStore`s are **not** created here — they are provisioned per ControlPlane
by `deploy/openbao/bootstrap/setup-eso-tenant.sh` when a tenant opts in via
`spec.secretStoreRef` (see the
[OpenBao bootstrap reference](./openbao-bootstrap.md#setup-eso-tenant-sh) and the
[multi-tenant deployment guide](../../guides/multi-tenant-deployment.md#per-controlplane-secret-stores-and-openbao-identities)).

### Memcached Operator

**File:** `deploy/flux-system/releases/memcached-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `memcached-system` |
| Chart | `memcached-operator` |
| Version constraint | `>=0.1.0 <1.0.0` |
| Source | `c5c3-charts` HelmRepository (shared OCI registry) |
| Dependencies | `cert-manager` in `cert-manager` namespace, `prometheus-operator-crds` in `monitoring` namespace |

**Source reference:** The Memcached Operator chart is published to the shared `c5c3-charts`
OCI registry (`oci://ghcr.io/c5c3/charts`), not a dedicated HelmRepository. The
`sourceRef.name` is `c5c3-charts`, matching the OCI HelmRepository in `sources/`.

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `metrics.enabled` | `true` | Expose Prometheus metrics |
| `webhook.enabled` | `true` | Enable admission webhooks for Memcached CRDs |

### Garage Operator

**File:** `deploy/flux-system/releases/garage-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `garage-system` |
| Chart | `garage-operator` |
| Version constraint | `>=0.6.26 <1.0.0` |
| Source | `garage-operator` HelmRepository (third-party OCI registry) |
| Dependencies | `cert-manager` in `cert-manager` namespace |

The [garage-operator](https://github.com/rajsinghtech/garage-operator) deploys
[Garage](https://garagehq.deuxfleurs.fr), a lightweight S3-compatible object store, as
the in-cluster S3 backend for the CI/e2e stack (the Glance multi-store e2e suites need an
S3 endpoint before any glance-operator lands). Its instance CRs are described under
[Garage Object Store](#garage-object-store) below.

No Helm values are overridden. The Garage image rides the operator/chart releases (no
custom `GarageCluster.spec.image` pin), and the operator's admission/conversion webhooks
are on by default — hence the `dependsOn: cert-manager` edge.

**Accepted risk (decided 2026-07-15):** garage-operator is a young, single-maintainer,
pre-1.0 (v0.6.x) project. It is accepted for **test infrastructure only** — never a
production dependency of the operators — and the consuming surface is deliberately thin
(three instance CRs plus three ExternalSecrets), so a later provider swap stays local to
this layer.

### OpenBao Operator

**File:** `deploy/flux-system/releases/openbao-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `openbao-operator-system` |
| Chart | `openbao-operator` |
| Version constraint | `0.4.2`, pinned by artifact digest |
| Source | `openbao-operator` OCIRepository, referenced via `spec.chartRef` |
| Dependencies | `cert-manager` in `cert-manager` namespace |

The [openbao-operator](https://github.com/dc-tec/openbao-operator) manages
`OpenBaoCluster` instances: static-seal auto-unseal, cert-manager TLS in External mode,
declarative self-init, raft snapshots, and upgrade strategies. It delivers the
per-service OpenBao instances a Barbican secret store builds on. The in-repo instance it
manages is described under [OpenBao Proving Instance](#openbao-proving-instance). The
shared management OpenBao ([OpenBao](#openbao) below) stays on the upstream Helm chart
and is untouched by this operator.

The chart ships ValidatingAdmissionPolicies and requires Kubernetes 1.33 or newer. The
operator needs no certificate of its own from cert-manager, but the instance TLS
Certificates do, and `dependsOn: cert-manager` keeps this release behind the same base
layer as garage-operator.

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `tenancy.mode` | `multi` | Deploy the provisioner next to the controller, so the controller reconciles in every namespace an `OpenBaoTenant` admits |
| `tenancy.namespacePodSecurityLabels.mode` | `external` | Deny the provisioner every `Namespace` mutation, so it stamps no Pod Security labels |

The c5c3 operator projects a dedicated `OpenBaoCluster` into the Barbican service
namespace of every ControlPlane, and those namespaces are created at runtime, so no
single watched namespace covers them. Multi-tenant mode covers them: the controller
reconciles in every namespace an `OpenBaoTenant` has onboarded.

`tenancy.mode` only shapes the chart's own output. The controller reads its tenancy from
the `WATCH_NAMESPACE` environment variable, and no chart template sets it, so leaving
`controller.extraEnv` unset is what runs it multi-tenant. A value reintroduced there pins
the controller to one namespace and strands every other one.

Under the chart default (`enforce`) the provisioner holds update and patch on namespaces
cluster-wide and stamps restricted Pod Security labels onto every namespace it onboards,
`openstack` included, which hosts every OpenStack service workload that restricted would
start rejecting. `external` drops those verbs from the ClusterRole and widens the chart's
ValidatingAdmissionPolicy to deny the provisioner any `Namespace` update. Tenant RBAC
onboarding is unaffected.

**Accepted gap.** Nothing in the repository stamps the Pod Security labels instead, so
an onboarded namespace admits pods at whatever the cluster default is. For a namespace
holding a dedicated OpenBao instance, the seal key and the raft volume are then in reach
of any pod that namespace admits.

Every namespace hosting an `OpenBaoCluster` needs an `OpenBaoTenant`. Without one the
controller waits for the `openbao-operator-tenant-rolebinding` RoleBinding that tenant
onboarding creates and pauses every reconcile in that namespace. It logs that pause at
`V(1)`, so the visible symptom is an `OpenBaoCluster` that keeps an empty status and
never gets a `StatefulSet`. The c5c3 operator creates a tenant for each service
namespace it projects into, and the kind overlay carries a static one for the proving
instance (see [OpenBao Proving Instance](#openbao-proving-instance)).

**Accepted risk (decided 2026-08-05):** openbao-operator is a young, single-maintainer,
pre-1.0 (v0.4.x) project, and unlike garage-operator it sits in the production data path
of every future Barbican secret store. Three things bound the exposure. The consumed
surface is a single `OpenBaoCluster` CR shape. A Barbican secret store can also attach to
the shared management OpenBao, where the bootstrap scripts provision the same mount,
policy, and AppRole role without this operator taking part (see the
[OpenBao bootstrap reference](./openbao-bootstrap.md)). And an in-repo StatefulSet
projection derived from the openbao Helm chart stays available as a fallback. Accepted
for the Barbican onboarding.

**Why the chart is pinned by digest.** The chart carries no cosign signature Flux can
verify, so no `spec.verify` policy can gate it, and the operator holds cluster-wide RBAC
over the namespace that stores the instance seal key. Two weaker pins were rejected. The
`>=x <1.0.0` range garage-operator uses would install every future 0.x release of an
individual-owned GHCR namespace within the 30-minute reconcile interval. An exact chart
version on a HelmRepository is not enough either: `0.4.2` is a mutable OCI tag, Flux
tracks the resolved artifact digest rather than the tag string, and re-pushing that tag
with different bytes would upgrade the release on its own — no Git change, no reviewer.
Pinning `spec.ref.digest` on the OCIRepository is what makes every change to what runs
here a reviewed commit.

Renovate keeps that pin from becoming a freeze. Its native Flux manager reads
`deploy/flux-system/`, but a packageRule switches it off for
`sources/openbao-operator.yaml`: the pin is tracked by an explicit `customManagers` entry
in `renovate.json` that rewrites the tag and the digest in the same pull request. The
entry never automerges: every bump of this operator is a human decision.

### OpenBao

**File:** `deploy/flux-system/releases/openbao.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `shared-services` |
| Chart | `openbao` |
| Version constraint | `>=0.5.0 <1.0.0` |
| Source | `openbao` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace |

OpenBao is deployed as a 3-replica HA Raft cluster with **mutual TLS (mTLS)
enforced on the API listener**. The injector is disabled. The server
TLS certificate is sourced from a cert-manager-provisioned Secret
(`openbao-tls`), and two additional cert-manager-provisioned client
certificates (`openbao-client-tls`, `eso-openbao-client-tls`) are required so
that the OpenBao pods themselves (Raft `retry_join` + in-pod `bao` exec
wrappers) and the External Secrets Operator can complete the TLS handshake.
The listener carries `tls_client_ca_file = "/openbao/tls/ca.crt"` and
`tls_require_and_verify_client_cert = true`, so every connection on `:8200` —
whether from a Raft peer, the in-pod bootstrap script, or
`ClusterSecretStore/openbao-cluster-store` — must present a client certificate
that chains to the same self-signed CA bundle as the server cert; the
Kubernetes-token auth method (`auth.kubernetes`) is unchanged and runs
*after* the transport-layer admission gate.

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `global.tlsDisable` | `false` | Enable TLS globally |
| `server.authDelegator.enabled` | `true` | Enable ClusterRoleBinding for TokenReview API (ESO auth) |
| `server.ha.enabled` | `true` | Enable HA mode |
| `server.ha.replicas` | `3` | 3-node Raft cluster |
| `server.ha.raft.enabled` | `true` | Use Raft storage backend |
| `server.ha.raft.config` listener `tls_client_ca_file` | `/openbao/tls/ca.crt` | CA the listener uses to verify presented client certs (same bundle as server cert) |
| `server.ha.raft.config` listener `tls_require_and_verify_client_cert` | `true` | Reject any TLS handshake without a valid client cert before app-layer auth runs |
| `server.ha.raft.config` `retry_join.leader_client_cert_file` × 3 | `/openbao/client-tls/tls.crt` | Client cert each Raft peer presents on `retry_join` to every other peer (same value in all three stanzas) |
| `server.ha.raft.config` `retry_join.leader_client_key_file` × 3 | `/openbao/client-tls/tls.key` | Matching private key for `leader_client_cert_file` |
| `server.volumes` / `server.volumeMounts` — `client-tls` | Secret `openbao-client-tls` → `/openbao/client-tls` (`readOnly: true`) | Mounts the in-pod client keypair distinct from the server cert at `/openbao/tls` so server and client lifecycles do not collide |
| `server.dataStorage.size` | `10Gi` | Persistent volume size |
| `injector.enabled` | `false` | Disable the Vault/Bao agent injector |

**Client certificates.** The two client `Certificate` resources are
declared in `deploy/flux-system/infrastructure/openbao-client-tls-cert.yaml`
and registered in `deploy/flux-system/infrastructure/kustomization.yaml`
immediately after `openbao-tls-cert.yaml`, so cert-manager reconciles them
*before* the OpenBao StatefulSet and `ClusterSecretStore` consume them
(first-apply ordering, see also "Apply ordering" notes below):

| Certificate | Secret (namespace) | Consumer | Reference |
| --- | --- | --- | --- |
| `openbao-client-tls` | `openbao-client-tls` (`shared-services`) | OpenBao pods — Raft `retry_join` + in-pod `bao` exec | StatefulSet volume `client-tls` mounted at `/openbao/client-tls`; env vars `VAULT_CLIENT_CERT` / `VAULT_CLIENT_KEY` in every exec wrapper (`deploy/openbao/bootstrap/common.sh`, `init-unseal.sh`, `hack/deploy-infra.sh`) |
| `eso-openbao-client-tls` | `eso-openbao-client-tls` (`shared-services`) | ESO `ClusterSecretStore/openbao-cluster-store` | `spec.provider.vault.tls.certSecretRef` / `keySecretRef` in `deploy/eso/clustersecretstore.yaml`; `auth.kubernetes` block (mountPath `kubernetes/management`, role `eso-management`) is unchanged — mTLS is purely transport-layer |

Both client certs are issued from the same `openbao-ca-issuer` as
`openbao-tls` (a CA-type ClusterIssuer defined in
`deploy/flux-system/infrastructure/openbao-ca-issuer.yaml` and itself
bootstrapped by `selfsigned-cluster-issuer`). Sharing one CA is what makes the
listener's `tls_client_ca_file = /openbao/tls/ca.crt` validate every presented
client cert — a SelfSigned issuer would mint each Certificate as its own root
and leave the chains unrelated. Both client certs carry
`usages: ["client auth"]`, with the same `duration` / `renewBefore` as
`openbao-tls` so server and client rotation cadences stay aligned. See
[OpenBao Bootstrap Procedure — TLS Configuration](./openbao-bootstrap.md#tls-configuration)
for the full SAN/usages table, the `VAULT_CLIENT_CERT` / `VAULT_CLIENT_KEY`
operator interface, and the runnable mTLS-enforcement probe.

### Keystone Operator

**File:** `deploy/flux-system/releases/keystone-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `keystone-system` (controller); operator-managed Keystone workload remains in `openstack` |
| Chart | `keystone-operator` |
| Version constraint | `>=0.8.0 <1.0.0` (floor = first chart whose values schema accepts `image.digest`) |
| Source | `c5c3-charts` HelmRepository (shared OCI registry) |
| Dependencies | `cert-manager`, `mariadb-operator`, `memcached-operator`, `external-secrets` |

The Keystone Operator manages OpenStack Keystone identity service instances. It depends
on four upstream operators: cert-manager for TLS, mariadb-operator for database
provisioning, memcached-operator for caching, and external-secrets for secret management.

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `replicas` | `2` | Run 2 controller replicas for HA |
| `leaderElection.enabled` | `true` | Enable leader election for HA |
| `image.tag` | `latest` | Use latest image until a versioned release publishes a semver tag |
| `image.digest` | _(injected)_ | Optional immutable digest merged in via the `valuesFrom` ConfigMap; absent by default |

The release carries an optional `valuesFrom` reference to a
`keystone-operator-image-digest` ConfigMap (key `values.yaml`) in
`keystone-system`. `hack/refresh-operator-image-digests.sh` (re)applies that
ConfigMap at deploy time on the `WITH_CONTROLPLANE=true` flux path, pinning
the mutable `latest` tag to the digest current at deploy so a freshly merged
operator image actually rolls out. When the ConfigMap is absent — the default
Quick Start and CI paths — the release renders tag-only, exactly as before.
Flux merges `valuesFrom` first and `spec.values` on top per-key, so
`image.tag` and `image.digest` coexist.

### K-ORC (OpenStack Resource Controller)

**File:** `deploy/flux-system/releases/k-orc.yaml`

| Property | Value |
| --- | --- |
| Kind | `Kustomization` (`kustomize.toolkit.fluxcd.io/v1`) |
| Target namespace | `orc-system` (the upstream installer self-namespaces) |
| Source | `k-orc` `GitRepository` (commit `ae545905edc2966600944e391b4e106ac58cba06` on `main`) |
| Path | `./config/default` |
| Image | `quay.io/orc/openstack-resource-controller:commit-ae54590`, pulled by digest |
| Dependencies | None |

K-ORC (the OpenStack Resource Controller) installs the declarative Keystone resource
CRDs — `ApplicationCredential`, `Service`, `Endpoint`, and related kinds — that the
c5c3-operator drives to project a `ControlPlane`'s desired state into Keystone.

K-ORC ships no Helm chart, so it is applied as a Flux `Kustomization` rather than a
HelmRelease. Upstream flattens its installer into `dist/install.yaml` only at release
time, and no release ships the `RoleAssignment` and `Region` kinds the c5c3-operator
owns. The `GitRepository` source is therefore pinned to a `main` commit and vendors
`./config`, and the Kustomization builds `./config/default` (`prune: true`,
`wait: true`). That base references the placeholder image `controller:latest`, so
`spec.images` rewrites it to the per-commit image upstream publishes. The digest is
what resolves the pull, and the `commit-<short sha>` tag next to it is the drift anchor
`hack/ci-deploy-korc.sh` checks against the source commit. A commit bump moves the
source commit, the image tag and digest, and the Go pseudo-version in
`operators/c5c3/go.mod` together. The base declares the `orc-system` Namespace and
namespaces every resource into it, so no `spec.targetNamespace` is set.
The short, stable name `k-orc` (not the upstream `openstack-resource-controller`) keeps
diagnostics and cross-references terse.

The c5c3-operator patches one pod-template annotation per ControlPlane,
`c5c3.io/korc-catalog-epoch-<hash>`, onto `Deployment orc-controller-manager` under
the field manager `cobaltcore-operator` whenever the service catalog registered
through that ControlPlane settles on a new value. Each change rolls the K-ORC pod,
and the new process logs in against the current catalog
([`reconcileKORCCatalogRefresh`](../c5c3/controlplane-reconciler.md#reconcilekorccatalogrefresh)).
The `k-orc` Kustomization sets no such annotation, and kustomize-controller reverts
only the fields a `kubectl` manager owns, so it leaves the key alone.

The upstream installer has no global-cloud-config knob (the previous HelmRelease set
`globalCloudConfig.secretName`). That is not on the credential critical path: K-ORC
authenticates **per resource** via each CR's `CloudCredentialsRef`, resolved in the
CR's own (control-plane) namespace, so the credential chain below materialises a
co-located `k-orc-clouds-yaml` copy there via the per-ControlPlane ExternalSecret the
c5c3-operator creates and owns (`reconcileKORC`). K-ORC therefore needs no global
default `clouds.yaml` mount, so there is no longer an `orc-system` copy — the static
manifest that previously declared it has been removed. The `orc-system` Namespace
itself remains because the K-ORC installer's own resources land there. See
[Admin Credential Chain](#admin-credential-chain) below.

### RabbitMQ Cluster Operator

**File:** `deploy/flux-system/releases/rabbitmq-cluster-operator.yaml`

| Property | Value |
| --- | --- |
| Kind | `Kustomization` (`kustomize.toolkit.fluxcd.io/v1`) |
| Target namespace | `rabbitmq-system` (the upstream base self-namespaces) |
| Source | `rabbitmq-cluster-operator` `GitRepository` (tag `v2.22.5`, commit `17dd297f71de40a722baf69167b8af511072175e`) |
| Path | `./config/installation` |
| Image | `ghcr.io/rabbitmq/cluster-operator`, `newTag: "2.22.5"`, `digest: "sha256:2727b84b835ada97247bbb65ebfa6998168b4e8ee11b0e6cece56ac2c9c4f0fb"` |
| Dependencies | None declared; cert-manager is an implicit one |

The RabbitMQ Cluster Operator serves the `rabbitmq.com/v1beta1` `RabbitmqCluster`
CRD. The c5c3-operator projects one such CR for every ControlPlane that declares
managed [`spec.infrastructure.messaging`](../c5c3/controlplane-crd.md#messagingspec),
and this operator turns it into the message bus that control plane's services
share.

The base at `./config/installation` installs the `rabbitmq-system` Namespace, the
`rabbitmqclusters.rabbitmq.com` CRD (served version `v1beta1`), the operator's
RBAC, the manager Deployment (requests and limits `200m` / `500Mi`), the two
admission webhooks it has shipped since v2.22.0, and the self-signed `Issuer` plus
the `Certificate`s that back them. cert-manager has been required since v2.20.

**Why a Kustomization and no chart.** Upstream publishes none; the install path it
documents is the kustomize base carried in its own repository. The only OCI chart
that ever wrapped this operator was Bitnami's
`oci://registry-1.docker.io/bitnamicharts/rabbitmq-cluster-operator`, and it is
retired: it stopped at chart 4.4.34 on 2025-08-21 (operator 2.16.1), and the
`docker.io/bitnami/rabbitmq-cluster-operator` images it points at are gone from
Docker Hub.

**Accepted posture.** Upstream is maintained by VMware/Broadcom under the Mozilla
Public License 2.0 and publishes no cosign signature, so `spec.verify` would have
nothing to check. Both halves are pinned by content instead: the Git commit in the
source, the image digest here. The inner kustomization already rewrites the dev
placeholder `rabbitmqoperator/cluster-operator-dev` to
`ghcr.io/rabbitmq/cluster-operator` with the mutable tag `latest`, so the `images`
override keys on that resolved name and is what keeps `latest` out of the cluster.
`newTag` records the tag the digest was resolved from (`crane digest
ghcr.io/rabbitmq/cluster-operator:<tag>`) and doubles as the offline drift anchor,
the same split `releases/k-orc.yaml` uses.

Keying on a **name** is the weak point: kustomize's image transformer silently
no-ops when no resource matches, so if upstream ever re-points the base at a
different image (it already moved once, from the dev placeholder), the override
would apply to nothing, no error would be raised, and the cluster would run the
mutable `latest` tag for a controller holding cluster-wide RBAC over
`RabbitmqCluster`s, StatefulSets, and Secrets. Neither the Renovate tests (which
guard the literal YAML pairing) nor the production-posture test (which guards
byte-identity of this file) render the kustomize output, so the check has to look
at what actually landed: after waiting for the Kustomization, both
`hack/deploy-infra.sh` and `hack/deploy-mgmt-cluster.sh` read every container
image of every Deployment in `rabbitmq-system` and abort the deploy unless each
one carries an `@sha256:` digest.

**Renovate.** Two customManagers track the pair, one on the source
(`github-tags`, capturing `tag` and `commit`) and one on the image (`docker`,
capturing `newTag` and `digest`). Their two packageRules share
`groupName: "rabbitmq cluster-operator"`, so all four values move in one reviewed
PR, with `automerge: false` and `minimumReleaseAge: "3 days"`. Both managers
anchor on the literal YAML shape (the paired lines adjacent, both values
double-quoted), which
`tests/unit/renovate/rabbitmq_cluster_operator_source_custommanager_test.sh` and
`tests/unit/renovate/rabbitmq_cluster_operator_image_custommanager_test.sh` guard.

### c5c3-operator

**File:** `deploy/flux-system/releases/c5c3-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `c5c3-system` |
| Chart | `c5c3-operator` |
| Version constraint | `>=0.7.0 <1.0.0` (floor = first chart whose values schema accepts `image.digest`) |
| Source | `c5c3-charts` HelmRepository (shared OCI registry) |
| Dependencies | `keystone-operator`, `external-secrets`, `mariadb-operator`, `memcached-operator` |

The c5c3-operator runs the `ControlPlane` reconciler that orchestrates a Keystone
control plane end-to-end. It depends on the four operators whose CRs
it projects — `keystone-operator` for the Keystone instance, `external-secrets` and
`mariadb-operator` and `memcached-operator` for the supporting platform services. It
also drives K-ORC's `ApplicationCredential` / `Service` / `Endpoint` CRDs to register
the catalog and rotate the admin credential, but K-ORC is the separate Flux
`Kustomization` above, not a `dependsOn` edge (so K-ORC, like the other CRD providers,
is a hard dependency the manager requires at startup rather than tolerating). The
operator child CRs are created
in the `ControlPlane`'s own namespace, not a hard-coded one. For the reconciliation
contract see the [`ControlPlane` reconciler reference](../c5c3/controlplane-reconciler.md).

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `replicas` | `2` | Run 2 controller replicas for HA |
| `leaderElection.enabled` | `true` | Enable leader election for HA |
| `image.tag` | `latest` | Use latest image until a versioned release publishes a semver tag |
| `image.digest` | _(injected)_ | Optional immutable digest merged in via the `valuesFrom` ConfigMap; absent by default |

The release consumes the same image-digest `valuesFrom` mechanism as the
keystone-operator release above (a `c5c3-operator-image-digest` ConfigMap in
`c5c3-system`); the horizon-operator release is wired identically.

## HelmRelease–HelmRepository Cross-Reference

Each HelmRelease `sourceRef.name` must match a HelmRepository `metadata.name` in
`sources/`. This table shows the mapping. The `openbao-operator` release is the one
exception: it names its source through `spec.chartRef` instead, because that source is an
OCIRepository.

| HelmRelease | `sourceRef.name` | HelmRepository file |
| --- | --- | --- |
| `cert-manager` | `cert-manager` | `sources/cert-manager.yaml` |
| `prometheus-operator-crds` | `prometheus-community` | `sources/prometheus-community.yaml` |
| `mariadb-operator-crds` | `mariadb-operator` | `sources/mariadb-operator.yaml` |
| `mariadb-operator` | `mariadb-operator` | `sources/mariadb-operator.yaml` |
| `external-secrets` | `external-secrets` | `sources/external-secrets.yaml` |
| `memcached-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |
| `openbao` | `openbao` | `sources/openbao.yaml` |
| `garage-operator` | `garage-operator` | `sources/garage-operator.yaml` |
| `openbao-operator` | `openbao-operator` (`chartRef`) | `sources/openbao-operator.yaml` |
| `keystone-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |
| `horizon-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |
| `glance-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |
| `placement-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |
| `c5c3-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |

`k-orc` and `rabbitmq-cluster-operator` are not in this table: each is a Flux
`Kustomization` whose `sourceRef` is a `GitRepository` (`sources/k-orc.yaml` and
`sources/rabbitmq-cluster-operator.yaml`), not a HelmRelease backed by a
HelmRepository.

The kind-only `chaos-mesh` HelmRelease ships in the opt-in overlay at
`deploy/kind/chaos-mesh/release.yaml`, with its own local
`source.yaml`. It is intentionally absent from this always-on table because
production overlays do not install it.

## Infrastructure Custom Resources

Infrastructure CRs are instance-level resources managed by the operators installed via
HelmReleases above. They are separated into their own kustomization
(`infrastructure/kustomization.yaml`) because they depend on CRDs that are only available
after the corresponding operator HelmReleases install their Helm charts.

### Self-Signed ClusterIssuer

**File:** `deploy/flux-system/infrastructure/cluster-issuer.yaml`

| Property | Value |
| --- | --- |
| API version | `cert-manager.io/v1` |
| Kind | `ClusterIssuer` |
| Name | `selfsigned-cluster-issuer` |
| Scope | Cluster-scoped (no namespace) |

The self-signed ClusterIssuer provides a default certificate issuer for development
environments. It requires cert-manager CRDs (`cert-manager.io/v1`) which are installed
by the cert-manager HelmRelease.

### OpenStack DB CA Issuer

**File:** `deploy/flux-system/infrastructure/db-ca-issuer.yaml`

Provisions the dedicated cert-manager CA that anchors the OpenStack database trust
domain. The file declares two resources:

| Resource | API version | Kind | Name | Namespace |
| --- | --- | --- | --- | --- |
| CA keypair Certificate | `cert-manager.io/v1` | `Certificate` | `openstack-db-ca` | `cert-manager` |
| CA ClusterIssuer | `cert-manager.io/v1` | `ClusterIssuer` | `openstack-db-ca-issuer` | Cluster-scoped |

The `selfsigned-cluster-issuer` mints a self-signed CA `Certificate` (`isCA: true`,
3-year lifetime, 30-day `renewBefore`) into the `openstack-db-ca` Secret in the
`cert-manager` namespace — cert-manager's default `--cluster-resource-namespace`,
which is where a CA-type `ClusterIssuer` looks up its `secretName`. The
`openstack-db-ca-issuer` `ClusterIssuer` then signs every leaf certificate inside
the OpenStack DB trust domain:

- MariaDB Galera server TLS material (`spec.tls.serverCertIssuerRef`, see below).
- MaxScale listener TLS material (same issuer via inheritance / explicit
  `serverCertIssuerRef`).
- The Keystone DB-client keypair issued by the keystone-operator's
  `reconcileDatabaseTLS` sub-reconciler — the constant
  [`dbCAIssuerName`](https://github.com/c5c3/cobaltcore/blob/main/operators/keystone/internal/controller/reconcile_databasetls.go)
  hard-codes the same string (`"openstack-db-ca-issuer"`), so a rename here MUST be
  matched in the operator.

**Apply ordering.** This manifest is also applied out-of-band from the infrastructure
kustomization by `hack/deploy-infra.sh` (Phase 2, alongside `cluster-issuer.yaml` and
`openbao-tls-cert.yaml`) so that MariaDB has the issuer available the moment it tries
to render its server certificate. The infrastructure kustomization still references
`db-ca-issuer.yaml` so subsequent `kubectl apply -k` runs are idempotent.

For the end-to-end TLS path the issuer participates in, see the
[Enable Keystone Database TLS](../../guides/keystone/enable-keystone-database-tls.md) how-to.

### OVN CA Issuer

**File:** `deploy/flux-system/infrastructure/ovn-ca-issuer.yaml`

Provisions the cert-manager CA that anchors the OVN database trust domain. The file
declares two resources:

| Resource | API version | Kind | Name | Namespace |
| --- | --- | --- | --- | --- |
| CA keypair Certificate | `cert-manager.io/v1` | `Certificate` | `openstack-ovn-ca` | `cert-manager` |
| CA ClusterIssuer | `cert-manager.io/v1` | `ClusterIssuer` | `openstack-ovn-ca-issuer` | Cluster-scoped |

The bootstrap path matches the DB CA above. `selfsigned-cluster-issuer` mints a
self-signed CA `Certificate` (`isCA: true`, ECDSA P-256, 3-year lifetime, 30-day
`renewBefore`) into the `openstack-ovn-ca` Secret in the `cert-manager` namespace, and
`openstack-ovn-ca-issuer` signs the leaves from it: the Northbound and Southbound
ovsdb-server certificates, plus the client certificates ovn-northd, the relays, and the
chassis agents present to those databases.

OVN keeps a CA of its own instead of sharing `openstack-db-ca-issuer`. OVS/OVN
authenticates a peer with a single check: the peer certificate must chain to the
configured CA. Every leaf this issuer signs is therefore a valid OVSDB client, so it
signs OVN database and client certificates and nothing else.

The `OVNCentral` CR names the ClusterIssuer in `spec.tls.issuerRef`. That field defaults
to `kind: ClusterIssuer` because the chassis agents run outside the OVNCentral's
namespace, which a namespaced `Issuer` cannot reach. An issuer that exposes no `ca.crt`
is rejected by the ovn-operator's TLS sub-reconciler.

### MariaDB Galera Cluster

**File:** `deploy/flux-system/infrastructure/mariadb.yaml`

| Property | Value |
| --- | --- |
| API version | `k8s.mariadb.com/v1alpha1` |
| Kind | `MariaDB` |
| Name | `openstack-db` |
| Namespace | `openstack` |
| Replicas | `3` |
| Galera | Enabled (`spec.galera.enabled: true`) |
| MaxScale | Enabled, 2 replicas (`spec.maxScale.enabled: true`, `spec.maxScale.replicas: 2`) |
| Storage | `100Gi`, storage class `ceph-rbd` |

The MariaDB CR provisions a 3-node Galera cluster with synchronous replication managed
by the mariadb-operator. MaxScale is enabled with 2 replicas to provide intelligent query
routing and read/write splitting across the Galera nodes.

The root password is sourced from a Kubernetes Secret (`mariadb-root-password`, key
`password`). The production stack ships **no** `ExternalSecret` for it — a non-kind Flux
MariaDB baseline is expected to provide the `mariadb-root-password` Secret itself. On
kind, a kind-only overlay shim
(`deploy/kind/infrastructure/mariadb-root-password-externalsecret.yaml`) materialises it
from the OpenBao path `infrastructure/mariadb` so the single-node Quick Start stays
self-contained.

> **Non-Goal — operator-owned root credential.** Unlike the Keystone admin password
> (which the c5c3-operator now projects per ControlPlane as a dedicated
> `ExternalSecret`), the MariaDB **root** password is deliberately **not**
> operator-owned. Provisioning it is left to the MariaDB baseline — the kind shim above,
> or a production Flux baseline — keeping the operator off the database superuser
> credential path.

**Services:**

| Service | Type | Purpose |
| --- | --- | --- |
| Primary | `ClusterIP` | Read-write endpoint for application connections |
| Secondary | `ClusterIP` | Read-only endpoint for read replicas |

**Monitoring:** Prometheus metrics are enabled (`spec.metrics.enabled: true`).

**TLS.** Galera inter-node replication, the MaxScale client
listener, and every Keystone-to-database connection all sit inside the OpenStack DB
trust domain rooted at the `openstack-db-ca-issuer` ClusterIssuer documented above.
The MariaDB CR enables TLS in `spec.tls` and the MaxScale sub-spec inherits it:

| Field | Value | Purpose |
| --- | --- | --- |
| `spec.tls.enabled` | `true` | Turn on TLS for the MariaDB cluster |
| `spec.tls.required` | `true` | Reject any non-TLS connection at the transport layer (verified by the chainsaw plaintext-rejection probe in `tests/e2e/keystone/database-tls/chainsaw-test.yaml`) |
| `spec.tls.serverCertIssuerRef` | `openstack-db-ca-issuer` (ClusterIssuer, `cert-manager.io`) | Issue server certs for Galera + MaxScale from the shared DB CA |
| `spec.tls.clientCertIssuerRef` | `openstack-db-ca-issuer` (ClusterIssuer, `cert-manager.io`) | Trust client certs minted by the same DB CA (the Keystone operator issues its DB-client keypair from this issuer; see [reconcile_databasetls.go](https://github.com/c5c3/cobaltcore/blob/main/operators/keystone/internal/controller/reconcile_databasetls.go)) |
| `spec.maxScale.tls.enabled` | `true` | MaxScale terminates TLS on its client listener (proxy-side); explicit block documents intent even where the proxy would otherwise inherit `spec.tls` |

The rendered YAML in `deploy/flux-system/infrastructure/mariadb.yaml` is:

```yaml
spec:
  tls:
    enabled: true
    required: true
    serverCertIssuerRef:
      name: openstack-db-ca-issuer
      kind: ClusterIssuer
      group: cert-manager.io
    clientCertIssuerRef:
      name: openstack-db-ca-issuer
      kind: ClusterIssuer
      group: cert-manager.io
  maxScale:
    enabled: true
    replicas: 2
    tls:
      enabled: true
```

The mariadb-operator (v0.30+) auto-derives the server and client CA bundles from the
referenced issuer, so explicit `serverCASecretRef` / `clientCASecretRef` entries are
intentionally omitted — see the inline `DECISION` comment in `mariadb.yaml` for the
trade-off against the cross-namespace `*CASecretRef` form. End-to-end verification
that the live connection is encrypted lives in
[`tests/e2e/keystone/database-tls/chainsaw-test.yaml`](https://github.com/c5c3/cobaltcore/blob/main/tests/e2e/keystone/database-tls/chainsaw-test.yaml)
(asserts `SHOW STATUS LIKE 'Ssl_cipher'` reports a non-empty cipher).

To turn the path on for a `Keystone` CR, follow the
[Enable Keystone Database TLS](../../guides/keystone/enable-keystone-database-tls.md) guide.

### Memcached Cluster

**File:** `deploy/flux-system/infrastructure/memcached.yaml`

| Property | Value |
| --- | --- |
| API version | `memcached.c5c3.io/v1beta1` |
| Kind | `Memcached` |
| Name | `openstack-memcached` |
| Namespace | `openstack` |
| Replicas | `3` |
| Image | `memcached:1.6` |

The Memcached CR provisions a 3-replica Memcached cluster for OpenStack session and
token caching. The memcached-operator manages pod lifecycle and provides stable DNS-based
service discovery for operator consumers.

**API group:** The API group is `memcached.c5c3.io`, matching the CRD definition
shipped by the [memcached-operator](https://github.com/C5C3/memcached-operator) Helm chart.

### Garage Object Store

**File:** `deploy/flux-system/infrastructure/garage.yaml`

Four instance CRs (API group `garage.rajsingh.info`) declare the S3 object store the
[garage-operator](#garage-operator) manages. All live in the `shared-services` namespace:

| Kind | API version | Name | Purpose |
| --- | --- | --- | --- |
| `GarageCluster` | `garage.rajsingh.info/v1beta2` | `garage` | Storage tier StatefulSet, config, and cluster layout — declarative, no manual `garage layout assign/apply` |
| `GarageBucket` | `garage.rajsingh.info/v1beta1` | `glance-images` | Pre-created bucket with an explicit `globalAlias` (deterministic S3 name) |
| `GarageBucket` | `garage.rajsingh.info/v1beta1` | `glance-images-2` | Second, independent bucket for the Glance S3 multi-store e2e suite (same explicit-`globalAlias` rationale) |
| `GarageKey` | `garage.rajsingh.info/v1beta1` | `glance-s3` | Imported S3 credentials with read/write on both the `glance-images` and `glance-images-2` buckets |

`GarageCluster` is written against the `v1beta2` storage version with
`replication.factor: 1`, a single storage tier, `network.service.type: ClusterIP`, and
the S3 API on `:3900` with SigV4 region `garage` (path-style addressing — virtual-host
style would need wildcard DNS). No `spec.image` is set, so the Garage version rides the
operator/chart releases. The kind overlay
(`deploy/kind/infrastructure/kustomization.yaml`) patches the storage tier to a single
node with small PVCs on the `standard` storage class, mirroring the MariaDB/Memcached
single-node kind footprint. This is a **CI/dev fixture** — plain HTTP in-cluster, single
tier; production-grade multi-node/zone-aware guidance is out of scope.

**Credential flow — OpenBao stays the single source of truth.** No key material is read
back from Garage, and no Secret is copied from one namespace to another: each consuming
namespace gets its own ExternalSecret on the same OpenBao path.

1. `write-bootstrap-secrets.sh` seeds an admin token at
   `bootstrap/openstack/garage/admin-token` and a `GK`-prefixed S3 access/secret pair at
   `bootstrap/openstack/garage/s3-credentials` (see the
   [OpenBao bootstrap reference](./openbao-bootstrap.md#write-bootstrap-secrets-sh)). The
   `openstack` segment names the consuming tenant, not the namespace Garage runs in.
2. Kind-only ExternalSecrets
   (`deploy/kind/infrastructure/garage-{admin-token,s3-credentials}-externalsecret.yaml`)
   materialize them through the shared `openbao-cluster-store`: `garage-admin-token` and
   one `garage-s3-credentials` copy into `shared-services`, beside the CRs that read them,
   and a second `garage-s3-credentials` copy into `openstack`.
3. The `GarageCluster` reads the admin token via `spec.admin.adminTokenSecretRef`; the
   `GarageKey` **imports** the pre-existing S3 pair via `spec.importKey.secretRef` (rather
   than the operator minting a fresh key), so the key material never diverges from
   OpenBao. A `GlanceBackend` resolves its `credentialsSecretRef` in the Glance service's
   own namespace, which is what the `openstack` copy serves.

### OpenBao Proving Instance

**File:** `deploy/kind/infrastructure/openbao-instance.yaml`

Five resources declare the proving instance: the single-replica `OpenBaoCluster` that the
[openbao-operator](#openbao-operator) manages, plus the TLS and RBAC objects it depends
on. All live in the `openstack` namespace except the cluster-scoped ClusterRoleBinding:

| Kind | API version | Name | Purpose |
| --- | --- | --- | --- |
| `Certificate` | `cert-manager.io/v1` | `openbao-instance-tls-server` | Server certificate for the API listener, carrying the SAN `openbao-cluster-openbao-instance.local` that the operator's External-mode validation requires |
| `Certificate` | `cert-manager.io/v1` | `openbao-instance-tls-ca` | Delivers the trust-domain CA into the fixed-name Secret the operator reads (data key `ca.crt`) |
| `ServiceAccount` | `v1` | `openbao-instance-provisioner` | Client identity the Kubernetes-auth role `provisioner` binds to |
| `ClusterRoleBinding` | `rbac.authorization.k8s.io/v1` | `openbao-instance-auth-delegator` | Grants `system:auth-delegator` to the operator-created instance ServiceAccount `openbao-instance-serviceaccount` |
| `OpenBaoCluster` | `openbao.org/v1alpha1` | `openbao-instance` | Profile `Development`, version `2.6.2`, one replica, 1Gi raft storage, TLS mode `External`, static seal, self-init enabled, applied `paused`, API-server egress patched in at deploy time |

**Tenant.** `deploy/kind/infrastructure/openbao-tenant.yaml` declares the
`OpenBaoTenant` `openstack` that admits the `openstack` namespace to the multi-tenant
operator. It lives in `openbao-operator-system` and targets `openstack` from there,
because the chart's admission policy accepts `spec.quota` and `spec.limitRange` only on
a tenant in the operator namespace. Without those overrides the provisioner writes its
default LimitRange into `openstack`, which gives every container without a CPU limit a
500m one. The overrides are the provisioner defaults of chart 0.4.2 minus the CPU
limits:

| Object | Setting | Value |
| --- | --- | --- |
| ResourceQuota | `pods` | `50` |
| ResourceQuota | `requests.cpu` | `20` |
| ResourceQuota | `requests.memory` | `64Gi` |
| ResourceQuota | `limits.memory` | `128Gi` |
| LimitRange (`Container`) | `default.memory` | `512Mi` |
| LimitRange (`Container`) | `defaultRequest.cpu` | `100m` |
| LimitRange (`Container`) | `defaultRequest.memory` | `128Mi` |

The upstream default `default.cpu: 500m` and `limits.cpu: 40` are left out: the service
operators set no CPU limit, and a `limits.cpu` quota rejects every pod that sets none.
An override replaces the whole default object, so every kept key is spelled out.
`hack/deploy-infra.sh` waits for `status.provisioned` on the tenant in
`openbao-operator-system` before it waits for the instance. A kind cluster created
before the tenant moved keeps its old `openstack/openstack` tenant beside the new one,
so recreate it.

The instance runs in every kind deploy, so the primitives a managed Barbican secret store
needs are exercised with no Barbican attached: static-seal auto-unseal, cert-manager TLS
in External mode, declarative self-init, and the AppRole and Kubernetes-auth methods a
service and its operator log in with.

**Why it is kind-only.** Everything above is a proving posture, not a production one. The
`Development` profile is what keeps the instance on a single node — one replica, so no
PodDisruptionBudget and no anti-affinity — and the static seal keeps its key material in
a Secret in the shared `openstack` namespace. `Hardened`, the profile that forces an
external-KMS unseal and three replicas, is the production control, and this instance
deliberately opts out of it to exercise the static-seal path. Shipping that from
`deploy/flux-system/infrastructure/` would put it, a cluster-scoped RBAC binding, and a
self-initialized AppRole with no consumer into every production deployment. Only the
[openbao-operator](#openbao-operator) itself ships in the production base. The dedicated
instance a ControlPlane projects for a managed Barbican secret store carries the same
proving-grade `Development` profile; a `Hardened` production-shaped instance is still
future work.
[`tests/unit/deploy/openbao_instance_overlay_test.sh`](https://github.com/c5c3/cobaltcore/blob/main/tests/unit/deploy/openbao_instance_overlay_test.sh)
asserts both directions of that split.

**Two Certificates, one trust domain.** The operator's External TLS contract reads two
fixed-name Secrets, `<cluster>-tls-server` and `<cluster>-tls-ca`. cert-manager has no
primitive that materializes a CA-only Secret in another namespace, so
`openbao-instance-tls-ca` is a minimal Certificate that exists for one reason: every
Secret issued by a CA-type ClusterIssuer carries the trust-domain CA in `ca.crt`. Its
leaf keypair goes unused. Both Certificates are issued by `openbao-ca-issuer`, the same
root as the management OpenBao's server and client certificates, so the server
certificate chains directly to the CA the operator hands the instance. The operator
mounts both Secrets and waits for them, but never rotates them; cert-manager owns the
renewal.

Because `openbao-ca-issuer` is also what the management OpenBao trusts as
`tls_client_ca_file` behind `tls_require_and_verify_client_cert`, both Certificates pin
`usages` to `digital signature`, `key encipherment`, and `server auth`. Without the pin
cert-manager emits no EKU at all, and a certificate without an EKU passes client-auth
verification — these Secrets live in the shared workload namespace, so they would be
ready-made client identities for that mTLS gate. For the same reason the server
certificate carries no loopback IP SAN: such a SAN authenticates whatever listens on
localhost rather than this instance. In-pod clients connect over the loopback address and
verify against the operator's own SAN by passing `VAULT_TLS_SERVER_NAME`.

**TokenReview authority.** OpenBao validates a Kubernetes-auth login by issuing a
TokenReview under the instance pod's own projected ServiceAccount token. The operator
creates that ServiceAccount (`<cluster>-serviceaccount`), and neither the operator nor
its chart grants it TokenReview, so `openbao-instance-auth-delegator` supplies the
binding. Without it every login fails with 403 permission denied. The binding is
overlay-owned while its subject is created and garbage-collected by the operator with the
CR, so deleting the CR leaves it dangling — the operator exposes no way to point the
instance at a ServiceAccount whose lifecycle the overlay controls, which is one more
reason the instance stays out of the production overlay.

**Unseal-key custody.** The management OpenBao is the root of trust for the static seal:

1. `write-bootstrap-secrets.sh` seeds a generated key at
   `bootstrap/openstack/openbao-instance/unseal-key` (key `key`), behind the
   `write_secret_if_missing` guard.
2. The ExternalSecret
   `deploy/kind/infrastructure/openbao-instance-unseal-key-externalsecret.yaml` reads that
   path through the shared `openbao-cluster-store` and materializes the Secret
   `openbao-instance-unseal-key` in `openstack`. It is `refreshPolicy: CreatedOnce`: the
   Secret is materialized once and never converged again. Its target is
   `creationPolicy: Orphan`, which leaves the Secret's single controller `ownerReference`
   slot free for step 4.
3. The CR is applied with `spec.paused: true`. The operator blind-creates the same
   fixed-name Secret with a random key on its first reconcile and adopts a pre-existing
   one only against an ownership proof, so it must not run first.
4. `hack/deploy-infra.sh` syncs the ExternalSecret, patches a controller `ownerReference`
   to the CR onto the materialized Secret, un-pauses the CR, and later waits for condition
   `Available`. The operator accepts either that reference or its
   `openbao.org/owner-uid` annotation as proof, but the
   `openbao-lock-managed-resource-mutations` ValidatingAdmissionPolicy the operator chart
   ships reserves the annotation for the operator's own ServiceAccounts and denies every
   other writer, so the reference is the only proof the deploy can attach.
5. The operator adopts the Secret and mounts the key at `/etc/bao/unseal`, where the
   static seal reads it on every start. Nothing ever sends an unseal command.

The key is never rotated, and both ends enforce that rather than assume it: a static seal
whose key material changes seals the instance permanently, with no path back to the
stored data. `write_secret_if_missing` keeps the seed side from re-generating it, and
`refreshPolicy: CreatedOnce` keeps the consuming side from re-materializing a changed
seed — the case that arises when the management OpenBao loses its raft PVC and is
re-seeded while the instance's own PVC survives. No PushSecret targets the path.

> **Accepted risk (openbao-operator upstream).** The operator's adoption guard is metadata
> carrying the CR's UID, and that UID is readable by any principal with
> `get openbaocluster`. It is an ownership marker, not an ownership proof: anyone able to
> create Secrets in the namespace before the instance first initializes can plant a seal
> key of their choosing. Accepted because this instance is CI/dev-only; a production
> instance must not depend on that guard.

**API-server egress.** The operator wraps the instance pods in a deny-by-default
NetworkPolicy and derives the API-server egress rule from the in-cluster service VIP on
port 443. kindnet enforces egress against the post-DNAT destination from kind 0.32
onwards, and the packet it inspects is addressed to the API server's own endpoint on port
6443, so the VIP rule never matches. The instance then loses the API server: raft
auto-join times out, self-init cannot complete, and the partial raft state wedges every
later initialization attempt, recoverable only by deleting the CR together with its PVC.

`spec.network.apiServerEndpointIPs` closes that gap with one egress rule per address on
port 6443. The manifest sets no value, because a kind node address does not survive a
cluster re-creation. `hack/deploy-infra.sh` reads the addresses from the EndpointSlice
`kubernetes` in `default`, which kube-apiserver maintains itself, and applies them in the
same patch that un-pauses the CR, so the operator's first reconcile already renders the
rules. Resolving no address aborts the deploy. The operator reports its own verdict on the
result as condition `APIServerNetworkReady`, which stays `Unknown` with reason
`APIServerEndpointIPsRecommended` while only the service VIP is allowed.

The operator renders the rule for `apiServerEndpointIPs` on port 6443 and no other.
The same patch therefore also sets `spec.network.egressRules`: one rule per address, a
`/32` block (`/128` for IPv6) on TCP at the port the EndpointSlice publishes. On kind that
is 6443 and the rule duplicates the operator's own; behind Gardener's apiserver-proxy on
the [metal-stack lab](#lab-overlay) it is 443, where the addresses alone allow nothing. A
slice that publishes no port aborts the deploy like one without an address.

**Self-init surface.** The operator renders `spec.selfInit.requests` into OpenBao's
initialize stanzas, which run once against freshly initialized storage; OpenBao revokes
the root token afterwards. The list below is therefore the instance's complete permanent
configuration:

| Requests | Paths | Result |
| --- | --- | --- |
| `barbican_kv` | `sys/mounts/barbican` | KV v2 mount `barbican/` |
| `barbican_secretstore_policy` | `sys/policies/acl/barbican-secretstore` | Policy `barbican-secretstore`: create/read/update/delete/list on `barbican/data/*`, and the same minus `delete` on `barbican/metadata/*` |
| `approle_auth`, `barbican_approle_role` | `sys/auth/approle`, `auth/approle/role/barbican` | AppRole role `barbican` with `token_policies=barbican-secretstore`, `token_ttl=1h`, `token_max_ttl=4h`, `secret_id_ttl=720h` |
| `kubernetes_auth`, `kubernetes_auth_config` | `sys/auth/kubernetes`, `auth/kubernetes/config` | Kubernetes auth mount, `kubernetes_host=https://kubernetes.default.svc` |
| `provisioner_policy`, `provisioner_k8s_role` | `sys/policies/acl/provisioner`, `auth/kubernetes/role/provisioner` | Policy `provisioner` (read the barbican role ID, create the secret ID, the same `barbican/` data and metadata grants as above, read/update on `sys/mounts/barbican`) and the Kubernetes role bound to the `openbao-instance-provisioner` ServiceAccount in `openstack`, `audience=openbao-instance` |

No policy grants `delete` on `barbican/metadata/*`. In KV v2 that verb permanently
destroys every version of a secret and its metadata, which is the only in-store recovery
mechanism tenant key material has; castellan's Vault key manager deletes through
`barbican/data/*`, where the delete is a recoverable soft delete. `secret_id_ttl` is 30
days rather than a year for a related reason: an AppRole secret ID carries no use count
and no CIDR bound, so its TTL is the only thing that bounds a leaked one.

The Kubernetes-auth role is audience-bound. Without `audience`, the role accepts any
default-audience token of that ServiceAccount, including one auto-mounted into an
unrelated pod; bound to `openbao-instance`, only a token minted deliberately for this
instance logs in (`kubectl create token --audience=openbao-instance`). The ServiceAccount
itself sets `automountServiceAccountToken: false`, since every consumer mints explicitly.

> **Self-init is one-shot.** Changing the request list on a running instance changes
> nothing. The stanzas only run against freshly initialized storage, so a different
> configuration means recreating the instance: delete the CR **and** its PVC. Runtime
> work (minting AppRole secret IDs, writing secrets) goes through the `provisioner` role
> instead. Because that failure is silent — the CR reconciles, `Available` stays `True`,
> and the new request is simply never applied —
> [`tests/unit/deploy/openbao_instance_overlay_test.sh`](https://github.com/c5c3/cobaltcore/blob/main/tests/unit/deploy/openbao_instance_overlay_test.sh)
> pins the request names, so an edit cannot land without confronting it.

**Shared names with the brownfield leg.** The mount, policy, and role names (`barbican/`,
`barbican-secretstore`, `barbican`) match the ones the bootstrap scripts provision on the
shared management OpenBao (`enable_barbican_kv` in `setup-secret-engines.sh`,
`deploy/openbao/policies/barbican-secretstore.hcl`, and the `barbican` AppRole role in
`setup-auth.sh`; see the
[OpenBao bootstrap reference](./openbao-bootstrap.md#setup-secret-engines-sh)). A
deployment that attaches Barbican to the shared instance instead of a dedicated one
therefore differs only in which instance it points at.

[`tests/e2e/infrastructure/openbao-instance/chainsaw-test.yaml`](https://github.com/c5c3/cobaltcore/blob/main/tests/e2e/infrastructure/openbao-instance/chainsaw-test.yaml)
locks the whole path: the operator Deployment and its HelmRelease, the unseal-key
ExternalSecret reporting `SecretSynced` **and** the instance's `ownerReference` adoption of
the Secret it materialized, the CR reporting `Available`, a Kubernetes-auth login that reads
the AppRole role ID and mints a secret ID, a rejected login with a wrong secret ID, and a
KV v2 round-trip on `barbican/`. The last step triggers a rolling restart through
`spec.runtime.restartAt` on the CR and waits for the replacement pod to come back unsealed.
It does not delete the pod directly: the operator ships a `ValidatingAdmissionPolicy` that
denies any mutation of the resources it manages, so a restart has to go through the parent
CR. The suite issues no unseal command anywhere, which is what makes that step a proof of
the static seal.

### Admin Credential Chain

The c5c3-operator mints a single restricted admin Application Credential per cluster and
mirrors it to OpenBao, from where the External Secrets Operator materialises it as the
`clouds.yaml` Secret that K-ORC authenticates with. The chain materialises the
Kubernetes Secret `k-orc-clouds-yaml` via a single `ExternalSecret`, created per
ControlPlane by the operator:

| Namespace | Source | Purpose |
| --- | --- | --- |
| `openstack` (control-plane) | **operator-created per-CR** (`reconcileKORC` → `ensureKORCCloudsYAMLExternalSecret`) | **C1 co-location** — the c5c3-operator creates the K-ORC `ApplicationCredential`/`Service`/`Endpoint` CRs in the control-plane namespace, and K-ORC resolves each CR's `CloudCredentialsRef` Secret in that *same* namespace, so the admin clouds.yaml must live here for K-ORC to authenticate. This is the copy the `AdminCredentialReady` gate waits on. |

**Control-plane copy (operator-created per ControlPlane)** — the
control-plane-namespace `k-orc-clouds-yaml` ExternalSecret is **not** a static
manifest: the c5c3-operator creates and owns one **per ControlPlane**
(`reconcileKORC` → `ensureKORCCloudsYAMLExternalSecret`), owner-ref'd to
the CR for GC and created in the ControlPlane's child namespace. It is named after
`spec.korc.adminCredential.cloudCredentialsRef.secretName` (default
`k-orc-clouds-yaml`) and reads the per-CR OpenBao key
`openstack/keystone/{namespace}/{name}/admin/app-credential` (property
`clouds.yaml`, store-relative to the KV-v2 mount) via the `openbao-cluster-store`
`ClusterSecretStore`, with `creationPolicy: Owner` and `refreshInterval: 1h`.
Because both the ExternalSecret name and the OpenBao key are derived per-CR, an
arbitrarily named ControlPlane resolves to the correct key with **no manifest
edit** — the operator now resolves what was previously deferred for this ExternalSecret.

**Optional `cacert` entry (External keystone mode)** — when the ControlPlane sets
`spec.services.keystone.external.caBundleSecretRef`, the ExternalSecret carries a
**second** data entry that reads the `cacert` property back from the same
per-CR OpenBao key. K-ORC reads that inline PEM key natively from the credentials
Secret, so the materialised `k-orc-clouds-yaml` ends up carrying the private-CA
trust anchor next to `clouds.yaml`. No extra push plumbing is needed: the
PushSecret mirrors the source Secret **whole** (it declares no `match.secretKey`),
so the `cacert` key the operator projects into the app-credential Secret already
reaches OpenBao alongside `clouds.yaml`. Clearing the ref drops the read-back
entry on the next reconcile; the now-orphaned `cacert` property lingers at the
OpenBao key because the PushSecret's `deletionPolicy` is `None`, but nothing reads
it.

**No `orc-system` copy** — the static `deploy/eso/externalsecrets/k-orc-clouds-yaml.yaml`
manifest that previously declared K-ORC's global default `clouds.yaml` mount has been
removed. K-ORC authenticates **per resource** via each CR's `CloudCredentialsRef`,
resolved in the control-plane namespace, so no cluster-global default mount is needed.
The `orc-system` Namespace itself is retained (co-declared in
`deploy/flux-system/namespaces.yaml`) because the K-ORC installer's own resources land
there — it no longer hosts a `clouds.yaml` copy.

On a fresh cluster the bootstrap `clouds.yaml` at the per-CR OpenBao key is seeded
by the **operator** (`reconcileKORC` → `seedBootstrapCloudsYAML`, write-if-empty):
it writes a password-based bootstrap `clouds.yaml` into the admin
Application Credential Secret, and the operator's PushSecret mirrors it to OpenBao
so the ExternalSecrets can materialise before any credential is minted. Once the
c5c3-operator mints the admin Application Credential the same PushSecret overwrites
the key with the App-Cred-based `clouds.yaml`.

**OpenBao policy** — `deploy/openbao/policies/eso-tenant.hcl`

This per-tenant policy grants the write path for each ControlPlane's admin
credential PushSecret. Because the admin credential path is keyed per
ControlPlane (`openstack/keystone/{namespace}/{name}/admin/app-credential`), the
grant templates the namespace to the caller's OWN `service_account_namespace`
(`{ns}` below) and matches the ControlPlane name with a single `+` segment. The
policy is bound to the `eso-tenant` auth role and reached through the namespaced
`openbao-tenant-store` SecretStore, so a tenant's PushSecret can only write its
own namespace's leaves:

| Path | Capabilities | Purpose |
| --- | --- | --- |
| `kv-v2/data/openstack/keystone/{ns}/+/admin/app-credential` | `create`, `update`, `read`, `delete` | Write (and soft-delete on teardown) each ControlPlane's admin Application Credential `clouds.yaml` data leaf (`{ns}` is templated to the caller's namespace; the `+` segment is the ControlPlane name) |
| `kv-v2/metadata/openstack/keystone/{ns}/+/admin/app-credential` | `create`, `update`, `read` | Allow ESO's Vault provider to write `custom_metadata` on the KV-v2 PushSecret (a data-only grant 403s on the metadata PUT and the PushSecret never reaches Ready) |
| `kv-v2/data/openstack/keystone/{ns}/+/service-accounts/+` | `create`, `update`, `read`, `delete` | Write (and soft-delete on teardown) each declared service account's password `clouds.yaml` data leaf; the `+` segments are the ControlPlane's name and the account name (each a DNS-1123 label, so a single `+` leaf suffices) |
| `kv-v2/metadata/openstack/keystone/{ns}/+/service-accounts/+` | `create`, `update`, `read` | Allow ESO's Vault provider to write `custom_metadata` on the service-account KV-v2 PushSecret |

`{ns}` is substituted by OpenBao ACL identity templating with the caller's own
service-account namespace, and each `+` matches exactly one path segment, so the
grants terminate at the literal `/admin/app-credential` and `/service-accounts/+`
leaves and admit no deeper, sibling, or cross-tenant paths. Read coverage needs
no separate grant: the same `eso-tenant` policy carries `read` on the caller's
own `kv-v2/data/openstack/keystone/{ns}/*` subtree, which covers the read-back
leg of every per-CR `admin/app-credential` and `service-accounts` PushSecret.
These grants stay scoped to the per-tenant admin-credential and service-account
leaves, adding no blast radius beyond them. For the mTLS transport gate and the
SecretStore auth path these manifests ride on, see
[OpenBao Bootstrap Procedure](./openbao-bootstrap.md).

## Kustomization

Deployment is split into two kustomize overlays to separate base resources from
CRD-dependent infrastructure resources:

### Base Kustomization

**File:** `deploy/flux-system/kustomization.yaml`

The base kustomization uses `apiVersion: kustomize.config.k8s.io/v1beta1` and includes
namespaces, the FluxInstance CR, HelmRepository sources, and HelmRelease operators.
These resources do not depend on any custom CRDs.

**Resource count:** 26 files producing 40 Kubernetes resources.

| Category | Count | Resources |
| --- | --- | --- |
| Namespace | 15 | cert-manager, mariadb-system, external-secrets, monitoring, memcached-system, garage-system, keystone-system, horizon-system, glance-system, placement-system, openstack, shared-services, openbao-operator-system, c5c3-system, orc-system |
| FluxInstance | 1 | flux (drives the flux-operator) |
| HelmRepository | 7 | cert-manager, mariadb-operator, external-secrets, openbao, c5c3-charts, prometheus-community, garage-operator |
| OCIRepository | 1 | openbao-operator |
| GitRepository | 1 | k-orc |
| HelmRelease | 14 | cert-manager, prometheus-operator-crds, mariadb-operator-crds, mariadb-operator, external-secrets, memcached-operator, garage-operator, openbao, openbao-operator, keystone-operator, horizon-operator, glance-operator, placement-operator, c5c3-operator |
| Kustomization | 1 | k-orc |
| **Total** | **40** | |

The `chaos-mesh` HelmRepository, HelmRelease, and Namespace ship in the
kind-only opt-in overlay at `deploy/kind/chaos-mesh/` and are not
counted here.

### Infrastructure Kustomization

**File:** `deploy/flux-system/infrastructure/kustomization.yaml`

The infrastructure kustomization includes CRD-dependent resources that require their
operator CRDs to be installed first. This kustomization must be applied after the base
kustomization and after operators have finished installing their CRDs.

**Resource count:** 7 manifests producing 13 Kubernetes resources (the
`db-ca-issuer.yaml`, `ovn-ca-issuer.yaml`, and `openbao-ca-issuer.yaml` manifests declare
two resources each: a CA Certificate and the CA-type ClusterIssuer that signs from it;
`garage.yaml` declares four).

| Category | Count | Resources |
| --- | --- | --- |
| ClusterIssuer | 4 | `selfsigned-cluster-issuer`, `openstack-db-ca-issuer`, `openstack-ovn-ca-issuer`, `openbao-ca-issuer` (all require cert-manager CRDs) |
| Certificate (CA keypairs) | 3 | `openstack-db-ca`, `openstack-ovn-ca`, `openbao-ca` — all three CA keypair Secrets in the `cert-manager` namespace, signed by `selfsigned-cluster-issuer` |
| MariaDB | 1 | `openstack-db` (requires mariadb-operator CRDs; TLS enabled per [MariaDB Galera Cluster](#mariadb-galera-cluster)) |
| Memcached | 1 | `openstack-memcached` (requires memcached-operator CRDs) |
| GarageCluster / GarageBucket / GarageKey | 4 | `garage`, `glance-images`, `glance-images-2`, `glance-s3` (require garage-operator CRDs; see [Garage Object Store](#garage-object-store)) |
| **Total** | **13** | |

The proving instance's own five resources (two Certificates, a ServiceAccount, a
ClusterRoleBinding, and the `OpenBaoCluster`) are not counted here: they ship in the kind
overlay — see [OpenBao Proving Instance](#openbao-proving-instance).

<!-- NOTE: count excludes openbao-tls-cert.yaml, openbao-client-tls-cert.yaml,
and the ../../eso overlay that
the infrastructure kustomization also references. Those resources are documented
in their own reference pages (reference/infrastructure/openbao-bootstrap.md and
the ESO reference docs); a full audit of the kustomization resource list is out
of scope here. -->


## Deployment

### Step 1: Apply base resources

```bash
kubectl apply -k deploy/flux-system/
```

This applies 40 resources: 15 namespaces, 1 FluxInstance, 9 sources
(8 HelmRepository + 1 GitRepository for K-ORC), 14 HelmRelease operators, and
1 Kustomization (K-ORC). FluxCD resolves the dependency graph between
HelmReleases and installs operators in the correct order. Wait for all operators to
finish installing before proceeding to step 2.

### Step 2: Apply infrastructure resources

```bash
kubectl apply -k deploy/flux-system/infrastructure/
```

This applies the CRD-dependent resources: the `selfsigned-cluster-issuer`
ClusterIssuer, the `openstack-db-ca-issuer` ClusterIssuer plus its backing CA
`Certificate`, the `openstack-ovn-ca-issuer` ClusterIssuer plus its backing CA
`Certificate`, the `openbao-ca-issuer` ClusterIssuer plus
its backing CA `Certificate`, the MariaDB Galera cluster, the
Memcached cluster, and the Garage object store (`GarageCluster` / `GarageBucket` /
`GarageKey`). These resources require CRDs that are installed
by the operator HelmReleases in step 1. If CRDs are not yet available, the apply will
fail — wait for the operators to finish installing and retry.

> **`hack/deploy-infra.sh` ordering.** The end-to-end deploy script applies the
> three TLS-prerequisite manifests (`cluster-issuer.yaml`, `openbao-tls-cert.yaml`,
> `db-ca-issuer.yaml`) directly in its **Phase 2**, before the main infrastructure
> kustomization, so that MariaDB has `openstack-db-ca-issuer` available the moment
> it tries to render its server certificate. The kustomization apply that follows
> is idempotent — the same manifests are listed in
> `infrastructure/kustomization.yaml` so a manual `kubectl apply -k` path also
> works.

> **Expected transient failure:** The MariaDB cluster references a
> `rootPasswordSecretKeyRef` Secret (`mariadb-root-password`). On kind, the overlay
> shim materialises it from OpenBao via the External Secrets Operator; in production a
> Flux MariaDB baseline must provide it. Until that Secret exists, the
> mariadb-operator will enter a failed reconciliation loop with
> `Secret "mariadb-root-password" not found` errors. This is expected and resolves
> automatically once the Secret is provisioned.

### Validate manifests locally

```bash
kustomize build deploy/flux-system/
kustomize build deploy/flux-system/infrastructure/
```

These commands render the manifest output without applying it. Use them to verify YAML
syntax and resource inclusion before deployment.

### Prerequisites

- A Kubernetes cluster with FluxCD installed (source-controller and helm-controller)
- `kubectl` configured with cluster access
- For local validation only: `kustomize` CLI

## Sizing and placement overrides

The manifests under `deploy/flux-system/` ship the components' own sizing and no
node placement. A deployer changes both with a kustomize overlay per apply phase
and leaves the shipped files untouched. `deploy/examples/sizing-overlay/` is an
example of such an overlay. No script applies it, and
`tests/unit/deploy/sizing_overlay_example_test.sh` renders it and pins every
field it patches. The kind devstack's `deploy/kind/base/` is an overlay built
the same way.

### Recipe

1. Copy `deploy/examples/sizing-overlay/`, including its `infrastructure/`
   subdirectory. Keep, change or drop each patch, and add one for every other
   workload you size, using the field the
   [table below](#where-each-workload-is-sized) names.
2. Point the base entry in each phase's `resources` list at its base; the
   `priorityclass.yaml` entry stays as it is. Inside a checkout of this
   repository a relative path works. Elsewhere, use a remote base pinned to a
   commit: `https://github.com/C5C3/cobaltcore//deploy/flux-system?ref=<commit>`
   for the base phase and
   `https://github.com/C5C3/cobaltcore//deploy/flux-system/infrastructure?ref=<commit>`
   for the infrastructure phase. When you move `<commit>` later, compare each
   copied `spec.chart.spec.version` with the base's at the new commit. The
   patch replaces the whole range, so an upper bound the base has moved past
   holds the release on an older chart, and Flux downgrades a release that
   already runs a newer one.
3. Render both phases with `kustomize build <overlay>/` and
   `kustomize build <overlay>/infrastructure/` and review the output.
4. Label the nodes the platform workloads move to before you apply either
   phase: `kubectl label node <node> node.c5c3.io/role=platform`. A pod whose
   `nodeSelector` matches no node stays `Pending`. To keep other workloads off
   those nodes, also taint them with
   `kubectl taint node <node> node.c5c3.io/role=platform:NoSchedule`; the
   example's tolerations admit its own pods. Check that
   `kubectl get nodes -l node.c5c3.io/role=platform` lists the nodes you
   expect.
5. Run `kubectl apply -k <overlay>/` in place of
   [Step 1](#step-1-apply-base-resources), then
   `kubectl apply -k <overlay>/infrastructure/` in place of
   [Step 2](#step-2-apply-infrastructure-resources). The base phase applies
   the `cobaltcore-platform` PriorityClass that the patches of both phases
   reference. The infrastructure phase does not include it, so
   `kubectl delete -k <overlay>/infrastructure/` leaves the class in place for
   the base phase's pods. On a running deployment the MariaDB patch changes
   the Galera pod template, and mariadb-operator replaces the pods one at a
   time, replicas first and the primary last (its default
   `ReplicasFirstPrimaryLast` update strategy). A replaced pod that cannot be scheduled stops the update with
   the cluster one member short, so watch
   `kubectl get mariadb openstack-db -n openstack` until it reports `Ready`
   again before you change anything else.

The example uses the node label `node.c5c3.io/role: platform`, a matching
`NoSchedule` toleration, and the PriorityClass `cobaltcore-platform` at
`1000000`. Its resource figures are illustrative; size them against the usage
you observe.

### Example files

The base phase, `deploy/examples/sizing-overlay/kustomization.yaml`:

<<< @/../deploy/examples/sizing-overlay/kustomization.yaml

The PriorityClass, `deploy/examples/sizing-overlay/priorityclass.yaml`, which
the base phase includes:

<<< @/../deploy/examples/sizing-overlay/priorityclass.yaml

The infrastructure phase, `deploy/examples/sizing-overlay/infrastructure/kustomization.yaml`:

<<< @/../deploy/examples/sizing-overlay/infrastructure/kustomization.yaml

### Where each workload is sized

One row per entry of `deploy/flux-system/kustomization.yaml` and
`deploy/flux-system/infrastructure/kustomization.yaml` that runs pods:

| Entry | Kind | Field to patch | Sizing keys |
| --- | --- | --- | --- |
| `fluxinstance.yaml` | FluxInstance | `spec.kustomize.patches`, one entry per controller Deployment (`source-controller`, `kustomize-controller`, `helm-controller`, `notification-controller`) | [JSON6902 recipe](#patching-the-upstream-installers-and-the-flux-controllers) |
| `releases/cert-manager.yaml` | HelmRelease | `spec.values` | Checked against chart v1.21.2: `resources`, `nodeSelector` and `tolerations` for the controller, the same keys under `webhook` and `cainjector` for the other two Deployments and under `startupapicheck` for the Job the release runs once at install, and `global.priorityClassName` for all four pods |
| `releases/mariadb-operator.yaml` | HelmRelease | `spec.values` | The chart's values reference at `https://mariadb-operator.github.io/mariadb-operator` (`sources/mariadb-operator.yaml`) |
| `releases/external-secrets.yaml` | HelmRelease | `spec.values` | The chart's values reference at `https://charts.external-secrets.io` (`sources/external-secrets.yaml`) |
| `releases/memcached-operator.yaml` | HelmRelease | `spec.values` | The chart's values reference at `oci://ghcr.io/c5c3/charts` (`sources/c5c3-charts.yaml`) |
| `releases/openbao.yaml` | HelmRelease | `spec.values`; the shipped release already sets `server.resources` | The chart's values reference at `https://openbao.github.io/openbao-helm` (`sources/openbao.yaml`) |
| `releases/garage-operator.yaml` | HelmRelease | `spec.values` | The chart's values reference at `oci://ghcr.io/rajsinghtech/charts` (`sources/garage-operator.yaml`) |
| `releases/openbao-operator.yaml` | HelmRelease | `spec.values` | The chart's values reference at `oci://ghcr.io/dc-tec/charts/openbao-operator` (`sources/openbao-operator.yaml`) |
| `releases/keystone-operator.yaml`, `horizon-operator.yaml`, `glance-operator.yaml`, `placement-operator.yaml`, `barbican-operator.yaml`, `ovn-operator.yaml`, `neutron-operator.yaml`, `cinder-operator.yaml`, `nova-operator.yaml`, `c5c3-operator.yaml` | HelmRelease | `spec.values`, and `spec.chart.spec.version` with the placement keys ([merge rule 3](#merge-rules)) | [`replicas`, `resources`, `nodeSelector`, `tolerations`, `priorityClassName`](../backend/helm-values-schema.md#nodeselector-tolerations-and-priorityclassname) |
| `releases/k-orc.yaml` | Flux Kustomization | `spec.patches` | [JSON6902 recipe](#patching-the-upstream-installers-and-the-flux-controllers) |
| `releases/rabbitmq-cluster-operator.yaml` | Flux Kustomization | `spec.patches` | [JSON6902 recipe](#patching-the-upstream-installers-and-the-flux-controllers) |
| `infrastructure/mariadb.yaml` | CR (`MariaDB`) | `spec.resources`, `spec.nodeSelector`, `spec.tolerations`, `spec.priorityClassName` | The `ContainerTemplate` and `PodTemplate` fields of mariadb-operator v0.38.1. The embedded `spec.maxScale` carries none of them |
| `infrastructure/memcached.yaml` | CR (`Memcached`) | `spec.resources` | The CRD has no placement fields. The operator's webhook requires a memory limit of at least `maxMemoryMB` plus 32Mi, 96Mi at the default of 64 |
| `infrastructure/garage.yaml` | CR (`GarageCluster`) | `spec.storage.resources`, `spec.storage.nodeSelector`, `spec.storage.tolerations`, `spec.storage.priorityClassName` | The pod template `GarageCluster` v1beta2 inlines into `spec.storage`; confirm against the installed CRD with `kubectl explain garagecluster.spec.storage` |

The two CRD-only releases (`prometheus-operator-crds`, `mariadb-operator-crds`),
the sources, namespaces, issuers and certificates, and the External Secrets
`ClusterSecretStore` run no pods. The flux-operator itself is installed out of
band by `hack/deploy-infra.sh` and is outside the overlay's reach.

### Patching the upstream installers and the Flux controllers

K-ORC and the RabbitMQ cluster operator are Flux Kustomizations over an upstream
installer, and the FluxInstance renders the Flux controllers. None of them has a
values file, so the overlay adds a JSON6902 patch that the kustomize-controller
or the flux-operator applies to the rendered Deployment:

- The K-ORC installer (`config/default`) and the RabbitMQ cluster-operator
  installer (`config/installation`) each render one Deployment with one
  container at the pinned commits; a pin move has to re-check that.
  `target: {kind: Deployment}` selects the Deployment whatever name prefix the
  upstream base applies (K-ORC's `namePrefix: orc-` turns `controller-manager`
  into `orc-controller-manager`), and `containers/0` is the manager
  container.
- A FluxInstance entry names one controller Deployment, for example
  `target: {kind: Deployment, name: helm-controller}`.
- `op: add` sets a member whether or not it exists, and replaces the whole
  value when it does. The patch therefore replaces the upstream `resources`
  block instead of merging into it. The example sizes the K-ORC manager this
  way and leaves the RabbitMQ cluster operator's upstream `200m` / `500Mi` in
  place.
- The Flux manifests run the source, kustomize and helm controllers with
  `priorityClassName: system-cluster-critical`, which ranks above
  `cobaltcore-platform`, so the example's FluxInstance patch leaves the helm
  controller's class alone. The notification-controller has no class and can
  take `cobaltcore-platform` as a fourth operation.

### Merge rules

1. kustomize applies a merge patch to a HelmRelease's `spec.values`: a patched
   list, such as `tolerations`, replaces the base list, and a patched map merges
   key by key. Helm then merges the result with the chart defaults the same
   way, so cert-manager's default `kubernetes.io/os: linux` node selector stays
   beside a label the patch adds.
2. A chart value the shipped release sets can be removed only by setting it to
   `null` in the patch; omitting the key keeps the shipped value.
3. The `nodeSelector`, `tolerations` and `priorityClassName` keys of the
   CobaltCore operator charts need operator-library 0.9.0: keystone-operator
   0.11.0, c5c3-operator 0.14.0, horizon-operator 0.4.0, cinder-operator and
   nova-operator 0.2.0, the other five charts 0.3.0, or later. An older chart
   rejects the keys through its values schema, and the HelmRelease reports the
   failure in its `Ready` condition. The shipped releases'
   `spec.chart.spec.version` floors still admit such a chart, so a patch that
   sets the keys raises the floor to the version above as well, as the
   example does for keystone-operator with `>=0.11.0 <1.0.0`.

## Extensibility

The manifest structure is designed for straightforward extension. Adding a new operator
(e.g., OpenBao) requires four steps:

1. **Add a source file** in `sources/` (e.g., `sources/openbao.yaml`) — or reuse an
   existing HelmRepository if the chart is in a shared registry
2. **Add a release file** in `releases/` (e.g., `releases/openbao.yaml`) with the
   HelmRelease CR, `dependsOn` for cert-manager, and the standard install/upgrade settings
3. **Add both paths** to the `resources` list in `kustomization.yaml`
4. **Add the release's target namespace** to `namespaces.yaml` (e.g., `shared-services`,
   where `releases/openbao.yaml` puts the OpenBao HelmRelease) so the namespace exists
   before `kubectl apply -k` creates the namespaced HelmRelease CR

The [garage-operator](#garage-operator) is the worked example of an **OCI-type
third-party** operator following this recipe: step 1 adds an OCI HelmRepository
(`sources/garage-operator.yaml`, `spec.type: oci`, the registry namespace as `url`); the
release (`releases/garage-operator.yaml`) references it by `sourceRef.name` and carries
the chart name in `chart.spec.chart`. The OCI variant changes nothing else in the recipe.
Renovate's native Flux manager reads the range of an OCI chart through the docker
datasource, and a generic packageRule sets `versioning: helm` for every such range that
starts with `>=`, so a new OCI chart needs no rule of its own as long as its range is
spelled `>=X <Y` (see
[Flux HelmRelease chart versions](../../contributing/dependency-management.md#flux-helmrelease-chart-versions)).

**An operator with no chart** takes the Git-sourced variant of the recipe, which
[K-ORC](#k-orc-openstack-resource-controller) and the
[RabbitMQ Cluster Operator](#rabbitmq-cluster-operator) both follow. Step 1 adds a
`GitRepository` scoped to the installer path with `spec.ignore`, and step 2 adds a
Flux `Kustomization` over that path instead of a HelmRelease, with the image
pinned through `spec.images` (`newTag` plus `digest`) because a kustomize base
carries no values file. Renovate's native Flux manager is switched off for both
files, since a customManager moves a tag together with its commit or digest in one
match. Each half needs a customManager anchored on the literal YAML shape, a
packageRule sharing one `groupName` so the pair moves in one PR, and one test per
manager under `tests/unit/renovate/`, and both files join the `matchFileNames` of
the flux rule with `enabled: false` in `renovate.json`. Two more places have to learn the name: the
`FLUX_KUSTOMIZATIONS` array in `hack/deploy-mgmt-cluster.sh`, which applies and
waits for such a Kustomization after the HelmReleases, and the Phase 3b wait in
`hack/deploy-infra.sh`, since a Kustomization is invisible to
`wait_for_helmreleases`.

Infrastructure instance CRs (e.g., a new database, cache, or object-store cluster) follow
the same pattern: add a file in `infrastructure/` and list it in
`infrastructure/kustomization.yaml` (see [Garage Object Store](#garage-object-store) and
its single-node kind patch for a multi-CR example).

**Attaching a Barbican secret store.** Two targets are already provisioned. The shared
management OpenBao carries the brownfield outputs from the bootstrap scripts: the KV v2
mount `barbican/`, the policy `barbican-secretstore`, and the AppRole role `barbican`
(see the [OpenBao bootstrap reference](./openbao-bootstrap.md)). For a dedicated instance,
[OpenBao Proving Instance](#openbao-proving-instance) is the template: the same three
names, self-initialized by the openbao-operator, plus the Kubernetes-auth role a service
operator authenticates with to mint the AppRole secret ID.

## Design Decisions

### Two-phase kustomization

Resources are split into a base kustomization (namespaces, sources, releases) and an
infrastructure kustomization (CRD-dependent resources). This separation ensures that
`kubectl apply -k` does not attempt to create CRD-dependent resources before the
corresponding CRDs exist. The base kustomization can be applied independently, and the
infrastructure kustomization is applied after operators have installed their CRDs.

In FluxCD-managed clusters, this pattern maps to two FluxCD Kustomization CRs where the
infrastructure Kustomization depends on the base Kustomization (using `spec.dependsOn`),
eliminating noisy first-apply failures.

### Explicit namespace resources

All target namespaces are defined as explicit `Namespace` resources in `namespaces.yaml`.
While HelmReleases set `install.createNamespace: true` for FluxCD's helm-controller, the
explicit namespace resources ensure namespaces exist before `kubectl apply -k` attempts
to create namespaced resources (HelmRelease CRs specify a target namespace in their
metadata).

### Namespace auto-creation

All HelmReleases set `install.createNamespace: true` as a safety net for FluxCD
deployments. This is complementary to the explicit `Namespace` resources — the explicit
resources handle the `kubectl apply -k` path, while `createNamespace` handles edge cases
in FluxCD reconciliation.

### No secret configuration

The manifests intentionally contain no password, credential, or secret configuration.
Secret management is handled by the External Secrets Operator integration,
which provisions secrets from an external vault into the cluster.

### Memcached Operator source

The Memcached Operator chart is sourced from the shared `c5c3-charts` OCI registry
rather than a dedicated HelmRepository. This follows the project convention of publishing
internally-built charts to `oci://ghcr.io/c5c3/charts`.

## Kind Overlay Demo Addons

The kind overlay (`deploy/kind/base/kustomization.yaml`) layers a small set of
kind-only demo manifests on top of the production base. These files live under
`deploy/kind/base/` and are **not** referenced from `deploy/flux-system/kustomization.yaml`,
so they never reach production clusters. The section below catalogues these addons;
earlier kind-only manifests (Headlamp, OpenBao UI patch) are documented in the Quick Start.
Chaos Mesh ships as a
separate **opt-in** kind overlay at `deploy/kind/chaos-mesh/` — applied only when
`WITH_CHAOS_MESH=true` is set on `make deploy-infra`; see
[Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in) below.

### Flux Web UI ResourceSet

**File:** `deploy/kind/base/flux-web.yaml`

A single `ResourceSet` CR drives the flux-operator's bundled
[Flux Web UI](https://fluxoperator.dev/web-ui/) as a demo surface for the kind
Quick Start (Step 4a). The `ResourceSet` renders two sibling resources — an
`OCIRepository` pointing at the official flux-operator Helm chart and a
`HelmRelease` that installs that chart with only the Web UI sub-chart enabled.

| Property | Value |
| --- | --- |
| API version | `fluxcd.controlplane.io/v1` |
| Kind | `ResourceSet` |
| Name | `flux-web` |
| Namespace | `flux-system` |
| Chart URL | `oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator` |
| Version pin (input) | `0.53.x` — SemVer range locked to the minor track of `FLUX_OPERATOR_VERSION` in `hack/deploy-infra.sh` |

**Helm values on the nested `HelmRelease`:**

| Key | Value | Purpose |
| --- | --- | --- |
| `web.serverOnly` | `true` | Render only the Web UI Deployment + Service; skip the operator Deployment, CRDs, and RBAC that the original `install.yaml` bootstrap already owns |
| `installCRDs` | `false` | The flux-operator CRDs (`FluxInstance`, `ResourceSet`, `ResourceSetInputProvider`, …) are already installed by the out-of-band `install.yaml` apply in `hack/deploy-infra.sh` — re-applying them here would fight the bootstrap on every reconcile |
| `fullnameOverride` | `flux-web` | Give the Web UI Deployment / Service / ServiceAccount a distinct identity so they do not collide with the operator's own `flux-operator-*` workload names |

**Version tracking.** The `spec.inputs[0].version` SemVer range is updated
automatically by a Renovate `customManager` entry in `renovate.json` that
targets `deploy/kind/base/flux-web.yaml` and pulls release metadata from
`controlplaneio-fluxcd/flux-operator` GitHub releases. The customManager shares
the same `packageRules` as `hack/deploy-infra.sh` — major upgrades are
disabled, minor/patch upgrades auto-merge after a three-day `minimumReleaseAge`
cooldown.

**Production opt-out.** `deploy/flux-system/kustomization.yaml` deliberately
does **not** list `deploy/kind/base/flux-web.yaml`. The flux-operator Web UI
ships without token authentication, without TLS termination, and without an
Ingress story — it is safe as a localhost port-forward demo on a single-node
kind cluster, not as a shared-cluster surface. Production overlays can opt
back in explicitly once upstream adds those prerequisites.

**Access (kind Quick Start, Step 4a):**

```bash
kubectl port-forward svc/flux-web -n flux-system 9080:9080
```

Browse <http://localhost:9080> — no login required. The Web UI complements
Headlamp by rendering the three flux-operator-specific CRDs (`ResourceSet`,
`ResourceSetInputProvider`, `FluxReport`) that the generic Headlamp Flux
plugin does not know about.

### Chaos Mesh (kind-only opt-in)

**File:** `deploy/kind/chaos-mesh/kustomization.yaml`

[Chaos Mesh](https://chaos-mesh.org/) ships as a separate **opt-in** kind
overlay. The default `make deploy-infra` flow does **not** install
it — first-run deployments skip the privileged `chaos-daemon` DaemonSet, the
`chaos-mesh` namespace, and the upstream HelmRepository / HelmRelease pair so
that developers who never run chaos E2E suites pay zero install cost. The
production `deploy/flux-system/` overlay also does not install Chaos Mesh.

The overlay is self-contained: the `HelmRepository` lives in
`deploy/kind/chaos-mesh/source.yaml` and the `HelmRelease` in
`deploy/kind/chaos-mesh/release.yaml` (both relocated from the former
`deploy/flux-system/{sources,releases}/chaos-mesh.yaml` locations). The
overlay bundles them with:

| Property | Value |
| --- | --- |
| Target namespace | `chaos-mesh` (created inline with the privileged PodSecurity label required by `chaos-daemon`'s host PID/network access) |
| Chart | `chaos-mesh` |
| Version constraint | `>=2.6.0 <3.0.0` |
| Source | `chaos-mesh` HelmRepository (`deploy/kind/chaos-mesh/source.yaml`) |
| Dependencies | `cert-manager` in `cert-manager` namespace |

**Kind-tuning patch** (relocated here from
`deploy/kind/base/kustomization.yaml` because kustomize requires the patch
target to live in the same overlay):

| Helm value | Override | Purpose |
| --- | --- | --- |
| `chaosDaemon.runtime` | `containerd` | Match the kind node's container runtime |
| `chaosDaemon.socketPath` | `/run/containerd/containerd.sock` | Mount the kind containerd socket so chaos-daemon can attack pods |
| `chaosDaemon.resources` | `25m / 64Mi` requests | Reduce footprint on single-node kind |
| `dashboard.create` | `false` | Dashboard is unnecessary in CI |
| `controllerManager.resources` | `25m / 64Mi` requests | Reduce footprint on single-node kind |

These overrides diverge intentionally from the upstream chart defaults
(dashboard enabled, larger resource requests, auto-detected runtime), which
target multi-node production clusters. Because the patch and the
HelmRelease both live in the kind-only overlay, production environments that
opt into Chaos Mesh start from the upstream defaults instead of inheriting
the kind-tuning values.

**No load-restrictor flag required.** The overlay has no parent-directory
`../../` references — every resource (`namespace.yaml`, `source.yaml`,
`release.yaml`) lives under `deploy/kind/chaos-mesh/`. Kustomize's default
`LoadRestrictionsRootOnly` security check is therefore satisfied without
`--load-restrictor=LoadRestrictionsNone`, which matters because kubectl's
embedded kustomize does not expose that flag (kubernetes/kubectl#948) and
`hack/deploy-infra.sh` invokes the apply via `kubectl apply -k`.

**Opt-in usage:**

```bash
WITH_CHAOS_MESH=true make deploy-infra
```

This is the prerequisite for `make e2e-chaos`. See
[Chaos E2E Tests](../testing/chaos-e2e-tests.md) for the full workflow.

### kube-prometheus-stack (kind-only opt-in)

**File:** `deploy/kind/prometheus/kustomization.yaml`

[`kube-prometheus-stack`](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
ships as a separate **opt-in** kind overlay. The default
`make deploy-infra` flow does **not** install it — the `monitoring`
namespace stays absent, and Prometheus / Grafana / the prometheus-operator
pods do not consume any of the kind node's CPU or memory budget unless a
contributor explicitly opts in. The production `deploy/flux-system/` overlay
also does not install the stack: production clusters are expected to run
their own Prometheus and widen its `serviceMonitorSelector` to pick up the
keystone-operator chart's `ServiceMonitor` (see
[Enable Keystone Operator Metrics](../../guides/keystone/enable-keystone-operator-metrics.md)
for that wiring path).

The overlay is self-contained: the `Namespace` and `HelmRelease` live in
`deploy/kind/prometheus/namespace.yaml` and
`deploy/kind/prometheus/release.yaml`, and the upstream `prometheus-community`
HelmRepository in `deploy/flux-system/sources/prometheus-community.yaml` is
**reused** (it is already present for the `prometheus-operator-crds`
HelmRelease in the production base, so no new source manifest is added to
the production tree). The overlay bundles the resources with:

| Property | Value |
| --- | --- |
| Target namespace | `monitoring` (created inline; no PodSecurity label override required) |
| Chart | `kube-prometheus-stack` |
| Version constraint | `>=65.0.0 <70.0.0` |
| Source | `prometheus-community` HelmRepository (reused from `deploy/flux-system/sources/`) |
| Dependencies | `cert-manager` in `cert-manager` namespace |

**Kind-tuned values** (deliberately too lean for a real workload — they exist
so the stack fits in a single-node kind cluster alongside Flux, the operators,
and the OpenStack control plane):

| Helm value | Override | Purpose |
| --- | --- | --- |
| `crds.enabled` | `false` | The `monitoring.coreos.com` CRDs are already installed by the production-base `prometheus-operator-crds` HelmRelease — re-installing them from the chart would fight that release on every reconcile |
| `alertmanager.enabled` | `false` | No alert routing in a developer cluster |
| `nodeExporter.enabled` | `false` | Single-node kind has no meaningful node-level metrics worth scraping |
| `kubeStateMetrics.enabled` | `false` | Kube-state-metrics adds noise the kind dashboards do not consume |
| `prometheus.prometheusSpec.retention` | `6h` | Short retention keeps the Prometheus PVC tiny on kind |
| `prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues` | `false` | Allow the operator chart's `ServiceMonitor` to be scraped without forcing a `release: kube-prometheus-stack` label on it |
| `prometheus.prometheusSpec.serviceMonitorSelector` | `{}` | Match every `ServiceMonitor` in the cluster (kind only — production overlays should use a tighter selector) |
| `prometheus.prometheusSpec.serviceMonitorNamespaceSelector` | `{}` | Match every namespace (kind only — see above) |
| `prometheus.prometheusSpec.resources` / `grafana.resources` | `100m CPU / 256Mi mem` caps | Hard cap on kind resource use |

**Dashboard provisioning**. The overlay also adds a
`configMapGenerator` that bundles the keystone-operator dashboard JSON
(`operators/keystone/dashboards/keystone-operator.json` — the **single source
of truth**, never forked into the overlay) with the
`grafana_dashboard: "1"` and `app.kubernetes.io/part-of: kube-prometheus-stack`
labels. Grafana's sidecar discovers the labelled ConfigMap on startup and
imports it into the **Dashboards → Keystone Operator** entry without any
manual API call. Because the dashboard JSON lives outside the overlay
directory, `hack/deploy-infra.sh` performs an idempotent copy
into `deploy/kind/prometheus/keystone-operator.json` immediately before
`kubectl apply -k` runs — this satisfies kustomize's default
`LoadRestrictionsRootOnly` constraint (the overlay has no `../` references)
without requiring `--load-restrictor=LoadRestrictionsNone`.

**Local validation (`make stage-prometheus-dashboard`).** The staged
`deploy/kind/prometheus/keystone-operator.json` is git-ignored — the
canonical file lives only at `operators/keystone/dashboards/keystone-operator.json`.
Developers who want to run `kustomize build deploy/kind/prometheus/`,
`kubectl apply -k deploy/kind/prometheus/`, or `chainsaw lint` against the
overlay **without** running `WITH_PROMETHEUS=true make deploy-infra` first
must stage the dashboard manually:

```bash
make stage-prometheus-dashboard
```

The target performs the same `cp -f` that `hack/deploy-infra.sh` runs at
deploy time, so local renders match CI exactly. `make deploy-infra`
re-runs the copy on every invocation, so explicit staging is not needed
when going through the full deploy path.

**ServiceMonitor enablement**. The keystone-operator
chart defaults to `monitoring.serviceMonitor.enabled=false` so production
overlays inherit the safe default. When `WITH_PROMETHEUS=true`,
`hack/deploy-infra.sh` waits for the `kube-prometheus-stack` HelmRelease to
become Ready, then runs:

```bash
kubectl patch helmrelease keystone-operator -n keystone-system --type=merge \
  -p '{"spec":{"values":{"monitoring":{"serviceMonitor":{"enabled":true}}}}}'
```

…and waits for the keystone-operator HelmRelease to reconcile back to
`Ready=True` on the new values. The patch is **only applied when
`WITH_PROMETHEUS=true`** — the chart values themselves are never modified,
which keeps the production posture unchanged.

**Opt-in usage:**

```bash
WITH_PROMETHEUS=true make deploy-infra
```

This is the prerequisite for `make e2e-prometheus` (see
[CI / e2e-prometheus job](../ci-cd/ci-workflow#e2e-prometheus) for the workflow). For the kind UI
walkthrough — port-forward, default Grafana credentials, the bundled
`Keystone Operator` dashboard, and a Prometheus targets sanity-check — see
[Extended Quick Start — Step 4c](../../quick-start-extended.md#step-4c-grafana-ui).

**Posture summary.** Reviewers checking new kind-only opt-ins should treat
this entry as a parallel of the `Chaos Mesh (kind-only opt-in)` example
above: the production omission is explicit, the opt-in flag has a single
documented name (`WITH_PROMETHEUS`), and the kind overlay is self-contained
under `deploy/kind/prometheus/` so the production kustomization root is
untouched. The
[`document-intentional-environment-divergence-in-overlays`](https://github.com/c5c3/cobaltcore/blob/main/.planwerk/review_patterns/document-intentional-environment-divergence-in-overlays.md)
review pattern catalogues the full surface area.

### metrics-server (kind-only opt-in)

**File:** `deploy/kind/metrics-server/kustomization.yaml`

[`metrics-server`](https://github.com/kubernetes-sigs/metrics-server) ships as
a separate **opt-in** kind overlay. The default `make deploy-infra` flow does
**not** install it — the `kube-system` metrics-server stays absent so the
default Quick Start does not spend the kind node's budget on a component most
tutorials do not need. The production `deploy/flux-system/` overlay also does
not install it: managed distributions ship their own metrics-server, and
production clusters bring their own.

The overlay backs the
[Autoscaling (HPA) recipe](../../guides/advanced-configuration.md#autoscaling-hpa)
and the `e2e-autoscaling` CI job, which deploys it with
`WITH_METRICS_SERVER=true` to drive a Keystone HPA from one pod to its maximum
(see [e2e-autoscaling](../testing/controlplane-e2e-tests.md#e2e-autoscaling)):
the operator-generated `HorizontalPodAutoscaler` reads CPU/memory utilisation
from the resource-metrics API, and without a metrics-server it reports
`unknown/80%` and never scales.

The overlay is self-contained: the `HelmRepository` and `HelmRelease` live in
`deploy/kind/metrics-server/source.yaml` and
`deploy/kind/metrics-server/release.yaml`. Unlike the chaos-mesh and
kube-prometheus-stack overlays it ships **no** `Namespace` — the chart defaults
to `priorityClassName: system-cluster-critical`, which only resolves in the
pre-existing `kube-system` Namespace, so the HelmRelease targets `kube-system`
directly.

| Property | Value |
| --- | --- |
| Target namespace | `kube-system` (pre-existing; no inline Namespace) |
| Chart | `metrics-server` |
| Version constraint | `>=3.12.0 <4.0.0` |
| Source | `metrics-server` HelmRepository (`https://kubernetes-sigs.github.io/metrics-server/`) |
| Dependencies | none |

**Kind-tuned values:**

| Helm value | Override | Purpose |
| --- | --- | --- |
| `args` | `["--kubelet-insecure-tls"]` | kind's kubelets serve `/metrics/resource` with a self-signed certificate metrics-server cannot verify against the cluster CA; skipping verification lets scrapes succeed. This replaces the runtime `kubectl patch` the autoscaling recipe previously documented — never set it on a production cluster with properly issued kubelet certificates |

When `WITH_METRICS_SERVER=true`, `hack/deploy-infra.sh` runs
`kubectl apply -k deploy/kind/metrics-server` in Step 3 and appends
`metrics-server` to the Phase 3 HelmRelease wait list. Both actions are gated
strictly on the flag; the chart values are never modified, which keeps the
production posture unchanged.

**Opt-in usage:**

```bash
WITH_METRICS_SERVER=true make deploy-infra
```

**Posture summary.** Same shape as the two entries above: the production
omission is explicit, the opt-in flag has a single documented name
(`WITH_METRICS_SERVER`), and the kind overlay is self-contained under
`deploy/kind/metrics-server/` so the production kustomization root is untouched.

### VPA recommender (kind-only opt-in)

**File:** `deploy/kind/vpa/kustomization.yaml`

The [VerticalPodAutoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler)
recommender ships as a separate opt-in kind overlay. Neither the default
`make deploy-infra` flow nor the production `deploy/flux-system/` overlay
installs it.

It has two consumers. The first is the
[sizing measurement](../testing/sizing-calibration.md).
`hack/ci-vpa-recommendations.sh`
creates a VPA with `updateMode: "Off"` for every workload in the `openstack`
namespace and records the recommender's CPU and memory targets. The
`ci:measure-sizing` label runs that measurement in CI (see the
[CI workflow](../ci-cd/ci-workflow.md) label table).

The second is the `e2e-autoscaling` job, which always deploys the overlay. Its
operators find the VerticalPodAutoscaler CRD at startup and create the VPAs
that the ControlPlane's and the OVNCentral's `verticalAutoscaling` blocks opt
into, and the
[e2e-autoscaling suite](../testing/controlplane-e2e-tests.md#e2e-autoscaling)
waits for the recommender to serve one of them. Without the updater and the
admission controller, an opt-in with `updateMode` `Initial`, `Recreate` or
`Auto` changes no pod on this overlay either, so the suite asserts the VPA
objects and a recommendation, not rewritten requests.

Recommendation-only mode changes no pod, so the release installs the VPA CRDs
and the recommender alone: the updater and the admission controller are
disabled. At one replica the chart turns leader election off. The overlay
ships no `Namespace`; the HelmRelease targets the pre-existing `kube-system`
Namespace, and the `HelmRepository` lives in `flux-system`.

| Property | Value |
| --- | --- |
| Target namespace | `kube-system` (pre-existing; no inline Namespace) |
| Chart | `vertical-pod-autoscaler` |
| Version constraint | `>=0.13.0 <0.14.0` (0.13.0 ships VPA 1.8.0; the minor bound keeps the recommender's behaviour fixed between measurements) |
| Source | `autoscaler` HelmRepository (`https://kubernetes.github.io/autoscaler`) |
| Dependencies | metrics-server (the recommender reads the resource-metrics API) |

**Kind-tuned values:**

| Helm value | Override | Purpose |
| --- | --- | --- |
| `admissionController.enabled` | `false` | No VPA of the measurement rewrites pod requests |
| `updater.enabled` | `false` | No VPA of the measurement evicts pods |
| `recommender.replicas` | `1` | One recommender; the chart disables leader election |
| `recommender.extraArgs` | `--pod-recommendation-min-cpu-millicores=1`, `--pod-recommendation-min-memory-mb=1` | The recommender divides a per-pod floor (25 millicores and 250 MiB by default) evenly among a pod's containers. With the default floor a container using less than its share reports the floor instead of its use |
| `recommender.resources` | requests `50m` / `256Mi`, limit `512Mi` memory | Keeps the recommender itself small on the measured node |
| `crds.enabled` | `true` | Installs `verticalpodautoscalers.autoscaling.k8s.io`, which the collector checks for and the operators need to create opt-in VPAs |

`--memory-saver` keeps its default `false`, so the recommender samples every
pod from its start, whether or not a VPA selects it yet.

When `WITH_VPA=true`, `hack/deploy-infra.sh` sets `WITH_METRICS_SERVER=true`,
runs `kubectl apply -k deploy/kind/vpa` in Step 3 right after the
metrics-server overlay, and appends `vertical-pod-autoscaler` to the Phase 3
HelmRelease wait list. The banner prints the flag as `VPA recommender`.
Once the MariaDB CRD is established, the script also removes the scale
subresource from `mariadbs.k8s.mariadb.com`. The recommender only accepts a
VPA on the topmost well-known or scalable controller, and the MariaDB scale
subresource has no pod selector, so without the removal the database
StatefulSet cannot be measured. The mariadb-operator-crds HelmRelease has no
drift detection, so the change lasts until the next chart upgrade, and
nothing in the kind stack scales a MariaDB through the subresource.

**Opt-in usage:**

```bash
WITH_VPA=true make deploy-infra
```

### dizzy load/chaos stack (kind-only opt-in)

**File:** `deploy/kind/dizzy/kustomization.yaml`

[dizzy](https://github.com/B42Labs/dizzy) is a scenario-driven load and
consistency tester for OpenStack control planes. Its VictoriaMetrics + Grafana
observability stack ships as a separate **opt-in** kind overlay. The default
`make deploy-infra` flow does **not** install it — the `dizzy` namespace stays
absent, and neither VictoriaMetrics nor Grafana runs unless a contributor opts
in. The production `deploy/flux-system/` overlay ships none of it.

The overlay is self-contained, seven tracked files under `deploy/kind/dizzy/`:

| File | Purpose |
| --- | --- |
| `namespace.yaml` | The `dizzy` Namespace, declared inline so the overlay is self-contained |
| `source-victoria-metrics.yaml` | `victoria-metrics` HelmRepository |
| `source-grafana.yaml` | `grafana` HelmRepository |
| `release-victoria-metrics.yaml` | `dizzy-victoria-metrics` HelmRelease (chart `victoria-metrics-single`, NodePort 30428, emptyDir storage, 30d retention, OTLP ingest at `/opentelemetry/v1/metrics`) |
| `release-grafana.yaml` | `dizzy-grafana` HelmRelease (anonymous Viewer access, provisioned `victoriametrics` datasource, dizzy Overview home dashboard) |
| `httproute.yaml` | The static `dizzy-grafana` HTTPRoute attaching to the `https-dizzy` listener |
| `kustomization.yaml` | Ties the resources together and generates the dashboard ConfigMap |

Both HelmRepositories (`victoria-metrics`, `grafana`) are declared into the
`flux-system` namespace, where Flux resolves HelmRelease sourceRefs, even though
the two source files live inside the kind-only overlay. That keeps
`deploy/flux-system/**` untouched while the overlay stays self-contained.

**Dashboard staging (`hack/dizzy.sh stage-dashboards`).** The overlay's
`configMapGenerator` wraps three dizzy dashboard JSONs, but
`deploy/kind/dizzy/dashboards/` is git-ignored; the dashboards are a
version-pinned dizzy asset staged from the release tarball, so the tree tracks
none of them. `hack/dizzy.sh stage-dashboards` copies them out of that tarball
(cached under `_output/dizzy/<version>/`) before `kubectl apply -k` runs.
`WITH_DIZZY=true make deploy-infra` performs the staging automatically; a raw
`kustomize build deploy/kind/dizzy` on a fresh checkout fails until
`stage-dashboards` has populated the directory.

**Gateway wiring.** Two pieces are present even without `WITH_DIZZY`: the
`https-dizzy` listener on Gateway `openstack-gw`
(`deploy/kind/base/openstack-gateway.yaml`) and the `dizzy-nip-io-tls`
Certificate (`deploy/kind/infrastructure/dizzy-nip-io-tls-certificate.yaml`).
The listener admits routes from the `dizzy` namespace through a namespace
selector on the automatic `kubernetes.io/metadata.name` label, so no
ReferenceGrant is required. This is the repo's first cross-namespace-admitting
listener. The `dizzy-grafana` HTTPRoute itself ships only in the gated overlay;
without it the `dizzy.127-0-0-1.nip.io` hostname answers 404.

**Host port mapping.** `hack/kind-config.yaml` maps host `127.0.0.1:8428` to the
node's containerPort 30428, bridging host-side OTLP export to the
VictoriaMetrics NodePort. A cluster created before this mapping existed keeps
working, but the tooling's port probe warns; recreate the cluster
(`make teardown-infra && WITH_DIZZY=true make deploy-infra`) to pick it up.

**Opt-in usage:**

```bash
WITH_DIZZY=true make deploy-infra
```

To drive the chaos soak against the ControlPlane, see
[dizzy Chaos Testing](../testing/dizzy-chaos-testing.md).

**Posture summary.** Same shape as the entries above: the production omission is
explicit, the opt-in flag has a single documented name (`WITH_DIZZY`), and the
kind overlay is self-contained under `deploy/kind/dizzy/` so the production
kustomization root ships none of it.

### NFS storage stack (opt-in)

**File:** `deploy/kind/nfs/kustomization.yaml`

The NFS storage stack, an in-cluster NFSv4 server plus the
[csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) mounter,
ships as a separate **opt-in** kind overlay for the Cinder e2e suites of
[#979](https://github.com/c5c3/cobaltcore/issues/979). The default
`make deploy-infra` flow does **not** install it. Unlike the other opt-ins of
this section it is not kind-only: the metal-stack lab builds its
[Lab NFS stack](#lab-nfs-stack) on this overlay. Production omits it for two
reasons: users bring their own NFS server (a `CinderBackend` takes a `server`
and a `path` per backend), and the mounter is a prerequisite of the target
cluster. Shipping `csi-driver-nfs` from `deploy/flux-system/releases/` would
put a privileged `hostNetwork` DaemonSet into every production deployment for
a service no production cluster runs yet.

One more reason keeps it opt-in on kind. The server image
`itsthenetwork/nfs-server-alpine:12` is amd64-only (a single-architecture
manifest, last pushed 2019-05-08) and drives the host kernel's `rpc.nfsd`. An
always-on manifest would put a privileged pod into the default Quick Start
that CrashLoops on every arm64 laptop and every host without a loadable
`nfsd`.

The overlay is self-contained: `source.yaml`, `release.yaml` and
`nfs-server.yaml` are all local to `deploy/kind/nfs/`. It ships **no**
`Namespace`. The HelmRelease targets the pre-existing `kube-system`, because
the chart defaults to `priorityClassName: system-cluster-critical`, which
resolves in no other Namespace. The server lands in the pre-existing
`openstack`, which `deploy/flux-system/namespaces.yaml` reserves for
components that exist only to run the operators standalone. `shared-services`
is not used: it is the trust zone holding `openbao-init-keys`, with Garage as
its one accepted co-tenant.

| Property | Value |
| --- | --- |
| Target namespaces | `kube-system` for the chart, `openstack` for the server (both pre-existing; no inline Namespace) |
| Chart | `csi-driver-nfs` |
| Chart version | `4.13.4`, an exact pin tracked by Renovate's flux manager (majors disabled, no automerge). Unlike the chaos-mesh, metrics-server and dizzy overlays this one carries no `>=x <y` range: a range would let Flux adopt a new chart on its next reconcile with no repo diff, and this release installs a privileged `hostNetwork` DaemonSet whose relied-on chart defaults it does not override |
| Source | `csi-driver-nfs` HelmRepository (`https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts`). That is the only place upstream publishes the chart; every index entry carries an absolute tarball URL back under `master/charts/`, so pinning the repository URL to a tag would freeze the index without making the downloaded chart more immutable. The version pin therefore controls which version Flux installs, not which bytes: Flux `spec.verify` is OCI-only and nothing records a checksum, so a rewrite of the pinned tarball upstream is adopted on the next reconcile. Accepted for the kind overlay and for the lab, which takes the release unchanged; a content pin means mirroring the chart into a registry this project controls and referencing it by digest |
| Server image | `docker.io/itsthenetwork/nfs-server-alpine:12`, digest-pinned, tracked by a Renovate `customManager` |
| Dependencies | none |

**Export layout.** The server exports `/exports` with `fsid=0`, which makes it
the NFSv4 pseudo-root. The init container `prepare-exports` creates
`/exports/volumes` and `/exports/backups` as `42424:42424` with mode `0770`. A
client therefore mounts the two shares as:

```text
nfs-server.openstack.svc.cluster.local:/volumes
nfs-server.openstack.svc.cluster.local:/backups
```

A path of `:/exports/volumes` resolves to `/exports/exports/volumes` on the
server and fails. Those two share strings are what the `CinderBackend` and
`CinderBackupBackend` of #979 carry. The server speaks NFSv4 only
(`rpc.nfsd --no-udp --no-nfs-version 2 --no-nfs-version 3`), so `2049/TCP` is
the whole client surface and the Service exposes neither 111 nor a mountd
port.

**Value overrides:**

| Helm value | Override | Purpose |
| --- | --- | --- |
| `controller.enableSnapshotter` | `false` | The chart defaults it to `true` while `externalSnapshotter.enabled` defaults to `false`, so the `csi-snapshotter` sidecar would be deployed against `snapshot.storage.k8s.io` CRDs nothing in this stack installs. The Cinder NFS backend has snapshots off |
| `storageClass.create` | `false` | Already the chart default, set here with the reason: the cinder-operator mounts inline volumes, so no dynamic class is wanted, and an unwanted class in a kind cluster competes for `is-default-class` |
| `feature.enableInlineVolume` | `true` | The cinder-operator mounts every backend and backup share as an inline `csi:` volume, and the driver serves such a volume only when its CSIDriver lists the `Ephemeral` lifecycle mode. The chart adds that mode behind this flag. `CSIDriver.spec.volumeLifecycleModes` is immutable, so a kind cluster created before this value was set carries a `CSIDriver` the chart cannot patch; `hack/deploy-infra.sh` deletes that object before it applies the overlay and the chart recreates it (see below). A fresh cluster needs nothing |

Everything else stays at the chart default: `driver.name: nfs.csi.k8s.io`,
`attachRequired: false`, `fsGroupPolicy: File` and
`kubeletDir: /var/lib/kubelet`.

When `WITH_NFS=true`, `hack/deploy-infra.sh` does four things. In kind mode it
loads `nfsd`, `nfs` and `nfsv4` on the host before the cluster is created,
best-effort through the same loader as `WITH_OVN_KERNEL_MODULES` (Linux only,
root or passwordless sudo, otherwise a warning). Under `EXTERNAL_CLUSTER=true`
it loads nothing on the host and applies the overlay's `nfs/` in place of
`deploy/kind/nfs`, whose pods load the modules on the nodes; see
[Lab NFS stack](#lab-nfs-stack). On a cluster whose
`CSIDriver/nfs.csi.k8s.io` lists no `Ephemeral` lifecycle mode it deletes that
object, because the field is immutable and the chart's patch would otherwise
be rejected for the lifetime of the cluster: a reused cluster (a second run,
or a runner keeping a warm one with `SKIP_KIND_CREATE=true`) would fail the
`csi-driver-nfs` wait below with the cause buried in the HelmRelease status.
Only a `NotFound` counts as "no such object" there; any other failed read (an
API server still settling, a denied cluster-scoped read) aborts the run rather
than skipping the check. A delete is followed by a forced `csi-driver-nfs`
reconcile with the release's failure counts reset and a hard wait for the
object to reappear, because the re-applied overlay can be identical to what
the cluster already carries and helm-controller retries neither an unchanged
release nor an upgrade whose remediation retries are spent: a cluster left
with no NFS CSI driver at all is worse than the one the delete started from.
A `NotFound` on a cluster that already carries the `csi-driver-nfs`
HelmRelease enters that same recreate path, because it is not a fresh cluster
but one an earlier run left driverless when it died between the delete and the
forced reconcile. The wait is on the recreated object's
`spec.volumeLifecycleModes`, not only on its existence: a remediation rollback
racing the delete puts the pre-`Ephemeral` object back, which an
existence-only check would accept.
In kind mode it applies `deploy/kind/nfs` in Step 3 and waits for the
`nfs-server` Deployment to roll out; a failed rollout is an error that stops
the run and names the `nfsd` module, because a CrashLooping server on a host
without `nfsd` must not end in a green summary. It appends `csi-driver-nfs`
to the Phase 3 HelmRelease wait list. All four actions are gated strictly on
the flag; the default run is unchanged.

**Opt-in usage:**

```bash
WITH_NFS=true make deploy-infra
```

The `nfs-health` suite in
[E2E Deployment](e2e-deployment.md#chainsaw-e2e-test) is the check that both
shares mount under `restricted` PodSecurity.

**Posture summary.** Same shape as the entries above: the production omission
is explicit, the opt-in flag has a single documented name (`WITH_NFS`), and
the kind overlay is self-contained under `deploy/kind/nfs/`. The
non-production posture is recorded in the header of `nfs-server.yaml`: a
privileged server,
`sec=sys` with `no_root_squash` and a wildcard client list, an amd64-only
image, and `ghcr.io/nfs-ganesha/nfs-ganesha` as the recorded fallback if a
runner kernel lacks `nfsd`. The `Ephemeral` lifecycle mode widens that posture
by one step: reaching the export no longer needs a cluster-scoped
`PersistentVolume`, so anyone who can create a Pod mounts both shares from the
pod spec alone. PodSecurity does not bound that. In a namespace that is not
`restricted` the pod mounts them as root. A `restricted` namespace admits a
`csi` volume and the UID 42424, the owner of both exports, which is how the
`nfs-health` probe mounts them. The metal-stack lab carries the same posture,
as [Lab NFS stack](#lab-nfs-stack) states.

### Message bus (kind-only opt-in)

**Files:** `deploy/kind/messaging/kustomization.yaml`,
`deploy/kind/messaging/shared-rabbitmq.yaml`

One `RabbitmqCluster` named `shared-rabbitmq` in `openstack`, for the
standalone Cinder e2e suites of
[#988](https://github.com/c5c3/cobaltcore/issues/988) and, later, the
ControlPlane suites of #989. The RabbitMQ Cluster Operator that reconciles it
is not part of the overlay: it reaches every cluster this script provisions
through the Flux Kustomization in
`deploy/flux-system/releases/rabbitmq-cluster-operator.yaml`. Only the broker
instance is opt-in, and the default `make deploy-infra` flow creates none.

Production ships no equivalent object. A production `ControlPlane` declares
`spec.infrastructure.messaging`, and the c5c3 operator projects a
`RabbitmqCluster` for it into the ControlPlane's own namespace.

| Property | Value |
| --- | --- |
| Target namespace | `openstack` (pre-existing; the overlay ships no inline `Namespace`) |
| API version | `rabbitmq.com/v1beta1` |
| Replicas | `1` |
| Requests | `100m` CPU, `512Mi` memory |
| Limits | `512Mi` memory, no CPU limit |
| Dependencies | the RabbitMQ Cluster Operator (Phase 3b) and the `rabbitmqclusters.rabbitmq.com` CRD (the Step 5 `wait_for_crds` list) |

**Sizing.** The cluster operator requests 1 CPU and 2Gi per pod by default.
That request does not fit beside the rest of the stack on the self-hosted
runner's kind node: it takes the last schedulable CPU and the next pod stays `Pending` on
`Insufficient cpu`. `tests/e2e-chaos/neutron-broker-outage` records the same
finding for its own broker. The e2e suites push little traffic through the
bus, so `100m` and `512Mi` carry them. Memory is limited at the request.
There is no CPU limit, so a busy moment goes unthrottled.

**One vhost per suite.** Every standalone Cinder e2e suite creates its own
vhost, named after the `Cinder` CR, plus a Secret `<cr-name>-messaging`
holding the matching `transport_url`. `tests/e2e/cinder/broker-vhost.sh`
creates both. Two suites on one vhost would share the RPC topics
`cinder-scheduler` and `cinder-volume.<host>@<backend>`, so a scheduler in one
suite could hand a volume to the volume service of another. Managed mode
(`spec.messaging.clusterRef`) always lands on the default vhost, so it is used
only where a single Cinder runs alone on the cluster: the tempest legs.

The c5c3 `full-controlplane-keystone` suite is the broker's second consumer. It
calls the same helper, naming its vhost after the `ControlPlane` rather than
after a `Cinder` CR, and the ControlPlane copies the transport URL out of the
resulting `controlplane-keystone-messaging` Secret into both of its bus-consuming
children, Neutron and Cinder.

**Deploy-infra wiring.** With `WITH_MESSAGING=true`, `hack/deploy-infra.sh`
applies `deploy/kind/messaging` after Step 5, where both prerequisites are
settled: the `openstack` namespace from the Step 3 base overlay and the
`rabbitmqclusters.rabbitmq.com` CRD from the Phase 3b operator wait. It then
waits for `rabbitmqcluster/shared-rabbitmq` to report `AllReplicasReady`. The
cluster operator publishes no `Ready` condition, so that is the condition to
gate on. On timeout the run stops: it prints
`kubectl describe rabbitmqcluster/shared-rabbitmq` and the events of
`shared-rabbitmq-server-0`, then exits 1.

**Opt-in usage:**

```bash
WITH_MESSAGING=true make deploy-infra
```

**Posture summary.** Same shape as the entries above: the production omission
is explicit, the opt-in flag has a single documented name (`WITH_MESSAGING`),
and the kind overlay is self-contained under `deploy/kind/messaging/`.

### Glance large-upload listener

**Files:** `deploy/kind/base/openstack-gateway.yaml`,
`deploy/kind/infrastructure/glance-upload-nip-io-tls-certificate.yaml`

**Gateway wiring.** Gateway `openstack-gw` carries a fifth HTTPS listener,
`https-glance-upload`, on the hostname `glance-upload.127-0-0-1.nip.io`. It
shares port 443 with the keystone, horizon, glance, and dizzy listeners, routed
by SNI, and terminates with its own `glance-upload-nip-io-tls` Certificate from
the `selfsigned-cluster-issuer` ClusterIssuer — the per-listener pattern the
four sibling nip.io certificates already follow. Unlike them it serves a test
suite. `tests/e2e/glance/gateway-large-upload` streams 512 MiB through the
public endpoint, while the Quick Start smoke suite already holds
`glance.127-0-0-1.nip.io`; two HTTPRoutes on one hostname with the same `/` path
prefix would race under chainsaw's parallelism, so the suite gets a hostname of
its own.

The listener and its Certificate are unconditional; the HTTPRoute is not. Like
the `https-dizzy` listener above, the hostname answers 404 until a route is
projected onto it. The Glance operator creates that route from `spec.gateway` on
the Glance CR the suite applies, and deletes it again when the suite cleans up.

## Metal-stack lab

`deploy/lab/metal-stack/` holds the manifests of the CobaltCore lab on a
metal-stack cluster, planned in
[#1138](https://github.com/c5c3/cobaltcore/issues/1138).
`deploy/flux-system/kustomization.yaml` does not reference the tree.
`hack/deploy-infra.sh` applies its `base/` and `infrastructure/` under
`EXTERNAL_CLUSTER=true` (see [Lab overlay](#lab-overlay)), and its `nfs/` as
well when `WITH_NFS=true` is set (see [Lab NFS stack](#lab-nfs-stack)); the
probe is applied by hand, and so is `controlplane/`, once the deploy has
finished (see [Lab ControlPlane](#lab-controlplane)), and after it
`hypervisor-fixtures/` and `hypervisor/` (see
[Lab hypervisors](#lab-hypervisors)). The
[Quick Start (metal-stack)](../../quick-start-metal-stack.md) is the
walkthrough that runs them in order, from a bare cluster to a migrated server
and back.

### Node probe

**File:** `deploy/lab/metal-stack/probe/kustomization.yaml`

The node probe is the prerequisite check of the lab. The
[Quick Start (metal-stack)](../../quick-start-metal-stack.md#cp-probe) has a
reader run it first, against any metal-stack cluster, before anything else is
deployed. It
is one Job, `node-probe`, that prints the node facts the lab depends on under
twelve fixed headers. It exits 0 whatever it finds: a node that lacks something
prints `absent`, `none` or `NOT FOUND`, and the Job still completes, so
`kubectl wait --for=condition=complete` returns.

The container runs privileged, because only a privileged container sees the
host's `/dev/kvm`. The host root is mounted read-only at `/host`, with
`recursiveReadOnly: IfPossible` keeping its submounts read-only where the
runtime supports it. The pod shares no host PID or network namespace and
mounts no ServiceAccount token. The script loads no module, writes nothing and
installs nothing, so the probe needs no egress beyond the image pull.
`tests/unit/deploy/metal_stack_probe_test.sh` fails when an edit adds
`nsenter`, `chroot`, `modprobe`, `insmod`, `rmmod`, `mount`, `umount`,
`sysctl`, `apt-get`, `apt`, `tee`, `dd`, `mknod` or `rm` to the script, or a
redirection other than `2>/dev/null`.

```bash
kubectl apply -k deploy/lab/metal-stack/probe
kubectl wait --for=condition=complete job/node-probe -n default --timeout=5m
kubectl logs -n default job/node-probe
kubectl delete job -n default node-probe
```

The Job runs on the node the scheduler picks. On a cluster with more than one
worker, pin it to each node in turn, with the same wait, logs and delete after
every run. The delete is needed because a Job's pod template is immutable:

```bash
yq '.spec.template.spec.nodeName = "<node>"' deploy/lab/metal-stack/probe/node-probe.yaml | kubectl apply -f -
```

`ttlSecondsAfterFinished: 3600` removes the Job, and its logs with it, an hour
after it completes. A DaemonSet would cover every node in one run, but it
never completes and has to be deleted by hand; the Job keeps each run bounded.

| Property | Value |
| --- | --- |
| Target namespace | `default`. The probe runs before `deploy/flux-system/namespaces.yaml` has created `openstack` or `shared-services`, and the metal-stack shoot enforces no PodSecurity level on `default` |
| Image | `docker.io/library/debian:bookworm-slim`, digest-pinned. A Renovate `customManager` refreshes the digest (automerged after 3 days); Renovate never moves the `bookworm-slim` codename tag, so a Debian release change is a manual edit |
| Runs as | one Job with `backoffLimit: 0`, one privileged container, the host root read-only at `/host` |
| Dependencies | none |

| Header | What it answers for the lab |
| --- | --- |
| `== kvm device` | Whether the node has `/dev/kvm`. With `== cpu` it decides `virtType: kvm` |
| `== cpu` | CPU model, count and topology, the virtualization extension, and how many CPUs carry the `vmx` or `svm` flag |
| `== loaded modules` | Which KVM, vhost, Open vSwitch, Geneve, VXLAN, bridge, NBD, multipath, NVMe/TCP and NFS modules are loaded: the NFS server (`nfsd`), the NFS client (`nfs`, `nfsv4`) and `sunrpc`. It also lists the modules Chaos Mesh NetworkChaos uses: `ip_set` with every `ip_set_*` type module, which shows the ipset types `calico-node` has loaded, `xt_set`, `sch_netem` and `sch_tbf` |
| `== module files for <kernel>` | Whether the running kernel ships `kvm`, `vhost_net`, `openvswitch`, `geneve` and the other module files, so the OVN chassis and the libvirt DaemonSet can load what they need. The five NFS files, `nfsd`, `nfs`, `nfsv4`, `lockd` and `sunrpc`, decide whether the NFS server and clients of [#1193](https://github.com/c5c3/cobaltcore/issues/1193) can use the node's kernel. The six Chaos Mesh files, `ip_set`, `ip_set_hash_ip`, `ip_set_hash_net`, `xt_set`, `sch_netem` and `sch_tbf`, decide D3 of [#1219](https://github.com/c5c3/cobaltcore/issues/1219): whether a pod can load the modules NetworkChaos needs, and which ones it has to load. A module compiled into the kernel prints `builtin` |
| `== filesystems` | Whether the kernel has registered `nfs4`, the filesystem type of an NFSv4 mount, and `nfsd`, the NFS server's control filesystem. Each prints `registered` or `not registered`, whether the code is a loaded module or compiled into the kernel. The `nfs` module registers `nfs4`, not `nfsv4`: only the `nfsv4` module file, or the load test's `nfsv4:` line, shows that the NFSv4 client code is there |
| `== nested / iommu` | The `nested` parameter of `kvm_intel` or `kvm_amd`, and the number of IOMMU groups |
| `== memory` | `MemTotal` and the hugepage reservations |
| `== disks` | The block devices, where `/var/lib` lives and how much it holds |
| `== cgroup` | The cgroup filesystem type, `cgroup2fs` on cgroup v2 |
| `== host os / binaries` | The host OS, and that no `libvirtd`, `qemu-system-x86_64`, `ovs-vswitchd` or `rpc.nfsd` is installed on the host |
| `== containerd socket` | Where the containerd socket the Chaos Mesh daemon mounts lives. Two lines test containerd's default path, `run/containerd/containerd.sock`, and the k3s one, `run/k3s/containerd/containerd.sock`, under the host's `/run`; each prints `socket`, `present, not a socket` or `absent`. The third line prints the `address` of the `[grpc]` table in the host's `/etc/containerd/config.toml`, quotes included and a trailing comment dropped. `not set` means the probe read no `address` in a `[grpc]` table: the file is missing, is an absolute symlink, which resolves inside the pod, or sets no such key. An `imports` file or containerd's `--address` flag can still move the socket, so the two socket lines are the evidence |
| `== nics` | Every host interface with its MTU and state: the uplinks, and the host end of each pod's veth (`cali*`), which carries the pod network's MTU. Neutron's `global_physnet_mtu` must not exceed the MTU of the network the Geneve tunnels run on (see [Lab ControlPlane](#lab-controlplane)) |

The values a lab-ready node shows come from the 2026-09-29 survey in
[#1138](https://github.com/c5c3/cobaltcore/issues/1138). The header comment of
`deploy/lab/metal-stack/probe/node-probe.yaml` lists them in the probe's own
output format, from a run on the survey's node on 2026-10-04 that includes the
NFS lines, the Chaos Mesh module lines and the containerd socket lines. The
survey's NIC lines show the pod's own `eth0`; the probe reads the host's sysfs
and lists the host's interfaces instead.

On the lab, the probe found the six Chaos Mesh module files on both workers
on 2026-10-04, each with a path under `/lib/modules/6.1.0-49-amd64`. So D3
of #1219 stands:
[#1221](https://github.com/c5c3/cobaltcore/issues/1221) is to load `ip_set`,
`ip_set_hash_ip`, `ip_set_hash_net`, `xt_set`, `sch_netem` and `sch_tbf`.
Loaded at the time of the run were `xt_set`, `ip_set_hash_ip`,
`ip_set_hash_net` and `ip_set`. This does not shorten the list: a reboot or a
replaced node starts without them. Both workers printed
`run/containerd/containerd.sock: socket`, so #1221 can mount
`/run/containerd/containerd.sock`, the kind overlay's value. The
[comment on #1219](https://github.com/c5c3/cobaltcore/issues/1219#issuecomment-5981396257)
holds the output of both workers.

**File:** `deploy/lab/metal-stack/probe/nfs-module-load.yaml`

The NFS module load test answers the one question of #1193 the probe cannot:
whether a pod can load the NFS modules on a lab worker. It is one Job,
`nfs-module-load` in `default`, that loads `nfsd`, `nfs` and `nfsv4` with
`modprobe`, prints each result and the `nfs4` and `nfsd` filesystem lines, and
removes what it loaded. A failed load prints `<module>: FAILED:` with
`modprobe`'s message. The Job exits 0 whatever it finds, so the result is read
from the log.

Unlike the probe, the load test is not read-only. It loads up to three modules
and their dependencies into the node's kernel and removes them again with
`rmmod`. It removes the modules its own load added: those that are new since a
`/proc/modules` snapshot taken before the load and are one of `nfsd`, `nfs`
and `nfsv4` it tried to load or one of their dependencies, as
`modprobe --show-depends` lists them. A module that prints `already loaded` is
not tried: one loaded before the run stays, and so does one another pod loaded
since the snapshot. A module outside that list, such as `vhost_net`, is
neither counted nor removed. Any other NFS module that another pod loads in
the seconds between the snapshot and the unload can be taken for the run's
own, so run the test on a node where nothing else loads NFS modules. A module
it cannot remove stays loaded until the node reboots and is named on the last
line, `still loaded: ...`; otherwise the last line is `module list as before`. The container mirrors the init
container `host-prepare` of the [lab hypervisors](#lab-hypervisors), which
loads `vhost_net` the same way: the DaemonSet's pinned
`ghcr.io/c5c3/libvirt:<tag>@sha256:<digest>`, pulled `IfNotPresent`,
privileged, as root, with the node's `/lib/modules` mounted read-only as its
one volume. Renovate moves the reference in the pull request that moves the
DaemonSet's.

The probe's `kustomization.yaml` leaves the file out of `resources`, so
`kubectl apply -k deploy/lab/metal-stack/probe` stays read-only. The load test
is applied by file, pinned to one node:

```bash
kubectl delete job -n default nfs-module-load --ignore-not-found
yq '.spec.template.spec.nodeName = "<node>"' deploy/lab/metal-stack/probe/nfs-module-load.yaml | kubectl apply -f -
kubectl wait --for=condition=complete job/nfs-module-load -n default --timeout=5m
kubectl logs -n default job/nfs-module-load
kubectl delete job -n default nfs-module-load
```

On the lab, the probe found the five NFS module files on both workers on
2026-10-03, and the load test on `shoot--df33f0b4c1--forge-group-0-666b6-qmhrg`
loaded all three modules, registered `nfs4` and `nfsd`, and removed the eleven
modules the load added. That settled the two open decisions of #1193: the NFS
server is the kernel's `nfsd` in a privileged pod, as on kind (D1), and a
privileged init container loads the modules from the node's `/lib/modules`
(D2). The
[comment on #1193](https://github.com/c5c3/cobaltcore/issues/1193#issuecomment-5969676743)
holds the output of both runs; the header comment of `nfs-module-load.yaml`
carries the load test's.

### Lab overlay

**Files:** `deploy/lab/metal-stack/base/kustomization.yaml`,
`deploy/lab/metal-stack/infrastructure/kustomization.yaml`

`hack/deploy-infra.sh` applies these two kustomizations in Steps 3 and 5 when
it deploys onto the lab. They take `deploy/kind/base` and
`deploy/kind/infrastructure` as their base, so the lab inherits what the kind
overlay patches:

- the NodePort EnvoyProxy on `31443`, which keeps the Envoy Service from
  pending as a `LoadBalancer` on a MetalLB without an address pool, and the
  twelve-listener `openstack-gw` Gateway;
- headlamp and flux-web, the optional addons of the kind base;
- the standalone OpenBao, the suspended service-operator releases at one
  replica, and the single-replica MariaDB, Memcached and Garage;
- the Flux controller requests.

The patches remove the kind pin `standard` from the OpenBao, MariaDB and
Garage volumes. The lab has a `standard` class too, so the pin would bind to it
by coincidence. These volumes and the proving `OpenBaoCluster` name no class
and bind to the cluster's default class, `premium` on `forge`. No
metrics-server or VPA release is rendered, because the platform runs both.

The base overlay also labels every namespace it renders with
`apiserver-proxy.networking.gardener.cloud/inject: disable`. The lab is a
Gardener shoot, and Gardener's `kubernetes-service-host` webhook sets
`KUBERNETES_SERVICE_HOST` in every new pod to the API server's DNS name, so
that pods bypass the node-local apiserver-proxy. The stack's NetworkPolicies
expect the address instead. openbao-operator 0.4.2 reads the variable as an IP
address when it derives the API-server egress of the policy it renders over an
`OpenBaoCluster`; a name makes the read fail, the fallback (`get` on Service
`default/kubernetes`) is outside the operator's RBAC, and the instance stays at
`APIServerNetworkReady=False` with reason `APIServerNetworkConfigurationInvalid`.
The memcached-operator's chart policy allows DNS on port 53 alone, and the
shoot's CoreDNS answers behind its Service on 8053, so that pod cannot resolve
the name and every API request times out. The label is Gardener's opt-out: the
webhook skips namespaces that carry it, so the pods keep the kubelet's value,
the `kubernetes` Service's ClusterIP, and reach the API server through the
apiserver-proxy, the same path as on kind. It is set on every namespace rather
than on the ones known to break, so the stack's network posture is one thing on
the lab.

```bash
EXTERNAL_CLUSTER=true make deploy-infra
kubectl -n envoy-gateway-system port-forward \
  "$(kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=openstack-gw -o name)" 8443:443
curl -sk https://keystone.127-0-0-1.nip.io:8443/v3
EXTERNAL_CLUSTER=true make teardown-infra
```

The deploy runs against the current kubeconfig context and never switches it.
It refuses the kind-only opt-ins (`WITH_NFS` aside, which applies `nfs/`),
checks the cluster for a default StorageClass, for the absence of a
`node-local-dns` DaemonSet (the instance's NetworkPolicy would need
`spec.network.dnsEndpointIPs` for a host-networked resolver) and for a Ready
node, and prints the port-forward command when it
completes. The teardown removes the stack in finalizer order and leaves the
platform's namespaces and CRDs alone. Both are described in
[E2E Deployment](e2e-deployment.md#make-teardown-infra), with every variable.

| Property | Value |
| --- | --- |
| Storage class | the cluster's default class; no manifest names one |
| Access | `kubectl port-forward` to the Envoy Service on local port 8443; the `*.127-0-0-1.nip.io` hostnames are unchanged |
| Platform overlap | none: no metrics-server, VPA, MetalLB pool or DNS entry |
| Gardener | `apiserver-proxy.networking.gardener.cloud/inject: disable` on every namespace of the base render |
| Dependencies | a default StorageClass and no `node-local-dns` on the cluster |

### Lab NFS stack

**Files:** `deploy/lab/metal-stack/nfs/kustomization.yaml`,
`deploy/lab/metal-stack/nfs/client-modules-daemonset.yaml`,
`deploy/lab/metal-stack/nfs/client-policy.yaml`

The NFS server and the `csi-driver-nfs` mounter of
[NFS storage stack](#nfs-storage-stack-opt-in), for the metal-stack
lab ([#1196](https://github.com/c5c3/cobaltcore/issues/1196)). Cinder's volume
and backup backends are NFS shares, so the lab runs Cinder only with this
stack. `hack/deploy-infra.sh` applies the directory in Step 3 when
`WITH_NFS=true` is set beside `EXTERNAL_CLUSTER=true`, in place of
`deploy/kind/nfs`:

```bash
EXTERNAL_CLUSTER=true WITH_NFS=true make deploy-infra
```

The kustomization takes `deploy/kind/nfs` as its base, the way the
[Lab overlay](#lab-overlay) takes the kind base, and adds the DaemonSet
`nfs-client-modules`. Its render holds six objects and no Namespace. The three
differences from kind follow the decisions D1 to D3 of
[#1193](https://github.com/c5c3/cobaltcore/issues/1193); the load test of the
[Node probe](#node-probe) settled D1 and D2. A fourth difference sits outside
the kustomization: the deploy script applies the NetworkPolicy of
`client-policy.yaml`.

| Property | Value |
| --- | --- |
| Export claim | `nfs-server-exports`, 100Gi, `ReadWriteOnce` (D3), with no storage class. The patch removes the kind pin `standard`, so the claim binds to the cluster's default class like the volumes of the [Lab overlay](#lab-overlay). D3 named `premium`, which is the default class of `forge`. One volume holds both exports, so 100Gi bounds every Cinder volume and backup of the lab together. The kind claim asks for 5Gi on `standard` |
| Server and shares | as on kind: `itsthenetwork/nfs-server-alpine:12` by digest, the Service on 2049, and the shares `nfs-server.openstack.svc.cluster.local:/volumes` and `:/backups`. The image is amd64 only, and so are both workers |
| Mounter | as on kind: the `csi-driver-nfs` HelmRelease in `kube-system`, chart `4.13.4` with its three values. The chart's `kubeletDir`, `/var/lib/kubelet`, is the lab's kubelet root |
| `nfsd` | the init container `load-nfsd` of the server pod, before `prepare-exports` (D1, D2). It runs `ghcr.io/c5c3/libvirt:<tag>@sha256:<digest>`, the pinned image of `host-prepare` in [Lab hypervisors](#lab-hypervisors), pulled `IfNotPresent`, privileged, as root, with a read-only root filesystem and the node's `/lib/modules` mounted read-only. Renovate moves the reference in the pull request that moves the DaemonSet's. In the server's own pod the load precedes the server on every start, also after a node reboot |
| `nfs` and `nfsv4` | the DaemonSet `nfs-client-modules` in `openstack`, on every node, tolerating every taint as `csi-nfs-node` does. A privileged init container `load` on the same image loads both, and an unprivileged container `hold` keeps the pod running, so the load repeats after a reboot. `csi-nfs-node` and `nova-compute` mount the shares through the node's kernel, and the chart and the nova-operator render them, so neither can carry an init container from this repository |
| Client policy | the NetworkPolicy `nfs-server-clients` in `openstack`, from the template `client-policy.yaml`. It selects the server's pods and admits one `ipBlock`, the cluster's node network, to TCP 2049. `csi-nfs-node` and `nova-compute` are host-network pods, so the node network names every client. The template carries the placeholder `NODE_NETWORK`; the deploy script replaces it with `data.nodeNetwork` of the ConfigMap `kube-system/shoot-info`, which Gardener writes into every shoot, `10.128.44.0/22` on `forge` |
| Namespaces | `openstack` for the server and `nfs-client-modules`, `kube-system` for the chart, `flux-system` for the HelmRepository |
| Gardener label | none added. The server and `nfs-client-modules` pods run in `openstack`, which the [Lab overlay](#lab-overlay) labels, and mount no ServiceAccount token. The chart's pods are host-network pods in `kube-system`, the platform's namespace, so the lab's `gardener.cloud--deny-all` NetworkPolicy there does not apply to them, and they reach the API server as the platform's own host-network pods do |
| Pinned by | `tests/unit/deploy/metal_stack_nfs_test.sh` |

In this mode the deploy script's preflight accepts `WITH_NFS=true` only for an
overlay with `nfs/kustomization.yaml` and refuses any other before it contacts
the cluster. Step 1 then refuses a cluster whose `CSIDriver/nfs.csi.k8s.io`
the HelmRelease `kube-system/csi-driver-nfs` did not install, read from its
`helm.toolkit.fluxcd.io` labels: that cluster runs its own NFS CSI driver,
which Step 3 would replace and the HelmRelease would adopt, and the teardown
would uninstall. Step 1 also reads the node network for the client policy and
logs it as `NFS client network  : <cidr>`. It refuses a cluster whose
ConfigMap `kube-system/shoot-info` is missing or names no IPv4 CIDR, and one
where a node has no IPv4 `InternalIP` inside that network, because the policy
would drop that node's mounts. The script loads no module on the machine it
runs on; it logs
`Skipping the host-side NFS kernel modules (EXTERNAL_CLUSTER=true; ...)`
instead. Step 3 runs the `CSIDriver` lifecycle-mode guard of the kind mode,
applies the client policy and then `<overlay>/nfs`, so the server never
listens without the policy, and waits up to `POD_TIMEOUT` seconds for the
`nfs-server` Deployment and then for the `nfs-client-modules` DaemonSet. A
failed wait exits 1 and names the log to read. Phase 3 waits for the
`csi-driver-nfs` HelmRelease, as on kind. The two loaders log one line per pod:

```bash
kubectl logs -n openstack deployment/nfs-server -c load-nfsd
kubectl logs -n openstack -l app.kubernetes.io/name=nfs-client-modules -c load --prefix --tail=-1
```

A pod prints `load-nfsd: nfsd is loaded` or
`nfs-client-modules: nfs and nfsv4 are loaded`. When `modprobe` fails, it
prints `cannot load <module> from /lib/modules/<kernel>` after its prefix and
exits 1, and its pod stays in `Init`.

**Posture.** The lab carries the kind posture (D1 of #1193), narrowed in one
place by the client policy. The server container is privileged, the export is
`sec=sys` with `no_root_squash` and a wildcard client list, and with the
`Ephemeral` lifecycle mode every principal that can create a pod mounts both
shares from the pod spec. PodSecurity does not bound that: in a namespace that
is not `restricted` the pod mounts them as root, and a `restricted` namespace
admits a `csi` volume and the UID 42424, the owner of both exports.
`kube-system` enforces no PodSecurity level on the lab and already runs
privileged host-network pods (`calico-node`, `lb-csi-node`), so it admits the
privileged `csi-nfs-node`. The lab is one tenant's cluster, and the Service is
a ClusterIP.

The client policy narrows one path. On kind every pod that dials 2049 reads
and writes both shares as root. On the lab only the node network reaches the
port, so a pod gets to the shares only through a volume the node mounts for
it. The policy does not narrow who can ask for such a volume. Calico routes
the lab's pod network without encapsulation, so the server sees a node's own
address, also through the Service's ClusterIP. A probe on `forge` on
2026-10-04 confirmed it: host-network clients on both workers appeared as
`10.128.44.1` and `10.128.44.3`, and under a policy with the `ipBlock`
`10.128.44.0/22` they connected while pods on either worker timed out.

**Teardown.** `EXTERNAL_CLUSTER=true make teardown-infra` removes the stack at
the end of its step 2, once the ControlPlane and its Cinder are gone and while
the helm-controller still runs (see
[E2E Deployment](e2e-deployment.md#make-teardown-infra)). While the HelmRelease
`csi-driver-nfs` exists, it waits until no pod mounts an inline
`nfs.csi.k8s.io` volume and no PersistentVolume of that driver is `Bound`,
because the kubelet unmounts one through `csi-nfs-node`; a driver the platform
runs has its pods and claims left alone. Then it
deletes the overlay, whose HelmRelease finalizer has the helm-controller
uninstall the chart, and the `CSIDriver` the release created, selected by the
two labels the helm-controller sets on every object of a release. The client
policy is deleted after the overlay, once the server it guarded is gone. The
claim goes with the overlay. Where the default class has the reclaim policy
`Delete`, as `premium` on `forge` has, its volume and every Cinder volume and
backup on it go too.

**What the pods change on a node.**

- `load-nfsd` loads `nfsd` and its dependencies on the node of the server pod,
  and `nfs-client-modules` loads `nfs`, `nfsv4` and their dependencies on every
  node. Together these are the eleven modules the load test of #1194 listed:
  `auth_rpcgss`, `dns_resolver`, `fscache`, `grace`, `lockd`, `netfs`, `nfs`,
  `nfs_acl`, `nfsd`, `nfsv4` and `sunrpc`. Nothing unloads them; they stay
  until the node reboots, also after a teardown.
- The server pod runs the kernel's NFS server threads and mounts the `nfsd`
  control filesystem inside the pod; port 2049 listens in the pod's network
  namespace.
- `csi-nfs-node` registers its socket under
  `/var/lib/kubelet/plugins/csi-nfsplugin` and
  `/var/lib/kubelet/plugins_registry` and mounts the shares below
  `/var/lib/kubelet/pods`; these go with the pods.
- While a server on a node has a volume attached, `nova-compute` holds the
  share `/volumes` mounted below `/var/lib/nova/mnt/<md5>` in the host's mount
  namespace, and unmounts it with the node's last detach.
- The deploy script itself runs no `modprobe` and writes no file on a node.

No lab run of this stack is recorded yet.

### Lab ControlPlane

**Files:** `deploy/lab/metal-stack/controlplane/kustomization.yaml`,
`deploy/lab/metal-stack/controlplane/ovncentral.yaml`,
`deploy/lab/metal-stack/controlplane/controlplane-lab.yaml`

The kustomization is the `OVNCentral` of Step 3 and the ControlPlane CR of
Step 4 of the
[Quick Start (ControlPlane)](../../quick-start-controlplane.md), and the CR
carries the `cinder` block of that page's optional `# block-storage.yaml`
fragment. The data is unchanged except for two keys the lab adds to the
ControlPlane.
`hack/deploy-infra.sh` names the directory in its `WITH_CONTROLPLANE=true`
completion hint and never applies it. Its preflight renders the directory and
refuses it unless it holds exactly one ControlPlane,
`openstack/<CONTROLPLANE_NAME>`, because Step 7 seeds the admin-password paths
of that namespace and name only. The lab CR is `openstack/controlplane`, which
the default name matches. Without `WITH_NFS=true` the preflight refuses the
directory as well, because the CR's Cinder backends `nfs1` and `nfsbk` sit on
the [Lab NFS stack](#lab-nfs-stack), which only `WITH_NFS=true` deploys.
`tests/unit/deploy/metal_stack_controlplane_test.sh` compares both files with
three blocks of the page, the first `# controlplane.yaml` block, the
`# block-storage.yaml` fragment and the `# controlplane-ovn.yaml` block, and
fails when they drift apart. The CR file is not named
`controlplane.yaml`, because `.gitignore` ignores that basename in every
directory.

| Setting | Value | Why |
| --- | --- | --- |
| `spec.sizing.profile` | `Minimal` | The quick start's profile: one replica per component and a 512Mi database volume (`operators/c5c3/api/v1alpha1/sizing_profiles.go`), which fits a worker with 16 CPUs and 128 GiB. The Lightbits CSI driver rounds a request up to its 1 GiB granularity (`getReqCapacity` in `pkg/driver/controller.go` of [LightBitsLabs/los-csi](https://github.com/LightBitsLabs/los-csi)), so the 512Mi claim binds as a 1 GiB volume. A lab that needs a value between `Minimal` and `Standard` declares a cluster-scoped `SizingProfile` with `spec.base: Minimal` and selects it with `spec.sizing.profileRef.name`, which excludes `spec.sizing.profile` |
| `spec.services.neutron.extraConfig.DEFAULT.global_physnet_mtu` | `"1460"` | Tenant networks are Geneve. Neutron 27.0.3 computes their MTU as `global_physnet_mtu` minus 20 (the IPv4 header, `get_mtu` in `neutron/plugins/ml2/drivers/type_tunnel.py`) minus `[ml2_type_geneve] max_header_size`, which the Neutron operator owns at 38 (`operators/neutron/internal/controller/reconcile_config.go`): 1460 - 20 - 38 = 1402 for every tenant network. 1460 is the pod network's MTU (the `cali*` lines of the [node probe](#node-probe)). The chassis tunnels over the node network, whose uplinks `lan0` and `lan1` carry 9000, so 1460 is a bound, not a match: it holds without a path-MTU measurement between the two racks, and none exists yet. The value is a string, because `extraConfig` is `map[string]map[string]string` |
| `spec.services.nova.hypervisorOperator` | `{}` | Provisions the Keystone user `hypervisor-operator` (project `service-hypervisor-operator`, role `admin`) and writes the Secret `controlplane-nova-hypervisor-operator-auth` into `openstack` (see [`ServiceNovaHypervisorOperatorSpec`](../c5c3/controlplane-crd.md#servicenovahypervisoroperatorspec)), which [Lab hypervisors](#lab-hypervisors) feeds into the hypervisor operator's chart |

Part 1 of the [Quick Start (metal-stack)](../../quick-start-metal-stack.md#cp-deploy)
applies the directory and checks the ControlPlane, from the deploy in its
Step 3 to the [checks](../../quick-start-metal-stack.md#cp-verify) of its Step 7.

For the hypervisor package
([#1142](https://github.com/c5c3/cobaltcore/issues/1142)) the ControlPlane
publishes the OVN central `controlplane-ovn` and three Secrets in `openstack`:
the compute contract `controlplane-nova-compute-config`, the metadata proxy
secret `controlplane-nova-metadata-secret`, and the hypervisor operator's
credentials `controlplane-nova-hypervisor-operator-auth`. The auth Secret's
`auth_url` is `https://keystone.127-0-0-1.nip.io:8443/v3`, the loopback URL the
public catalog carries, and from inside a pod it resolves to the pod itself. The
hypervisor operator of [Lab hypervisors](#lab-hypervisors) therefore takes the
in-cluster URL `http://controlplane-keystone.openstack.svc:5000/v3` for Keystone.
With `OS_INTERFACE=internal` it takes the internal compute, placement, image and
network endpoints of the catalog, which are the in-cluster Service URLs.

| Property | Value |
| --- | --- |
| Namespace | `openstack` |
| Applied | by hand, after `EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra` |
| Storage | through the cluster's default class; neither CR names a `storageClassName` |
| Metadata gateway | none; the metadata API stays in-cluster at `controlplane-nova-metadata.openstack.svc:8775` |
| Block storage | Cinder with the volume backend `nfs1` and the backup backend `nfsbk` on the shares `/volumes` and `/backups` of the [Lab NFS stack](#lab-nfs-stack); `CinderReady` reports `True` with reason `CinderReady` |
| Removed by | `EXTERNAL_CLUSTER=true make teardown-infra`, which deletes the ControlPlane and then the `OVNCentral` in its first step |
| Pinned by | `tests/unit/deploy/metal_stack_controlplane_test.sh` |

### Lab hypervisors

**Files:** `deploy/lab/metal-stack/hypervisor-fixtures/kustomization.yaml`,
`deploy/lab/metal-stack/hypervisor/kustomization.yaml` and the seven manifests
it lists, `deploy/lab/metal-stack/migration-ports/kustomization.yaml` and the
two manifests it lists

`hypervisor-fixtures/` and `hypervisor/` turn the lab's two workers into KVM
hypervisors of the [Lab ControlPlane](#lab-controlplane), planned in
[#1142](https://github.com/c5c3/cobaltcore/issues/1142). They add libvirt in a
DaemonSet, the OVN chassis, the metadata agent and a `NovaCompute` pool, and
run openstack-hypervisor-operator (hvo) and kvm-node-agent (kna) from upstream
with the settings that carry both on a Debian node under Gardener. Both are
applied by hand once the ControlPlane is `Ready`. `hypervisor/` pulls in
`migration-ports/`, which reserves QEMU's migration ports on every node and
which Part 2, Step 1 of the
[Quick Start (metal-stack)](../../quick-start-metal-stack.md#hv-nodes) applies
on its own before the [node port check](#node-port-check).
`hack/deploy-infra.sh` applies none of the three.
`tests/unit/deploy/metal_stack_hypervisor_test.sh` pins the three renders.

| File | Content |
| --- | --- |
| `hypervisor-fixtures/kustomization.yaml` | The kind fixtures of `deploy/kind/hypervisor-operator-fixtures/` without `VolumeType/hvo-premium`, `Network/hvo-smoke-test` and `Subnet/hvo-smoke-test`: only hvo's smoke test uses them, and every lab `Hypervisor` skips it through the node label `cobaltcore.cloud.sap/node-hypervisor-lifecycle=skip-tests`. The domain `cc3test` and the project `test`, which hvo scopes a token to at start, stay, and so do the flavor ID `1` (1 vCPU, 256 MiB, 1 GiB) and the image `cirros-kvm` the run boots |
| `migration-ports/namespace.yaml` | Namespace `hypervisor-system`, with the lab's Gardener opt-out label |
| `migration-ports/reservation-daemonset.yaml` | DaemonSet `migration-port-reservation` in `hypervisor-system`, on every node: it reserves QEMU's migration ports (see [Migration port reservation](#migration-port-reservation)) |
| `hypervisor/kustomization.yaml` | Takes `../migration-ports` as a resource, so the hypervisor overlay applies the namespace and the reservation too and the teardown removes both with it |
| `hypervisor/libvirt-ca.yaml` | Certificate `libvirt-migration-ca` (ECDSA 256, three years, bootstrapped from `selfsigned-cluster-issuer`) and Issuer `nova-hypervisor-agents-ca-issuer`, hvo's default issuer name, in `hypervisor-system`. The CA signs nothing else |
| `hypervisor/libvirt-configmap.yaml` | ConfigMap `libvirt-lab`: `host-prepare.sh`, `libvirtd.sh`, `libvirtd.conf` and `qemu.conf` |
| `hypervisor/libvirt-daemonset.yaml` | DaemonSet `libvirt` in `openstack` |
| `hypervisor/compute.yaml` | `OVNChassis/lab-chassis` on `controlplane-ovn`, `NeutronMetadataAgent/lab-metadata-agent` on the in-cluster Nova metadata API, and `NovaCompute/lab` with `virtType: kvm`, `cpuMode: custom`, `cpuModels: [Skylake-Server-IBRS]` and `imagesType: qcow2`, all in `openstack` |
| `hypervisor/sources.yaml` | One digest-pinned `OCIRepository` per chart in `flux-system` |
| `hypervisor/hvo-release.yaml` | `HelmRelease/openstack-hypervisor-operator` in `openstack` |
| `hypervisor/kna-release.yaml` | `HelmRelease/kvm-node-agent` in `hypervisor-system` |

The libvirt DaemonSet runs `ghcr.io/c5c3/libvirt:<tag>@sha256:<digest>` (see
[libvirt](../ci-cd/container-images.md#libvirt)) on the nodes labelled
`openstack.c5c3.io/nova-compute-pool=lab`, the pool's own label, privileged
as uid 0 in the host's network, PID and IPC namespaces. `<tag>` is the keeper
tag `<libvirt-package-version>-r<N>`, such as `10.0.0-2ubuntu8.19-r1`, which
`hack/ci-tag-libvirt-keeper.sh` mints once on `main` and never moves (see
[Release-independent images](../ci-cd/build-images-workflow.md#release-independent-images)).
`imagePullPolicy: IfNotPresent` pulls the digest once per node. Renovate
proposes each new keeper tag as one pull request, never automerged, that moves
all six lines naming the image: the two containers here, the load test of the
[Node probe](#node-probe), `load-nfsd` and both containers of
`nfs-client-modules` in the [Lab NFS stack](#lab-nfs-stack). The update
strategy is `OnDelete` and the pod has no liveness probe: a rollout, or a
restart on a slow answer, would interrupt running migrations. A merged bump
therefore reaches a node when its libvirt pod is deleted. The readiness probe
runs `virsh -c qemu:///system version`.

| Host path | Why the pod mounts it |
| --- | --- |
| `/run/libvirt` | libvirtd's sockets, which `nova-compute` and kna open |
| `/var/lib/libvirt`, `/var/lib/nova` | The domains' state and the instance disks; `/var/lib/nova` with `Bidirectional` propagation, like the `NovaCompute` pod. The NFS share `nova-compute` mounts below `/var/lib/nova/mnt` for a Cinder volume appears in the libvirt pod through this propagation |
| `/etc/pki/CA`, `/etc/pki/libvirt`, `/etc/pki/qemu` | The TLS files kna writes, read-only |
| `/dev`, `/sys/fs/cgroup`, `/lib/modules` | `/dev/kvm` and the guests' devices, their cgroups, and the module tree for `vhost_net`, read-only |
| `/run/systemd`, `/run/dbus` | The host's systemd (its private socket and `/run/systemd/system`) and its D-Bus socket, the latter read-only |

The init container `host-prepare` loads `vhost_net` and fails the pod with
`host-prepare: cannot load vhost_net from /lib/modules/<kernel>` or
`host-prepare: /dev/kvm is missing on this node`. The main container's
`libvirtd.sh` then:

1. waits, without a timeout, for `/etc/pki/CA/cacert.pem`,
   `/etc/pki/libvirt/servercert.pem` and
   `/etc/pki/libvirt/private/serverkey.pem`, and logs
   `libvirtd: waiting for the TLS files kvm-node-agent installs under /etc/pki`
   every 10 seconds. A pod that is `Running` but not `Ready` waits here;
2. copies `libvirtd.conf` and `qemu.conf` to `/etc/libvirt/`, with the node's
   address in `listen_addr`;
3. stops `cobaltcore-libvirtd.scope`, which holds a libvirtd an earlier
   container left behind when it was killed after its grace period, and
   resets its failed state: a stop that runs out leaves the scope failed, and
   a failed unit keeps its name. It then starts
   `systemd-run --collect --scope --slice=system --unit=cobaltcore-libvirtd libvirtd --listen`.
   The scope moves libvirtd out of the pod's cgroup into the host's
   `system.slice`, so the QEMU processes it forks survive a restart of the pod.
   `--collect` lets systemd unload the scope even when it ends failed;
4. waits up to 60 seconds for `/run/libvirt/libvirt-sock` while libvirtd
   lives, exits with libvirtd's status when it dies, and exits 1 with
   `libvirtd: /run/libvirt/libvirt-sock did not appear within 60s` otherwise;
5. writes two runtime units into the host's `/run/systemd/system`, reloads the
   host's systemd and starts `libvirtd.service`;
6. on `TERM` stops that unit, deletes both, reloads, and stops libvirtd. The
   domains keep running. Every other end of the script, a failed step or
   libvirtd's own exit included, deletes the units as well and stops the
   scope.

kna reads libvirt's state from the host's systemd. It reports nothing
about libvirt while `libvirtd.service` is not active, and it sets
`TLSCertificateInstalled=True` only once starting
`virt-admin-server-update-tls.service` succeeds. A Debian host without libvirt
has neither unit, so the script writes stand-ins. Each starts with the line
`# Written by the cobaltcore lab libvirt DaemonSet (openstack/libvirt); removed when its pod stops.`

| Unit | Content |
| --- | --- |
| `libvirtd.service` | `Type=simple`, `ExecStart=/usr/bin/tail --pid=<libvirtd PID> -f /dev/null`: active as long as the containerized libvirtd lives |
| `virt-admin-server-update-tls.service` | `Type=oneshot`, `ExecStartPre=/usr/bin/grep -q /cobaltcore-libvirtd.scope /proc/<libvirtd PID>/cgroup`, `ExecStart=/usr/bin/nsenter --target <libvirtd PID> --mount -- /usr/bin/virt-admin server-update-tls libvirtd`: kna starts it after every certificate write. The check fails the start once the PID has left the scope, so a unit left behind never runs as host root in another process's mount namespace |

This bends decision D1 of
[#1138](https://github.com/c5c3/cobaltcore/issues/1138), that the host is not
modified, knowingly. `/run` is a tmpfs, so a reboot removes the units, and the
script removes them whenever it ends. `systemctl` takes a container in the
host's PID namespace for a chroot and ignores `start` and `daemon-reload`
there, so the script sets `SYSTEMD_IGNORE_CHROOT=1`.

`libvirtd.conf` listens for TLS on the node's address and port `16514` only
(`listen_tcp = 0`, `auth_tls = "none"`) and gives the socket the numeric group
`+108` with mode `0770`, the group kna's chart gives its pod
(`supplementalGroups: [108]`); the agent runs as root here (below), which opens
the socket either way. `qemu.conf` runs QEMU as `root:root` without a
security driver: the host's `/dev/kvm` is `root:103`, and the image's `kvm`
group has another ID. QEMU's migration TLS reads `/etc/pki/qemu` and verifies
the peer. The kna image of this repository writes all three private keys,
`libvirt/private/serverkey.pem`, `qemu/server-key.pem` and
`ch/server-key.pem`, with mode 0600 (see
[kvm-node-agent](../ci-cd/container-images.md#kvm-node-agent)), and QEMU as
root reads its key either way. `PKI_KEY_GROUP` therefore stays unset: it gives
QEMU's key a group and mode 0640, which only a QEMU that runs as another user
needs.

`qemu.conf` also sets `stdio_handler = "file"`. libvirt's default, `logd`,
sends QEMU's output, the console log Nova reads included, through a pipe to
`virtlogd`. A `virtlogd` in the pod dies with the pod while QEMU lives on, and
`openstack console log show` then stops at the restart
([#1174](https://github.com/c5c3/cobaltcore/issues/1174)). With `file`,
libvirtd opens each log file and hands QEMU the descriptor, so the script
starts no `virtlogd`. This gives up `virtlogd`'s rollover, by default 2 MiB per
file and three backups: `/var/lib/nova/instances/<uuid>/console.log` grows for
as long as the guest writes to its console. A domain takes its handler when it
starts. libvirtd reads `qemu.conf` when it starts too, and the script copies
the file and starts libvirtd only when the container starts (steps 2 and 3).
The DaemonSet updates `OnDelete`, so applying the change switches no node. On
a lab with running servers, delete the libvirt pod of each node and wait until
it is `Ready`. Only then run `openstack server reboot --hard` on each server of
that node. A hard reboot before the pod delete starts the domain under the old
pod's `virtlogd` again, and its console log stops once more at the delete. The
status XML of a switched domain, `/run/libvirt/qemu/<instance_name>.xml`,
carries no `<chardevStdioLogd/>`.

`qemu.conf` sets `dynamic_ownership = 0` as well. libvirt adds its DAC driver
whenever QEMU runs privileged, also under `security_driver = "none"`
(`qemuSecurityInit` in `src/qemu/qemu_driver.c`). With dynamic ownership on,
that driver hands every disk of a domain to QEMU's user, `0:0` here
(`virSecurityDACSetImageLabelInternal` in `src/security/security_dac.c`). Both
were read at libvirt v9.0.0; the image installs the libvirt of Ubuntu noble.
Cinder's NFS backends keep each volume file `42424:42424` with mode `660`
(`nas_secure_file_permissions = true`, see
[Rendered backend section](../cinder/cinder-backend-crd.md#rendered-backend-section)),
and the Cinder pods read it as uid 42424. After one attach the file would
belong to root, and a backup of the volume would fail with
`Permission denied`. With the setting off, QEMU as root opens every file it
needs without a `chown`: the instance disks `nova-compute` creates as root, the
console log libvirtd opens for it, `/dev/kvm` and the TLS keys. The owner
change is read from libvirt's source, and no lab run has shown it yet.
libvirtd reads `qemu.conf` when it starts, and the DaemonSet updates
`OnDelete`, so on a running lab delete the libvirt pod of each node and wait
until it is `Ready` before the first volume attach. An attach through a pod
that predates the setting hands the volume file to `0:0` on the share, and no
later detach or pod restart gives it back. Once every libvirt pod has been
replaced, repair such a file in the NFS server pod with
`kubectl exec -n openstack deploy/nfs-server -c nfs-server -- chown 42424:42424 /exports/volumes/volume-<id>`.
A lab deployed after a teardown has the setting from the start.

The DaemonSet, the three compute CRs and hvo run in `openstack`. hvo sits there
because its release reads the ControlPlane's auth Secret through `valuesFrom`,
which reads only Secrets of the release's own namespace. The CA, its
Issuer, the per-node Certificates `libvirt-<node>` with their Secrets
`tls-libvirt-<node>`, and kna live in `hypervisor-system`: every CobaltCore
operator reads Secrets in `openstack`, and a `tls-libvirt-<node>` Secret is
root on its node's libvirtd (see
[Live migration](../nova/novacompute-crd.md#live-migration)).

| Release | Setting | Value | Why |
| --- | --- | --- | --- |
| hvo | chart | `1.2.3_sha-a2baf3f` | The upstream chart of the pinned hvo commit, the `ARG HVO_COMMIT` line of `images/openstack-hypervisor-operator/Dockerfile`. Its `appVersion`, `sha-<commit>`, is the image tag |
| hvo | `fullnameOverride` | `hypervisor-operator` | The chart names its metrics Service `<fullname>-controller-manager-metrics-service`, 64 characters with the release name as fullname, and the install fails |
| hvo | `controllerManager.manager.image.repository` | `ghcr.io/c5c3/openstack-hypervisor-operator` | The image built from that commit with its four patches: the Eviction patch, which leaves block migration to Nova, the two below that let onboarding finish, and patch 0004, which reads the catalog interface from `OS_INTERFACE` (see [openstack-hypervisor-operator](../ci-cd/container-images.md#openstack-hypervisor-operator)) |
| hvo | `valuesFrom` | six keys of `controlplane-nova-hypervisor-operator-auth` | The account the ControlPlane provisions: `password` into `secret.servicePassword`; `username`, `user_domain_name`, `project_name`, `project_domain_name` and `region_name` into the matching `controllerManager.manager.env.os*` values |
| hvo | `env.osAuthUrl` | `http://controlplane-keystone.openstack.svc:5000/v3` | The in-cluster Keystone URL, the `spec.keystoneEndpoint` of `controlplane-nova`. The auth Secret's `auth_url` is the public loopback URL |
| hvo | `env.certificateNamespace` | `hypervisor-system` | The Issuer's namespace |
| hvo | `env.agentNamespaces` | `openstack` | The namespace of the pool, chassis and metadata agent pods, which an offboarding waits for |
| hvo | `serviceMonitor.enabled`, `prometheusRules.create`, `dashboards.create`, `customResourceMetrics.create` | `false` | The lab runs no Prometheus Operator |
| hvo | post-renderer | `OS_INTERFACE=internal` on the manager | hvo takes the endpoints of `compute`, `placement`, `image` and `network` from the catalog interface `OS_INTERFACE` names (patch 0004), and the chart has no value for the variable. The internal endpoints are the in-cluster Service URLs over plain HTTP, so the pod needs neither a host alias nor the gateway certificates. Without the variable hvo reads the `public` endpoints, the `*.127-0-0-1.nip.io:8443` URLs, which resolve to the pod itself |
| hvo | post-renderer | `imagePullPolicy: Always` on the manager | The chart sets no pull policy, so a node would keep the image it cached under `sha-<commit>`. `build-images.yaml` moves that tag to every `main` build, so its content changes whenever a patch does. An image built before patch 0002 refuses the flag below, and one built before patch 0004 ignores `OS_INTERFACE` |
| hvo | post-renderer | `--default-high-availability=false` appended to the manager's `args` | The flag of the image's patch 0002: hvo creates each `Hypervisor` with `spec.highAvailability: false`. While the field is `true`, onboarding waits for `HaEnabled=True`, which only SAP's kvm-ha-service sets. The chart has no value for the flag, so a JSON patch appends it to the argument list the chart renders, and `controllerManager.manager.args` stays unset |
| kna | chart | `0.2.0_sha-1e4e4b8` | The upstream chart of the pinned kna commit, the `ARG KNA_COMMIT` line of `images/kvm-node-agent/Dockerfile`. Its `appVersion`, `sha-<commit>`, is the image tag. Upstream publishes no image under that tag, and the tag `0.2.0` would leave the private keys at 0644 |
| kna | `controllerManager.manager.image.repository` | `ghcr.io/c5c3/kvm-node-agent` | The image built from that commit with the key mode patch, which writes the private keys with mode 0600 (see [kvm-node-agent](../ci-cd/container-images.md#kvm-node-agent)) |
| kna | `controllerManager.manager.env.libvirtDefaultUri` | `qemu:///system` | The chart's default `ch:///system` is Cloud Hypervisor |
| kna | `controllerManager.manager.env.nodeLabelFieldPath` | `spec.nodeName` | The chart renders it into the field reference of `NODE_LABEL` and defines no value |
| kna | `controllerManager.manager.containerSecurityContext` | every capability dropped but `DAC_OVERRIDE`, no `runAsUser` or `runAsGroup` | The image runs as `0:0`: upstream's uid 42438 has no passwd entry on the host, and the host's dbus-daemon closes the connection of a uid it cannot resolve. Starting a unit over the system bus needs root too. The chart's init container hands the PKI directories to 42438, and root needs `DAC_OVERRIDE` to write there |
| kna | post-renderer | `NAMESPACE=hypervisor-system` | kna falls back to `monsoon3` without it, and the chart sets none |

`Hypervisor.spec.createCertManagerCertificate` stays at its default `false`, so
each node's certificate is hvo's alone.

No manifest can patch a Node, so each node gets four labels by hand:

| Label | Why |
| --- | --- |
| `openstack.c5c3.io/chassis=true` | `OVNChassis/lab-chassis` selects it |
| `openstack.c5c3.io/nova-compute-pool=lab` | `NovaCompute/lab` and the libvirt DaemonSet select it |
| `nova.openstack.cloud.sap/virt-driver=kvm` | hvo creates a `Hypervisor` and a Certificate for such a node, and kna runs there |
| `cobaltcore.cloud.sap/node-hypervisor-lifecycle=skip-tests` | The only way a `Hypervisor` gets `lifecycleEnabled` and `skipTests`. The smoke test boots onto a 64 GiB volume of the type `premium` on the network `hvo-smoke-test`, which the lab fixtures leave out |

Onboarding waits at its `Handover` phase for two more conditions, and the hvo
image of this repository patches the controller behind each. Upstream hvo sets
`TraitsUpdated` only when a custom trait differs; patch 0003 sets
`TraitsUpdated=True` when none differs, so a node without the
`nova.openstack.cloud.sap/custom-traits` annotation onboards without a custom
trait in Placement. Onboarding also waits for `HaEnabled=True` while
`spec.highAvailability` is `true`, and only SAP's kvm-ha-service sets that
condition. Upstream hvo creates every `Hypervisor` with the field `true`;
patch 0002's flag, which the release sets to `false`, makes it create the field
`false`. hvo reads the flag on create only, so a `Hypervisor` that exists keeps
its value. `EXTERNAL_CLUSTER=true make teardown-infra` still removes the
annotation, which a lab deployed from an older version of the quick start
carries.

[Part 2](../../quick-start-metal-stack.md#hypervisors) of the Quick Start
(metal-stack) runs the sequence, from the node network to an Eviction with a
Cinder volume attached and the volume's backup.

#### Checks outside the quick start

These checks need a lab on which Part 2 of the quick start has booted `lab-a`
and `lab-b`. They confirm that the images the manifests name are published,
read the value the manifests copy from the ControlPlane,
show that a server survives a restart of its libvirt pod and that its console
log keeps growing, and show that a live migration dials libvirt over TLS:

```bash
# lab-a and lab-b run (Part 2, Step 5 of docs/quick-start-metal-stack.md),
# lab-a on nodes[0]
nodes=($(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'))

# the images; the libvirt keeper tag still names the digest the DaemonSet pins
libvirt_image="$(yq '.spec.template.spec.containers[0].image' deploy/lab/metal-stack/hypervisor/libvirt-daemonset.yaml)"
[ "$(docker buildx imagetools inspect "${libvirt_image%@*}" --format '{{json .Manifest.Digest}}' | tr -d '"')" = "${libvirt_image#*@}" ]
docker manifest inspect "ghcr.io/c5c3/openstack-hypervisor-operator:sha-$(hack/ci-resolve-hvo-commit.sh)" >/dev/null
docker manifest inspect "ghcr.io/c5c3/kvm-node-agent:sha-$(hack/ci-resolve-kna-commit.sh)" >/dev/null

# the Keystone URL hvo-release.yaml carries
kubectl get nova controlplane-nova -n openstack -o jsonpath='{.spec.keystoneEndpoint}{"\n"}'

# a libvirt restart under a running server, whose console log keeps growing
kubectl delete pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${nodes[0]}"
kubectl wait pod -l app.kubernetes.io/name=libvirt -n openstack --for=condition=Ready --timeout=10m
openstack --insecure server show lab-a -c status -f value
libvirt_pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${nodes[0]}" -o name)
console_log="/var/lib/nova/instances/$(openstack --insecure server show lab-a -c id -f value)/console.log"
kubectl exec -n openstack "${libvirt_pod}" -c libvirtd -- stat -c %s "${console_log}"
kubectl exec -n openstack "${libvirt_pod}" -c libvirtd -- \
  virsh reset "$(openstack --insecure server show lab-a -c OS-EXT-SRV-ATTR:instance_name -f value)"
sleep 30
kubectl exec -n openstack "${libvirt_pod}" -c libvirtd -- stat -c %s "${console_log}"
openstack --insecure console log show lab-a | tail -n 3

# the source libvirtd logs its migration steps
kubectl exec -n openstack "${libvirt_pod}" -c libvirtd -- \
  bash -c 'virt-admin daemon-log-filters 1:qemu.qemu_migration && virt-admin daemon-log-outputs 1:stderr'
```

The restart check prints the size of `lab-a`'s console log before and after a
reset of the guest, and the second number is larger.

Then run the live migration of
[Part 2, Step 8](../../quick-start-metal-stack.md#hv-migrate) and read the
URIs the source libvirtd dialed, in the same shell:

```bash
kubectl logs -n openstack "${libvirt_pod}" -c libvirtd | grep -o 'qemu+tls://[^ ,]*' | sort -u
```

A lab that moves to the kna image of this repository in place keeps its 0644
keys: kna writes the files only when the node's Secret changes. Deleting
`tls-libvirt-<node>` makes cert-manager reissue the certificate with a new
key, which kna then writes with mode 0600. That also retires a key every local
user of the node could read. Delete the Secrets only once every node runs the
new image. kna records the `resourceVersion` it wrote in the host's
`/etc/pki/CA/.last_resource_version` and reads it back at start, so a pod of
the old image that handles the reissued Secret writes the new key with mode
0644, and the new pod then skips the Secret as unchanged:

```bash
image="ghcr.io/c5c3/kvm-node-agent:sha-$(hack/ci-resolve-kna-commit.sh)"
# the chart names the DaemonSet <release>-controller-manager
kubectl wait daemonset/kvm-node-agent-controller-manager -n hypervisor-system \
  --for=jsonpath="{.spec.template.spec.containers[?(@.name==\"manager\")].image}=${image}" \
  --timeout=10m
kubectl rollout status daemonset/kvm-node-agent-controller-manager \
  -n hypervisor-system --timeout=15m
for node in "${nodes[@]}"; do
  kubectl delete secret "tls-libvirt-${node}" -n hypervisor-system
done
kubectl wait certificate --all -n hypervisor-system --for=condition=Ready --timeout=10m
for pod in $(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt -o name); do
  kubectl exec -n openstack "${pod}" -c libvirtd -- \
    stat -c '%a %n' /etc/pki/libvirt/private/serverkey.pem /etc/pki/qemu/server-key.pem
done
```

Each key shows `600`. A key that still shows `644` has not been rewritten by a
pod of the new image; deleting its node's Secret again has that pod write it.
These commands have not run on the lab. A lab deployed after a teardown gets
new Secrets, so kna writes new keys anyway.

The block below, run on the lab on 2026-10-02, changes `cpuMode` under a
running server, once with the rollout awaited and once with a pod that
predates the change, and prints the pod's CPU keys and the server's `<cpu>`
element at each point:

```bash
# after Part 2, Steps 1 to 5 of docs/quick-start-metal-stack.md, without
# lab-a and lab-b; nodes and zone come from the opening block of Part 2

compute_selector=app.kubernetes.io/instance=lab,app.kubernetes.io/component=nova-compute
# prints the name of the nova-compute pod on nodes[0]
compute_pod() {
  kubectl get pod -n openstack -l "${compute_selector}" \
    --field-selector "spec.nodeName=${nodes[0]}" -o name
}
# waits until the operator has rendered the current spec; kubectl wait skips a
# condition whose observedGeneration is older than metadata.generation
wait_seen() {
  kubectl wait novacompute/lab -n openstack --for=condition=ConfigReady --timeout=5m
}
# waits until the nova-compute pod on nodes[0] mounts the ConfigMap of the
# current spec and its process has reported to Nova; until then a hard reboot
# keeps the old CPU or is lost. Updated At carries nova-conductor's clock and
# startedAt the node's, so it waits for Updated At to change twice while one
# container runs
wait_compute() {
  local i gen cfg pod have start updated seen base changes
  for i in $(seq 60); do
    gen=$(kubectl get novacompute lab -n openstack -o jsonpath='{.metadata.generation}')
    cfg=$(kubectl get novacompute lab -n openstack \
      -o jsonpath='{range .status.conditions[?(@.type=="ConfigReady")]}{.status} {.observedGeneration} {.message}{end}')
    pod=$(compute_pod)
    have=$(kubectl get -n openstack "${pod}" \
      -o jsonpath='{.spec.volumes[?(@.name=="pool-config")].configMap.name}')
    start=$(kubectl get -n openstack "${pod}" \
      -o jsonpath='{.status.containerStatuses[?(@.name=="nova-compute")].state.running.startedAt}')
    updated=$(openstack --insecure compute service list --service nova-compute \
      --host "${nodes[0]}" -c 'Updated At' -f value)
    if [[ "${cfg}" != "True ${gen} "* || "${cfg##* }" != "${have}" || -z "${start}" \
      || -z "${updated}" ]]; then
      seen=
    elif [[ "${seen}" != "${pod}@${start}" ]]; then
      seen="${pod}@${start}" base="${updated}" changes=0
    elif [[ "${updated}" != "${base}" ]]; then
      # the first change can be a report the old process sent before it died,
      # applied late by nova-conductor; the second is the new process's
      base="${updated}"
      (( ++changes >= 2 )) && return 0
    fi
    sleep 5
  done
  echo "wait_compute: ConfigReady '${cfg}', mounted '${have}', started '${start}'," \
    "Updated At '${updated}', changes ${changes:-0}" >&2
  return 1
}
# waits until every pod runs the current template (RollingUpdate only)
wait_pool() {
  wait_seen &&
    kubectl rollout status daemonset/lab-nova-compute -n openstack --timeout=15m &&
    kubectl wait novacompute/lab -n openstack --for=condition=DaemonSetReady --timeout=15m
}
# prints the start time and the cpu keys of the nova-compute pod on nodes[0]
pool_state() {
  local pod
  pod=$(compute_pod)
  kubectl get -n openstack "${pod}" -o jsonpath='{.status.startTime}{"\n"}'
  kubectl exec -n openstack "${pod}" -c nova-compute -- \
    grep -E '^cpu_(mode|models)' /etc/nova/compute-pool.conf.d/compute-pool.conf
}
# prints the <cpu> element of the live and of the persistent definition of cpu-a
domain_cpu() {
  local pod name
  pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
    --field-selector "spec.nodeName=${nodes[0]}" -o name)
  name=$(openstack --insecure server show cpu-a -c OS-EXT-SRV-ATTR:instance_name -f value)
  kubectl exec -n openstack "${pod}" -c libvirtd -- sh -c \
    "virsh dumpxml ${name} | sed -n '/<cpu /,/<\/cpu>/p'; echo --inactive; virsh dumpxml --inactive ${name} | sed -n '/<cpu /,/<\/cpu>/p'"
}

# A. baseline: the pool on host-passthrough, one server
kubectl patch novacompute lab -n openstack --type merge \
  -p '{"spec":{"libvirt":{"cpuMode":"host-passthrough","cpuModels":null}}}'
wait_pool; pool_state
wait_compute && openstack --insecure server create cpu-a --image cirros-kvm --flavor 1 \
  --network lab-net --availability-zone "${zone}:${nodes[0]}" --wait
domain_cpu

# B. the cpuMode change with the rollout awaited
date -u +%FT%TZ
kubectl patch novacompute lab -n openstack --type merge \
  -p '{"spec":{"libvirt":{"cpuMode":"custom","cpuModels":["Skylake-Server-IBRS"]}}}'
wait_pool; pool_state
domain_cpu                                  # before the reboot
wait_compute && openstack --insecure server reboot --hard --wait cpu-a
domain_cpu                                  # after the reboot
pool_state                                  # unchanged

# C. a hard reboot handled by a pod that predates the change
kubectl patch novacompute lab -n openstack --type merge \
  -p '{"spec":{"updateStrategy":{"type":"OnDelete"},"libvirt":{"cpuMode":"host-passthrough","cpuModels":null}}}'
wait_seen; pool_state                       # the pod did not restart
kubectl get novacompute lab -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="DaemonSetReady")].status}{"\n"}'
openstack --insecure server reboot --hard --wait cpu-a
domain_cpu
kubectl delete pod -n openstack -l "${compute_selector}"
kubectl wait novacompute/lab -n openstack --for=condition=DaemonSetReady --timeout=15m
if wait_compute; then
  pool_state                                # a new pod, the new file
  openstack --insecure server reboot --hard --wait cpu-a
fi
domain_cpu

# D. back to the manifests, and away
openstack --insecure server delete --wait cpu-a
openstack --insecure server list --all-projects
kubectl apply -k deploy/lab/metal-stack/hypervisor
kubectl patch novacompute lab -n openstack --type merge \
  -p '{"spec":{"updateStrategy":{"type":"RollingUpdate"}}}'
wait_pool
EXTERNAL_CLUSTER=true make teardown-infra
```

The lab ran the block with looser waits. `wait_seen` waited for
`status.observedGeneration`, which a pass that stops before the ConfigMap step
sets too. `wait_pool` waited for `Ready`, which also needs every compute service
of the pool up, and called `wait_compute` itself. `wait_compute` compared
`Updated At`, from nova-conductor's clock, with the pod's start time, from the
node's, and it did not check the mounted ConfigMap. Every command ran whether
the wait before it held or not. The waits as written here have not run on the
lab.

| Property | Value |
| --- | --- |
| Namespaces | `openstack` (libvirt, the compute CRs, hvo), `hypervisor-system` (the CA, the node certificates, kna, the migration port reservation), `flux-system` (the chart sources) |
| Applied | by hand: `migration-ports/`, then the node labels, then `hypervisor-fixtures/`, then `hypervisor/`, after the Lab ControlPlane is `Ready` |
| Removed by | `EXTERNAL_CLUSTER=true make teardown-infra`, step 0, labels and `maint-<node>` objects included. The node state under `/var/lib/nova`, `/var/lib/libvirt` and `/etc/pki` stays, and so do the reserved ports in `net.ipv4.ip_local_reserved_ports` until a node reboots |
| Pinned by | `tests/unit/deploy/metal_stack_hypervisor_test.sh`; the hvo chart tag follows `hack/ci-resolve-hvo-commit.sh`, and the kna chart tag `hack/ci-resolve-kna-commit.sh` |
| Dependencies | the Lab ControlPlane with `spec.services.nova.hypervisorOperator`; `/dev/kvm` and `vhost_net` on every node; TCP 16514 and 49152 to 49215 open between the nodes ([Node port check](#node-port-check)) |

The run of 2026-10-01 on the lab ran upstream's kna v0.2.0 image and chart,
with uid 0 set in the release, on two Xeon D-2141I workers on Debian 12 with
kernel 6.1. It passed every step of the sequence that is now Part 2 of the
[Quick Start (metal-stack)](../../quick-start-metal-stack.md#hypervisors) and
of the checks above, but the kna image check, which came with the image of
[#1178](https://github.com/c5c3/cobaltcore/issues/1178), and the console-log
part of the restart check, which came with
[#1174](https://github.com/c5c3/cobaltcore/issues/1174). Both ran on
2026-10-02 (below). The run also created `/var/lib/nova/instances` by hand. The
`NovaCompute` pod creates it since
[#1171](https://github.com/c5c3/cobaltcore/issues/1171). libvirtd kept its
domains across a restart
of its pod: the scope held libvirtd alone, and QEMU ran in the host cgroup
`/machine/qemu-<n>-<instance>.libvirt-qemu` that libvirt created itself. The
socket's group was `108`, libvirtd listened on `<node IP>:16514` only, and the
live migration dialed `qemu+tls://<destination IP>/system`. A teardown with a
server left stopped at the `NovaCompute` delete with the servers hint; after the
servers were deleted, two teardowns exited 0 and left no `kvm.cloud.sap` CRD,
`hypervisor-system` namespace, `maint-<node>` object, node label or host unit.

A run on 2026-10-02 applied `stdio_handler = "file"`
([#1174](https://github.com/c5c3/cobaltcore/issues/1174)) on the same two
workers. Once `lab-a` was hard-rebooted, no `virtlogd` ran on its node and its
status XML carried no `<chardevStdioLogd/>`. After a restart of the libvirt
pod, a `virsh reset` of the guest left the QEMU PID unchanged, `console.log`
grew, and `openstack console log show` returned the new boot output.

A second run on 2026-10-02 executed the
[Quick Start (metal-stack)](../../quick-start-metal-stack.md) from commit
`715eafd3`, from a bare cluster through the teardown, on the same two workers.
Every block of the page exited 0, two of them after repeats: Part 1, Step 4,
which named a repeat then, and Part 2, Step 3. The kna pods ran
`ghcr.io/c5c3/kvm-node-agent:sha-1e4e4b8e7050bbfa2d5a990aa21b849ba7b51700`
with no `runAsUser` in their pod spec and restarted once each; the run did not
read why. Both `Hypervisor` objects showed `TLSCertificateInstalled` `True`,
and the key-mode loop above printed `600` for `serverkey.pem` and
`server-key.pem` on both nodes. The `NovaCompute` pods carried the init
container `create-instances-dir`, and both servers booted with no directory
created by hand; the nodes still held `/var/lib/nova` from the earlier runs, so
the run does not show the directory created on a fresh node. The two
`lab-metadata-agent` pods had a restart count of 0 after both servers booted
and after the eviction
([#1173](https://github.com/c5c3/cobaltcore/issues/1173)). The teardown exited
0 and left neither `openstack` nor `hypervisor-system`; it waited five minutes
for the Keystone CR's backup PushSecrets
([#1186](https://github.com/c5c3/cobaltcore/issues/1186)).

hvo and kna come from SAP's own environment, and several of their defaults
assume it. [#1066](https://github.com/c5c3/cobaltcore/issues/1066) collects
what would have to change upstream. Each item with what it did on the lab:

| Item | Upstream | On the lab | Here |
| --- | --- | --- | --- |
| `maint-<node>` image | hvo's Gardener lifecycle controller creates Deployment `maint-<node>` in `kube-system` with `keppel.global.cloud.sap/ccloud-dockerhub-mirror/library/busybox:latest` | both Deployments stay `0/1`; their pods are `Pending` in `ImagePullBackOff` | none; the teardown deletes the objects |
| `kube-system` interlock | the same controller keeps a PodDisruptionBudget `maint-<node>` with `minAvailable: 1` on that Deployment, so a node drain waits until the hypervisor is offboarded | both PDBs allow 0 disruptions, on pods that never start | none |
| Operating system | kna fills `Hypervisor.status.operatingSystem` from the host's `os-release` and drives updates through `systemd-sysupdate`, both written for Garden Linux, the Gardener project's Debian-based host OS | the status carries the kernel, the firmware and the hardware, and no `version`, `prettyVersion`, `variantID` or Garden Linux field; the `VERSION` column is empty, and no update condition appears | none |
| `BlockMigration: false` | hvo's Eviction live-migrates without block migration, which Nova refuses for a server on local disks | the patched image's Eviction moved both servers off a node with local qcow2 disks, two `completed` live migrations | patched in the image ([#1163](https://github.com/c5c3/cobaltcore/issues/1163)) |
| `HaEnabled` gate | onboarding waits for `HaEnabled=True` while `spec.highAvailability` is `true`; only kvm-ha-service sets it | with `highAvailability: false` patched by hand, both Hypervisors reached `Onboarding=False`, reason `Succeeded`. The flag has not run on the lab yet | patched in the image: `--default-high-availability=false` (patch 0002), set by the release, creates each `Hypervisor` with `spec.highAvailability: false` |
| `TraitsUpdated` gate | onboarding waits for `TraitsUpdated=True`, which hvo sets only when a custom trait differs; it assigns the trait in Placement and does not create it | with the annotation, Placement answered 400 `No such trait CUSTOM_C5C3_LAB` until the trait existed. Patch 0003 has not run on the lab yet | patched in the image: `TraitsUpdated=True` when no custom trait differs (patch 0003), so the nodes carry no annotation |
| Host units | kna reads `libvirtd.service` and `openvswitch-switch.service` and starts `virt-admin-server-update-tls.service` through the host's systemd | with the stand-ins kna reports `libvirtd.service`, `LibVirtConnection` and `TLSCertificateInstalled` as `True`; `openvswitch-switch.service` stays `False`, because Open vSwitch runs in the chassis pod | runtime stand-ins written by the libvirt DaemonSet |
| Agent uid | kna's image runs as uid 42438 and authenticates to the system bus with it | the host has no such user, and its dbus-daemon drops the connection; kna exits at start. Run as uid 0 from the release, upstream's image started. The image of #1178 ran on 2026-10-02 with no `runAsUser` in the pod spec | the image of [#1178](https://github.com/c5c3/cobaltcore/issues/1178) runs as uid 0; the release keeps `DAC_OVERRIDE` |
| Key mode | kna writes every TLS file with mode 0644, the private keys included ([#1175](https://github.com/c5c3/cobaltcore/issues/1175), found in upstream's code) | not checked with upstream's image. With the image of #1178, both keys showed `600` on both nodes on 2026-10-02 | patched in the image ([#1178](https://github.com/c5c3/cobaltcore/issues/1178)): 0600, or 0640 for QEMU's and Cloud Hypervisor's keys with `PKI_KEY_GROUP` |
| `monsoon3` fallback | kna's namespace without `NAMESPACE`, which its chart does not set | with `NAMESPACE` kna installs the certificates of `hypervisor-system` | `NAMESPACE` set by a post-renderer |
| Catalog interface | hvo reads only the `public` endpoints and takes no CA | through host aliases onto an Envoy Service and `SSL_CERT_DIR`, hvo ran 35 minutes without a restart and without an `x509` or `connection refused` line. `OS_INTERFACE=internal` has not run on the lab yet | patched in the image: `OS_INTERFACE` (patch 0004), set to `internal` by the release |
| Chart object names | the chart builds names from the fullname, the release name when it contains the chart name | `openstack-hypervisor-operator` makes the metrics Service name 64 characters, and the install fails | `fullnameOverride: hypervisor-operator` |
| Hand-set node labels | hvo flags changes made with kubectl | both Hypervisors reported `Tainted=True`, reason `Kubectl`. The taint controller reads `kubectl` from the managers of the `Hypervisor`'s own managed fields (`hypervisor_taint_controller.go`), which the `kubectl patch` of `spec.highAvailability` wrote; the labels sit on the Nodes. No run without that patch has read the condition yet | none |
| Images per main commit | hvo and kna publish a chart for every main commit but no image under its tag; hvo pushes only `latest` | the chart of `a2baf3f` ran `ghcr.io/c5c3/openstack-hypervisor-operator:sha-a2baf3f…`; the kna chart of `1e4e4b8` ran `ghcr.io/c5c3/kvm-node-agent:sha-1e4e4b8…` on 2026-10-02 | the images of [#1163](https://github.com/c5c3/cobaltcore/issues/1163) and [#1178](https://github.com/c5c3/cobaltcore/issues/1178) |

The run also found what this repository's own pieces owe a real hypervisor.
The fake driver of the kind suites reaches none of it:

| Item | On the lab | Here |
| --- | --- | --- |
| Live-migration CPU check | with `cpuMode: host-passthrough`, and with `host-model`, every live migration ends in `NoValidHost`: Nova's pre-check on the destination fails with `Unacceptable CPU info: CPU doesn't have compatibility`, although `virsh hypervisor-cpu-compare` there accepts the guest CPU | `cpuMode: custom` with `Skylake-Server-IBRS`, the host-model of both workers |
| Console log after a libvirt restart | with libvirt's default `stdio_handler = "logd"`, `openstack console log show` stopped at the restart: QEMU's log went through `virtlogd`, which ran in the old pod. The guest and its network kept running. With `file` the log grows across a restart | `stdio_handler = "file"` in `qemu.conf` ([#1174](https://github.com/c5c3/cobaltcore/issues/1174)) |
| CPU model change under a server | on 2026-10-01 a server booted with `host-passthrough` kept that CPU through a hard reboot after the pool moved to `custom`; the run recorded no time for the reboot. The cpuMode block above ran on 2026-10-02 on `nodes[0]`. A, after the boot: the pod's file says `cpu_mode = host-passthrough`, the domain `mode='host-passthrough'`. B, before the reboot: a pod started 10 seconds after the patch, its file says `cpu_mode = custom` and `cpu_models = Skylake-Server-IBRS`, the domain is still `host-passthrough`. B, after the reboot: the same pod, and the live and the `--inactive` `<cpu>` say `mode='custom'` with the model `Skylake-Server-IBRS`. C, first reboot: the pod of B with its `custom` file, `DaemonSetReady` `False`, the domain `mode='custom'` while the CR says `host-passthrough`. C, second reboot: a new pod, its file says `cpu_mode = host-passthrough` and no `cpu_models`, the domain `mode='host-passthrough'`. A first pass of the same run sent C's second reboot 3 seconds after the new pods started, before `nova-compute` took requests: it cleared the reboot's task state at start-up, the reboot action ended in `Error`, `--wait` reported success and the domain stayed `custom`. `wait_compute` comes from that pass | [Changing the libvirt settings of a pool with servers](../nova/novacompute-crd.md#changing-the-libvirt-settings-of-a-pool-with-servers) |

### Node port check

**File:** `hack/lab-node-ports.sh`

Live migration between the lab's hypervisors
([#1142](https://github.com/c5c3/cobaltcore/issues/1142)) needs libvirt's TLS
port `16514` and the QEMU migration range `49152`-`49215` open from every node
to every other node on the node network. The script proves that on the
current context's cluster. It starts one listener Pod per node (perl, one
socket per port on `0.0.0.0`) and one client Pod per ordered node pair on the
source node (bash `/dev/tcp` to the destination's first InternalIP, the
address `status.hostIP` and so the NovaCompute `live_migration_inbound_addr`
resolve to), all host-networked and
unprivileged: no capability, no privilege escalation, UID 65534 on a read-only
root filesystem, no ServiceAccount token. It runs on the node probe's pinned
debian image, which carries `bash`, `perl` and `timeout`, so it has no image or
Renovate pin of its own. Every Pod is labelled
`app.kubernetes.io/name=lab-node-ports`. A run first deletes the Pods an
earlier run left behind, so their logs are never read as its results, and an
EXIT trap deletes the Pods on every path.

```bash
hack/lab-node-ports.sh
NODE_PORTS_TCP=16514 hack/lab-node-ports.sh
```

It prints one line per ordered pair, then a summary:

```text
worker-a (10.128.44.10) -> worker-b (10.128.44.11): 65/65 open
worker-b (10.128.44.11) -> worker-a (10.128.44.10): 63/65 open, closed: 16514 49215
```

A port is `closed` when the connect fails, `listener bind failed` when the
destination's listener could not bind it, and `no result` when the client
produced no line for it before the client wait ran out. The migration range
sits in Linux's ephemeral port range, so an outgoing connection on the node can
hold one of its ports. `deploy/lab/metal-stack/migration-ports` reserves the
range on every node, which keeps new connections off it. The listener tries a
port it cannot bind again every second for `NODE_PORTS_BIND_TIMEOUT` seconds
before it reports it. A port a connection took before the reservation stays
`listener bind failed` and fails the run, which then prints a `NOTE:` line
saying the port was not tested; restarting the process that holds the port
frees it.

| Variable | Default | Description |
| --- | --- | --- |
| `NODE_PORTS_TCP` | `16514 49152-49215` | Space-separated ports and inclusive ranges (65 ports by default) |
| `NODE_PORTS_NAMESPACE` | `default` | Namespace of the Pods |
| `NODE_PORTS_IMAGE` | the `image:` of `deploy/lab/metal-stack/probe/node-probe.yaml` | Image with `bash`, `perl` and `timeout`; the check exits 2 when it is unset and the probe manifest is missing |
| `NODE_PORTS_NODE_SELECTOR` | empty (every node) | Label selector limiting the nodes |
| `NODE_PORTS_CONNECT_TIMEOUT` | `5` | Seconds per connect |
| `NODE_PORTS_BIND_TIMEOUT` | `10` | Seconds the listener keeps retrying a port it cannot bind, once per second. The wait for the listeners' `listening` line adds it to `NODE_PORTS_POD_TIMEOUT` |
| `NODE_PORTS_POD_TIMEOUT` | `120` | Seconds for the listener waits and for removing an earlier run's Pods. The client wait adds `NODE_PORTS_CONNECT_TIMEOUT` per port, because a client probes its ports one after another and a firewall that drops the packets makes every probe take the full connect timeout |

| Exit code | Meaning |
| --- | --- |
| `0` | Every port of every ordered pair is open |
| `1` | At least one port is closed, has no result, or was not bound by the listener within `NODE_PORTS_BIND_TIMEOUT` |
| `2` | Usage or cluster error: a missing tool or image, an invalid variable, fewer than two nodes, a node without an InternalIP or whose first one is IPv6 (the listeners bind IPv4 only) or not an address, Pods of an earlier run that cannot be deleted, Pods that cannot be created, or listeners that never became Ready |

#### Migration port reservation

**Files:** `deploy/lab/metal-stack/migration-ports/kustomization.yaml`,
`namespace.yaml` and `reservation-daemonset.yaml` beside it

The kernel picks the local port of an outgoing connection from
`net.ipv4.ip_local_port_range`, `32768` to `65535` on the lab's nodes, which
holds the migration range. A host-network process can therefore get a
migration port for a connection that lasts for days: on shoot `forge`, `calico-node` held
`49152` for its connection to the API server, and the check answered
`listener bind failed: 49152` on every run. libvirt skips a migration port it
cannot bind, so migrations go on; the check cannot test the port.

The DaemonSet `migration-port-reservation` in `hypervisor-system` runs on
every node. Its init container `reserve` adds `49152-49215` to
`net.ipv4.ip_local_reserved_ports`, which the kernel leaves out when it picks a
local port; a bind to a named port, QEMU's and the check's, is not affected. It
keeps a value the node already carries. The setting belongs to the host's
network namespace and `/proc/sys` is writable only in a privileged container,
so the pod uses the host's network and that container is privileged. The second
container, `hold`, only keeps the pod running, as UID 65534 without a
capability: the setting is gone after a reboot, and the init container then
runs again. Both run the node probe's pinned debian image, and Renovate moves
the pins in one group.

Part 2, Step 1 of the
[Quick Start (metal-stack)](../../quick-start-metal-stack.md#hv-nodes) applies
the directory in front of the check. The hypervisor overlay takes it as a
resource, so `EXTERNAL_CLUSTER=true make teardown-infra` removes the DaemonSet
with that overlay. The reserved ports stay on a node until it reboots.

The reservation closes no connection. One that held a port of the range before
it keeps the port, and the check then still reports `listener bind failed`.
The init container names such a socket and the process that owns it, which is
what the pod's host PID namespace is for:

```bash
kubectl logs -n hypervisor-system -l app.kubernetes.io/name=migration-port-reservation \
  -c reserve --prefix --tail=-1
```

```text
[pod/migration-port-reservation-5gm99/reserve] reserved: 49152-49215 (before: none)
[pod/migration-port-reservation-5gm99/reserve] in use: port 49152 by calico-node (pid 3798)
[pod/migration-port-reservation-bjjzt/reserve] reserved: 49152-49215 (before: none)
[pod/migration-port-reservation-bjjzt/reserve] in use: none
```

`--tail=-1` keeps every line: with a label selector, `kubectl logs` prints only
the last 10 lines of each pod, which can drop a pod's `reserved:` line and its
first holders. `kubectl get pod -n hypervisor-system -o wide` maps each pod to
its node. A socket no process holds prints `by an unknown owner`. Restarting
the named process frees its port, and its next connection gets a port outside
the range. For a process a DaemonSet runs, delete its pod on that node, as for
`calico-node` on `forge`:

```bash
kubectl delete pod -n kube-system -l k8s-app=calico-node --field-selector spec.nodeName=<node>
```

The lines describe the moment the pod started. Delete the reservation's pod on
the node to read them anew.

