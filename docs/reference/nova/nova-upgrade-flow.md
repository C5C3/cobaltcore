---
title: Nova Upgrade Flow
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Nova Upgrade Flow

Reference documentation for the Nova release upgrade: what the operator runs
when `spec.openStackRelease` advances, in which order the five compute
control-plane processes move onto the new image, and how to stop an upgrade
that is stuck. For the field definitions see [Nova CRD](./nova-crd.md); for the
sub-reconciler pipeline and the condition vocabulary see
[Nova Reconciler Architecture](./nova-reconciler.md).

---

## Overview

OpenStack services move a database schema forward under a live API with the
expand-migrate-contract pattern. Additive schema work lands first, every process
is restarted onto the new code, and only then does the data backfill run against
rows the old code no longer writes. The operator walks four phases in a fixed
order: `Expanding`, `Migrating`, `RollingUpdate`, `Contracting`.

The phase machine lives in `internal/common/database` (`upgrade.go`) and is
shared with Keystone, Glance and Cinder. Nova supplies the service-specific
parts: the `nova-status` and `nova-manage` phase commands, the five workloads
the rolling update has to converge, and the `DatabaseReady` condition every
phase reports on. The sibling pages are
[Cinder Upgrade Flow](../cinder/cinder-upgrade-flow.md),
[Glance Upgrade Flow](../glance/glance-upgrade-flow.md) and
[Keystone Upgrade Flow](../keystone/keystone-upgrade-flow.md).

Nova migrates two schemas, `nova_api` and the cell schema, and splits the work
differently from the phase names. Both migrations run in the expand phase,
because both are additive. The migrate phase is a read, since nova has no
migrate verb of its own. The contract phase runs the online data migrations,
once for the cell schema and once more for cell0.

---

## Trigger

