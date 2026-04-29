---
title: Keystone Schema Drift Detection
quadrant: operator
---

# Keystone Schema Drift Detection

Reference documentation for the Keystone schema drift detection feature.
After the `db_sync` Job completes successfully, the operator runs a read-only
schema-check Job that verifies the database schema matches the expected Alembic
migration head. This detects drift caused by manual DDL changes, partial migration
failures, or database restores from stale backups before Keystone pods serve traffic
with an incompatible schema.

For the reconciler architecture and sub-reconciler contracts, see
[Keystone Reconciler Architecture](./keystone-reconciler.md). For the upgrade flow
(which has its own schema validation), see
[Keystone Upgrade Flow](./keystone-upgrade-flow.md).

---

## Overview

The schema-check Job is inserted into the `reconcileDatabase` flow as a second stage
after `db_sync`, before `DatabaseReady` is set to `True`. It is implemented as a
separate Kubernetes Job (not an init container) because:

1. It reuses the existing `job.RunJob` infrastructure with pod-spec hash comparison
   for stale detection.
2. It allows an independent `backoffLimit` (2 vs 4 for `db_sync`) to fail faster on
   permanent schema drift while still retrying transient database connection failures.
3. It enables `ttlSecondsAfterFinished` for automatic cleanup of completed/failed Jobs.
4. It cleanly separates the write operation (`db_sync`) from the read-only verification
   (`schema-check`).

The schema-check Job applies only to the non-upgrade (simple `db_sync`) path. The
upgrade flow (expand-migrate-contract) has its own validation via Alembic's
`_validate_upgrade_order` check.

---

## Data Flow

```text
reconcileDatabase()
  |
  +-- MariaDB CRs ready? (managed mode only)
  |
  +-- Upgrade path? --> delegate to reconcileUpgrade
  |
  +-- db_sync Job (existing)
  |     |
  |     +-- failed  --> DatabaseReady=False/DBSyncFailed, return error
  |     +-- running --> DatabaseReady=False/DBSyncInProgress, requeue 30s
  |     +-- done    --> proceed to schema check
  |
  +-- schema-check Job
  |     |
  |     +-- failed  --> DatabaseReady=False/SchemaDriftDetected, return error
  |     +-- running --> DatabaseReady=False/SchemaCheckInProgress, requeue 30s
  |     +-- done    --> proceed to set DatabaseReady=True
  |
  +-- status.installedRelease = spec.image.tag
  +-- DatabaseReady=True/DatabaseSynced (message includes "revision verified")
```

---

## Schema Check Job

### Job Specification

| Field | Value |
| --- | --- |
| Name | `{name}-schema-check` |
| Image | `{spec.image.repository}:{spec.image.tag}` |
| Command | `/bin/sh -eu -c {schema-check-script}` |
| BackoffLimit | 2 |
| TTLSecondsAfterFinished | 300 (5 minutes) |
| RestartPolicy | `Never` |
| SecurityContext | PSS Restricted profile via `restrictedSecurityContext()` |
| Config mount | Keystone configuration ConfigMap at `/etc/keystone/keystone.conf.d/` (read-only) |

The Job shares the same Keystone container image, SecurityContext, and config volume
mount as the `db_sync` Job. The differences are:

| Property | db_sync | schema-check |
| --- | --- | --- |
| BackoffLimit | 4 | 2 |
| TTLSecondsAfterFinished | not set | 300 |
| Command | `keystone-manage db_sync` | `/bin/sh -eu -c` with embedded Python script |

### Stale Job Detection

The schema-check Job uses the same `job.RunJob` infrastructure as `db_sync`. If the
operator detects a completed schema-check Job whose pod template spec differs from
the desired spec (e.g., after an operator upgrade changes the container image), the
stale Job is deleted and a new one is created. This ensures the verification always
runs with the current image.

---

## Schema Check Script

The schema-check Job runs an embedded Python script that performs a read-only
comparison between the actual database schema version and the expected version.

### Script Execution Steps

1. **Read configuration** -- Parse Keystone configuration files from
   `/etc/keystone/keystone.conf.d/*.conf` using Python's `configparser` to extract
   the `[database] connection` URL.

2. **Connect to database** -- Parse the connection URL and connect via PyMySQL using
   the same credentials as `db_sync`. If the connection fails, the script prints an
   error to stderr and exits with code 1.

