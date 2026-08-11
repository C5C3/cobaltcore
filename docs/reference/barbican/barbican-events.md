---
title: Barbican Controller Events
quadrant: operator
---

# Barbican Controller Events

Reference documentation for Kubernetes events emitted by the Barbican operator.
Two controllers run in the one manager and both record events: the Barbican
controller on the Barbican CR, the BarbicanSecretStore controller on its own CR.
They provide observability via `kubectl describe barbican`,
`kubectl describe barbicansecretstore`, and `kubectl get events` without access
to controller logs.

Events complement status conditions: conditions reflect current state for
programmatic consumers, while events provide a timestamped audit trail of
transitions for human operators and alerting systems.

For the reconciler architecture and sub-reconciler contracts, see
[Barbican Reconciler Architecture](./barbican-reconciler.md). For the spec and
status contracts the conditions belong to, see [Barbican CRD](./barbican-crd.md)
and [BarbicanSecretStore CRD](./barbican-secret-store-crd.md).

---

## Event Conventions

All events follow these conventions:

- **Reason strings** are stable PascalCase identifiers. They are part of the
  controllers' public API and will not change without a deprecation notice.
- **Normal** type indicates successful completion of a lifecycle transition.
- **Warning** type indicates a failure, validation error, or unexpected condition
  that requires operator attention.
- **No events are emitted for in-progress/polling states** (e.g. while the
  db-sync Job is still running). This prevents event noise from repeated requeue
  cycles.
- Events on the **Barbican** CR (`involvedObject.kind: Barbican`) come from the
  recorder named `barbican-controller`; events on the **BarbicanSecretStore** CR
  come from `barbicansecretstore-controller`. Both names are wired in
  `operators/barbican/main.go` and are what the `reportingComponent` field
  selector matches.
- The Kubernetes API server deduplicates events by (involvedObject, reason,
  message, source). Repeated identical events increment a counter rather than
  creating new event objects.

---

## Event Reasons Reference

### Configuration

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `ExtraConfigOwnedKeyOverride` | Warning | `spec.extraConfig` overrides one or more operator-owned configuration keys (the per-service ownership registry) | `spec.extraConfig overrides operator-owned keys: [queue] enable (enabling the queue makes the API hand order processing to a worker that is not deployed, so orders stay pending instead of failing)` |

**Source:** the shared `RecordExtraConfigHealth` in
`internal/common/config/ownership.go`, called from `reconcileConfig` in
`reconcile_config.go`

> **Note:** The event is gated on the `ExtraConfigHealthy=False` condition's
> message. It fires once on the transition into `False` and once more when the
> overridden-key set changes, never on the steady reconcile poll. Removing the
> overrides transitions the condition back to `ExtraConfigHealthy=True,
> Reason=NoOwnedKeysOverridden` without a further event. The condition is
> informational and is not aggregated into `Ready`. The three credential keys
> the validating webhook refuses at admission never reach this path. The guard
> runs before the last-good-retention short-circuit, so it stays maintained even
> on a pass that re-renders nothing.

### Secret Stores

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `BarbicanSecretStoreSkipped` | Warning | The default store passed its own credential gate, but its credentials Secret cannot be read this pass, or an assembled `[vault_plugin]` option carries a newline | `Skipping secret store openbao-primary: secret "openbao-primary-approle" key "secret-id" not found` |
| `SecretStoreDetached` | Warning | A store recorded in `status.projectedSecretStores` is absent from the projection this pass renders | `Secret store "openbao-legacy" is no longer projected: its [secretstore:openbao-legacy] section is dropped from barbican.conf, so every secret barbican wrote through that store stops resolving` |

**Source:** `reconcileSecretStores` and `recordProjectedStores` in
`reconcile_secretstores.go`

> **Note:** `SecretStoreDetached` fires on the pass that de-projects the store,
> which is the pass the operator would otherwise report as a success. Nothing
> else marks the loss: the store CR carries no finalizer and its deletion is not
> validated. The counting violations behind
> `SecretStoresReady=False / NoDefaultSecretStore` and `MultipleOpenBaoStores`
> raise **no** event; read those off the condition.

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
| `ImageReleaseMismatch` | Warning | The requested release bump leaves `spec.image` at the reference that migrated the installed schema, so no migration would run | `upgrade from 2025.2 to 2026.1 leaves spec.image unchanged (ghcr.io/c5c3/barbican:2025.2), so no migration would run: either bump spec.image in lockstep with spec.openStackRelease, or […] patch status.installedRelease to 2026.1` |

