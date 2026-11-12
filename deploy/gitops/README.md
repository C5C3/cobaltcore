# Staged GitOps consumption

Use the [public base catalog](../bases/README.md) for new deployment repositories.
The existing `deploy/gitops/bases` paths remain supported as compatibility entry
points; [migration notes](../MIGRATION.md) map them to the catalog.

| Existing stage | Public base | Responsibility |
| --- | --- | --- |
| `bases/operators` | [`../bases/operators`](../bases/operators/) | Sources, namespaces and prerequisite/service operators |
| `bases/tls` | [`../bases/tls`](../bases/tls/) | Issuers and certificates |
| `bases/openbao` | [`../bases/shared-openbao`](../bases/shared-openbao/) | Shared/bootstrap OpenBao HelmRelease, with environment-selected storage |
| `bases/secrets` | [`../bases/eso-store`](../bases/eso-store/) | Shared ClusterSecretStore |
| `bases/controlplane-operator` | [`../bases/controlplane-operator`](../bases/controlplane-operator/) | c5c3 operator HelmRelease |

These are Kustomize packages, not a complete GitOps installer. Your deployment
repository provides the Git/OCI source, Flux Kustomization CRs, environment
patches, bootstrap execution, health gates and retention policy. The checked-in
FluxInstance has no Git sync configuration. Flux APIs must exist before these
Flux resources are applied.

## Lifecycle boundaries

Build a dependency graph that preserves these prerequisites:

1. Install flux-operator and wait for the FluxInstance/Flux APIs.
2. Reconcile sources and operators; wait for required CRDs and admission paths.
3. Reconcile TLS before workloads need its certificate Secrets.
4. Deliver shared OpenBao, then initialize/unseal it and configure engines,
   auth, policies and bootstrap data.
5. Validate the ESO store and synchronize the required bootstrap secrets.
6. Reconcile the c5c3 operator after its prerequisites, including K-ORC. Submit
   the ControlPlane, onboard its managed database tenant, then check final
   ControlPlane readiness.

OpenBao delivery and usable-API readiness are different gates. A bootstrap Job
cannot wait on the unsealed readiness that it creates. Likewise, database
onboarding must not depend on final ControlPlane readiness if that readiness
needs the database credentials being onboarded. Choose and test the health
checks for your topology; the base directories do not encode that entire graph.

[`../openbao/bootstrap`](../openbao/bootstrap/) and
[`../openbao/policies`](../openbao/policies/) package ConfigMaps for
environment-owned Jobs. Read [OpenBao bootstrap](../openbao/README.md) before
adding those execution resources.

## Composition and ownership

Use one Flux owner per resource identity. Do not reconcile the existing broad
`flux-system` aggregate and these stages over the same objects. Preserve the
operator stage's Namespace prune protection and make explicit retention choices
for shared OpenBao and other stateful resources. A directory move within one
owner differs from transferring a resource between owners; see
[migration notes](../MIGRATION.md).

Keep environment-specific instances and gateways in your overlay. The shared
ESO store's namespace allow-list is an authorization grant, and per-ControlPlane
stores have separate tenant identities. A blanket namespace transform or widened
shared store is not a substitute for configuring those relationships.

Use reviewed immutable Git refs or vendored local directories for base imports.
Keep the default Kustomize load restrictions and render each stage before
reconciling it. The [deployment reference](../../docs/reference/infrastructure/e2e-deployment.md)
explains the existing script-based workflow for comparison.
