---
title: Glance Reconciler Architecture
quadrant: operator
---

# Glance Reconciler Architecture

The Glance controller runs the shared table-driven pipeline
(`internal/common/reconcile`) with nine sub-reconcilers. Every step is
instrumented under the `glance_operator` metrics prefix, and the first step to
return a non-zero result or an error short-circuits the chain — conditions and
the requeue are persisted on every exit path through the shared status skeleton.

Two Prometheus vectors cover the pipeline
(`glance_operator_reconcile_duration_seconds`,
`glance_operator_reconcile_errors_total`, the latter labelled by
`sub_reconciler` and `condition_type`), two per-CR db-sync collectors cover
the schema migration (`glance_operator_db_sync_total`,
`glance_operator_db_sync_duration_seconds`), and two per-CR db-purge
collectors cover the recurring purge (`glance_operator_db_purge_total`,
`glance_operator_db_purge_duration_seconds`).

## Pipeline

```text
Secrets ──► DBConnectionSecret ──► Backends ──► Config ──► Database ──► Deployment ──► ┬─ HTTPRoute
                                                                                       ├─ HealthCheck
                                                                                       ├─ HPA
                                                                                       ├─ NetworkPolicy
                                                                                       └─ DBPurge        (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| Secrets | Gates on the selected secret store (`spec.secretStoreRef`, default `openbao-cluster-store`), then the ESO-synced database and service-user credential Secrets; digests the service-user password for the rollout annotation | `SecretsReady` |
| DBConnectionSecret | Materializes the pymysql DSN into the derived `{name}-db-connection` Secret and digests it; **reports through `SecretsReady`** | `SecretsReady` |
| Backends | Aggregates the attached, credential-ready `GlanceBackend`s into the content-hashed backends Secret. Waiting states never short-circuit the pipeline (first-install proceeds; a backend status flip re-enqueues via the watch) | `BackendsReady` |
| Config | Renders `glance-api.conf` / `glance-api-paste.ini` (plus `policy.yaml` / `logging.conf` when applicable) into an immutable content-addressed ConfigMap. With `spec.imageCache` set it adds the three `[DEFAULT]` `image_cache_*` keys (the cache directory, the `sqlite` driver, and a pruner threshold at 80% of the volume bound) and injects the `cache` paste filter directly before the root app. An invalid projection keeps the live Deployment's last-good names instead of re-rendering. **Reports through `SecretsReady`** (failure reason `ConfigError`) | `SecretsReady` |
| Database | Provisions/migrates the schema (MariaDB gate, `Database`/`User`/`Grant`, one `glance-manage db sync` Job); a release bump instead runs the shared expand-migrate-contract flow; promotes `installedRelease` | `DatabaseReady` |
| Deployment | Ensures the API Deployment (both launch modes), Service (port 9292), and PDB; stamps `status.endpoint`. Both scratch `emptyDir`s (`staging`, `tasks-work`) carry the `sizeLimit` resolved from `spec.staging` — none at all when `spec.staging.unbounded` is set — so changing the bound rolls the Deployment. `spec.imageCache` adds a third `emptyDir`, `image-cache`, bounded by its own `sizeLimit` and mounted into `glance-api`, plus a `cache-maintenance` sidecar looping `glance-cache-pruner` and `glance-cache-cleaner`. That loop prunes on the cache's own size — polled every 30s against `image_cache_max_size` — and on `maintenanceInterval`, whichever comes first. It gets no condition type of its own, and no failure of it ever exits the sidecar: every failed run is absorbed and shows only as a `glance-cache-maintenance failed: <n> consecutive` line in `kubectl logs -c cache-maintenance`, because the loop shares the pod with `glance-api` and a `CrashLoopBackOff` would drop that API replica from the Service. A count that keeps climbing is a pruner that can never run, and since nothing else enforces `image_cache_max_size`, every replica's cache then grows to the `emptyDir` bound and the kubelet evicts it — outside the eviction API, so the PDB does not stagger it. This sub-reconciler reports that only as `WaitingForDeployment`; no condition, event, or metric names the cause. Mid-upgrade it flips the `RollingUpdate` phase to `Contracting` once the rolled-out Deployment reports ready | `DeploymentReady` |
| HTTPRoute | Full `spec.gateway` lifecycle; reflects the Gateway's Accepted condition. The route renders `timeouts.request: "4h"`, so the gateway does not truncate a long image transfer and a stalled one releases its worker eventually. The bound is on duration only — nothing the operator renders caps how many requests may occupy the API's workers at once | `HTTPRouteReady` |
| HealthCheck | HTTP GET of the cluster-local `/healthcheck` through the shared TTL probe cache | `GlanceAPIReady` |
| HPA | Creates/deletes the HorizontalPodAutoscaler | `HPAReady` |
| NetworkPolicy | Creates/deletes the NetworkPolicy (auto-derived DB/cache/S3 egress); refuses an empty ingress list (fail-closed) | `NetworkPolicyReady` |
| DBPurge | Projects the `{name}-db-purge` CronJob and reports the newest terminal run it spawned | `DBPurgeReady` |

`DBConnectionSecret` and `Config` deliberately reuse `SecretsReady` rather than
a dedicated `ConfigReady` condition: both produce Secret/ConfigMap artefacts
that gate the same downstream graph, so a distinct `sub_reconciler` label on the
error counter disambiguates them during triage while the status contract stays
minimal.

## Conditions

The aggregate `Ready` condition is `True` (reason `AllReady`) exactly when all
nine sub-conditions are `True`; otherwise `False` (`NotAllReady`).

| Type | True reasons | False reasons |
| --- | --- | --- |
| `SecretsReady` | `SecretsAvailable` | `TargetClusterUnavailable`, `SecretStoreNotReady`, `WaitingForDBCredentials`, `WaitingForServiceUserCredentials`, `ConfigError` |
| `BackendsReady` | `AllBackendsProjected` | `WaitingForBackends`, `NoDefaultBackend` |
| `DatabaseReady` | `DatabaseSynced` | `ClusterNotReady`, `WaitingForDatabase`, `DBSyncFailed`, `DBSyncInProgress`, `WaitingForBackends`, `ImageReleaseMismatch`, `VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`, `UpgradeTargetChanged`, `ExpandInProgress`, `MigrateInProgress`, `UpgradeRollingUpdate`, `ContractInProgress`, `ExpandFailed`, `MigrateFailed`, `ContractFailed` |
| `DeploymentReady` | `DeploymentReady` | `WaitingForDeployment`, `WaitingForBackends` |
| `GlanceAPIReady` | `APIHealthy` | `APIUnhealthy`, `EndpointNotReady`, `HealthCheckTimeout`, `ConnectionFailed`, `HealthCheckFailed` |
| `HPAReady` | `HPAReady`, `HPANotRequired` | — (errors propagate) |
| `NetworkPolicyReady` | `NetworkPolicyReady`, `NetworkPolicyNotRequired` | — (errors propagate) |
| `HTTPRouteReady` | `HTTPRouteAccepted`, `HTTPRouteNotRequired` | `HTTPRouteNotAccepted`, `GatewayAPINotInstalled`, `CapabilityProbeFailed` |
| `DBPurgeReady` | `DBPurgeScheduled`, `DBPurgeSuspended` | `DBPurgeJobFailed` |

`TargetClusterUnavailable` is set ahead of every sub-reconciler, when
`spec.targetClusterRef` names a target cluster that is not registered or no
longer resolves. The message carries the resolver's error, the CR requeues after
15 seconds and acquires no finalizer, and nothing is created on any cluster. An
attached `GlanceBackend` reports the same reason on `CredentialsReady`. See
[Target Clusters](../target-clusters.md).

Both `DatabaseReady` and `DeploymentReady` carry a `WaitingForBackends` reason:
the schema cannot be `db sync`-ed and the Deployment cannot be created until a
ready default backend has produced a rendered config to run against.

## Launch modes

`spec.openStackRelease` selects the launch mode at the `2026.1` boundary: a
release `>= 2026.1` launches under uWSGI, anything below runs the eventlet
`glance-api` server. An unparseable release (a CR that bypassed the CRD pattern)
falls back to the eventlet mode. Both modes load the **same two** oslo.config
`--config-dir` roots — the immutable config ConfigMap
(`/etc/glance/glance-api.conf.d/`) and the backends Secret
(`/etc/glance/backends.conf.d/`).

- **uWSGI (`>= 2026.1`).** The command is `uwsgi --http :9292
  --http-auto-chunked --http-chunked-input --chunked-input-limit 16777216 …` —
  the two chunked flags are always on because Glance streams image bodies with
  chunked transfer encoding, and the limit raises uWSGI's 1 MB cap on one
  request chunk to 16 MiB: glanceclient uploads a body of unknown size in 1 MiB
  chunks, which the default rejects with `OSError: unable to receive chunked
  part` and a 500, so cinder's upload-to-image and nova's snapshot upload fail
  under the default while the eventlet mode has no such cap. Keep-alive
  (`--http-keepalive`, and `--http-keepalive-timeout` when set) and `--harakiri`
  are emitted from the `spec.apiServer.uwsgi` knobs. uWSGI loads the WSGI app
  through `--wsgi-file /var/lib/openstack/bin/glance-wsgi-api` (the image-shipped
  shim), passing both `--config-dir` roots via `--pyargv`, because glance's stock
  WSGI module ignores `sys.argv`.
- **eventlet (`< 2026.1`).** The command is `glance-api --config-dir
  /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/`; the
  worker count comes from `[DEFAULT] workers` in the config (from
  `spec.apiServer.workers`), not the CLI.

Setting the wrong knob for the active mode is legal but inert; the validating
webhook returns an admission warning rather than rejecting.

## Backends aggregation

The Backends step lists the attached `GlanceBackend`s (via the
`spec.glanceRef.name` field index), sorts them by name for deterministic output,
and renders one `[<name>]` store section per **credential-ready** backend into a
content-hashed backends Secret (rolled on any content change, 3 historical
retained). It gates each backend on `CredentialsReady==True` — never the
aggregate `Ready`, which also requires `ConfigProjected` and would deadlock,
since `ConfigProjected` only turns True after this step projects the backend.

- **Exactly-one-Ready-default rule.** Zero or more than one credential-ready
  default is an invalid projection (`BackendsReady=False / NoDefaultBackend`):
  nothing is re-rendered and the config step retains last-good.
- **Per-backend fault isolation.** A backend whose credentials Secret is
  missing/unreadable, or whose rendered values carry a control character, is
  skipped with a `GlanceBackendSkipped` Warning event on the Glance CR while the
  healthy siblings keep projecting.

## Database

The Database step runs the shared provisioning + sync flow: a MariaDB cluster
gate and `Database`/`User`/`Grant` in managed mode (a no-op in brownfield),
then a single `{name}-db-sync` Job running `glance-manage db sync` followed by
`db load_metadefs`. Because Glance's migrations apply in one idempotent pass,
there is **no schema-check Job**, so, unlike Keystone, `SchemaDriftDetected`
never fires for Glance. On fresh installs and patch bumps `installedRelease` is
promoted to `spec.openStackRelease` on Job success.

A release transition (a `spec.openStackRelease` bump with the image in lockstep)
instead dispatches to the shared expand-migrate-contract flow
(`internal/common/database`), the same phase machine Keystone runs. The step
validates the path against `installedRelease`, rejecting a downgrade or a
non-sequential jump with `VersionParseError`, `DowngradeNotSupported`, or
`UpgradePathInvalid`, then walks `Expanding → Migrating → RollingUpdate →
Contracting` with `glance-manage db expand|migrate|contract` phase Jobs on the
new image. The outgoing pods keep serving the expanded schema across the
rollout, so the API stays available, and `installedRelease` is promoted only
after contract completes. The Deployment step owns the `RollingUpdate →
Contracting` flip: once `reconcileDeployment` sees the rolled-out Deployment
ready, it advances the phase to `Contracting` and requeues so the contract Job
runs. See the [Glance Upgrade Flow](./glance-upgrade-flow.md) for the phase
table, condition reasons, events, and abort semantics.

### Connection cap

The operator sets the MariaDB `User`'s `max_user_connections` to a cap sized
for the CR's own topology. It owns the field and applies the computed figure on
every reconcile, so a value edited on the `User` by hand is reset; the cap
rises only with the counts in the formula below. The mariadb-operator default
of 10 is too small for the default fleet: three 2026.1 pods of two uWSGI
processes hold up to 30 connections, and a single eventlet worker without the
pool pin described below opened 32. A cap below the fleet's demand does not
degrade gracefully. The process that opens the connection past it gets MySQL
error 1226: at start-up its pod crash-loops, and under load the upload, import
or snapshot it serves answers HTTP 500.

```text
(apiPods + 1) x perPod + 2

