---
title: Cinder Controller Events
quadrant: operator
---

# Cinder Controller Events

Reference documentation for the Kubernetes events the Cinder controllers emit.
The events make the lifecycle transitions readable through
`kubectl describe cinder` and `kubectl get events`, without access to the
controller logs.

Events complement the status conditions: a condition reflects current state for
programmatic consumers, while an event is a timestamped record of a transition
for human operators and alerting systems.

For the reconciler architecture and the sub-reconciler contracts, see
[Cinder Reconciler Architecture](./cinder-reconciler.md). For the two satellite
kinds whose contract is carried by conditions, see
[CinderBackend CRD](./cinder-backend-crd.md) and
[CinderBackupBackend CRD](./cinder-backup-backend-crd.md).

---

## Event Conventions

All events follow these conventions:

- Reason strings are stable PascalCase identifiers. They are part of the
  controllers' public API and will not change without a deprecation notice.
- Normal type indicates successful completion of a lifecycle transition.
- Warning type indicates a failure, a validation error, or a condition that
  requires operator attention.
- No events are emitted for in-progress or polling states, such as a db-sync Job
  that is still running. This keeps repeated requeue cycles from producing event
  noise.
- Almost every event is emitted on the Cinder CR (`involvedObject.kind:
  Cinder`), including the ones a satellite's detach produces: the satellite is
  being deleted, so an event on it is collected with it, while the Cinder is
  where an operator reads why a detach is stuck. The one exception is
  `ServiceRemoveSkipped`, which lands on the `CinderBackend`. The
  `CinderBackupBackend` controller emits no events at all.
- The Kubernetes API server deduplicates events by (involvedObject, reason,
  message, source). Repeated identical events increment a counter rather than
  creating new event objects.

---

## Event Reasons Reference

### Storage attachment

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `CinderBackendSkipped` | Warning | An attached volume backend carries no `spec.nfs` block, or a rendered section value carries a control character; the backend is skipped while its healthy siblings keep projecting | `Skipping backend nfs-a: <error>` |
| `CinderBackupBackendSkipped` | Warning | The attached backup backend carries no `spec.nfs` block, or its rendered section carries a control character; nothing is projected and `BackupBackendReady` stays in its waiting state | `Skipping backup backend nfs-backups: <error>` |
| `SharedExportMountOptionsIgnored` | Warning | Two volume backends serve the same export, which the backup pod mounts once, and their `mountOptions` differ; the backup service mounts it with the options of the backend projected first, and the other backend's are not applied there | `Backend nfs-b serves the export nfs.example.com:/exports/volumes that backend nfs-a already mounts with "nfsvers=4.1,soft,timeo=30,retrans=2" in the backup service, so its own mountOptions "nfsvers=3,soft" are not applied there` |

**Source:** `reconcileBackends` in `reconcile_backends.go`;
`reconcileBackupBackend` in `reconcile_backup_backend.go`;
`reconcileBackupService` in `reconcile_backupservice.go`

### Backend detach

