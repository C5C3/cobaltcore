---
title: Attach an RBD Backend to Cinder
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Attach an RBD Backend to Cinder

An RBD backend keeps each volume as an RBD image in a Ceph pool. This guide
attaches two RBD backends to the lab's Cinder: one with the key Secret Rook's
`CephClient` produces, one with a Secret made by hand from `ceph auth get-key`.
It places a volume on each, rotates both keys, and detaches the backends again.
The operator creates no pool and no Ceph user; it projects the Ceph client
configuration and the user's keyring into the volume service and opens the
Ceph ports towards the networks the backend names.

For the full field reference, see the
[CinderBackend CRD API Reference](../../reference/cinder/cinder-backend-crd.md),
in particular [RBDBackendSpec](../../reference/cinder/cinder-backend-crd.md#rbdbackendspec)
and [The key Secret](../../reference/cinder/cinder-backend-crd.md#the-key-secret).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (metal-stack)](../../quick-start-metal-stack.md)** devstack. Stand it up first:

```bash
EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_CEPH=true make deploy-infra
```

Follow that tutorial through Part 1, so a `ControlPlane` CR named
`controlplane` is `Ready` in the `openstack` namespace, its projected
`controlplane-cinder` Cinder child is running, and the lab Ceph reports
`HEALTH_OK`. Every resource name in the examples below is one that devstack
produces.
:::

The backends of this guide are user-owned `CinderBackend` CRs that you attach to
the projected `controlplane-cinder` by hand; the projected Cinder child itself
stays untouched. The c5c3-operator's prune sweep deletes only the backends it
projected, so it never reverts or deletes these two, and the ControlPlane does
not declare an RBD backend in `services.cinder.backends` yet. The `openstack`
commands use the `OS_*` variables and the `OS_CACERT` file of Part 1, Step 7 of
the tutorial, with the Keystone port-forward of Step 6 running.

---

## Step 1 — What the lab Ceph provides

`WITH_CEPH=true` deploys a Ceph through Rook in `rook-ceph`: the RBD pools
`volumes` and `backups`, and the Ceph users `cinder` and `cinder-backup`, each
with a key of the `aes` type. The noble image's `librbd` reads no `aes256k`
key, which is why the lab pins the type. The keys reach `openstack` through
OpenBao as the Secrets `ceph-client-cinder` and `ceph-client-cinder-backup`,
each carrying the raw key under `userKey`, the data key the backend reads:

```bash
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph status
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph osd pool ls
kubectl -n openstack get secret ceph-client-cinder -o jsonpath='{.data}' | jq 'keys'
```

`ceph status` shows `HEALTH_OK`, the pool list names `volumes`, `backups` and
`.mgr`, and the Secret carries the one key `userKey`.

The backend needs the monitor address and the networks the Ceph cluster answers
on. Rook advertises each monitor through a Service, so the monitors answer on
the shoot's service network and the OSDs on its pod network. Gardener records
both in the ConfigMap `kube-system/shoot-info`:

```bash
kubectl -n rook-ceph get svc -l app=rook-ceph-mon
kubectl -n kube-system get configmap shoot-info -o jsonpath='{.data.podNetwork} {.data.serviceNetwork}'; echo
```

The first command lists one Service per monitor, `rook-ceph-mon-a` on the lab's
single monitor; its DNS name is
`rook-ceph-mon-a.rook-ceph.svc.cluster.local`. The second prints the two CIDRs.
The examples below use `10.244.0.0/16` and `10.96.0.0/12` in their place: write
the two your shoot prints.

## Step 2 — Attach rbd1 with the key Rook produces

The backend names the pool, the user without its `client.` prefix, the monitor,
the two networks and the Secret Rook's key arrives in:

```bash
kubectl apply -f - <<'EOF'
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackend
metadata:
  name: rbd1
  namespace: openstack
spec:
  cinderRef:
    name: controlplane-cinder
  type: RBD
  rbd:
    pool: volumes
    user: cinder
    monitors:
    - rook-ceph-mon-a.rook-ceph.svc.cluster.local
    networks:
    - 10.244.0.0/16
    - 10.96.0.0/12
    keySecretRef:
      name: ceph-client-cinder
EOF
kubectl -n openstack wait cinderbackend/rbd1 --for=condition=Ready --timeout=300s
kubectl -n openstack get cinderbackend rbd1 \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{"\n"}{end}'
```

The conditions read `CredentialsReady=True/CredentialsAvailable`,
`ConfigProjected=True/ConfigProjected` and `Ready=True/AllReady`. The operator
rendered the section, `ceph.conf` and the keyring into a Secret of its own and
started the Deployment `controlplane-cinder-volume-rbd1`, which mounts the two
Ceph files at `/etc/ceph`.

Wait until the volume service reports in and the scheduler holds the backend,
then create a volume type for it and a 1 GiB volume:

```bash
timeout 300 bash -c 'until openstack volume backend pool list -f value | grep -q "@rbd1#"; do sleep 5; done'
openstack volume service list --service cinder-volume
openstack volume type create --property volume_backend_name=rbd1 rbd
openstack volume create --type rbd --size 1 rbd-vol-1
timeout 120 bash -c 'until [ "$(openstack volume show rbd-vol-1 -c status -f value)" = available ]; do sleep 2; done'
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd ls volumes
```

The service list shows `cinder-volume` on `controlplane-cinder@rbd1` `up`
beside `controlplane-cinder@nfs1`, and the pool `volumes` holds
`volume-<id>`, the id of `rbd-vol-1`.

## Step 3 — Rotate the Rook key

A raised `keyGeneration` has Rook rotate the user's key. Ceph accepts only the
new key from then on, and the PushSecret and the ExternalSecret of the hand-off
carry it to `openstack/ceph-client-cinder` within their one-minute refresh
intervals. The changed Secret re-renders the backend's projection under a new
name, and the volume service rolls onto it:

```bash
ceph_secret() {
  kubectl -n openstack get deploy controlplane-cinder-volume-rbd1 \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="ceph")].secret.secretName}'
}
before=$(ceph_secret)
kubectl -n rook-ceph patch cephclient cinder --type merge \
  -p '{"spec":{"security":{"cephx":{"keyGeneration":2}}}}'
for _ in $(seq 60); do [ "$(ceph_secret)" != "$before" ] && break; sleep 5; done
echo "$before -> $(ceph_secret)"
kubectl -n openstack rollout status deploy/controlplane-cinder-volume-rbd1 --timeout=300s
kubectl -n openstack get secret ceph-client-cinder -o jsonpath='{.data.userKey}' | base64 -d | sha256sum
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get-key client.cinder | sha256sum
```

The `echo` prints two different projection Secret names, the rollout finishes,
and the two digests are equal: the Secret carries the key Ceph accepts. They
hash the key, so the output can be shared without publishing a credential.
A volume created now reaches `available`:

```bash
openstack volume create --type rbd --size 1 rbd-vol-2
timeout 120 bash -c 'until [ "$(openstack volume show rbd-vol-2 -c status -f value)" = available ]; do sleep 2; done'
```

## Step 4 — Attach rbd2 with a Secret made by hand

A key Secret needs no Rook: any Secret in `openstack` with the user's key under
`userKey` serves. Create a second Ceph user in the toolbox, limited to the
`volumes` pool. Its key has to be of the `aes` type, like the lab's own users:
the `aes256k` type a fresh Ceph 20 prefers fails to authenticate from the
cinder image. `--key_type` selects it:

```bash
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get-or-create client.cinder-manual \
  mon 'profile rbd' osd 'profile rbd pool=volumes' mgr 'profile rbd pool=volumes' --key_type=aes >/dev/null
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get client.cinder-manual -f json | jq 'map(del(.key))'
```

The last command prints the user's entry without its key: the capabilities, and
the key type, which has to be `aes`. Then pipe the key into a Secret, which
keeps it off the `kubectl` command line, and attach the backend:

```bash
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get-key client.cinder-manual |
  kubectl -n openstack create secret generic cinder-manual-key --from-file=userKey=/dev/stdin
kubectl apply -f - <<'EOF'
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackend
metadata:
  name: rbd2
  namespace: openstack
spec:
  cinderRef:
    name: controlplane-cinder
  type: RBD
  rbd:
    pool: volumes
    user: cinder-manual
    monitors:
    - rook-ceph-mon-a.rook-ceph.svc.cluster.local
    networks:
    - 10.244.0.0/16
    - 10.96.0.0/12
    keySecretRef:
      name: cinder-manual-key
EOF
kubectl -n openstack wait cinderbackend/rbd2 --for=condition=Ready --timeout=300s
timeout 300 bash -c 'until openstack volume backend pool list -f value | grep -q "@rbd2#"; do sleep 5; done'
openstack volume type create --property volume_backend_name=rbd2 rbd-manual
openstack volume create --type rbd-manual --size 1 rbd-vol-3
timeout 120 bash -c 'until [ "$(openstack volume show rbd-vol-3 -c status -f value)" = available ]; do sleep 2; done'
```

Both backends share the pool, so `rbd ls volumes` lists all three volumes.

## Step 5 — Rotate the hand-made key

`ceph auth rotate` replaces the key, and Ceph stops accepting the old one at
once. It takes `--key_type` as well, so the new key stays an `aes` one. Patch
the new key into the Secret; never delete and recreate it, which would stop the
backend's volume service until the Secret is back:

```bash
ceph_secret2() {
  kubectl -n openstack get deploy controlplane-cinder-volume-rbd2 \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="ceph")].secret.secretName}'
}
before=$(ceph_secret2)
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth rotate client.cinder-manual --key_type=aes >/dev/null
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get-key client.cinder-manual |
  jq -Rc '{stringData: {userKey: .}}' |
  kubectl -n openstack patch secret cinder-manual-key --patch-file=/dev/stdin
for _ in $(seq 60); do [ "$(ceph_secret2)" != "$before" ] && break; sleep 5; done
kubectl -n openstack rollout status deploy/controlplane-cinder-volume-rbd2 --timeout=300s
openstack volume create --type rbd-manual --size 1 rbd-vol-4
timeout 120 bash -c 'until [ "$(openstack volume show rbd-vol-4 -c status -f value)" = available ]; do sleep 2; done'
```

The volume service rolled onto a projection Secret of a new name, and the volume
created afterwards reaches `available` with the rotated key.

## Step 6 — Detach

Delete the volumes before their backends: a backend's volumes are keyed by its
host identity, and nothing serves them once it is gone. Then delete the volume
types, the two `CinderBackend` CRs (each runs its service-remove Job before it
goes), the hand-made Secret and the hand-made Ceph user:

```bash
openstack volume delete rbd-vol-1 rbd-vol-2 rbd-vol-3 rbd-vol-4
timeout 120 bash -c 'until ! openstack volume list -c Name -f value | grep -q "^rbd-vol-"; do sleep 2; done'
openstack volume type delete rbd rbd-manual
kubectl -n openstack delete cinderbackend rbd1 rbd2 --timeout=300s
kubectl -n openstack delete secret cinder-manual-key
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth del client.cinder-manual
openstack volume service list --service cinder-volume
```

The service list shows `controlplane-cinder@nfs1` alone, and `rbd ls volumes`
lists nothing. The Secret `ceph-client-cinder` stays: the lab's hand-off owns it.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `CredentialsReady=False/WaitingForCredentials`, `RBD key ExternalSecret openstack/<name> not found yet` | No Secret of that name exists in `openstack` and no ExternalSecret produces one |
| `... waiting for the RBD key Secret to carry the userKey data key` | An ExternalSecret of that name has not synced yet |
| `... RBD key Secret exists but is missing expected keys` | The Secret carries no `userKey`, for example a keyring file under another key |
| `... Secret "<name>" carries an empty userKey` | The key is empty after trimming |
| `... Secret "<name>" carries a userKey that is not a cephx key (base64 expected)` | The value is not the base64 `ceph auth get-key` prints |
| The volume pod logs `Error connecting to ceph cluster.` and `Failed to initialize driver.` | The monitors, the networks or the key: the monitor name does not resolve, a network is missing from `spec.rbd.networks`, or Ceph refuses the key. The volume service does not repeat the setup on its own; delete its pod once the cause is fixed |
| The apply is refused with `user is the cephx name without its client. prefix` | `spec.rbd.user` names `client.cinder`; write `cinder` |

## Standalone Cinder, without a ControlPlane

A standalone Cinder takes the same two CRs with `cinderRef.name` set to its own
Cinder, `cinder` in the examples of the reference page. Nothing else differs:
the key Secret lives in that Cinder's namespace on the cluster its
`spec.targetClusterRef` names, and no ControlPlane projection is involved.

## See also

- [CinderBackend CRD API Reference](../../reference/cinder/cinder-backend-crd.md):
  the RBD fields, the key Secret contract, the rendered section, `ceph.conf` and
  keyring, and the caveats of the RBD driver.
- [Attach an NFS Backend to Cinder](./attach-an-nfs-backend.md): the NFS
  backend the devstack projects, and the detach sequence.
- [Lab Ceph](../../reference/infrastructure/infrastructure-manifests.md#lab-ceph):
  the Rook cluster, the pools, the users and the key hand-off through OpenBao.

## Tested by

The credentials gate, the projected `ceph.conf`, keyring and section, the
`/etc/ceph` mount, the Ceph egress rule and the roll on a replaced key are
asserted end-to-end on the CI e2e kind cluster, which runs no Ceph, by this
chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/cinder/rbd-backend
```

::: details The backend CR the rbd-backend suite applies
The suite runs its own Cinder in the shared `openstack` namespace, and
`spec.cinderRef` is immutable, so the fixture carries the suite's isolation
names: Cinder `cinder-rbd` with the backend `rbdbe-rbd1` and its Secret
`rbdbe-rbd1-key`, and a monitor no cluster resolves. The walkthrough above keeps
the `controlplane-cinder` / `rbd1` names the devstack produces.

<<< @/../tests/e2e/cinder/rbd-backend/02-cinderbackend-cr.yaml#cinder-backend
:::

## Proven by

No lab run has proven this page yet. The run records here the date, the shoot,
the commit the page was run at, and whether every `bash` block exited 0 on its
first attempt.
