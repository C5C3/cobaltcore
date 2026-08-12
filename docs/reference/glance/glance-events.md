---
title: Glance Controller Events
quadrant: operator
---

# Glance Controller Events

Reference documentation for Kubernetes events emitted by the Glance controller.
The controller emits events on key lifecycle transitions to provide
observability via `kubectl describe glance` and `kubectl get events` without
requiring access to controller logs.

Events complement status conditions: conditions reflect current state for
programmatic consumers, while events provide a timestamped audit trail of
transitions for human operators and alerting systems.

For the reconciler architecture and sub-reconciler contracts, see
[Glance Reconciler Architecture](./glance-reconciler.md). For the per-store CRD
whose contract is carried by conditions rather than events, see
[GlanceBackend CRD](./glance-backend-crd.md).

---

## Event Conventions

All events follow these conventions:

- **Reason strings** are stable PascalCase identifiers. They are part of the
  controller's public API and will not change without a deprecation notice.
- **Normal** type indicates successful completion of a lifecycle transition.
- **Warning** type indicates a failure, validation error, or unexpected
  condition that requires operator attention.
- **No events are emitted for in-progress/polling states** (e.g. while the
  db-sync Job is still running). This prevents event noise from repeated requeue
  cycles.
- Every Glance event is emitted on the **Glance** CR (`involvedObject.kind:
  Glance`). The `GlanceBackend` controller emits **no events at all** — its
  `CredentialsReady` / `ConfigProjected` / `Ready` conditions carry the entire
  per-backend contract.
- The Kubernetes API server deduplicates events by (involvedObject, reason,
  message, source). Repeated identical events increment a counter rather than
  creating new event objects.

---

## Event Reasons Reference

### Backends

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `GlanceBackendSkipped` | Warning | An attached backend's credentials Secret is missing/unreadable, or a rendered store-section value carries a control character; the backend is skipped while healthy siblings keep projecting | `Skipping backend garage-a: <error>` |

**Source:** `reconcileBackends` in `reconcile_backends.go`

### Configuration

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `ExtraConfigOwnedKeyOverride` | Warning | `spec.extraConfig` overrides one or more operator-owned configuration keys (the per-service ownership registry) | `spec.extraConfig overrides operator-owned keys: [DEFAULT] enabled_backends (must agree with the projected backends Secret)` |

**Source:** `reconcileConfig` in `reconcile_config.go`

> **Note:** The event is gated on the `ExtraConfigHealthy=False` condition's
> message — it fires once on the transition into `False` and once more when the
> overridden-key set changes, never on the steady reconcile poll. Removing the
> overrides transitions the condition back to `ExtraConfigHealthy=True,
> Reason=NoOwnedKeysOverridden` without a further event. The condition is
> informational and is not aggregated into `Ready`.

### Database Sync

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DatabaseSynced` | Normal | The `db-sync` Job completes successfully | `Database schema is up to date` |
| `DBSyncFailed` | Warning | The `db-sync` Job fails | `db_sync job failed: <error>` |
| `DBSyncMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation fails, deferring `db_sync` metric emission to the next reconcile | `Patching last-observed db-sync Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** the shared `ReconcileSyncJobs` in `internal/common/database/flow.go`
(`DatabaseSynced` / `DBSyncFailed`); `recordDBJobTerminalState` in
`db_job_metrics.go` (`DBSyncMetricEmissionDeferred`)

> **Note:** Glance runs a single `glance-manage db sync` with no separate
> schema-check Job, so — unlike Keystone — **`SchemaDriftDetected` never fires
> for Glance**.

### DB Purge

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DBPurgeJobFailed` | Warning | The newest terminal Job spawned by the `{name}-db-purge` CronJob failed | `Database purge Job <name> failed; inspect its pod logs — the soft-deleted image and task rows keep accumulating until a run succeeds` |
| `DBPurgeMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation fails, deferring `db_purge` metric emission to the next reconcile | `Patching last-observed db-purge Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** `reconcileDBPurge` in `reconcile_dbpurge.go` (`DBPurgeJobFailed`);
the shared `RecordJobTerminalState` in `internal/common/job/terminal.go`
(`DBPurgeMetricEmissionDeferred`)

> **Note:** There is no `Normal` event for a successful purge run — a
> succeeding run only flips `DBPurgeReady` back to `True`. The CronJob is
> projected on every Glance regardless of `spec.dbPurge`; `spec.dbPurge.suspend`
> pauses it without deleting it, and a suspended purge keeps `DBPurgeReady`
> `True`, so it raises no event either — it is visible only through the
> condition's `DBPurgeSuspended` reason.

### Upgrade

