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
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

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

Set the Keystone replica count on the `ControlPlane` CR; the operator projects it
onto the `controlplane-keystone` child and Kubernetes handles the rollout.

```bash
kubectl patch controlplane controlplane -n openstack \
  --type merge \
  -p '{"spec":{"services":{"keystone":{"replicas":5}}}}'
```

Watch the rollout on the projected child:

```bash
kubectl rollout status deploy/controlplane-keystone -n openstack
```

Scale down the same way. The keystone-operator maintains a `PodDisruptionBudget`
named `controlplane-keystone`, sized from the child's replica count: at
`replicas > 1` it sets `minAvailable=1` so a voluntary disruption never drains
the last healthy pod; at `replicas == 1` it sets `maxUnavailable=1` instead,
deliberately allowing eviction so a node drain cannot deadlock on a
single-replica child.

::: tip Load-driven autoscaling is standalone-only
The `ControlPlane` CRD does not expose `spec.autoscaling`, so on a ControlPlane
deployment there is no HPA knob — scale by setting
`spec.services.keystone.replicas`. Load-driven autoscaling with a
`HorizontalPodAutoscaler` is available only on a standalone Keystone CR — see
[Advanced Configuration — Autoscaling (HPA)](./advanced-configuration.md#autoscaling-hpa).
:::

---

## Upgrade the OpenStack release

Change `spec.openStackRelease` on the `ControlPlane` CR to a newer release. The
operator projects the new image tag (`ghcr.io/c5c3/keystone:<release>`) onto the
`controlplane-keystone` child, which triggers the keystone-operator's
expand-migrate-contract pipeline. The API stays available throughout — old and
new schemas coexist while data is migrated.

Before you patch, make the target-release images node-local. The devstack
pre-loads only the `2025.2` Keystone image, and the projected Horizon child
follows `spec.openStackRelease` too — so pull and `kind load` both the Keystone
and Horizon images for the target release, or the rollout stalls on an image
pull:

```bash
docker pull ghcr.io/c5c3/keystone:2026.1
kind load docker-image ghcr.io/c5c3/keystone:2026.1 --name forge

docker pull ghcr.io/c5c3/horizon:2026.1
kind load docker-image ghcr.io/c5c3/horizon:2026.1 --name forge
```

```bash
kubectl patch controlplane controlplane -n openstack \
  --type merge \
  -p '{"spec":{"openStackRelease":"2026.1"}}'
```

The child image tag is derived from `spec.openStackRelease` **unless**
`spec.services.keystone.image` overrides the whole image reference. An image
override pins the tag, so it must be dropped before a release upgrade takes
effect; likewise a patch-suffix build such as `2025.2-p1` is not a valid
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
| `Expanding` | Runs `db_sync --expand` with the new image — adds columns/tables without dropping anything |
| `Migrating` | Runs `db_sync --migrate` — copies/transforms data into new schema elements |
| `RollingUpdate` | Updates the Deployment to the new image and waits for rollout |
| `Contracting` | Runs `db_sync --contract` — drops old columns/tables that are no longer read |

When all four complete successfully:

- The child's `.status.installedRelease` is set to the new tag
- `.status.targetRelease` and `.status.upgradePhase` are cleared
- The ControlPlane's `.status.services[?(@.name=='keystone')].release` reports the new release
- A `UpgradeComplete` event is emitted on the child

::: warning Upgrade constraints
Only **sequential** upgrades are supported: `2025.1 → 2025.2`, `2025.2 → 2026.1`,
`2026.1 → 2026.2`. A **downgrade** is rejected at ControlPlane admission with
`openStackRelease downgrade from "…" to "…" is not permitted; Keystone DB
migrations are not reversible`. A **skip-level** jump (e.g. `2024.2 → 2026.1`) is
admitted at the ControlPlane but surfaces as an `UpgradePathInvalid` Warning
event on the `controlplane-keystone` child, which the keystone-operator refuses
to run.

Full contract in [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md).
:::

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
for the cron job to fire — useful after a suspected key compromise.

On the ControlPlane path the schedule is set through
`spec.services.keystone.rotationInterval` — a duration (e.g. `168h`) the operator
converts to a cron expression and projects onto the child's
`spec.fernet.rotationSchedule` and `spec.credentialKeys.rotationSchedule`. The
`ControlPlane` CRD does not expose the `suspend` or `maxActiveKeys` knobs; pausing
scheduled rotation or tuning the overlap window is standalone-only (see the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section).

Rotation uses a split staging→production path: the CronJob writes the new key set
to a *staging* Secret, and the operator validates it and applies it to the
production `controlplane-keystone-fernet-keys` Secret on its next reconcile. So
the right signal that a manual rotation landed is the operator's event on the
child, not the production Secret's contents immediately after the Job finishes.
Creating a Job from the CronJob is not a CR edit, so it is valid against the
operator-owned child.

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
  child's `maxActiveKeys` is exceeded — tokens issued before rotation remain
  valid through the overlap window.
- **No Deployment rollout happens.** Running pods pick up the new keys via the
  in-place Secret projection (~60s) — their UIDs stay unchanged.
- Credential keys rotate the same way and are **always managed**. Swap `fernet` →
  `credential` in the CronJob name:

  ```bash
  kubectl -n openstack create job \
    --from=cronjob/controlplane-keystone-credential-rotate \
    controlplane-keystone-credential-rotate-manual-$(date +%s)
  ```

For the full staging-aware verification flow, the operator's validation contract, and
recovery from a rejected rotation (`RotationRejected`), see
[Rotate Keystone Fernet and Credential Keys](./keystone-key-rotation.md).

::: tip Cleanup
Manual rotation Jobs are not garbage-collected automatically and accumulate if you run
them often — delete them after verification:

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

For load-driven scaling use `spec.autoscaling` instead — see
[Advanced Configuration — Autoscaling (HPA)](./advanced-configuration.md#autoscaling-hpa).

**Upgrade** by patching `spec.image.tag`:

```bash
kubectl patch keystone keystone -n openstack \
  --type merge \
  -p '{"spec":{"image":{"tag":"2026.1"}}}'
```

The same sequential-only constraint, the four-phase pipeline, and the
forward-only recovery path apply. Make the target image node-local first, or the
upgrade stalls at its first phase that needs it — `Expanding`, which runs
`db_sync --expand` with the new image:

```bash
docker pull ghcr.io/c5c3/keystone:2026.1
kind load docker-image ghcr.io/c5c3/keystone:2026.1 --name forge
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

- [Observability & Diagnostics](./observability.md) — reading conditions, events, and status fields while operations run
- [Rotate Keystone Fernet and Credential Keys](./keystone-key-rotation.md) — the full staging→production rotation flow, validation contract, and recovery
- [Rotate the Keystone Admin Password](./keystone-admin-password-rotation.md) — manual admin-password rotation at the OpenBao source
- [Schedule Keystone Admin Password Rotation](./keystone-admin-password-scheduled-rotation.md) — CronJob-driven scheduled admin-password rotation
- [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md) — state machine, job names, retry behavior
- [Keystone Controller Events](../reference/keystone/keystone-events.md) — full event catalogue for upgrade, rotation, and scale events
- [Advanced Configuration](./advanced-configuration.md) — brownfield DB, autoscaling, network policy, and more

## Tested by

Scale, release upgrade, image upgrade, zero-downtime rollout, and manual Fernet
rotation are each asserted on the CI e2e kind cluster by these chainsaw suites:

```bash
chainsaw test --test-dir tests/e2e/keystone/release-upgrade
chainsaw test --test-dir tests/e2e/keystone/image-upgrade
chainsaw test --test-dir tests/e2e/keystone/rolling-update-zero-downtime
chainsaw test --test-dir tests/e2e/keystone/fernet-rotation
```