| Reason | Type | Object | Trigger Condition | Example Message |
| --- | --- | --- | --- | --- |
| `ServiceRemoved` | Normal | Cinder | The service-remove Job succeeded and the backend's finalizer was released | `Removed the volume service cinder@nfs-a from the service registry` |
| `ServiceRemoveJobFailed` | Warning | Cinder | The service-remove Job failed permanently; the finalizer stays and deleting the Job retries the removal | `Job cinder-nfs-a-service-remove could not remove the volume service of backend nfs-a` |
| `ServiceRemoveSkipped` | Normal | CinderBackend | The backend is deleting and its parent Cinder no longer exists, so no registry row can be removed and the finalizer is released unconditionally | `Parent Cinder openstack/cinder no longer exists, so no service registry row can be removed; released the backend` |
| `ServiceRemoveMetricEmissionDeferred` | Warning | Cinder | Patching the last-observed Job UID annotation failed, deferring the `service_remove` metric emission to the next reconcile | `Patching last-observed service-remove-nfs-a Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** `detachBackend` in `reconcile_volumeservices.go`
(`ServiceRemoved` / `ServiceRemoveJobFailed`); `reconcileDeleting` in
`cinderbackend_controller.go` (`ServiceRemoveSkipped`); the shared
`RecordJobTerminalState` in `internal/common/job/terminal.go`
(`ServiceRemoveMetricEmissionDeferred`)

### Configuration

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `ExtraConfigOwnedKeyOverride` | Warning | `spec.extraConfig` overrides one or more operator-owned configuration keys (the per-service ownership registry) | `spec.extraConfig overrides operator-owned keys: [DEFAULT] host (the volumes a backend owns are keyed by this identity, so renaming it strands them on a host no service backs)` |

**Source:** `reconcileConfig` in `reconcile_config.go`

> **Note:** The event is gated on the `ExtraConfigHealthy=False` condition's
> message. It fires once on the transition into `False` and once more when the
> overridden-key set changes, never on the steady reconcile poll. Removing the
> overrides transitions the condition back to `ExtraConfigHealthy=True,
> Reason=NoOwnedKeysOverridden` without a further event. The condition is
> informational and is not aggregated into `Ready`. The keys the webhook
> rejects outright never reach it; they are listed under
> [extraConfig](./cinder-crd.md#extraconfig).

### Database Sync

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DatabaseSynced` | Normal | The `db-sync` Job completes successfully | `Database schema is up to date` |
| `DBSyncFailed` | Warning | The `db-sync` Job fails | `db_sync job failed: <error>` |
| `DBSyncMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation fails, deferring the `db_sync` metric emission to the next reconcile | `Patching last-observed db-sync Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** the shared `ReconcileSyncJobs` in
`internal/common/database/flow.go` (`DatabaseSynced` / `DBSyncFailed`);
`recordDBJobTerminalState` in `db_job_metrics.go`
(`DBSyncMetricEmissionDeferred`)

> **Note:** Cinder runs a single `cinder-manage db sync` with no separate
> schema-check Job, so `SchemaDriftDetected` never fires for Cinder.

