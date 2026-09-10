---
title: Cinder Reconciler Architecture
quadrant: operator
---

# Cinder Reconciler Architecture

The Cinder controller runs the shared table-driven pipeline
(`internal/common/reconcile`) with twelve sequential sub-reconcilers and a
parallel group of four. Every step is instrumented under the `cinder_operator`
metrics prefix, and the first step to return a non-zero result or an error
short-circuits the chain. Conditions and the requeue are persisted on every exit
path through the shared status skeleton.

Two Prometheus vectors cover the pipeline
(`cinder_operator_reconcile_duration_seconds`,
`cinder_operator_reconcile_errors_total`, the latter labelled by
`sub_reconciler` and `condition_type`). Three per-CR collector pairs cover the
Jobs: `cinder_operator_db_sync_total` and
`cinder_operator_db_sync_duration_seconds` for the schema migrations,
`cinder_operator_db_purge_*` for the recurring purge, and
`cinder_operator_service_remove_*` for a backend detach. Each pair is labelled
by `cinder` and `namespace`, and the counters carry the terminal `result`. The
Grafana dashboard `operators/cinder/dashboards/cinder-operator.json` reads them
under the uid `cinder-operator`.

Two more controllers run beside it, one per satellite kind. Each is the single
writer of its own CR's status and creates nothing.

## Pipeline

```text
Secrets ──► DBConnectionSecret ──► TransportURLSecret ──► Backends ──► BackupBackend ──► Config ──►
Database ──► Scheduler ──► VolumeServices ──► BackupService ──► Deployment ──► DBPurge ──► ┬─ HTTPRoute
                                                                                           ├─ HealthCheck
                                                                                           ├─ HPA
                                                                                           └─ NetworkPolicy  (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| Secrets | Gates on the selected secret store (`spec.secretStoreRef`, default `openbao-cluster-store`), then the ESO-synced database and service-user credential Secrets; digests the service-user password for the rollout annotation | `SecretsReady` |
| DBConnectionSecret | Materializes the pymysql DSN into the derived `{name}-db-connection` Secret and digests it; reports through `SecretsReady` | `SecretsReady` |
| TransportURLSecret | Materializes the `rabbit://` URL into the derived `{name}-transport-url` Secret, digests it, and resolves the broker port the NetworkPolicy member opens; reports through `SecretsReady` | `SecretsReady` |
| Backends | Renders every attached, credential-ready `CinderBackend` into its own content-hashed Secret and sweeps the base names nothing projects anymore. Waiting states never short-circuit the pipeline: a Cinder without backends still converges, and a backend's status flip re-enqueues through the watch | `BackendsReady` |
| BackupBackend | Renders the single attached, credential-ready `CinderBackupBackend`, under the same never-short-circuit contract | `BackupBackendReady` |
| Config | Renders `cinder.conf` and `scheduler.conf` (plus `logging.ini` and `policy.yaml` when applicable) into an immutable content-addressed ConfigMap, and maintains the informational `ExtraConfigHealthy` condition. A section carrying a control character is not re-rendered: the step returns the artefacts the live API Deployment currently mounts, so the running pods keep their last-good config. Failures report through `SecretsReady` with the reason `ConfigError` | `SecretsReady` |
| Database | Provisions and migrates the schema (MariaDB gate, `Database`/`User`/`Grant`, one `cinder-manage db sync` Job); a release bump instead runs the expand-migrate-contract flow; promotes `installedRelease`; sizes the SQL user's connection cap | `DatabaseReady` |
| Scheduler | Ensures the `{name}-scheduler` Deployment. No Service, no PodDisruptionBudget, no autoscaling: the scheduler takes its work off the bus | `SchedulerReady` |
| VolumeServices | Ensures one `{name}-volume-{backend}` Deployment per projected backend, deletes the Deployments of backends that left, reports `status.volumeServices`, and drives the detach of the backends that are being deleted | `VolumeServicesReady` |
| BackupService | Ensures the `{name}-backup` Deployment, or deletes it when no backup target is projected. It mounts every volume backend's export as well, because a backup reads the source volume itself | `BackupServiceReady` |
| Deployment | Ensures the API Deployment, its Service (port 8776) and the PDB, and stamps `status.endpoint`. Mid-upgrade it flips the `RollingUpdate` phase to `Contracting` once the rollout has fully converged | `DeploymentReady` |
| DBPurge | Projects the `{name}-db-purge` CronJob and reports the newest terminal run it spawned. It runs before the parallel group rather than inside it, because it needs only the rendered config the Config step produced | `DBPurgeReady` |
| HTTPRoute | Full `spec.gateway` lifecycle; reflects the Gateway's Accepted condition | `HTTPRouteReady` |
| HealthCheck | HTTP GET of the cluster-local `/healthcheck` through the shared TTL probe cache | `CinderAPIReady` |
| HPA | Creates and deletes the HorizontalPodAutoscaler of the API Deployment | `HPAReady` |
| NetworkPolicy | Creates and deletes the NetworkPolicy (auto-derived egress, including the broker port and one rule for the NFS exports); refuses an empty ingress list (fail-closed) | `NetworkPolicyReady` |