**Source:** `gateReleaseTransition` and `rejectReleaseTransition` in
`reconcile_database.go`

> **Note:** `ImageReleaseMismatch` has a second trigger that raises **no event**.
> When a tag-pinned `spec.image` names a different OpenStack release than
> `spec.openStackRelease`, `checkImageReleaseMismatch` sets `DatabaseReady=False`
> with that reason and requeues without recording anything, because the check
> runs on every pass and would otherwise re-fire for as long as the two fields
> disagree. Read that case off the condition, not the event stream.

### Database Clean-up

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DBCleanJobFailed` | Warning | The newest terminal Job the `{name}-db-clean` CronJob spawned failed | `Database clean-up Job barbican-db-clean-29387460 failed; inspect its pod logs — the soft-deleted secret, container and order rows keep accumulating until a run succeeds` |
| `DBCleanMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation fails, deferring `db_clean` metric emission to the next reconcile | `Patching last-observed db-clean Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** `reconcileDBClean` in `reconcile_dbclean.go` (`DBCleanJobFailed`);
the shared `RecordJobTerminalState` (`DBCleanMetricEmissionDeferred`)

> **Note:** A run that wedges instead of failing produces the same event. Each
> Job carries an `activeDeadlineSeconds` of one hour, so a pod stuck in
> `ImagePullBackOff` or a `barbican-manage` blocked on a database lock reaches a
> terminal failure within the hour instead of staying active while
> `DBCleanReady` keeps reporting a healthy schedule. A suspended clean-up raises
> no event at all: it reports `DBCleanReady=True` under its own reason, and the
> only signal that the backlog is growing is that the `db_clean` metric stops
> incrementing.

### Finalization

Emitted while the finalizer tears a deleted Barbican CR down.
`FinalizingDatabase` is gated on live MariaDB cleanup work remaining, so
brownfield CRs (no MariaDB CRs) and repeated requeue polls do not produce noise.

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FinalizingDatabase` | Normal | Deletion begins while MariaDB Database/User/Grant CRs are still live | `Cleaning up MariaDB Database, User, and Grant before removing Barbican` |
| `DatabaseFinalized` | Normal | MariaDB resources marked for deletion; finalizer released | `MariaDB Database, User, and Grant marked for deletion; releasing finalizer` |

**Source:** `reconcileDelete` in `barbican_controller.go`

### Secret Store Controller

