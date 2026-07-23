---
title: Glance Upgrade Flow
quadrant: operator
---

# Glance Upgrade Flow

Reference documentation for the Glance expand-migrate-contract database upgrade
flow. When `spec.openStackRelease` advances to a new OpenStack release, the
operator runs phased database migrations while the Glance API keeps serving.

For CRD type definitions (including the upgrade status fields), see
[Glance CRD](./glance-crd.md). For the sub-reconciler pipeline and condition
vocabulary, see [Glance Reconciler Architecture](./glance-reconciler.md).

---

## Overview

OpenStack services use the expand-migrate-contract pattern to upgrade a database
schema without taking the API down. The three phases let the old and new code
run against the same database while the schema moves forward:

1. **Expand.** Add the new columns, tables, and triggers so the installed
   release can still read and write while the new elements populate.
2. **Migrate.** Backfill and transform data into the new schema elements.
3. **Contract.** Drop the old columns, tables, and triggers the new release no
   longer needs.

Glance was the second operator to adopt this machine. The phase choreography
lives in `internal/common/database` (`upgrade.go`) and is shared with Keystone;
the Glance controller supplies the service-specific parts: the
`spec.openStackRelease` seam, the `glance-manage` phase commands, and the
`DatabaseReady` condition it reports on. Keystone's own behaviour is documented
in [Keystone Upgrade Flow](../keystone/keystone-upgrade-flow.md).

The flow is driven by two sub-reconcilers. `reconcileDatabase` runs the expand,
migrate, and contract phases; `reconcileDeployment` owns the rolling update
between migrate and contract.

---

## Trigger

