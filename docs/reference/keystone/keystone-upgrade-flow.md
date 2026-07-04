---
title: Keystone Upgrade Flow
quadrant: operator
---

# Keystone Upgrade Flow

Reference documentation for the Keystone expand-migrate-contract database upgrade
flow. The operator automatically performs phased database migrations when
`spec.image.tag` changes to a new OpenStack release, maintaining API availability
throughout the upgrade.

For CRD type definitions (including upgrade status fields), see
[Keystone CRD API Reference](./keystone-crd.md). For the reconciler architecture
and sub-reconciler contracts, see
[Keystone Reconciler Architecture](./keystone-reconciler.md).

---

## Overview

OpenStack services use the expand-migrate-contract pattern to perform zero-downtime
database schema upgrades across releases. The three phases allow old and new code to
coexist during the transition:

1. **Expand** -- Add new columns, tables, and triggers so the old code can still
   read/write while new schema elements are populated.
2. **Migrate** -- Copy or transform data from old schema elements to new ones.
3. **Contract** -- Remove old columns, tables, and triggers that are no longer needed.

The Keystone operator implements this as a state machine within the `reconcileDatabase`
sub-reconciler, coordinated with `reconcileDeployment` for the rolling update phase
between migrate and contract.

---

## Version Format

OpenStack releases follow a `YYYY.N` naming convention, with two releases per year:

| Component | Format | Examples |
| --- | --- | --- |
| Base release | `YYYY.N` where N is 1 or 2 | `2025.1`, `2025.2`, `2026.1` |
| Patch release | `YYYY.N-suffix` | `2025.2-p1`, `2026.1-hotfix` |

The operator parses version strings using `ParseRelease()` in
`operators/keystone/internal/controller/version.go`. Parsing rules:

- The base part must be exactly two dot-separated integers (`YYYY.N`).
- Year must be >= 2010.
- Minor version must be exactly 1 or 2.
- An optional suffix after `-` is treated as a patch identifier and stripped for
  upgrade comparison.
- Invalid formats (e.g., `latest`, `abc`, `2025`, `2025.3`, empty string) are
  rejected with a `VersionParseError` condition.

### Valid Upgrade Paths

Only **sequential** upgrades are supported. A sequential upgrade is exactly one
release step forward:

| From | To | Valid | Reason |
| --- | --- | --- | --- |
| `2025.1` | `2025.2` | Yes | Same year, minor +1 |
| `2025.2` | `2026.1` | Yes | Year +1, from minor 2 to minor 1 |
| `2026.1` | `2026.2` | Yes | Same year, minor +1 |
| `2024.2` | `2026.1` | **No** | Skip-level (skips 2025.x entirely) |
| `2025.2` | `2026.2` | **No** | Skip-level (skips 2026.1) |
| `2026.1` | `2025.2` | **No** | Downgrade |

### Patch Revisions

Tag changes that differ only in patch suffix (e.g., `2025.2` to `2025.2-p1`) are
**not** treated as upgrades. They use the simple `db_sync` path because patch
revisions do not change the database schema.

---

## Status Fields

Three status fields track the upgrade lifecycle. All are updated atomically via the
status subresource.

| Field | Type | During Upgrade | Outside Upgrade |
| --- | --- | --- | --- |
| `status.installedRelease` | `string` | The release version **before** the upgrade began | The currently deployed release version |
| `status.targetRelease` | `string` | The release version being upgraded **to** | Empty (`""`) |
| `status.upgradePhase` | `UpgradePhase` | Current phase (see below) | Empty (`""`) |

### installedRelease

Set after the first successful `db_sync` Job completion. For fresh deployments
(no prior `installedRelease`), the value is initialized to `spec.image.tag` after
`db_sync` completes. This field is visible as the `Release` printer column in
`kubectl get keystones` output.

### targetRelease

