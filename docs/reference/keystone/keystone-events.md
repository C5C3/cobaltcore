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
| `AdminSecretInvalid` | Warning | Admin password Secret is missing, unreadable, or has an empty `password` value | `Admin password Secret openstack/keystone-admin is missing, unreadable, or has an empty "password" value` |

**Source:** `reconcileBootstrap` in `reconcile_bootstrap.go`

### Database Sync (Non-Upgrade)

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DatabaseSynced` | Normal | `db_sync` and `schema-check` Jobs both complete successfully | `Database schema is up to date` |
| `DBSyncFailed` | Warning | `db_sync` Job fails | `db_sync job failed: <error>` |
| `SchemaDriftDetected` | Warning | `schema-check` Job fails after `db_sync` succeeds (schema does not match Alembic head) | `schema-check job failed: <error>` |
| `DBSyncMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation fails, deferring `db_sync` metric emission to the next reconcile | `Patching last-observed db-sync Job UID failed; db_sync metric emission deferred to the next reconcile: <error>` |

**Source:** `reconcileDatabase` in `reconcile_database.go`; `recordDBJobTerminalState` in `db_job_metrics.go`

### Upgrade Initiation

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `UpgradeInitiated` | Normal | Upgrade validated and initiated with expand-migrate-contract pipeline | `Upgrade initiated: 2025.2 → 2026.1` |
| `VersionParseError` | Warning | Installed release or target release version string cannot be parsed | `Failed to parse installed release "invalid": <error>` |
| `DowngradeNotSupported` | Warning | Target release is older than installed release | `Downgrade from 2026.1 to 2025.2 is not supported` |
| `UpgradePathInvalid` | Warning | Target release skips an intermediate version (non-sequential upgrade) | `Upgrade from 2025.1 to 2026.1 is not sequential` |
| `UpgradeTargetChanged` | Warning | `spec.image.tag` changed while an upgrade is already in progress | `Image tag changed to 2026.2 during active upgrade 2025.2 → 2026.1` |
| `UpgradeAborted` | Normal | `spec.image.tag` reverted to the installed release while an upgrade was in progress; upgrade Jobs are deleted and phase/target reset | `Upgrade 2025.2 → 2026.1 aborted: spec.image.tag reverted to installed release 2025.2` |

**Source:** `initiateUpgrade` in `reconcile_upgrade.go`; `UpgradeTargetChanged` and `UpgradeAborted` from `reconcileDatabase`/`abortUpgrade` in `reconcile_database.go`

### Upgrade Phases

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `ExpandComplete` | Normal | Expand phase Job completes successfully | `Expand phase complete: 2025.2 → 2026.1` |
| `ExpandFailed` | Warning | Expand phase Job fails | `Expand job <name> failed: <error>` |
| `MigrateComplete` | Normal | Migrate phase Job completes successfully | `Migrate phase complete: 2025.2 → 2026.1` |
| `MigrateFailed` | Warning | Migrate phase Job fails | `Migrate job <name> failed: <error>` |
| `UpgradeComplete` | Normal | Contract phase Job completes, finishing the entire upgrade | `Upgrade complete: 2025.2 → 2026.1` |
| `ContractFailed` | Warning | Contract phase Job fails | `Contract job <name> failed: <error>` |

**Source:** `reconcileExpand`, `reconcileMigrate`, `reconcileContract`, and the shared
`runUpgradePhase` step driver in `reconcile_upgrade.go`

