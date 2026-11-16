---
title: Restore an OVN Database Snapshot
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Restore an OVN Database Snapshot

Every `OVNCentral` gets a backup CronJob. Raft replication covers the loss of a
minority of the members; it does nothing for an operator error applied to all of
them, and these snapshots are the way back from that. Restoring one is a manual
procedure: the operator writes the snapshots and prunes them, and replays none
of them on its own.

This guide takes a snapshot of the `controlplane-ovn` central on demand as the
way back from a wrong replay, picks the snapshot to go back to off the volume,
and replays its Northbound half into the running database with `ovsdb-client`.

The figure shows the backup CronJob beside the two databases, and northd, which
compiles a replayed Northbound model into the Southbound database again.

![What one OVNCentral runs and who talks to it. The Northbound database {central}-nb and the Southbound database {central}-sb are Raft clusters of three members each by default. northd reads the Northbound database and writes the flows into the Southbound database. A relay in front of the Southbound database exists only with spec.relay. A CronJob backs both databases up, and cert-manager issues a server certificate per database, one for the relay and one client certificate from the ClusterIssuer that spec.tls.issuerRef names. On the Neutron side the API with its ML2/OVN driver, the maintenance worker and the periodic workers hold both connections, and the optional CronJob {neutron}-ovn-db-sync compares the Northbound database with the Neutron database or rewrites it. On every chassis node ovn-controller registers the chassis in the Southbound database and reads its flows there, and the metadata agent watches the Southbound database and writes its Chassis_Private row. Two Jobs of an OVNChassis write as well: the evacuation removes gateway bindings from the Northbound database, and the chassis deletion removes the chassis row from the Southbound database. Only ovn-controller is pointed at the relay; every other client dials the database members.](../../diagrams/compute-ovn-control-plane.svg)

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 7, so the `OVNCentral` `controlplane-ovn` is Ready in the `openstack`
namespace and its logical model holds something worth snapshotting.
:::

If you intend to run the drift check at the end, ensure the `ControlPlane`
network service is Ready. The snapshot replay itself requires only the central.

## Where the snapshots live

The CronJob and the volume share one name, `controlplane-ovn-backup`:

```bash
kubectl get cronjob,pvc controlplane-ovn-backup -n openstack
```

The volume is mounted at `/backup` in every run. Each successful run writes two files,
`nb-<timestamp>.backup` and `sb-<timestamp>.backup`, where the timestamp is
`date -u +%Y%m%dT%H%M%SZ`, for example `nb-20260906T020000Z.backup`. Unless
`spec.backup` says otherwise, the schedule is `0 2 * * *` and the retention
setting is 14 days by default; both resolve at reconcile time, and a nil
`spec.backup` behaves like an empty one. The script applies retention with
GNU `find -mtime +N`, which rounds file age down to full 24-hour periods before
matching values greater than `N`. With the default `N=14`, a snapshot becomes
eligible for pruning at 15 days old.

## Steps

### 1. Take a snapshot of the state you are about to replace

The replay in Step 3 replaces the Northbound database with the snapshot
contents. Take a snapshot of the current state before replaying an older file,
and record its name as recovery from an incorrect replay.

Creating a Job from the CronJob runs the same pod template the schedule runs,
so the on-demand snapshot is written by the code path the nightly one uses,
including its retention sweep:

::: warning The on-demand run prunes the volume as well
After both database snapshots succeed, this Job deletes `*.backup` files whose
whole-day age exceeds `spec.backup.retentionDays`. With the default value of 14,
files become eligible at 15 days old. This on-demand run can therefore remove
an older candidate before you list the volume.

If the snapshot you need is near the retention cutoff, first list the volume
with the pod described in Step 2 and copy the candidate outside the sweep's
`*.backup` pattern. In that pod's `--overrides`, use this command instead of
`["ls", "-l", "/backup"]`:

```json
"command": ["cp", "/backup/nb-20260823T020000Z.backup", "/backup/hold-nb-20260823T020000Z"]
```

Then run the listing pod again with `["ls", "-l", "/backup"]` and confirm the
`hold-*` file exists before taking the snapshot. Step 3 can replay that held
path. Delete it after the restore is complete.
:::

```bash
set -euo pipefail
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$RANDOM"
JOB="controlplane-ovn-backup-manual-$RUN_ID"
kubectl create job "$JOB" \
  --from=cronjob/controlplane-ovn-backup -n openstack
if ! kubectl wait --for=condition=complete -n openstack \
  "job/$JOB" --timeout=5m; then
  kubectl logs -n openstack "job/$JOB" --all-containers=true || true
  kubectl describe job "$JOB" -n openstack || true
  exit 1
fi
kubectl logs -n openstack "job/$JOB" --all-containers=true
```

