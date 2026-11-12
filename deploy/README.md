# Deploy CobaltCore

Choose an entry point for your workflow. The [base catalog](bases/README.md)
provides reusable Kustomize packages; environment overlays and deployment
scripts supply the settings, ordering and bootstrap work for a running stack.

| I want to… | Start here |
| --- | --- |
| Run a local development stack | [kind](kind/README.md), using `make deploy-infra` |
| Run the metal-stack lab | [metal-stack](lab/metal-stack/README.md), using `EXTERNAL_CLUSTER=true make deploy-infra` |
| Compose my own deployment repository | [Reusable bases](bases/README.md) and [GitOps stages](gitops/README.md) |
| Customize sizing or placement | [Examples](examples/README.md) |
| Initialize or configure shared OpenBao | [OpenBao bootstrap and policies](openbao/README.md) |
| Attach a target cluster | [Target-cluster access](target-cluster/README.md) |
| Migrate an existing manifest import | [Migration notes](MIGRATION.md) |

Run commands from the repository root. The detailed
[ControlPlane quick start](../docs/quick-start-controlplane.md) and
[metal-stack quick start](../docs/quick-start-metal-stack.md) include prerequisites
and the steps after infrastructure installation.

## What the directories contain

| Directory | Responsibility |
| --- | --- |
| [`bases/`](bases/README.md) | Five public packages: operators, TLS, shared OpenBao, the ESO store and the ControlPlane operator |
| [`kind/`](kind/README.md) | Local development settings, optional addons and test fixtures |
| [`lab/`](lab/README.md) | Settings for existing lab clusters; metal-stack reuses kind definitions with its own overrides |
| [`openbao/`](openbao/README.md) | Executable bootstrap scripts and HCL policies; Kustomize can package them as ConfigMaps |
| [`target-cluster/`](target-cluster/README.md) | A Helm chart granting management operators access to another cluster |
| [`examples/`](examples/README.md) | Customization examples to inspect and adapt |
| [`flux-system/`](flux-system/README.md), [`gitops/`](gitops/README.md), [`eso/`](eso/README.md) | Existing aggregate, staged and store entry points retained for compatibility |

The broad `flux-system` aggregate is a reference composition, not a complete
production profile. Storage, availability, exposure, bootstrap custody and
authorization still need environment choices. The Garage reference is intended
for development/CI. Flux runtime bootstrap retains its existing raw manifest
entry point, and static database/cache/Garage resources remain in the existing
infrastructure composition; they are not additional public catalog bases.

## Rendering, reconciliation and bootstrap

A local `kustomization.yaml` (`kustomize.config.k8s.io`) composes YAML. A Flux
`Kustomization` (`kustomize.toolkit.fluxcd.io`) continuously reconciles a source
path and has its own dependencies, health checks and pruning policy. Rendering
the former does not perform the latter's work or wait for operators and CRDs.

The checked-in FluxInstance installs Flux controllers through flux-operator; it
does not configure a Git sync source. A deployment repository supplies that
source and its reconciliation stages.

`make deploy-infra` runs the ordered workflow in
[`hack/deploy-infra.sh`](../hack/deploy-infra.sh):

1. Create kind or check the existing external cluster, then install flux-operator
   and wait for the FluxInstance and APIs.
2. Apply the environment's `base/` and selected addons, respecting operator
   ownership and suspension settings.
3. Wait for cert-manager admission, apply the early TLS prerequisites, and wait
   for the remaining controllers.
4. Apply `infrastructure/` after its CRDs exist. In ControlPlane mode, omit the
   static resources that the c5c3 operator will own.
5. Wait for shared OpenBao to run, initialize/unseal it, configure engines, auth
   and policies, seed bootstrap data, and synchronize secrets.
6. Finish dependent infrastructure and optional fixture setup. Apply the
   ControlPlane and onboard its database tenant according to the chosen quick
   start.

The [deployment reference](../docs/reference/infrastructure/e2e-deployment.md)
describes the exact phases and flags. A single recursive apply is not a
replacement for these readiness gates. In particular, an OpenBao bootstrap stage
must be able to run before the readiness that initialization/unseal establishes.

The shared/bootstrap OpenBao server, OpenBao operator and development proving
instance have different roles; [their relationship](openbao/README.md) explains
the apparent second OpenBao configuration. ESO likewise has a controller, a
shared store, and separate tenant stores.

When composing GitOps stages, give each resource one owner. Do not reconcile
both the broad aggregate and the staged bases over the same objects. Preserve
namespace and stateful-resource retention policies when changing imports; see
[migration notes](MIGRATION.md).
