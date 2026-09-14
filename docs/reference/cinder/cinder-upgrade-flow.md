---
title: Cinder Upgrade Flow
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Cinder Upgrade Flow

Reference documentation for the Cinder release upgrade: what the operator runs
when `spec.openStackRelease` advances, in which order the block-storage
processes move onto the new image, and how to stop an upgrade that is stuck.
For the field definitions see [Cinder CRD](./cinder-crd.md); for the
sub-reconciler pipeline and the condition vocabulary see
[Cinder Reconciler Architecture](./cinder-reconciler.md).

---

## Overview

OpenStack services move a database schema forward under a live API with the
expand-migrate-contract pattern. Additive schema work lands first, every process
is restarted onto the new code, and only then does the data backfill run against
rows the old code no longer writes. The operator walks four phases in a fixed
order: `Expanding`, `Migrating`, `RollingUpdate`, `Contracting`.

The phase machine lives in `internal/common/database` (`upgrade.go`) and is
shared with Keystone and Glance. Cinder supplies the service-specific parts: the
`spec.openStackRelease` seam, the `cinder-manage` and `cinder-status` phase
commands, and the `DatabaseReady` condition every phase reports on. The sibling
pages are [Glance Upgrade Flow](../glance/glance-upgrade-flow.md) and
[Keystone Upgrade Flow](../keystone/keystone-upgrade-flow.md).

Cinder has no separate expand and contract verbs. `cinder-manage db sync` is an
idempotent alembic upgrade to head, so the expand phase runs the same command
the steady state does, and the contract phase runs the online data migrations
that backfill what the new schema needs.

---

## Trigger

