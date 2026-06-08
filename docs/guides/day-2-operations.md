<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Day 2 Operations

Three operational patterns you will use most often on a running Keystone CR: scaling,
upgrading the OpenStack release, and rotating Fernet keys.

**Prerequisites:** A running Keystone CR from the [Quick Start (Extended)](../quick-start-extended.md) (Steps 1–9).
The examples assume the CR is named `keystone` in the `openstack` namespace.

---

## Scale replicas

`spec.replicas` can be patched at any time; the operator updates the Deployment and the
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

Scale down the same way. With a `PodDisruptionBudget` in place (created by the operator
based on `spec.replicas`), Kubernetes will not drain the last healthy pod during a
voluntary disruption.

::: tip Prefer autoscaling?
For load-driven scaling use `spec.autoscaling` instead of hand-patching `spec.replicas`
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

The operator ships a `CronJob` that rotates the Fernet Secret on the schedule in
`spec.fernet.rotationSchedule` (default weekly). You can trigger a rotation immediately
without waiting for the cron job to fire — useful after a suspected key compromise.

```bash
# Trigger an on-demand rotation by creating a Job from the CronJob
kubectl create job keystone-manual-fernet-rotate \
  --from=cronjob/keystone-fernet-rotate \
  -n openstack

kubectl wait --for=condition=complete \
  job/keystone-manual-fernet-rotate -n openstack --timeout=120s
```

Verify the Secret actually changed:

```bash
kubectl get secret keystone-fernet-keys -n openstack \
  -o jsonpath='{.data}' | sha256sum
```

### What to expect

- The Fernet key Secret now holds a new primary key. Older keys stay in the Secret
  until `spec.fernet.maxActiveKeys` is exceeded — tokens issued before rotation
  remain valid through the overlap window.
- **No Deployment rollout happens.** The API pods pick up the new keys via the
  Secret remount — their UIDs stay unchanged. Verify with:

  ```bash
  kubectl get pods -n openstack -l app.kubernetes.io/name=keystone \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.uid}{"\n"}{end}'
  ```
- The same pattern works for credential keys — if you have configured
  `spec.credentialKeys`, use a distinct Job name so both rotation recipes can run
  side-by-side without colliding:

  ```bash
  kubectl create job keystone-manual-credential-rotate \
    --from=cronjob/keystone-credential-rotate \
    -n openstack
  ```

::: tip Cleanup
Manual rotation Jobs accumulate if you run them often — delete after verification:

```bash
kubectl delete job keystone-manual-fernet-rotate keystone-manual-credential-rotate -n openstack --ignore-not-found
```
:::

---

## Further reading

- [Observability & Diagnostics](./observability.md) — reading conditions, events, and status fields while operations run
- [Rotate the Keystone Admin Password](./keystone-admin-password-rotation.md) — manual admin-password rotation at the OpenBao source
- [Schedule Keystone Admin Password Rotation](./keystone-admin-password-scheduled-rotation.md) — CronJob-driven scheduled admin-password rotation
- [Keystone Upgrade Flow](../reference/keystone/keystone-upgrade-flow.md) — state machine, job names, retry behavior
- [Keystone Controller Events](../reference/keystone/keystone-events.md) — full event catalogue for upgrade, rotation, and scale events
- [Advanced Configuration](./advanced-configuration.md) — brownfield DB, autoscaling, network policy, and more
