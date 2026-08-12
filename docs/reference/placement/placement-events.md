---
title: Placement Controller Events
quadrant: operator
---

# Placement Controller Events

Reference documentation for Kubernetes events emitted by the Placement
controller. The controller emits events on key lifecycle transitions to provide
observability via `kubectl describe placement` and `kubectl get events` without
requiring access to controller logs.

Events complement status conditions: conditions reflect current state for
programmatic consumers, while events provide a timestamped audit trail of
transitions for human operators and alerting systems.

For the reconciler architecture and sub-reconciler contracts, see
[Placement Reconciler Architecture](./placement-reconciler.md). For the spec and
status contract the conditions belong to, see
[Placement CRD](./placement-crd.md).

---

## Event Conventions

All events follow these conventions:

- **Reason strings** are stable PascalCase identifiers. They are part of the
  controller's public API and will not change without a deprecation notice.
- **Normal** type indicates successful completion of a lifecycle transition.
- **Warning** type indicates a failure, validation error, or unexpected condition
  that requires operator attention.
- **No events are emitted for in-progress/polling states** (e.g. while the
  db-sync Job is still running). This prevents event noise from repeated requeue
  cycles.
- Every event is emitted on the **Placement** CR (`involvedObject.kind:
  Placement`) by the recorder named `placement-controller`, which is what the
  `reportingComponent` field selector matches.
- The Kubernetes API server deduplicates events by (involvedObject, reason,
  message, source). Repeated identical events increment a counter rather than
  creating new event objects.

---

## Event Reasons Reference

### Configuration

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `ExtraConfigOwnedKeyOverride` | Warning | `spec.extraConfig` overrides one or more operator-owned configuration keys (the per-service ownership registry) | `spec.extraConfig overrides operator-owned keys: [placement_database] connection (the runtime value comes from the OS_PLACEMENT_DATABASE__CONNECTION env override, so the file override is ignored)` |

**Source:** the shared `RecordExtraConfigHealth` in
`internal/common/config/ownership.go`, called from `reconcileConfig` in
`reconcile_config.go`

> **Note:** The event is gated on the `ExtraConfigHealthy=False` condition's
> message. It fires once on the transition into `False` and once more when the
> overridden-key set changes, never on the steady reconcile poll. Removing the
> overrides transitions the condition back to `ExtraConfigHealthy=True,
> Reason=NoOwnedKeysOverridden` without a further event. The condition is
> informational and is not aggregated into `Ready`. The three keys the
> validating webhook refuses at admission never reach this path.

### Database Sync

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DatabaseSynced` | Normal | The `db-sync` Job completes successfully | `Database schema is up to date` |
| `DBSyncFailed` | Warning | The `db-sync` Job fails | `db_sync job failed: <error>` |
| `DBSyncMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation fails, deferring `db_sync` metric emission to the next reconcile | `Patching last-observed db-sync Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** the shared `ReconcileSyncJobs` in `internal/common/database/flow.go`
(`DatabaseSynced` / `DBSyncFailed`); the shared `RecordJobTerminalState` in
`internal/common/job/terminal.go`, wired through `recordDBJobTerminalState` in
`db_job_metrics.go` (`DBSyncMetricEmissionDeferred`)

### Release Transitions

The release gate validates a `spec.openStackRelease` change against
`status.installedRelease` before any migration Job runs. A refused transition
sets `DatabaseReady=False`, raises one Warning event whose reason names the rule
that refused it, and returns an error so the controller backs off.

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `VersionParseError` | Warning | The installed or the requested release is not a valid `YYYY.N` string | `parsing requested release "latest": invalid release format "latest": expected YYYY.N` |
| `DowngradeNotSupported` | Warning | The requested release is older than the installed one | `downgrade from 2026.1 to 2025.2 is not supported` |
| `UpgradePathInvalid` | Warning | The requested jump is more than one release | `upgrade from 2024.2 to 2026.1 is not sequential; upgrade one release at a time` |
| `ImageReleaseMismatch` | Warning | The requested release bump leaves `spec.image` at the reference that migrated the installed schema, so no migration would run | `upgrade from 2025.2 to 2026.1 leaves spec.image unchanged (ghcr.io/c5c3/placement:2025.2), so no migration would run; bump spec.image in lockstep with spec.openStackRelease` |

**Source:** `gateReleaseTransition` and `rejectReleaseTransition` in
`reconcile_database.go`

> **Note:** `ImageReleaseMismatch` has a second trigger that raises **no event**.
> When a tag-pinned `spec.image` names a different OpenStack release than
> `spec.openStackRelease`, the reconcile sets `DatabaseReady=False` with that
> reason and requeues without recording anything, because the check runs on every
> pass and would otherwise re-fire for as long as the two fields disagree. Read
> that case off the condition, not the event stream.

### Finalization

Emitted while the finalizer tears a deleted Placement CR down.
`FinalizingDatabase` is gated on live MariaDB cleanup work remaining, so
brownfield CRs (no MariaDB CRs) and repeated requeue polls do not produce noise.

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FinalizingDatabase` | Normal | Deletion begins while MariaDB Database/User/Grant CRs are still live | `Cleaning up MariaDB Database, User, and Grant before removing Placement` |
| `DatabaseFinalized` | Normal | MariaDB resources marked for deletion; finalizer released | `MariaDB Database, User, and Grant marked for deletion; releasing finalizer` |
| `RemoteChildrenAbandoned` | Warning | Deletion begins while the target cluster the CR named no longer resolves; the finalizer is released without touching what was written there | `Target cluster is no longer registered; releasing the finalizer without deleting the MariaDB Database, User, and Grant on it` |