An upgrade starts when `spec.openStackRelease` moves one release forward, for
example `2025.2` to `2026.1`. The release field drives tracking and upgrade
detection, while every phase Job and every workload runs `spec.image`. The image
reference is therefore bumped in the same edit, and the operator refuses to
advance while the two disagree; see
[Image and Release Lockstep](#image-and-release-lockstep).

Bumping the image alone, with `spec.openStackRelease` unchanged, is not an
upgrade. It stays on the single-pass sync; see
[Fresh Installs and Patch Bumps](#fresh-installs-and-patch-bumps).

---

## Version Format

`spec.openStackRelease` follows the OpenStack date-based scheme, two releases per
year:

| Component | Format | Examples |
| --- | --- | --- |
| Release | `YYYY.N` where N is 1 or 2 | `2025.1`, `2025.2`, `2026.1` |

The CRD pattern, the validating webhook, and `release.ParseRelease` agree on
this shape, so a non-cadence minor such as `2025.9` is rejected at admission.

### Accepted Transitions

`IsSequentialUpgrade` accepts a single step forward and nothing else. A
same-release reconcile is a no-op for the schema; a skip-level jump and a
downgrade are both refused:

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
| `status.installedRelease` | `string` | The release installed before the upgrade began | The currently installed release |
| `status.targetRelease` | `string` | The release being upgraded to | Empty (`""`) |
| `status.upgradePhase` | `UpgradePhase` | The current phase (see below) | Empty (`""`) |

`installedRelease` is promoted to `targetRelease` only after the contract phase
completes, so a CR that failed mid-upgrade still reports the release its schema
is actually at. `targetRelease` is set when the upgrade initiates and cleared on
completion or abort. `upgradePhase` takes one of four values while an upgrade is
active:

| Value | Meaning |
| --- | --- |
| `Expanding` | `cinder-manage db sync` running on the target image |
| `Migrating` | `cinder-status upgrade check` running on the target image |
| `RollingUpdate` | Waiting for the four Deployments to converge on the target image |
| `Contracting` | `cinder-manage db online_data_migrations` running on the target image |

---

## Phases

Each transition is driven by a phase Job completing or by every Deployment
reporting a finished rollout.

```text
spec.openStackRelease bumped (e.g. 2025.2 -> 2026.1, image in lockstep)
        |
        v
  Expanding      <cinder>-db-expand: cinder-manage db sync
        |
        v
  Migrating      <cinder>-db-migrate: cinder-status upgrade check
        |
        v
  RollingUpdate  scheduler, volume services, backup, API roll onto the new image
        |
        v
  Contracting    <cinder>-db-contract: cinder-manage db online_data_migrations
        |
        v
  installedRelease = "2026.1", targetRelease = "", upgradePhase = ""
```

| Phase | What runs | Job |
| --- | --- | --- |
| `Expanding` | `cinder-manage --config-dir /etc/cinder/cinder.conf.d db sync` on the target image while every service still runs the installed release. Between the cinder 27.0.0 and 28.0.0 trees the alembic revision set is identical, so this pass applies zero schema revisions | `<cinder>-db-expand` |
| `Migrating` | `cinder-status --config-dir /etc/cinder/cinder.conf.d upgrade check` inside a tolerant wrapper. The wrapper writes `cinder-status upgrade check exit <rc>` to the pod's termination message and exits 0 for rc 0, 1 and 2, so no severity the check reports can wedge the Job; anything else fails with its own code | `<cinder>-db-migrate` |
| `RollingUpdate` | No Job. The four workloads roll in the order `scheduler → volume → backup → api`, because that is the order the pipeline ensures them in, and each step is gated on its own rollout before the next one runs: `<cinder>-scheduler`, then every `<cinder>-volume-<name>`, then `<cinder>-backup`, then the API `<cinder>` | — |
| `Contracting` | `cinder-manage --config-dir /etc/cinder/cinder.conf.d db online_data_migrations`, which exits 0 or 2 and never 1: without `--max_count` it loops in batches until a pass migrates nothing, so the "work remains" exit code is unreachable. The only online migration at these tags is `remove_temporary_admin_metadata_data_migration` | `<cinder>-db-contract` |

Every phase Job runs `spec.image`, the target-release image, with
`backoffLimit: 4`. The gate during `RollingUpdate` is stricter than steady-state
readiness: a Deployment counts as rolled out only once every replica is updated,
ready and counted, because the surge-tolerant readiness signal turns true while
old-image pods still serve, and the contract phase would then run migrations
those pods have no code for.

### The second roll

Promoting `installedRelease` changes the `cinder.c5c3.io/installed-release` pod
annotation the scheduler, the volume services and the backup service carry, so
those three roll a second time after `Contracting`. Each caches the RPC version
its peers announced at startup, and that cache pins the wire format for the life
of the process. The API carries no such annotation: it rolled last during
`RollingUpdate` and came up holding the new minimum. The mechanism is described
under [Release upgrade](./cinder-reconciler.md#release-upgrade).

### The pre-flight checks

`cinder-status upgrade check` runs seven checks at cinder 28.0.0: Backup Driver
Path, Use of Policy File, Removed Drivers, Periodic Interval Use, Service UUIDs,
Attachment specs, and Use of Nested Quota Driver. At 27.0.0 an eighth, Windows
Driver Path, runs alongside them. Two of them have consequences for how the
operator wires the Job.

Use of Policy File locates the configuration through
`CONF.find_file('cinder.conf')` and warns when it finds nothing, so the rendered
file the Jobs mount has to carry the name `cinder.conf`. That is what the
ConfigMap data key `cinderConfDataKey` in
`operators/cinder/internal/controller/reconcile_config.go` sets, and the whole
ConfigMap is mounted at `/etc/cinder/cinder.conf.d`.

Service UUIDs returns a failed check, exit code 2, whenever a single volume row
has `service_uuid IS NULL`. One volume left in `error` by a failed scheduling
attempt is enough. A cluster in that state would otherwise be unable to upgrade
at all, which is why the Migrate Job tolerates exit 2 and the operator records
`UpgradeCheckWarnings` instead of wedging the CR in `MigrateFailed`.

---

## Condition Reasons

Every phase reports through `DatabaseReady`; the upgrade adds no new condition
type. The message carries the source and target release strings.

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
| `UpgradePathInvalid` | The requested jump is not a single sequential step |
| `UpgradeTargetChanged` | `spec.openStackRelease` changed to a third value during an active upgrade |
| `ExpandFailed` | The expand phase Job failed permanently |
| `MigrateFailed` | The migrate phase Job failed permanently |
| `ContractFailed` | The contract phase Job failed permanently |

A validation failure and a phase-Job failure both set `DatabaseReady=False`,
emit a Warning event, and return an error, so the controller backs off and
retries. Under `UpgradeTargetChanged` a running upgrade neither advances nor
restarts: revert `spec.openStackRelease` to `targetRelease` to continue, or to
`installedRelease` to abort.

---

## Events

The upgrade emits these events on the Cinder CR. They are documented with their
example messages in [Controller Events](./cinder-events.md#upgrade).

| Type | Reason | Trigger |
| --- | --- | --- |
| Normal | `UpgradeInitiated` | An accepted release bump starts the upgrade |
| Normal | `ExpandComplete` | The expand phase Job succeeded |
| Normal | `MigrateComplete` | The migrate phase Job succeeded |
| Normal | `UpgradeCheckCompleted` | The upgrade check exited 0, or its exit code could not be read off the pod |
| Warning | `UpgradeCheckWarnings` | The same check exited 1 or 2; neither wedges the upgrade |
| Normal | `DeploymentRolloutComplete` | The API Deployment rolled out; the phase flips to Contracting |
| Normal | `UpgradeComplete` | The contract phase Job succeeded; the upgrade finished |
| Normal | `UpgradeAborted` | `spec.openStackRelease` was reverted to the installed release |
| Warning | `UpgradeCheckEventEmissionDeferred` | Patching the last-observed Job UID annotation failed; the check report moves to the next reconcile |
| Warning | `VersionParseError` | Unparseable installed or target release |
| Warning | `DowngradeNotSupported` | Target older than installed |
| Warning | `UpgradePathInvalid` | Non-sequential jump |
| Warning | `UpgradeTargetChanged` | Spec target changed mid-upgrade |
| Warning | `ExpandFailed` | The expand phase Job failed permanently |
| Warning | `MigrateFailed` | The migrate phase Job failed permanently |
| Warning | `ContractFailed` | The contract phase Job failed permanently |

The in-progress polling states emit nothing, so a requeue loop does not flood
the event stream.

---

## Aborting an Upgrade

Revert `spec.openStackRelease` to the value in `status.installedRelease` while an
upgrade is active. The operator then:

1. Deletes the `<cinder>-db-expand`, `<cinder>-db-migrate`, and
   `<cinder>-db-contract` Jobs (background propagation removes their Pods too).
2. Clears `status.upgradePhase` and `status.targetRelease`.
3. Emits a Normal `UpgradeAborted` event.
4. Requeues, so the next reconcile takes the steady-state `db sync` path and
   restores `DatabaseReady` against the installed release.

```bash
# Abort an in-flight upgrade by reverting both fields to the installed release.
# Digest-pinned image: drop the "image" key — tag and digest are mutually
# exclusive, and the whole patch is rejected if both are set.
kubectl patch cinder <name> --type=merge \
  -p '{"spec":{"openStackRelease":"<installed-release>","image":{"tag":"<installed-release>"}}}'
```

Revert `spec.image` in the same patch. The abort trigger itself needs only the
release field — the lockstep guard is skipped while an upgrade is active and
`spec.openStackRelease` has been reverted, which is what keeps a wedged upgrade
unstuckable. But the abort clears `status.upgradePhase`, and with the phase gone
the next reconcile is an ordinary one: the guard runs unconditionally there, and
a `spec.image.tag` still naming the target release parks the CR at
`DatabaseReady=False` under `ImageReleaseMismatch` instead of letting the
steady-state `db sync` run. Reverting the release alone therefore triggers the
abort and then stalls on it.

A digest-pinned image, or a tag that does not parse as a release, carries no
release string for the guard to compare, so for those the release field is the
whole patch — and the Deployments keep running the target image until that
digest is rolled back too.

::: warning Abort is only safe before the contract phase
Expand and migrate are additive: they add schema elements and read the database
without dropping anything the installed release still needs, so the pre-contract
schema is a superset both releases run against. Aborting during `Expanding`,
`Migrating`, or `RollingUpdate` is therefore safe. The contract phase backfills
rows for the new code; aborting during `Contracting` can leave the installed
release reading data it did not write. The operator clears the upgrade state
from any phase, so validate the database before relying on a Contracting-phase
abort. Once contract completes, reverting the release is a downgrade, which the
operator rejects.
:::

---

## Image and Release Lockstep

`spec.image` and `spec.openStackRelease` are separate fields so digest pinning
stays possible: the release drives tracking and upgrade detection, the phase
Jobs and the four Deployments run the image. The operator's contract is that the
two are bumped together, and for a tag-pinned image the reconciler enforces it.
When `spec.image.tag` parses as an OpenStack release that differs from
`spec.openStackRelease`, `DatabaseReady` goes `False` and neither the upgrade nor
the steady-state sync advances until the image is bumped. The patch suffix is
ignored, so a patched build tagged `2026.1-p1` still matches release `2026.1`.

Without the guard, a lone release bump would run the old `cinder-manage` binary
against a schema already at its own head. The Jobs would exit 0 as no-ops and
the flow would promote `installedRelease` to a release the pods neither run nor
migrated to. A digest-pinned image, or a tag that does not parse as a release,
carries no comparable release string and is trusted to match the declared
`spec.openStackRelease`.

The guard is skipped on one path: while an upgrade is active and
`spec.openStackRelease` has been reverted to `installedRelease`. That is the
abort trigger, and it has to stay reachable while the two fields disagree.

---

## Fresh Installs and Patch Bumps

A fresh install has an empty `status.installedRelease`. It runs the single
`<cinder>-db-sync` Job, `cinder-manage db sync`, and sets `installedRelease` to
`spec.openStackRelease` on success. No phase is entered and no expand, migrate,
or contract Job is created.

A change that keeps `spec.openStackRelease` the same, such as bumping the image
to a patch build, is not an upgrade either. The pod-spec-hash gate re-runs the
`db-sync` Job on the new image, `cinder-manage db sync` applies any pending
migrations in one pass, and the Deployments roll without the phase machine.