### DB Purge

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DBPurgeJobFailed` | Warning | The newest terminal Job spawned by the `{name}-db-purge` CronJob failed | `Database purge Job cinder-db-purge-29387520 failed; inspect its pod logs` |
| `DBPurgeMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation fails, deferring the `db_purge` metric emission to the next reconcile | `Patching last-observed db-purge Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** `reconcileDBPurge` in `reconcile_dbpurge.go` (`DBPurgeJobFailed`);
the shared `RecordJobTerminalState` in `internal/common/job/terminal.go`
(`DBPurgeMetricEmissionDeferred`)

> **Note:** There is no Normal event for a successful purge run; a succeeding
> run only flips `DBPurgeReady` back to `True`. A run that wedges rather than
> fails reaches a terminal `Failed` state through the Job's
> `activeDeadlineSeconds` within the hour, so it surfaces on the same path.
> `spec.dbPurge.suspend` pauses the CronJob without deleting it and raises no
> event: a suspended purge is visible only through the `DBPurgeSuspended`
> condition reason.

### Upgrade

A release transition (a `spec.openStackRelease` bump with the image in lockstep)
walks the shared expand-migrate-contract flow, which emits these events on the
Cinder CR. For the phase machine see
[Release upgrade](./cinder-reconciler.md#release-upgrade).

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `UpgradeInitiated` | Normal | An accepted release bump starts the upgrade | `Upgrade initiated: 2025.2 to 2026.1` |
| `ExpandComplete` | Normal | The expand phase Job succeeded | `Expand phase complete: 2025.2 to 2026.1` |
| `MigrateComplete` | Normal | The migrate phase Job succeeded | `Migrate phase complete: 2025.2 to 2026.1` |
| `UpgradeCheckCompleted` | Normal | The migrate phase's `cinder-status upgrade check` exited 0, or its exit code could not be read off the pod | `cinder-status upgrade check exit 0` |
| `UpgradeCheckWarnings` | Warning | The same check exited 1 (warnings) or 2 (a failed check, typically a volume whose `service_uuid` is still NULL); neither wedges the upgrade | `cinder-status upgrade check exit 2` |
| `DeploymentRolloutComplete` | Normal | The API Deployment rolled out; the phase flips to Contracting | `Deployment rollout complete during upgrade 2025.2 to 2026.1` |
| `UpgradeComplete` | Normal | The contract phase Job succeeded; the upgrade finished | `Upgrade complete: 2025.2 to 2026.1` |
| `UpgradeAborted` | Normal | `spec.openStackRelease` reverted to the installed release, cancelling the upgrade | `Upgrade 2025.2 to 2026.1 aborted: spec release reverted to installed release 2025.2` |
| `UpgradeCheckEventEmissionDeferred` | Warning | Patching the last-observed Job UID annotation failed, deferring the upgrade-check report to the next reconcile | `Patching last-observed db-migrate-check Job UID failed; metric emission deferred to the next reconcile: <error>` |
| `VersionParseError` | Warning | The installed or target release is not a valid `YYYY.N` string | `Failed to parse target release "latest": ...` |
| `DowngradeNotSupported` | Warning | The target release is older than the installed release | `Downgrade from 2026.1 to 2025.2 is not supported` |
| `UpgradePathInvalid` | Warning | The requested jump is not a single sequential step | `Upgrade from 2024.2 to 2026.1 is not sequential` |
| `UpgradeTargetChanged` | Warning | `spec.openStackRelease` changed to a third value during an active upgrade | `Spec release changed to 2026.2 during active upgrade 2025.2 to 2026.1` |
| `ExpandFailed` | Warning | The expand phase Job failed permanently | `Expand job cinder-db-expand failed: ...` |
| `MigrateFailed` | Warning | The migrate phase Job failed permanently | `Migrate job cinder-db-migrate failed: ...` |
| `ContractFailed` | Warning | The contract phase Job failed permanently | `Contract job cinder-db-contract failed: ...` |

**Source:** the shared expand-migrate-contract flow in
`internal/common/database/upgrade.go`; `reportUpgradeCheck` in
`reconcile_database.go` for the two check events

### Finalization

Emitted while the finalizer tears a deleted Cinder CR down.
`FinalizingDatabase` is gated on live MariaDB cleanup work remaining, so
brownfield CRs and repeated requeue polls produce no noise.

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FinalizingDatabase` | Normal | Deletion begins while MariaDB Database/User/Grant CRs are still live | `Cleaning up MariaDB Database, User, and Grant before removing Cinder` |
| `DatabaseFinalized` | Normal | The MariaDB resources are marked for deletion; the finalizer is released | `MariaDB Database, User, and Grant marked for deletion; releasing finalizer` |
| `RemoteChildrenAbandoned` | Warning | Deletion begins while the target cluster the CR named no longer resolves; the finalizer is released without touching what was written there | `Target cluster is no longer registered; releasing the finalizer without deleting the MariaDB Database, User, and Grant on it` |

**Source:** `reconcileDelete` in `cinder_controller.go`

---

## Alerting Configuration

Event reason strings are stable identifiers for alerting rules. Use
`kubectl get events --field-selector` to filter by reason:

```bash
# Watch for a failed backend detach
kubectl get events --field-selector reason=ServiceRemoveJobFailed -w

# Watch for a failed database purge
kubectl get events --field-selector reason=DBPurgeJobFailed -w

# Watch for a skipped volume backend
kubectl get events --field-selector reason=CinderBackendSkipped -w

# Watch for all Warning events from the cinder-controller
kubectl get events --field-selector type=Warning,reportingComponent=cinder-controller -w
```

### Prometheus Alertmanager Example

