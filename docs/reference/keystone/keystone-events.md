---
title: Keystone Controller Events
quadrant: operator
---

# Keystone Controller Events

Reference documentation for Kubernetes events emitted by the Keystone controller. The controller emits events on key lifecycle transitions to provide
observability via `kubectl describe keystone` and `kubectl get events` without
requiring access to controller logs.

Events complement status conditions: conditions reflect current state for
programmatic consumers, while events provide a timestamped audit trail of
transitions for human operators and alerting systems.

For the reconciler architecture and sub-reconciler contracts, see
[Keystone Reconciler Architecture](./keystone-reconciler.md). For the upgrade
flow that drives most upgrade-related events, see
[Keystone Upgrade Flow](./keystone-upgrade-flow.md). For schema drift detection
events, see [Keystone Schema Drift Detection](./keystone-schema-drift-detection.md).

---

## Event Conventions

All events follow these conventions:

- **Reason strings** are stable PascalCase identifiers. They are part of the
  controller's public API and will not change without a deprecation notice.
- **Normal** type indicates successful completion of a lifecycle transition.
- **Warning** type indicates a failure, validation error, or unexpected condition
  that requires operator attention.
- **No events are emitted for in-progress/polling states** (e.g., while a Job is
  still running). This prevents event noise from repeated requeue cycles.
- The Kubernetes API server deduplicates events by (involvedObject, reason,
  message, source). Repeated identical events increment a counter rather than
  creating new event objects.

---

## Event Reasons Reference

### Bootstrap

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `BootstrapComplete` | Normal | Bootstrap Job completes successfully | `Keystone bootstrap completed successfully` |
| `BootstrapFailed` | Warning | Bootstrap Job fails | `Keystone bootstrap job failed: <error>` |

**Source:** `reconcileBootstrap` in `reconcile_bootstrap.go`

### Database Sync (Non-Upgrade)

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DatabaseSynced` | Normal | `db_sync` and `schema-check` Jobs both complete successfully | `Database schema is up to date` |
| `DBSyncFailed` | Warning | `db_sync` Job fails | `db_sync job failed: <error>` |
| `SchemaDriftDetected` | Warning | `schema-check` Job fails after `db_sync` succeeds (schema does not match Alembic head) | `schema-check job failed: <error>` |

**Source:** `reconcileDatabase` in `reconcile_database.go`

### Upgrade Initiation

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `UpgradeInitiated` | Normal | Upgrade validated and initiated with expand-migrate-contract pipeline | `Upgrade initiated: 2025.2 → 2026.1` |
| `VersionParseError` | Warning | Installed release or target release version string cannot be parsed | `Failed to parse installed release "invalid": <error>` |
| `DowngradeNotSupported` | Warning | Target release is older than installed release | `Downgrade from 2026.1 to 2025.2 is not supported` |
| `UpgradePathInvalid` | Warning | Target release skips an intermediate version (non-sequential upgrade) | `Upgrade from 2025.1 to 2026.1 is not sequential` |
| `UpgradeTargetChanged` | Warning | `spec.image.tag` changed while an upgrade is already in progress | `Image tag changed to 2026.2 during active upgrade 2025.2 → 2026.1` |

**Source:** `initiateUpgrade` and `reconcileDatabase` in `reconcile_database.go`

### Upgrade Phases

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `ExpandComplete` | Normal | Expand phase Job completes successfully | `Expand phase complete: 2025.2 → 2026.1` |
| `ExpandFailed` | Warning | Expand phase Job fails | `Expand job <name> failed: <error>` |
| `MigrateComplete` | Normal | Migrate phase Job completes successfully | `Migrate phase complete: 2025.2 → 2026.1` |
| `MigrateFailed` | Warning | Migrate phase Job fails | `Migrate job <name> failed: <error>` |
| `UpgradeComplete` | Normal | Contract phase Job completes, finishing the entire upgrade | `Upgrade complete: 2025.2 → 2026.1` |
| `ContractFailed` | Warning | Contract phase Job fails | `Contract job <name> failed: <error>` |

**Source:** `reconcileExpand`, `reconcileMigrate`, `reconcileContract` in `reconcile_database.go`

### Encryption Key Generation

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FernetKeysGenerated` | Normal | Initial Fernet encryption keys Secret is created (first reconcile only) | `Initial Fernet encryption keys have been generated` |
| `CredentialKeysGenerated` | Normal | Initial credential encryption keys Secret is created (first reconcile only) | `Initial credential encryption keys have been generated` |

