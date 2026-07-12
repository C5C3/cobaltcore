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
    │   ├── c5c3-charts.yaml              C5C3 shared OCI chart registry
    │   ├── k-orc.yaml                    K-ORC (OpenStack Resource Controller) Helm chart registry
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
    │   ├── keystone-operator.yaml        Keystone Operator (from c5c3-charts)
    │   ├── k-orc.yaml                    K-ORC OpenStack Resource Controller
    │   ├── c5c3-operator.yaml            c5c3-operator ControlPlane orchestrator (from c5c3-charts)
    │   └── chaos-mesh.yaml               Chaos Mesh (kind-only addon — see "Kind Overlay Demo Addons")
    └── infrastructure/                   CRD-dependent infrastructure resources
        ├── kustomization.yaml            Infrastructure kustomize overlay
        ├── cluster-issuer.yaml           Self-signed ClusterIssuer (requires cert-manager CRDs)
        ├── db-ca-issuer.yaml             OpenStack DB CA Certificate + ClusterIssuer
        ├── mariadb.yaml                  MariaDB Galera cluster for OpenStack (with TLS)
        └── memcached.yaml                Memcached cluster for OpenStack
```

All YAML files carry the SPDX Apache-2.0 license header (3 lines: copyright, blank
comment, license identifier).

## Namespaces

Ten `Namespace` resources are defined in `namespaces.yaml` and included as the first
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
| `keystone-system` | Keystone Operator controller (workload CRs continue to live in `openstack`) |
| `openstack` | Infrastructure instance CRs (MariaDB cluster, Memcached cluster) |
| `openbao-system` | OpenBao HA Raft cluster |
| `c5c3-system` | c5c3-operator controller; the `ControlPlane` and its child CRs are created in the `ControlPlane`'s own namespace |
| `orc-system` | K-ORC (OpenStack Resource Controller) and its installer resources |

The `chaos-mesh` namespace is **not** part of the production base. It is created
inline by the kind-only opt-in overlay at `deploy/kind/chaos-mesh/` when
`WITH_CHAOS_MESH=true make deploy-infra` is used. See
[Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in) below.

**Note:** The `install.createNamespace: true` setting on HelmReleases instructs FluxCD's
helm-controller to create namespaces when installing charts. However, this does not help
when applying HelmRelease CRs via `kubectl apply -k` — the target namespace must already
exist for the API server to accept namespaced resources. The explicit `Namespace` resources
solve this chicken-and-egg problem.

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
from Git add a `spec.sync` block on top of this base.

**Kustomize ordering.** Kustomize applies `Namespace` resources first by default, so
`flux-system` exists before the `FluxInstance` is created. The flux-operator itself is
installed out-of-band by `hack/deploy-infra.sh` (pinned `FLUX_OPERATOR_VERSION`,
applied via `kubectl apply -f install.yaml`) before this kustomization is applied.

## HelmRepository Sources

Six HelmRepository CRs define the Helm chart registries that FluxCD pulls from. All
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

**K-ORC is sourced from Git, not Helm.** K-ORC publishes no Helm chart (its
`github.io` page serves no Helm index), so `sources/k-orc.yaml` is a `GitRepository`
— still `source.toolkit.fluxcd.io/v1`, in `flux-system`, polling at `interval: 1h`
— pinned to the upstream release tag `v2.6.0` and scoped to `/dist` via `spec.ignore`.
It is applied by a Flux `Kustomization`, not a HelmRelease; see
[K-ORC (OpenStack Resource Controller)](#k-orc-openstack-resource-controller).

The `chaos-mesh` HelmRepository ships in the kind-only opt-in overlay at
`deploy/kind/chaos-mesh/source.yaml` — it is intentionally absent
from `deploy/flux-system/{sources,kustomization.yaml}`. See
[Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in).

The `c5c3-charts` and `prometheus-community` repositories are OCI-type sources
(`spec.type: oci`). `c5c3-charts` hosts internally-built operator charts (e.g.,
memcached-operator) in the GitHub Container Registry. `prometheus-community` hosts
Prometheus community charts (e.g., prometheus-operator-crds). All other repositories
use standard HTTPS Helm registries.

## HelmRelease Operators

Nine HelmRelease CRs deploy the infrastructure operators and CRD charts (K-ORC is
applied separately via a Flux `Kustomization` — see
[K-ORC (OpenStack Resource Controller)](#k-orc-openstack-resource-controller)). All use
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
├── openbao               dependsOn: cert-manager
├── keystone-operator     dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets
└── c5c3-operator         dependsOn: keystone-operator, external-secrets, mariadb-operator, memcached-operator
```