**Source:** `reconcileDelete` in `placement_controller.go`

### Reasons that never fire for Placement

| Reason | Why it cannot occur |
| --- | --- |
| `SchemaDriftDetected` | The Job set carries no schema-check command. `placement-manage db sync` applies every pending migration in one idempotent pass, and the `placement-status upgrade check` inside the same Job already validates the result |
| `UpgradeInitiated`, `ExpandComplete`, `MigrateComplete`, `DeploymentRolloutComplete`, `UpgradeComplete`, `UpgradeAborted`, `UpgradeTargetChanged`, `ExpandFailed`, `MigrateFailed`, `ContractFailed` | These belong to the shared expand-migrate-contract flow. Placement runs no phase machine and its CR carries no `upgradePhase`; a release bump takes the same single db-sync Job a fresh install takes |

---

## Alerting Configuration

Event reason strings are designed to be stable identifiers for alerting rules.
Use `kubectl get events --field-selector` to filter by reason:

```bash
# Watch for db-sync failures
kubectl get events --field-selector reason=DBSyncFailed -w

# Watch for a rejected release upgrade path
kubectl get events --field-selector reason=UpgradePathInvalid -w

# Watch for an extraConfig override of an operator-owned key
kubectl get events --field-selector reason=ExtraConfigOwnedKeyOverride -w

# Watch for all Warning events from the placement-controller
kubectl get events --field-selector type=Warning,reportingComponent=placement-controller -w
```

### Prometheus Alertmanager Example

When using [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics)
with event metrics enabled, you can alert on specific event reasons:

```yaml
groups:
  - name: placement-events
    rules:
      - alert: PlacementDBSyncFailed
        expr: |
          increase(kube_event_count{
            reason="DBSyncFailed",
            involved_object_kind="Placement"
          }[5m]) > 0
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "Placement db-sync failed"
          description: "The Placement db-sync Job has failed. Check the Job logs for details."
```

---

## Event Flow

```text
PlacementReconciler.Reconcile()
  │
  ├── reconcileDelete() (deletionTimestamp set)
  │     ├─ MariaDB CRs still live  → Normal  FinalizingDatabase
  │     └─ MariaDB cleanup done    → Normal  DatabaseFinalized
  │
  ├── reconcileConfig()
  │     └─ spec.extraConfig overrides operator-owned keys → Warning ExtraConfigOwnedKeyOverride
  │       (gated on transition into ExtraConfigHealthy=False, Reason=OwnedKeysOverridden)
  │
  └── reconcileDatabase()
        ├─ tag/release mismatch      → (condition only, no event)
        ├─ release gate refuses      → Warning VersionParseError / DowngradeNotSupported /
        │                                      UpgradePathInvalid / ImageReleaseMismatch
        ├─ db_sync fails             → Warning DBSyncFailed
        ├─ db_sync succeeds          → Normal  DatabaseSynced
        └─ Job-UID patch fails       → Warning DBSyncMetricEmissionDeferred
```