`DBConnectionSecret`, `TransportURLSecret` and `Config` reuse `SecretsReady`
rather than a dedicated `ConfigReady` condition: all three produce artefacts
that gate the same downstream graph, so a distinct `sub_reconciler` label on the
error counter disambiguates them during triage while the status contract stays
minimal.

The four members of the parallel group have no inter-dependency. Each operates
on its own copy of the CR and sets exactly one condition, and the group merges
the conditions back before the status write.

## Conditions

The aggregate `Ready` is True (reason `AllReady`) exactly when all thirteen
sub-conditions are True; otherwise False (`NotAllReady`). `ExtraConfigHealthy`
is deliberately outside the aggregate: it reports on an overlay the user owns
and must not depool a Cinder whose API serves fine.

| Type | True reasons | False reasons |
| --- | --- | --- |
| `SecretsReady` | `SecretsAvailable` | `TargetClusterUnavailable`, `SecretStoreNotReady`, `WaitingForDBCredentials`, `WaitingForServiceUserCredentials`, `WaitingForMessagingCredentials`, `ConfigError` |
| `BackendsReady` | `AllBackendsProjected`, `NoBackends` | `WaitingForBackends` |
| `BackupBackendReady` | `BackupBackendProjected`, `NoBackupBackend` | `WaitingForBackupBackend`, `MultipleBackupBackends` |
| `DatabaseReady` | `DatabaseSynced` | `ClusterNotReady`, `WaitingForDatabase`, `WaitingForConfig`, `ImageReleaseMismatch`, `DBSyncFailed`, `DBSyncInProgress`, `VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`, `UpgradeTargetChanged`, `ExpandInProgress`, `MigrateInProgress`, `UpgradeRollingUpdate`, `ContractInProgress`, `ExpandFailed`, `MigrateFailed`, `ContractFailed` |
| `SchedulerReady` | `SchedulerReady` | `WaitingForScheduler` |
| `VolumeServicesReady` | `AllVolumeServicesReady` | `NoBackends`, `WaitingForVolumeServices`, `ServiceRemoveJobFailed` |
| `BackupServiceReady` | `BackupServiceReady`, `BackupNotConfigured` | `WaitingForBackupService` |
| `DeploymentReady` | `DeploymentReady` | `WaitingForDeployment` |
| `CinderAPIReady` | `APIHealthy` | `APIUnhealthy`, `EndpointNotReady`, `HealthCheckTimeout`, `ConnectionFailed`, `HealthCheckFailed` |
| `HPAReady` | `HPAReady`, `HPANotRequired` | errors propagate |
| `NetworkPolicyReady` | `NetworkPolicyReady`, `NetworkPolicyNotRequired` | errors propagate |
| `HTTPRouteReady` | `HTTPRouteAccepted`, `HTTPRouteNotRequired` | `HTTPRouteNotAccepted`, `GatewayAPINotInstalled`, `CapabilityProbeFailed` |
| `DBPurgeReady` | `DBPurgeScheduled`, `DBPurgeSuspended` | `DBPurgeJobFailed` |
| `ExtraConfigHealthy` | `NoOwnedKeysOverridden` | `OwnedKeysOverridden` |

Three of those True reasons describe an absence rather than a success.
`NoBackends` and `NoBackupBackend` say that no storage is attached, which is a
posture and not a fault: the API and the scheduler run either way.
`BackupNotConfigured` is the workload side of the second one. `DBPurgeSuspended`
is the fourth of its kind: a paused purge is an operator's decision, so the
condition stays True while the metric simply stops incrementing.

`TargetClusterUnavailable` is set ahead of every sub-reconciler, when
`spec.targetClusterRef` names a target cluster that is not registered or no
longer resolves. The CR requeues after 15 seconds and acquires no finalizer, and
nothing is created on any cluster. An attached satellite reports the same reason
on its own `CredentialsReady`. See [Target Clusters](../target-clusters.md).

