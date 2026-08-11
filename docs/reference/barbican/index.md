---
title: Barbican Operator
quadrant: operator
---

# Barbican Operator

The Barbican operator deploys and manages the OpenStack Key Manager service as
a Kubernetes-native workload. It is the fifth service operator built on the
shared scaffolding the Keystone operator established (`internal/common`, the
`operator-library` Helm chart, the parameterized operator image), after
Keystone, Horizon, Glance, and Placement.

Barbican keeps secret metadata in its own MariaDB schema and hands the secret
material itself to a secret-store plugin. That store is modeled out-of-band as
a [`BarbicanSecretStore`](./barbican-secret-store-crd.md) CR attached through
`spec.barbicanRef`, the same inverted attachment `GlanceBackend` uses, so a
store is added or replaced without editing the Barbican CR. Phase 1 ships one
store type, `OpenBao`.

## Design decisions

The v1 operator resolves the onboarding decisions as follows:

- **No message bus.** The rendered config carries `[queue] enable = false`. The
  only workload the operator projects is the API Deployment: there is no
  `barbican-worker` consumer behind the queue, and no retry or keystone-listener
  daemon either. The queue only moves asynchronous order processing to such a
  worker, so with none deployed an enabled queue would leave orders pending
  instead of failing them. The key is therefore operator-owned, and an
  `extraConfig` override of it is reported through `ExtraConfigHealthy`.
- **One OpenBao store per Barbican.** The vault plugin reads its server URL,
  its AppRole and its mount from the process-global `[vault_plugin]` section,
  which every store in one `barbican.conf` shares. A second OpenBao store would
  not get a second server: both `[secretstore:<name>]` sections would resolve to
  the same plugin configuration and whichever rendered last would decide where
  the other one's secrets are written. `validateSiblingOpenBaoUniqueness` in
  `operators/barbican/api/v1alpha1/barbicansecretstore_webhook.go` rejects the
  second store at admission, and the aggregation step carries the fail-closed
  backstop for a CR that reached etcd without the webhook
  (`SecretStoresReady=False / MultipleOpenBaoStores`).
- **No upgrade phase machine.** The CR has no `upgradePhase` status field. A
  release bump runs the one `{name}-db-sync` Job, which chains nothing:
  `barbican-manage db upgrade` is a single alembic upgrade to head that applies
  every pending revision in one pass. Status tracks the release through
  `installedRelease`, `installedImage`, and `targetRelease`, the same posture
  the Placement operator uses. Upgrade paths stay sequential and a downgrade is
  refused before any Job runs.
- **Config mounted at `/etc/barbican`.** Barbican accepts no `--config-dir` and
  resolves both of its startup files by name: oslo.config loads `barbican.conf`
  from its fixed search path, and the API app looks `barbican-api-paste.ini` up
  the same way. `/etc/barbican` is the one directory that can carry either, so
  the rendered config Secret mounts as that whole directory
  (`barbicanConfigDir` in `operators/barbican/internal/controller/reconcile_config.go`)
  and shadows the image's own copy. The db-sync Job and the clean-up CronJob
  mount it at the same path and therefore pass no `--config-file`.
- **The clean-up CronJob is always scheduled.** Barbican never hard-deletes:
  deleting a secret, container, or order flips its row to deleted, so the tables
  grow for the lifetime of the deployment. Every Barbican gets the
  `{name}-db-clean` CronJob; `spec.dbClean` only varies its retention window,
  schedule, extra passes, and suspension. An unbounded soft-delete backlog is a
  deferred outage rather than a posture worth offering.

## Owned resources

For a Barbican CR named `{name}` the operator manages:

| Resource | Name | Purpose |
| --- | --- | --- |
| Deployment | `{name}` | The Barbican API pods (port 9311) |
| Service | `{name}` | ClusterIP in front of the API pods on port 9311 |
| PodDisruptionBudget | `{name}` | `minAvailable: 1` (or `maxUnavailable: 1` at a single replica); the selector excludes Job pods |
| HorizontalPodAutoscaler | `{name}` | Only when `spec.autoscaling` is set |
| NetworkPolicy | `{name}` | Only when `spec.networkPolicy` is set |
| HTTPRoute | `{name}` | Only when `spec.gateway` is set |
| Secret | `{name}-config-<hash>` | Immutable, content-hashed `barbican.conf` and `barbican-api-paste.ini` (plus `policy.yaml` when applicable; 3 historical retained) |
| Secret | `{name}-db-connection` | Derived pymysql DSN, consumed via `OS_DATABASE__CONNECTION` |
| Job | `{name}-db-sync` | Schema migration (`barbican-manage db upgrade`) |
| CronJob | `{name}-db-clean` | The recurring `barbican-manage db clean` sweep |
| MariaDB `Database` / `User` / `Grant` | `{name}` | Managed mode only (`spec.database.clusterRef`); a brownfield database is left alone |

The AppRole credentials Secret of a managed store, `<store>-approle`, is owned
by its `BarbicanSecretStore` and not by the Barbican; see
[Retained Artefacts](./barbican-secret-store-crd.md#retained-artefacts).

## Reference pages

- [Barbican CRD](./barbican-crd.md) — the full `spec`/`status` contract
- [BarbicanSecretStore CRD](./barbican-secret-store-crd.md) — the attached
  secret store, its credentials, and its conditions
- [Controller Events](./barbican-events.md) — the Kubernetes events the two
  controllers emit
- [Reconciler Architecture](./barbican-reconciler.md) — the sub-reconciler
  pipeline, conditions, and requeue semantics