3. **Query actual version** -- Execute `SELECT version_num FROM alembic_version`.
   If the table does not exist (query raises an exception) or returns no rows, the
   script prints `alembic_version table not found` to stderr and exits with code 1.
   Multiple rows (as used by Keystone's branched migration model) are sorted and
   joined with commas.

4. **Query expected version** -- Run
   `keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ db_version` as a
   subprocess. The first whitespace-delimited token of stdout is extracted as the
   expected Alembic revision hash.

5. **Compare versions** -- If actual matches expected, print the revision to stdout
   and exit with code 0. If they differ, print
   `Schema drift detected: expected {expected}, got {actual}` to stderr and exit
   with code 1.

### Safety Properties

- **Read-only** -- The script uses only `SELECT` statements. No DDL or DML is
  executed. Running the schema check repeatedly or concurrently cannot corrupt the
  database or interfere with migrations.
- **Idempotent** -- Running the schema check multiple times produces the same result,
  assuming no concurrent schema changes.
- **Same connection** -- The script reads the database connection string from the same
  ConfigMap used by `db_sync`, ensuring it connects to the same database instance.

### Error Scenarios

| Scenario | Exit Code | Stderr Output |
| --- | --- | --- |
| Database connection failure | 1 | `Failed to connect to database: {error}` |
| `alembic_version` table missing | 1 | `alembic_version table not found` |
| `alembic_version` table empty | 1 | `alembic_version table not found` |
| `keystone-manage db_version` failure | 1 | `keystone-manage db_version failed: {stderr}` |
| Version mismatch | 1 | `Schema drift detected: expected {expected}, got {actual}` |
| Version match | 0 | `{revision}` (printed to stdout) |

---

## Condition Reasons

The `DatabaseReady` condition reflects schema-check state with specific reasons:

### Progress Reasons

| Reason | Description |
| --- | --- |
| `SchemaCheckInProgress` | The schema-check Job is running or pending. Reconciler requeues with 30s interval. |

### Success Reasons

| Reason | Description |
| --- | --- |
| `DatabaseSynced` | Both `db_sync` and schema-check completed successfully. The condition message includes `"revision verified"` to indicate the schema check passed. |

### Failure Reasons

| Reason | Description |
| --- | --- |
| `SchemaDriftDetected` | The schema-check Job failed. This can indicate a version mismatch, missing `alembic_version` table, or persistent database connectivity issues after 2 retries. The operator returns an error (not just a requeue) so the failure is visible in controller logs. |

All conditions include `ObservedGeneration` set to `keystone.Generation`.

---

## Interaction with Upgrade Flow

The schema-check Job runs only on the **non-upgrade** (simple `db_sync`) path. The
expand-migrate-contract upgrade flow has its own validation through Alembic's
`_validate_upgrade_order` check, which ensures expand, migrate, and contract steps
execute in the correct order.

When the upgrade flow completes, the contract phase sets `DatabaseReady=True` with
reason `DatabaseSynced` and a message indicating the upgrade versions. No separate
schema-check Job is created during upgrades.

---

## Interaction with InstalledRelease

The `status.installedRelease` field is set only after **both** `db_sync` and
schema-check complete successfully. If `db_sync` succeeds but the schema-check fails
(drift detected), `installedRelease` is **not** updated to the new tag. This prevents
the operator from recording a successful deployment when the database schema is in an
inconsistent state.

---

## Troubleshooting

### Checking Schema Check Status

Inspect the `DatabaseReady` condition for schema-check-specific reasons:

```bash
kubectl get keystone <name> -o jsonpath='{.status.conditions[?(@.type=="DatabaseReady")]}'
```

During a schema check, this shows:

```json
{
  "type": "DatabaseReady",
  "status": "False",
  "reason": "SchemaCheckInProgress",
  "message": "schema-check job is running",
  "observedGeneration": 3
}
```

On successful completion:

```json
{
  "type": "DatabaseReady",
  "status": "True",
  "reason": "DatabaseSynced",
  "message": "Database schema is up to date (revision verified)",
  "observedGeneration": 3
}
```

### Inspecting the Schema Check Job

```bash
kubectl get job <name>-schema-check
kubectl logs job/<name>-schema-check
kubectl describe job/<name>-schema-check
```

### Common Issues

#### SchemaDriftDetected

**Symptom:** `DatabaseReady=False`, reason `SchemaDriftDetected`.

**Possible causes:**

1. **Manual DDL changes** -- Someone modified the database schema outside of
   `keystone-manage db_sync`.
2. **Partial migration failure** -- A previous `db_sync` run partially applied
   migrations before failing.
3. **Database restore from stale backup** -- The database was restored from a backup
   that predates the latest migration.
4. **Missing alembic_version table** -- The database has never been migrated (the
   `alembic_version` table does not exist).

**Resolution:**

1. Check the schema-check Job logs for the specific error:
   ```bash
   kubectl logs job/<name>-schema-check
   ```
2. For version mismatch: compare expected vs actual versions in the error message.
   Determine whether the database needs a manual `db_sync` or if a restore is needed.
3. For missing `alembic_version` table: the database has not been initialized. Verify
   the database connection and credentials.
4. After resolving the root cause, delete the failed Job to allow the operator to
   retry:
   ```bash
   kubectl delete job <name>-schema-check
   ```
   The TTL controller will also automatically clean up the Job after 5 minutes.

#### SchemaCheckInProgress Stuck

**Symptom:** `DatabaseReady=False`, reason `SchemaCheckInProgress` for an extended
period.

**Possible causes:**

1. The schema-check pod is stuck in `Pending` (resource constraints, node scheduling).
2. The database is unreachable, causing the script to hang on connection.

**Resolution:**

1. Check pod status: `kubectl get pods -l job-name=<name>-schema-check`
2. Check pod events: `kubectl describe pod -l job-name=<name>-schema-check`
3. Verify database connectivity from within the namespace.

---

## Limitations

1. **Cannot extract actual revision from controller.** The controller cannot access
   pod logs to extract the actual Alembic version string. The `DatabaseSynced`
   condition message uses a static `"revision verified"` marker rather than the actual
   revision hash. To see the actual revision, inspect the schema-check Job logs:
   ```bash
   kubectl logs job/<name>-schema-check
   ```

2. **Non-upgrade path only.** Schema drift detection runs only after the simple
   `db_sync` path. The expand-migrate-contract upgrade flow relies on Alembic's
   built-in validation.

3. **No resource limits on schema-check Jobs.** The schema-check Job inherits no
   resource requests/limits (BestEffort QoS).

4. **Transient connectivity failures.** The `backoffLimit: 2` provides limited retry
   for transient database connectivity issues. If the database is temporarily
   unreachable, the schema-check may fail with `SchemaDriftDetected` even though the
   schema is correct. Deleting the failed Job triggers a retry on the next reconcile.