### Encryption Key Generation

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FernetKeysGenerated` | Normal | Initial Fernet encryption keys Secret is created (first reconcile only) | `Initial Fernet encryption keys have been generated` |
| `CredentialKeysGenerated` | Normal | Initial credential encryption keys Secret is created (first reconcile only) | `Initial credential encryption keys have been generated` |

**Source:** `reconcileFernetKeys` in `reconcile_fernet.go`, `reconcileCredentialKeys` in `reconcile_credential.go`

> **Note:** These events fire only on initial Secret creation. Subsequent key
> rotations run as CronJobs; when the controller commits a completed rotation
> from the staging Secret it emits the events in
> [Rotation Commit (Staged)](#rotation-commit-staged) below.

### Rotation Commit (Staged)

The Fernet, credential-key, and admin-password rotation CronJobs write their
output to a staging Secret. The controller validates and commits the staged
payload onto the production Secret and reports the outcome as events:

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FernetKeysRotated` | Normal | Staged Fernet rotation validated and applied to the main keys Secret | `rotation applied from staging secret keystone-fernet-keys-rotation (3 active keys)` |
| `CredentialKeysRotated` | Normal | Staged credential-key rotation validated and applied to the main keys Secret | `rotation applied from staging secret keystone-credential-keys-rotation (2 active keys)` |
| `RotationAnnotationInvalid` | Warning | Staging Secret's `forge.c5c3.io/rotation-completed-at` annotation is not valid RFC3339 (Fernet/credential) | `staging secret <name> has malformed forge.c5c3.io/rotation-completed-at annotation: <error>` |
| `RotationRejected` | Warning | Staged Fernet/credential key set fails validation (key count outside `[min, max]`); the staged data is cleared | `staging secret <name> rejected: <error>` |
| `AdminPasswordRotated` | Normal | Staged admin-password rotation validated and applied to the push-source Secret | `admin password rotation applied from staging secret <name>` |
| `AdminPasswordRotationAnnotationInvalid` | Warning | Admin-password staging Secret's completion annotation is malformed | `staging secret <name> has malformed forge.c5c3.io/rotation-completed-at annotation: <error>` |
| `AdminPasswordRotationRejected` | Warning | Staged admin password fails validation (e.g. below minimum length); the rejected password is retained for inspection | `staging secret <name> rejected: <error>` |

**Source:** shared `commitStagedRotation` in `rotation_staging.go`, invoked from
`reconcile_fernet.go`, `reconcile_credential.go`, and `reconcile_passwordrotation.go`

### Trust Flush

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `TrustFlushBypass` | Warning | `spec.trustFlush` is nil on a CR that predates webhook defaulting; the existing trust-flush CronJob is deleted | `Trust flush legacy bypass: spec.trustFlush is nil (webhook defaulting did not run); existing CronJob deleted` |

**Source:** `reconcileTrustFlush` in `reconcile_trustflush.go`

### Identity Backends