An upgrade starts when `spec.openStackRelease` moves one release forward, for
example `2025.2` to `2026.1`. Glance keys the upgrade off this field rather than
the image tag, so the image reference must be bumped in the same edit: the phase
Jobs run `spec.image`, and the new release's migration tree owns the schema
deltas. See [Image and Release Lockstep](#image-and-release-lockstep) for the
contract and its digest-pinning nuance.

Bumping the image alone, with `spec.openStackRelease` unchanged, is not an
upgrade. It re-runs the single-pass sync; see
[Fresh Installs and Patch Bumps](#fresh-installs-and-patch-bumps).

---

## Version Format

`spec.openStackRelease` follows the OpenStack date-based scheme, two releases per
year:

| Component | Format | Examples |
| --- | --- | --- |
| Release | `YYYY.N` where N is 1 or 2 | `2025.1`, `2025.2`, `2026.1` |

The CRD pattern `^\d{4}\.[12]$`, the validating webhook, and
`release.ParseRelease` agree on this shape, so a non-cadence minor such as
`2025.9` is rejected at admission.

### Accepted Transitions

The operator accepts a same-release reconcile (a no-op for the schema) and a
single sequential step forward. Everything else is refused:

| From | To | Accepted | Reason |
| --- | --- | --- | --- |
| `2025.1` | `2025.2` | Yes | Same year, minor +1 |
| `2025.2` | `2026.1` | Yes | Year +1, minor 2 to minor 1 |
| `2024.2` | `2026.1` | No | Skip-level (skips `2025.x`) |
| `2025.2` | `2026.2` | No | Skip-level (skips `2026.1`) |
| `2026.1` | `2025.2` | No | Downgrade |

---

## Status Fields

Three status fields track the upgrade, all written through the status
subresource.

| Field | Type | During an upgrade | Steady state |
| --- | --- | --- | --- |
| `status.installedRelease` | `string` | The release installed **before** the upgrade began | The currently installed release |
| `status.targetRelease` | `string` | The release being upgraded **to** | Empty (`""`) |
| `status.upgradePhase` | `UpgradePhase` | The current phase (see below) | Empty (`""`) |

`installedRelease` is promoted to `targetRelease` only after the contract phase
completes; it is the `Release` printer column in `kubectl get glances`.
`targetRelease` is set when the upgrade initiates and cleared on completion or
abort, and is not stamped on every steady-state pass. `upgradePhase` takes one
of four values while an upgrade is active:

| Value | Meaning |
| --- | --- |
| `Expanding` | `glance-manage db expand` running on the new image |
| `Migrating` | `glance-manage db migrate` running on the new image |
| `RollingUpdate` | Waiting for the Deployment to roll out on the new image |
| `Contracting` | `glance-manage db contract` running on the new image |

---

## Phases

The upgrade walks a fixed sequence. Each transition is driven by a phase Job
completing or by the Deployment reporting ready.

```text
spec.openStackRelease bumped (e.g. 2025.2 -> 2026.1, image in lockstep)
        |
        v
  Expanding  --- <name>-db-expand: glance-manage db expand --- Job complete
        |
        v
  Migrating  --- <name>-db-migrate: glance-manage db migrate --- Job complete
        |
        v
  RollingUpdate --- Deployment rolls to the new image, waits for readiness
        |
        v
  Contracting --- <name>-db-contract: glance-manage db contract --- Job complete
        |
        v
  installedRelease = "2026.1", targetRelease = "", upgradePhase = ""
```

Each job-running phase creates one distinctly named Job on `spec.image` (the new
release image) with `backoffLimit: 4`:

| Phase | Job name | Command |
| --- | --- | --- |
| Expanding | `<name>-db-expand` | `glance-manage --config-dir /etc/glance/glance-api.conf.d/ db expand` |
| Migrating | `<name>-db-migrate` | `glance-manage --config-dir /etc/glance/glance-api.conf.d/ db migrate` |
| Contracting | `<name>-db-contract` | `glance-manage --config-dir /etc/glance/glance-api.conf.d/ db contract` |

Expand and migrate run with the new image because the target release's migration
tree owns the schema deltas: running expand with the old binary would leave the
contract step ahead of expand and fail the service's upgrade-order check.

### Rolling Update and the Launch-Mode Flip

`RollingUpdate` builds no Job. `reconcileDatabase` returns an empty result, the
pipeline proceeds to `reconcileDeployment`, and Kubernetes rolls the pods onto
the new image. Old pods keep serving the expanded schema until the new pods pass
their readiness checks, so the API stays available.

The launch mode derives from `spec.openStackRelease`: the eventlet `glance-api`
server below `2026.1`, uWSGI from `2026.1` onward. A `2025.2` to `2026.1`
upgrade therefore switches the container command from eventlet to uWSGI during
this rollout. Both modes load the same two `--config-dir` roots; the reconciler
reference covers the [launch modes](./glance-reconciler.md#launch-modes) in
detail.

Once the Deployment reports ready, `reconcileDeployment` flips the phase from
`RollingUpdate` to `Contracting`, emits `DeploymentRolloutComplete`, and
requeues so `reconcileDatabase` runs the contract Job on the next pass. The
status endpoint is not stamped on that flip pass.

When the contract Job completes, `installedRelease` is promoted to
`targetRelease`, `targetRelease` and `upgradePhase` are cleared, and
`DatabaseReady` is set `True` with reason `DatabaseSynced`.

---

## Condition Reasons

Every phase reports through `DatabaseReady`; the upgrade adds no new condition
types. The message carries the source and target release strings, for example
`Migrate phase running: 2025.2 → 2026.1`.

### In progress

| Reason | Phase |
| --- | --- |
| `ExpandInProgress` | Expanding |
| `MigrateInProgress` | Migrating |
| `UpgradeRollingUpdate` | RollingUpdate |
| `ContractInProgress` | Contracting |

### Failure

| Reason | Cause |
| --- | --- |
| `VersionParseError` | The installed or target release is not a valid `YYYY.N` string |
| `DowngradeNotSupported` | The target release is older than the installed release |
| `UpgradePathInvalid` | The jump is not a single sequential step (skip-level) |
| `UpgradeTargetChanged` | `spec.openStackRelease` changed to a third value mid-upgrade |
| `ExpandFailed` | The expand Job failed past its backoff limit |
| `MigrateFailed` | The migrate Job failed past its backoff limit |
| `ContractFailed` | The contract Job failed past its backoff limit |

A validation failure and a phase-Job failure both set `DatabaseReady=False`,
emit a Warning event, and return an error, so the controller backs off and
retries. The `UpgradeTargetChanged` guard means an upgrade that is already
running neither advances nor restarts when the spec target moves again: revert
`spec.openStackRelease` to `targetRelease` to continue, or to `installedRelease`
to abort.

---

## Events

The upgrade path emits these events on the Glance CR. The reasons come from
`internal/common/database` and are shared with Keystone.

| Type | Reason | Trigger |
| --- | --- | --- |
| Normal | `UpgradeInitiated` | An accepted release bump starts the flow |
| Normal | `ExpandComplete` | The expand Job succeeded |
| Normal | `MigrateComplete` | The migrate Job succeeded |
| Normal | `DeploymentRolloutComplete` | The Deployment rolled out; phase flips to Contracting |
| Normal | `UpgradeComplete` | The contract Job succeeded; the upgrade finished |
| Normal | `UpgradeAborted` | The upgrade was aborted (see below) |
| Warning | `VersionParseError` | Unparseable installed or target release |
| Warning | `DowngradeNotSupported` | Target older than installed |
| Warning | `UpgradePathInvalid` | Non-sequential jump |
| Warning | `UpgradeTargetChanged` | Spec target changed mid-upgrade |
| Warning | `ExpandFailed` | The expand Job failed permanently |
| Warning | `MigrateFailed` | The migrate Job failed permanently |
| Warning | `ContractFailed` | The contract Job failed permanently |

No events fire for the in-progress polling states, so a requeue loop does not
flood the event stream.

---

## Aborting an Upgrade

Revert `spec.openStackRelease` to the value in `status.installedRelease` while an
upgrade is active. The operator then:

1. Deletes the `<name>-db-expand`, `<name>-db-migrate`, and `<name>-db-contract`
   Jobs (background propagation removes their Pods too).
2. Clears `status.upgradePhase` and `status.targetRelease`.
3. Emits a Normal `UpgradeAborted` event.
4. Requeues, so the next reconcile takes the steady-state `db sync` path and
   restores `DatabaseReady` against the installed release.

```bash
# Abort an in-flight upgrade by reverting the release to the installed one.
kubectl patch glance <name> --type=merge \
  -p '{"spec":{"openStackRelease":"<installed-release>"}}'
```

This is the escape hatch for an upgrade wedged on an expand or migrate Job that
cannot make progress.

::: warning Abort is only safe before the contract phase
Expand and migrate are additive: they add columns and tables and backfill data
without dropping anything the installed release still reads, so the pre-contract
schema is a superset both releases run against. Aborting during `Expanding`,
`Migrating`, or `RollingUpdate` is therefore safe. Contract drops the columns the
new release no longer needs; aborting during `Contracting` can leave the
installed release pointed at a schema missing fields it expects. The operator
clears the upgrade state from any phase, so validate the database before relying
on a Contracting-phase abort. Once contract completes, reverting the release is a
downgrade, which the operator rejects.
:::

---

## Image and Release Lockstep

`spec.image` and `spec.openStackRelease` are separate fields — the release
drives tracking, the launch mode, and upgrade detection, while the phase Jobs and
the Deployment run `spec.image` — so digest pinning stays possible. The
operator's contract is that the two are bumped together, and for a tag-pinned
image the reconciler enforces it: when `spec.image.tag` parses as an OpenStack
release that differs from `spec.openStackRelease` (the patch suffix is ignored,
so `2026.1-p1` still matches `2026.1`), `DatabaseReady` goes `False` with reason
`ImageReleaseMismatch` and neither the upgrade nor the steady-state sync advances
until the image is bumped in lockstep. This closes the gap where bumping the
release alone would run the wrong `glance-manage` binary as a no-op and falsely
promote `installedRelease`. A digest-pinned image, or a tag that does not parse
as a release, carries no comparable release string and is trusted to match the
declared `spec.openStackRelease`.

Keystone keys its upgrade off the image tag and skips release detection for a
digest-pinned image. Glance keys off `spec.openStackRelease`, which is always
set, so a digest-pinned Glance still upgrades: on a release bump the phase Jobs
run by digest. `spec.openStackRelease` also selects the launch mode, so a
digest-pinned image resolves both a schema target and a launch command.

---

## Fresh Installs and Patch Bumps

A fresh install (empty `status.installedRelease`) runs a single `<name>-db-sync`
Job (`glance-manage db sync` followed by `db load_metadefs`) and sets
`installedRelease` to `spec.openStackRelease` on success. No expand, migrate, or
contract Jobs are created.

A change that keeps `spec.openStackRelease` the same, such as bumping the image
to a patch build, is not an upgrade either. It stays on the single-pass sync: the
pod-spec-hash gate re-runs the `db-sync` Job on the new image, and
`glance-manage db sync` applies any pending migrations in one idempotent pass.

---

## Post-Upgrade Metadata Definitions

The phase Jobs do not touch Glance's metadata definitions. After contract
completes and `installedRelease` advances, the steady-state `<name>-db-sync` Job
is rebuilt with the new image. Its pod-spec-hash gate re-runs it, and its
idempotent `db sync && db load_metadefs` pass inserts the new release's metadata
definitions. Namespaces already present are skipped, so the load only adds what
the new release ships.