K-ORC is **not** in this graph: it is applied by a Flux `Kustomization`, not a
HelmRelease, and a HelmRelease `dependsOn` can only reference other HelmReleases. The
c5c3-operator therefore does **not** `dependsOn` K-ORC even though K-ORC is a **hard
dependency**: `SetupWithManager` `Owns` the K-ORC kinds, so the manager only starts
once those CRDs are installed (until then the pod restarts), and converges once they
appear.

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
| `startupapicheck.enabled` | `false` | Disable startup API check job |

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
[OpenBao bootstrap reference](./openbao-bootstrap.md#setup-eso-tenantsh) and the
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

### OpenBao

**File:** `deploy/flux-system/releases/openbao.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `openbao-system` |
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
*after* the transport-layer admission gate. See
`architecture/docs/09-implementation/09-openbao-deployment.md` for design
rationale.

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
| `openbao-client-tls` | `openbao-client-tls` (`openbao-system`) | OpenBao pods — Raft `retry_join` + in-pod `bao` exec | StatefulSet volume `client-tls` mounted at `/openbao/client-tls`; env vars `VAULT_CLIENT_CERT` / `VAULT_CLIENT_KEY` in every exec wrapper (`deploy/openbao/bootstrap/common.sh`, `init-unseal.sh`, `hack/deploy-infra.sh`) |
| `eso-openbao-client-tls` | `eso-openbao-client-tls` (`openbao-system`) | ESO `ClusterSecretStore/openbao-cluster-store` | `spec.provider.vault.tls.certSecretRef` / `keySecretRef` in `deploy/eso/clustersecretstore.yaml`; `auth.kubernetes` block (mountPath `kubernetes/management`, role `eso-management`) is unchanged — mTLS is purely transport-layer |

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
| Source | `k-orc` `GitRepository` (tag `v2.6.0`) |
| Path | `./dist` |
| Dependencies | None |

K-ORC (the OpenStack Resource Controller) installs the declarative Keystone resource
CRDs — `ApplicationCredential`, `Service`, `Endpoint`, and related kinds — that the
c5c3-operator drives to project a `ControlPlane`'s desired state into Keystone.

K-ORC ships no Helm chart, so it is applied as a Flux `Kustomization` over the upstream
release manifest rather than a HelmRelease. The `GitRepository` source vendors `./dist`
from the pinned tag `v2.6.0`; `dist/install.yaml` there is byte-identical to the
published `install.yaml` release asset. The path carries no `kustomization.yaml`, so the
kustomize-controller generates one over `dist/install.yaml` and applies it verbatim
(`prune: true`, `wait: true`). The installer already declares the `orc-system`
Namespace and namespaces every resource into it, so no `spec.targetNamespace` is set.
The short, stable name `k-orc` (not the upstream `openstack-resource-controller`) keeps
diagnostics and cross-references terse.

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
contract see the upstream design chapter
`architecture/docs/09-implementation/08-c5c3-operator.md`.

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
`sources/`. This table shows the mapping:

| HelmRelease | `sourceRef.name` | HelmRepository file |
| --- | --- | --- |
| `cert-manager` | `cert-manager` | `sources/cert-manager.yaml` |
| `prometheus-operator-crds` | `prometheus-community` | `sources/prometheus-community.yaml` |
| `mariadb-operator-crds` | `mariadb-operator` | `sources/mariadb-operator.yaml` |
| `mariadb-operator` | `mariadb-operator` | `sources/mariadb-operator.yaml` |
| `external-secrets` | `external-secrets` | `sources/external-secrets.yaml` |
| `memcached-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |
| `openbao` | `openbao` | `sources/openbao.yaml` |
| `keystone-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |
| `c5c3-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |

`k-orc` is not in this table: it is a Flux `Kustomization` whose `sourceRef` is the
`k-orc` `GitRepository` (`sources/k-orc.yaml`), not a HelmRelease backed by a
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
  [`dbCAIssuerName`](https://github.com/c5c3/forge/blob/main/operators/keystone/internal/controller/reconcile_databasetls.go)
  hard-codes the same string (`"openstack-db-ca-issuer"`), so a rename here MUST be
  matched in the operator.

**Apply ordering.** This manifest is also applied out-of-band from the infrastructure
kustomization by `hack/deploy-infra.sh` (Phase 2, alongside `cluster-issuer.yaml` and
`openbao-tls-cert.yaml`) so that MariaDB has the issuer available the moment it tries
to render its server certificate. The infrastructure kustomization still references
`db-ca-issuer.yaml` so subsequent `kubectl apply -k` runs are idempotent.

For the end-to-end TLS path the issuer participates in, see the
[Enable Keystone Database TLS](../../guides/enable-keystone-database-tls.md) how-to.

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
| `spec.tls.clientCertIssuerRef` | `openstack-db-ca-issuer` (ClusterIssuer, `cert-manager.io`) | Trust client certs minted by the same DB CA (the Keystone operator issues its DB-client keypair from this issuer; see [reconcile_databasetls.go](https://github.com/c5c3/forge/blob/main/operators/keystone/internal/controller/reconcile_databasetls.go)) |
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
[`tests/e2e/keystone/database-tls/chainsaw-test.yaml`](https://github.com/c5c3/forge/blob/main/tests/e2e/keystone/database-tls/chainsaw-test.yaml)
(asserts `SHOW STATUS LIKE 'Ssl_cipher'` reports a non-empty cipher).

To turn the path on for a `Keystone` CR, follow the
[Enable Keystone Database TLS](../../guides/enable-keystone-database-tls.md) guide.

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

**OpenBao policy** — `deploy/openbao/policies/push-app-credentials.hcl`

This policy grants the write path for each ControlPlane's admin credential
PushSecret. Because the admin credential path is keyed per
ControlPlane (`openstack/keystone/{namespace}/{name}/admin/app-credential`), the
grants template the namespace and name as two `+/+` segments. The pre-existing
mid-path grant `kv-v2/data/openstack/*/app-credential`
matches only a single mid-segment (`openstack/<svc>/app-credential`) and therefore does
**not** cover the four-segment per-CR shape. Rather than
widening that glob, the policy adds two per-ControlPlane `+/+` grants:

| Path | Capabilities | Purpose |
| --- | --- | --- |
| `kv-v2/data/openstack/keystone/+/+/admin/app-credential` | `create`, `update`, `read`, `delete` | Write (and soft-delete on teardown) each ControlPlane's admin Application Credential `clouds.yaml` data leaf (the two `+` segments are its namespace and name) |
| `kv-v2/metadata/openstack/keystone/+/+/admin/app-credential` | `create`, `update`, `read` | Allow ESO's Vault provider to write `custom_metadata` on the KV-v2 PushSecret (a data-only grant 403s on the metadata PUT and the PushSecret never reaches Ready) |
| `kv-v2/data/openstack/keystone/+/+/service-accounts/+` | `create`, `update`, `read`, `delete` | Write (and soft-delete on teardown) each declared service account's password `clouds.yaml` data leaf; the three `+` segments are the ControlPlane's namespace, its name, and the account name (each a DNS-1123 label, so a single `+` leaf suffices) |
| `kv-v2/metadata/openstack/keystone/+/+/service-accounts/+` | `create`, `update`, `read` | Allow ESO's Vault provider to write `custom_metadata` on the service-account KV-v2 PushSecret |

A `+` matches exactly one path segment, so even though the namespace and name vary
per ControlPlane the grant still terminates at the literal `/admin/app-credential`
leaf and admits no deeper or sibling paths. Read coverage needs no widening:
eso-management's read-only `kv-v2/data/openstack/keystone/*` trailing wildcard
already covers every per-CR `+/+/admin/app-credential` leaf. Both grants stay
scoped to the per-ControlPlane `+/+` admin-credential leaves, adding no blast
radius beyond admin app-credentials. For the mTLS transport gate and the
`openbao-cluster-store` auth path these manifests ride on, see
[OpenBao Bootstrap Procedure](./openbao-bootstrap.md).

## Kustomization

Deployment is split into two kustomize overlays to separate base resources from
CRD-dependent infrastructure resources:

### Base Kustomization

**File:** `deploy/flux-system/kustomization.yaml`

The base kustomization uses `apiVersion: kustomize.config.k8s.io/v1beta1` and includes
namespaces, the FluxInstance CR, HelmRepository sources, and HelmRelease operators.
These resources do not depend on any custom CRDs.

**Resource count:** 19 files producing 28 Kubernetes resources.

| Category | Count | Resources |
| --- | --- | --- |
| Namespace | 10 | cert-manager, mariadb-system, external-secrets, monitoring, memcached-system, keystone-system, openstack, openbao-system, c5c3-system, orc-system |
| FluxInstance | 1 | flux (drives the flux-operator) |
| HelmRepository | 6 | cert-manager, mariadb-operator, external-secrets, openbao, c5c3-charts, prometheus-community |
| GitRepository | 1 | k-orc |
| HelmRelease | 9 | cert-manager, prometheus-operator-crds, mariadb-operator-crds, mariadb-operator, external-secrets, memcached-operator, openbao, keystone-operator, c5c3-operator |
| Kustomization | 1 | k-orc |
| **Total** | **28** | |

The `chaos-mesh` HelmRepository, HelmRelease, and Namespace ship in the
kind-only opt-in overlay at `deploy/kind/chaos-mesh/` and are not
counted here.

### Infrastructure Kustomization

**File:** `deploy/flux-system/infrastructure/kustomization.yaml`

The infrastructure kustomization includes CRD-dependent resources that require their
operator CRDs to be installed first. This kustomization must be applied after the base
kustomization and after operators have finished installing their CRDs.

**Resource count:** 4 manifests producing 6 Kubernetes resources (the
`db-ca-issuer.yaml` manifest declares two resources: a CA Certificate and the
CA-type ClusterIssuer that signs from it).

| Category | Count | Resources |
| --- | --- | --- |
| ClusterIssuer | 3 | `selfsigned-cluster-issuer`, `openstack-db-ca-issuer`, `openbao-ca-issuer` (all require cert-manager CRDs) |
| Certificate | 2 | `openstack-db-ca`, `openbao-ca` — both CA keypair Secrets in the `cert-manager` namespace, signed by `selfsigned-cluster-issuer` |
| MariaDB | 1 | `openstack-db` (requires mariadb-operator CRDs; TLS enabled per [MariaDB Galera Cluster](#mariadb-galera-cluster)) |
| Memcached | 1 | `openstack-memcached` (requires memcached-operator CRDs) |
| **Total** | **6** | |

<!-- NOTE: count excludes openbao-tls-cert.yaml and the ../../eso overlay that
the infrastructure kustomization also references. Those resources are documented
in their own reference pages (reference/infrastructure/openbao-bootstrap.md and
the ESO reference docs); a full audit of the kustomization resource list is out
of scope here. -->


## Deployment

### Step 1: Apply base resources

```bash
kubectl apply -k deploy/flux-system/
```

This applies 28 resources: 10 namespaces, 1 FluxInstance, 7 sources
(6 HelmRepository + 1 GitRepository for K-ORC), 9 HelmRelease operators, and
1 Kustomization (K-ORC). FluxCD resolves the dependency graph between
HelmReleases and installs operators in the correct order. Wait for all operators to
finish installing before proceeding to step 2.

### Step 2: Apply infrastructure resources

```bash
kubectl apply -k deploy/flux-system/infrastructure/
```

This applies the CRD-dependent resources: the `selfsigned-cluster-issuer`
ClusterIssuer, the `openstack-db-ca-issuer` ClusterIssuer plus its backing CA
`Certificate`, the `openbao-ca-issuer` ClusterIssuer plus
its backing CA `Certificate`, the MariaDB Galera cluster, and the
Memcached cluster. These resources require CRDs that are installed by the operator
HelmReleases in step 1. If CRDs are not yet available, the apply will fail — wait
for the operators to finish installing and retry.

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

## Extensibility

The manifest structure is designed for straightforward extension. Adding a new operator
(e.g., OpenBao) requires four steps:

1. **Add a source file** in `sources/` (e.g., `sources/openbao.yaml`) — or reuse an
   existing HelmRepository if the chart is in a shared registry
2. **Add a release file** in `releases/` (e.g., `releases/openbao.yaml`) with the
   HelmRelease CR, `dependsOn` for cert-manager, and the standard install/upgrade settings
3. **Add both paths** to the `resources` list in `kustomization.yaml`
4. **Add the operator namespace** to `namespaces.yaml` (e.g., `openbao-system`) so the
   namespace exists before `kubectl apply -k` creates the namespaced HelmRelease CR

Infrastructure instance CRs (e.g., a new database or cache cluster) follow the same
pattern: add a file in `infrastructure/` and list it in
`infrastructure/kustomization.yaml`.

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
internally-built charts to `oci://ghcr.io/c5c3/charts` (see
`architecture/docs/09-implementation/07-ci-cd-and-packaging.md`).

### c5c3-operator and K-ORC design source

The upstream design for the c5c3-operator, K-ORC, and the admin-credential lifecycle
documented above lives in the `architecture/` git submodule:

- `architecture/docs/09-implementation/08-c5c3-operator.md` — the c5c3-operator `ControlPlane` reconciler contract
- `architecture/docs/03-components/01-control-plane/05-korc.md` — the K-ORC component and chart constraint
- `architecture/docs/05-deployment/01-gitops-fluxcd/01-credential-lifecycle.md` — the restricted admin Application Credential lifecycle

These chapters are the authoritative design source. They are **updated upstream only**
and reach this repository through a submodule pointer bump — they are **not** edited from
this repository or worktree. Treat any divergence between these chapters and
the manifests above as a drift to reconcile at the source, not by editing the submodule
in place.

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
[Enable Keystone Operator Metrics](../../guides/enable-keystone-operator-metrics.md)
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
[`document-intentional-environment-divergence-in-overlays`](https://github.com/c5c3/forge/blob/main/.planwerk/review_patterns/document-intentional-environment-divergence-in-overlays.md)
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

The overlay's sole consumer is the
[Autoscaling (HPA) recipe](../../guides/advanced-configuration.md#autoscaling-hpa):
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