Domain-lifecycle events are emitted on the **KeystoneIdentityBackend** CR by
its dedicated controller; the projection warning is emitted on the
**Keystone** CR by the keystone-side sub-reconciler:

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DomainCreated` | Normal | Manage mode provisions the domain through the identity API | `Created domain "corp" (id <id>)` |
| `DomainAdopted` | Normal | Adopt mode resolves the pre-existing domain by name (first observation only) | `Adopted pre-existing domain "corp" (id <id>)` |
| `DomainDisabled` | Normal | deletionPolicy Delete disables the domain before deleting it (keystone forbids deleting an enabled domain) | `Disabled domain "corp" before deletion` |
| `DomainDeleted` | Normal | deletionPolicy Delete removed the domain | `Deleted domain "corp" (id <id>)` |
| `DomainDeleteFailed` | Warning | Disabling/deleting the domain failed (retried on a bounded poll), or the admin credential vanished mid-teardown (fail-open: the domain is retained and the finalizer released) | `Deleting domain "corp" failed: <error>` |
| `IdentityProviderCreated` | Normal | An OIDC backend's keystone identity provider is registered | `Created identity provider "keycloak-forge" (remote ID <issuer>)` |
| `MappingCreated` | Normal | The federation mapping is created from the typed rules | `Created federation mapping "keycloak-forge-mapping" (2 rules)` |
| `MappingUpdated` | Normal | Rules drift converged back with a single update | `Updated federation mapping "keycloak-forge-mapping" (2 rules)` |
| `ProtocolCreated` | Normal | The federation protocol is bound to the mapping | `Created protocol "openid" on identity provider "keycloak-forge" (mapping keycloak-forge-mapping)` |
| `FederationGroupCreated` | Normal | A declarative target group is created in the backend's domain | `Created group "federated-users" in domain <id>` |
| `FederationObjectsDeleted` | Normal | Finalizer teardown removed protocol, mapping, and identity provider (reverse dependency order) | `Deleted protocol "openid", mapping "keycloak-forge-mapping", and identity provider "keycloak-forge"` |
| `FederationTeardownFailed` | Warning | A federation-object delete failed (retried on a bounded poll), or the admin credential vanished mid-teardown (fail-open) | `Deleting mapping "keycloak-forge-mapping" failed: <error>` |
| `IdentityBackendSkipped` | Warning | A backend's bind/client Secret is missing or lacks its fixed data key, the provider metadata is unreachable or issuer-mismatched, or a rendered value carries a control character; the backend is skipped while healthy siblings keep projecting (emitted on the Keystone CR) | `Skipping identity backend corp-ldap: <error>` |
| `FederationProxyImageMissing` | Warning | At least one OIDC backend is attached but `spec.federation.proxyImage` is unset; every OIDC backend stays pending (emitted on the Keystone CR, once per transition) | `Identity backend keycloak-forge is pending: spec.federation.proxyImage is not set — configure the mod_auth_openidc sidecar image before attaching OIDC backends` |
| `FederationMetadataStale` | Warning | A discovery-based backend's provider metadata endpoint was unreachable on a cache miss (e.g. an operator restart); the operator reused the last-known-good discovery document so federation stays up until the IdP recovers (emitted on the Keystone CR) | `Identity backend keycloak-forge: provider metadata unavailable (<error>); reusing the last-known-good discovery document so federation stays up` |

**Source:** `keystoneidentitybackend_controller.go` (domain lifecycle);
`reconcile_federation_objects.go` (federation objects);
`reconcileIdentityBackends` in `reconcile_identitybackends.go`
(`IdentityBackendSkipped`, `FederationProxyImageMissing`);
`renderOIDCBackend` in `reconcile_federation.go` (`FederationMetadataStale`)

### Finalization

Emitted while the two finalizers tear a deleted Keystone CR down. The
"Finalizing" events are gated on live cleanup work remaining, so brownfield
CRs (no MariaDB CRs) and repeated requeue polls do not produce noise:

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `FinalizingDatabase` | Normal | Deletion begins while MariaDB Database/User/Grant CRs are still live | `Cleaning up MariaDB Database, User, and Grant before removing Keystone` |
| `DatabaseFinalized` | Normal | MariaDB resources marked for deletion; database finalizer released | `MariaDB Database, User, and Grant marked for deletion; releasing finalizer` |
| `FinalizingOpenBaoSecrets` | Normal | Deletion begins while OpenBao backup PushSecrets are still live | `Cleaning up OpenBao backup PushSecrets before removing Keystone` |
| `OpenBaoSecretsFinalized` | Normal | Backup PushSecrets deleted; OpenBao finalizer released | `OpenBao backup PushSecrets deleted; releasing openbao-finalizer` |
| `ESOAdoptionTimedOut` | Warning | A backup PushSecret was not adopted by ESO within the bounded wait after deletion; the controller force-deletes it to release the finalizer | `PushSecret "<name>" not adopted by ESO within <timeout> of deletion; force-deleting to release the openbao-finalizer (the OpenBao kv-v2 path may be orphaned only if ESO is not running)` |

**Source:** finalizer handlers in `keystone_controller.go`; `ESOAdoptionTimedOut`
from the adoption-wait pass in `reconcile_secrets.go`

### Deployment Rollout

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `DeploymentRolloutComplete` | Normal | Deployment becomes ready during the `UpgradePhaseRollingUpdate` phase of an upgrade | `Deployment rollout complete during upgrade 2025.2 → 2026.1` |

**Source:** `reconcileDeployment` in `reconcile_deployment.go`

> **Note:** This event fires only during an active upgrade's rolling update phase.
> Normal steady-state Deployment readiness does not emit an event.

### Logging

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `LoggingStderrDisabled` | Warning | `spec.extraConfig` overrides `[DEFAULT].use_stderr` to a non-`true` value, causing container logs to no longer reach `kubectl logs` | `spec.extraConfig overrode [DEFAULT].use_stderr to "false"; container logs will not reach kubectl logs` |

**Source:** `reconcileConfig` in `reconcile_config.go`

> **Note:** The event is gated on a state transition into the
> `LoggingHealthy=False, Reason=StderrDisabled` status condition, so it fires
> at most once per transition rather than on every reconcile poll. Restoring
> `[DEFAULT].use_stderr=true` (e.g. by removing the `spec.extraConfig`
> override) transitions the condition back to `LoggingHealthy=True,
> Reason=StderrEnabled` without emitting an additional Warning event.

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
  ├── finalizers (deletionTimestamp set)
  │     ├─ MariaDB CRs still live         → Normal  FinalizingDatabase
  │     ├─ MariaDB cleanup done           → Normal  DatabaseFinalized
  │     ├─ Backup PushSecrets still live  → Normal  FinalizingOpenBaoSecrets
  │     ├─ ESO adoption wait exceeded     → Warning ESOAdoptionTimedOut
  │     └─ PushSecrets deleted            → Normal  OpenBaoSecretsFinalized
  │
  ├── reconcileBootstrap()
  │     ├─ Admin Secret missing/invalid → Warning AdminSecretInvalid
  │     ├─ Job succeeds  → Normal  BootstrapComplete
  │     └─ Job fails     → Warning BootstrapFailed
  │
  ├── reconcileDatabase()
  │     ├─ Non-upgrade path:
  │     │     ├─ db_sync fails      → Warning DBSyncFailed
  │     │     ├─ schema-check fails → Warning SchemaDriftDetected
  │     │     ├─ Both succeed       → Normal  DatabaseSynced
  │     │     └─ Job-UID patch fails → Warning DBSyncMetricEmissionDeferred
  │     │
  │     └─ Upgrade path:
  │           ├─ Tag changed mid-upgrade → Warning UpgradeTargetChanged
  │           ├─ Tag reverted mid-upgrade → Normal UpgradeAborted
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
  │     ├─ Initial Secret created → Normal FernetKeysGenerated
  │     ├─ Staged rotation applied → Normal FernetKeysRotated
  │     ├─ Malformed completion annotation → Warning RotationAnnotationInvalid
  │     └─ Staged key set rejected → Warning RotationRejected
  │
  ├── reconcileCredentialKeys()
  │     ├─ Initial Secret created → Normal CredentialKeysGenerated
  │     ├─ Staged rotation applied → Normal CredentialKeysRotated
  │     ├─ Malformed completion annotation → Warning RotationAnnotationInvalid
  │     └─ Staged key set rejected → Warning RotationRejected
  │
  ├── reconcilePasswordRotation()
  │     ├─ Staged rotation applied → Normal AdminPasswordRotated
  │     ├─ Malformed completion annotation → Warning AdminPasswordRotationAnnotationInvalid
  │     └─ Staged password rejected → Warning AdminPasswordRotationRejected
  │
  ├── reconcileConfig()
  │     └─ spec.extraConfig overrides use_stderr → Warning LoggingStderrDisabled
  │       (gated on transition into LoggingHealthy=False, Reason=StderrDisabled)
  │
  ├── reconcileTrustFlush()
  │     └─ Legacy nil spec.trustFlush bypass → Warning TrustFlushBypass
  │
  └── reconcileDeployment()
        └─ Ready during upgrade rollout → Normal DeploymentRolloutComplete
```
