# Deployment base catalog: compatibility and migration

The initial [public catalog](bases/README.md) gives new consumers five named,
staged bases. Existing `make deploy-infra`, environment overlays, OpenBao scripts,
raw manifest paths and target-cluster chart paths remain supported. Existing consumers do not
need to change imports merely to continue using those entry points.

## Moving a staged import

| Existing directory | Public catalog directory |
| --- | --- |
| `deploy/gitops/bases/operators` | `deploy/bases/operators` |
| `deploy/gitops/bases/tls` | `deploy/bases/tls` |
| `deploy/gitops/bases/openbao` | `deploy/bases/shared-openbao` |
| `deploy/gitops/bases/secrets` or `deploy/eso` | `deploy/bases/eso-store` |
| `deploy/gitops/bases/controlplane-operator` | `deploy/bases/controlplane-operator` |

Select a reviewed revision containing the new path, update the import in your
deployment repository, and compare rendered resource identities and fields.
Keep environment patches, Flux Kustomization names, source identities and
prune/retention policies unchanged for a path-only migration. In particular,
preserve Namespace prune protection and the staged OpenBao base's absence of a
storage-class pin.

The broad `deploy/flux-system` aggregate has no one-for-one staged replacement:
it also includes FluxInstance, shared OpenBao and c5c3. Its infrastructure root
includes static instances. Moving from that aggregate to staged ownership is a
separate migration, not just changing a path in this table.

## Preserve ownership and ordering

Do not apply the old aggregate and new stages as independent owners of the same
resources. If a resource moves between Flux Kustomizations, prevent the old
inventory from pruning it, remove it from that inventory, reconcile and verify
the new owner, then restore the intended retention policy. Account for namespaces
and data-bearing resources explicitly.

Retain the readiness phases described in [GitOps stages](gitops/README.md) and
the [deployment overview](README.md). A successful Kustomize build proves YAML
composition; it does not prove Helm workload readiness, OpenBao initialization,
secret synchronization or ControlPlane readiness.

Directory-based compatibility wrappers cannot preserve a raw `apply -f`, YAML
parser, copied file, shell entry point or Helm chart path by themselves. Those
paths remain in place. Any later relocation must update its literal callers,
teardown, CI filters, tests, Renovate rules, generated-input staging and docs
together.

## Scope of this cleanup

The catalog clarifies reuse and the deployment READMEs explain entry points. It
does not change the following contracts:

- `EXTERNAL_OVERLAY` still exposes `base/`, `infrastructure/`, required templates
  and the optional child directories used by its enabled flags.
- `INFRA_ONLY`, ControlPlane mode and external-operator mode retain their
  resource ownership, filtering and suspension behavior.
- Kind and metal-stack retain their distinct storage, gateway, networking,
  metrics, node preparation and hypervisor settings.
- OpenBao bootstrap scripts/policies retain their paths and ConfigMap data keys;
  they remain separate from server installation and environment-owned Jobs.
- The target-cluster chart retains its name, path and synchronized CRD copies.

The Prometheus composed flow now has one Namespace owner: kind installation and
the lab use `deploy/kind/prometheus-managed`, which omits the Namespace supplied
by the platform base. The existing `deploy/kind/prometheus` root retains its
output, including the Namespace, for direct consumers. The external overlay's
`prometheus/` interface remains unchanged.

Flux runtime and individual static database/cache/Garage bases are deferred;
their existing raw-file and aggregate entry points remain. Publishing additional
leaves must preserve those callers and default Kustomize load restrictions
without introducing independently editable manifest copies. The operators base
retains a bounded compatibility adapter for the same reason; the catalog is an
initial public interface, not a complete physical reorganization.

Physical environment-directory moves, consolidation of initialization helpers,
other addon ownership changes, and new bootstrap custody or storage behavior
require separate migration and validation. No removal date is assigned to the
compatibility paths here.
