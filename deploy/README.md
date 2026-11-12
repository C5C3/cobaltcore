# Deploy CobaltCore

This directory contains reusable bases, environment overlays, bootstrap inputs
and examples. Run commands from the repository root; use the linked operational
guides for complete installation and teardown procedures.

| Area | Purpose and entry point |
| --- | --- |
| [Deployment order](#deployment-order) | Prerequisites, reconciliation and bootstrap gates |
| [Reusable bases](#reusable-bases) | `bases/`: five stages for your deployment repository |
| [Customization](#customization-and-examples) | `examples/`: controller sizing, service placement and storage patches |
| [kind](#kind-development-environment) | `kind/`: local development with `make deploy-infra` |
| [metal-stack](#metal-stack-and-external-environments) | `lab/`: an existing cluster with `EXTERNAL_CLUSTER=true` |
| [Addons](#addons-and-generated-inputs) | Optional monitoring, storage and test fixtures |
| [OpenBao and ESO](#openbao-and-eso) | `openbao/` scripts/policies and `eso/` shared store |
| [Target-cluster access](#target-cluster-access) | `target-cluster/`: access chart for projected workloads |
| [Compatibility](#legacy-entry-points-and-compatibility) | `flux-system/`, `gitops/` and migration of existing imports |

## Deployment order

A local `kustomization.yaml` composes YAML. A Flux `Kustomization` reconciles a
source with its own readiness and pruning policies. Rendering does not install
chart workloads or wait for CRDs. The checked-in
[FluxInstance](flux-system/fluxinstance.yaml) installs Flux through flux-operator;
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
those credentials. K-ORC is a Flux Kustomization, so its dependency requires a
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

`operators` excludes FluxInstance, the shared OpenBao server and c5c3. It keeps a
three-object compatibility filter over the legacy aggregate because Kustomize's
default restrictions prevent directly importing the existing raw namespace file
from a new directory. This avoids copying that file or breaking raw callers.
The [source](flux-system/sources/kustomization.yaml) and
[controller](flux-system/releases/kustomization.yaml) catalogs are direct lists;
add controllers there and namespaces to [namespaces.yaml](flux-system/namespaces.yaml).

Use one reconciler per resource identity. Choose the stages or the legacy
aggregate for an installation. The operators stage protects Namespaces with
`kustomize.toolkit.fluxcd.io/prune: disabled`; that does not protect Helm releases
or their data from uninstall. Preserve TLS issuer/Secret identities and make
stateful-resource retention explicit when changing owners or removing stages.

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

[`kind/`](kind/) configures a local development/CI stack. Start with
`make deploy-infra`, or `WITH_CONTROLPLANE=true make deploy-infra` to prepare
ControlPlane operators. `WITH_CONTROLPLANE_CR=true` additionally requests the
bundled CR; `CONTROLPLANE_OPERATORS=external` lets development/CI provide builds.
Follow the [ControlPlane quick start](../docs/quick-start-controlplane.md) for
ports, prerequisites, CR submission and database onboarding.

[`base/`](kind/base/) supplies sources, releases, development sizing and gateway/UI
resources after Flux is ready. [`infrastructure/`](kind/infrastructure/) supplies
certificates, static instances, secret shims and the proving OpenBao instance
after their CRDs exist. Shared OpenBao runs standalone here with TLS/mTLS and no
Kubernetes service registration. Do not independently apply the full static
infrastructure overlay over ControlPlane-owned resources; preserve the script's
mode-specific filtering and suspension behavior.

## metal-stack and external environments

[`lab/metal-stack/`](lab/metal-stack/) adapts kind to an existing metal-stack/Gardener
cluster, with distinct namespace labels, default-class storage, gateway,
networking, persistent metrics and optional Ceph/physical-hypervisor settings.
It is not a generic production profile. Follow the
[metal-stack quick start](../docs/quick-start-metal-stack.md), then run:

```sh
EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_CEPH=true make deploy-infra
```

The script creates no kind cluster and defaults `EXTERNAL_OVERLAY` to
`deploy/lab/metal-stack`. It prepares infrastructure; apply the lab ControlPlane
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
| `kind/{chaos-mesh,dizzy,nfs}` and lab equivalents | Matching `WITH_*` flags after the base; lab variants add environment-specific storage/node preparation |
| `kind/prometheus-managed` and `lab/metal-stack/prometheus` | `WITH_PROMETHEUS=true`; omit Namespace/monitoring already owned by the base |
| [kind/prometheus](kind/prometheus/) | Compatible standalone root including its Namespace; shares chart/dashboard definitions with the managed variant |
| `kind/{metrics-server,vpa}` | Kind measurement opt-ins; VPA also requires metrics-server |
| [kind/messaging](kind/messaging/) | `WITH_MESSAGING=true` after infrastructure, in either cluster mode |
| `lab/metal-stack/ceph` and `ceph/cluster` | `WITH_CEPH=true`: operator first, then cluster and credential handoff |
| ControlPlane, compute and diagnostic fixtures | Manual entry points include kind `fake-compute`/`hypervisor-operator-fixtures` and lab `controlplane-nfs`, `hypervisor-fixtures`, `hypervisor`, `migration-ports`, `probe`; `dizzy-soak` uses `make dizzy-soak-start` |

Prometheus and Dizzy require real dashboard files before a direct render. Their
`WITH_*` paths stage them automatically; prepare inputs alone with:

```sh
make stage-prometheus-dashboard
hack/dizzy.sh stage-dashboards
kubectl kustomize deploy/kind/prometheus-managed
```

Lab variants inherit these generated inputs. Keep default Kustomize load
restrictions. The external overlay's `prometheus/` interface remains unchanged;
the managed variant requires a prior owner of Namespace/monitoring and the usual
Flux, chart-source and cert-manager prerequisites.

## OpenBao and ESO

The apparently duplicated OpenBao definitions have distinct lifecycles:

| Role | Definition and responsibility |
| --- | --- |
| Shared/bootstrap server | [HelmRelease](flux-system/releases/openbao/helmrelease.yaml) in `shared-services`: bootstrap material, auth/policies, tenant secrets and dynamic database credentials |
| OpenBao operator | [Separate controller](flux-system/releases/openbao-operator.yaml) for `OpenBaoCluster` and `OpenBaoTenant` resources |
| Development proving instance | [kind instance](kind/infrastructure/openbao-instance.yaml), inherited by the lab: separate static-seal server for Barbican integration |
| Dedicated service instances | Created by the [c5c3 operator](../operators/c5c3/internal/controller/reconcile_barbican_openbao.go) at runtime in service namespaces |

“Management OpenBao” names the shared server's role: the split devstack places
it and ESO on the target cluster. The instance operator does not manage that
Helm installation. The proving instance adopts its unseal key from the shared
backend through ESO before being unpaused.

[`openbao/bootstrap/`](openbao/bootstrap/) contains executable scripts;
[`openbao/policies/`](openbao/policies/) holds HCL policies. After initialization/
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

[`eso/`](eso/) and `bases/eso-store` expose only the shared store, not the ESO
controller or application ExternalSecrets. It uses mTLS plus Kubernetes auth as
`eso-management`. Its `openstack`/`shared-services` allow-list is an authorization
boundary; ControlPlane traffic uses separate tenant stores. Preserve the store
identity and coordinate endpoint/trust/auth changes with bootstrap. Wait for
validation and secret synchronization before admitting consumers; deleting the
store breaks refresh rather than cleaning up secrets.

## Target-cluster access

The [target-cluster-access chart](target-cluster/target-cluster-access/) creates
the target's access identity/token, RBAC, optional namespaces and order-resource
CRDs. It neither installs the complete stack nor registers the target with
management. Follow the [target-cluster guide](../docs/guides/deploy-to-a-target-cluster.md)
to install it, assemble its kubeconfig, register the management Secret and place
a workload. `namespaces`/`assignedNamespaces` grants must match registration;
use `createNamespaces=false` for namespaces already owned elsewhere.

[Chart values](target-cluster/target-cluster-access/values.yaml) define optional
grants. Package with `make helm-package-target-cluster`; keep intentional CRD
copies synchronized with `make verify-crd-sync`. Existing targets require the
explicit CRD upgrade procedure in the [target-cluster reference](../docs/reference/target-clusters.md).

## Legacy entry points and compatibility

[`flux-system/`](flux-system/) remains the broad aggregate of namespaces,
FluxInstance, sources, operators, shared OpenBao and c5c3.
[`infrastructure/`](flux-system/infrastructure/) adds TLS, static MariaDB/MaxScale,
Memcached, the development/CI Garage reference and shared ESO store after CRDs
exist. These references retain environment assumptions, including storage defaults;
“production” in older comments distinguishes them from kind sizing, not a complete
production profile. Scripts retain their early TLS ordering.

[`gitops/bases/`](gitops/bases/) keeps existing staged imports as aliases:

| Existing import | Public import |
| --- | --- |
| `gitops/bases/operators` | `bases/operators` |
| `gitops/bases/tls` | `bases/tls` |
| `gitops/bases/openbao` | `bases/shared-openbao` |
| `gitops/bases/secrets` or `eso` | `bases/eso-store` |
| `gitops/bases/controlplane-operator` | `bases/controlplane-operator` |

For a path-only migration, pin a revision containing the new base and compare
rendered identities and fields. Keep patches, Flux reconciler/source identities,
namespace prune protection and retention policies unchanged. Preserve the staged
OpenBao base's absence of a storage-class pin. The broad aggregate has no
one-for-one staged replacement: splitting its ownership is a separate migration.
When transferring an object between Flux owners, prevent the old inventory from
pruning it, remove it there, reconcile/verify the new owner, then restore the
intended retention policy.

Existing raw manifests, bootstrap scripts/policy keys, chart paths and environment
interfaces remain supported. Directory aliases alone cannot preserve raw-file
callers. Separate Flux runtime/static-instance bases, physical environment moves,
helper consolidation and bootstrap/storage changes remain deferred; future moves
must update scripts, teardown, CI, tests, Renovate, generated-input staging and docs
together. No removal date is assigned to compatibility paths.