A release transition (a `spec.openStackRelease` bump with the image in lockstep)
walks the shared expand-migrate-contract flow, which emits these events on the
Glance CR. For the phase machine see the
[Glance Upgrade Flow](./glance-upgrade-flow.md).

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `UpgradeInitiated` | Normal | An accepted release bump starts the upgrade | `Upgrade initiated: 2025.2 → 2026.1` |
| `ExpandComplete` | Normal | The expand phase Job succeeded | `Expand phase complete: 2025.2 → 2026.1` |
| `MigrateComplete` | Normal | The migrate phase Job succeeded | `Migrate phase complete: 2025.2 → 2026.1` |
| `DeploymentRolloutComplete` | Normal | The Deployment rolled out; the phase flips to Contracting | `Deployment rollout complete during upgrade 2025.2 → 2026.1` |
| `UpgradeComplete` | Normal | The contract phase Job succeeded; the upgrade finished | `Upgrade complete: 2025.2 → 2026.1` |
| `UpgradeAborted` | Normal | `spec.openStackRelease` reverted to the installed release, cancelling the upgrade | `Upgrade 2025.2 → 2026.1 aborted: spec release reverted to installed release 2025.2` |
| `VersionParseError` | Warning | The installed or target release is not a valid `YYYY.N` string | `Failed to parse target release "latest": ...` |
| `DowngradeNotSupported` | Warning | The target release is older than the installed release | `Downgrade from 2026.1 to 2025.2 is not supported` |
| `UpgradePathInvalid` | Warning | The requested jump is not a single sequential step | `Upgrade from 2024.2 to 2026.1 is not sequential` |
| `UpgradeTargetChanged` | Warning | `spec.openStackRelease` changed to a third value during an active upgrade | `Spec release changed to 2026.2 during active upgrade 2025.2 → 2026.1` |
| `ExpandFailed` | Warning | The expand phase Job failed permanently | `Expand job glance-db-expand failed: ...` |
| `MigrateFailed` | Warning | The migrate phase Job failed permanently | `Migrate job glance-db-migrate failed: ...` |
| `ContractFailed` | Warning | The contract phase Job failed permanently | `Contract job glance-db-contract failed: ...` |

**Source:** the shared expand-migrate-contract flow in
`internal/common/database/upgrade.go`

### Finalization

Emitted while the finalizer tears a deleted Glance CR down. `FinalizingDatabase`
is gated on live MariaDB cleanup work remaining, so brownfield CRs (no MariaDB
CRs) and repeated requeue polls do not produce noise.

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FinalizingDatabase` | Normal | Deletion begins while MariaDB Database/User/Grant CRs are still live | `Cleaning up MariaDB Database, User, and Grant before removing Glance` |
| `DatabaseFinalized` | Normal | MariaDB resources marked for deletion; finalizer released | `MariaDB Database, User, and Grant marked for deletion; releasing finalizer` |
| `RemoteChildrenAbandoned` | Warning | Deletion begins while the target cluster the CR named no longer resolves; the finalizer is released without touching what was written there | `Target cluster is no longer registered; releasing the finalizer without deleting the MariaDB Database, User, and Grant on it` |

**Source:** `reconcileDelete` in `glance_controller.go`

---

## Alerting Configuration

Event reason strings are designed to be stable identifiers for alerting rules.
Use `kubectl get events --field-selector` to filter by reason:

```bash
# Watch for db-sync failures
kubectl get events --field-selector reason=DBSyncFailed -w

# Watch for a rejected release upgrade path
kubectl get events --field-selector reason=UpgradePathInvalid -w

# Watch for a skipped image-store backend
kubectl get events --field-selector reason=GlanceBackendSkipped -w

# Watch for all Warning events from the glance-controller
kubectl get events --field-selector type=Warning,reportingComponent=glance-controller -w
```

### Prometheus Alertmanager Example

When using [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics)
with event metrics enabled, you can alert on specific event reasons:

```yaml
groups:
  - name: glance-events
    rules:
      - alert: GlanceDBSyncFailed
        expr: |
          increase(kube_event_count{
            reason="DBSyncFailed",
            involved_object_kind="Glance"
          }[5m]) > 0
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "Glance db-sync failed"
          description: "The Glance db-sync Job has failed. Check the Job logs for details."

      - alert: GlanceDBPurgeFailed
        expr: |
          increase(kube_event_count{
            reason="DBPurgeJobFailed",
            involved_object_kind="Glance"
          }[5m]) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "Glance database purge failed"
          description: "The Glance db-purge Job has failed. Soft-deleted image and task rows keep accumulating until a run succeeds; check the Job logs for details."

      - alert: GlanceUpgradePhaseFailed
        expr: |
          increase(kube_event_count{
            reason=~"ExpandFailed|MigrateFailed|ContractFailed",
            involved_object_kind="Glance"
          }[5m]) > 0
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "Glance database upgrade phase failed"
          description: "An expand, migrate, or contract Job failed during a Glance release upgrade. Check the phase Job logs and consider aborting the upgrade."
```

---

## Event Flow

```text
GlanceReconciler.Reconcile()
  │
  ├── reconcileDelete() (deletionTimestamp set)
  │     ├─ MariaDB CRs still live  → Normal  FinalizingDatabase
  │     └─ MariaDB cleanup done    → Normal  DatabaseFinalized
  │
  ├── reconcileBackends()
  │     └─ per-backend fault (missing Secret / control char) → Warning GlanceBackendSkipped
  │
  ├── reconcileConfig()
  │     └─ spec.extraConfig overrides operator-owned keys → Warning ExtraConfigOwnedKeyOverride
  │       (gated on transition into ExtraConfigHealthy=False, Reason=OwnedKeysOverridden)
  │
  ├── reconcileDatabase()
  │     ├─ db_sync fails              → Warning DBSyncFailed
  │     ├─ db_sync succeeds           → Normal  DatabaseSynced
  │     ├─ Job-UID patch fails        → Warning DBSyncMetricEmissionDeferred
  │     └─ release upgrade            → Normal  UpgradeInitiated / ExpandComplete /
  │                                             MigrateComplete / UpgradeComplete / UpgradeAborted
  │                                      Warning VersionParseError / DowngradeNotSupported /
  │                                             UpgradePathInvalid / UpgradeTargetChanged /
  │                                             ExpandFailed / MigrateFailed / ContractFailed
  │
  ├── reconcileDeployment()
  │     └─ rollout ready mid-upgrade  → Normal  DeploymentRolloutComplete
  │
  └── reconcileDBPurge()
        ├─ newest run failed          → Warning DBPurgeJobFailed
        └─ Job-UID patch fails        → Warning DBPurgeMetricEmissionDeferred
```
