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
than a full copy. This guide swaps the lab's projected backup target `cephbk`
for a user-owned RBD target `rbdbk` on the same pool and user, backs up an RBD
volume twice and restores it, backs up an NFS volume where the lab has one,
rotates the backup key, and puts `cephbk` back. The operator creates no pool
and no Ceph user; it renders the Ceph backup driver's section, the Ceph client
configuration and the keyring into the backup service, and projects the keyring
of every RBD volume backend beside them, because a backup reads its source
volume with that backend's own user.

For the full field reference, see the
[CinderBackupBackend CRD API Reference](../../reference/cinder/cinder-backup-backend-crd.md),
in particular [RBDBackupBackendSpec](../../reference/cinder/cinder-backup-backend-crd.md#rbdbackupbackendspec),
[The key Secret](../../reference/cinder/cinder-backup-backend-crd.md#the-key-secret)
and [Ceph files in the backup pod](../../reference/cinder/cinder-backup-backend-crd.md#ceph-files-in-the-backup-pod).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (metal-stack)](../../quick-start-metal-stack.md)** devstack. Stand it up first:

```bash
EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_CEPH=true make deploy-infra
```

Follow that tutorial through Part 1, so a `ControlPlane` CR named
`controlplane` is `Ready` in the `openstack` namespace with its projected
`controlplane-cinder` Cinder child running, and the lab Ceph reports
`HEALTH_OK`. Then attach the RBD volume backend `rbd1` with Steps 1 and 2 of
[Attach an RBD Backend to Cinder](./attach-an-rbd-backend.md), which also
create the volume type `rbd`. Every resource name in the examples below is one
that devstack produces.
:::

::: warning The projected target is detached for the duration of this guide
The lab's `ControlPlane` projects the RBD backup target `cephbk` on the pool
`backups`, and a Cinder takes one backup target. The guide attaches a
user-owned RBD target so Step 6 can restore the lab as it was: Step 1 removes
the `backupBackend` entry from the `ControlPlane` CR and Step 6 puts it back. A
`ControlPlane` can declare the other target itself, as
[Switching the backup target on the ControlPlane](#switching-the-backup-target-on-the-controlplane)
shows. Editing the projected `cephbk` by hand would be reverted on the next
reconcile; the entry on the `ControlPlane` is the one to change. While no target
is attached, between Steps 1 and 2 and after the delete of `rbdbk` in Step 6,
no backup can be written or restored, including the quick start's `lab-bk`.
`rbdbk` runs the same Ceph backup driver on the same pool as `cephbk`, so the
backups `cephbk` wrote, `lab-bk` included, stay reachable through `rbdbk`, and
the ones written through `rbdbk` stay reachable through `cephbk` once Step 6 has
put it back. Step 6 still deletes the guide's own backups, to leave the pool as
it was.
:::

The RBD target is a user-owned `CinderBackupBackend` that you attach to the
projected `controlplane-cinder` by hand. The `openstack` commands use the
`OS_*` variables and the `OS_CACERT` file of Part 1, Step 7 of the tutorial,
with the Keystone port-forward of Step 6 running.

---

## Step 1 — Detach the projected target

Remove the entry from the `ControlPlane` CR. The c5c3-operator prunes the
`CinderBackupBackend` it projected, and the Cinder deletes its backup
Deployment:

```bash
kubectl -n openstack patch controlplane controlplane --type json \
  -p '[{"op":"remove","path":"/spec/services/cinder/backupBackend"}]'
kubectl -n openstack wait cinderbackupbackend/cephbk --for=delete --timeout=120s
kubectl -n openstack wait deploy/controlplane-cinder-backup --for=delete --timeout=120s
kubectl -n openstack get cinder controlplane-cinder \
  -o jsonpath='{range .status.conditions[?(@.type=="BackupBackendReady")]}{.type}={.status}/{.reason}{"\n"}{end}{range .status.conditions[?(@.type=="BackupServiceReady")]}{.type}={.status}/{.reason}{"\n"}{end}'
```

The patch prints the webhook's warning that `cephbk` (RBD) is removed and that
every backup the RBD driver wrote can be neither restored nor deleted until an
RBD backup target on the same storage is declared again; Step 2 declares one.
The last command prints `BackupBackendReady=True/NoBackupBackend` and
`BackupServiceReady=True/BackupNotConfigured`: backups are opt-in, so a Cinder
without a target reports both conditions `True`.

## Step 2 — Attach rbdbk

The lab Ceph has the pool `backups` and the Ceph user `cinder-backup`, whose key
reaches `openstack` as the Secret `ceph-client-cinder-backup` with the raw key
under `userKey`. The target names the pool, the user without its `client.`
prefix, the monitor and the two networks Step 1 of the RBD backend guide read,
and that Secret, the values `cephbk` carries. The examples use `10.244.0.0/16`
and `10.96.0.0/12`: write the two your shoot prints.

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
`ceph.client.cinder.keyring`. The last one is the keyring of `ceph1` and
`rbd1`: a backup of an RBD volume reads the volume with the volume backend's
user, and both backends are the user `cinder` with the Secret
`ceph-client-cinder`, so one file serves both and no `CephKeyringConflict`
event arises. The service list shows `cinder-backup` on
`controlplane-cinder-backup` `up`.

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

## Step 4 — Back up an NFS volume

This step needs the NFS volume backend `nfs1`, which
[Part 3](../../quick-start-metal-stack.md#nfs) of the quick start adds beside
`ceph1` on a lab deployed with `WITH_NFS=true` as well. A lab without Part 3
has no NFS backend and skips to Step 5. A volume on `nfs1` backs up to the same
target as a full copy, read through the export the backup pod mounts. Part 3's
`lab-nfs` is such a volume, `available` after Part 3, Step 4:

```bash
nfs_vol=lab-nfs
openstack volume show "$nfs_vol" -c os-vol-host-attr:host -f value
```

The host reads `controlplane-cinder@nfs1#...`. Where `lab-nfs` is gone, create
a volume of the type `nfs`, which Part 3, Step 2 created for `nfs1`, and use it
instead:

```bash
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
backup service rolls onto it. The patch raises the generation it reads by one,
so it rotates the key also where an earlier run raised it before:

```bash
backup_secret() {
  kubectl -n openstack get deploy controlplane-cinder-backup \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="backup")].secret.secretName}'
}
before=$(backup_secret)
generation=$(kubectl -n rook-ceph get cephclient cinder-backup -o jsonpath='{.spec.security.cephx.keyGeneration}')
[ -n "${generation}" ] && kubectl -n rook-ceph patch cephclient cinder-backup --type merge \
  -p "{\"spec\":{\"security\":{\"cephx\":{\"keyGeneration\":$((generation + 1))}}}}"
for _ in $(seq 60); do [ "$(backup_secret)" != "$before" ] && break; sleep 5; done
echo "$before -> $(backup_secret)"
kubectl -n openstack rollout status deploy/controlplane-cinder-backup --timeout=300s
kubectl -n openstack get secret ceph-client-cinder-backup -o jsonpath='{.data.userKey}' | base64 -d | sha256sum
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get-key client.cinder-backup | sha256sum
```

The `echo` prints two different projection Secret names, the rollout finishes,
and the two digests are equal: the Secret carries the key Ceph accepts. An empty
read of the generation patches nothing, and the `echo` then prints the same
name twice. `cephbk`, which Step 6 puts back, reads the same Secret, so it
starts on the rotated key. A backup taken now reaches `available` with the
rotated key:

```bash
timeout 300 bash -c 'until openstack volume service list --service cinder-backup -f value -c State | grep -qx up; do sleep 5; done'
openstack volume backup create --incremental --name rbdbk-rotated rbdbk-vol
timeout 300 bash -c 'until [ "$(openstack volume backup show rbdbk-rotated -c status -f value)" = available ]; do sleep 2; done'
```

A rotation of the volume key `cinder`, as Step 3 of the RBD backend guide runs
it, rolls the backup pod as well, because that key's keyring is projected into
it.

## Step 6 — Detach rbdbk and restore the projected target

Delete every backup this guide wrote, newest first, because cinder refuses to
delete a backup an incremental one still depends on; `nfs-on-rbd` exists only
when Step 4 ran. Then delete the RBD volume and the target:

```bash
for bk in rbdbk-rotated rbdbk-incr rbdbk-full nfs-on-rbd; do
  if openstack volume backup show "$bk" >/dev/null 2>&1; then
    openstack volume backup delete "$bk"
    timeout 300 bash -c "while openstack volume backup show $bk >/dev/null 2>&1; do sleep 2; done"
  fi
done
openstack volume delete rbdbk-vol
kubectl -n openstack delete cinderbackupbackend rbdbk
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd ls backups
```

`rbd ls backups` lists none of the guide's images. The images of the quick
start's backups stay: `lab-bk`'s after Part 2, Step 10, and `lab-nfs-bk`'s
after Part 3. If Step 4 created `nfsbk-vol`, delete it too with
`openstack volume delete nfsbk-vol`; the volume type `nfs` belongs to Part 3 of
the quick start and stays. Then put the entry of the lab's `ControlPlane` back, the `cephbk` of
`deploy/lab/metal-stack/controlplane/controlplane-lab.yaml`; on a shoot whose
networks you wrote into that file, write the same two here:

```bash
kubectl -n openstack patch controlplane controlplane --type json -p '[{"op":"add","path":"/spec/services/cinder/backupBackend","value":{
  "name":"cephbk","type":"RBD","rbd":{
    "pool":"backups","user":"cinder-backup",
    "monitors":["rook-ceph-mon-a.rook-ceph.svc.cluster.local"],
    "networks":["10.240.0.0/13","10.248.0.0/18"],
    "keySecretRef":{"name":"ceph-client-cinder-backup"}}}}]'
