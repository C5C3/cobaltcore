---
title: Keystone Upgrade Flow
quadrant: operator
feature: CC-0056
---

# Keystone Upgrade Flow

Reference documentation for the Keystone expand-migrate-contract database upgrade
flow (CC-0056). The operator automatically performs phased database migrations when
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
the upgrade completes successfully. During an active upgrade, if `spec.image.tag`
is changed to a value that differs from both `installedRelease` and `targetRelease`,
the operator blocks with an `UpgradeTargetChanged` condition.

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
  |  +- Tag changed during upgrade? -> block with UpgradeTargetChanged
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
# keystone   True    http://keystone-api.openstack.svc:5000/v3           2025.2    7d
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

**Resolution:** Either revert `spec.image.tag` to match `status.targetRelease` to
continue the current upgrade, or wait for the current upgrade to complete before
changing the tag again. The operator does not support changing the upgrade target
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

## Limitations

1. **Sequential upgrades only.** Skip-level (SLURP) upgrades are not yet supported.
   Each intermediate release must be applied in order.

2. **No automatic rollback.** If an upgrade fails, manual intervention is required.
   The operator does not automatically revert to the previous release.

3. **Single-service scope.** This feature covers only the Keystone operator's
   internal upgrade flow. Cross-service upgrade orchestration is handled by the
   c5c3-operator.

4. **No resource limits on upgrade Jobs.** Upgrade Jobs inherit no resource
   requests/limits (BestEffort QoS). This is tracked under CC-0042.

5. **Image compatibility required.** The `db_sync --expand`, `--migrate`, and
   `--contract` flags must be supported by the Keystone container image. If the
   image does not support these flags, the upgrade Jobs will fail with a clear
   error condition.
