# Reusable deployment bases

Start with [deploy/README.md](../README.md) to choose a deployment workflow.
These bases compose the existing definitions; they do not replace the lifecycle
orchestration in `make deploy-infra`.

| Base | Objects it owns | Prerequisites |
| --- | --- | --- |
| [operators](operators/README.md) | Namespaces, source objects and infrastructure/service operator reconciliation objects | Flux controllers and APIs already installed |
| [tls](tls/README.md) | Trust issuers and OpenBao/database/OVN certificates | Namespaces and ready cert-manager admission/controller |
| [shared-openbao](shared-openbao/README.md) | Shared/bootstrap OpenBao HelmRelease | Its source, namespace and TLS; separate initialization/unseal |
| [eso-store](eso-store/README.md) | Shared `openbao-cluster-store` ClusterSecretStore | ESO, TLS and initialized/auth-configured shared OpenBao |
| [controlplane-operator](controlplane-operator/README.md) | c5c3 operator HelmRelease | Infrastructure/service operators and K-ORC APIs; bootstrap prerequisites |

Each stage has a distinct object inventory. `operators` excludes FluxInstance,
the shared OpenBao server and c5c3, but includes their source definitions.
Namespaces carry `kustomize.toolkit.fluxcd.io/prune: disabled`. This protects them
from Flux garbage collection; it does not authorize two reconcilers to manage the
same objects. Use the staged bases **or** the legacy aggregate for an installation.

## Consume and customize

Create a Kustomize overlay in your deployment repository. With a checkout vendored
at `vendor/cobaltcore` and the overlay at `operators/`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../vendor/cobaltcore/deploy/bases/operators
```

For a remote base, use a full reviewed commit from the fork that contains this
catalog (replace `FULL_COMMIT_SHA` before rendering):

```yaml
resources:
  - https://github.com/stefanm8/cobaltcore//deploy/bases/operators?ref=FULL_COMMIT_SHA
```

Keep all stages on the same source revision. A Git pin does not freeze chart
version ranges; record resolved charts and images when validating an upgrade.

The [renderable customization example](../examples/base-customization/README.md)
patches controller resources and shared OpenBao storage without copying either
release. Run `kubectl kustomize <overlay-directory>` to preview. This renders Flux
CRs, not the workloads inside their Helm charts or upstream Git installers. Use
explicit patch targets; a global namespace transform would break cross-namespace
source and secret references.

Controller Helm values size the operator. To size its OpenStack services, patch
the ControlPlane/service CR instead; see the [sizing example](../examples/sizing-overlay/).
Do not apply static MariaDB/Memcached instances to identities a ControlPlane owns.

## Orchestration and remaining source paths

A local `kustomization.yaml` is a build recipe. A Flux `Kustomization` is a runtime
reconciler with a source, readiness gates and pruning. The checked-in FluxInstance
has no Git synchronization configured. Your deployment repository supplies it.

The typical stages are operators → TLS → shared OpenBao delivery → environment-owned
bootstrap → usable ESO store and c5c3 → ControlPlane submission → database tenant
onboarding → final ControlPlane readiness. The shared server must be runnable
before bootstrap, but initialization/unseal must not wait for the readiness that
bootstrap itself creates. K-ORC is a Flux Kustomization, so its dependency cannot
be expressed in a HelmRelease-only `dependsOn` list. See [GitOps ordering](../gitops/README.md).

Flux runtime bootstrap still uses the existing raw
[FluxInstance](../flux-system/fluxinstance.yaml) and
[namespaces](../flux-system/namespaces.yaml) files. Static database/cache/Garage
reference instances remain under [flux-system/infrastructure](../flux-system/infrastructure/).
OpenBao [scripts and policies](../openbao/README.md) retain their executable and
ConfigMap interfaces. These are deliberate compatibility boundaries, not missing
public install stages. The [migration note](../MIGRATION.md) explains the bounded
operator adapter and deferred physical moves.