The two satellite controllers set `CredentialsReady`, `ConfigProjected` and
their own aggregate `Ready`. Their vocabulary lives with the kinds:
[CinderBackend](./cinder-backend-crd.md#conditions) and
[CinderBackupBackend](./cinder-backup-backend-crd.md#conditions).

## Database

The Database step runs the shared provisioning flow first: a MariaDB cluster
gate and `Database`/`User`/`Grant` in managed mode, a no-op in brownfield. It
then waits for a rendered config, because the migration Jobs mount the config
ConfigMap as their whole config directory and an empty name would render a
volume the API server rejects (`DatabaseReady=False`, reason
`WaitingForConfig`).

Before either path advances release tracking, the step compares a tag-pinned
`spec.image` against `spec.openStackRelease`. The two fields are deliberately
separate, so that a digest-pinned image still resolves a schema, and nothing
else enforces that they agree: an image whose tag names a different release
would run the wrong `cinder-manage` binary against a schema already at its own
head, and the phase Jobs would exit 0 as no-ops while `installedRelease` was
promoted to a release the pods do not run. A mismatch sets
`DatabaseReady=False` under `ImageReleaseMismatch` and requeues. A
digest-pinned image and an unparseable tag carry no comparable release and are
left to the explicit `spec.openStackRelease` declaration; a patch suffix such as
`2026.1-p1` still matches `2026.1`.

Steady state is a single `{name}-db-sync` Job running `cinder-manage db sync`,
which applies every pending alembic revision in one idempotent pass. There is no
schema-check Job, so `SchemaDriftDetected` never fires for Cinder.
`installedRelease` is promoted on Job success.

### Connection cap

The MariaDB `User` is created with a `max_user_connections` cap sized for the
CR's own topology, because the mariadb-operator default of 10 is exceeded by the
default topology before a single request is served, and a cap below the fleet's
steady state does not degrade gracefully: the last processes to start fail their
pool with MySQL error 1226 and crash-loop their pod.

```text
(apiPods + 1) x uwsgiProcesses x uwsgiThreads x 2
  + 2 x (schedulerReplicas + volumeDeployments + 1)
  + 2
```

`apiPods` is the autoscaling ceiling when an HPA owns the count, and the `+ 1`
beside it is the rolling-update surge. Each API worker process holds two pooled
connections: the request-serving session, plus the one oslo.db's pool keeps open
behind it. The second line is the single-writer services, each with the same
pair: every scheduler pod, every `cinder-volume` Deployment, and the one backup
service (the `+ 1` inside the parenthesis). The trailing `+ 2` is headroom for a
migration Job overlapping the fleet.

### Release upgrade

A `spec.openStackRelease` bump with the image in lockstep dispatches to the
shared expand-migrate-contract flow (`internal/common/database`), the same phase
machine Keystone and Glance run. The step validates the path against
`installedRelease` and rejects a downgrade or a non-sequential jump
(`VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`), then walks
four phases.

| Phase | What runs | Job |
| --- | --- | --- |
| `Expanding` | `cinder-manage db sync`. Cinder has no separate expand verb, and the sync is the same idempotent upgrade-to-head the steady state runs | `{name}-db-expand` |
| `Migrating` | `cinder-status upgrade check`, which reports readiness for the target release | `{name}-db-migrate` |
| `RollingUpdate` | No Job. The scheduler, the volume services, the backup service and the API roll onto the new image, in that order, because that is the order the pipeline ensures them in. Each step requeues until its Deployments have fully converged, so a later phase never runs against a process still on the old image | |
| `Contracting` | `cinder-manage db online_data_migrations`, the backfill the new schema needs, which the completed rollout makes safe | `{name}-db-contract` |

The upgrade check's exit code is a severity rather than a success flag: 0 is
clean, 1 carries warnings, and 2 means a check failed, which for a Cinder is
typically a volume whose `service_uuid` is still NULL and which the service
backfills on its own. None of the three may wedge an upgrade, so the phase Job
normalises 0, 1 and 2 to a zero exit and lets anything else fail. The real code
travels in the pod's termination message, which the operator turns into an
`UpgradeCheckCompleted` or `UpgradeCheckWarnings` event.

`installedRelease` is promoted only after the contract phase completes. That
promotion changes the `cinder.c5c3.io/installed-release` pod annotation, which
rolls the scheduler, the volume services and the backup service once more. Each
of them caches the RPC version its peers announced at startup, and the cache
pins the wire format for the life of the process, so the second roll is what
drops it. The API carries no such annotation: it rolled during `RollingUpdate`
and came up holding the new minimum.

## Volume services and the detach sequence

The VolumeServices step projects one Deployment per backend rather than one
Deployment serving all of them. A `cinder-volume` owns its backend through a
host identity, so a process serving two backends could not be restarted for one
of them alone, and a backend whose export is unreachable would take its siblings
down with it.

A deleted `CinderBackend` is held by the
`cinder.openstack.c5c3.io/service-remove` finalizer, and this step releases it,
one backend per pass, in name order:

1. The volume Deployment is deleted. The pass returns here rather than
   continuing, because a running `cinder-volume` reports itself back into the
   service registry every few seconds and an entry removed underneath it would
   come back.
2. Once a later pass finds the Deployment gone, the
   `{name}-{backend}-service-remove` Job runs `cinder-manage service remove
   cinder-volume {name}@{backend}` against the same database and with the same
   environment the volume service ran with. Exit code 2 is tolerated.
3. The backend's projection Secrets are swept whole, with no rollback history:
   they name an export this Cinder no longer serves.
4. The finalizer is released, a `ServiceRemoved` event is emitted on the Cinder,
   and the run feeds the `cinder_operator_service_remove_*` pair once per
   (backend, Job UID) tuple.

A failed Job stops at step 2 and keeps the finalizer:
`VolumeServicesReady=False` under `ServiceRemoveJobFailed` names the Job, and
deleting that Job retries the removal once the cause is understood.

## Requeue semantics

| Interval | Constant | Used by |
| --- | --- | --- |
| 1s | `RequeueNextPass` | After a finalizer add, and after the `RollingUpdate` to `Contracting` flip |
| 10s | `RequeueDeploymentPolling` | Deployment readiness polling, the rollout wait of every workload step, and each stage of a detach |
| 15s | `RequeueSecretPolling` | The secret-store and credential gates, the derived-Secret steps, an unresolvable target cluster, and the satellite controllers' projection backstop |
| 30s | `RequeueDatabaseWait` | The MariaDB gate, the db-sync wait, and the config wait |
| 30s | `RequeueUpgradeWait` | The expand, migrate and contract phase Jobs |
| 10s / 30s TTL | health-check retry, probe cache | A failed `/healthcheck` probe retries; a passing one is reused within the TTL |

## Watches and indexes

The Cinder controller `Owns` its Deployment, Service, ConfigMap, Secret,
PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, Job and CronJob.
The CronJob is in that list for its `status.active`: the list changes as each
spawned run starts and finishes, which is what wakes the reconcile that
refreshes `DBPurgeReady`. The HTTPRoute joins the set only when the Gateway API
CRD is installed on the management cluster; otherwise `spec.gateway` surfaces
`HTTPRouteReady=False / GatewayAPINotInstalled` instead of crashing the
controller. A CR that names a target cluster has the kind probed against that
cluster's RESTMapper on every pass, and the same children are watched once more
on the clusters a CR can project onto, by ownership label rather than by owner
reference.

Beyond the owned set it watches:

- Secrets, mapped to the Cinder CRs that reference them by name through the
  `spec.secretRefs.name` field index (the union of
  `spec.database.secretRef.name`, `spec.serviceUser.secretRef.name` and
  `spec.messaging.secretRef.name`) or own them through an owner reference. There
  is no satellite leg: an NFS export references no Secret, so no Secret event
  can reach a Cinder through one.
- MariaDB clusters referenced by `spec.database.clusterRef`, so an upstream
  database outage reflects in `DatabaseReady` without waiting for a periodic
  requeue.
- Both the cluster-scoped `ClusterSecretStore` and the namespaced `SecretStore`
  a Cinder can select, so a store-backend outage reflects in `SecretsReady`.
- `CinderBackend` and `CinderBackupBackend`, mapped to their parent through the
  two `spec.cinderRef.name` field indexes, with no generation predicate: the
  status flip to `CredentialsReady=True` and the deletion timestamp are the
  signals. Both legs stay local, because a satellite is a management-plane CR
  and lives nowhere else.

The Cinder reconciler registers all three field indexes at setup, so it has to
be set up before the two satellite controllers. Each satellite controller in
turn watches its parent Cinder with no generation predicate: the rendered
section landing in a Deployment is the wake signal its `ConfigProjected` gate
waits on.

## Rotation and deletion

Credential rotation happens at the OpenBao source. The service-user password,
the assembled DSN and the transport URL are all environment-delivered, so a
restart is required for a rotation to take effect. The three credential steps
digest each value into a pod-template annotation
(`cinder.c5c3.io/authtoken-hash`, `cinder.c5c3.io/db-connection-hash`,
`cinder.c5c3.io/transport-url-hash`), so a changed digest rolls the workloads
that carry it.

Deletion runs the `cinder.openstack.c5c3.io/finalizer`. It issues Delete on the
MariaDB `Database`/`User`/`Grant` CRs before the owner-ref chain disappears,
emitting `FinalizingDatabase` while cleanup remains and `DatabaseFinalized` when
the finalizer is released, then drops the per-CR metrics and evicts the health
probe cache. Every other owned resource is namespace-scoped with a controller
owner reference, so Kubernetes garbage collection reclaims it. A CR whose target
cluster was deregistered in the meantime cannot reach any of it: the finalizer
is released against no cleanup at all, a `RemoteChildrenAbandoned` Warning names
what stays behind, and the CR leaves etcd instead of hanging in Terminating.

Deleting a Cinder does not unregister its volume services. The registry lives in
the database that is being torn down, so a `CinderBackend` whose parent is
already gone releases its finalizer unconditionally and records a
`ServiceRemoveSkipped` event.