The backup script prints no success message; the completed Job is the success
signal. It writes each snapshot to a `.tmp` file and renames it only after
`ovsdb-client` exits zero with a non-empty result, so a run that dies mid-copy
leaves no completed snapshot for that database. A full volume is why the
emptiness check exists: `ovsdb-client` can exit zero having written nothing,
and an empty file restores no database. After both snapshots succeed, retention
prunes expired `*.backup` files, sweeps `*.backup.tmp` leftovers older than a
day, and removes any zero-byte `*.backup` file left by an earlier run.

A failed run reports itself twice, as a `BackupJobFailed` event and as
`ovn_operator_backup_total{result="failed"}`:

```bash
kubectl get events -n openstack --field-selector reason=BackupJobFailed
```

### 2. Pick the file to replay

Nothing mounts the volume between runs, so list it from a short-lived pod that
borrows the OVN image from the running Northbound StatefulSet:

```bash
set -euo pipefail
IMG="$(kubectl get sts controlplane-ovn-nb -n openstack \
  -o jsonpath='{.spec.template.spec.containers[0].image}')"
test -n "$IMG"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$RANDOM"
POD="ovn-backup-ls-$RUN_ID"

OVERRIDES="$(cat <<JSON
{"spec":{
  "securityContext": {"fsGroup": 42424},
  "containers": [{"name": "ovn-backup-ls", "image": "${IMG}",
    "command": ["ls", "-l", "/backup"],
    "volumeMounts": [{"name": "backup", "mountPath": "/backup"}]}],
  "volumes": [{"name": "backup",
    "persistentVolumeClaim": {"claimName": "controlplane-ovn-backup"}}]
}}
JSON
)"

kubectl run "$POD" -n openstack --image="$IMG" --restart=Never \
  --overrides="$OVERRIDES"

if ! kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
  "pod/$POD" -n openstack --timeout=2m; then
  kubectl logs "$POD" -n openstack || true
  kubectl describe pod "$POD" -n openstack || true
  exit 1
fi
kubectl logs "$POD" -n openstack
kubectl delete pod "$POD" -n openstack
```

The pod-level `fsGroup` gives the image's UID 42424 access to the snapshot
volume. The wait's failure path prints the pod log and description, exits
nonzero, and leaves the pod available for inspection. The backup script removes
an incomplete temporary file; do not restore any zero-byte file you find.

Choose an `nb-*.backup` file or a protected `hold-*` copy and keep its full path
for Step 3. The newest file is rarely the one you want: the Step 1 snapshot is
the newest when written, and a nightly run after the damage may capture the
damaged model. Read the timestamps against the last moment the logical model
was the one you want back.

### 3. Replay the Northbound snapshot

The restore runs as a Job of your own, from the same image, mounting the
snapshot volume and the client keypair the central publishes. `SNAPSHOT` names
the file you picked in Step 2; the value below is an example, and replaying a
different file means editing that one line:

```bash
set -euo pipefail
IMG="$(kubectl get sts controlplane-ovn-nb -n openstack \
  -o jsonpath='{.spec.template.spec.containers[0].image}')"
NB="$(kubectl get ovncentral controlplane-ovn -n openstack \
  -o jsonpath='{.status.northbound.internalDbAddress}')"
test -n "$IMG"
test -n "$NB"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$RANDOM"
RESTORE_JOB="controlplane-ovn-restore-$RUN_ID"

cat <<JOB | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${RESTORE_JOB}
  namespace: openstack
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 300
  template:
    spec:
      restartPolicy: Never
      securityContext:
        fsGroup: 42424
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: restore
        image: ${IMG}
        securityContext:
          allowPrivilegeEscalation: false
          runAsNonRoot: true
          runAsUser: 42424
          runAsGroup: 42424
          readOnlyRootFilesystem: true
          capabilities:
            drop: [ALL]
          seccompProfile:
            type: RuntimeDefault
        env:
        - name: NB_ADDR
          value: "${NB}"
        - name: SNAPSHOT
          value: "/backup/nb-20260906T020000Z.backup"
        command:
        - sh
        - -ec
        - |
          test -s "\$SNAPSHOT"
          echo "restoring \$SNAPSHOT"
          ovsdb-client -p /certs/tls.key -c /certs/tls.crt -C /certs/ca.crt restore "\$NB_ADDR" OVN_Northbound < "\$SNAPSHOT"
        volumeMounts:
        - name: backup
          mountPath: /backup
        - name: certs
          mountPath: /certs
          readOnly: true
        - name: tmp
          mountPath: /tmp
      volumes:
      - name: backup
        persistentVolumeClaim:
          claimName: controlplane-ovn-backup
      - name: certs
        secret:
          secretName: controlplane-ovn-client
      - name: tmp
        emptyDir: {}
JOB

if ! kubectl wait --for=condition=complete -n openstack \
  "job/$RESTORE_JOB" --timeout=5m; then
  kubectl logs -n openstack "job/$RESTORE_JOB" --all-containers=true || true
  kubectl describe job "$RESTORE_JOB" -n openstack || true
  exit 1
fi
kubectl logs -n openstack "job/$RESTORE_JOB" --all-containers=true
```