With [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics) and
event metrics enabled, the two Warning reasons that leave work undone are worth
alerting on:

```yaml
groups:
  - name: cinder-events
    rules:
      - alert: CinderDBPurgeFailed
        expr: |
          increase(kube_event_count{
            reason="DBPurgeJobFailed",
            involved_object_kind="Cinder"
          }[5m]) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "Cinder database purge failed"
          description: "The Cinder db-purge Job has failed. Soft-deleted volume, snapshot and backup rows keep accumulating until a run succeeds; check the Job logs for details."

      - alert: CinderServiceRemoveFailed
        expr: |
          increase(kube_event_count{
            reason="ServiceRemoveJobFailed",
            involved_object_kind="Cinder"
          }[5m]) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "Cinder backend detach is stuck"
          description: "A service-remove Job failed, so the detaching CinderBackend keeps its finalizer and stays in Terminating. Check the Job logs, then delete the Job to retry the removal."

      - alert: CinderUpgradePhaseFailed
        expr: |
          increase(kube_event_count{
            reason=~"ExpandFailed|MigrateFailed|ContractFailed",
            involved_object_kind="Cinder"
          }[5m]) > 0
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "Cinder database upgrade phase failed"
          description: "An expand, migrate, or contract Job failed during a Cinder release upgrade. Check the phase Job logs and consider aborting the upgrade."
```

---

## Event Flow

```text
CinderReconciler.Reconcile()
  │
  ├── reconcileDelete() (deletionTimestamp set)
  │     ├─ MariaDB CRs still live  → Normal  FinalizingDatabase
  │     ├─ MariaDB cleanup done    → Normal  DatabaseFinalized
  │     └─ target cluster gone     → Warning RemoteChildrenAbandoned
  │
  ├── reconcileBackends()
  │     └─ per-backend fault           → Warning CinderBackendSkipped
  │
  ├── reconcileBackupBackend()
  │     └─ backup-target fault         → Warning CinderBackupBackendSkipped
  │
  ├── reconcileConfig()
  │     └─ extraConfig overrides an operator-owned key
  │                                    → Warning ExtraConfigOwnedKeyOverride
  │
  ├── reconcileDatabase()
  │     ├─ db_sync fails               → Warning DBSyncFailed
  │     ├─ db_sync succeeds            → Normal  DatabaseSynced
  │     ├─ Job-UID patch fails         → Warning DBSyncMetricEmissionDeferred
  │     ├─ migrate phase terminal      → Normal  UpgradeCheckCompleted /
  │     │                                Warning UpgradeCheckWarnings
  │     └─ release upgrade             → Normal  UpgradeInitiated / ExpandComplete /
  │                                              MigrateComplete / UpgradeComplete /
  │                                              UpgradeAborted
  │                                      Warning VersionParseError / DowngradeNotSupported /
  │                                              UpgradePathInvalid / UpgradeTargetChanged /
  │                                              ExpandFailed / MigrateFailed / ContractFailed
  │
  ├── reconcileVolumeServices()
  │     ├─ detach completed            → Normal  ServiceRemoved
  │     ├─ service-remove Job failed   → Warning ServiceRemoveJobFailed
  │     └─ Job-UID patch fails         → Warning ServiceRemoveMetricEmissionDeferred
  │
  ├── reconcileBackupService()
  │     └─ shared export, divergent mountOptions
  │                                    → Warning SharedExportMountOptionsIgnored
  │
  ├── reconcileDeployment()
  │     └─ rollout ready mid-upgrade   → Normal  DeploymentRolloutComplete
  │
  └── reconcileDBPurge()
        ├─ newest run failed           → Warning DBPurgeJobFailed
        └─ Job-UID patch fails         → Warning DBPurgeMetricEmissionDeferred

CinderBackendReconciler.Reconcile()
  └── reconcileDeleting() (parent Cinder already gone)
        └─ finalizer released          → Normal  ServiceRemoveSkipped
```