Set to `spec.image.tag` when an upgrade is initiated. Cleared (set to `""`) when
the upgrade completes successfully or is aborted. During an active upgrade,
reverting `spec.image.tag` to `installedRelease` aborts the upgrade (see
[Aborting an Upgrade](#aborting-an-upgrade)); changing it to any other value that
differs from `targetRelease` blocks with an `UpgradeTargetChanged` condition.

### upgradePhase

An enum with four valid values during an upgrade:

| Value | Description |
| --- | --- |
| `Expanding` | Running `db_sync --expand` with the NEW image |
| `Migrating` | Running `db_sync --migrate` with the NEW image |
| `RollingUpdate` | Waiting for the Deployment to roll out with the NEW image |
| `Contracting` | Running `db_sync --contract` with the NEW image |

---

## Phase Transitions

The upgrade proceeds through a fixed sequence of phases. Each phase transition is
driven by Job completion or Deployment readiness.

```text
spec.image.tag changed (e.g., 2025.2 -> 2026.1)
         |
         v
  validateUpgradePath()
  - ParseRelease(installedRelease)
  - ParseRelease(spec.image.tag)
  - IsSequentialUpgrade(from, to)
         |
    +----v-----+
    | Expanding |--- db_sync --expand (NEW image: 2026.1) --- Job complete
    +----------+                                                  |
         +--------------------------------------------------------+
    +----v------+
    | Migrating |--- db_sync --migrate (NEW image: 2026.1) --- Job complete
    +-----------+                                                  |
         +-----------------------------------------------------   +
    +----v-----------+
    | RollingUpdate   |--- Deployment updates to NEW image (2026.1)
    +----------------+    waits for rollout --- rollout complete
                                                        |
         +----------------------------------------------+
    +----v---------+
    | Contracting  |--- db_sync --contract (NEW image: 2026.1) --- Job complete
    +--------------+                                                    |
         +--------------------------------------------------------------+
         v
  installedRelease = "2026.1"
  targetRelease    = ""
  upgradePhase     = ""
```

### Phase Details

#### Expanding

- **Job name:** `{name}-db-expand`
- **Image:** NEW release (`spec.image.tag`) — expand migrations are owned by
  the target release's alembic tree; running them with the old binary would
  leave the contract step ahead of expand (`_validate_upgrade_order` fails).
- **Command:** `keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ db_sync --expand`
- **On completion:** Phase transitions to `Migrating`, requeues immediately.
- **On failure:** `DatabaseReady=False` with reason `ExpandFailed`.

#### Migrating

- **Job name:** `{name}-db-migrate`
- **Image:** NEW release (`spec.image.tag`) — same rationale as Expanding.
- **Command:** `keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ db_sync --migrate`
- **On completion:** Phase transitions to `RollingUpdate`, requeues immediately.
- **On failure:** `DatabaseReady=False` with reason `MigrateFailed`.

#### RollingUpdate

- **No Job created.** The `reconcileDatabase` sub-reconciler returns `ctrl.Result{}`
  (empty result), allowing the reconciler chain to continue to `reconcileDeployment`.
- `reconcileDeployment` ensures the Deployment is configured with `spec.image.tag` (the NEW image).
  Kubernetes performs a rolling update of the pods.
- **On Deployment ready:** `reconcileDeployment` transitions the phase to
  `Contracting` and requeues.
- **While not ready:** Requeues with `RequeueDeploymentPolling` (10s).
- The Keystone API remains available throughout because old pods continue serving
  traffic until new pods pass readiness checks.

#### Contracting

- **Job name:** `{name}-db-contract`
- **Image:** NEW release (`spec.image.tag`)
- **Command:** `keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ db_sync --contract`
- **On completion:** Upgrade finalized (see below).
- **On failure:** `DatabaseReady=False` with reason `ContractFailed`.

#### Upgrade Completion

When the contract Job completes successfully:

1. `status.installedRelease` is updated to `status.targetRelease` (the new version).
2. `status.targetRelease` is cleared.
3. `status.upgradePhase` is cleared.
4. `DatabaseReady` condition is set to `True` with reason `DatabaseSynced`.

---

## Condition Reasons

The `DatabaseReady` condition reflects upgrade state with specific reasons:

### Progress Reasons

| Reason | Phase | Description |
| --- | --- | --- |
| `ExpandInProgress` | Expanding | Expand Job is running or phase was just initiated |
| `MigrateInProgress` | Migrating | Migrate Job is running or expand just completed |
| `UpgradeRollingUpdate` | RollingUpdate | Waiting for Deployment rollout to complete |
| `ContractInProgress` | Contracting | Contract Job is running |

### Failure Reasons

| Reason | Phase | Description |
| --- | --- | --- |
| `ExpandFailed` | Expanding | Expand Job failed permanently (exceeded backoff limit) |
| `MigrateFailed` | Migrating | Migrate Job failed permanently |
| `ContractFailed` | Contracting | Contract Job failed permanently |

### Validation Reasons

| Reason | Phase | Description |
| --- | --- | --- |
| `VersionParseError` | Pre-upgrade | `installedRelease` or `spec.image.tag` is not a valid `YYYY.N` format |
| `UpgradePathInvalid` | Pre-upgrade | Upgrade is not sequential (e.g., skip-level) |
| `UpgradeTargetChanged` | Any active phase | `spec.image.tag` was changed during an active upgrade to a value different from `targetRelease` |

All condition messages include the source and target release version strings for
operator visibility (e.g., `"Expand phase running: 2025.2 -> 2026.1"`).

---

## Data Flow

The upgrade logic is distributed across two sub-reconcilers that execute sequentially
in the main reconcile loop:

```text
reconcileDatabase (called first in reconciler chain)
  |
  +- Active upgrade? (upgradePhase != "")
  |  +- Tag reverted to installedRelease? -> abort: delete phase Jobs, clear state, requeue
  |  +- Tag changed to another value? -> block with UpgradeTargetChanged
  |  +- Delegate to reconcileUpgrade() -> dispatch by phase
  |
  +- isUpgrade()? (installedRelease != "" && tag != installedRelease && !patchOnly)
  |  +- No: simple db_sync -> on completion: set installedRelease if empty
  |  +- Yes: initiateUpgrade() -> validate path -> set targetRelease + Expanding -> requeue
  |
  +--- continues to reconcileDeployment (only if result is zero-value) ---+
                                                                          |
reconcileDeployment                                                       |
  |                                                                       |
  +- EnsureDeployment(spec.image.tag)  <----------------------------------+
  +- If upgradePhase == RollingUpdate && Deployment ready:
  |    -> upgradePhase = Contracting -> requeue (back to reconcileDatabase)
  +- Normal: set DeploymentReady, Endpoint
```

The sequential sub-reconciler pattern (return early on non-zero result) naturally
prevents `reconcileDeployment` from running during expand, migrate, and contract
phases. Only the `RollingUpdate` phase passes through to `reconcileDeployment`.

---

## Job Details

Each upgrade phase creates a distinctly named Job to avoid interference with the
existing `db_sync` Job and to provide a clear audit trail.

| Phase | Job Name | Image Tag | db_sync Flag |
| --- | --- | --- | --- |
| Expanding | `{name}-db-expand` | `spec.image.tag` (NEW) | `--expand` |
| Migrating | `{name}-db-migrate` | `spec.image.tag` (NEW) | `--migrate` |
| Contracting | `{name}-db-contract` | `spec.image.tag` (NEW) | `--contract` |

All upgrade Jobs share these properties:

- **Backoff limit:** 4 retries before permanent failure.
- **Restart policy:** `Never` (failed pods are not restarted).
- **Security context:** PSS Restricted profile via `restrictedSecurityContext()`.
- **Config mount:** Keystone configuration ConfigMap at `/etc/keystone/keystone.conf.d/`.
- **Idempotency:** `RunJob` checks for existing Jobs before creating new ones. If a
  completed Job exists with a matching pod template spec, it is reused. If the spec
  changed (e.g., operator upgrade), the old Job is deleted and a new one is created.

---

## Fresh Deployments

When `status.installedRelease` is empty (first deployment), the operator uses the
existing simple `db_sync` path:

1. A single `{name}-db-sync` Job runs `keystone-manage db_sync` (no phase flags).
2. On successful completion, `status.installedRelease` is set to `spec.image.tag`.
3. No expand, migrate, or contract Jobs are created.

This avoids adding unnecessary upgrade overhead to new installations.

---

## Interrupted Upgrades

The upgrade state machine is resilient to operator restarts and API server timeouts
because all state is persisted in the Keystone CR status:

- `status.upgradePhase` persists across reconcile cycles and operator pod restarts.
- On restart, the reconciler reads the current phase and resumes from that point.
- Completed Jobs are detected via their existing status — the operator does not
  re-run a phase whose Job has already completed.
- `RunJob` idempotency prevents duplicate Jobs from being created if the operator
  restarts while a Job is still running.

### Resume Behavior by Phase

| Scenario | Behavior |
| --- | --- |
| Operator restarts during Expanding, expand Job still running | Reconciler polls the existing Job, requeues until complete |
| Operator restarts after expand Job completed | Reconciler detects completion, transitions to Migrating |
| Operator restarts during RollingUpdate | Reconciler checks Deployment readiness, transitions to Contracting when ready |
| Operator restarts during Contracting, contract Job completed | Reconciler detects completion, finalizes upgrade |

---

## Aborting an Upgrade

A sequential upgrade can be aborted before it completes by reverting
`spec.image.tag` to the value in `status.installedRelease`. This is the
supported escape hatch when an upgrade is wedged — for example, an expand or
migrate Job that fails permanently (exceeds its backoff limit) or whose Pod
cannot pull the target image.

When the operator observes `spec.image.tag == status.installedRelease` while an
upgrade is active (`status.upgradePhase` is set), it:

1. Deletes the `<cr-name>-db-expand`, `<cr-name>-db-migrate`, and
   `<cr-name>-db-contract` Jobs (with background propagation, so their Pods are
   removed too).
2. Clears `status.upgradePhase` and `status.targetRelease`.
3. Emits an `UpgradeAborted` event on the Keystone CR.
4. Requeues, so the next reconcile takes the steady-state `db_sync` path and
   restores `DatabaseReady` against the installed release.

```bash
# Abort an in-flight upgrade by reverting the tag to the installed release.
kubectl patch keystone <name> --type=merge \
  -p '{"spec":{"image":{"tag":"<installed-release>"}}}'
```

::: warning Abort is only safe before the contract phase
Expand and migrate are **additive** by design: they add the new columns and
tables and backfill data without dropping anything the installed release still
reads, so the pre-contract schema is a superset both releases run against.
Aborting during **Expanding**, **Migrating**, or **RollingUpdate** is therefore
safe. The **contract** phase drops the columns the new release no longer needs;
aborting during **Contracting** can leave the installed release pointed at a
schema missing fields it expects. The operator clears the upgrade state from any
active phase, so validate the database before relying on a Contracting-phase
abort. Once contract completes, the upgrade is finished and reverting the tag is
a downgrade, which the operator rejects.
:::

---

## Default-on Trust Flush at Upgrade Time

Starting with the release that ships default-on trust flush, the defaulting
webhook materializes `spec.trustFlush` whenever the field is omitted, so every Keystone
CR ends up running `keystone-manage trust_flush` hourly by default. This
applies to both fresh deployments and brownfield CRs whose original manifests
never set the field. For the CRD-level shape and `suspend: true` opt-out, see
[`TrustFlushSpec`](./keystone-crd.md#trustflushspec).

### Materialization on the Next Admission Round-trip

The webhook does not mutate stored objects in etcd at the moment the operator
upgrade completes — Kubernetes admission webhooks only run on `CREATE` and
`UPDATE` requests. As a result, an existing brownfield CR that lacks
`spec.trustFlush` retains that shape in etcd until something writes the
object. The trust-flush CronJob is created on the next admission round-trip,
which can happen via any of:

- A fresh `kubectl apply -f` (or GitOps reconciliation) of the CR manifest.
- A `kubectl edit` or `kubectl patch` on the same object.

> **Note:** Status subresource writes (which the controller does on its own)
> bypass the defaulting webhook, so the operator's reconciliation alone will
> not trigger materialization. Use one of the spec writes above, or see
> "Forcing Materialization" below.

### CronJob Creation Timeline

| Time | Cluster State |
| --- | --- |
| `t0` | New operator (with the default-on trust-flush webhook) is installed and Ready. Existing brownfield CR with no `spec.trustFlush` is unchanged in etcd. No `{name}-trust-flush` CronJob exists. |
| `t1` | A spec write occurs (apply / edit / patch). The webhook materializes `spec.trustFlush = {schedule: "0 * * * *", suspend: false}`; the persisted CR now carries the field. |
| `t1+ε` | The reconciler observes the updated CR and calls `job.EnsureCronJob` on the trust-flush spec. The `{name}-trust-flush` CronJob appears and `TrustFlushReady=True/TrustFlushReady` is set. |
| First `:00` minute after `t1+ε` | The Kubernetes CronJob controller creates the first Job pod, which runs `keystone-manage trust_flush` against the database. |

### Forcing Materialization

If you want to materialize the field immediately after an operator upgrade
without waiting for the next GitOps sync, trigger an empty patch through the
defaulting webhook:

```bash
kubectl patch keystone <name> --type=merge -p '{}'
```

The webhook still runs on this admission round-trip and materializes
`spec.trustFlush`. The reconciler then ensures the CronJob.

### Pre-upgrade Recommendation for Large Trust Tables

The first `trust_flush` execution on a cluster that has never run the command
walks the full `trust` table, deleting expired delegations in a single
transaction. On clusters with very large trust tables (hundreds of thousands
of expired rows or more), this first run can hold MariaDB locks long enough
to disturb concurrent Keystone API workloads.

To smooth the transition, either:

1. **Pre-flush before upgrade.** Run a one-shot trust flush against the old
   release with a recent `--date` so the bulk of the historical purge happens
   in a controlled maintenance window:

   ```bash
   # Replace 2026-04-19T00:00:00Z with a date 7 days before today (UTC, ISO 8601).
   # The literal date is used here instead of $(date -u -d '7 days ago' +%FT%TZ)
   # because GNU `date -d` is not available on macOS/BSD shells.
   kubectl exec -n openstack deploy/<name> -- \
     keystone-manage trust_flush --date 2026-04-19T00:00:00Z
   ```

2. **Stagger the rollout.** Roll the operator upgrade out one Keystone CR at a
   time on multi-CR clusters, and pause between CRs to confirm the first
   `{name}-trust-flush` Job pod completes within the SLO before continuing.
   Each CR's first run happens independently — staggering caps the
   concurrent database load.

After the first successful run, subsequent hourly runs only delete rows whose
`expires_at` fell within the previous hour, and lock pressure returns to
steady-state levels.

### Opting Out per CR

To keep an individual CR off the hourly cadence after upgrade — for example,
because that environment never issues trusts — set
`spec.trustFlush.suspend: true` rather than removing the field. The CronJob
resource is preserved with `spec.suspend: true` and can be unsuspended later
without a delete/recreate. Removing the field is not effective on a
webhook-enabled cluster: the next admission round-trip re-materializes the
hourly schedule.

---

## Troubleshooting

### Checking Upgrade Status

View the current upgrade state:

```bash
kubectl get keystone <name> -o jsonpath='{.status.installedRelease}'
kubectl get keystone <name> -o jsonpath='{.status.targetRelease}'
kubectl get keystone <name> -o jsonpath='{.status.upgradePhase}'
```

Or inspect all three fields together:

```bash
kubectl get keystone <name> -o jsonpath='{
  "installed": "{.status.installedRelease}",
  "target": "{.status.targetRelease}",
  "phase": "{.status.upgradePhase}"
}'
```

The `Release` printer column shows `installedRelease` in standard `kubectl get`
output:

```bash
kubectl get keystones
# NAME       READY   ENDPOINT                                            RELEASE   AGE
# keystone   True    http://keystone.openstack.svc:5000/v3               2025.2    7d
```

### Inspecting Conditions

Check the `DatabaseReady` condition for upgrade-specific reasons:

```bash
kubectl get keystone <name> -o jsonpath='{.status.conditions[?(@.type=="DatabaseReady")]}'
```

During an active upgrade, this shows the current phase reason and a message with
version strings:

```json
{
  "type": "DatabaseReady",
  "status": "False",
  "reason": "MigrateInProgress",
  "message": "Migrate phase running: 2025.2 -> 2026.1",
  "observedGeneration": 3
}
```

### Inspecting Upgrade Jobs

List the upgrade Jobs:

```bash
kubectl get jobs -l app.kubernetes.io/managed-by=keystone-controller \
  --sort-by=.metadata.creationTimestamp
```

Or directly by name pattern:

```bash
kubectl get jobs <name>-db-expand <name>-db-migrate <name>-db-contract
```

Check Job logs for a specific phase:

```bash
kubectl logs job/<name>-db-expand
kubectl logs job/<name>-db-migrate
kubectl logs job/<name>-db-contract
```

### Common Issues

#### UpgradePathInvalid

**Symptom:** `DatabaseReady=False`, reason `UpgradePathInvalid`.

**Cause:** The operator detected a non-sequential upgrade (e.g., `2024.2` to
`2026.1`).

**Resolution:** Only sequential upgrades are supported. Upgrade through each
intermediate release in order:

```yaml
# Step 1: 2024.2 -> 2025.1
spec:
  image:
    tag: "2025.1"
# Wait for upgrade to complete, then:
# Step 2: 2025.1 -> 2025.2
spec:
  image:
    tag: "2025.2"
# Wait for upgrade to complete, then:
# Step 3: 2025.2 -> 2026.1
spec:
  image:
    tag: "2026.1"
```

#### VersionParseError

**Symptom:** `DatabaseReady=False`, reason `VersionParseError`.

**Cause:** Either `status.installedRelease` or `spec.image.tag` is not a valid
`YYYY.N` format (e.g., `latest`, `abc`, `2025`).

**Resolution:** Use a valid OpenStack release tag in `spec.image.tag`. If
`installedRelease` is corrupted, manual status patching may be required (see
below).

#### UpgradeTargetChanged

**Symptom:** `DatabaseReady=False`, reason `UpgradeTargetChanged`.

**Cause:** `spec.image.tag` was changed during an active upgrade to a value
different from the current `targetRelease`.

**Resolution:** Choose one of:

- Revert `spec.image.tag` to `status.targetRelease` to continue the current
  upgrade.
- Revert `spec.image.tag` to `status.installedRelease` to abort the upgrade and
  return to the installed release (see
  [Aborting an Upgrade](#aborting-an-upgrade)).
- Wait for the current upgrade to complete before changing the tag again.

The operator does not support retargeting an upgrade to a third release
mid-flight.

#### ExpandFailed / MigrateFailed / ContractFailed

**Symptom:** `DatabaseReady=False`, reason `ExpandFailed`, `MigrateFailed`, or
`ContractFailed`.

**Cause:** The `keystone-manage db_sync --{phase}` command failed. This typically
indicates a database schema incompatibility, missing database connectivity, or an
issue with the Keystone image.

**Resolution:**

1. Check Job logs: `kubectl logs job/<name>-db-{phase}`
2. Check Job events: `kubectl describe job/<name>-db-{phase}`
3. Verify database connectivity from within the namespace.
4. Verify the Keystone image supports the `db_sync --{phase}` flags.
5. After fixing the root cause, delete the failed Job to allow the operator to
   retry: `kubectl delete job/<name>-db-{phase}`

#### Manual Status Patching

In exceptional cases (corrupted status, testing), the status subresource can be
patched directly:

```bash
# Reset upgrade state (use with caution)
kubectl patch keystone <name> --type=merge --subresource=status \
  -p '{"status":{"upgradePhase":"","targetRelease":"","installedRelease":"2025.2"}}'
```

::: warning
Manual status patching bypasses operator validation and can leave the database in
an inconsistent state. Only use this as a last resort when the operator cannot
recover automatically.
:::

---

## Sub-Resource Rename

A previous change dropped the `-api` suffix from operator-managed sub-resources.
Clusters deployed before this change have sub-resources named `<cr-name>-api`; <!-- keystone-api-legacy: pre-rename name documented by the upgrade runbook. -->
clusters deployed after that change carry sub-resources named `<cr-name>` (bare CR
name). When the operator is upgraded across this boundary, the rename takes effect
on the next reconcile that touches each sub-resource.

### Affected Sub-Resources

The following sub-resources are renamed from `<cr-name>-api` to `<cr-name>`: <!-- keystone-api-legacy: pre-rename name documented by the upgrade runbook. -->

- `Deployment`
- `Service` (ClusterIP)
- `PodDisruptionBudget`
- `HorizontalPodAutoscaler` (when `spec.autoscaling` is set)
- `NetworkPolicy`
- `HTTPRoute` (when `spec.gateway` is set)
- Container name and named container port (`5000`) inside the Deployment Pod template

The cluster-internal Service DNS therefore changes from
`http://<cr-name>-api.<namespace>.svc.cluster.local:5000/v3` to <!-- keystone-api-legacy: pre-rename DNS documented by the upgrade runbook. -->
`http://<cr-name>.<namespace>.svc.cluster.local:5000/v3`. This is reflected
in `status.endpoint` once the controller re-reconciles. See the
[Keystone CRD Sub-Resource Naming Convention section](./keystone-crd.md#sub-resource-naming-convention).

### Catalog Self-Heal via `keystone-manage bootstrap`

The bootstrap Job re-runs `keystone-manage bootstrap` whenever the CR
Generation changes, with `--bootstrap-{admin,internal}-url` arguments derived
from the new bare-name Service DNS. `keystone-manage bootstrap` is idempotent:
it overwrites the existing `identity` service endpoints in the catalog with
the supplied URLs. After a Generation bump on an upgraded cluster, the
internal/admin endpoints in the OpenStack catalog therefore self-heal to the
new `http://<cr-name>.<namespace>.svc.cluster.local:5000/v3` form on the
next bootstrap reconcile.

### Operator Workflows

Two paths are supported for the post-upgrade catalog refresh:

1. **Generation bump (recommended).** Apply any change to the Keystone CR
   `spec` (e.g., bump the image tag, tweak `spec.deployment.replicas`, or use a no-op
   annotation update on the CR `metadata`). The increased `metadata.generation`
   makes `reconcileBootstrap` re-run the bootstrap Job, which calls
   `keystone-manage bootstrap` with the new bare-name URLs. Catalog endpoints
   are overwritten in place.

2. **Manual `openstack endpoint set`.** If you cannot bump the Generation,
   patch the catalog directly:

   ```bash
   openstack endpoint list --service identity
   openstack endpoint set <endpoint-id> \
     --url http://<cr-name>.<namespace>.svc.cluster.local:5000/v3
   ```

   Repeat for the `admin` and `internal` interfaces. The `public` interface
   typically resolves through the Gateway and is unaffected by the rename.

### Manual Cleanup of Legacy Sub-Resources

The renamed sub-resources are new objects with new `metadata.uid` values and
fresh `ownerReferences` to the Keystone CR. The legacy `<cr-name>-api` <!-- keystone-api-legacy: pre-rename name documented by the upgrade runbook. -->
sub-resources from earlier operator releases carry their own owner references
to the same Keystone CR, but the current reconciler does **not** issue
`Delete` calls for them — it simply stops reconciling those names. As a
result, on an upgraded cluster the legacy objects persist alongside the new
bare-name objects until an operator removes them.

::: danger Cleanup is operator-driven, not automatic
There is currently no controller-side garbage collection of pre
sub-resources. Because the legacy objects retain valid
`ownerReferences` to the still-existing Keystone CR, the Kubernetes garbage
collector will not delete them on its own. Operators upgrading across the
rename boundary must remove them by hand. Skipping this step leaves stale
Deployments/Services/PDBs/HPAs/HTTPRoutes/NetworkPolicies in the namespace,
which can confuse `kubectl get`, score against `ResourceQuota`, and — in
the case of the legacy Service — keep an unused ClusterIP routable.
:::

Run the following commands once per upgraded namespace, substituting
`<namespace>` and `<cr-name>` (the value of `metadata.name` on the
Keystone CR). Each command is safe to re-run: `--ignore-not-found` makes
the deletes idempotent, so re-applying the runbook on a partially cleaned
namespace is a no-op for already-removed objects.

```bash
# Core API sub-resources (always present on a pre-rename cluster).
kubectl -n <namespace> delete deployment              <cr-name>-api --ignore-not-found  # keystone-api-legacy: targets the pre-rename Deployment.
kubectl -n <namespace> delete service                 <cr-name>-api --ignore-not-found  # keystone-api-legacy: targets the pre-rename Service.
kubectl -n <namespace> delete poddisruptionbudget     <cr-name>-api --ignore-not-found  # keystone-api-legacy: targets the pre-rename PodDisruptionBudget.

# Optional sub-resources — delete only if the corresponding spec.* field
# was set on the pre-upgrade CR. Running them unconditionally is safe
# because of --ignore-not-found.
kubectl -n <namespace> delete horizontalpodautoscaler <cr-name>-api --ignore-not-found  # keystone-api-legacy: targets the pre-rename HPA.
kubectl -n <namespace> delete networkpolicy           <cr-name>-api --ignore-not-found  # keystone-api-legacy: targets the pre-rename NetworkPolicy.
kubectl -n <namespace> delete httproute.gateway.networking.k8s.io \
                                                      <cr-name>-api --ignore-not-found  # keystone-api-legacy: targets the pre-rename HTTPRoute.
```

Deleting the legacy `Deployment` cascades to its `ReplicaSet`s and `Pod`s
through the standard Kubernetes ownerReference chain; no extra cleanup of
those child objects is required.

After the deletes complete, verify the namespace contains only the new
bare-name sub-resources:

```bash
kubectl -n <namespace> get deployment,service,pdb,hpa,networkpolicy,httproute.gateway.networking.k8s.io \
  --selector=app.kubernetes.io/name=keystone,app.kubernetes.io/instance=<cr-name>
```

A clean upgrade shows exactly one row per kind, all named `<cr-name>`
(without the `-api` suffix). Any row still ending in `-api` indicates a
missed delete — re-run the corresponding `kubectl delete` above.

::: warning Until a cleanup sub-reconciler exists
A future change may add an automated cleanup step to the operator. Until then,
the manual runbook above is the only supported path for removing legacy
leftovers; observed legacy objects on a live cluster will **not** disappear on
subsequent reconciles.
:::

---

## Limitations

1. **Sequential upgrades only.** Skip-level (SLURP) upgrades are not yet supported.
   Each intermediate release must be applied in order.

2. **No automatic rollback.** If an upgrade fails, manual intervention is required.
   The operator does not automatically revert to the previous release, but an
   in-flight upgrade can be aborted before the contract phase by reverting
   `spec.image.tag` to `status.installedRelease` (see
   [Aborting an Upgrade](#aborting-an-upgrade)).

3. **Single-service scope.** This feature covers only the Keystone operator's
   internal upgrade flow. Cross-service upgrade orchestration is handled by the
   c5c3-operator.

4. **No resource limits on upgrade Jobs.** Upgrade Jobs inherit no resource
   requests/limits (BestEffort QoS).

5. **Image compatibility required.** The `db_sync --expand`, `--migrate`, and
   `--contract` flags must be supported by the Keystone container image. If the
   image does not support these flags, the upgrade Jobs will fail with a clear
   error condition.