perPod under uWSGI (2026.1 and later):  uwsgiProcesses x (uwsgiThreads + 5)
perPod under eventlet (below 2026.1):   workers x 5
```

The defaults size to 50 at 2026.1 and 42 below it. `apiPods` is the
autoscaling ceiling when an HPA owns the count, and the `+ 1` beside it is the
rolling-update surge. Under uWSGI each request thread holds one connection, and
the `+ 5` is what the per-process async import pool added on top while
`web-download` imports ran. A heavier import mix pushes that term toward
`[wsgi] task_pool_threads`, which defaults to 16 per process. Under eventlet
each worker is bounded at a pool size of 5 by the pool pin.
Each launch mode ignores the other's knob: `spec.apiServer.workers` is inert
under uWSGI, and `spec.apiServer.uwsgi` is inert below 2026.1.

`perPod` is the largest figure over `spec.openStackRelease`,
`status.installedRelease` and `status.targetRelease`. The launch modes mix only
across a release boundary. During the `RollingUpdate` phase of a 2025.2 to
2026.1 upgrade the old eventlet pods run beside the new uWSGI pods, and an abort
mixes them the other way. Sized from the spec release alone, a 2025.2 Glance
with `workers: 8` would drop to a cap of 50 mid-upgrade while its old pods hold
up to 120. The trailing `+ 2` is headroom for one migration Job (db-sync, or the
active expand, migrate or contract Job) overlapping a `{name}-db-purge` run. The
`cache-maintenance` sidecar opens no database connection.

Below 2026.1 the operator renders `[database] max_pool_size = 5` and
`max_overflow = 0`. The eventlet server runs every request as a greenthread on
one pool per worker, so with oslo.db's default overflow of 50 a worker's
connection count follows client concurrency, up to 55, and no topology figure
bounds it. With the pin, greenthreads beyond five wait for a pooled connection,
up to SQLAlchemy's `pool_timeout` of 30 s, instead of failing with error 1226.
The pool size of 5 is oslo.db's default. The operator renders it anyway, so the
figure the cap counts is one it sets rather than one it assumes. Measured
against the 2025.2 image under 16 concurrent clients, the pinned worker served
more requests than an unpinned one and logged no pool timeout. Under uWSGI
neither key is rendered: the thread count bounds demand there, and the overflow
pin would queue requests in any process running more than five threads. An
`extraConfig` override of `[database] max_pool_size` or `max_overflow` is
honoured and reported through `ExtraConfigHealthy`.

## DBPurge

The DBPurge step runs in the parallel group, after Database and Deployment, so
the purge CronJob can mount the same rendered config ConfigMap the Deployment
already consumes. It projects the `{name}-db-purge` CronJob on every pass with
the settings `effectiveDBPurge` resolves, and it derives run visibility rather
than watching the CronJob: it lists the Jobs carrying the Glance's common
labels, keeps the ones the CronJob controls, and reports on the newest that
reached a terminal state.

A failed run flips `DBPurgeReady` to `False` (reason `DBPurgeJobFailed`) and
raises a matching Warning event; a later successful run flips it back to
`True`, so an old failure a bounded history has not yet pruned cannot wedge
the condition once a newer run succeeds. A run that wedges rather than fails —
an unschedulable pod, a `glance-manage` blocked on a database lock — reaches a
terminal `Failed` state through the Job's `activeDeadlineSeconds`, so it
surfaces on the same path instead of leaving `DBPurgeReady` reporting a purge
that never happens. Every terminal run also feeds the
`glance_operator_db_purge_total` / `glance_operator_db_purge_duration_seconds`
pair exactly once per Job UID. Because that once-per-UID annotation is a write
to the CR from inside the parallel group, the group adopts the member's
post-patch metadata onto the primary object before the status write.

A CronJob suspended via `spec.dbPurge.suspend` keeps `DBPurgeReady` `True` — a
pause is a posture, not a failure — but reports it under its own reason
`DBPurgeSuspended`, because nothing else does: the CronJob never fires again, so
no run fails, no Warning event follows, and `glance_operator_db_purge_total`
merely stops incrementing while the soft-deleted backlog keeps growing.

See [DBPurgeSpec](./glance-crd.md#dbpurgespec) for the CronJob shape and the
`glance-manage` commands it runs.

The CronJob fires on the Kubernetes scheduler's clock, not on the pipeline's,
so it keeps running during a release upgrade — while the Database step is
driving expand/migrate/contract, the chain short-circuits before DBPurge and
the CronJob keeps its pre-upgrade image. A run landing inside that window fails
loudly (`DBPurgeJobFailed`) rather than corrupting anything, since the purge
only issues bounded deletes. Suspend the CronJob via `spec.dbPurge.suspend`
across a planned upgrade to avoid the noise.

## Requeue semantics

| Interval | Used by |
| --- | --- |
| 10s | Deployment readiness polling, HTTPRoute acceptance, health-check retry |
| 15s | ESO secret-gate polling (Secrets, DBConnectionSecret; the GlanceBackend controller's credential/projection backstop) |
| 30s | MariaDB / db-sync / ready-default-backend database wait |
| 30s TTL | Health-probe cache (a passing `/healthcheck` probe is reused within the TTL) |

## Rotation and deletion

- **Credential rotation** happens at the OpenBao source. The service-user
  password (consumed via `OS_KEYSTONE_AUTHTOKEN__PASSWORD`) and the assembled
  DSN (consumed via `OS_DATABASE__CONNECTION`) are both env-var-delivered, so a
  restart is required for a rotation to take effect. The Secrets and
  DBConnectionSecret steps digest each value into a pod-template annotation —
  `glance.c5c3.io/authtoken-hash` and `glance.c5c3.io/db-connection-hash` — so a
  changed digest rolls the Deployment.
- **Deletion** runs the `glance.openstack.c5c3.io/finalizer`: it issues Delete
  on the MariaDB `Database`/`User`/`Grant` CRs before the owner-ref chain
  disappears, emitting `FinalizingDatabase` while cleanup remains and
  `DatabaseFinalized` when the finalizer is released. Every other owned resource
  is namespace-scoped with a controller owner reference, so Kubernetes garbage
  collection reclaims it. A CR whose target cluster was deregistered in the meantime cannot
  reach any of it: the finalizer is released against no cleanup at all, a
  `RemoteChildrenAbandoned` Warning names what stays behind, and the CR
  leaves etcd instead of hanging in Terminating. See
  [Target Clusters](../target-clusters.md).
  The `GlanceBackend` controller has no finalizer: the
  operator provisions no S3 resources, so there is nothing to clean up remotely.
  Deletion still **de-registers the store** — the bucket and its objects are
  external state, and image location rows referencing that store id are left
  dangling, so those images become undownloadable until a backend with the same
  name is re-attached.

## Watches

The Glance controller `Owns` its Deployment, Service, ConfigMap, Secret,
PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, and — for the
db-sync migration — Job, plus the db-purge CronJob: its `status.active` list
changes as each spawned run starts and finishes, which is what wakes the
reconcile that refreshes `DBPurgeReady`. The HTTPRoute is added to the `Owns`
set only when the Gateway API CRD is installed on the management cluster
(otherwise `spec.gateway` surfaces
`HTTPRouteReady=False / GatewayAPINotInstalled` instead of crashing the
controller). That setup probe answers for CRs without a
`spec.targetClusterRef`; a CR that names a target cluster has the kind probed
against that cluster's RESTMapper on every pass. Beyond the owned set it
watches:

- **Secrets**, mapped to the Glance CRs that reference them directly (via the
  `spec.serviceUser.secretRef.name` / `spec.database.secretRef.name` field
  index) or through an attached backend's S3 credentials Secret (via the backend
  secret-name index).
- **GlanceBackends**, mapped to their parent Glance with no generation predicate
  — a backend's `CredentialsReady` flip is exactly what wakes the aggregation.
- **MariaDB** clusters referenced by `spec.database.clusterRef`, so an upstream
  database outage reflects in `DatabaseReady` without waiting for a periodic
  requeue.
- Both the cluster-scoped `ClusterSecretStore` and the namespaced `SecretStore`
  a Glance can select, so a store-backend outage reflects in `SecretsReady`.

The Glance reconciler registers **both** controllers' field indexes at setup
(`spec.glanceRef.name` and the secret-name indexes), so it must be set up before
the `GlanceBackend` controller. The `GlanceBackend` controller in turn watches
its parent Glance (no generation predicate: the store config landing in the
Deployment is the wake signal its `ConfigProjected` gate waits on) and is the
**single writer** of `GlanceBackend` status.