Every waiting and failure state of the BarbicanSecretStore controller goes
through one helper, which records the False sub-condition and a Warning event
carrying the same reason and message. None of these states reaches the workqueue
as an error: an unreachable server or a credential nobody has provided yet is a
state to report and retry (at 30s), not a reconcile failure. The reason
vocabulary is therefore identical to the condition reasons documented under
[Conditions](./barbican-secret-store-crd.md#conditions).

| Reason | Type | Condition it lands on | Trigger Condition |
| --- | --- | --- | --- |
| `WaitingForCredentials` | Warning | `CredentialsReady` | A referenced Secret does not carry a non-empty `role-id`, `secret-id`, or `ca.crt` yet |
| `InvalidCredentials` | Warning | `CredentialsReady` | The server rejects the AppRole credentials, and the re-mint cooldown declines to replace them |
| `InsufficientCapabilities` | Warning | `CredentialsReady` | The AppRole policy does not grant the required capabilities on the mount's data path |
| `OpenBaoUnreachable` | Warning | `CredentialsReady` or `ProvisioningReady` | The server did not answer, or the client could not be built |
| `InstanceNotFound` | Warning | `ProvisioningReady` | The referenced `OpenBaoCluster` does not exist |
| `WaitingForInstance` | Warning | `ProvisioningReady` | The instance is not Available yet, or its provisioner ServiceAccount is missing |
| `WaitingForInstanceTLS` | Warning | `ProvisioningReady` | The `<instance>-tls-ca` Secret does not carry the trust bundle yet |
| `InstanceNotProvisioned` | Warning | `ProvisioningReady` | The KV mount or the AppRole the self-init contract provisions is absent |
| `ProvisioningDenied` | Warning | `CredentialsReady` or `ProvisioningReady` | The operator may not mint a bound token for the provisioner ServiceAccount, or OpenBao answered 403 |

**Source:** the `fail` and `failOpenBao` helpers in
`barbicansecretstore_controller.go`

> **Note:** `ConfigProjected=False / WaitingForProjection` raises no event. It is
> the normal state between a store turning credential-ready and the parent
> rolling the new config out, so an event per store per install would be noise.

### Reasons that never fire for Barbican

| Reason | Why it cannot occur |
| --- | --- |
| `SchemaDriftDetected` | The Job set carries no schema-check command (`SchemaCheckCommand: nil`). `barbican-manage db upgrade` is an idempotent alembic upgrade to head, so a second read-only Job would assert nothing the sync itself has not already established |
| `UpgradeInitiated`, `ExpandComplete`, `MigrateComplete`, `DeploymentRolloutComplete`, `UpgradeComplete`, `UpgradeAborted`, `UpgradeTargetChanged`, `ExpandFailed`, `MigrateFailed`, `ContractFailed` | These belong to the shared expand-migrate-contract flow. Barbican runs no phase machine and its CR carries no `upgradePhase`; a release bump takes the same single db-sync Job a fresh install takes |
| `DBSyncInProgress` | A `DatabaseReady=False` condition reason while the Job runs, never an event: the polling states raise none |

---

## Alerting Configuration

Event reason strings are designed to be stable identifiers for alerting rules.
Use `kubectl get events --field-selector` to filter by reason:

```bash
# Watch for db-sync failures
kubectl get events --field-selector reason=DBSyncFailed -w

# Watch for a clean-up run that failed or ran past its deadline
kubectl get events --field-selector reason=DBCleanJobFailed -w

# Watch for a secret store dropping out of the rendered config
kubectl get events --field-selector reason=SecretStoreDetached -w

# Watch for all Warning events from the barbican-controller
kubectl get events --field-selector type=Warning,reportingComponent=barbican-controller -w

# Watch for all Warning events from the store controller
kubectl get events --field-selector type=Warning,reportingComponent=barbicansecretstore-controller -w
```

### Prometheus Alertmanager Example

When using [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics)
with event metrics enabled, you can alert on specific event reasons:

```yaml
groups:
  - name: barbican-events
    rules:
      - alert: BarbicanSecretStoreDetached
        expr: |
          increase(kube_event_count{
            reason="SecretStoreDetached",
            involved_object_kind="Barbican"
          }[5m]) > 0
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "A Barbican secret store was de-projected"
          description: "Secrets written through the detached store no longer resolve. Re-attach the store or restore it from the last-good config."
```

---

## Event Flow

```text
BarbicanReconciler.Reconcile()
  │
  ├── reconcileDelete() (deletionTimestamp set)
  │     ├─ MariaDB CRs still live  → Normal  FinalizingDatabase
  │     └─ MariaDB cleanup done    → Normal  DatabaseFinalized
  │
  ├── reconcileSecretStores()
  │     ├─ credentials unreadable  → Warning BarbicanSecretStoreSkipped
  │     ├─ store de-projected      → Warning SecretStoreDetached
  │     └─ no/several defaults     → (condition only, no event)
  │
  ├── reconcileConfig()
  │     └─ spec.extraConfig overrides operator-owned keys → Warning ExtraConfigOwnedKeyOverride
  │       (gated on transition into ExtraConfigHealthy=False, Reason=OwnedKeysOverridden)
  │
  ├── reconcileDBClean()
  │     ├─ newest terminal run failed → Warning DBCleanJobFailed
  │     └─ Job-UID patch fails        → Warning DBCleanMetricEmissionDeferred
  │
  └── reconcileDatabase()
        ├─ tag/release mismatch      → (condition only, no event)
        ├─ release gate refuses      → Warning VersionParseError / DowngradeNotSupported /
        │                                      UpgradePathInvalid / ImageReleaseMismatch
        ├─ db_sync fails             → Warning DBSyncFailed
        ├─ db_sync succeeds          → Normal  DatabaseSynced
        └─ Job-UID patch fails       → Warning DBSyncMetricEmissionDeferred

BarbicanSecretStoreReconciler.Reconcile()
  │
  ├── reconcileManaged()     → Warning InstanceNotFound / WaitingForInstance /
  │                                    WaitingForInstanceTLS / InstanceNotProvisioned /
  │                                    ProvisioningDenied / OpenBaoUnreachable /
  │                                    InvalidCredentials
  ├── reconcileBrownfield()  → Warning WaitingForCredentials / InvalidCredentials /
  │                                    InsufficientCapabilities / OpenBaoUnreachable
  └── config projection observed → (condition only, no event)
```