An upgrade starts when `spec.openStackRelease` moves one release forward, for
example `2025.2` to `2026.1`. The release field drives tracking and upgrade
detection, while every phase Job and every workload runs `spec.image`. The image
reference is therefore bumped in the same edit, and the operator refuses to
advance while the two disagree; see
[Image and Release Lockstep](#image-and-release-lockstep).

On a ControlPlane both fields come from the plane. The projection sets the
child's `spec.openStackRelease` from the plane's own and derives the image
`ghcr.io/c5c3/nova:{spec.openStackRelease}` unless `services.nova.image`
overrides it, so bumping the ControlPlane's release moves both in one edit.

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
completes, so a CR that failed mid-upgrade still reports the release its schemas
are actually at. `targetRelease` is set when the upgrade initiates and cleared on
completion or abort. `upgradePhase` takes one of four values while an upgrade is
active:

| Value | Meaning |
| --- | --- |
| `Expanding` | `nova-status upgrade check`, then `nova-manage api_db sync` and `nova-manage db sync`, running on the target image |
| `Migrating` | `nova-manage cell_v2 list_cells` running on the target image |
| `RollingUpdate` | Waiting for the five Deployments to converge on the target image |
| `Contracting` | `nova-manage db online_data_migrations` running on the target image, for the cell schema and then for cell0 |

---

## Phases

Each transition is driven by a phase Job completing or by every rendered role
reporting a finished rollout.

```text
spec.openStackRelease bumped (e.g. 2025.2 -> 2026.1, image in lockstep)
        |
        v
  Expanding      {name}-db-expand: nova-status upgrade check,
        |                          nova-manage api_db sync, nova-manage db sync
        v
  Migrating      {name}-db-migrate: nova-manage cell_v2 list_cells
        |
        v
  RollingUpdate  conductor, scheduler, metadata, console proxy, API roll onto the new image
        |
        v
  Contracting    {name}-db-contract: nova-manage db online_data_migrations, cell then cell0
        |
        v
  installedRelease = "2026.1", targetRelease = "", upgradePhase = ""
```

| Phase | What runs | Job |
| --- | --- | --- |
| `Expanding` | `nova-status upgrade check`, then `nova-manage --config-dir /etc/nova/nova.conf.d api_db sync` and `nova-manage --config-dir /etc/nova/nova.conf.d db sync` on the target image, while every process still runs the installed release. Both migrations are additive, so the old release keeps running against the widened schemas. Between 2025.2 and 2026.1 neither schema takes a revision (decision D3 of [#1014](https://github.com/C5C3/cobaltcore/issues/1014), lab evidence in [#1015](https://github.com/C5C3/cobaltcore/issues/1015#issuecomment-5685726178), section (b)) | `{name}-db-expand` |
| `Migrating` | `nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 list_cells`. Nova has no migrate verb of its own, so the phase is a read that proves the new code can address the `nova_api` schema the expand phase migrated | `{name}-db-migrate` |
| `RollingUpdate` | No Job. The roles roll in the order `conductor -> scheduler -> metadata -> novncproxy -> api`, because that is the order the pipeline ensures them in: `{name}-conductor`, `{name}-scheduler`, `{name}-metadata`, `{name}-novncproxy` while the console proxy is enabled, then the API `{name}` | |
| `Contracting` | `nova-manage --config-dir /etc/nova/nova.conf.d db online_data_migrations --max-count 1000` in a loop, first against the cell schema and then against cell0. Between 2025.2 and 2026.1 there is no data migration to run, so both loops finish on their first batch | `{name}-db-contract` |

Every phase Job runs `spec.image`, the target-release image, with
`backoffLimit: 4`, so a phase gets five tries before it fails. A try against a
database the Job cannot reach is slow: `nova-manage api_db sync` retries its
connection and exits 255 after 207 seconds (same lab evidence), so each such try
costs about 3.5 minutes.

### The second roll

Promoting `installedRelease` changes the `nova.c5c3.io/installed-release` pod
annotation the conductor, the scheduler, the metadata API and the console proxy
carry, so those four roll a second time after `Contracting`. The scheduler and
the conductor cache the RPC version their peers announced at startup, and that
cache pins the wire format for the life of the process; the metadata API and the
console proxy read rows the contract phase rewrites. The API carries no such
annotation: it rolled last during `RollingUpdate` and came up holding the new
minimum.

### The pre-flight checks

The expand script runs `nova-status upgrade check` ahead of the two migrations
and writes `nova-status upgrade check exit <rc>` to the pod's termination log
before it acts on the code. It reads the exit code as a severity rather than as
a success flag:

| Exit code | Meaning | Job |
| --- | --- | --- |
| `0` | Every check passed | Continues with the migrations |
| `1` | Warnings a correct deployment can keep. A deployment without Cinder keeps the volume-attachment warnings, because those checks report on an integration it does not run | Continues with the migrations |
| `2` and above | A check failed, typically a cell mapping the `{name}-db-sync` Job has not written yet | Fails with that exit code |

The operator reads the termination log off the Job's pod and reports it as an
event on the Nova CR: `UpgradeCheckCompleted` for exit 0, or when the code could
not be read off the pod, and the Warning `UpgradeCheckWarnings` for anything
else. A failed check still fails the Job, so after five tries it also surfaces
as `ExpandFailed`.

### The contract loop and cell0

`online_data_migrations` answers 1 for "there is more to do" and 0 for "nothing
left", one bounded batch of 1000 rows at a time. The contract script loops until
a batch answers 0, and anything above 1 exits with its own code.

The loop runs twice. `online_data_migrations` works on the one cell database
`[database] connection` names and, unlike `db sync`, does not fan out to cell0.
cell0 holds the instances that failed scheduling, which `nova-api` reads on every
instance list, so a row written under an older release that never got its
backfill breaks the listing once a later release drops the code that reads the
old format. The second pass runs with `OS_DATABASE__CONNECTION` rewritten to the
cell0 schema: the running connection with `_cell0` appended to the schema, and
the query, TLS parameters included, kept. That is the same derivation cell0 is
provisioned with; see [Nova Cells](./nova-cells.md).

### The flip gate

The `RollingUpdate` to `Contracting` flip waits on every rendered role, not on
the API alone. The readiness the shared Deployment helper reports is
surge-tolerant: under `MaxSurge=1`/`MaxUnavailable=0` it turns true as soon as
the first new-image pod is Ready while old-image pods still serve. The contract
phase would then run data migrations those old pods have no code for, and nova
spreads that code over five processes: the conductor writes the rows the API
reads, so a lagging conductor is as dangerous as a lagging API.

The API step therefore reads the conductor, scheduler and metadata Deployments
back, plus the console proxy while it is enabled, and flips the phase only once
each has every replica updated to the image `spec.image` names, ready and
available. A role whose Deployment does not exist yet counts as not rolled out.
The flip emits `DeploymentRolloutComplete`.

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

The upgrade emits these events on the Nova CR. They are documented with their
example messages in [Controller Events](./nova-events.md#upgrade).

| Type | Reason | Trigger |
| --- | --- | --- |
| Normal | `UpgradeInitiated` | An accepted release bump starts the upgrade |
| Normal | `ExpandComplete` | The expand phase Job succeeded |
| Normal | `UpgradeCheckCompleted` | The upgrade check exited 0, or its exit code could not be read off the pod |
| Warning | `UpgradeCheckWarnings` | The same check exited with anything but 0; exit 2 and above also fail the phase Job |
| Normal | `MigrateComplete` | The migrate phase Job succeeded |
| Normal | `DeploymentRolloutComplete` | Every role finished rolling out the new image; the phase flips to Contracting |
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

1. Deletes the `{name}-db-expand`, `{name}-db-migrate`, and `{name}-db-contract`
   Jobs (background propagation removes their Pods too).
2. Clears `status.upgradePhase` and `status.targetRelease`.
3. Emits a Normal `UpgradeAborted` event.
4. Requeues, so the next reconcile takes the steady-state `{name}-db-sync` path
   and restores `DatabaseReady` against the installed release.

```bash
# Abort an in-flight upgrade by reverting both fields to the installed release.
# Digest-pinned image: drop the "image" key; tag and digest are mutually
# exclusive, and the whole patch is rejected if both are set.
kubectl patch nova <name> --type=merge \
  -p '{"spec":{"openStackRelease":"<installed-release>","image":{"tag":"<installed-release>"}}}'
```

Revert `spec.image` in the same patch. The abort trigger itself needs only the
release field: the lockstep guard is skipped while an upgrade is active and
`spec.openStackRelease` has been reverted, which keeps a wedged upgrade
recoverable. But the abort clears `status.upgradePhase`, and with the phase gone
the next reconcile is an ordinary one. The guard runs unconditionally there, and
a `spec.image.tag` still naming the target release parks the CR at
`DatabaseReady=False` under `ImageReleaseMismatch` instead of letting the
steady-state sync run. Reverting the release alone therefore triggers the abort
and then stalls on it.

A digest-pinned image, or a tag that does not parse as a release, carries no
release string for the guard to compare, so for those the release field is the
whole patch. The Deployments keep running the target image until that digest is
rolled back too.

The recipe is for a Nova you own. A Nova a ControlPlane projects has no abort
path: the ControlPlane's webhook rejects any `spec.openStackRelease` downgrade
(see [ControlPlane CRD](../c5c3/controlplane-crd.md#controlplanespec)), and the
plane re-asserts both fields on its child on every reconcile, so a patch applied
to the child directly is taken back. On a ControlPlane an upgrade that stalls is
repaired forward, by fixing what its failing phase reports.

::: warning Abort is only safe before the contract phase
Expand and migrate are additive: they add schema elements and read the database
without dropping anything the installed release still needs, so the pre-contract
schemas are a superset both releases run against. Aborting during `Expanding`,
`Migrating`, or `RollingUpdate` is therefore safe. The contract phase backfills
rows for the new code; aborting during `Contracting` can leave the installed
release reading data it did not write. The operator clears the upgrade state
from any phase, so validate both databases before relying on a Contracting-phase
abort. Once contract completes, reverting the release is a downgrade, which the
operator rejects.
:::

---

## Image and Release Lockstep

`spec.image` and `spec.openStackRelease` are separate fields so digest pinning
stays possible: the release drives tracking and upgrade detection, the phase
Jobs and the five Deployments run the image. The operator's contract is that the
two are bumped together, and for a tag-pinned image the reconciler enforces it.
When `spec.image.tag` parses as an OpenStack release that differs from
`spec.openStackRelease`, `DatabaseReady` goes `False` under
`ImageReleaseMismatch`, and neither the upgrade nor the steady-state sync
advances until the image is bumped. The patch suffix is ignored, so a patched
build tagged `2026.1-p1` still matches release `2026.1`.

Without the guard, a lone release bump would run the old `nova-manage` binary
against schemas already at its own head. The Jobs would exit 0 as no-ops and the
flow would promote `installedRelease` to a release the pods neither run nor
migrated to. A digest-pinned image, or a tag that does not parse as a release,
carries no comparable release string and is trusted to match the declared
`spec.openStackRelease`.

The guard is skipped on one path: while an upgrade is active and
`spec.openStackRelease` has been reverted to `installedRelease`. That is the
abort trigger, and it has to stay reachable while the two fields disagree.

---

## Fresh Installs and Patch Bumps

A fresh install has an empty `status.installedRelease`. It runs the single
`{name}-db-sync` Job, which migrates both schemas and maps cell0 and `cell1` in
one pass (the eight commands under
[The cells sequence](./nova-reconciler.md#the-cells-sequence)), and sets
`installedRelease` to `spec.openStackRelease` on success. No phase is entered and
no expand, migrate, or contract Job is created.

A change that keeps `spec.openStackRelease` the same, such as bumping the image
to a patch build, is not an upgrade either. The pod-spec-hash gate re-runs the
`{name}-db-sync` Job on the new image, both `sync` commands apply any pending
migrations in one pass, the cell guard finds the existing cells and maps nothing
twice, and the Deployments roll without the phase machine.
