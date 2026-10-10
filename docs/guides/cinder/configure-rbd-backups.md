---
title: Configure RBD Volume Backups
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Configure RBD Volume Backups

An RBD backup target writes volume backups as RBD images into a Ceph pool. A
backup of an RBD volume is then the difference between two snapshots rather
than a full copy. This guide swaps the lab's NFS backup target for an RBD one,
backs up an RBD volume twice and an NFS volume once, restores each, rotates the
backup key, and puts the NFS target back. The operator creates no pool and no
Ceph user; it renders the Ceph backup driver's section, the Ceph client
configuration and the keyring into the backup service, and projects the
keyring of every RBD volume backend beside them, because a backup reads its
source volume with that backend's own user.

For the full field reference, see the
[CinderBackupBackend CRD API Reference](../../reference/cinder/cinder-backup-backend-crd.md),
in particular [RBDBackupBackendSpec](../../reference/cinder/cinder-backup-backend-crd.md#rbdbackupbackendspec),
[The key Secret](../../reference/cinder/cinder-backup-backend-crd.md#the-key-secret)
and [Ceph files in the backup pod](../../reference/cinder/cinder-backup-backend-crd.md#ceph-files-in-the-backup-pod).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (metal-stack)](../../quick-start-metal-stack.md)** devstack. Stand it up first:

```bash
EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_CEPH=true make deploy-infra
```

Follow that tutorial through Part 1, so a `ControlPlane` CR named
`controlplane` is `Ready` in the `openstack` namespace with its projected
`controlplane-cinder` Cinder child running, and the lab Ceph reports
`HEALTH_OK`. Then attach the RBD volume backend `rbd1` with Steps 1 and 2 of
[Attach an RBD Backend to Cinder](./attach-an-rbd-backend.md), which also
create the volume type `rbd`. Every resource name in the examples below is one
that devstack produces.
:::

::: warning The NFS target is detached for the duration of this guide
The lab's `ControlPlane` projects the NFS backup target `nfsbk` on `/backups`. A
Cinder takes one backup target, and the `ControlPlane` cannot declare an RBD one
yet, so Step 1 removes the `backupBackend` entry from the `ControlPlane` CR and
Step 6 puts it back. Editing the projected `nfsbk` by hand would be reverted on
the next reconcile; the entry on the `ControlPlane` is the one to change. While
the entry is gone, no backup can be written to or restored from `/backups`,
including the quick start's `lab-bk`. The backups written to Ceph in between
must be deleted before Step 6, because the NFS driver cannot delete them: cinder
refuses with `Delete backup aborted, the backup service currently configured
[...] is not the backup service that was used to create this backup`.
:::

The RBD target is a user-owned `CinderBackupBackend` that you attach to the
projected `controlplane-cinder` by hand. The `openstack` commands use the
`OS_*` variables and the `OS_CACERT` file of Part 1, Step 7 of the tutorial,
with the Keystone port-forward of Step 6 running.

---

## Step 1 — Detach the projected NFS target

Remove the entry from the `ControlPlane` CR. The c5c3-operator prunes the
`CinderBackupBackend` it projected, and the Cinder deletes its backup
Deployment:

```bash
kubectl -n openstack patch controlplane controlplane --type json \
  -p '[{"op":"remove","path":"/spec/services/cinder/backupBackend"}]'
kubectl -n openstack wait cinderbackupbackend/nfsbk --for=delete --timeout=120s
kubectl -n openstack wait deploy/controlplane-cinder-backup --for=delete --timeout=120s
kubectl -n openstack get cinder controlplane-cinder \
  -o jsonpath='{range .status.conditions[?(@.type=="BackupBackendReady")]}{.type}={.status}/{.reason}{"\n"}{end}{range .status.conditions[?(@.type=="BackupServiceReady")]}{.type}={.status}/{.reason}{"\n"}{end}'
```

The last command prints `BackupBackendReady=True/NoBackupBackend` and
`BackupServiceReady=True/BackupNotConfigured`: backups are opt-in, so a Cinder
without a target reports both conditions `True`.

## Step 2 — Attach rbdbk

The lab Ceph has the pool `backups` and the Ceph user `cinder-backup`, whose key
reaches `openstack` as the Secret `ceph-client-cinder-backup` with the raw key
under `userKey`. The target names the pool, the user without its `client.`
prefix, the monitor and the two networks Step 1 of the RBD backend guide read,
and that Secret. The examples use `10.244.0.0/16` and `10.96.0.0/12`: write the
two your shoot prints.

```bash
kubectl apply -f - <<'EOF'
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackupBackend
metadata:
  name: rbdbk
  namespace: openstack
spec:
  cinderRef:
    name: controlplane-cinder
  type: RBD
  rbd:
    pool: backups
    user: cinder-backup
    monitors:
    - rook-ceph-mon-a.rook-ceph.svc.cluster.local
    networks:
    - 10.244.0.0/16
    - 10.96.0.0/12
    keySecretRef:
      name: ceph-client-cinder-backup
EOF
kubectl -n openstack wait cinderbackupbackend/rbdbk --for=condition=Ready --timeout=300s
kubectl -n openstack get cinderbackupbackend rbdbk \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{"\n"}{end}'
kubectl -n openstack get cinder controlplane-cinder \
  -o jsonpath='{range .status.conditions[?(@.type=="BackupBackendReady")]}{.type}={.status}/{.reason}{"\n"}{end}{range .status.conditions[?(@.type=="BackupServiceReady")]}{.type}={.status}/{.reason}{"\n"}{end}'
```

The target reads `CredentialsReady=True/CredentialsAvailable`,
`ConfigProjected=True/ConfigProjected` and `Ready=True/AllReady`, and the Cinder
`BackupBackendReady=True/BackupBackendProjected` and
`BackupServiceReady=True/BackupServiceReady`. Read the driver section and the
Ceph files off the backup pod:

```bash
kubectl -n openstack exec deploy/controlplane-cinder-backup -- cat /etc/cinder/backup.conf.d/backup.conf
kubectl -n openstack exec deploy/controlplane-cinder-backup -- ls /etc/ceph
timeout 300 bash -c 'until openstack volume service list --service cinder-backup -f value -c State | grep -qx up; do sleep 5; done'
openstack volume service list --service cinder-backup
```

`backup.conf` carries the Ceph backup driver:

```ini
[DEFAULT]
backup_ceph_conf = /etc/ceph/ceph.conf
backup_ceph_pool = backups
backup_ceph_user = cinder-backup
backup_driver = cinder.backup.drivers.ceph.CephBackupDriver
backup_use_same_host = false
host = controlplane-cinder-backup
```

`/etc/ceph` lists `ceph.conf`, `ceph.client.cinder-backup.keyring` and
`ceph.client.cinder.keyring`. The last one is the keyring of `rbd1`: a backup of
an RBD volume reads the volume with the volume backend's user. The service list
shows `cinder-backup` on `controlplane-cinder-backup` `up`.

## Step 3 — Back up an RBD volume twice

Create a volume on `rbd1` and take a full backup of it. The Ceph driver writes
the backup into a base image named after the volume and the backup, and keeps a
snapshot on both images:

```bash
openstack volume create --type rbd --size 1 rbdbk-vol
timeout 120 bash -c 'until [ "$(openstack volume show rbdbk-vol -c status -f value)" = available ]; do sleep 2; done'
openstack volume backup create --name rbdbk-full rbdbk-vol
timeout 300 bash -c 'until [ "$(openstack volume backup show rbdbk-full -c status -f value)" = available ]; do sleep 2; done'
vol=$(openstack volume show rbdbk-vol -c id -f value)
full=$(openstack volume backup show rbdbk-full -c id -f value)
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd ls backups
```

The pool `backups` lists `volume-<vol>.backup.<full>`, the base image of
`rbdbk-full`. An incremental backup adds the blocks that changed since, as the
difference between the two source snapshots:

```bash
openstack volume backup create --incremental --name rbdbk-incr rbdbk-vol
timeout 300 bash -c 'until [ "$(openstack volume backup show rbdbk-incr -c status -f value)" = available ]; do sleep 2; done'
openstack volume backup show rbdbk-incr -c is_incremental -f value
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd snap ls "backups/volume-${vol}.backup.${full}"
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd snap ls "volumes/volume-${vol}"
```

`is_incremental` prints `True`. The base image carries two snapshots named
`backup.<backup id>.snap.<timestamp>`, one per backup, and the source image
carries the same two: with `backup_ceph_max_snapshots` at its default of `0` the
driver keeps every source snapshot, and the newest is the one the next
incremental backup starts from. Restore the incremental backup into the volume:

```bash
openstack volume backup restore --force rbdbk-incr rbdbk-vol
timeout 300 bash -c 'until [ "$(openstack volume show rbdbk-vol -c status -f value)" = available ]; do sleep 2; done'
```

The volume returns to `available`.

## Step 4 — Back up the NFS volume

A volume on the NFS backend `nfs1` backs up to the same target as a full copy,
read through the export the backup pod mounts. The quick start's `lab-vol` is
such a volume, `available` after Part 2, Step 10:

```bash
nfs_vol=lab-vol
openstack volume show "$nfs_vol" -c os-vol-host-attr:host -f value
```

The host reads `controlplane-cinder@nfs1#...`. Without Part 2 there is no
`lab-vol`; create a volume on `nfs1` through a volume type that selects it, and
use it instead:

```bash
openstack volume type create --property volume_backend_name=nfs1 nfs
openstack volume create --type nfs --size 1 nfsbk-vol
timeout 120 bash -c 'until [ "$(openstack volume show nfsbk-vol -c status -f value)" = available ]; do sleep 2; done'
nfs_vol=nfsbk-vol
```

Back the volume up and restore it:

```bash
openstack volume backup create --name nfs-on-rbd "$nfs_vol"
timeout 600 bash -c 'until [ "$(openstack volume backup show nfs-on-rbd -c status -f value)" = available ]; do sleep 2; done'
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd ls backups
openstack volume backup restore --force nfs-on-rbd "$nfs_vol"
timeout 600 bash -c "until [ \"\$(openstack volume show $nfs_vol -c status -f value)\" = available ]; do sleep 2; done"
```

The pool lists a second base image, `volume-<id>.backup.<id>` of the NFS volume
and of `nfs-on-rbd`. The driver wrote it in pieces of `backup_ceph_chunk_size`,
128 MiB by default, and the volume returns to `available` after the restore.

## Step 5 — Rotate the backup key

A raised `keyGeneration` has Rook rotate the key of `cinder-backup`. The
PushSecret and the ExternalSecret of the lab's hand-off carry it to
`openstack/ceph-client-cinder-backup` within their one-minute refresh intervals.
The changed Secret re-renders the target's projection under a new name, and the
backup service rolls onto it:

```bash
backup_secret() {
  kubectl -n openstack get deploy controlplane-cinder-backup \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="backup")].secret.secretName}'
}
before=$(backup_secret)
kubectl -n rook-ceph patch cephclient cinder-backup --type merge \
  -p '{"spec":{"security":{"cephx":{"keyGeneration":2}}}}'
for _ in $(seq 60); do [ "$(backup_secret)" != "$before" ] && break; sleep 5; done
echo "$before -> $(backup_secret)"
kubectl -n openstack rollout status deploy/controlplane-cinder-backup --timeout=300s
kubectl -n openstack get secret ceph-client-cinder-backup -o jsonpath='{.data.userKey}' | base64 -d | sha256sum
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get-key client.cinder-backup | sha256sum
```

The `echo` prints two different projection Secret names, the rollout finishes,
and the two digests are equal: the Secret carries the key Ceph accepts. A backup
taken now reaches `available` with the rotated key:

```bash
timeout 300 bash -c 'until openstack volume service list --service cinder-backup -f value -c State | grep -qx up; do sleep 5; done'
openstack volume backup create --incremental --name rbdbk-rotated rbdbk-vol
timeout 300 bash -c 'until [ "$(openstack volume backup show rbdbk-rotated -c status -f value)" = available ]; do sleep 2; done'
```

A rotation of the volume key `cinder`, as Step 3 of the RBD backend guide runs
it, rolls the backup pod as well, because that key's keyring is projected into
it.

## Step 6 — Detach rbdbk and restore the NFS target

Delete every backup written to Ceph, newest first, because cinder refuses to
delete a backup an incremental one still depends on. Then delete the RBD volume
and the target:

```bash
for bk in rbdbk-rotated rbdbk-incr rbdbk-full nfs-on-rbd; do
  openstack volume backup delete "$bk"
  timeout 300 bash -c "while openstack volume backup show $bk >/dev/null 2>&1; do sleep 2; done"
done
openstack volume delete rbdbk-vol
kubectl -n openstack delete cinderbackupbackend rbdbk
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd ls backups
```

`rbd ls backups` prints nothing. If Step 4 created `nfsbk-vol`, delete it and
its type too (`openstack volume delete nfsbk-vol`, then
`openstack volume type delete nfs`). Then put the entry of the lab's
`ControlPlane` fragment back:

```bash
kubectl -n openstack patch controlplane controlplane --type merge \
  -p '{"spec":{"services":{"cinder":{"backupBackend":{"name":"nfsbk","type":"NFS","nfs":{"server":"nfs-server.openstack.svc.cluster.local","path":"/backups"}}}}}}'
kubectl -n openstack wait cinderbackupbackend/nfsbk --for=condition=Ready --timeout=300s
kubectl -n openstack wait deploy/controlplane-cinder-backup --for=condition=Available --timeout=300s
```

`nfsbk` is `Ready` again, the backup Deployment is available, and `lab-bk` can
be restored and deleted once more.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `CredentialsReady=False/WaitingForCredentials`, `RBD key ExternalSecret openstack/<name> not found yet` | No Secret of that name exists in `openstack` and no ExternalSecret produces one |
| `... waiting for the RBD key Secret to carry the userKey data key` | An ExternalSecret of that name has not synced yet |
| `... RBD key Secret exists but is missing expected keys` | The Secret carries no `userKey`, for example a keyring file under another key |
| `... Secret "<name>" carries an empty userKey` | The key is empty after trimming |
| `... Secret "<name>" carries a userKey that is not a cephx key (base64 expected)` | The value is not the base64 `ceph auth get-key` prints |
| The backup pod logs `_setup_backup_driver` failed with `error connecting to the cluster` | The monitors, the networks or the key: the monitor name does not resolve, a network is missing from `spec.rbd.networks`, or Ceph refuses the key. The backup service does not repeat the setup on its own; delete its pod once the cause is fixed |
| A backup fails with `Keyring path /etc/ceph/<cluster>.client.<user>.keyring is not readable` | The backup pod carries no keyring for the source volume's backend: that `CinderBackend` is not projected, so check its conditions |
| A backup of an RBD volume fails to authenticate, and the Cinder carries a `CephKeyringConflict` event | Two sources project one keyring file with different keys; the event names which one the backup pod carries |
| The apply is refused with `already has CinderBackupBackend` | The projected `nfsbk` is still attached: Step 1 was skipped |
| A restore or delete fails with `is not the backup service that was used to create this backup` | The backup was written by the other driver; it is reachable again once its target is back |

## Standalone Cinder, without a ControlPlane

A standalone Cinder takes the same `CinderBackupBackend` with `cinderRef.name`
set to its own Cinder, `cinder` in the examples of the reference page. There is
no detour: nothing projects a backup target onto a standalone Cinder, so Steps 1
and 6 do not apply, and a target already attached is deleted before the RBD one
is applied. The key Secret lives in that Cinder's namespace on the cluster its
`spec.targetClusterRef` names.

## See also

- [CinderBackupBackend CRD API Reference](../../reference/cinder/cinder-backup-backend-crd.md):
  the RBD fields, the key Secret contract, the rendered section, the backup
  pod's Ceph files and the caveats of the Ceph backup driver.
- [Configure NFS Volume Backups](./configure-nfs-backups.md): the NFS target the
  devstack projects, and a backup round trip on it.
- [Attach an RBD Backend to Cinder](./attach-an-rbd-backend.md): the RBD volume
  backend whose volumes Step 3 backs up.

## Tested by

The credentials gate, the projected driver section, `ceph.conf` and keyring,
the volume backend's keyring in the backup pod, the Ceph egress rule and the
roll on a replaced backup key and on a replaced volume key are asserted
end-to-end on the CI e2e kind cluster, which runs no Ceph, by this chainsaw
suite:

```bash
chainsaw test --test-dir tests/e2e/cinder/backup-rbd
```

::: details The backup target the backup-rbd suite applies
The suite runs its own Cinder in the shared `openstack` namespace, and
`spec.cinderRef` is immutable, so the fixture carries the suite's isolation
names: Cinder `cinder-bkrbd` with the target `bkrbd-rbdbk` and its Secret
`bkrbd-rbdbk-key`, and a monitor no cluster resolves. The walkthrough above
keeps the `controlplane-cinder` / `rbdbk` names the devstack produces.

<<< @/../tests/e2e/cinder/backup-rbd/04-cinderbackupbackend-cr.yaml#backup-backend
:::

## Proven by

No lab run has proven this page yet. The run records here the date, the shoot,
the commit the page was run at, and whether every `bash` block exited 0 on its
first attempt.