**Source:** `reconcileFernetKeys` in `reconcile_fernet.go`, `reconcileCredentialKeys` in `reconcile_credential.go`

> **Note:** These events fire only on initial Secret creation. Subsequent key
> rotations are handled by CronJobs and do not emit controller events.

### Deployment Rollout

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DeploymentRolloutComplete` | Normal | Deployment becomes ready during the `UpgradePhaseRollingUpdate` phase of an upgrade | `Deployment rollout complete during upgrade 2025.2 → 2026.1` |

**Source:** `reconcileDeployment` in `reconcile_deployment.go`

> **Note:** This event fires only during an active upgrade's rolling update phase.
> Normal steady-state Deployment readiness does not emit an event.

---

## Alerting Configuration

Event reason strings are designed to be stable identifiers for alerting rules.
Use `kubectl get events --field-selector` to filter by reason:

```bash
# Watch for any bootstrap failure
kubectl get events --field-selector reason=BootstrapFailed -w

# Watch for upgrade-related warnings
kubectl get events --field-selector reason=ExpandFailed -w
kubectl get events --field-selector reason=MigrateFailed -w
kubectl get events --field-selector reason=ContractFailed -w

# Watch for schema drift
kubectl get events --field-selector reason=SchemaDriftDetected -w

# Watch for all Warning events from the keystone-controller
kubectl get events --field-selector type=Warning,reportingComponent=keystone-controller -w
```

### Prometheus Alertmanager Example

When using [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics)
with event metrics enabled, you can alert on specific event reasons:

```yaml
groups:
  - name: keystone-events
    rules:
      - alert: KeystoneBootstrapFailed
        expr: |
          increase(kube_event_count{
            reason="BootstrapFailed",
            involved_object_kind="Keystone"
          }[5m]) > 0
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "Keystone bootstrap failed"
          description: "The Keystone bootstrap Job has failed. Check the Job logs for details."

      - alert: KeystoneSchemaDrift
        expr: |
          increase(kube_event_count{
            reason="SchemaDriftDetected",
            involved_object_kind="Keystone"
          }[5m]) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "Keystone schema drift detected"
          description: "The database schema does not match the expected Alembic migration head."

      - alert: KeystoneUpgradePhaseFailed
        expr: |
          increase(kube_event_count{
            reason=~"ExpandFailed|MigrateFailed|ContractFailed",
            involved_object_kind="Keystone"
          }[5m]) > 0
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "Keystone upgrade phase failed"
          description: "An upgrade phase Job has failed. The upgrade is stalled and requires investigation."
```

---

## Event Flow

```text
KeystoneReconciler.Reconcile()
  │
  ├── reconcileBootstrap()
  │     ├─ Job succeeds  → Normal  BootstrapComplete
  │     └─ Job fails     → Warning BootstrapFailed
  │
  ├── reconcileDatabase()
  │     ├─ Non-upgrade path:
  │     │     ├─ db_sync fails      → Warning DBSyncFailed
  │     │     ├─ schema-check fails → Warning SchemaDriftDetected
  │     │     └─ Both succeed       → Normal  DatabaseSynced
  │     │
  │     └─ Upgrade path:
  │           ├─ Tag changed mid-upgrade → Warning UpgradeTargetChanged
  │           ├─ Version parse error     → Warning VersionParseError
  │           ├─ Downgrade attempted     → Warning DowngradeNotSupported
  │           ├─ Non-sequential upgrade  → Warning UpgradePathInvalid
  │           ├─ Upgrade validated       → Normal  UpgradeInitiated
  │           ├─ Expand succeeds         → Normal  ExpandComplete
  │           ├─ Expand fails            → Warning ExpandFailed
  │           ├─ Migrate succeeds        → Normal  MigrateComplete
  │           ├─ Migrate fails           → Warning MigrateFailed
  │           ├─ Contract succeeds       → Normal  UpgradeComplete
  │           └─ Contract fails          → Warning ContractFailed
  │
  ├── reconcileFernetKeys()
  │     └─ Initial Secret created → Normal FernetKeysGenerated
  │
  ├── reconcileCredentialKeys()
  │     └─ Initial Secret created → Normal CredentialKeysGenerated
  │
  └── reconcileDeployment()
        └─ Ready during upgrade rollout → Normal DeploymentRolloutComplete
```
