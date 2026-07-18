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

### Release Validation

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `InvalidReleaseTransition` | Warning | The requested `spec.openStackRelease` is a downgrade, or a jump that is neither patch-only nor a single sequential step, relative to `status.installedRelease` | `downgrade from 2026.1 to 2025.2 is not supported` |

**Source:** `rejectReleaseTransition` in `reconcile_database.go`

### Finalization

Emitted while the finalizer tears a deleted Glance CR down. `FinalizingDatabase`
is gated on live MariaDB cleanup work remaining, so brownfield CRs (no MariaDB
CRs) and repeated requeue polls do not produce noise.

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FinalizingDatabase` | Normal | Deletion begins while MariaDB Database/User/Grant CRs are still live | `Cleaning up MariaDB Database, User, and Grant before removing Glance` |
| `DatabaseFinalized` | Normal | MariaDB resources marked for deletion; finalizer released | `MariaDB Database, User, and Grant marked for deletion; releasing finalizer` |

**Source:** `reconcileDelete` in `glance_controller.go`

---

## Alerting Configuration

Event reason strings are designed to be stable identifiers for alerting rules.
Use `kubectl get events --field-selector` to filter by reason:

```bash
# Watch for db-sync failures
kubectl get events --field-selector reason=DBSyncFailed -w

# Watch for a rejected release transition
kubectl get events --field-selector reason=InvalidReleaseTransition -w

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

      - alert: GlanceInvalidReleaseTransition
        expr: |
          increase(kube_event_count{
            reason="InvalidReleaseTransition",
            involved_object_kind="Glance"
          }[5m]) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "Glance release transition rejected"
          description: "spec.openStackRelease requests an unsupported transition from the installed release."
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
  └── reconcileDatabase()
        ├─ Invalid release transition → Warning InvalidReleaseTransition
        ├─ db_sync fails              → Warning DBSyncFailed
        ├─ db_sync succeeds           → Normal  DatabaseSynced
        └─ Job-UID patch fails        → Warning DBSyncMetricEmissionDeferred
```