kubectl -n openstack wait --for=create cinderbackupbackend/cephbk --timeout=120s
kubectl -n openstack wait cinderbackupbackend/cephbk --for=condition=Ready --timeout=300s
kubectl -n openstack wait --for=create deploy/controlplane-cinder-backup --timeout=120s
kubectl -n openstack wait deploy/controlplane-cinder-backup --for=condition=Available --timeout=300s
```

The two create waits give the operators time to project the target and its
Deployment: a Ready or Available wait alone exits 1 with
`Error from server (NotFound)` while the object is missing. `cephbk` is `Ready`
again, the backup Deployment is available, and `lab-bk` can be restored and
deleted through it as before.

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
| The apply is refused with `already has CinderBackupBackend` | The projected `cephbk` is still attached: Step 1 was skipped |
| A restore or delete fails with `is not the backup service that was used to create this backup` | Another driver wrote the backup, for example an NFS target's; it is reachable again once a target of that driver is back |

## Switching the backup target on the ControlPlane

A `ControlPlane` swaps its backup target in one edit. Replace the
`backupBackend` entry, and the c5c3-operator prunes `cephbk` before it projects
the new target, because a Cinder takes one. Run this after Step 6, which put
`cephbk` back: the c5c3-operator refuses to adopt a `CinderBackupBackend` it did
not create, so the hand-made `rbdbk` has to be gone. While it is there, the
patch leaves `cephbk` in place and the `ControlPlane` reports
`CinderReady=False/CinderBackendError`. The entry carries the Step 2 values:

```bash
kubectl -n openstack patch controlplane controlplane --type json -p '[{"op":"replace","path":"/spec/services/cinder/backupBackend","value":{
  "name":"rbdbk","type":"RBD","rbd":{
    "pool":"backups","user":"cinder-backup",
    "monitors":["rook-ceph-mon-a.rook-ceph.svc.cluster.local"],
    "networks":["10.244.0.0/16","10.96.0.0/12"],
    "keySecretRef":{"name":"ceph-client-cinder-backup"}}}}]'
