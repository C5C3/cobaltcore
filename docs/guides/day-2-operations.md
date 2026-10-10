---
title: Day 2 Operations
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Day 2 Operations

Three operational patterns you will use most often on a running control plane:
scaling, upgrading the OpenStack release, and rotating Fernet keys.

## Prerequisites

::: info Devstack
This guide is written against the
**[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand
it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-keystone` Keystone child is running. Every resource name in the
examples below is one that devstack produces.
:::

::: warning The Keystone child is operator-owned
On a ControlPlane deployment the `controlplane-keystone` Keystone CR is
**projected** by the c5c3-operator. It re-asserts the projected fields
(image, database, cache, replicas, federation, policy overrides, gateway) on
every reconcile, so a knob you set directly on the child is reverted. Set
operational knobs on the `ControlPlane` CR and let the operator project them
down. Where the `ControlPlane` CRD does not expose a knob, this guide points to
the [Standalone Keystone](#standalone-keystone-without-a-controlplane) section,
which drives a Keystone CR you own. See the
[ControlPlane Reconciler](../reference/c5c3/controlplane-reconciler.md) for the
full projection contract.
:::

---

## Scale replicas

Set the Keystone API replica count under `spec.sizing` on the `ControlPlane` CR;
the operator projects it onto the `controlplane-keystone` child and Kubernetes
handles the rollout.

```bash
kubectl patch controlplane controlplane -n openstack \
  --type merge \
  -p '{"spec":{"sizing":{"keystone":{"api":{"replicas":5}}}}}'
```

Watch the rollout on the projected child:

```bash
kubectl rollout status deploy/controlplane-keystone -n openstack
```

Scale down the same way. The keystone-operator maintains a `PodDisruptionBudget`
named `controlplane-keystone`, sized from the child's replica count. At
`replicas > 1` it sets `minAvailable=1` so a voluntary disruption never drains
the last healthy pod; at `replicas == 1` it sets `maxUnavailable=1` instead,
allowing eviction so a node drain cannot deadlock on a
single-replica child.

::: tip Load-driven autoscaling
`spec.sizing.keystone.api.autoscaling` projects a `HorizontalPodAutoscaler` onto
the Keystone child, which then scales between its bounds on load. The HPA needs
metrics-server, so no sizing profile sets it. See
[Advanced Configuration: Autoscaling (HPA)](./advanced-configuration.md#autoscaling-hpa).
:::

---

## Upgrade the OpenStack release

::: warning Back up the database first
Make a database backup before starting the upgrade. Recovery from a bad upgrade
requires restoring the database because the schema migrations are not
reversible.
:::

Before you patch, make the target-release images node-local. The devstack
preloads only the `2026.1` Keystone image, while all six enabled service children
follow `spec.openStackRelease`. Pull and load the target release images into kind,
or their rollouts stall on image pulls:

```bash
RELEASE=2026.2
for operator in barbican glance horizon keystone neutron placement; do
  podman pull "ghcr.io/c5c3/${operator}:${RELEASE}"
  kind load docker-image "ghcr.io/c5c3/${operator}:${RELEASE}" --name cobaltcore
done
```

Change `spec.openStackRelease` on the `ControlPlane` CR to the target release.
The operator projects the new image tags onto the service children and triggers
their release-specific upgrade flows. Keystone uses its
expand-migrate-contract pipeline. Its API stays available throughout because
old and new schemas coexist while data is migrated.

```bash
kubectl patch controlplane controlplane -n openstack \
  --type merge \
  -p '{"spec":{"openStackRelease":"2026.2"}}'
```

The child image tag is derived from `spec.openStackRelease` **unless**
`spec.services.keystone.image` overrides the whole image reference. An image
override pins the tag, so it must be dropped before a release upgrade takes
effect; likewise a patch-suffix build such as `2026.1-p1` is not a valid
`openStackRelease` value (the field only accepts the `YYYY.N` cadence) and is
delivered through the image override instead.

Watch the ControlPlane's driven release and the child's upgrade phases:

```bash
kubectl get controlplane controlplane -n openstack -w
```

```bash
kubectl get keystone controlplane-keystone -n openstack -w \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.upgradePhase,FROM:.status.installedRelease,TO:.status.targetRelease,READY:.status.conditions[?(@.type=="Ready")].status'
```

Expected timeline on the child:

| Phase | What the operator does |
|-------|------------------------|
| `Expanding` | Runs `db_sync --expand` with the new image, adding columns/tables without dropping anything |
| `Migrating` | Runs `db_sync --migrate`, copying or transforming data into new schema elements |
| `RollingUpdate` | Updates the Deployment to the new image and waits for rollout |
| `Contracting` | Runs `db_sync --contract`, dropping old columns/tables that are no longer read |

When all four complete successfully:

- The child's `.status.installedRelease` is set to the new tag
- `.status.targetRelease` and `.status.upgradePhase` are cleared
- The ControlPlane's `.status.services[?(@.name=='keystone')].release` reports the new release
- A `UpgradeComplete` event is emitted on the child

The figure shows the same four phases with their failure states. Its abort
arrows start at the child's own spec, which the ControlPlane owns here: lowering
`spec.openStackRelease` is rejected at admission, so
[Recovering from a bad upgrade](#recovering-from-a-bad-upgrade) describes the
way back on this path.

![The release upgrade as a state machine, in two panels. Phased upgrade, which Keystone, Glance, Cinder, Nova and Neutron share: a spec release one release ahead of installedRelease starts Expanding, and a release that does not parse, is older or skips a release is rejected with VersionParseError, DowngradeNotSupported or UpgradePathInvalid while the old image keeps running. The Database step moves the upgrade from Expanding to Migrating and on to RollingUpdate as each phase Job completes, the Deployment step moves it to Contracting once every replica runs the new image, and the Database step ends it when the contract Job completes and installedRelease becomes the target. A phase Job that used up its retries holds its phase as ExpandFailed, MigrateFailed or ContractFailed. A spec that changes to a third release holds the upgrade as UpgradeTargetChanged until it names the target again. Setting the spec back to installedRelease aborts from every phase: that is safe during Expanding, Migrating and RollingUpdate and unsafe during Contracting, where the old release would meet a contracted schema. Single pass, which Barbican and Placement run: one db-sync Job on the new image, the same rejections plus ImageReleaseMismatch, the failure state DBSyncFailed, no phases and no abort.](../diagrams/service-upgrade-phases.svg)

::: warning Upgrade constraints
Only **sequential** upgrades are supported. `2026.1 → 2026.2` is the one step
the release floor leaves today, and a `YYYY.2 → YYYY+1.1` step such as
`2026.2 → 2027.1` is sequential too. A **downgrade** is rejected at ControlPlane
admission with `openStackRelease downgrade from "…" to "…" is not permitted;
Keystone DB migrations are not reversible`. A **skip-level** jump (e.g.
`2026.1 → 2027.1`) is admitted at the ControlPlane but surfaces as an
`UpgradePathInvalid` Warning event on the `controlplane-keystone` child, which
the keystone-operator refuses to run.

Full contract in [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md).
:::

### Minimum supported release

This operator version supports OpenStack `2026.1` and later. Before you install
it, bring every `ControlPlane`, every service resource with
`spec.openStackRelease` (`Barbican`, `Cinder`, `Glance`, `Neutron`,
`NeutronMetadataAgent`, `Nova` and `Placement`), and every `Keystone` or
`Horizon` whose `spec.image.tag` names a release to `2026.1` or later. Run that
upgrade with the operator version you already have.

The validating webhooks reject a create, or a change of the release, below the
floor:

```text
spec.openStackRelease: Invalid value: "2025.2": must be 2026.1 or later: this operator version no longer supports OpenStack releases below 2026.1
```

An update that keeps a stored release below the floor is admitted with a
warning, so the resource stays editable:

```text
Warning: spec.openStackRelease "2025.2" is below 2026.1, the oldest OpenStack release this operator version supports; the unchanged value is admitted, but the operator renders the 2026.1 configuration for it. Set spec.openStackRelease to 2026.1 or later.
```

For `Keystone` and `Horizon` the field in both messages is `spec.image.tag`. A
digest-pinned image, or a tag that names no release, is not checked.

From this operator version on, the controllers render the `2026.1` launch and
paste layout for any release: Glance runs under uWSGI, and Barbican's paste file
carries the `request_id` filter. The Glance connection cap no longer accounts
for a `status.installedRelease` of `2025.2`. A `ControlPlane` left at `2025.2`
stays editable, but a service child it creates at that release is refused by
the child's webhook.

### Recovering from a bad upgrade

The upgrade pipeline is forward-only at both levels: the keystone-operator has no
`db_sync --downgrade`, and the ControlPlane webhook rejects lowering
`spec.openStackRelease`. There is therefore no in-place rollback. If a new release
is broken, recovery is to restore the database from backup and redeploy at the
restored release. Plan cut-overs around a maintenance window and a tested backup.

---

## Rotate Fernet keys manually

The keystone-operator ships a `CronJob` on the projected child that rotates the
Fernet keys on a schedule. You can trigger a rotation immediately without waiting
for the cron job to fire. This is useful after a suspected key compromise.

On the ControlPlane path the schedule is set through
`spec.services.keystone.rotationInterval`, a duration (e.g. `168h`) the operator
converts to a cron expression and projects onto the child's
`spec.fernet.rotationSchedule` and `spec.credentialKeys.rotationSchedule`. The
`ControlPlane` CRD does not expose the `suspend` or `maxActiveKeys` knobs; pausing
scheduled rotation or tuning the overlap window is standalone-only (see the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section).

Rotation uses a split staging-to-production path: the CronJob writes the new key set
to a *staging* Secret, and the operator validates it and applies it to the
production `controlplane-keystone-fernet-keys` Secret on its next reconcile. So
the right signal that a manual rotation landed is the operator's event on the
child, not the production Secret's contents immediately after the Job finishes.
Creating a Job from the CronJob is not a CR edit, so it is valid against the
operator-owned child.

A Job you create from the CronJob enters the figure at step 1. The
[reconciler reference](../reference/keystone/keystone-reconciler.md#key-rotation-rbac-split)
lists the steps.

![Staged rotation of Fernet keys in six numbered steps. The CronJob mounts the production Secret read-only, runs keystone-manage fernet_rotate on a copy and patches the result onto a staging Secret, the only Secret its Role may write. The keystone-operator validates the staged keys, replaces the data of the production Secret and deletes the staging Secret. A rejected payload raises the event RotationRejected, the staging data is cleared and the production Secret stays as it was. The kubelet projects the new keys into the running Keystone pods without a rollout, and a PushSecret copies them to OpenBao as a backup. Credential keys follow the same path with credential_rotate and credential_migrate.](../diagrams/secrets-rotation-keys.svg)

```bash
# Trigger an on-demand rotation by creating a Job from the CronJob
kubectl -n openstack create job \
  --from=cronjob/controlplane-keystone-fernet-rotate \
  controlplane-keystone-fernet-rotate-manual-$(date +%s)
```

Confirm the operator applied the staged rotation:

```bash
kubectl -n openstack describe keystone controlplane-keystone | grep FernetKeysRotated
```

### What to expect

- The production Secret now holds a new primary key. Older keys stay until the
  child's `maxActiveKeys` is exceeded, so tokens issued before rotation remain
  valid through the overlap window.
- **No Deployment rollout happens.** Running pods pick up the new keys via the
  in-place Secret projection (~60s). Their UIDs stay unchanged.
- Credential keys rotate the same way and are **always managed**. Swap `fernet` for
  `credential` in the CronJob name:

  ```bash
  kubectl -n openstack create job \
    --from=cronjob/controlplane-keystone-credential-rotate \
    controlplane-keystone-credential-rotate-manual-$(date +%s)
  ```

For the full staging-aware verification flow, the operator's validation contract, and
recovery from a rejected rotation (`RotationRejected`), see
[Rotate Keystone Fernet and Credential Keys](./keystone/keystone-key-rotation.md).

::: tip Cleanup
Manual rotation Jobs are not garbage-collected automatically and accumulate if you run
them often. Delete them after verification:

```bash
kubectl -n openstack get jobs -o name \
  | grep -E '/controlplane-keystone-(fernet|credential)-rotate-manual-' \
  | xargs -r kubectl -n openstack delete
```
:::

---

## Standalone Keystone, without a ControlPlane

On the [Quick Start](../quick-start.md) / [Quick Start (Extended)](../quick-start-extended.md)
devstacks a standalone Keystone CR named `keystone` runs with no ControlPlane
projecting it. Drive these operations directly on that CR.

**Scale** by patching `spec.deployment.replicas`:

```bash
kubectl patch keystone keystone -n openstack \
  --type merge \
  -p '{"spec":{"deployment":{"replicas":5}}}'

kubectl rollout status deploy/keystone -n openstack
```

For load-driven scaling use `spec.autoscaling` instead. See
[Advanced Configuration: Autoscaling (HPA)](./advanced-configuration.md#autoscaling-hpa).

**Upgrade** by patching `spec.image.tag`:

```bash
kubectl patch keystone keystone -n openstack \
  --type merge \
  -p '{"spec":{"image":{"tag":"2026.1"}}}'
```

The same sequential-only constraint, the four-phase pipeline, and the
forward-only recovery path apply. Make the target image node-local first, or the
upgrade stalls at its first phase that needs it, `Expanding`, which runs
`db_sync --expand` with the new image:

```bash
docker pull ghcr.io/c5c3/keystone:2026.1
kind load docker-image ghcr.io/c5c3/keystone:2026.1 --name cobaltcore
```

On the [Quick Start (Extended)](../quick-start-extended.md) local-build path,
rebuild the service image with `RELEASE=2026.1` per that tutorial's Step 6
instead of pulling it.

**Rotate Fernet keys** against the `keystone-fernet-rotate` /
`keystone-credential-rotate` CronJobs, and tune or pause scheduled rotation with
the standalone-only `spec.fernet` fields:

```bash
kubectl -n openstack create job \
  --from=cronjob/keystone-fernet-rotate \
  keystone-fernet-rotate-manual-$(date +%s)
```

`spec.fernet.rotationSchedule` (a cron string, default weekly) sets the cadence,
`spec.fernet.suspend: true` (and `spec.credentialKeys.suspend: true`) pauses
scheduled rotation during an incident without deleting the CronJob, and
`spec.fernet.maxActiveKeys` bounds the overlap window.

---

## Further reading

- [Observability & Diagnostics](./observability.md): reading conditions, events, and status fields while operations run
- [Rotate Keystone Fernet and Credential Keys](./keystone/keystone-key-rotation.md): the full staging-to-production rotation flow, validation contract, and recovery
- [Rotate the Keystone Admin Password](./keystone/keystone-admin-password-rotation.md): manual admin-password rotation at the OpenBao source
- [Schedule Keystone Admin Password Rotation](./keystone/keystone-admin-password-scheduled-rotation.md): CronJob-driven scheduled admin-password rotation
- [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md): state machine, job names, retry behavior
- [Keystone Controller Events](../reference/keystone/keystone-events.md): full event catalogue for upgrade, rotation, and scale events
- [Advanced Configuration](./advanced-configuration.md): brownfield DB, autoscaling, network policy, and more

## Tested by

Scale, release upgrade, image upgrade, zero-downtime rollout, manual Fernet
rotation, and the release floor are each asserted on the CI e2e kind cluster by
these chainsaw suites:

```bash
chainsaw test --test-dir tests/e2e/keystone/release-upgrade
chainsaw test --test-dir tests/e2e/keystone/image-upgrade
chainsaw test --test-dir tests/e2e/keystone/rolling-update-zero-downtime
chainsaw test --test-dir tests/e2e/keystone/fernet-rotation
chainsaw test --test-dir tests/e2e/c5c3/invalid-cr
```
