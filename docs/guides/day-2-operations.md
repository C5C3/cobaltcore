---
title: Day 2 Operations
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Day 2 Operations

Three operational patterns you will use most often on a running Keystone CR: scaling,
upgrading the OpenStack release, and rotating Fernet keys.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (Extended)](../quick-start-extended.md)** devstack. Stand it up first:

```bash
kind create cluster --name forge --config hack/kind-config.yaml
make deploy-infra
```

Follow that tutorial through to its final **Verify the deployment** step, so a
Keystone CR named `keystone` is `Ready` in the `openstack` namespace. Every
resource name in the examples below is one that devstack produces.
:::

---

## Scale replicas

`spec.deployment.replicas` can be patched at any time; the operator updates the Deployment and the
rollout is handled by Kubernetes.

```bash
kubectl patch keystone keystone -n openstack \
  --type merge \
  -p '{"spec":{"replicas":5}}'
```

Watch the rollout:

```bash
kubectl rollout status deploy/keystone -n openstack
```

Scale down the same way. The operator maintains a `PodDisruptionBudget` sized from
`spec.deployment.replicas`: at `replicas > 1` it sets `minAvailable=1` so a voluntary disruption
never drains the last healthy pod; at `replicas == 1` it sets `maxUnavailable=1`
instead, deliberately allowing eviction so a node drain cannot deadlock on a
single-replica CR.

::: tip Prefer autoscaling?
For load-driven scaling use `spec.autoscaling` instead of hand-patching `spec.deployment.replicas`
— see [Advanced Configuration — Autoscaling (HPA)](./advanced-configuration.md#autoscaling-hpa).
The HPA then owns `spec.replicas` on the Deployment.
:::

---

## Image upgrade with UpgradePhase

Changing `spec.image.tag` to a newer OpenStack release triggers the operator's
expand-migrate-contract pipeline. The API stays available throughout — old and new
schemas coexist while data is migrated.

```bash
kubectl patch keystone keystone -n openstack \
  --type merge \
  -p '{"spec":{"image":{"tag":"2026.1"}}}'
```

Watch the four phases:

```bash
kubectl get keystone keystone -n openstack -w \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.upgradePhase,FROM:.status.installedRelease,TO:.status.targetRelease,READY:.status.conditions[?(@.type=='Ready')].status
```

Expected timeline:

| Phase | What the operator does |
|-------|------------------------|
| `Expanding` | Runs `db_sync --expand` with the new image — adds columns/tables without dropping anything |
| `Migrating` | Runs `db_sync --migrate` — copies/transforms data into new schema elements |
| `RollingUpdate` | Updates the Deployment to the new image and waits for rollout |
| `Contracting` | Runs `db_sync --contract` — drops old columns/tables that are no longer read |

When all four complete successfully:

- `.status.installedRelease` is set to the new tag
- `.status.targetRelease` and `.status.upgradePhase` are cleared
- A `UpgradeComplete` event is emitted

::: warning Upgrade constraints
Only **sequential** upgrades are supported: `2025.1 → 2025.2`, `2025.2 → 2026.1`,
`2026.1 → 2026.2`. Skip-level jumps (e.g. `2024.2 → 2026.1`) and downgrades are
rejected with `UpgradePathInvalid` or `DowngradeNotSupported` Warning events. Tags
that differ only in patch suffix (`2025.2` → `2025.2-p1`) use the plain `db_sync`
path — no upgrade pipeline.

Full contract in [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md).
:::

### Rolling back a bad upgrade

The upgrade pipeline is forward-only. If a new release is broken, the recovery path is
to restore the database from backup, then patch `spec.image.tag` back. There is no
`db_sync --downgrade`. Plan cut-overs around a maintenance window and a tested backup.

---

## Rotate Fernet keys manually

The operator ships a `CronJob` that rotates the Fernet keys on the schedule in
`spec.fernet.rotationSchedule` (default weekly). You can trigger a rotation immediately
without waiting for the cron job to fire — useful after a suspected key compromise.
The inverse also works: `spec.fernet.suspend: true` (and
`spec.credentialKeys.suspend: true`) pauses scheduled rotation during an
incident without deleting the CronJob.

Rotation uses a split staging→production path: the CronJob writes the new key set to a
*staging* Secret, and the operator validates it and applies it to the production
`keystone-fernet-keys` Secret on its next reconcile. So the right signal that a manual
rotation landed is the operator's event on the CR, not the production Secret's contents
immediately after the Job finishes.

```bash
# Trigger an on-demand rotation by creating a Job from the CronJob
kubectl -n openstack create job \
  --from=cronjob/keystone-fernet-rotate \
  keystone-fernet-rotate-manual-$(date +%s)
```

Confirm the operator applied the staged rotation:

```bash
kubectl -n openstack describe keystone keystone | grep FernetKeysRotated
```

### What to expect

- The production Secret now holds a new primary key. Older keys stay until
  `spec.fernet.maxActiveKeys` is exceeded — tokens issued before rotation remain valid
  through the overlap window.
- **No Deployment rollout happens.** Running pods pick up the new keys via the in-place
  Secret projection (~60s) — their UIDs stay unchanged.
- Credential keys rotate the same way and are **always managed** (independent of whether
  `spec.credentialKeys` is set, which only tunes the schedule). Swap `fernet` →
  `credential` in the CronJob name:

  ```bash
  kubectl -n openstack create job \
    --from=cronjob/keystone-credential-rotate \
    keystone-credential-rotate-manual-$(date +%s)
  ```

For the full staging-aware verification flow, the operator's validation contract, and
recovery from a rejected rotation (`RotationRejected`), see
[Rotate Keystone Fernet and Credential Keys](./keystone-key-rotation.md).

::: tip Cleanup
Manual rotation Jobs are not garbage-collected automatically and accumulate if you run
them often — delete them after verification:

```bash
kubectl -n openstack get jobs -o name \
  | grep -E '/keystone-(fernet|credential)-rotate-manual-' \
  | xargs -r kubectl -n openstack delete
```
:::

---

## Further reading

- [Observability & Diagnostics](./observability.md) — reading conditions, events, and status fields while operations run
- [Rotate Keystone Fernet and Credential Keys](./keystone-key-rotation.md) — the full staging→production rotation flow, validation contract, and recovery
- [Rotate the Keystone Admin Password](./keystone-admin-password-rotation.md) — manual admin-password rotation at the OpenBao source
- [Schedule Keystone Admin Password Rotation](./keystone-admin-password-scheduled-rotation.md) — CronJob-driven scheduled admin-password rotation
- [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md) — state machine, job names, retry behavior
- [Keystone Controller Events](../reference/keystone/keystone-events.md) — full event catalogue for upgrade, rotation, and scale events
- [Advanced Configuration](./advanced-configuration.md) — brownfield DB, autoscaling, network policy, and more
