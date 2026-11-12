# Deploy CobaltCore

Shared resources live in `bases/`; each environment composes them under
`overlays/`. Kind and metal-stack share the same bases and keep their own
gateway, storage and host settings. Run commands from the repository root; use the linked operational
guides for complete installation and teardown procedures.

| Area | Purpose and entry point |
| --- | --- |
| [Deployment order](#deployment-order) | Prerequisites, reconciliation and bootstrap gates |
| [Reusable bases](#reusable-bases) | `bases/`: canonical manifests, bootstrap assets and access chart |
| [Customization](#customization-and-examples) | `examples/`: controller sizing, service placement and storage patches |
| [kind](#kind-development-environment) | `overlays/kind/`: local development with `make deploy-infra` |
| [metal-stack](#metal-stack-and-external-environments) | `overlays/metal-stack/`: an existing cluster with `EXTERNAL_CLUSTER=true` |
| [Addons](#addons-and-generated-inputs) | Optional monitoring, storage and test fixtures |
| [OpenBao and ESO](#openbao-and-eso) | `bases/shared-openbao/`: scripts/policies; `bases/eso-store/`: shared store |
| [Target-cluster access](#target-cluster-access) | `bases/target-cluster-access/`: access chart for projected workloads |
| [Path migration](#path-migration) | Updating imports from the previous layout |

```text
deploy/
  bases/
    flux/  operators/  tls/  eso-store/  controlplane-operator/
    shared-openbao/           # HelmRelease, bootstrap scripts and policies
    infrastructure/          # reference database, cache and object-store instances
    target-cluster-access/   # Helm chart for projected workloads
    devstack/                # shared development controller/instance stages
    chaos-mesh/  dizzy/  nfs/  prometheus/  messaging/  hypervisor-fixtures/
  overlays/
    kind/                    # local gateway, storage and development fixtures
    metal-stack/             # public gateway, Gardener, Ceph and physical hosts
  examples/                  # compositions and patches to copy
```

## Deployment order

A local `kustomization.yaml` composes YAML. A Flux `Kustomization` reconciles a
source with its own readiness and pruning policies. Rendering does not install
chart workloads or wait for CRDs. The checked-in
[FluxInstance](bases/flux/fluxinstance.yaml) installs Flux through flux-operator;
it has no Git sync configured. A deployment repository supplies the source,
Flux reconciliation stages, environment patches and bootstrap execution.

Preserve these lifecycle gates when composing the [bases](#reusable-bases):

1. Install flux-operator and wait for the FluxInstance and Flux APIs.
2. Reconcile sources/operators; wait for the required CRDs and admission paths.
3. Deliver usable TLS before workloads need its certificate Secrets.
4. Deliver shared OpenBao, then initialize/unseal it and configure engines,
   auth, policies and bootstrap data.
5. Validate the ESO store and synchronize required secrets. Reconcile c5c3
   after its operator, K-ORC and bootstrap prerequisites.
6. Submit the ControlPlane, onboard its database tenant, then wait for final
   ControlPlane readiness.

Bootstrap must run before the unsealed OpenBao readiness it establishes;
database onboarding must precede the final ControlPlane readiness that needs
those credentials. During teardown, uninstall Prometheus before removing its
shared CRD release so Helm can still resolve the resources it deletes.
K-ORC is a Flux Kustomization, so its dependency requires a
stage gate rather than a HelmRelease-only `dependsOn` list.

[`make deploy-infra`](../hack/deploy-infra.sh) orchestrates kind/external-cluster
setup, the environment's `base/`, early TLS, CRD-dependent `infrastructure/`,
OpenBao bootstrap and optional fixtures. Its ControlPlane mode filters out
static resources that c5c3 will own. Follow the
[deployment reference](../docs/reference/infrastructure/e2e-deployment.md) for
exact phases, flags and waits; a recursive apply of this tree is not equivalent.

## Reusable bases

Each [base](bases/) owns a distinct stage. Preview one with
`kubectl kustomize deploy/bases/<name>` under the default load restrictions.

| Base | Owns | Prerequisites |
| --- | --- | --- |
| [operators](bases/operators/) | Namespaces, Helm/OCI/Git sources and infrastructure/service operator reconciliation objects | Flux controllers and APIs |
| [tls](bases/tls/) | Self-signed issuer, OpenBao certificates and database/OVN trust issuers | Namespaces and ready cert-manager APIs, controller and admission webhook |
| [shared-openbao](bases/shared-openbao/) | HelmRelease `shared-services/openbao`, retaining HA/three Raft replicas and mTLS, with no storage-class pin | Source, namespace and usable TLS; separate initialization/unseal |
| [eso-store](bases/eso-store/) | Shared `ClusterSecretStore/openbao-cluster-store` | ESO, TLS and initialized/auth-configured shared OpenBao |
| [controlplane-operator](bases/controlplane-operator/) | c5c3 operator HelmRelease; no ControlPlane instance | Infrastructure/service operators, K-ORC APIs and bootstrap prerequisites |

The files live directly inside their owning bases. `operators` includes
[namespaces](bases/operators/namespaces.yaml), [sources](bases/operators/sources/)
and [controller releases](bases/operators/releases/); it excludes FluxInstance,
the shared OpenBao server and c5c3. [flux](bases/flux/) owns the Flux runtime CR.
[shared-openbao/bootstrap](bases/shared-openbao/bootstrap/) and
[shared-openbao/policies](bases/shared-openbao/policies/) package bootstrap inputs
separately from the server release, preserving their lifecycle gates.
[infrastructure](bases/infrastructure/) composes TLS, ESO and the reference
MariaDB, Memcached and Garage instances after their CRDs exist.

Use one reconciler per resource identity. The operators stage protects Namespaces
with `kustomize.toolkit.fluxcd.io/prune: disabled`; that does not protect Helm
releases or their data from uninstall. Preserve TLS issuer/Secret identities and
make stateful-resource retention explicit when changing owners or removing stages.

The environment overlays share [`bases/devstack/base`](bases/devstack/base/)
for development controllers/sizing and [`bases/devstack/infrastructure`](bases/devstack/infrastructure/)
for small instances and secret shims. These retain the development defaults;
they are separate from the five reusable production-building-block stages above.
Shared addon bases hold each release/source and generated dashboard once.
Environment overlays provide their gateway resources, storage classes, persistence,
namespace metadata and node preparation. For example, the lab never imports
kind's loopback certificates or NodePort proxy and then deletes them.

## Customization and examples

Create an overlay in your deployment repository. For an overlay at `operators/`
and a checkout vendored at `vendor/cobaltcore`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../vendor/cobaltcore/deploy/bases/operators
```

Alternatively, use a reviewed full commit from the fork, replacing the placeholder:

```yaml
resources:
  - https://github.com/stefanm8/cobaltcore//deploy/bases/operators?ref=FULL_COMMIT_SHA
```

Keep all stages on one revision. A Git pin does not freeze chart version ranges;
record resolved charts/images for upgrades. Patch specific HelmRelease
`spec.values` or Flux Kustomization `spec.patches`/`spec.images`, preserving source
references. A blanket namespace transform can break cross-namespace references.

The independent [base-customization examples](examples/base-customization/)
change Keystone controller resources and storage for a **new** shared OpenBao
installation, without copied releases or generated inputs:

```sh
kubectl kustomize deploy/examples/base-customization/operators
kubectl kustomize deploy/examples/base-customization/shared-openbao
```

Replace `example-fast-storage` and the illustrative sizing with site choices.
Existing PVC changes require a migration. One replica alone does not reproduce
the development OpenBao profile, which also changes Raft/service registration.

The [sizing overlay](examples/sizing-overlay/) patches operators, Flux sizing
and a PriorityClass; its [infrastructure phase](examples/sizing-overlay/infrastructure/)
patches static database/cache resources after their operators exist. To size
ControlPlane-managed services, patch their ControlPlane/service CRs instead of
installing competing static resources. See
[sizing and placement overrides](../docs/reference/infrastructure/infrastructure-manifests.md#sizing-and-placement-overrides).
Examples are templates to inspect and adapt; `make deploy-infra` does not apply
the examples directory or supply their deployment-repository orchestration.

## kind development environment

[`overlays/kind/`](overlays/kind) configures a local development/CI stack. Start with
`make deploy-infra`, or `WITH_CONTROLPLANE=true make deploy-infra` to prepare
ControlPlane operators. `WITH_CONTROLPLANE_CR=true` additionally requests the
bundled CR; `CONTROLPLANE_OPERATORS=external` lets development/CI provide builds.
Follow the [ControlPlane quick start](../docs/quick-start-controlplane.md) for
ports, prerequisites, CR submission and database onboarding.

[`base/`](overlays/kind/base) supplies sources, releases, development sizing and gateway/UI
resources after Flux is ready. [`infrastructure/`](overlays/kind/infrastructure) supplies
certificates, static instances, secret shims and the proving OpenBao instance
after their CRDs exist. Shared OpenBao runs standalone here with TLS/mTLS and no
Kubernetes service registration. Do not independently apply the full static
infrastructure overlay over ControlPlane-owned resources; preserve the script's
mode-specific filtering and suspension behavior.

## metal-stack and external environments

[`overlays/metal-stack/`](overlays/metal-stack) adapts the shared bases to a metal-stack/Gardener
cluster, with distinct namespace labels, default-class storage, gateway,
networking, persistent metrics and optional Ceph/physical-hypervisor settings.
It is not a generic production profile. Follow the
[metal-stack quick start](../docs/quick-start-metal-stack.md), then run:

```sh
EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_CEPH=true make deploy-infra
```

The script creates no kind cluster and defaults `EXTERNAL_OVERLAY` to
`deploy/overlays/metal-stack`. It prepares infrastructure; apply the lab ControlPlane
separately as the quick start describes. The platform supplies metrics-server
and VPA, so the kind-specific flags do not substitute for that setup.

A custom `EXTERNAL_OVERLAY` must retain `base/`, `infrastructure/`, the gateway
certificate template and child directories required by enabled flags. Relative
roots resolve from the repository; absolute roots also work. The lab's gateway
certificate, NFS client policy and Dizzy soak Job/scenario are script inputs with
runtime substitutions, not resources to recursively apply. See the
[lab reference](../docs/reference/infrastructure/infrastructure-manifests.md#lab-overlay)
for environment details and teardown.

## Addons and generated inputs

The [infrastructure reference](../docs/reference/infrastructure/infrastructure-manifests.md)
and [complete overlay table](../docs/reference/infrastructure/e2e-deployment.md#kustomize-overlay-structure)
describe every addon/fixture and its caller.

| Area | Selection and ownership |
| --- | --- |
| `overlays/{kind,metal-stack}/{chaos-mesh,dizzy,nfs}` | Matching `WITH_*` flags after the base; lab variants add environment-specific storage/node preparation |
| `overlays/kind/prometheus-managed` and `overlays/metal-stack/prometheus` | `WITH_PROMETHEUS=true`; omit Namespace/monitoring already owned by the base |
| [kind/prometheus](overlays/kind/prometheus) | Compatible standalone root including its Namespace; shares chart/dashboard definitions with the managed variant |
| `overlays/kind/{metrics-server,vpa}` | Kind measurement opt-ins; VPA also requires metrics-server |
| [bases/messaging](bases/messaging) | `WITH_MESSAGING=true` after infrastructure, in either cluster mode; `overlays/kind/messaging` remains a manual entry point |
| `overlays/metal-stack/ceph` and `ceph/cluster` | `WITH_CEPH=true`: operator first, then cluster and credential handoff |
| ControlPlane, compute and diagnostic fixtures | Manual entry points include kind `fake-compute`/`hypervisor-operator-fixtures` and lab `controlplane-nfs`, `hypervisor-fixtures`, `hypervisor`, `migration-ports`, `probe`; `dizzy-soak` uses `make dizzy-soak-start` |

Prometheus and Dizzy require real dashboard files before a direct render. Their
`WITH_*` paths stage them automatically; prepare inputs alone with:

```sh
make stage-prometheus-dashboard
hack/dizzy.sh stage-dashboards
kubectl kustomize deploy/overlays/kind/prometheus-managed
```

Both environments consume the dashboard inputs staged once under
`bases/prometheus` and `bases/dizzy`. Keep default Kustomize load
restrictions. The external overlay's `prometheus/` interface remains unchanged;
the managed variant requires a prior owner of Namespace/monitoring and the usual
Flux, chart-source and cert-manager prerequisites.

## OpenBao and ESO

The apparently duplicated OpenBao definitions have distinct lifecycles:

| Role | Definition and responsibility |
| --- | --- |
| Shared/bootstrap server | [HelmRelease](bases/shared-openbao/helmrelease.yaml) in `shared-services`: bootstrap material, auth/policies, tenant secrets and dynamic database credentials |
| OpenBao operator | [Separate controller](bases/operators/releases/openbao-operator.yaml) for `OpenBaoCluster` and `OpenBaoTenant` resources |
| Development proving instance | [Shared development instance](bases/devstack/infrastructure/openbao-instance.yaml), selected by both environments: separate static-seal server for Barbican integration |
| Dedicated service instances | Created by the [c5c3 operator](../operators/c5c3/internal/controller/reconcile_barbican_openbao.go) at runtime in service namespaces |

“Management OpenBao” names the shared server's role: the split devstack places
it and ESO on the target cluster. The instance operator does not manage that
Helm installation. The proving instance adopts its unseal key from the shared
backend through ESO before being unpaused.

[`bases/shared-openbao/bootstrap/`](bases/shared-openbao/bootstrap/) contains executable scripts;
[`bases/shared-openbao/policies/`](bases/shared-openbao/policies/) holds HCL policies. After initialization/
unseal, the script workflow runs `setup-secret-engines.sh`, `setup-auth.sh`,
`setup-policies.sh`, then `write-bootstrap-secrets.sh`. Auth precedes policies
that substitute its mount accessor. Database and ESO tenant onboarding are
separate. See the [bootstrap reference](../docs/reference/infrastructure/openbao-bootstrap.md)
for direct execution, environment variables, key/token custody and recovery.

The two directories also render `cobaltcore-openbao-scripts` and
`cobaltcore-openbao-policies` ConfigMaps with content-derived suffixes. They create
no Jobs: the environment supplies execution, RBAC, credentials, mounts and
readiness checks. Compose Jobs and generators together so generated-name
references update. Scripts need adjacent `common.sh`; policy setup needs adjacent
policies or `POLICIES_DIR`. Bootstrap must be runnable while OpenBao is sealed.

[`bases/eso-store/`](bases/eso-store/) exposes only the shared store, not the ESO
controller or application ExternalSecrets. It uses mTLS plus Kubernetes auth as
`eso-management`. Its `openstack`/`shared-services` allow-list is an authorization
boundary; ControlPlane traffic uses separate tenant stores. Preserve the store
identity and coordinate endpoint/trust/auth changes with bootstrap. Wait for
validation and secret synchronization before admitting consumers; deleting the
store breaks refresh rather than cleaning up secrets.

## Target-cluster access

The [target-cluster-access chart](bases/target-cluster-access/) creates
the target's access identity/token, RBAC, optional namespaces and order-resource
CRDs. It neither installs the complete stack nor registers the target with
management. Follow the [target-cluster guide](../docs/guides/deploy-to-a-target-cluster.md)
to install it, assemble its kubeconfig, register the management Secret and place
a workload. `namespaces`/`assignedNamespaces` grants must match registration;
use `createNamespaces=false` for namespaces already owned elsewhere.

[Chart values](bases/target-cluster-access/values.yaml) define optional
grants. Package with `make helm-package-target-cluster`; keep intentional CRD
copies synchronized with `make verify-crd-sync`. Existing targets require the
explicit CRD upgrade procedure in the [target-cluster reference](../docs/reference/target-clusters.md).

## Path migration

The old `flux-system/`, `openbao/`, `eso/`, `gitops/` and `target-cluster/`
directories are removed. Update deployment-repository imports and raw-file callers:

| Previous location | Canonical location |
| --- | --- |
| `flux-system/fluxinstance.yaml` | `bases/flux/fluxinstance.yaml` |
| `flux-system/namespaces.yaml`, `sources/`, `releases/` | `bases/operators/namespaces.yaml`, `sources/`, `releases/` |
| `flux-system/releases/openbao/` | `bases/shared-openbao/` |
| `flux-system/releases/c5c3-operator/` | `bases/controlplane-operator/` |
| `flux-system/infrastructure/` and its `tls/` | `bases/infrastructure/` and `bases/tls/` |
| `openbao/bootstrap/`, `openbao/policies/` | `bases/shared-openbao/bootstrap/`, `bases/shared-openbao/policies/` |
| `eso/` | `bases/eso-store/` |
| `target-cluster/target-cluster-access/` | `bases/target-cluster-access/` |
| `gitops/bases/{operators,tls,openbao,secrets,controlplane-operator}` | `bases/{operators,tls,shared-openbao,eso-store,controlplane-operator}` |
| `kind/`, `lab/metal-stack/` | `overlays/kind/`, `overlays/metal-stack/` |

The old broad Flux aggregate has no single replacement: compose the staged bases
with the [deployment order](#deployment-order), as the development composition and
sizing example do. Keep one owner per resource when transferring Flux inventories;
prevent pruning by the old owner until the new owner has reconciled successfully.
The reusable shared OpenBao stage leaves storage-class selection to the environment.

Workload identities and bootstrap keys stay unchanged. Bootstrap scripts and
policies reference the canonical paths throughout; their generated ConfigMap
hashes change with those comment updates. Repository scripts, tests, CI, Renovate
and chart packaging use the same paths. The custom `EXTERNAL_OVERLAY`
child-directory contract remains unchanged.
