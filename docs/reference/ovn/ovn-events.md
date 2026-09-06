---
title: OVN Controller Events
quadrant: operator
---

# OVN Controller Events

Reference documentation for the Kubernetes events the two OVN controllers emit.
They make a failed background Job visible through `kubectl describe ovncentral`
and `kubectl get events` without reading controller logs.

Events complement status conditions. A condition reports current state for
programmatic consumers; an event is a timestamped record of a transition, which
is what an operator reads after the fact and what an alerting rule matches on.
For the conditions themselves see [OVNCentral CRD](./ovn-central-crd.md#conditions)
and [OVNChassis CRD](./ovn-chassis-crd.md#conditions); for the pipelines that
set them, [Reconciler Architecture](./ovn-reconciler.md).

---

## Event Conventions

- Reason strings are stable PascalCase identifiers. They are part of the
  controllers' public API and will not change without a deprecation notice.
- Every event this operator emits is of type **Warning**. It records no Normal
  event: what the two controllers surface are failed background Jobs and a
  teardown that gave up on its target cluster.
- No event is emitted for an in-progress or polling state, which keeps a requeue
  cycle from producing event noise.
- An event on an `OVNCentral` (`involvedObject.kind: OVNCentral`) carries the
  recorder name `ovncentral-controller`, and one on an `OVNChassis` carries
  `ovnchassis-controller`. That is what the `reportingComponent` field selector
  matches. Both recorders are wired in `operators/ovn/main.go`.
- The API server deduplicates events by (involvedObject, reason, message,
  source). Repeated identical events increment a counter rather than creating
  new objects.

---

## Event Reasons Reference

### Backup

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `BackupJobFailed` | Warning | The newest terminal backup Job of an `OVNCentral` failed | `Backup Job ovn-basic-backup-29357280 failed; inspect its pod logs. The databases keep running, but there is no snapshot of what they hold now` |
| `BackupMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation onto the `OVNCentral` failed, so the backup metrics are deferred to the next reconcile | `Patching last-observed backup Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** `reconcileBackup` in `reconcile_backup.go`, both raised through the
shared `RecordJobTerminalState` in `internal/common/job/terminal.go`; the second
reason is the deferral reason that call passes.

> **Note:** `BackupJobFailed` is also the reason of the `BackupReady=False`
> condition, and the two fire on different schedules. The condition is set on
> every pass for as long as the failed Job stays the newest terminal one; the
> event rides the terminal-state helper, which dedupes on the Job UID through an
> annotation on the CR, so it fires once per failed run. Read the current state
> off the condition and the history off the events.

### Chassis maintenance

The maintenance step runs one Job per node and per kind: `apply` writes the
node's `external_ids`, `evacuate` moves gateway responsibilities off it, and
`chassis-del` removes its chassis row from the Southbound database. The Job name
carries the kind and an eight-character hash of the node name.

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `ChassisMaintenanceJobFailed` | Warning | A per-node maintenance Job reached a terminal failure | `The evacuate Job ovn-chassis-evacuate-1f3a9c2b for node worker-2 failed; inspect its pod logs` |
| `ChassisMaintenanceMetricEmissionDeferred` | Warning | Patching the last-observed Job UID annotation onto the `OVNChassis` failed | `Patching last-observed maintenance Job UID failed; metric emission deferred to the next reconcile: <error>` |

**Source:** `runMaintenanceJob` in `reconcile_maintenance.go`, through the same
`RecordJobTerminalState` helper.

A permanently failed maintenance Job is reported on the condition instead of
being returned as an error. Rerunning it under an unchanged key produces the same failure, so a
returned error would only put the CR on the workqueue's backoff.

### Finalization

| Reason | Type | Trigger Condition | Example Message |
| --- | --- | --- | --- |
| `RemoteChildrenAbandoned` | Warning | Deletion of a CR whose `spec.targetClusterRef` no longer resolves, once the abandon window has passed. The finalizer is released without touching what was written on that cluster | `Target cluster is no longer registered; releasing the remote-children finalizer without deleting the objects on it labelled as owned by this OVNCentral` |

**Source:** `SweepRemoteChildren` in `internal/common/multicluster/teardown.go`,
called from `reconcileDeleteRemoteChildren` in both controllers.

The trailing kind name is read from the owner's GVK, so the same event reads
`… owned by this OVNChassis` on the node-layer CR. Before the window expires the
deletion requeues instead, and the wait is reported with reason
`TargetClusterUnavailable` on the first condition of the CR's pipeline:
`TLSReady=False` on an `OVNCentral`, `CentralReady=False` on an `OVNChassis`.
Engagement of a target cluster is asynchronous, so an operator restart looks
like a deregistration until the provider has synced.

---

## Alerting Configuration

Use `kubectl get events --field-selector` to filter by reason:

```bash
# Watch for failed database snapshots
kubectl get events --field-selector reason=BackupJobFailed -w

# Watch for failed per-node maintenance Jobs
kubectl get events --field-selector reason=ChassisMaintenanceJobFailed -w

# Watch for children left behind on a deregistered target cluster
kubectl get events --field-selector reason=RemoteChildrenAbandoned -w

# Watch for all Warning events from either OVN controller
kubectl get events --field-selector type=Warning,reportingComponent=ovncentral-controller -w
kubectl get events --field-selector type=Warning,reportingComponent=ovnchassis-controller -w
```

A failed backup run also moves `ovn_operator_backup_total{result="failed"}`,
which is the signal to alert on when the event stream is not collected.

---

## Event Flow

```text
OVNCentralReconciler.Reconcile()
  │
  ├── deletion, target cluster unresolvable past the abandon window
  │     └─ Warning RemoteChildrenAbandoned  (finalizer released, children left behind)
  │
  └── reconcileBackup()
        ├─ newest terminal backup Job failed → Warning BackupJobFailed
        └─ Job-UID patch failed              → Warning BackupMetricEmissionDeferred

OVNChassisReconciler.Reconcile()
  │
  ├── deletion, target cluster unresolvable past the abandon window
  │     └─ Warning RemoteChildrenAbandoned
  │
  └── reconcileMaintenance()
        ├─ apply / evacuate / chassis-del Job failed → Warning ChassisMaintenanceJobFailed
        └─ Job-UID patch failed                      → Warning ChassisMaintenanceMetricEmissionDeferred
```