kubectl -n openstack wait cinderbackupbackend/cephbk --for=delete --timeout=120s
kubectl -n openstack wait --for=create cinderbackupbackend/rbdbk --timeout=120s
kubectl -n openstack wait cinderbackupbackend/rbdbk --for=condition=Ready --timeout=300s
kubectl -n openstack wait deploy/controlplane-cinder-backup --for=condition=Available --timeout=300s
```

A `replace` of the whole entry is the way to swap it. Between an NFS and an
RBD entry a merge patch would keep the old `nfs` block beside the new `rbd`
one, and the union rule refuses the result. Changing the type while keeping the
name is refused with `type is immutable while name is unchanged`, because a
restore reads a backup with the driver that wrote it. Both entries here are
RBD, so the rename keeps the type and the update prints no warning: the
backups `cephbk` wrote, such as `lab-bk`, stay reachable while `rbdbk` is the
target, because the same driver reads the same pool.

The way back is the same `replace` with the `cephbk` entry of Step 6. The
backups written while `rbdbk` was the target stay reachable through `cephbk`
for the same reason:

```bash
kubectl -n openstack patch controlplane controlplane --type json -p '[{"op":"replace","path":"/spec/services/cinder/backupBackend","value":{
  "name":"cephbk","type":"RBD","rbd":{
    "pool":"backups","user":"cinder-backup",
    "monitors":["rook-ceph-mon-a.rook-ceph.svc.cluster.local"],
    "networks":["10.240.0.0/13","10.248.0.0/18"],
    "keySecretRef":{"name":"ceph-client-cinder-backup"}}}}]'
kubectl -n openstack wait cinderbackupbackend/rbdbk --for=delete --timeout=120s
kubectl -n openstack wait --for=create cinderbackupbackend/cephbk --timeout=120s
kubectl -n openstack wait cinderbackupbackend/cephbk --for=condition=Ready --timeout=300s
```

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
- [Configure NFS Volume Backups](./configure-nfs-backups.md): the NFS backup
  target of the ControlPlane devstack on kind, and a backup round trip on it.
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
