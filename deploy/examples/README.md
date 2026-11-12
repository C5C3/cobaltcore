# Deployment customization examples

Start with the [base catalog](../bases/README.md) to choose the resources your
overlay owns. Examples are inputs to inspect and adapt; `make deploy-infra` does
not apply this directory.

[`base-customization/`](base-customization/README.md) is a small, locally
renderable example of the public catalog. Its independent overlays patch
Keystone controller resources and shared OpenBao storage without copying either
release. They require no generated inputs or network access to render:

```bash
kubectl kustomize deploy/examples/base-customization/operators
kubectl kustomize deploy/examples/base-customization/shared-openbao
```

[`sizing-overlay/`](sizing-overlay/) demonstrates two separate phases:

| Entry point | Customizes |
| --- | --- |
| [`sizing-overlay/`](sizing-overlay/) | Operator HelmRelease values, Flux-managed installer patches, Flux controller sizing and a shared PriorityClass |
| [`sizing-overlay/infrastructure/`](sizing-overlay/infrastructure/) | Static database/cache resources and placement, after their operators and the first phase exist |

The second phase references the first phase's PriorityClass without declaring
another owner. Copy the example into your deployment repository, update its
base references to a local checkout or reviewed pinned commit, and choose
values for your environment. Keep the chart version floors required by patched
fields; the example comments explain them.

Preview both phases from the repository root:

```bash
kubectl kustomize deploy/examples/sizing-overlay
kubectl kustomize deploy/examples/sizing-overlay/infrastructure
```

Patching a HelmRelease's controller resources sizes the operator. To size
ControlPlane-managed OpenStack workloads, customize the ControlPlane CR instead
of installing competing static infrastructure. Read
[sizing and placement overrides](../../docs/reference/infrastructure/infrastructure-manifests.md#sizing-and-placement-overrides)
for the supported field shapes and ownership model.

For staged reconciliation and bootstrap prerequisites, see
[GitOps stages](../gitops/README.md). Rendering a HelmRelease validates its YAML
composition, not the eventual chart workload or live readiness.