The pod-level `fsGroup: 42424` is what
hands a freshly provisioned volume to the group the container runs as, and the
container context is the Restricted one the backup run itself carries; without
both, the replay cannot read the files the backup wrote. Redirect the snapshot
to a regular file because `ovsdb-client restore` needs seekable stdin.
`--force` proceeds despite a schema mismatch, which can fail or produce
surprising results
([`ovsdb-client(1)`](https://www.openvswitch.org/support/dist-docs/ovsdb-client.1.html)).
Do not use it just because the database is clustered.
The image comes from the running Northbound StatefulSet, but an older snapshot
may use a different schema. If restore reports a schema mismatch, stop and
check the linked OVSDB manual's schema-conversion guidance for the snapshot and
running version. Do not retry with `--force` as a workaround.
`status.northbound.internalDbAddress` is a comma-separated list with one
`ssl:<ip>:6641` address per Raft member. `ovsdb-client` accepts that cluster
address form and tries the members until it finds the leader, so pass the full
value in `$NB_ADDR`.

The replay **replaces** the database with the snapshot contents. Snapshot rows
return under fresh UUIDs, so references to old row UUIDs become invalid. Every
Northbound row created since the snapshot is deleted. A network a tenant
created after the backup ran is gone from the logical model by the time the Job
reports complete. The Step 1 snapshot brings it back. northd recompiles the
Southbound database from the restored Northbound model, and each chassis
re-registers itself there. The `sb-*.backup` file is useful for forensic
analysis; a routine repair goes through the Northbound database.

### 4. Check Neutron against the restored model

Neutron's own database was not part of the snapshot, so after a replay the two
disagree wherever the model moved on since the backup. Run the drift check in
its reporting mode before changing anything:
[Repair OVN Drift with db-sync](../neutron/repair-ovn-drift-with-db-sync.md)
covers the `log` mode and what to do with what it finds.

Its `repair` mode rebuilds from Neutron what OVN lacks, so it recovers nothing
that only ever existed in the Northbound database. Those rows come back from a
snapshot or not at all, which is what the Step 1 file is for.

## See also

- [OVNCentral CRD](../../reference/ovn/ovn-central-crd.md): the `spec.backup`
  block, the S3 shifter, and the sub-resource names.
- [OVN Controller Events](../../reference/ovn/ovn-events.md): `BackupJobFailed`
  and the other events the central controller emits.
- [Enable the OVN Operator Metrics Endpoint](./enable-ovn-operator-metrics.md):
  the backup counter and duration histogram to alert on.

## Tested by

The suite below asserts a full snapshot round trip on the CI e2e kind cluster:
it seeds a logical switch, snapshots it with the CronJob's pod template, removes
it, replays the snapshot, and finds the switch again. It also creates a switch
the snapshot predates and asserts that replay removes it, which is the
replacement half of the paragraph above. It runs the same `ovsdb-client`
invocation this guide prints against a one-member clustered Northbound database.
A production OVNCentral can have multiple members, and `ovsdb-client` accepts
the full comma-separated address list for that cluster:

```bash
chainsaw test --test-dir tests/e2e/ovn/central-backup-restore
```

::: details The OVNCentral the suite applies
The suite shares the `openstack` namespace with every other OVN suite, so its CR
is isolation-named (`ovn-restore`) where the walkthrough above uses the
`controlplane-ovn` name the devstack produces. Its backup is suspended, so the
only snapshot on the volume is the one the suite writes by hand.

<<< ../../../tests/e2e/ovn/central-backup-restore/00-ovncentral-cr.yaml#ovncentral-cr
:::
